#!/usr/bin/env bash
# Loki: push one stream per tenant (t1,t2), then query with federated header.
# Requires querier.multi_tenant_queries_enabled: true (see loki.yaml).
source "$(dirname "$0")/common.sh"
docker rm -f tf-loki >/dev/null 2>&1 || true
docker run -d --name tf-loki -p 13100:3100 -v "$HERE/loki.yaml:/etc/loki/c.yaml:ro" "$LOKI_IMAGE" -config.file=/etc/loki/c.yaml >/dev/null
trap 'docker rm -f tf-loki >/dev/null' EXIT
wait_ready http://localhost:13100/ready
sleep 20
NOW=$(date +%s)000000000
for t in t1 t2; do
  curl -s -XPOST localhost:13100/loki/api/v1/push -H "X-Scope-OrgID: $t" -H 'Content-Type: application/json' \
    -d "{\"streams\":[{\"stream\":{\"app\":\"a-$t\"},\"values\":[[\"$NOW\",\"hello from $t\"]]}]}"
done
sleep 3

# q <org-id header> <logql>: prints a compact summary (log lines returned, or error text).
q() {
  out=$(curl -s -G localhost:13100/loki/api/v1/query_range -H "X-Scope-OrgID: $1" \
    --data-urlencode "query=$2" --data-urlencode "since=1h")
  case "$out" in
    '{"status":"success"'*) echo "OK lines: $(echo "$out" | grep -o 'hello from t[0-9]' | sort | tr '\n' ',')" ;;
    *) echo "ERR: $out" ;;
  esac
}
show "single tenant t1";                       q t1 '{app=~".+"}'
show "federated t1|t2";                        q 't1|t2' '{app=~".+"}'
show "federated + __tenant_id__=t1 only";      q 't1|t2' '{__tenant_id__="t1"}'
show "federated + __tenant_id__=t1 + app";     q 't1|t2' '{__tenant_id__="t1",app=~".+"}'
show "federated + __tenant_id__=~t1 + app";    q 't1|t2' '{__tenant_id__=~"t1",app=~".+"}'
show "federated + __tenant_id__=~t1|t2 + app"; q 't1|t2' '{__tenant_id__=~"t1|t2",app=~".+"}'
show "federated + __tenant_id__!=t1 + app";    q 't1|t2' '{__tenant_id__!="t1",app=~".+"}'
show "single tenant + __tenant_id__=t1 + app"; q t1 '{__tenant_id__="t1",app=~".+"}'
show "labels federated"; curl -s localhost:13100/loki/api/v1/labels -H 'X-Scope-OrgID: t1|t2'; echo
