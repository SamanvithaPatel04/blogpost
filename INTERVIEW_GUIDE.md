# Blogpost Platform: Interview Deep-Dive Guide

This is an interview preparation guide for the `blogpost` project. It explains what was built, how requests move through the system, why each technology is present, what the current code actually does, how to test it, and what improvements should be proposed in a production discussion.

The most important interview rule is to describe the current implementation honestly. Some production-grade improvements are listed separately under **Next Improvements**; they should not be presented as already implemented.

## 1. One-Minute Project Summary

> Blogpost is a Go REST API with an embedded browser client for a social blogging workflow. PostgreSQL stores users, posts, comments, and email-verification records. Passwords are hashed with bcrypt, login issues an HMAC-SHA256 JWT containing the user ID and role, and middleware authenticates bearer tokens. Redis caches paginated feed responses for 15 seconds and tracks request counts for rate limiting. Users must verify their email before login, can create posts and threaded comments, and admins can edit or delete any post while regular users can manage their own posts. The service ships as a single Go binary and can run locally with PostgreSQL and Redis or in a container.

## 2. What Was Built

The project started from an empty workspace and was built as a small, self-contained service:

- Go HTTP server using the standard library `net/http` router.
- PostgreSQL connection pooling through `pgxpool`.
- Redis client through `go-redis/v9`.
- JWT signing and validation through `golang-jwt/jwt/v5`.
- bcrypt password hashing through `golang.org/x/crypto/bcrypt`.
- SendGrid email delivery through its v3 HTTP API using `net/http`.
- PostgreSQL migration with foreign keys, checks, and indexes.
- Embedded HTML, CSS, and JavaScript frontend using Go `embed`.
- Docker Compose definition for local PostgreSQL and Redis.
- Multi-stage Dockerfile for deploying the API container.
- Root documentation in `README.md`, `IMPLEMENTATION.md`, and this guide.

The API is available at `http://localhost:8080` when running locally.

## 3. Repository Layout

```text
blogpost/
├── cmd/api/main.go
├── cmd/api/web/index.html
├── cmd/api/web/styles.css
├── cmd/api/web/app.js
├── migrations/001_init.sql
├── docker-compose.yml
├── Dockerfile
├── .env.example
├── .gitignore
├── go.mod
├── go.sum
├── README.md
├── IMPLEMENTATION.md
└── INTERVIEW_GUIDE.md
```

### `cmd/api/main.go`

This is the current application entry point and handler implementation. It contains:

- Configuration loading through environment variables.
- PostgreSQL pool creation and ping.
- Redis client creation.
- Route registration.
- Embedded frontend serving.
- JSON response helpers.
- Registration, login, verification, feed, post, and comment handlers.
- JWT authentication middleware.
- Redis feed invalidation.
- Redis rate-limiting middleware.
- SendGrid HTTP request creation.

### `cmd/api/web/index.html`

The browser interface contains:

- Sign-in form.
- Registration form.
- Local verification-token form.
- Feed and cache-status display.
- Post composer.
- Post edit/delete actions.
- Comment-thread display and comment form.

### `cmd/api/web/styles.css`

This is a responsive, dependency-free visual layer. It uses CSS variables, a desktop three-column authentication layout, a two-column signed-in layout, and mobile media queries.

### `cmd/api/web/app.js`

This is the browser API client. It:

- Stores the JWT and current user in `localStorage`.
- Adds `Authorization: Bearer <token>` to authenticated requests.
- Calls the authentication, feed, post, and comment endpoints.
- Renders the feed and nested comments.
- Shows `X-Cache` state.
- Handles local verification-token autofill.

### `migrations/001_init.sql`

This creates the PostgreSQL extension, tables, constraints, foreign keys, and indexes.

### `docker-compose.yml`

This starts PostgreSQL 16 and Redis 7. PostgreSQL mounts the migration into its initialization directory, so the migration runs automatically for a new database volume.

### `Dockerfile`

