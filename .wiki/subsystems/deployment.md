---
title: Build, CI and deployment subsystem
kind: subsystem
sources:
  - Dockerfile
  - deploy/config.docker.yml
  - .github/workflows/ci.yml
  - .github/workflows/docker.yml
  - Makefile
  - pr: 18
updated: 09d8ab0
---
# Build, CI and deployment subsystem

## Docker image

The multi-stage `Dockerfile` builds a static binary (`CGO_ENABLED=0`, `-trimpath`) on `golang:1.26.4-alpine` and ships it on `gcr.io/distroless/static-debian12:nonroot`, running as a non-root user. The server is PID 1 in exec form, so `docker stop` delivers SIGTERM and it shuts down gracefully and closes its files. `/data` is created and owned by the non-root user before it becomes a volume, so a fresh volume is writable. `CONFIG_PATH` is set to `/etc/go-sql/config.yml`, copied from `deploy/config.docker.yml` (binds `0.0.0.0:50051`, `data_dir: /data`) (#18). The repo's own `internal/config/config.yml` is unchanged.

## CI

Both workflows trigger only on pull requests into `main`, so stacked PRs are not checked until they are retargeted (#18).

- `ci.yml`: `gofmt -l .` must print nothing, then `go vet ./...`, `go build ./...`, `go test -race ./...`.
- `docker.yml`: builds the image (not pushed), checks it does not run as root, starts it, waits for the server to answer, stops it with SIGTERM and requires exit code 0 and a `server stopped` log line. The readiness check sends a real HTTP/2 request with `curl` because Docker's port proxy accepts connections even when nothing answers behind it (#18).

## Make targets

`make test` (default), `make proto`, `make generate` (Wire), `make docker-build`, `make docker-run`.

## Not covered

The Docker smoke test deliberately does not check persistence (#18). `main` has no branch protection, so the new job is not a required check (#18).

The `init-wiki` and `sync-wiki` skills (`.claude/skills/init-wiki/SKILL.md`, `.claude/skills/sync-wiki/SKILL.md`, #21) build and maintain this wiki. `/init-wiki` is the one-off build and `/sync-wiki` ingests the PRs merged since the marker in `log.md`. Neither commits, pushes or opens a PR; they write only inside `.wiki/` and propose a commit for approval. The conventions they follow are in [SCHEMA.md](../SCHEMA.md), and `.wiki/check.sh` is their validator.
