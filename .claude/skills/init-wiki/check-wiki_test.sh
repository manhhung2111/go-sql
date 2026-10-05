#!/usr/bin/env bash
# Tests for check-wiki.sh. Run: bash .claude/skills/init-wiki/check-wiki_test.sh
set -u
here="$(cd "$(dirname "$0")" && pwd)"
script="$here/check-wiki.sh"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT
pass=0
fail=0
out=""
status=0

# new_wiki NAME: build a valid repo under $tmp/NAME and print its wiki dir.
new_wiki() {
  local root="$tmp/$1"
  mkdir -p "$root/internal/engine" "$root/.wiki/code" "$root/.wiki/subsystems" "$root/.wiki/decisions" "$root/.wiki/concepts"
  touch "$root/internal/engine/catalogstore.go"
  cat > "$root/.wiki/code/engine.md" <<'EOF'
---
title: internal/engine
kind: code
sources:
  - internal/engine/
  - pr: 14
updated: 117c2f0
---
# internal/engine

See [catalog persistence](../subsystems/catalog.md) and [Go](https://go.dev).
EOF
  cat > "$root/.wiki/subsystems/catalog.md" <<'EOF'
---
title: Catalog persistence
kind: subsystem
sources:
  - internal/engine/catalogstore.go
updated: 117c2f0
---
# Catalog persistence

Code map: [engine](../code/engine.md#key-types). Decision: [commit point](../decisions/commit-point.md).
EOF
  cat > "$root/.wiki/decisions/commit-point.md" <<'EOF'
---
title: Catalog row is the commit point
kind: decision
status: accepted
sources:
  - internal/engine/catalogstore.go
  - pr: 14
updated: 117c2f0
---
# Catalog row is the commit point

Rationale not recorded.
EOF
  cat > "$root/.wiki/index.md" <<'EOF'
# Index

- [internal/engine](code/engine.md): engine package map
- [Catalog persistence](subsystems/catalog.md): how the catalog is stored
- [Catalog row is the commit point](decisions/commit-point.md): DDL commit rule
EOF
  cat > "$root/.wiki/log.md" <<'EOF'
# Log

## [2026-10-05] init | through 117c2f0
EOF
  echo "$root/.wiki"
}

run() {
  out="$(bash "$script" "$@" 2>&1)"
  status=$?
}

expect_ok() { # NAME ARGS...
  local name="$1"; shift
  run "$@"
  if [ "$status" -eq 0 ]; then pass=$((pass + 1)); else fail=$((fail + 1)); printf 'FAIL %s: want exit 0, got %d\n%s\n' "$name" "$status" "$out"; fi
}

expect_fail() { # NAME SUBSTRING ARGS...
  local name="$1" want="$2"; shift 2
  run "$@"
  if [ "$status" -ne 0 ] && printf '%s' "$out" | grep -qF -- "$want"; then
    pass=$((pass + 1))
  else
    fail=$((fail + 1)); printf 'FAIL %s: want nonzero exit containing %q, got %d\n%s\n' "$name" "$want" "$status" "$out"
  fi
}

# --- frontmatter and sources ---
w="$(new_wiki valid)"
expect_ok "valid wiki passes (directory source, pr entry, anchor, https link)" check "$w"

w="$(new_wiki no-updated)"
sed -i.bak '/^updated:/d' "$w/code/engine.md"
expect_fail "missing updated key" "frontmatter missing updated" check "$w"

w="$(new_wiki no-frontmatter)"
printf '# just a heading\n' > "$w/code/engine.md"
expect_fail "page without frontmatter" "missing frontmatter" check "$w"

w="$(new_wiki bad-kind)"
sed -i.bak 's/^kind: code/kind: blob/' "$w/code/engine.md"
expect_fail "unknown kind" "kind must be" check "$w"

w="$(new_wiki no-status)"
sed -i.bak '/^status:/d' "$w/decisions/commit-point.md"
expect_fail "decision without status" "needs status" check "$w"

w="$(new_wiki bad-source)"
sed -i.bak 's#internal/engine/catalogstore.go#internal/engine/gone.go#' "$w/subsystems/catalog.md"
expect_fail "source path missing" 'source "internal/engine/gone.go" does not exist' check "$w"

w="$(new_wiki inline-sources)"
sed -i.bak 's#^sources:#sources: [internal/engine/]#; /^  - internal\/engine\/$/d' "$w/code/engine.md"
expect_fail "inline sources list" "block list" check "$w"

# --- links and index ---
w="$(new_wiki dead-link)"
sed -i.bak 's#(\.\./subsystems/catalog\.md)#(../subsystems/nope.md)#' "$w/code/engine.md"
expect_fail "dead relative link" 'dead link "../subsystems/nope.md"' check "$w"

w="$(new_wiki dead-anchor-target)"
rm "$w/code/engine.md"
sed -i.bak '/engine\.md/d' "$w/index.md"
expect_fail "anchor link to a missing file" 'dead link "../code/engine.md#key-types"' check "$w"

w="$(new_wiki unlisted)"
cp "$w/code/engine.md" "$w/code/extra.md"
expect_fail "page missing from index" "not listed in index.md" check "$w"

w="$(new_wiki index-dead)"
printf -- '- [Ghost](code/ghost.md): not there\n' >> "$w/index.md"
expect_fail "index links to a missing page" 'dead link "code/ghost.md"' check "$w"

w="$(new_wiki no-index)"
rm "$w/index.md"
expect_fail "missing index.md" "index.md: missing" check "$w"

w="$(new_wiki no-log)"
rm "$w/log.md"
expect_fail "missing log.md" "log.md: missing" check "$w"

# --- summary ---
printf '%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