This uses a multi-stage build:

1. A Go builder downloads modules and compiles a static-ish binary.
2. A smaller Alpine image receives only the binary.
3. The process runs as a non-root `appuser` and exposes port 8080.

## 4. End-to-End Request Flows

### 4.1 Registration flow

1. The browser sends `POST /api/v1/auth/register` with:

   ```json
   {
     "email": "person@example.com",
     "password": "password123",
     "displayName": "Person"
   }
   ```

2. The handler limits the request body to 1 MiB with `http.MaxBytesReader` and decodes JSON.
3. The email is trimmed and lowercased.
4. The display name is trimmed.
5. Basic validation requires an email containing `@`, a non-empty display name, and a password of at least eight characters.
6. The password is hashed with bcrypt. The plaintext password is never stored.
7. A user row is inserted. PostgreSQL generates a UUID with `gen_random_uuid()`.
8. A cryptographically random 32-byte token is generated using `crypto/rand` and hex encoded.
9. Only the SHA-256 hash of that token is stored in `email_verifications`.
10. The token expires after 24 hours.
11. If `SENDGRID_API_KEY` exists, the API posts a verification email to SendGrid.
12. Without SendGrid, the token is logged and included in the response for local development.
13. The browser places a local development token into the verification form.

A duplicate email returns HTTP 409.

#### Registration security reasoning

The raw verification token is not stored in PostgreSQL. If someone reads the verification table, they cannot directly use the stored hash as the token. The local development response intentionally exposes the raw token only when SendGrid is not configured.

#### Current registration limitation

The user insert and verification-token insert are separate database operations. If the second operation fails after the user is created, the user remains without a token. A production version should wrap both operations in one PostgreSQL transaction and enqueue email delivery after commit.

### 4.2 Verification flow

1. The browser sends `GET /api/v1/auth/verify?token=<raw-token>`.
2. The API hashes the supplied token with SHA-256.
3. It deletes a matching, unexpired verification row and returns its user ID in the same SQL statement.
4. A successful delete marks `users.verified_at` with the current database time.
5. A missing or expired token returns HTTP 400.
6. Since the verification row is deleted on success, the token is single-use.

#### Why delete while consuming?

Deleting the row prevents reuse and avoids a separate race between checking a token and consuming it. For a stronger all-or-nothing implementation, the delete and `verified_at` update should be in one transaction.

### 4.3 Login flow

1. The browser sends `POST /api/v1/auth/login` with email and password.
2. The API lowercases the email and selects the user, password hash, role, display name, and verification timestamp.
3. bcrypt compares the submitted password against the stored hash.
4. Invalid credentials return HTTP 401.
5. A correct but unverified account returns HTTP 403.
6. A verified account receives a JWT signed with the configured `JWT_SECRET`.
7. The token expires 24 hours after issuance.
8. The response contains the token and user profile data.
9. The browser stores the token and user object in `localStorage`.

### 4.4 Authenticated request flow

1. The browser sends an `Authorization` header:

   ```http
   Authorization: Bearer eyJ...
   ```

2. The auth middleware removes the `Bearer ` prefix.
3. `jwt.ParseWithClaims` verifies the token signature and registered claims.
4. The key callback rejects algorithms other than `HS256`.
5. The API uses the configured server-side secret to validate the signature.
6. Expired or invalid tokens return HTTP 401.
7. Valid claims are placed in the request context.
8. Handlers access the authenticated user ID and role through `current(r)`.

The route registration wraps the feed, post, and comment endpoints with this middleware. Registration, login, and verification are public.

### 4.5 Feed flow

1. The browser sends `GET /api/v1/feed?page=1` with a JWT.
2. The handler constructs a Redis key using the page number: `feed:v1:<page>`.
3. It first attempts `GET` in Redis.
4. On a cache hit, it returns the cached JSON and header `X-Cache: HIT`.
5. On a miss, it normalizes the page to at least 1, uses page size 20, and calculates the SQL offset.
6. PostgreSQL joins posts with users to include author display names.
7. Posts are ordered newest-first by `created_at DESC, id DESC`.
8. The JSON result is stored in Redis for 15 seconds.
9. The response includes `X-Cache: MISS`.

