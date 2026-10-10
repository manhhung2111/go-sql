---
title: Build, CI and deployment subsystem
kind: subsystem
sources:
  - Dockerfile
  - deploy/config.docker.yml
  - .github/workflows/ci.yml
  - .github/workflows/docker.yml
  - .github/workflows/sync-wiki.yml
  - .claude/skills/sync-wiki/SKILL.md
  - docs/superpowers/specs/2026-10-05-llm-wiki-design.md
  - Makefile
  - pr: 18
  - pr: 23
  - pr: 27
  - pr: 28
  - pr: 29
  - pr: 30
updated: 7660bd0
---
# Build, CI and deployment subsystem

## Docker image

The multi-stage `Dockerfile` builds a static binary (`CGO_ENABLED=0`, `-trimpath`) on `golang:1.26.4-alpine` and ships it on `gcr.io/distroless/static-debian12:nonroot`, running as a non-root user. The server is PID 1 in exec form, so `docker stop` delivers SIGTERM and it shuts down gracefully and closes its files. `/data` is created and owned by the non-root user before it becomes a volume, so a fresh volume is writable. `CONFIG_PATH` is set to `/etc/go-sql/config.yml`, copied from `deploy/config.docker.yml` (binds `0.0.0.0:50051`, `data_dir: /data`) (#18). The repo's own `internal/config/config.yml` is unchanged.

## CI

`ci.yml` and `docker.yml` trigger only on pull requests into `main`, so stacked PRs are not checked until they are retargeted (#18). `sync-wiki.yml` is different: it runs on pushes to `main`.

- `ci.yml`: `gofmt -l .` must print nothing, then `go vet ./...`, `go build ./...`, `go test -race ./...`.
- `docker.yml`: builds the image (not pushed), checks it does not run as root, starts it, waits for the server to answer, stops it with SIGTERM and requires exit code 0 and a `server stopped` log line. The readiness check sends a real HTTP/2 request with `curl` because Docker's port proxy accepts connections even when nothing answers behind it (#18).
- `sync-wiki.yml`: automated workflow running after pushes to `main` (or via `workflow_dispatch`) that executes the `/sync-wiki` skill with Gemini (`sync-wiki.yml`, #23, #27, #28, #29, #30). It sets `GEMINI_CLI_TRUST_WORKSPACE=true` for headless execution, tries models in `GEMINI_MODELS` in order with fallback on failure (503, quota, 404, timeout, or silence), caps retries via `general.maxAttempts: 3` to avoid exhausting daily quotas, lints with `.wiki/check.sh`, appends to `log.md`, and opens/updates a PR via `peter-evans/create-pull-request` with job summaries. The PR is opened with `GITHUB_TOKEN`, so CI workflows do not run on it; `.wiki/check.sh` has already passed in the job.

## Make targets

`make test` (default), `make proto`, `make generate` (Wire), `make docker-build`, `make docker-run`.

## Not covered

The Docker smoke test deliberately does not check persistence (#18). `main` has no branch protection, so the new job is not a required check (#18).

The `init-wiki` and `sync-wiki` skills (`.claude/skills/init-wiki/SKILL.md`, `.claude/skills/sync-wiki/SKILL.md`, #21, #23) build and maintain this wiki. `/init-wiki` is the one-off build and `/sync-wiki` ingests the PRs merged since the marker in `log.md`. Run interactively, neither commits, pushes or opens a PR; they write only inside `.wiki/` and propose a commit for approval. The `sync-wiki.yml` workflow is the deliberate exception: it runs `/sync-wiki` unattended and has `peter-evans/create-pull-request` commit and open the PR. The conventions they follow are in [SCHEMA.md](../SCHEMA.md), and `.wiki/check.sh` is their validator.
