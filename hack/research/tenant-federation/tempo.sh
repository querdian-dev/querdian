#!/usr/bin/env bash
# Tempo: push one trace per tenant via OTLP/HTTP JSON, then search with federated header.
# Requires multitenancy_enabled + query_frontend.multi_tenant_queries_enabled (see tempo.yaml).
source "$(dirname "$0")/common.sh"
docker rm -f tf-tempo >/dev/null 2>&1 || true
docker run -d --name tf-tempo -p 13200:3200 -p 14318:4318 -v "$HERE/tempo.yaml:/etc/tempo.yaml:ro" "$TEMPO_IMAGE" -config.file=/etc/tempo.yaml >/dev/null
trap 'docker rm -f tf-tempo >/dev/null' EXIT
wait_ready http://localhost:13200/ready
sleep 20
NOW=$(date +%s)
START=${NOW}000000000
END=$((NOW + 1))000000000
tid() { [ "$1" = t1 ] && echo 0000000000000000000000000000a001 || echo 0000000000000000000000000000a002; }
for t in t1 t2; do
  curl -s -XPOST localhost:14318/v1/traces -H "X-Scope-OrgID: $t" -H 'Content-Type: application/json' -d "{
   \"resourceSpans\":[{\"resource\":{\"attributes\":[{\"key\":\"service.name\",\"value\":{\"stringValue\":\"svc-$t\"}}]},
    \"scopeSpans\":[{\"spans\":[{\"traceId\":\"$(tid $t)\",\"spanId\":\"00000000000000$( [ $t = t1 ] && echo 01 || echo 02)\",
     \"name\":\"op-$t\",\"kind\":1,\"startTimeUnixNano\":\"$START\",\"endTimeUnixNano\":\"$END\"}]}]}]}"
done
sleep 10

# s <org-id header> <traceql>: prints trace IDs found, or the error text.
s() {
  out=$(curl -s -G localhost:13200/api/search -H "X-Scope-OrgID: $1" --data-urlencode "q=$2" --data-urlencode "start=$((NOW-600))" --data-urlencode "end=$((NOW+600))")
  case "$out" in
    '{"traces"'*) echo "OK traces: $(echo "$out" | grep -o '"traceID":"[0-9a-f]*"' | sort -u | tr '\n' ' ')" ;;
    '{"metrics"'*) echo "OK traces: (none)" ;;
    *) echo "ERR/RAW: $out" ;;
  esac
}
show "single tenant t1 { }";              s t1 '{ }'
show "federated t1|t2 { }";               s 't1|t2' '{ }'
show "federated + resource.__tenant_id__"; s 't1|t2' '{ resource.__tenant_id__ = "t1" }'
show "federated + .__tenant_id__";         s 't1|t2' '{ .__tenant_id__ = "t1" }'
show "federated + span.__tenant_id__";     s 't1|t2' '{ span.__tenant_id__ = "t1" }'
show "federated + resource.__tenant_id__ regex"; s 't1|t2' '{ resource.__tenant_id__ =~ "t1|t2" }'
show "federated + resource.__tenant_id__ !="; s 't1|t2' '{ resource.__tenant_id__ != "t1" }'
show "federated + span.service.name (control)"; s 't1|t2' '{ resource.service.name = "svc-t1" }'
show "trace by id (federated, tenant t2 trace)"
curl -s -o /dev/null -w "HTTP %{http_code}\n" localhost:13200/api/traces/$(tid t2) -H 'X-Scope-OrgID: t1|t2'
show "trace by id v2 (federated)"
curl -s -w "\nHTTP %{http_code}\n" localhost:13200/api/v2/traces/$(tid t2) -H 'X-Scope-OrgID: t1|t2' | head -c 600; echo
show "tags federated"; curl -s localhost:13200/api/search/tags -H 'X-Scope-OrgID: t1|t2'; echo
