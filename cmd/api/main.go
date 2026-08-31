package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"golang.org/x/crypto/bcrypt"
)

type Server struct {
	db                        *pgxpool.Pool
	rdb                       *redis.Client
	jwtSecret                 []byte
	sendgridKey, sendgridFrom string
}
type claims struct {
	UserID string `json:"sub"`
	Role   string `json:"role"`
	jwt.RegisteredClaims
}
type userRequest struct{ Email, Password, DisplayName string }
type postRequest struct{ Title, Body string }
type commentRequest struct {
	Body     string `json:"body"`
	ParentID string `json:"parent_id"`
}
type user struct {
	ID          string `json:"id"`
	Email       string `json:"email"`
	DisplayName string `json:"display_name"`
	Role        string `json:"role"`
}
type post struct {
	ID         string    `json:"id"`
	AuthorID   string    `json:"author_id"`
	AuthorName string    `json:"author_name"`
	Title      string    `json:"title"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}
type comment struct {
	ID         string    `json:"id"`
	PostID     string    `json:"post_id"`
	AuthorID   string    `json:"author_id"`
	AuthorName string    `json:"author_name"`
	Body       string    `json:"body"`
	ParentID   *string   `json:"parent_id"`
	CreatedAt  time.Time `json:"created_at"`
	Replies    []comment `json:"replies,omitempty"`
}

//go:embed web/*
var frontend embed.FS

func main() {
	ctx := context.Background()
	db, err := pgxpool.New(ctx, env("DATABASE_URL", "postgres://blog:blog@localhost:5432/blog_social?sslmode=disable"))
	if err != nil {
		log.Fatal(err)
	}
	defer db.Close()
	if err = db.Ping(ctx); err != nil {
		log.Fatal(err)
	}
	rdbOpts, err := redis.ParseURL(env("REDIS_URL", "redis://localhost:6379/0"))
	if err != nil {
		log.Fatal(err)
	}
	rdb := redis.NewClient(rdbOpts)
	defer rdb.Close()
	server := &Server{db: db, rdb: rdb, jwtSecret: []byte(env("JWT_SECRET", "dev-secret-change-me")), sendgridKey: os.Getenv("SENDGRID_API_KEY"), sendgridFrom: env("SENDGRID_FROM_EMAIL", "no-reply@example.com")}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /api/v1/auth/register", server.register)
	mux.HandleFunc("POST /api/v1/auth/login", server.login)
	mux.HandleFunc("GET /api/v1/auth/verify", server.verify)
	mux.Handle("GET /api/v1/feed", server.auth(http.HandlerFunc(server.feed)))
	mux.Handle("POST /api/v1/posts", server.auth(http.HandlerFunc(server.createPost)))
	mux.Handle("PUT /api/v1/posts/{id}", server.auth(http.HandlerFunc(server.updatePost)))
	mux.Handle("DELETE /api/v1/posts/{id}", server.auth(http.HandlerFunc(server.deletePost)))
	mux.Handle("GET /api/v1/posts/{id}/comments", server.auth(http.HandlerFunc(server.listComments)))
	mux.Handle("POST /api/v1/posts/{id}/comments", server.auth(http.HandlerFunc(server.createComment)))
	mux.HandleFunc("GET /", frontendHandler)
	log.Printf("blog social API listening on :%s", env("PORT", "8080"))
	log.Fatal(http.ListenAndServe(":"+env("PORT", "8080"), server.rateLimit(server.cors(mux))))
}

func frontendHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		http.ServeFileFS(w, r, frontend, "web/index.html")
		return
	}
	http.FileServer(http.FS(frontend)).ServeHTTP(w, r)
}

func env(key, fallback string) string {
	if value := os.Getenv(key); value != "" {
		return value
	}
	return fallback
}
func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
func errorJSON(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]string{"error": message})
}
func decode(w http.ResponseWriter, r *http.Request, target any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(target); err != nil {
		errorJSON(w, http.StatusBadRequest, "invalid JSON body")
		return false
	}
	return true
}
func randomToken() string {
	bytes := make([]byte, 32)
	_, _ = rand.Read(bytes)
	return hex.EncodeToString(bytes)
}
func hash(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}
func pathID(r *http.Request) string { return r.PathValue("id") }

func (s *Server) register(w http.ResponseWriter, r *http.Request) {
	var input userRequest
	if !decode(w, r, &input) {
		return
	}
	input.Email = strings.ToLower(strings.TrimSpace(input.Email))
	input.DisplayName = strings.TrimSpace(input.DisplayName)
	if !strings.Contains(input.Email, "@") || len(input.Password) < 8 || input.DisplayName == "" {
		errorJSON(w, 400, "email, display name, and password of at least 8 characters are required")
		return
	}
	password, err := bcrypt.GenerateFromPassword([]byte(input.Password), bcrypt.DefaultCost)
	if err != nil {
		errorJSON(w, 500, "could not hash password")
		return
	}
	var id string
	err = s.db.QueryRow(r.Context(), `INSERT INTO users(email,password_hash,display_name) VALUES($1,$2,$3) RETURNING id`, input.Email, password, input.DisplayName).Scan(&id)
	if err != nil {
		if strings.Contains(err.Error(), "duplicate") {
			errorJSON(w, 409, "email already registered")
		} else {
			errorJSON(w, 500, "could not create account")
		}
		return
	}
	token := randomToken()
	_, err = s.db.Exec(r.Context(), `INSERT INTO email_verifications(user_id,token_hash,expires_at) VALUES($1,$2,now()+interval '24 hours')`, id, hash(token))
	if err != nil {
		errorJSON(w, 500, "could not create verification")
		return
	}
	if s.sendgridKey != "" {
		s.sendVerification(input.Email, input.DisplayName, token)
		writeJSON(w, 201, map[string]string{"id": id, "message": "account created; check your email to verify your account"})
		return
	}
	log.Printf("verification token for %s: %s", input.Email, token)
	writeJSON(w, 201, map[string]string{"id": id, "message": "account created; use the development token below to verify", "verification_token": token})
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	var input userRequest
	if !decode(w, r, &input) {
		return
	}
	var found user
	var passwordHash string
	var verified *time.Time
	err := s.db.QueryRow(r.Context(), `SELECT id,email,password_hash,display_name,role,verified_at FROM users WHERE email=$1`, strings.ToLower(strings.TrimSpace(input.Email))).Scan(&found.ID, &found.Email, &passwordHash, &found.DisplayName, &found.Role, &verified)
	if err != nil || bcrypt.CompareHashAndPassword([]byte(passwordHash), []byte(input.Password)) != nil {
		errorJSON(w, 401, "invalid credentials")
		return
	}
	if verified == nil {
		errorJSON(w, 403, "email verification required")
		return
	}
	token := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{UserID: found.ID, Role: found.Role, RegisteredClaims: jwt.RegisteredClaims{ExpiresAt: jwt.NewNumericDate(time.Now().Add(24 * time.Hour)), IssuedAt: jwt.NewNumericDate(time.Now())}})
	signed, err := token.SignedString(s.jwtSecret)
	if err != nil {
		errorJSON(w, 500, "could not issue token")
		return
	}
	writeJSON(w, 200, map[string]any{"token": signed, "user": found})
}

func (s *Server) verify(w http.ResponseWriter, r *http.Request) {
	token := r.URL.Query().Get("token")
	if token == "" {
		errorJSON(w, 400, "token is required")
		return
	}
	var userID string
	err := s.db.QueryRow(r.Context(), `DELETE FROM email_verifications WHERE token_hash=$1 AND expires_at>now() RETURNING user_id`, hash(token)).Scan(&userID)
	if errors.Is(err, pgx.ErrNoRows) {
		errorJSON(w, 400, "invalid or expired token")
		return
	}
	if err != nil {
		errorJSON(w, 500, "verification failed")
		return
	}
	_, err = s.db.Exec(r.Context(), `UPDATE users SET verified_at=now() WHERE id=$1`, userID)
	if err != nil {
		errorJSON(w, 500, "verification failed")
		return
	}
	writeJSON(w, 200, map[string]string{"message": "email verified"})
}

func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		parsed, err := jwt.ParseWithClaims(raw, &claims{}, func(token *jwt.Token) (any, error) {
			if token.Method != jwt.SigningMethodHS256 {
				return nil, errors.New("unexpected signing method")
			}
			return s.jwtSecret, nil
		})
		if err != nil || !parsed.Valid {
			errorJSON(w, 401, "valid bearer token required")
			return
		}
		c := parsed.Claims.(*claims)
		ctx := context.WithValue(r.Context(), "claims", c)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
func current(r *http.Request) *claims { return r.Context().Value("claims").(*claims) }
func (s *Server) invalidateFeed(ctx context.Context) {
	var cursor uint64
	for {
		keys, next, err := s.rdb.Scan(ctx, cursor, "feed:v1:*", 100).Result()
		if err != nil {
			return
		}
		if len(keys) > 0 {
			_ = s.rdb.Del(ctx, keys...).Err()
		}
		cursor = next
		if cursor == 0 {
			return
		}
	}
}
func (s *Server) rateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := "rate:" + r.RemoteAddr
		count, err := s.rdb.Incr(r.Context(), key).Result()
		if count == 1 {
			_ = s.rdb.Expire(r.Context(), key, time.Minute).Err()
		}
		if err == nil && count > 120 {
			errorJSON(w, 429, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, r)
	})
}
func (s *Server) cors(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Headers", "Authorization, Content-Type")
		if r.Method == "OPTIONS" {
			w.WriteHeader(204)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) feed(w http.ResponseWriter, r *http.Request) {
	cacheKey := "feed:v1:" + r.URL.Query().Get("page")
	if cached, err := s.rdb.Get(r.Context(), cacheKey).Result(); err == nil {
		w.Header().Set("X-Cache", "HIT")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(cached))
		return
	}
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	limit := 20
	offset := (page - 1) * limit
	rows, err := s.db.Query(r.Context(), `SELECT p.id,p.author_id,u.display_name,p.title,p.body,p.created_at,p.updated_at FROM posts p JOIN users u ON u.id=p.author_id ORDER BY p.created_at DESC,p.id DESC LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		errorJSON(w, 500, "could not load feed")
		return
	}
	defer rows.Close()
	posts := []post{}
	for rows.Next() {
		var item post
		if err := rows.Scan(&item.ID, &item.AuthorID, &item.AuthorName, &item.Title, &item.Body, &item.CreatedAt, &item.UpdatedAt); err == nil {
			posts = append(posts, item)
		}
	}
	payload, _ := json.Marshal(map[string]any{"page": page, "posts": posts})
	_ = s.rdb.Set(r.Context(), cacheKey, payload, 15*time.Second).Err()
	w.Header().Set("X-Cache", "MISS")
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(payload)
}

