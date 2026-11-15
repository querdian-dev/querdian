#!/usr/bin/env bash
# Classifies the files that a push or a pull request changes, so that CI can skip the jobs that the
# change cannot affect. It writes two lines for $GITHUB_OUTPUT:
#   go=true    a file outside the documentation changed: run every job
#   site=true  a file under site/ or docs/img/ changed: run the jobs that cover the Go code of site/ (lint,
#              test-race; site/svg_test.go reads docs/img/)
# Documentation alone (docs/ without docs/img/, .impeccable/, and Markdown files outside charts/, site/ and
# testdata/)
# gives go=false and site=false. CHANGELOG.md is not documentation here: a test reads it.
#
# When the script cannot decide, it gives go=true: an unknown event (schedule, workflow_dispatch), a
# missing base commit (a new branch), a diff that fails (for example after a force push), or a diff
# with no files. Running a job too often is safe; skipping a needed job is not.
#
#   hack/ci/changes.sh <event> <base> <head>   compare base...head (pull_request and push)
#   hack/ci/changes.sh classify                read the changed paths from stdin
set -uo pipefail

all() { printf 'go=true\nsite=true\n'; }

classify() {
  local go=false site=false any=false f
  while IFS= read -r f; do
    [ -n "$f" ] || continue
    any=true
    case "$f" in
      CHANGELOG.md | */testdata/* | charts/*) go=true ;;
      site/* | docs/img/*) site=true ;;
      docs/* | .impeccable/* | *.md) ;;
      *) go=true ;;
    esac
  done
  if [ "$any" = false ]; then
    all
    return
  fi
  printf 'go=%s\nsite=%s\n' "$go" "$site"
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
if ! files="$(git diff --name-only "$base...$head" 2>/dev/null)"; then
  all
  exit 0
fi
printf '%s\n' "$files" | classify
