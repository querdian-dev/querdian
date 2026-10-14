#!/bin/sh
# Re-records the Tempo 2.9.0 trace-by-ID fixtures (black box). Start Tempo with hack/research/languages/tempo.yaml
# (http 13200, OTLP 14318), push the fixture with hack/research/languages/tempo_push.py, wait ~15 s, run here.
set -eu
base=${TEMPO_URL:-http://localhost:13200}
id=00000000000000000000000000000001
curl -s "$base/api/traces/$id" | python3 -m json.tool >trace1_v1.json
curl -s "$base/api/v2/traces/$id" | python3 -m json.tool >trace1_v2.json
curl -s -H 'Accept: application/protobuf' "$base/api/traces/$id" -o trace1_v1.pb
curl -s -H 'Accept: application/protobuf' "$base/api/v2/traces/$id" -o trace1_v2.pb