func (s *Server) createPost(w http.ResponseWriter, r *http.Request) {
	var input postRequest
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Title) == "" || strings.TrimSpace(input.Body) == "" {
		errorJSON(w, 400, "title and body are required")
		return
	}
	var item post
	err := s.db.QueryRow(r.Context(), `INSERT INTO posts(author_id,title,body) VALUES($1,$2,$3) RETURNING id,author_id,title,body,created_at,updated_at`, current(r).UserID, strings.TrimSpace(input.Title), strings.TrimSpace(input.Body)).Scan(&item.ID, &item.AuthorID, &item.Title, &item.Body, &item.CreatedAt, &item.UpdatedAt)
	if err != nil {
		errorJSON(w, 500, "could not create post")
		return
	}
	_ = s.db.QueryRow(r.Context(), `SELECT display_name FROM users WHERE id=$1`, item.AuthorID).Scan(&item.AuthorName)
	s.invalidateFeed(r.Context())
	writeJSON(w, 201, item)
}
func (s *Server) updatePost(w http.ResponseWriter, r *http.Request) {
	var input postRequest
	if !decode(w, r, &input) {
		return
	}
	var item post
	err := s.db.QueryRow(r.Context(), `UPDATE posts SET title=$1,body=$2,updated_at=now() WHERE id=$3 AND (author_id=$4 OR $5='admin') RETURNING id,author_id,title,body,created_at,updated_at`, strings.TrimSpace(input.Title), strings.TrimSpace(input.Body), pathID(r), current(r).UserID, current(r).Role).Scan(&item.ID, &item.AuthorID, &item.Title, &item.Body, &item.CreatedAt, &item.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		errorJSON(w, 404, "post not found or not permitted")
		return
	}
	if err != nil {
		errorJSON(w, 500, "could not update post")
		return
	}
	s.invalidateFeed(r.Context())
	writeJSON(w, 200, item)
}
func (s *Server) deletePost(w http.ResponseWriter, r *http.Request) {
	result, err := s.db.Exec(r.Context(), `DELETE FROM posts WHERE id=$1 AND (author_id=$2 OR $3='admin')`, pathID(r), current(r).UserID, current(r).Role)
	if err != nil {
		errorJSON(w, 500, "could not delete post")
		return
	}
	if result.RowsAffected() == 0 {
		errorJSON(w, 404, "post not found or not permitted")
		return
	}
	s.invalidateFeed(r.Context())
	w.WriteHeader(204)
}