The UI displays whether the current feed response was fresh from PostgreSQL or served from Redis.

#### Why use `id` as a tie-breaker?

Two posts can share a timestamp at database precision. Ordering by both timestamp and UUID gives a deterministic order rather than allowing equal timestamps to shuffle between requests.

#### Current feed wording

The original product description says “real-time feeds.” The current implementation provides fast refreshable feeds with short-lived Redis caching; it does not implement WebSockets, Server-Sent Events, or push notifications. A true real-time version would add an event/publish layer and a browser subscription mechanism.

### 4.6 Post creation flow

1. The authenticated browser submits title and body to `POST /api/v1/posts`.
2. The JSON body is limited to 1 MiB.
3. The handler rejects blank title or body.
4. PostgreSQL inserts the post using the authenticated user ID.
5. The inserted post is returned with timestamps.
6. The author display name is loaded for the response.
7. All cached feed pages matching `feed:v1:*` are deleted.
8. The API returns HTTP 201.

### 4.7 Post update flow

1. The browser sends `PUT /api/v1/posts/{id}`.
2. The handler updates title, body, and `updated_at`.
3. The SQL `WHERE` clause requires either the authenticated user to own the post or the JWT role to be `admin`.
4. No matching or permitted row returns HTTP 404.
5. Successful updates invalidate feed cache pages.
6. The updated post is returned.

### 4.8 Post delete flow

1. The browser sends `DELETE /api/v1/posts/{id}`.
2. The SQL `WHERE` clause applies the same owner-or-admin rule.
3. PostgreSQL deletes the post.
4. Cascading foreign keys delete its comments.
5. Feed cache pages are invalidated.
6. Success returns HTTP 204.

### 4.9 Comment listing flow

1. The browser sends `GET /api/v1/posts/{id}/comments`.
2. PostgreSQL loads all comments for the post joined with author display names.
3. Results are sorted by creation time and ID.
4. Top-level comments are placed in an output slice.
5. Replies are grouped by `parent_id` in a map.
6. A recursive function attaches each group to its parent.
7. The API returns a nested JSON tree.

### 4.10 Comment creation flow

1. The browser sends `POST /api/v1/posts/{id}/comments`.
2. The request includes a body and optionally `parent_id`.
3. The SQL insert only succeeds when the post exists.
4. If a parent is supplied, the SQL also requires that the parent comment belongs to the same post.
5. PostgreSQL foreign keys enforce valid user, post, and parent references.
6. The response includes the author display name.

The current browser UI creates top-level comments. The API supports replies by sending `parent_id` from another client or API tool.

## 5. Database Design

### `users`

| Column | Purpose |
|---|---|
| `id UUID` | Primary key generated by PostgreSQL |
| `email TEXT` | Normalized unique login identifier |
| `password_hash TEXT` | bcrypt output, never plaintext |
| `display_name TEXT` | Public author/profile name |
| `role TEXT` | `user` or `admin` |
| `verified_at TIMESTAMPTZ` | Null until email verification |
| `created_at TIMESTAMPTZ` | Creation timestamp |

The `role` check constraint prevents values other than `user` and `admin`. The unique email constraint prevents duplicate accounts.

### `posts`

| Column | Purpose |
|---|---|
| `id UUID` | Post identity |
| `author_id UUID` | Foreign key to `users` |
| `title TEXT` | 1 to 160 characters |
| `body TEXT` | 1 to 50,000 characters |
| `created_at TIMESTAMPTZ` | Feed ordering |
| `updated_at TIMESTAMPTZ` | Edit tracking |

### `comments`

