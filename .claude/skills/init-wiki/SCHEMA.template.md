# Wiki schema

This wiki is persistent knowledge about go-sql for AI agents. Read `index.md` first, then only the pages you need. Facts live in exactly one page; other pages link to it. `CLAUDE.md` holds the short authoritative rules; this wiki holds detail and rationale and does not restate those rules.

Maintained by two skills: `/init-wiki` (one-off build) and `/sync-wiki` (ingest merged PRs since the marker in `log.md`). Validate with `.wiki/check.sh`.

## Layout

```
.wiki/
  index.md        every page, one line each, grouped by kind
  log.md          append-only record of init/sync runs; holds the marker
  SCHEMA.md       this file
  check.sh        validator (`check`), marker reader (`marker`), changed-path mapper (`pages`)
  subsystems/     one narrative page per layer
  code/           one map page per package
  decisions/      one page per significant "why"
  concepts/       one page per cross-cutting invariant or pattern
```

Kind folders are flat: no nested folders.

## Sources of truth

1. The code and its comments (primary).
2. Merged PRs and commit history (rationale): `git log`, `gh pr view`.
3. `plan/` is gitignored scratch and exists only on one machine. It may point you at something, but a claim must be confirmed against code, a comment, a commit or a PR. Never cite `plan/`.

Where the sources are silent, write "rationale not recorded". Never invent rationale.

## Page format

Every page in a kind folder starts with frontmatter:

```yaml
---
title: Catalog persistence
kind: subsystem          # subsystem | code | decision | concept
sources:                 # block list only, never inline
  - internal/engine/catalogstore.go
  - pr: 14
updated: <sha>           # the marker commit when the page was last touched
---
```

`decision` pages also carry `status: accepted` or `status: superseded`.

- `sources:` entries are repo paths (files or directories) or `pr: N`. Every path must exist.
- Cite code as file path plus symbol name. Never line numbers.
- Cite PRs as `#N`.
- Link other pages with relative markdown links, e.g. `[engine](../code/engine.md)`. Anchors are allowed.
- Every page must be linked from `index.md` as `- [Title](folder/page.md): one-line summary`.

### code/ (one per package; a very large file, about 500 lines or more, may get its own page)

Sections: Purpose; Depends on / Must not import; Files (table: file, responsibility); Key types and entry points (entry points only, not a copy of godoc); Called by / calls; Gotchas (each with a source); Related.

### decisions/ (short ADR)

Sections: Context; Decision; Consequences; Alternatives (only if a source states them); Superseded by. Never delete a decision. When a later change reverses it, set `status: superseded` and link the new page.

### concepts/

Sections: Rule; Why it matters; Where it applies; How to follow it when adding code; Verified by (the test or tool that enforces it, if any).

### subsystems/

A narrative that ties the code, concept and decision pages of one layer together. Link to them; do not restate their facts.

## log.md and the marker

`log.md` is append-only. One header line per run:

```
## [YYYY-MM-DD] init | through <sha>
## [YYYY-MM-DD] sync | <old-sha>..<new-sha> | PRs #N #N | pages: N
```

The marker is the SHA after `through`, or the right-hand SHA of `..`, in the newest entry. `.wiki/check.sh marker` prints it. Optional notes may follow a header line.

## Ingest rules (used by /sync-wiki)

- Range: `git log --first-parent <marker>..<main>`, one squash commit per PR. Skip commits that touch only `.wiki/`.
- Map each PR's changed paths to pages by their `sources:`: pipe the paths into `.wiki/check.sh pages`, which prints `<page><TAB><path>` for every page citing a path (exactly, or as a directory ending in `/`) and `uncited<TAB><path>` for a path no page cites. A changed file no page cites means a new or extended page. A removed file means its pages are updated or marked stale.
- Edit only affected pages and fix cross-links. PRs that only touch tests or formatting are logged and touch no pages.
- Update `updated:` on every touched page, update `index.md`, append the `sync` entry.
- Process more than about 15 PRs in chunks, advancing the marker per chunk.

## Lint (run `.wiki/check.sh` after every ingest)

Dead links, missing or malformed frontmatter, nonexistent `sources:` paths, pages missing from `index.md`. Fix every reported problem before proposing a commit.