func (s *Server) listComments(w http.ResponseWriter, r *http.Request) {
	rows, err := s.db.Query(r.Context(), `SELECT c.id,c.post_id,c.author_id,u.display_name,c.parent_id,c.body,c.created_at FROM comments c JOIN users u ON u.id=c.author_id WHERE c.post_id=$1 ORDER BY c.created_at,c.id`, pathID(r))
	if err != nil {
		errorJSON(w, 500, "could not load comments")
		return
	}
	defer rows.Close()
	all := []comment{}
	byParent := map[string][]comment{}
	for rows.Next() {
		var item comment
		var parent *string
		if err := rows.Scan(&item.ID, &item.PostID, &item.AuthorID, &item.AuthorName, &parent, &item.Body, &item.CreatedAt); err == nil {
			item.ParentID = parent
			if parent == nil {
				all = append(all, item)
			} else {
				byParent[*parent] = append(byParent[*parent], item)
			}
		}
	}
	var attach func([]comment)
	attach = func(items []comment) {
		for i := range items {
			items[i].Replies = byParent[items[i].ID]
			attach(items[i].Replies)
		}
	}
	attach(all)
	writeJSON(w, 200, map[string]any{"comments": all})
}
func (s *Server) createComment(w http.ResponseWriter, r *http.Request) {
	var input commentRequest
	if !decode(w, r, &input) {
		return
	}
	if strings.TrimSpace(input.Body) == "" {
		errorJSON(w, 400, "body is required")
		return
	}
	var item comment
	var parent any = nil
	if input.ParentID != "" {
		parent = input.ParentID
	}
	err := s.db.QueryRow(r.Context(), `INSERT INTO comments(post_id,author_id,parent_id,body) SELECT $1,$2,$3,$4 WHERE EXISTS(SELECT 1 FROM posts WHERE id=$1) AND ($3::uuid IS NULL OR EXISTS(SELECT 1 FROM comments parent WHERE parent.id=$3::uuid AND parent.post_id=$1)) RETURNING id,post_id,author_id,parent_id,body,created_at`, pathID(r), current(r).UserID, parent, strings.TrimSpace(input.Body)).Scan(&item.ID, &item.PostID, &item.AuthorID, &item.ParentID, &item.Body, &item.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		errorJSON(w, 404, "post not found")
		return
	}
	if err != nil {
		errorJSON(w, 400, "invalid parent comment or comment body")
		return
	}
	_ = s.db.QueryRow(r.Context(), `SELECT display_name FROM users WHERE id=$1`, item.AuthorID).Scan(&item.AuthorName)
	writeJSON(w, 201, item)
}

func (s *Server) sendVerification(email, name, token string) {
	body, _ := json.Marshal(map[string]any{
		"personalizations": []any{map[string]any{"to": []any{map[string]string{"email": email, "name": name}}}},
		"from":             map[string]string{"email": s.sendgridFrom, "name": "Blog Social"},
		"subject":          "Verify your Blog Social account",
		"content":          []any{map[string]string{"type": "text/plain", "value": fmt.Sprintf("Open http://localhost:%s/api/v1/auth/verify?token=%s", env("PORT", "8080"), token)}},
	})
	request, err := http.NewRequest(http.MethodPost, "https://api.sendgrid.com/v3/mail/send", bytes.NewReader(body))
	if err != nil {
		log.Printf("create verification email request: %v", err)
		return
	}
	request.Header.Set("Authorization", "Bearer "+s.sendgridKey)
	request.Header.Set("Content-Type", "application/json")
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		log.Printf("send verification email: %v", err)
		return
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		log.Printf("send verification email returned %s", response.Status)
	}
}