| Column | Purpose |
|---|---|
| `id UUID` | Comment identity |
| `post_id UUID` | Owning post |
| `author_id UUID` | Comment author |
| `parent_id UUID` | Null for top-level, otherwise parent comment |
| `body TEXT` | 1 to 5,000 characters |
| `created_at TIMESTAMPTZ` | Thread ordering |

`ON DELETE CASCADE` on posts means deleting a post deletes its comments. The parent relationship also cascades child replies when a parent comment is deleted.

### `email_verifications`

| Column | Purpose |
|---|---|
| `user_id UUID` | One current token per user |
| `token_hash TEXT` | SHA-256 hash of raw token |
| `expires_at TIMESTAMPTZ` | 24-hour expiration |

The primary key on `user_id` means the design supports one active verification token per user.

### Indexes

- `posts_feed_idx (created_at DESC, id DESC)`: supports newest-first feed ordering.
- `posts_author_idx (author_id, created_at DESC)`: supports author post lookups.
- `comments_thread_idx (post_id, parent_id, created_at, id)`: supports loading and ordering a post's comment tree.

### ACID explanation

PostgreSQL provides atomicity, consistency, isolation, and durability for each SQL statement and for explicit transactions. Foreign keys and check constraints maintain consistency. The current individual post and comment mutations are durable database writes. The registration workflow would be stronger if its related user and verification inserts were enclosed in an explicit transaction.

## 6. JWT and Role-Based Authorization

### JWT payload

The custom claims contain:

```json
{
  "sub": "user-uuid",
  "role": "user",
  "iat": 1767140000,
  "exp": 1767226400
}
```

The exact numeric timestamps vary. `sub` identifies the user. `role` controls owner/admin behavior. `iat` records issuance time and `exp` limits token lifetime.

### Why JWT?

- The API can authenticate a request without a session lookup on every request.
- The token is easy for the browser client to attach.
- The role travels with the authenticated identity.
- Expiration provides a bounded lifetime.

### JWT tradeoffs

JWTs are signed, not encrypted. Anyone holding a token can decode its claims, so no password or private data belongs in it. The server must protect `JWT_SECRET`. Since tokens are stateless, immediate revocation is not automatic.

### Current role enforcement

There is no separate `requireRole("admin")` middleware. Admin authorization is enforced in the SQL update and delete predicates:

```sql
WHERE id = $1
  AND (author_id = $2 OR $3 = 'admin')
```

That makes the database mutation itself refuse unauthorized rows. Regular users can operate on their own posts; an admin can operate on any post.

### Current role limitation

The role is read at login and copied into the JWT. If a user's database role changes, an existing token retains its previous role until it expires. A production system could use short-lived access tokens plus refresh tokens, include a token version, or re-check current role for sensitive operations.

### Current secret limitation

The code has a development fallback secret. Production must set a long random `JWT_SECRET` and should fail startup if it is missing rather than silently using the development fallback.

## 7. Redis Design

### Feed cache

- Key format: `feed:v1:<page>`.
- Value: serialized JSON response.
- TTL: 15 seconds.
- Read path: Redis first, PostgreSQL on miss.
- Write path: post create/update/delete scans and deletes matching feed keys.
- Client-visible signal: `X-Cache: HIT` or `X-Cache: MISS`.

### Cache invalidation tradeoff

The implementation uses Redis `SCAN` rather than `KEYS`, avoiding one large blocking key scan. For a very large keyspace, a production system might use versioned keys, a feed generation counter, or a targeted invalidation strategy rather than scanning all feed pages after every mutation.

### Rate limiting

The middleware increments a Redis key and gives it a one-minute expiration when first created. Counts above 120 are rejected with HTTP 429.

Current conceptual algorithm:

```text
key = rate:<client-address>
count = INCR key
if count == 1:
    EXPIRE key 60 seconds
if count > 120:
    return 429
otherwise:
    continue
```

### Rate-limit production caveat

