#!/usr/bin/env bash
# Like loki_run.sh but prints a compact summary: distinct msg labels (log queries),
# or distinct sample values + label keys of "foo" (metric queries), or the error text.
set -u
URL=${LOKI_URL:-http://localhost:13100}
end=$(date +%s); start=$((end-3600))
while IFS= read -r q; do
  [ -z "$q" ] && continue
  body=$(curl -s -G "$URL/loki/api/v1/query_range" --data-urlencode "query=$q" \
    --data-urlencode "start=$start" --data-urlencode "end=$end" --data-urlencode step=60 -w ' HTTP%{http_code}')
  case "$body" in
  *HTTP200)
    msgs=$(printf '%s' "$body" | grep -o '"msg":"[^"]*"' | sort -u | tr '\n' ' ')
    vals=$(printf '%s' "$body" | grep -o ',"[0-9.e+-]*"\]' | sort -u | tr '\n' ' ')
    foo=$(printf '%s' "$body" | grep -o '"foo":"[^"]*"' | sort -u | tr '\n' ' ')
    nlines=$(printf '%s' "$body" | grep -o '\["1[0-9]\{18\}"' | wc -l | tr -d ' ')
    apps=$(printf '%s' "$body" | grep -o '"app":"[^"]*"' | sort -u | tr '\n' ' ')
    printf '%s\n   => OK lines[%s] %s msgs[%s] vals[%s] %s\n' "$q" "$nlines" "$apps" "$msgs" "$vals" "$foo" ;;
  *) printf '%s\n   => %s\n' "$q" "$(printf '%s' "$body" | cut -c1-150)" ;;
  esac
done <"$1"
