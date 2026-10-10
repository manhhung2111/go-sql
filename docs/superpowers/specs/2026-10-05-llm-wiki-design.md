# LLM wiki for go-sql: design

Date: 2026-10-05
Status: draft, awaiting review

## Goal

Give AI agents persistent, repo-local knowledge of go-sql so they stop re-deriving the same facts each session. The pattern follows Karpathy's LLM Wiki: raw sources, an LLM-maintained markdown wiki, and a schema that says how to maintain it.

- Raw sources: the code and its comments (primary), merged PRs and commit history (rationale), and `plan/` (supplementary, optional).
- Wiki: `.wiki/` at the repo root, committed to git.
- Schema: `.wiki/SCHEMA.md`, holding page formats and the ingest rules.

Two skills maintain it: `/init-wiki` (one-off build) and `/sync-wiki` (incremental ingest of merged PRs).

## Decisions made during brainstorming

| Question | Decision |
| --- | --- |
| What should the wiki capture? | Both a code map and rationale: `code/`, `subsystems/`, `decisions/`, `concepts/`. |
| Primary source | Code and comments. `plan/` is gitignored and only a hint. No claim may depend on it alone. |
| Location | `.wiki/` at the repo root, committed. |
| How does sync find changes? | `git log --first-parent <marker>..main` (squash merges give one commit per PR), plus `gh pr view` for title, body and review comments. Falls back to git only if `gh` is unavailable. |
| Output | Skills edit `.wiki/` in the working tree, print a summary and propose a Conventional Commit. They ask before committing and never push or open a PR. |
| Relationship to `CLAUDE.md` | `CLAUDE.md` stays the short authoritative rules file and gains one pointer line to `.wiki/index.md`. The wiki holds detail and does not duplicate the rules. |

## Layout

```
.wiki/
  index.md        catalog of every page, one line each, grouped by kind
  log.md          append-only record of init/sync runs; holds the marker
  SCHEMA.md       page formats, ingest and lint rules
  subsystems/     one narrative page per layer
  code/           one map page per package
  decisions/      one page per significant "why"
  concepts/       one page per cross-cutting invariant or pattern
```

`index.md` is the only page agents are told to read unprompted.

### Page frontmatter

```yaml
---
title: Catalog persistence
kind: subsystem          # subsystem | code | decision | concept
sources:                 # what the page was derived from
  - internal/engine/catalogstore.go
  - pr: 14
updated: <sha>           # marker commit when the page was last touched
---
```

`decision` pages also carry `status: accepted | superseded`.

Rules for content:

- Claims cite file paths plus symbol names, never line numbers.
- PRs are cited as `#N`.
- Cross-links are relative markdown links.
- A fact lives in exactly one page and the others link to it.
- Where the sources are silent, the page says "rationale not recorded". Rationale is never invented.

### `code/` page (per package; a very large file may get its own page)

Sections: Purpose, Depends on / Must not import, Files (table of file and responsibility), Key types and entry points, Called by / calls, Gotchas, Related. Only entry points are listed, so the page does not duplicate godoc.

### `decisions/` page (short ADR)

Sections: Context, Decision, Consequences, Alternatives (only if a source states them), Superseded by. Decisions are never deleted. A later PR that reverses one sets `status: superseded` and links the new page.

### `concepts/` page

Sections: Rule, Why it matters, Where it applies, How to follow it when adding code, Verified by (the test or tool that enforces it, if any).

### `subsystems/` page

A narrative that ties the code, concept and decision pages for one layer together. It links to them and does not restate their facts.

## The marker

`log.md` is append-only. Each entry is one greppable header line:

```
## [2026-10-05] init | through 117c2f0
## [2026-10-12] sync | 117c2f0..ab12cd3 | PRs #13 #14 | pages: 6
```

The marker is the SHA after `through` or after `..` in the newest entry. There is no separate marker file, so the log and the marker cannot disagree. Sync ignores commits that touch only `.wiki/`, so the commit recording a marker never becomes input to the next sync.

## `/init-wiki`

Location: `.claude/skills/init-wiki/SKILL.md`, in the same format as `create-pr`.

1. **Preconditions.** Clean working tree, branch is `main` or cut from it. If `.wiki/` already has pages, stop and point to `/sync-wiki`, unless the user passes an explicit rebuild flag.
2. **Survey.** Use tokensave and `CLAUDE.md` to list packages, files and entry points. Read code and comments. Read `git log` and `gh pr view` for the rationale. Read `plan/` if present, as a hint to be confirmed against code or a PR.
3. **Write `SCHEMA.md` first.** Page formats and ingest rules.
4. **Write pages in dependency order:** `code/`, then `concepts/`, then `decisions/`, then `subsystems/`, then `index.md`. Every claim gets a `sources:` entry.
5. **Verify.** Link check, every `sources:` path exists, every page is in `index.md`. Fix problems or list them in the report.
6. **Log and hand back.** Write the `init` entry with `HEAD` as the marker and add the pointer line to `CLAUDE.md`. Print a summary, propose `docs(wiki): initialise the wiki`, and ask before committing.

## `/sync-wiki`

Location: `.claude/skills/sync-wiki/SKILL.md`.