The current key uses `r.RemoteAddr`. In Go, `RemoteAddr` can include a source port, and source ports may vary across connections. A production implementation should normalize to the client IP, account for trusted reverse proxies, and use an atomic Redis Lua script or a tested fixed-window/sliding-window algorithm so increment and expiration cannot be separated by failure.

## 8. SendGrid Integration

The API does not rely on the SendGrid Go SDK. It constructs the SendGrid v3 request with the standard library:

- URL: `https://api.sendgrid.com/v3/mail/send`.
- Method: `POST`.
- Authorization: `Bearer <SENDGRID_API_KEY>`.
- Content type: `application/json`.
- Sender: `SENDGRID_FROM_EMAIL`.
- Recipient: newly registered user's email.
- Message: local verification URL containing the raw token.

The key is read from the environment and never hardcoded. If the API request fails, the server logs the error. A production system should use a durable outbox or queue, retries with backoff, and avoid making registration latency depend directly on the email provider.

## 9. API Contract

| Method | Route | Auth | Purpose |
|---|---|---:|---|
| `POST` | `/api/v1/auth/register` | No | Create account and verification token |
| `POST` | `/api/v1/auth/login` | No | Validate credentials and issue JWT |
| `GET` | `/api/v1/auth/verify?token=...` | No | Consume email verification token |
| `GET` | `/api/v1/feed?page=1` | Yes | Read paginated feed |
| `POST` | `/api/v1/posts` | Yes | Create a post |
| `PUT` | `/api/v1/posts/{id}` | Yes | Update own/admin-accessible post |
| `DELETE` | `/api/v1/posts/{id}` | Yes | Delete own/admin-accessible post |
| `GET` | `/api/v1/posts/{id}/comments` | Yes | Read nested comments |
| `POST` | `/api/v1/posts/{id}/comments` | Yes | Create comment or reply |
| `GET` | `/` | No | Serve embedded browser frontend |

Common status codes:

- `200`: successful read/update/login/verification.
- `201`: account/post/comment created.
- `204`: post deleted or CORS preflight.
- `400`: malformed JSON, missing input, or invalid verification token.
- `401`: missing or invalid JWT, or invalid credentials.
- `403`: account has not been verified.
- `404`: resource not found or operation not permitted.
- `409`: duplicate email registration.
- `429`: Redis rate limit exceeded.
- `500`: database or server failure.

## 10. Frontend Architecture

The frontend is deliberately small and dependency-free.

### Session handling

After login, JavaScript stores the JWT and user profile in `localStorage`. Every API request helper adds the bearer header when a token exists. Logout removes the stored values and returns the interface to the authentication screen.

For a production application, storing access tokens in localStorage increases exposure to XSS. A stronger browser design would use secure, HttpOnly, SameSite cookies and CSRF protection, or a carefully designed short-lived access-token/refresh-token scheme.

### Rendering

The feed is fetched as JSON and converted into post HTML. User-controlled values are passed through an escaping helper before insertion into HTML. Comments are recursively rendered from the nested `replies` arrays.

The browser also displays:

- Current user profile.
- API connected status.
- PostgreSQL fresh versus Redis cached feed state.
- Error messages and success toasts.

### Browser bug fixes made during implementation

- The post form originally accessed `event.currentTarget` after an `await`, when it could be null. The form element is now captured before the asynchronous request.
- Go's default JSON field names did not match the frontend's snake_case property access. Explicit JSON tags were added for users, posts, and comments.
- The root embedded-file route initially produced a directory listing/redirect loop. The root now serves `web/index.html` directly.
- The verification input was susceptible to browser autofill with an email address. Autofill is disabled for that field, and local registration inserts the development token automatically.

## 11. Startup and Local Infrastructure

### Docker path

```sh
docker compose up -d
go mod tidy
go run ./cmd/api
```

The Compose file exposes:

- PostgreSQL on port 5432.
- Redis on port 6379.

### macOS Homebrew path used locally

```sh
brew install postgresql@16 redis
brew services start postgresql@16
brew services start redis
```

