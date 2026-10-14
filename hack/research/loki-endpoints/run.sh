#!/usr/bin/env bash
# Black-box test of Loki 3.5.0: detected_labels (docs/endpoints.md Q#1) and POST form on
# query / query_range (Q#8). Observes HTTP behaviour only. Requires docker (podman shim) and curl.
# Usage: ./run.sh > results.txt
set -euo pipefail
LOKI_IMAGE=${LOKI_IMAGE:-docker.io/grafana/loki:3.5.0}
HERE=$(cd "$(dirname "$0")" && pwd)
U=localhost:13101
H='X-Scope-OrgID: t1'
docker rm -f le-loki >/dev/null 2>&1 || true
docker run -d --name le-loki -p 13101:3100 -v "$HERE/loki.yaml:/etc/loki/c.yaml:ro" "$LOKI_IMAGE" -config.file=/etc/loki/c.yaml >/dev/null
trap 'docker rm -f le-loki >/dev/null' EXIT
for _ in $(seq 1 90); do curl -fs $U/ready >/dev/null 2>&1 && break; sleep 2; done
sleep 20
NOW=$(date +%s)000000000
curl -s -XPOST $U/loki/api/v1/push -H "$H" -H 'Content-Type: application/json' -d \
  "{\"streams\":[{\"stream\":{\"app\":\"a\",\"env\":\"prod\"},\"values\":[[\"$NOW\",\"line from a\"]]},{\"stream\":{\"app\":\"b\",\"team\":\"x\"},\"values\":[[\"$NOW\",\"line from b\"]]}]}"
sleep 3
S=$(( $(date +%s) - 3600 ))000000000
E=$(( $(date +%s) + 60 ))000000000
QA='%7Bapp%3D%22a%22%7D'
QB='%7Bapp%3D%22b%22%7D'
# r <title> <curl args>: prints the command, the body cut before the verbose "stats", and the status.
r() { echo "--- $1"; shift; echo "\$ curl $*"; curl -s -w '\n[HTTP %{http_code}]' "$@" | tr -d '\n' | sed -e 's/,"stats":.*\[HTTP/ ...[stats cut] [HTTP/'; echo; }

echo "===== version"
r "buildinfo" $U/loki/api/v1/status/buildinfo -H "$H"

echo "===== Q1 detected_labels"
D=$U/loki/api/v1/detected_labels
r "GET no query"            -G $D -H "$H" --data-urlencode "start=$S" --data-urlencode "end=$E"
r "GET query app=a"         -G $D -H "$H" --data-urlencode "start=$S" --data-urlencode "end=$E" --data-urlencode 'query={app="a"}'
r "GET query app=b"         -G $D -H "$H" --data-urlencode "start=$S" --data-urlencode "end=$E" --data-urlencode 'query={app="b"}'
r "GET query no match"      -G $D -H "$H" --data-urlencode "start=$S" --data-urlencode "end=$E" --data-urlencode 'query={app="zzz"}'
r "GET invalid query"       -G $D -H "$H" --data-urlencode "start=$S" --data-urlencode "end=$E" --data-urlencode 'query={app='
r "GET since, no start/end" -G $D -H "$H" --data-urlencode "since=1h" --data-urlencode 'query={app="a"}'
r "POST form query app=a"   -X POST $D -H "$H" --data-urlencode "start=$S" --data-urlencode "end=$E" --data-urlencode 'query={app="a"}'
r "POST URL query app=a, empty body" -X POST "$D?start=$S&end=$E&query=$QA" -H "$H"
r "POST URL app=a + form app=b" -X POST "$D?start=$S&end=$E&query=$QA" -H "$H" --data-urlencode 'query={app="b"}'
r "GET unknown param foo"   -G $D -H "$H" --data-urlencode "start=$S" --data-urlencode "end=$E" --data-urlencode 'query={app="a"}' --data-urlencode foo=bar
r "GET duplicate query (a,b)" "$D?start=$S&end=$E&query=$QA&query=$QB" -H "$H"

echo "===== Q8 POST on query_range / query"
for ep in query_range query; do
  X=$U/loki/api/v1/$ep
  if [ $ep = query_range ]; then
    QAL='{app="a"}'; QBL='{app="b"}'; QA_=$QA; QB_=$QB
    PU="start=$S&end=$E&limit=10"
    PF=(--data-urlencode "start=$S" --data-urlencode "end=$E" --data-urlencode limit=10)
  else
    # instant queries must be metric queries (log queries are rejected on /query)
    QAL='count_over_time({app="a"}[1h])'; QBL='count_over_time({app="b"}[1h])'
    QA_='count_over_time%28%7Bapp%3D%22a%22%7D%5B1h%5D%29'; QB_='count_over_time%28%7Bapp%3D%22b%22%7D%5B1h%5D%29'
    PU="time=$NOW&limit=10"
    PF=(--data-urlencode "time=$NOW" --data-urlencode limit=10)
  fi
  r "$ep GET app=a"            -G $X -H "$H" "${PF[@]}" --data-urlencode "query=$QAL"
  r "$ep POST form app=a"      -X POST $X -H "$H" "${PF[@]}" --data-urlencode "query=$QAL"
  r "$ep POST form app=b"      -X POST $X -H "$H" "${PF[@]}" --data-urlencode "query=$QBL"
  r "$ep POST URL only, empty body" -X POST "$X?$PU&query=$QA_" -H "$H"
  r "$ep POST URL app=a + form app=b" -X POST "$X?$PU&query=$QA_" -H "$H" --data-urlencode "query=$QBL"
  r "$ep POST URL app=b + form app=a" -X POST "$X?$PU&query=$QB_" -H "$H" --data-urlencode "query=$QAL"
  r "$ep POST form query twice (a,b)" -X POST $X -H "$H" "${PF[@]}" --data-urlencode "query=$QAL" --data-urlencode "query=$QBL"
  r "$ep GET query twice (a,b)" "$X?$PU&query=$QA_&query=$QB_" -H "$H"
  r "$ep POST JSON body"       -X POST "$X?$PU" -H "$H" -H 'Content-Type: application/json' -d '{"query":"{app=\"a\"}"}'
done
