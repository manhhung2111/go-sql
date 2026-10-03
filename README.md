# go-sql
A mini SQL server from scratch in Go

## Run with Docker

```
docker build -t go-sql .
docker run --rm -p 50051:50051 -v go-sql-data:/data go-sql
```

The server listens on port 50051 and keeps its files in the `/data` volume. The image runs as a non-root user, and `docker stop` sends SIGTERM, which shuts the server down gracefully. CI builds the image on every pull request into `main`, starts it, stops it with SIGTERM and requires a clean exit.
