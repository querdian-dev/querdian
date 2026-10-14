#!/usr/bin/env bash
# Mimir: a Prometheus container remote-writes to tenants t1 and t2, then query with federated header.
# Requires tenant_federation.enabled: true (see mimir.yaml).
source "$(dirname "$0")/common.sh"
PROM_IMAGE=${PROM_IMAGE:-docker.io/prom/prometheus:v3.5.0}
docker rm -f tf-mimir tf-prom >/dev/null 2>&1 || true
docker run -d --name tf-mimir -p 19009:9009 -v "$HERE/mimir.yaml:/etc/mimir.yaml:ro" "$MIMIR_IMAGE" -config.file=/etc/mimir.yaml >/dev/null
trap 'docker rm -f tf-mimir tf-prom >/dev/null' EXIT
wait_ready http://localhost:19009/ready
sleep 15
docker run -d --name tf-prom -v "$HERE/prometheus.yaml:/etc/prometheus/prometheus.yml:ro" "$PROM_IMAGE" >/dev/null
sleep 30

# q <org-id header> <promql>: prints the "origin" label values of the result, or the error text.
q() {
  out=$(curl -s -G localhost:19009/prometheus/api/v1/query -H "X-Scope-OrgID: $1" --data-urlencode "query=$2")
  case "$out" in
    '{"status":"success"'*) echo "OK origins: $(echo "$out" | grep -o '"origin":"[^"]*"' | sort | uniq -c | tr '\n' ' ') tenant_id: $(echo "$out" | grep -o '"__tenant_id__":"[^"]*"' | sort -u | tr '\n' ' ')" ;;
    *) echo "ERR: $out" ;;
  esac
}
show "single tenant t1";                          q t1 'up'
show "federated t1|t2";                           q 't1|t2' 'up'
show "federated + __tenant_id__=t1";              q 't1|t2' 'up{__tenant_id__="t1"}'
show "federated + __tenant_id__=~t1|t2";          q 't1|t2' 'up{__tenant_id__=~"t1|t2"}'
show "federated + __tenant_id__!=t1";             q 't1|t2' 'up{__tenant_id__!="t1"}'
show "single tenant + __tenant_id__=t1";          q t1 'up{__tenant_id__="t1"}'
show "labels federated"; curl -s localhost:19009/prometheus/api/v1/labels -H 'X-Scope-OrgID: t1|t2' | head -c 400; echo
