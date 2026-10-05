#!/usr/bin/env bash
# check-wiki.sh: validate a .wiki/ directory, or print its last-ingested marker.
# usage: check-wiki.sh [check|marker|pages] [WIKI_DIR]
# pages reads changed repo paths on stdin and prints the pages whose sources cite them.
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

# sources_of FILE: print the entries of a page's block-list sources, one per line.
sources_of() {
  frontmatter "$1" | awk '/^sources:/ { s = 1; next } /^[^ ]/ { s = 0 } s && /^  - / { sub(/^  - /, ""); print }'
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
  done < <(sources_of "$file")
}

# link_targets FILE: print the target of every markdown link outside code
# (fenced blocks and inline code spans are skipped), without any link title.
link_targets() {
  awk '/^```/ { f = !f; next } !f' "$1" | sed 's/`[^`]*`//g' | grep -oE '\]\([^)]+\)' | sed -e 's/^](//' -e 's/)$//' -e 's/ .*//'
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
  done < <(link_targets "$file")
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
      link_targets "$wiki/index.md" | sed -e 's/#.*//' -e 's#^\./##' | grep -qxF "$rel" || err "$rel" "not listed in index.md"
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

# pages: read changed repo paths on stdin; print "<page><TAB><path>" for every
# page whose sources cite the path (exactly, or as a directory ending in /),
# and "uncited<TAB><path>" for a path no page cites.
pages() {
  local path page src hit
  while IFS= read -r path; do
    [ -n "$path" ] || continue
    hit=0
    for page in "$wiki"/subsystems/*.md "$wiki"/code/*.md "$wiki"/decisions/*.md "$wiki"/concepts/*.md; do
      [ -e "$page" ] || continue
      while IFS= read -r src; do
        case "$src" in pr:*) continue ;; esac
        if [ -d "$repo/$src" ]; then src="${src%/}/"; fi
        case "$src" in
          */) case "$path" in "$src"*) hit=1; printf '%s\t%s\n' "${page#$wiki/}" "$path"; break ;; esac ;;
          *) if [ "$path" = "$src" ]; then hit=1; printf '%s\t%s\n' "${page#$wiki/}" "$path"; break; fi ;;
        esac
      done < <(sources_of "$page")
    done
    [ "$hit" -eq 1 ] || printf 'uncited\t%s\n' "$path"
  done
}

case "$cmd" in
  check) run_check ;;
  marker) marker ;;
  pages) pages ;;
  *) echo "usage: check-wiki.sh [check|marker|pages] [WIKI_DIR]" >&2; exit 2 ;;
esac
