#!/usr/bin/env bash
# Fails if the govulncheck JSON report (stdin) has a called-code finding that is not in
# hack/govulncheck-accepted.txt. Each accepted ID needs a written reason in that file.
set -euo pipefail
accepted="$(dirname "$0")/govulncheck-accepted.txt"
ids=$(grep -v '^#' "$accepted" | awk 'NF{print $1}' | jq -R . | jq -s .)
report=$(cat)
bad=$(printf '%s' "$report" | jq -r --argjson ok "$ids" '
  select(.finding != null) | .finding
  | select(.trace[0].function != null)
  | select(.osv as $o | ($ok | index($o)) | not) | .osv' | sort -u)
if [ -n "$bad" ]; then
  echo "govulncheck: reachable vulnerabilities not in $accepted:" >&2
  echo "$bad" >&2
  exit 1
fi
echo "govulncheck: no unaccepted reachable vulnerabilities"