Create the role and database:

```sh
/opt/homebrew/opt/postgresql@16/bin/psql -d postgres -c "DO \$\$ BEGIN CREATE ROLE blog LOGIN PASSWORD 'blog'; EXCEPTION WHEN duplicate_object THEN NULL; END \$\$;"
/opt/homebrew/opt/postgresql@16/bin/createdb -O blog blog_social
```

Apply the migration:

```sh
/opt/homebrew/opt/postgresql@16/bin/psql \
  'postgres://blog:blog@localhost:5432/blog_social?sslmode=disable' \
  -f migrations/001_init.sql
```

Run the API:

```sh
go run ./cmd/api
```

Open:

```text
http://localhost:8080/
```

### Startup sequence

The process exits early if PostgreSQL cannot be reached. It parses the Redis URL before constructing the Redis client, then starts the HTTP listener. PostgreSQL and Redis therefore need to be available before `go run ./cmd/api`.

## 12. Manual Test Plan

1. Start PostgreSQL and Redis.
2. Apply the migration.
3. Start the API.
4. Open the frontend.
5. Register a new account.
6. Confirm the local development token appears in the verification form when SendGrid is not configured.
7. Verify the email.
8. Log in.
9. Publish a post.
10. Confirm the post author name is displayed.
11. Open the comment thread.
12. Add a comment.
13. Use an API client to add a reply with `parent_id`.
14. Refresh the feed and inspect the cache label.
15. Edit the post.
16. Delete the post.
17. Attempt to edit another user's post with a normal-user JWT and confirm it is rejected.
18. Promote a user to admin in PostgreSQL, log in again to get a new JWT, and confirm the admin can manage another user's post.

## 13. Useful API Test Commands

Register locally:

```sh
curl -sS -X POST http://localhost:8080/api/v1/auth/register \
  -H 'Content-Type: application/json' \
  -d '{"email":"test@example.com","password":"password123","displayName":"Test User"}'
```

The response contains `verification_token` when SendGrid is not configured.

Verify:

```sh
curl -sS 'http://localhost:8080/api/v1/auth/verify?token=PASTE_TOKEN_HERE'
```

Login:

```sh
curl -sS -X POST http://localhost:8080/api/v1/auth/login \
  -H 'Content-Type: application/json' \
  -d '{"email":"test@example.com","password":"password123"}'
```

Read the feed:

```sh
curl -i http://localhost:8080/api/v1/feed?page=1 \
  -H 'Authorization: Bearer PASTE_JWT_HERE'
```

The headers show `X-Cache: MISS` on a database load and `X-Cache: HIT` on a cached load.

Create a post:

```sh
curl -sS -X POST http://localhost:8080/api/v1/posts \
  -H 'Authorization: Bearer PASTE_JWT_HERE' \
  -H 'Content-Type: application/json' \
  -d '{"title":"My first post","body":"A persistent thought."}'
```

Create a comment:

```sh
curl -sS -X POST http://localhost:8080/api/v1/posts/POST_ID/comments \
  -H 'Authorization: Bearer PASTE_JWT_HERE' \
  -H 'Content-Type: application/json' \
  -d '{"body":"Interesting perspective."}'
```

Create a reply:

```sh
curl -sS -X POST http://localhost:8080/api/v1/posts/POST_ID/comments \
  -H 'Authorization: Bearer PASTE_JWT_HERE' \
  -H 'Content-Type: application/json' \
  -d '{"body":"Following up on that.","parent_id":"COMMENT_ID"}'
```

## 14. Testing and Validation Performed

The following checks passed during implementation:

```sh
go test ./...
go vet ./...
node --check cmd/api/web/app.js
go build -o /tmp/blog-social-api ./cmd/api
```

The service was also tested live with:

- HTTP request to the embedded root frontend.
- HTTP requests for CSS and JavaScript assets.
- Registration.
- Development-token verification.
- Login.
- Post creation.
- Author-name response validation.
- PostgreSQL port check.
- Redis port check.

