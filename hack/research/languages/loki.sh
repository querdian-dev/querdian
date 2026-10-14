#!/usr/bin/env bash
# Black-box LogQL experiments against a Loki container (T0.5).
# Usage: loki.sh push            # ingest fixture lines
#        loki.sh q '<logql>'     # instant query, prints status + first 300 bytes
#        loki.sh r '<logql>'     # range query (last 1h, log or metric)
set -u
URL=${LOKI_URL:-http://localhost:13100}
case "$1" in
push)
  now=$(date +%s)000000000
  curl -s -XPOST "$URL/loki/api/v1/push" -H 'Content-Type: application/json' -d "{\"streams\":[
   {\"stream\":{\"app\":\"foo\",\"env\":\"a\"},\"values\":[[\"$now\",\"level=info msg=\\\"hello\\\" duration=250ms size=2KB\"],[\"$now\",\"level=error msg=\\\"bad\\\" duration=2s size=30KB\"]]},
   {\"stream\":{\"app\":\"bar\",\"env\":\"a\",\"service\":\"a:c\"},\"values\":[[\"$now\",\"level=info msg=\\\"x#y\\\" duration=1m size=1MiB\"]]}]}" -w '%{http_code}\n'
  ;;
q)
  curl -s -G "$URL/loki/api/v1/query" --data-urlencode "query=$2" -w '\nHTTP %{http_code}\n' | cut -c1-300
  ;;
r)
  end=$(date +%s); start=$((end-3600))
  curl -s -G "$URL/loki/api/v1/query_range" --data-urlencode "query=$2" --data-urlencode "start=$start" --data-urlencode "end=$end" --data-urlencode step=60 -w '\nHTTP %{http_code}\n' | cut -c1-300
  ;;
esac
