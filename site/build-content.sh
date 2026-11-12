#!/usr/bin/env bash
# Generates site/content/docs and site/data/versions.yaml from the repository's docs/*.md: the markdown
# stays the single source. One tree is written per documentation version:
#
#   * each release line (vMAJOR.MINOR) at its latest patch tag, read from git. The newest release is
#     the default and lives at /docs/, older lines live at /docs/<tag>/;
#   * the working tree as "main" at /docs/main/.
#
# Every version brings its own docs/, navigation (site/data/nav.yaml) and overview page, so a page
# describes the release it belongs to. Without release tags (a clone without tags, a source archive)
# only the working tree is published, at /docs/. A git error in a repository stops the script.
#
# A page gets front matter (title from the first H1, weight from the navigation order, its version)
# and loses the H1, which the layout renders. Run before `hugo`; see the Makefile.
set -euo pipefail
here="$(cd "$(dirname "$0")" && pwd)"
repo="$here/.."
dst="$here/content/docs"
data="$here/data/versions.yaml"
work="$(mktemp -d)"
trap 'rm -rf "$work"' EXIT

rm -rf "$dst"
mkdir -p "$dst"
: >"$data"

# yaml_str <text>: a double-quoted YAML string.
yaml_str() { printf '%s' "$1" | sed 's/\\/\\\\/g; s/"/\\"/g; s/^/"/; s/$/"/'; }

# alias_list <page path below the version, "" for the overview> <version name>...: the front matter
# line that keeps the addresses of other names of this version alive, or nothing.
alias_list() {
  local page="$1" out="" name
  shift
  for name in "$@"; do
    out="$out${out:+, }\"/docs/$name/${page:+$page/}\""
  done
  [ -z "$out" ] || printf 'aliases: [%s]\n' "$out"
}

# no_sitemap <latest: true|false> <file of the same page in the latest release>: the front matter line
# that keeps a page out of the sitemap, or nothing. A page of another version names the page of the
# latest release as its canonical address (layouts/_default/baseof.html), thus the sitemap lists only
# that page. A page that the latest release does not have stays in the sitemap.
no_sitemap() {
  if [ "$1" = false ] && [ -f "$2" ]; then printf 'sitemap: {disable: true}\n'; fi
}

# generate <name> <source root> <url path> <git ref> <latest: true|false> <release: true|false> [alias name...]
# The source root holds docs/, site/data/nav.yaml and the overview page of that version. The latest
# release must be the first call: the other versions compare their pages with its files.
generate() {
  local name="$1" root="$2" url="$3" ref="$4" latest="$5" release="$6"
  shift 6
  local out="$here/content$url" nav="$root/site/data/nav.yaml" overview="" f
  for f in "$root/site/docs-overview.md" "$root/site/content/docs/_index.md"; do
    if [ -f "$f" ]; then overview="$f"; break; fi
  done
  [ -f "$nav" ] || { echo "$name: missing site/data/nav.yaml" >&2; exit 1; }
  [ -n "$overview" ] || { echo "$name: missing the docs overview page" >&2; exit 1; }
  [ "$(head -n 1 "$overview")" = "---" ] || { echo "$name: the docs overview page has no front matter" >&2; exit 1; }
  # no_sitemap compares with the files of the latest release.
  [ "$latest" = true ] || [ -f "$dst/_index.md" ] || { echo "$name: the latest release must be generated first" >&2; exit 1; }

  mkdir -p "$out"
  {
    printf -- '---\nversion: %s\n' "$(yaml_str "$name")"
    alias_list "" "$@"
    no_sitemap "$latest" "$dst/_index.md"
    tail -n +2 "$overview"
  } >"$out/_index.md"

  # Pages published on the site, in navigation order. Lines: "path" or "path | Title override".
  local pages weight=10 line p override in title count=0
  pages=$(sed -n 's/^    - //p' "$nav")
  while IFS= read -r line; do
    [ -n "$line" ] || continue
    p="${line%% | *}"
    override=""
    [ "$p" != "$line" ] && override="${line#* | }"
    in="$root/docs/$p.md"
    [ -f "$in" ] || { echo "$name: missing docs/$p.md" >&2; exit 1; }
    mkdir -p "$(dirname "$out/$p.md")"
    title=$(awk '/^# /{sub(/^# /,""); print; exit}' "$in")
    [ -z "$override" ] || title="$override"
    [ -n "$title" ] || { echo "$name: docs/$p.md has no H1" >&2; exit 1; }
    {
      printf -- '---\ntitle: %s\nweight: %d\nsource: docs/%s.md\nversion: %s\n' \
        "$(yaml_str "$title")" "$weight" "$p" "$(yaml_str "$name")"
      alias_list "$p" "$@"
      no_sitemap "$latest" "$dst/$p.md"
      printf -- '---\n'
      # body without the first H1
      awk 'BEGIN{done=0} !done && /^# /{done=1; next} {print}' "$in"
    } >"$out/$p.md"
    weight=$((weight + 10))
    count=$((count + 1))
  done <<<"$pages"

  {
    printf -- '- name: %s\n  path: %s\n  ref: %s\n  latest: %s\n  release: %s\n  nav:\n' \
      "$(yaml_str "$name")" "$(yaml_str "$url")" "$(yaml_str "$ref")" "$latest" "$release"
    sed 's/^/    /' "$nav"
  } >>"$data"
  echo "generated $count docs pages for $name at $url/"
}

# Release tags, newest first. Pre-releases are not published. Only a source tree without a repository
# has no tags by definition; in a repository a git error must not look like "no releases".
tags=""
if [ -e "$repo/.git" ]; then
  all_tags=$(git -C "$repo" tag --list 'v[0-9]*')
  tags=$(printf '%s\n' "$all_tags" | grep -E '^v[0-9]+\.[0-9]+\.[0-9]+$' | sort -V -r || true)
fi

if [ -z "$tags" ]; then
  echo "no release tags found: publishing the working tree only" >&2
  generate main "$repo" /docs main true false
  exit 0
fi

# The first tag of a release line (vMAJOR.MINOR) in this order is its latest patch. Earlier patches
# of the line are not published; their addresses redirect to the latest patch.
lines=""
line_of() { printf '%s' "${1%.*}"; }
for tag in $tags; do
  case " $lines " in *" $(line_of "$tag") "*) ;; *) lines="$lines $(line_of "$tag")" ;; esac
done

newest=true
for l in $lines; do
  tag="" superseded=""
  for t in $tags; do
    [ "$(line_of "$t")" = "$l" ] || continue
    if [ -z "$tag" ]; then tag="$t"; else superseded="$superseded $t"; fi
  done
  root="$work/$tag"
  mkdir -p "$root/site"
  git -C "$repo" archive "$tag" docs site/data/nav.yaml | tar -x -C "$root"
  git -C "$repo" show "$tag:site/docs-overview.md" >"$root/site/docs-overview.md" 2>/dev/null ||
    git -C "$repo" show "$tag:site/content/docs/_index.md" >"$root/site/docs-overview.md"
  if $newest; then
    # shellcheck disable=SC2086 # word splitting of the tag list is intended
    generate "$tag" "$root" /docs "$tag" true true "$tag" $superseded
    newest=false
  else
    # shellcheck disable=SC2086
    generate "$tag" "$root" "/docs/$tag" "$tag" false true $superseded
  fi
done

generate main "$repo" /docs/main main false false
