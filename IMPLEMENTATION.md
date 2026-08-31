# Blog Social Platform: Implementation Notes

This document explains the social blogging platform implemented in this repository.

## Overview

The project is a Go REST API with a browser-based frontend. It supports account registration, email verification, JWT authentication, posts, feeds, threaded comments, PostgreSQL persistence, Redis caching, Redis-backed rate limiting, and optional SendGrid email delivery.

The API and frontend are packaged together: the Go binary embeds the files in `cmd/api/web` and serves the interface at `http://localhost:8080/`.

## Technology Stack

- **Go**: HTTP server, routing, request validation, authentication middleware, and application logic.
- **PostgreSQL**: ACID persistence for users, posts, comments, and verification records.
- **Redis**: Feed response caching and request rate limiting.
- **JWT**: Signed bearer tokens containing the user ID, role, issued time, and expiration time.
- **bcrypt**: Password hashing.
- **SendGrid**: Optional email verification delivery through the SendGrid v3 HTTP API.
- **HTML/CSS/JavaScript**: Embedded single-page browser client with no separate frontend build step.

## Project Structure

```text
.
├── cmd/api/main.go              Go API server and handlers
├── cmd/api/web/index.html       Browser application markup
├── cmd/api/web/styles.css       Responsive visual design
├── cmd/api/web/app.js           Browser API client and interactions
├── migrations/001_init.sql     PostgreSQL schema and indexes
├── docker-compose.yml           PostgreSQL and Redis containers
├── .env.example                 Environment variable template
├── go.mod                       Go dependencies
└── README.md                    Quick-start instructions
```

## Backend Features

### Authentication and roles

- `POST /api/v1/auth/register` creates a user with a bcrypt password hash.
- Email addresses are normalized to lowercase.
- New accounts receive a random verification token stored only as a SHA-256 hash.
- Tokens expire after 24 hours and are deleted when successfully used.
- `POST /api/v1/auth/login` checks credentials and verified status, then returns a signed JWT.
- JWT claims include `sub` for the user ID and `role` for role-based access.
- Roles are constrained in PostgreSQL to `user` and `admin`.
- Users can edit or delete their own posts; admins can manage any post.
- All post and comment routes require a valid bearer token.

### Email verification

When `SENDGRID_API_KEY` is configured, registration sends a verification email using `SENDGRID_FROM_EMAIL` as the sender.

When SendGrid is not configured, which is useful for local development, registration returns a development token in the response and logs the same token in the API terminal. The frontend automatically places this token in the Verify email form.

Raw development tokens are not returned when SendGrid is enabled.

### Posts and feed

- Create, edit, and delete post endpoints are available.
- Post titles are limited to 160 characters and bodies to 50,000 characters at the database level.
- The feed is ordered by creation time and uses a stable ID tie-breaker.
- PostgreSQL indexes support feed ordering and author-specific post lookups.
- Feed pages are cached in Redis for 15 seconds.
- Feed responses include `X-Cache: HIT` or `X-Cache: MISS`.
- Feed cache pages are invalidated after post creation, update, or deletion.

### Threaded comments

- Comments belong to a post and an author.
- Replies use `parent_id`.
- A reply parent must belong to the same post as the new comment.
- Comments are returned as a nested reply tree.
- PostgreSQL foreign keys cascade comments when a post is deleted.
- Comment bodies are limited to 5,000 characters.

### Rate limiting

Redis tracks request counts for the API. Each client is allowed 120 requests per minute. Requests above the limit receive HTTP `429 Too Many Requests`.

The middleware is applied around the entire HTTP server, including the frontend and API routes.

## Database Design

The migration creates these tables:

- `users`: identity, password hash, role, verification state, and timestamps.
- `posts`: author, title, body, and creation/update timestamps.
- `comments`: post, author, optional parent, body, and timestamp.
- `email_verifications`: hashed token and expiration timestamp.

Important indexes include:

- `posts_feed_idx` for newest-first feed queries.
- `posts_author_idx` for author post queries.
- `comments_thread_idx` for ordered thread loading.

## Frontend

The browser client provides:

- Registration, login, logout, and local verification.
- Profile display for the signed-in user.
- Feed loading with visible PostgreSQL/Redis cache status.
- Post creation with title and body fields.
- Post editing and deletion.
- Thread expansion for comments.
- Comment creation.
- Responsive layouts for desktop and mobile screens.
- Local browser storage for the JWT and current user session.

The frontend is intentionally dependency-free. Running the Go server is enough to serve it.

## Configuration

Copy the environment template:

```sh
cp .env.example .env
```

Available variables:

```text
DATABASE_URL       PostgreSQL connection string
REDIS_URL          Redis connection string
JWT_SECRET         Secret used to sign JWTs
SENDGRID_API_KEY   Optional SendGrid API key
SENDGRID_FROM_EMAIL Optional verified SendGrid sender address
PORT               HTTP port, default 8080
```

The default local values are:

```text
DATABASE_URL=postgres://blog:blog@localhost:5432/blog_social?sslmode=disable
REDIS_URL=redis://localhost:6379/0
PORT=8080
```

## Running With Docker Compose

If Docker is installed:

```sh
docker compose up -d
go mod tidy
go run ./cmd/api
```

The Compose file starts PostgreSQL 16 and Redis 7. The migration is mounted into PostgreSQL's initialization directory for a new database volume.

For a clean database reset:

```sh
docker compose down -v
docker compose up -d
go run ./cmd/api
```

## Running On macOS With Homebrew

The local environment used during implementation did not have Docker, so PostgreSQL 16 and Redis were installed with Homebrew:

```sh
brew install postgresql@16 redis
brew services start postgresql@16
brew services start redis
```

Create the application role and database:

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

Start the API:

```sh
go run ./cmd/api
```

## Manual Test Flow

1. Open `http://localhost:8080/`.
2. Register with a new email address and a password of at least eight characters.
3. Click **Start writing**.
4. In local mode, confirm the development token was placed in **Verify email**.
5. Click the arrow in the Verify email panel.
6. Sign in with the verified account.
7. Publish a post.
8. Confirm the author name appears in the feed.
9. Open **View thread** and add a comment.
10. Publish a reply using the comment API or a client that supplies `parent_id`.
11. Refresh the feed and observe the cache status change between fresh and cached requests.
12. Edit and delete the post.

If SendGrid is configured, use the verification link in the email instead of the local development token.

## API Smoke Tests

The following checks were run successfully during implementation:

```sh
go test ./...
go vet ./...
node --check cmd/api/web/app.js
go build -o /tmp/blog-social-api ./cmd/api
```

The API was also exercised end to end with registration, token verification, login, and post creation. The frontend root, CSS, and JavaScript assets returned HTTP 200 from the running server.

There are currently no dedicated automated integration test files. The existing `go test ./...` command confirms that the service compiles and packages are valid; the manual/API smoke flow verifies the main local workflow.

## Files Changed During Implementation

The service was built from an initially empty workspace. The main implementation areas are:

- `cmd/api/main.go`: API server, handlers, auth middleware, Redis behavior, SendGrid integration, and embedded frontend serving.
- `cmd/api/web/index.html`: test interface structure.
- `cmd/api/web/styles.css`: visual design and responsive behavior.
- `cmd/api/web/app.js`: browser-side API calls, session handling, feed rendering, posts, and comments.
- `migrations/001_init.sql`: schema, constraints, foreign keys, and indexes.
- `docker-compose.yml`: local PostgreSQL and Redis services.
- `.env.example`: configuration template.
- `README.md`: short quick-start and endpoint reference.
