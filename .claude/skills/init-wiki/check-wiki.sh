#!/usr/bin/env bash
# check-wiki.sh: validate a .wiki/ directory, or print its last-ingested marker.
# usage: check-wiki.sh [check|marker] [WIKI_DIR]
# WIKI_DIR defaults to the directory containing this script (.wiki/check.sh).
set -u

cmd="${1:-check}"
wiki="${2:-$(cd "$(dirname "$0")" && pwd)}"
repo="$(cd "$wiki/.." 2>/dev/null && pwd)"
errors=0

err() { printf 'ERROR: %s: %s\n' "$1" "$2"; errors=$((errors + 1)); }

# frontmatter FILE: print the lines between the opening and closing ---.
frontmatter() {
  awk 'NR == 1 { if ($0 != "---") exit; next } $0 == "---" { exit } { print }' "$1"
}

check_page() {
  local file="$1" rel="${1#$wiki/}" fm key kind src
  fm="$(frontmatter "$file")"
  if [ -z "$fm" ]; then err "$rel" "missing frontmatter"; return; fi
  for key in title kind sources updated; do
    printf '%s\n' "$fm" | grep -q "^$key:" || err "$rel" "frontmatter missing $key"
  done
  kind="$(printf '%s\n' "$fm" | sed -n 's/^kind: *\([a-z]*\).*/\1/p')"
  case "$kind" in
    subsystem|code|decision|concept) ;;
    *) err "$rel" "kind must be subsystem, code, decision or concept, got \"$kind\"" ;;
  esac
  if [ "$kind" = decision ]; then
    printf '%s\n' "$fm" | grep -qE '^status: *(accepted|superseded)' || err "$rel" "decision needs status accepted or superseded"
  fi
  if printf '%s\n' "$fm" | grep -qE '^sources: *\['; then
    err "$rel" "sources must be a block list"
  fi
  while IFS= read -r src; do
    case "$src" in pr:*) continue ;; esac
    [ -e "$repo/$src" ] || err "$rel" "source \"$src\" does not exist"
  done < <(printf '%s\n' "$fm" | awk '/^sources:/ { s = 1; next } /^[^ ]/ { s = 0 } s && /^  - / { sub(/^  - /, ""); print }')
}

# check_links FILE: every relative markdown link must resolve; anchors are
# stripped, external and in-page links are ignored.
check_links() {
  local file="$1" rel="${1#$wiki/}" dir target path
  dir="$(dirname "$file")"
  while IFS= read -r target; do
    case "$target" in http://*|https://*|mailto:*|\#*) continue ;; esac
    path="${target%%#*}"
    [ -e "$dir/$path" ] || err "$rel" "dead link \"$target\""
  done < <(grep -oE '\]\([^)]+\)' "$file" | sed -e 's/^](//' -e 's/)$//')
}

# check_index: every page in a kind folder must be linked from index.md.
check_index() {
  local dir file rel
  [ -f "$wiki/index.md" ] || { err index.md "missing"; return; }
  for dir in subsystems code decisions concepts; do
    [ -d "$wiki/$dir" ] || continue
    for file in "$wiki/$dir"/*.md; do
      [ -e "$file" ] || continue
      rel="${file#$wiki/}"
      grep -qF "]($rel)" "$wiki/index.md" || err "$rel" "not listed in index.md"
    done
  done
}

run_check() {
  local dir file
  for dir in subsystems code decisions concepts; do
    [ -d "$wiki/$dir" ] || continue
    for file in "$wiki/$dir"/*.md; do
      [ -e "$file" ] || continue
      check_page "$file"
      check_links "$file"
    done
  done
  [ -f "$wiki/index.md" ] && check_links "$wiki/index.md"
  check_index
  [ -f "$wiki/log.md" ] || err log.md "missing"
  if [ "$errors" -gt 0 ]; then printf '%d problem(s)\n' "$errors"; exit 1; fi
  echo ok
}

# marker: print the SHA the wiki last ingested through (newest init/sync entry).
marker() {
  local last sha
  [ -f "$wiki/log.md" ] || { echo "no log.md in $wiki" >&2; exit 2; }
  last="$(grep -E '^## \[[0-9]{4}-[0-9]{2}-[0-9]{2}\] (init|sync) \|' "$wiki/log.md" | tail -n 1)"
  [ -n "$last" ] || { echo "no init or sync entry in $wiki/log.md" >&2; exit 2; }
  sha="$(printf '%s\n' "$last" | sed -nE \
    -e 's/.*through ([0-9a-f]{7,40}).*/\1/p' \
    -e 's/.*[0-9a-f]{7,40}\.\.([0-9a-f]{7,40}).*/\1/p')"
  [ -n "$sha" ] || { echo "no SHA in log entry: $last" >&2; exit 2; }
  printf '%s\n' "$sha"
}

case "$cmd" in
  check) run_check ;;
  marker) marker ;;
  *) echo "usage: check-wiki.sh [check|marker] [WIKI_DIR]" >&2; exit 2 ;;
esac