There are currently no dedicated automated integration test files. `go test ./...` currently confirms compilation and package validity. A stronger next step is an integration test suite that starts disposable PostgreSQL and Redis instances or uses test containers.

## 15. Deployment and GitHub

The code was initialized as a Git repository on the `main` branch and pushed to:

```text
https://github.com/SamanvithaPatel04/blogpost
```

The repository is private. `.gitignore` excludes `.env`, logs, temporary files, and local binaries.

The Dockerfile is suitable for container hosts such as Railway, Render, Fly.io, or Google Cloud Run. A real hosted deployment still needs:

- A managed PostgreSQL database.
- A managed Redis instance.
- A secure JWT secret.
- Correct `DATABASE_URL` and `REDIS_URL` values.
- A verified SendGrid sender if email delivery is enabled.
- A host-provided `PORT` value.
- TLS/HTTPS and domain configuration.

GitHub stores source code; it does not run this Go/PostgreSQL/Redis backend by itself. GitHub Pages would only be suitable for static frontend files and would not host the API or databases.

## 16. Interview Questions and Strong Answers

### Why Go?

Go provides a fast compiled binary, simple concurrency primitives, a strong standard HTTP library, predictable deployment, and low operational overhead. It is a good fit for a high-concurrency REST API where request handlers spend time on database and Redis I/O.

### Why PostgreSQL instead of a document database?

The domain has relationships and integrity rules: users own posts, posts own comments, comments can reference parents, and verification records reference users. PostgreSQL gives foreign keys, check constraints, unique email enforcement, transactions, and mature indexing. Threaded comments can still be represented naturally with an adjacency-list `parent_id`.

### Why Redis if PostgreSQL is already fast?

Redis reduces repeated reads for the hottest endpoint, the feed. It also provides atomic counters for rate limiting. PostgreSQL remains the source of truth; Redis is disposable and can be rebuilt after eviction or restart.

### What happens if Redis goes down?

In the current code, feed cache reads fall through to PostgreSQL when `GET` misses or errors, but the rate-limit middleware does not distinguish Redis failures from normal traffic cleanly. A production design should define an explicit fail-open or fail-closed policy, add Redis health monitoring, and avoid allowing infrastructure errors to affect authorization.

### How is a password protected?

The plaintext password is hashed with bcrypt during registration. Login uses bcrypt comparison. The hash, not the original password, is stored in PostgreSQL. Password hashing is intentionally expensive to make offline guessing more costly.

### Why hash the email token?

The raw token is a bearer secret. Storing only its SHA-256 hash limits the impact of a database read. The server hashes the submitted token and compares hashes.

### How does the API prevent a user editing another user's post?

The update and delete SQL statements include both the post ID and an authorization predicate: the authenticated user must be the author unless the role claim is `admin`. This causes unauthorized or nonexistent targets to produce no matching row.

### Is the feed real-time?

The current implementation is not push-based real time. It is a short-lived cached feed that the browser refreshes on load, after publishing, or through the refresh control. True real time would require WebSockets or Server-Sent Events plus an event distribution mechanism such as Redis Pub/Sub.

### How would you paginate at very large scale?

The current implementation uses `LIMIT` and `OFFSET`, which is straightforward but becomes less efficient on deep pages. I would move to cursor/keyset pagination using `(created_at, id)` as the cursor, matching the existing deterministic ordering and feed index.

### How would you handle many comments?

The current adjacency list is simple and supports unlimited depth, but recursively materializing a very large tree can be expensive. Options include depth limits, lazy loading replies, a recursive CTE, materialized paths, or a closure table depending on read/write patterns.

### How would you invalidate every feed page more efficiently?

Instead of scanning and deleting every page, maintain a feed version key. Include the version in cache keys, increment it after a write, and let old keys expire naturally. This makes invalidation O(1) at the cost of old cached values remaining temporarily in Redis.