1. **Find the marker** in the newest `log.md` entry. If the SHA is not an ancestor of `main` (rebase or force-push), stop and ask. Never guess a range.
2. **List PRs** with `git log --first-parent <marker>..main`, skipping commits that touch only `.wiki/`. If none remain, report that the wiki is current and write nothing.
3. **Read each PR:** diff and commit message, plus title, body and review comments via `gh pr view`. Without `gh` or authentication, continue with git only and report that rationale may be thin.
4. **Map changes to pages** by matching changed paths against `sources:`. A changed file with no citing page means a new or extended page. A removed file means its pages are updated or marked stale.
5. **Update pages.** Edit only the affected pages and fix cross-links. Reversed decisions are marked `superseded`. PRs that only touch tests or formatting are logged and touch no pages.
6. **Light lint on touched pages:** dead links, missing `sources:`, pages absent from `index.md`.
7. **Log and hand back.** Append the `sync` entry with the new marker, update `index.md`, print a summary, propose `docs(wiki): sync through <sha>`, and ask before committing.

A range of more than about 15 PRs is processed in chunks, with the marker advanced per chunk so an interrupted run can resume.

## Rules shared by both skills

- Never commit, push or open a PR. Opening the PR is `/create-pr` (or, for automated syncs, the CI workflow described under "CI automation").
- Write only inside `.wiki/` and the single `CLAUDE.md` pointer line.
- Idempotent: a second `/sync-wiki` with no new merges changes nothing.
- Every factual claim traces to code, a comment, a commit or a PR.

## CI automation (added after the first release)

`.github/workflows/sync-wiki.yml` runs the sync on every push to `main` (and from the "Run workflow" button on `main`) and opens a PR with the result. The skills themselves still never commit, push or open a PR; the workflow hands those steps to `peter-evans/create-pull-request`, and the PR is reviewed by a human like any other.

- The workflow does the deterministic skill steps in shell: the prerequisites and marker (step 1), listing and reading the PRs (steps 2-3), the log line (step 7) and the `check.sh` lint loop. Gemini (`gemini-3.8-flash`) does only the page edits (steps 4-5) and the `index.md` update (step 6), with file tools only: shell and web tools are excluded and the checkout holds no write credentials.
- A Gemini that does not run must fail the job, not log an empty ingest. Headless Gemini refuses to start in an untrusted folder (exit 55), which a fresh CI checkout is, so the workflow sets `GEMINI_CLI_TRUST_WORKSPACE=true` (the checkout is the repo's own `main`; tool access is still restricted by the user-level settings). The Gemini step runs with `pipefail` and also fails on empty output. It tries the models in `GEMINI_MODELS` in order until one produces a result, resetting `.wiki/` between attempts; a failure is a 503 after the CLI's own retries, a quota or not-found error, a refusal, a timeout, or silence, while a `BLOCKED:` reply is a normal answer and stops the run without trying another model. Free-tier quotas are per model, so the list must not contain a name together with the one the CLI rewrites it to (`gemini-3.5-flash` becomes `gemini-3.8-flash`, `gemini-3.1-flash-lite` becomes `gemini-3.5-flash-lite`). A model the API key cannot use answers 404 in about two seconds (the key's model list still shows `gemini-2.5-flash`, which the API refuses with "no longer available to new users"), so a retired model in the list costs nothing and is reported by name with the API's suggested replacement. The CLI's own retries are capped with `general.maxAttempts: 3`, because every retry counts against the daily quota and a 503 storm once used all 20 requests without producing anything. A daily-quota error is recognised from stderr and reported by name, with the reset time, in the annotations and the job summary. Each attempt is capped by `GEMINI_TIMEOUT_SECONDS` and the job by `timeout-minutes: 45`. The model that ran is shown in the PR body. A final `if: always()` step writes a job summary (outcome, range, PRs, model and whether a later model than the first ran, pages changed, backlog left, PR link), so a run that opens no PR, such as one with nothing to ingest, still says what it did.
- Commits whose subject starts with `docs(wiki):` are never ingested, and neither are commits that touch only `.wiki/`.
- At most 15 commits are ingested per run. A backlog is continued by the next push to `main` or a manual run; merging the wiki PR does not retrigger the workflow.
- PR titles, bodies, comments and diffs are untrusted input to the model. The prompt says so, the scope check rejects any change outside `.wiki/` and any change to `.wiki/check.sh`, and the PR body tells the reviewer to check new rationale against the linked PRs.
- The PR is opened with `GITHUB_TOKEN`, so CI workflows do not run on it; `check.sh` has already passed in the job.

## Out of scope (YAGNI)

- A standalone `/lint-wiki` or query skill. Lint runs inside sync, and agents query by reading `index.md`.
- Embedding or vector search. `index.md` is enough at this size.
- CI that blocks merges when the wiki is stale. (A workflow that opens a sync PR after merges is in scope: see "CI automation".)

## Testing and verification

The `CLAUDE.md` TDD exceptions cover documentation and skill files, so these are verified by running them, not by Go unit tests:

- Run `/init-wiki` on the current repo. Check that every `sources:` path exists, every link resolves, every page is in `index.md`, and spot-check several claims against the code.
- Run `/sync-wiki` immediately afterwards and confirm it reports the wiki is current and changes nothing.
- Dry-run `/sync-wiki` against a prepared range: reset the marker to an older SHA and confirm the PRs in that range are ingested and the touched pages change.
- Run `/sync-wiki` with `gh` unavailable and confirm it falls back to git only and says so.
- Run `/sync-wiki` with a marker that is not an ancestor of `main` and confirm it stops and asks.

## Open questions

None blocking. The 500-line threshold for splitting a package page and the 15-PR chunk size are starting values to tune after the first real runs.
