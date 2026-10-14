#!/bin/sh
# Re-records the Tempo 2.9.0 response fixtures used by tempo_search_test.go (black box, no Tempo source).
# Start Tempo with hack/research/languages/tempo.yaml (ports 13200 http / 14318 OTLP), push the fixture with
# hack/research/languages/tempo_push.py and tempo_push.py --extra, wait ~15 s, then run this script from this dir.
set -eu
base=${TEMPO_URL:-http://localhost:13200}
search() { curl -s -G "$base/api/search" --data-urlencode "q=$1" --data-urlencode limit=20 | python3 -m json.tool >"$2"; }
search '{ span.a = "y" }' search_child_match.json
search '{ } | by(resource.service.name)' search_grouped.json
search '{ resource.service.name = "backend" } | select(span.a, span.http.status_code)' search_select.json
search '{ span.a = "r3" }' search_root_match.json
search '{ span.a = "o4" }' search_rootless.json
curl -s "$base/api/v2/search/tags" | python3 -m json.tool >tags_v2.json
curl -s "$base/api/v2/search/tag/span.a/values" | python3 -m json.tool >tag_values_v2.json
curl -s "$base/api/v2/search/tag/resource.service.name/values" | python3 -m json.tool >tag_values_service.json