### How would you make email delivery reliable?

Insert an outbox event in the same database transaction as account creation, then have a worker deliver email with retries and exponential backoff. This prevents a SendGrid outage from losing the verification event and keeps registration latency predictable.

### How would you revoke JWTs?

Use short-lived access tokens and refresh tokens, rotate refresh tokens, and store refresh-token state server-side. For immediate access-token revocation, include a user token version or maintain a denylist, accepting the additional lookup/storage cost.

### What are the risks of localStorage JWTs?

Any successful XSS can read localStorage. Production web clients commonly prefer HttpOnly Secure SameSite cookies, with CSRF protection, or carefully isolated token handling. Content Security Policy and output encoding are also important.

### What would you monitor?

I would monitor request latency by route, error rates, PostgreSQL pool saturation, slow queries, Redis hit ratio, Redis latency, rate-limit rejections, SendGrid failures, registration verification conversion, and process health. Structured logs should include request IDs but never passwords, JWTs, or raw verification tokens in production.

### How would you test concurrency?

Use Go race tests for in-process code, integration tests against PostgreSQL and Redis, parallel requests for post creation and feed reads, and load tests for the feed and rate limiter. Verify cache correctness after concurrent writes and ensure authorization cannot be bypassed under interleaving requests.

## 17. Current Limitations to State Clearly

- No WebSocket or Server-Sent Event feed; feed updates are refresh-driven.
- No dedicated automated integration test suite yet.
- Registration's user insert and verification insert are not in one explicit transaction.
- Verification update should ideally be in the same transaction as token consumption.
- Rate limiting should normalize `RemoteAddr` to a trusted client IP.
- Rate-limit increment and expiration are separate Redis commands.
- Development JWT fallback secret should be forbidden in production.
- Existing JWT role claims do not immediately reflect a database role change.
- Browser JWT storage uses localStorage.
- Post update input relies primarily on database checks; handler-level length validation could return clearer 400 responses.
- Feed pagination uses offset rather than a cursor.
- Cache invalidation scans feed keys after writes.
- Email delivery is synchronous and logs failures rather than using an outbox.
- No password reset, refresh-token, account deletion, moderation, following, reactions, notifications, or search features.
- The frontend supports top-level comment creation but does not expose a reply control, although the API accepts `parent_id`.
- The current CORS policy allows all origins and should be restricted for production.
- The API has no explicit health/readiness endpoint.

## 18. Recommended Production Roadmap

1. Add explicit request validation and typed error handling.
2. Add PostgreSQL transactions for registration and verification consumption.
3. Normalize client IPs and implement an atomic Redis rate limiter.
4. Add `/healthz` and `/readyz` endpoints.
5. Add structured logging, request IDs, metrics, and tracing.
6. Move JWTs to secure cookie or access/refresh-token architecture.
7. Require a production JWT secret at startup.
8. Add keyset feed pagination.
9. Replace feed scans with versioned cache keys.
10. Add an email outbox and worker retries.
11. Add integration tests and load tests.
12. Restrict CORS and configure HTTPS.
13. Add database migration versioning and rollback strategy.
14. Add WebSockets or SSE plus Redis Pub/Sub for true real-time feed events.
15. Add moderation, abuse controls, pagination limits, and audit logs.

## 19. Final Interview Closing Statement

> I built the service around PostgreSQL as the source of truth and Redis as an acceleration and protection layer. The request path is intentionally simple: the standard Go router applies rate limiting and CORS, protected routes validate a signed JWT, handlers perform parameterized PostgreSQL queries, and the feed uses Redis with short TTLs and invalidation after mutations. I kept the frontend embedded so the same binary is easy to run and deploy. I can also explain the current tradeoffs: the feed is refresh-based rather than push-based, offset pagination should become keyset pagination at scale, role claims are valid for the token lifetime, and production hardening would add transactions, durable email delivery, stronger token storage, observability, and integration tests.
