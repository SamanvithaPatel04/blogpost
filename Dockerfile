FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags='-s -w' -o /out/blogpost ./cmd/api

FROM alpine:3.20
RUN adduser -D -H appuser
WORKDIR /app
COPY --from=build /out/blogpost /app/blogpost
USER appuser
EXPOSE 8080
ENTRYPOINT ["/app/blogpost"]
