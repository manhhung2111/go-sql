# Publish the Docker Image on Merge to `main`

**Goal:** Every merge to `main` builds the image, runs the existing smoke test against it, and pushes it to a registry tagged with the commit it was built from.

**Scope:** build/CI config only, so per CLAUDE.md it is verified by running the workflow, not by a unit test.

## Decisions

| Question | Choice | Why |
|---|---|---|
| Registry | GHCR, `ghcr.io/manhhung2111/go-sql` | Authenticates with the workflow's own `GITHUB_TOKEN` (`packages: write`); no secret to create or rotate |
| Trigger | `push` to `main` | A merged PR is a push to `main`; this also covers a direct push, which should not be left unpublished |
| Tags | `sha-<short sha>` (e.g. `sha-ccd99ab`) and `latest` | The sha tag is immutable and maps a running container back to its exact commit; `latest` is the moving "newest main" pointer for people who just want to try it. A sha alone makes "what is current?" hard to answer; `latest` alone makes rollbacks and debugging guesswork |
| Gate | The push happens only after the existing smoke test passes on the same image | `ci.yml`/`docker.yml` ran on the PR head, not on the merge result; a broken merge must not be published |
| Platforms | `linux/amd64` only for now | The smoke test `load`s one platform; adding `linux/arm64` means QEMU, a slower build, and an untested arch. Easy to add later |
| Labels | OCI labels from `docker/metadata-action` (`org.opencontainers.image.revision`, `.source`, `.created`) | The commit is readable from the image itself, and `.source` links the GHCR package to this repo |

## Approach

Extend `docker.yml` instead of adding a second workflow, so the image that is published is exactly the image that was smoke-tested, and the build/test steps exist once:

1. Add `push: branches: [main]` to `on:`.
2. Concurrency: keep `cancel-in-progress` for PRs only (`${{ github.event_name == 'pull_request' }}`), so a quick second merge never cancels a publish half-way.
3. Permissions: `contents: read` at the top; the job gets `packages: write` only when it can use it (job-level `permissions` with `packages: write` is harmless on PRs from this repo; fork PRs get a read-only token anyway).
4. New step `docker/metadata-action@v5` computing the tags (`type=sha,prefix=sha-` and `type=raw,value=latest,enable={{is_default_branch}}`) and labels.
5. The existing build step keeps `load: true` and tags the image with both `go-sql:ci` (used by the smoke steps) and the metadata tags, plus the labels.
6. After the smoke test and the SIGTERM check, two steps gated on `github.event_name == 'push'`:
   - `docker/login-action@v3` to `ghcr.io` with `github.actor` / `secrets.GITHUB_TOKEN`
   - `docker push --all-tags ghcr.io/manhhung2111/go-sql` (pushes the already-tested local image; no rebuild)
7. Job summary line with the pushed tags.

## Files

| File | Action |
|---|---|
| `.github/workflows/docker.yml` | Modify as above |
| `README.md` | Add a "Pull the image" snippet (`docker pull ghcr.io/manhhung2111/go-sql:latest`) |
| `CLAUDE.md` | One line under Commands/Architecture: images publish to GHCR on push to `main` |

## Verification

- `actionlint` on the workflow locally (if it can be installed; otherwise a careful read).
- On the PR: the Docker job runs as today and the login/push steps show as skipped.
- After merge: the run on `main` pushes `sha-<sha>` and `latest`; `docker pull ghcr.io/manhhung2111/go-sql:sha-<sha>` works and `docker inspect` shows the revision label matching the merge commit.

## One-time manual step

The first push creates the package as **private**. To make it public: GitHub → Packages → `go-sql` → Package settings → Change visibility. Repository Settings → Actions → General → Workflow permissions does not need changing, since the job requests `packages: write` itself.
