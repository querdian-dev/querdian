#!/usr/bin/env bash
# Classifies the files that a push or a pull request changes, so that CI can skip the steps that the
# change cannot affect. It writes two lines for $GITHUB_OUTPUT:
#   go=true    a file outside the documentation changed: run all steps
#   site=true  go=true, or a file under site/ or docs/img/ changed: run lint and test-race, which cover
#              the Go code of site/ (site/svg_test.go reads docs/img/)
# Documentation alone gives go=false and site=false. Documentation is docs/ without docs/img/,
# .impeccable/, the Markdown files at the root without CHANGELOG.md (a test reads it), and the issue and
# pull request templates. All other paths are code, also Markdown in other directories: a package can
# embed it, and a test can read it.
#
# When the script cannot decide, it gives go=true: an unknown event (schedule, workflow_dispatch), a
# missing base commit (a new branch), a diff that fails (for example after a force push), or a diff
# with no files. Running a step too often is safe; skipping a needed step is not.
#
#   hack/ci/changes.sh <event> <base> <head>   compare base...head (pull_request and push);
#                                              .github/actions/changes runs this form
#   hack/ci/changes.sh classify                read the changed paths from stdin
set -uo pipefail

all() { printf 'go=true\nsite=true\n'; }

classify() {
  local go=false site=false any=false f
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    any=true
    # In a case pattern, * also matches "/".
    case "$f" in
      CHANGELOG.md) go=true ;;
      site/* | docs/img/*) site=true ;;
      docs/* | .impeccable/* | .github/ISSUE_TEMPLATE/*.md | .github/pull_request_template.md) ;;
      */*) go=true ;;
      *.md) ;;
      *) go=true ;;
    esac
  done
  if [ "$any" = false ] || [ "$go" = true ]; then
    all
    return
  fi
  printf 'go=false\nsite=%s\n' "$site"
}

if [ "${1:-}" = classify ]; then
  classify
  exit 0
fi

event="${1:-}" base="${2:-}" head="${3:-}"
case "$event" in
  pull_request | push) ;;
  *) all; exit 0 ;;
esac
if [ -z "$base" ] || [ -z "$head" ] || [ -z "${base//0/}" ]; then
  all
  exit 0
fi
# Without --no-renames, a renamed file gives only its new path: a move of code into docs/ would look
# like a change of documentation only.
if ! files="$(git diff --no-renames --name-only "$base...$head" 2>/dev/null)"; then
  all
  exit 0
fi
printf '%s\n' "$files" | classify
