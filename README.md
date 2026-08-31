# Blog Social API

A Go REST API for a blog social platform with PostgreSQL persistence, Redis caching and rate limiting, JWT role-based authentication, threaded comments, and optional SendGrid verification email.

## Run

```sh
cp .env.example .env
docker compose up -d
go mod tidy
go run ./cmd/api
```

The API listens on `http://localhost:8080`.

## Deploying

The included `Dockerfile` packages the API for container hosts such as Railway, Render, Fly.io, or Google Cloud Run. A hosted deployment also needs managed PostgreSQL and Redis services. Configure `DATABASE_URL`, `REDIS_URL`, `JWT_SECRET`, `PORT`, and optional SendGrid variables in the host's environment settings.

The suggested low-friction path is Railway: create a project, deploy this repository as a service, add PostgreSQL and Redis plugins, and copy their connection URLs into the API service variables. I can prepare the repository and deployment configuration, but the final deployment requires your hosting account authorization and billing/project selection.

## Endpoints

- `POST /api/v1/auth/register` creates an account and sends verification when SendGrid is configured.
- `GET /api/v1/auth/verify?token=...` verifies an email.
- `POST /api/v1/auth/login` returns a JWT.
- `GET /api/v1/feed` returns a cached public feed (authenticated).
- `POST /api/v1/posts`, `PUT /api/v1/posts/{id}`, `DELETE /api/v1/posts/{id}` manage posts.
- `GET /api/v1/posts/{id}/comments` lists a threaded comment tree.
- `POST /api/v1/posts/{id}/comments` creates a comment or reply with `parent_id`.

Set `role` to `admin` directly in PostgreSQL for admin access. All write routes require a verified account. Requests are limited per IP through Redis.
