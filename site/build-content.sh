#!/usr/bin/env bash
# Generates site/content/docs from the repository's docs/*.md: the markdown stays the single source.
# Adds front matter (title from the first H1, weight from data/nav.yaml order) and drops the H1,
# which the layout renders. Run before `hugo`; see .github/workflows/pages.yml.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
src="$here/../docs"
dst="$here/content/docs"

# Remove previously generated pages, keep the hand-written _index.md.
find "$dst" -type f ! -name '_index.md' -delete 2>/dev/null || true
find "$dst" -mindepth 1 -type d -empty -delete 2>/dev/null || true

# Pages published on the site, in navigation order (must match data/nav.yaml).
pages=$(sed -n 's/^    - //p' "$here/data/nav.yaml")  # lines: "path" or "path | Title override"
weight=10
while IFS= read -r line; do
  p="${line%% | *}"
  override=""
  [ "$p" != "$line" ] && override="${line#* | }"
  in="$src/$p.md"
  [ -f "$in" ] || { echo "missing docs/$p.md" >&2; exit 1; }
  out="$dst/$p.md"
  mkdir -p "$(dirname "$out")"
  title=$(awk '/^# /{sub(/^# /,""); print; exit}' "$in")
  [ -z "$override" ] || title="$override"
  [ -n "$title" ] || { echo "docs/$p.md has no H1" >&2; exit 1; }
  {
    printf -- '---\ntitle: %s\nweight: %d\nsource: docs/%s.md\n---\n' "$(printf '%s' "$title" | sed 's/"/\\"/g; s/^/"/; s/$/"/')" "$weight" "$p"
    # body without the first H1
    awk 'BEGIN{done=0} !done && /^# /{done=1; next} {print}' "$in"
  } >"$out"
  weight=$((weight + 10))
done <<<"$pages"
echo "generated $(echo "$pages" | wc -l | tr -d ' ') docs pages"
