#!/usr/bin/env bash
# Runs each line of a cases file as a range query; prints "HTTP code | query => body-prefix".
set -u
URL=${LOKI_URL:-http://localhost:13100}
end=$(date +%s); start=$((end-3600))
while IFS= read -r q; do
  [ -z "$q" ] && continue
  out=$(curl -s -G "$URL/loki/api/v1/query_range" --data-urlencode "query=$q" \
    --data-urlencode "start=$start" --data-urlencode "end=$end" --data-urlencode step=60 \
    -w ' HTTP%{http_code}' | tr -d '\n' | cut -c1-${WIDTH:-170})
  printf '%s\n   => %s\n' "$q" "$out"
done <"$1"
