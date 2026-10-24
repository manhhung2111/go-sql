# Publish the Docker Image on Merge to `main`

**Goal:** Every merge to `main` builds the image, runs the existing smoke test against it, and pushes it to a registry tagged with the commit it was built from.

**Scope:** build/CI config only, so per CLAUDE.md it is verified by running the workflow, not by a unit test.

## Decisions

| Question | Choice | Why |
|---|---|---|
| Registry | Docker Hub, `docker.io/<DOCKERHUB_USERNAME>/go-sql` | The owner's Docker account. The username is the repository variable `DOCKERHUB_USERNAME`, the credential the secret `DOCKERHUB_TOKEN` (a Docker Hub access token with Read & Write scope, not the password) |
| Trigger | `push` to `main` | A merged PR is a push to `main`; this also covers a direct push, which should not be left unpublished |
| Tags | `sha-<short sha>` (e.g. `sha-ccd99ab`) and `latest` | The sha tag is immutable and maps a running container back to its exact commit; `latest` is the moving "newest main" pointer for people who just want to try it. A sha alone makes "what is current?" hard to answer; `latest` alone makes rollbacks and debugging guesswork |
| Gate | The push happens only after the existing smoke test passes on the same image | `ci.yml`/`docker.yml` ran on the PR head, not on the merge result; a broken merge must not be published |
| Platforms | `linux/amd64` only for now | The smoke test `load`s one platform; adding `linux/arm64` means QEMU, a slower build, and an untested arch. Easy to add later |
| Labels | OCI labels from `docker/metadata-action` (`org.opencontainers.image.revision`, `.source`, `.created`) | The commit is readable from the image itself, and `.source` points back to this repo |

## Approach

Extend `docker.yml` instead of adding a second workflow, so the image that is published is exactly the image that was smoke-tested, and the build/test steps exist once:

1. Add `push: branches: [main]` to `on:`.
2. Concurrency: keep `cancel-in-progress` for PRs only (`${{ github.event_name == 'pull_request' }}`), so a quick second merge never cancels a publish half-way.
3. Permissions stay `contents: read`: Docker Hub uses its own token, not `GITHUB_TOKEN`. PR runs never log in or push, and fork PRs do not receive the secret.
4. New step `docker/metadata-action@v5` computing the tags (`type=sha,prefix=sha-` and `type=raw,value=latest,enable={{is_default_branch}}`) and labels.
5. The existing build step keeps `load: true` and tags the image with both `go-sql:ci` (used by the smoke steps) and the metadata tags, plus the labels.
6. After the smoke test and the SIGTERM check, three steps gated on `github.event_name == 'push'`:
   - a check that the variable and secret are set, with a clear error if not
   - `docker/login-action@v3` with `vars.DOCKERHUB_USERNAME` / `secrets.DOCKERHUB_TOKEN`
   - `docker push` of each metadata tag (the already-tested local image; no rebuild)
7. Job summary line with the pushed tags.

## Files

| File | Action |
|---|---|
| `.github/workflows/docker.yml` | Modify as above |
| `README.md` | Say where the image is published and how it is tagged |
| `CLAUDE.md` | One line under Commands/Architecture: images publish to Docker Hub on push to `main` |

## Verification

- `actionlint` on the workflow locally (if it can be installed; otherwise a careful read).
- On the PR: the Docker job runs as today and the login/push steps show as skipped.
- After merge: the run on `main` pushes `sha-<sha>` and `latest`; `docker pull <user>/go-sql:sha-<sha>` works and `docker inspect` shows the revision label matching the merge commit.

## One-time manual setup

1. Docker Hub → Account settings → Personal access tokens → generate one with **Read & Write** scope.
2. GitHub repo → Settings → Secrets and variables → Actions:
   - **Variables** tab: `DOCKERHUB_USERNAME` = the Docker Hub username
   - **Secrets** tab: `DOCKERHUB_TOKEN` = the token from step 1

The `go-sql` repository on Docker Hub is created by the first push, with the account's default visibility.
