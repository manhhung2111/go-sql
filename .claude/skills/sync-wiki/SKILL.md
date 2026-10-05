---
name: sync-wiki
description: Ingests the PRs merged into main since the wiki's last-ingested commit and updates the affected pages in .wiki/. Use when asked to "sync the wiki", "update the wiki" or after PRs have merged. Requires an existing wiki (see init-wiki).
---

# Sync Wiki

Brings `.wiki/` up to date with `main`. It edits the working tree and proposes a commit. It never commits, pushes or opens a PR, and it writes only inside `.wiki/`.

## Prerequisites

- `.wiki/check.sh` exists. If not, stop and tell the user to run `/init-wiki`.
- `git status --short` prints nothing, or only paths under `.wiki/` from an interrupted earlier sync. Otherwise stop and ask.
- Read `.wiki/SCHEMA.md` and follow its ingest rules.
- Use tokensave first for code exploration; say so if its index is stale.

## Instructions

1. **Find the marker.** Run `git -c http.lowSpeedLimit=1000 -c http.lowSpeedTime=30 fetch origin main`. Let `base` be `origin/main` if it exists and is ahead of `main`, else `main`; if you use `origin/main`, say so. Run `marker="$(.wiki/check.sh marker)"`. If it exits 2, stop and report its message. If `git merge-base --is-ancestor "$marker" "$base"` fails (the history was rebased or force-pushed), stop and ask the user which commit to start from. Never guess a range.

2. **List the PRs.** `git log --first-parent --reverse --format='%H %s' "$marker..$base"`. For each commit, list its paths with `git diff-tree --no-commit-id --name-only -r -m --first-parent <sha>` and drop it if every path is under `.wiki/`. If nothing is left, say the wiki is current and write nothing. The PR number is the `(#N)` in the subject; with none, treat it as a direct commit and read only its message and diff. Process more than 15 PRs in chunks of 15, finishing steps 3 to 7 per chunk so the marker advances per chunk and an interrupted run can resume.

3. **Read each PR.** The diff (`git show <sha>`), the commit message, and `gh pr view N --json title,body,comments,reviews`. Bound network calls. If `gh` is unavailable or unauthenticated, continue with git only and say rationale may be thin.

4. **Map changes to pages.** Pipe the changed paths into `.wiki/check.sh pages` (for example `git diff-tree --no-commit-id --name-only -r -m --first-parent <sha> | .wiki/check.sh pages`). It prints `<page><TAB><path>` for every page whose `sources:` cite the path, exactly or as a directory, and `uncited<TAB><path>` for a path no page cites. Do not hand-roll this with `grep`: a plain grep misses pages that cite a directory such as `internal/engine/`. An `uncited` path means a new page or an extended one. A removed file means its pages are updated or marked stale.

5. **Update pages.** Edit only the affected pages and fix cross-links. Set `updated:` to the new marker SHA on every page you touch. A PR that reverses a decision: set that decision's `status: superseded`, add `Superseded by`, and write the new decision page; never delete a decision. A PR that only touches tests or formatting is logged and touches no pages. Where a PR's rationale is not recorded, say so on the page and never invent it.

6. **Lint.** Update `index.md` for new or removed pages, then run `.wiki/check.sh`. Fix every reported problem and rerun until it prints `ok`.

7. **Log and hand back.** New marker: `new="$(git rev-parse --short=7 <last-commit-of-this-chunk>)"`. Append `## [YYYY-MM-DD] sync | <marker-short>..<new> | PRs #N #N | pages: <count touched>` to `.wiki/log.md` (an append, never an edit). Print a summary (PRs ingested, pages touched, anything thin), propose the commit message `docs(wiki): sync through <new>`, and **ask before committing**. End the commit message with `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`.

## Never

- Commit, push or open a PR (that is `/create-pr`).
- Write outside `.wiki/`.
- Guess a commit range, delete a decision page, cite `plan/`, or write a claim you could not trace to code, a comment, a commit or a PR.
- Write anything when there is nothing to ingest: running twice with no new merges changes nothing.
