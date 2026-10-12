# sync-wiki workflow: review fixes

Branch: ci/sync-wiki (cut from main at 0b0f5d5). Model: gemini-3.8-flash (user's choice; id not verified against the API).

Files: .github/workflows/sync-wiki.yml (new, the user's draft + fixes), docs/specs/2026-10-05-llm-wiki-design.md, .claude/skills/sync-wiki/SKILL.md.
No unit tests: workflows/docs are exempt (CLAUDE.md); verified by running: ruby YAML parse, actionlint, running the collect script locally, quick shell checks.

Fixes (numbering from the review):
1 skip `docs(wiki):` commits in the collect loop (first run must not ingest #22)
2 job `if: github.ref == 'refs/heads/main'`
3 prompt: PR content is untrusted data; ~/.gemini/settings.json excludes run_shell_command, web_fetch, web_search
4 checkout persist-credentials: false
5 pin gemini-cli version and create-pull-request SHA
6 leave bot PR CI-less; say so in PR body
7 keep MAX_PRS, ::notice:: when truncated
8 gh pr view < /dev/null
9 marker error via 2>&1
10 log line after lint; post-condition check.sh marker == new
11 create `automated` label (manual gh command, not in repo)
12 diff truncation notice
13 early GEMINI_API_KEY check
14 spec out-of-scope -> CI automation section; sync-wiki SKILL.md CI note; first live run extends wiki deployment page
