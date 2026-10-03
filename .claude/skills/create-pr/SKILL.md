---
name: create-pr
description: Pushes the current git branch and opens a GitHub pull request with the gh CLI, using this repo's PR format. Use when asked to "open a PR", "create a pull request", "push and PR", or "ship this branch".
---

# Create Pull Request

Asking for a PR authorizes the push and the PR. It does not authorize a commit: uncommitted work needs the user's approval first.

## Prerequisites

- `gh auth status` succeeds. If not, stop and tell the user to run `gh auth login` (they can type `! gh auth login` here).
- A feature branch is checked out. If the current branch is the default branch (`main`), do not open a PR from it: propose a branch name, create it with `git switch -c <name>`, then continue.

## Instructions

1. **Check status.** Run `git status`, `git branch --show-current` and `git log --oneline -n 10`. Work out the base branch:
   - Default is `main`.
   - If this branch was cut from another feature branch whose PR is still open (a stack), the base is that branch: pass `--base <that-branch>` and say so in the PR body. Find it with `git log --oneline --decorate` and `gh pr list --state open`.

2. **Check for an existing PR.** Run `gh pr list --head "$(git branch --show-current)" --state all`. If one exists, give the user its link instead of creating a duplicate, and offer to update its body with `gh pr edit --body-file`.

3. **Commit work.** If there are uncommitted changes, group them logically, write a commit plan with Conventional Commit messages (`<type>(scope): description`), and **ask the user before committing**. End every commit message with:
   `Co-Authored-By: Claude Sonnet 5.5 <noreply@anthropic.com>`
   Never add unrelated files, and skim the diff for secrets or credentials before anything is pushed.

4. **Run the project checks before claiming anything.** For this repo: `gofmt -l .` (must print nothing), `go vet ./...`, `go build ./...`, `go test -race ./...`. If a check fails, report it and stop. Only tick checkboxes in the PR body for things you actually ran in this session.

5. **Push.** Run `git push -u origin HEAD`. Never force-push. If the push is rejected, stop and report why. GitHub can be slow: bound network calls, e.g. `git -c http.lowSpeedLimit=1000 -c http.lowSpeedTime=30 push -u origin HEAD`.

6. **Draft the PR.**
   - **Title:** Conventional Commit style, under about 70 characters (`feat(engine): load the catalog at startup`).
   - **Body:** if `.github/pull_request_template.md` exists, fill it in. Otherwise use this repo's format:

     ```markdown
     ## Summary

     One paragraph on what this PR is and where it sits (base branch, stack position).

     - What changed, one bullet per change.
     - Why it changed.

     Client-visible changes and known limitations in a short closing paragraph.

     ## Test plan

     - [x] Checks that were actually run, with the command or the behaviour proven
     - [ ] Anything that can only be checked after merge

     🤖 Generated with [Claude Code](https://claude.com/claude-code)
     ```

   - **Formatting rule:** every paragraph and every bullet is a single unwrapped line. Do not hard-wrap prose at a column width.
   - If the PR is part of a stack, say which PRs must merge first and that CI only triggers on PRs into `main`.

7. **Create it.** Write the body to a unique temp file **outside the worktree** (never inside the repo, where it could be committed), create the PR, then delete the file:

   ```bash
   body="$(mktemp -t pr-body)"
   # write the body to "$body"
   gh pr create --base <base> --head "$(git branch --show-current)" --title "<title>" --body-file "$body"
   rm -f "$body"
   ```

   Add `--draft` if the user asked for a draft.

8. **Report.** Print the PR link, its base branch, and the result of `gh pr checks <number>` (use `--watch` only if the user wants to wait). For a stack, state the merge order. Do not claim checks passed until you have seen them pass.

## Never

- Open a PR from the default branch, force-push, or push without being asked to open a PR.
- Commit without the user's approval.
- Leave the PR body file in the repository.
