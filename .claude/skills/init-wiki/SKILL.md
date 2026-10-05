---
name: init-wiki
description: One-off build of this repo's LLM wiki in .wiki/ from the code, comments, merged PRs and commit history. Use when asked to "init the wiki", "build the wiki" or "create the llm-wiki". For later updates use sync-wiki.
---

# Init Wiki

Builds `.wiki/` once so agents can read `.wiki/index.md` instead of re-deriving the architecture. It edits the working tree and proposes a commit. It never commits, pushes or opens a PR (that is `/create-pr`), and it writes only inside `.wiki/` plus one pointer line in `CLAUDE.md`.

## Prerequisites

- `git status --short` prints nothing. If not, stop and ask the user to commit or stash.
- `git merge-base --is-ancestor main HEAD` succeeds (the branch contains the tip of `main`). If not, stop.
- `.wiki/` does not exist or holds no pages. If it has pages, stop and tell the user to run `/sync-wiki`, unless they passed `--rebuild`. With `--rebuild`, regenerate pages in place, keep `log.md` and append a new `init` entry, and list any existing page you did not regenerate for the user to decide on. Never delete files.
- Use tokensave first for code exploration (`tokensave_context`, `tokensave_search`, `tokensave_files`); check `tokensave_status` for freshness and say if the index is stale. Fall back to `Read`/`Grep` only if tokensave is unavailable.

## Instructions

1. **Install tooling.** Create `.wiki/`, copy `SCHEMA.template.md` to `.wiki/SCHEMA.md` and `check-wiki.sh` to `.wiki/check.sh` (`chmod +x`), both from this skill's directory. Create `index.md` and `log.md` (`# Log`) so the checker has them. Read `.wiki/SCHEMA.md` and follow it for everything below.

2. **Survey.** List packages, files and entry points: `CLAUDE.md`, then tokensave. Read the code and its comments. For rationale, run `git log --first-parent --format='%h %s' main` and, for each `(#N)` in a subject, `gh pr view N --json title,body,comments,reviews` (bound network calls; if `gh` is unavailable or unauthenticated, continue with git only and say rationale will be thin). Read `plan/` if it exists, as a hint to confirm against code or a PR. Never cite it.

3. **Record the marker SHA now.** `sha="$(git rev-parse --short=7 main)"`. This is the commit the wiki is read from.

4. **Write pages in dependency order**, each with `sources:` frontmatter and `updated: <sha>`:
   1. `code/`: one page per package under `internal/`, `cmd/` and `proto/` (a file of about 500 lines or more may get its own page).
   2. `concepts/`: cross-cutting rules that span packages (for example lock order, validate-then-write mutations, coercion only in the engine).
   3. `decisions/`: one page per significant "why" found in PR bodies, commit messages or comments. Say "rationale not recorded" when the source is silent.
   4. `subsystems/`: a narrative per layer (parser, engine, storage, catalog persistence, grpcserver, wiring) linking to the pages above.
   5. `index.md`: every page with a one-line summary, grouped by kind.

5. **Verify.** Run `.wiki/check.sh`. Fix every reported problem and rerun until it prints `ok`. Then spot-check at least five claims against the code (`tokensave_context` or `Read`) and fix any that are wrong. Report any you could not fix.

6. **Log and hand back.** Append to `.wiki/log.md`: `## [YYYY-MM-DD] init | through <sha>` (today's date, the SHA from step 3; with `--rebuild` this is a new entry, never an edit). Add one line to `CLAUDE.md` pointing to `.wiki/index.md` (for example: `Deeper architecture notes, decisions and a code map live in .wiki/index.md; read it first.`), only if it is not already there. Run `.wiki/check.sh` once more. Print a summary (pages written per folder, marker SHA, anything unverified), propose the commit message `docs(wiki): initialise the wiki`, and **ask before committing**. End the commit message with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.

## Never

- Commit, push or open a PR.
- Write outside `.wiki/` apart from the single `CLAUDE.md` pointer line.
- Cite `plan/`, invent rationale, or write a claim you could not trace to code, a comment, a commit or a PR.
- Delete files under `.wiki/`.
