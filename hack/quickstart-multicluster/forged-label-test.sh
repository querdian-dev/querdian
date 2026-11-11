#!/usr/bin/env bash
# The forged-label test of the ingest gateway (docs/multicluster.md, step 5). Run it while the spoke is
# still suspended. up.sh runs it for each spoke before it unsuspends the spoke; you can run it again at
# any time:
#
#   hack/quickstart-multicluster/forged-label-test.sh spoke-a
#
# 1. From the node container of the spoke, with the ingest cert of that spoke, it sends one sample, one
#    log line and one span to the gateway port of that spoke. Each signal claims the cluster label of
#    each other cluster: the other spokes and the hub. The claims are in the resource attributes and in
#    the attributes of the data point, the log record and the span. Prometheus also copies the resource
#    attributes to the series target_info, so the script checks it too.
# 2. It queries Prometheus, Loki and Tempo of the hub DIRECTLY (not through Querdian). The stored cluster
#    label of each item must be the spoke of the cert, and no item may carry a claimed value.
# 3. It checks that the gateway and the receiver refuse the wrong certs: the ingest cert on the port of
#    a different spoke, the agent cert on the gateway, the ingest cert on the receiver, and no cert.
#
# Requires: curl, jq, kubectl, docker. Exit status 1 if a check fails.
set -uo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

[ $# -eq 1 ] || die "usage: $0 SPOKE (one of: ${SPOKES[*]})"
S="$1"
printf ' %s ' "${SPOKES[@]}" | grep -q " $S " || die "unknown spoke '$S' (one of: ${SPOKES[*]})"
need curl jq kubectl docker
NODE="$(spoke_cluster "$S")-control-plane"
FAILN=0
pass() { printf 'PASS: %s\n' "$*"; }
failed() { FAILN=$((FAILN + 1)); printf 'FAIL: %s\n' "$*" >&2; }
accepted() { if [ "$2" = 200 ]; then pass "$1: accepted by the gateway"; else failed "$1: HTTP '$2'"; fi; }
cleanup() {
  pf_stop
  docker exec "$NODE" rm -rf /tmp/querdian-forged-label-test >/dev/null 2>&1
}
trap cleanup EXIT

for f in "ingest-$S.crt" "ingest-$S.key" "agent-$S.crt" "agent-$S.key" hub-server-ca.crt; do
  [ -f "$CERT_DIR/$f" ] || die "missing $CERT_DIR/$f (run up.sh first)"
done
docker exec "$NODE" curl --version >/dev/null 2>&1 || die "curl is not available in the node container $NODE"
HUB_IP="$(hub_ip)"
[ -n "$HUB_IP" ] || die "cannot find the address of the hub node"
port_of() { k -n "$1" get svc "$2" -o jsonpath="{.spec.ports[?(@.name==\"$3\")].nodePort}"; }
GW_PORT="$(port_of observability ingest-gateway "$S")"
RECEIVER_PORT="$(port_of querdian querdian-receiver receiver)"
[ -n "$GW_PORT" ] && [ -n "$RECEIVER_PORT" ] || die "cannot find the NodePorts of the gateway and the receiver"
OTHER_PORT=""
OTHER=""
for o in "${SPOKES[@]}"; do [ "$o" = "$S" ] || { OTHER_PORT="$(port_of observability ingest-gateway "$o")"; OTHER="$o"; break; }; done

# The certs go to a temporary directory in the node container of the spoke. The spoke already holds
# both certs (Secrets ingest-tls and querdian-agent-tls).
D=/tmp/querdian-forged-label-test
docker exec "$NODE" sh -c "umask 077 && rm -rf $D && mkdir -p $D" || die "cannot write to $NODE"
for f in "ingest-$S.crt" "ingest-$S.key" "agent-$S.crt" "agent-$S.key" hub-server-ca.crt; do
  docker exec -i "$NODE" sh -c "cat > $D/$f" <"$CERT_DIR/$f" || die "cannot copy $f to $NODE"
done

# node_post CERT-NAME PORT PATH: POST stdin from the node container. Prints the HTTP status. curl prints
# 000 if it gets no HTTP response, for example after a refused TLS handshake.
node_post() {
  local auth=()
  [ "$1" = none ] || auth=(--cert "$D/$1.crt" --key "$D/$1.key")
  docker exec -i "$NODE" curl -sS -o /dev/null -w '%{http_code}' --max-time 10 --cacert "$D/hub-server-ca.crt" \
    ${auth[@]+"${auth[@]}"} -H 'Content-Type: application/json' --data-binary @- "https://$HUB_IP:$2$3" 2>/dev/null || true
}

# --- 1. send the forged data -------------------------------------------------------------------------------
ID="$S-$(date +%s)-$RANDOM"
CLAIMS=()
for o in "${SPOKES[@]}"; do [ "$o" = "$S" ] || CLAIMS+=("$o"); done
CLAIMS+=("$HUB_NAME")
NOW_NS="$(date +%s)000000000"
attrs() { # claim [extra key=value...] -> OTLP attribute list
  local c="$1" kv out
  shift
  out="$(jq -cn --arg c "$c" '[{key:"cluster",value:{stringValue:$c}},{key:"k8s.cluster.name",value:{stringValue:$c}}]')"
  for kv in "$@"; do out="$(jq -c --arg k "${kv%%=*}" --arg v "${kv#*=}" '. + [{key:$k,value:{stringValue:$v}}]' <<<"$out")"; done
  printf '%s' "$out"
}
hex() { od -An -N"$1" -tx1 /dev/urandom | tr -d ' \n'; }

printf '\n--- %s: forged data through the gateway (port %s), test id %s\n' "$S" "$GW_PORT" "$ID"
for c in "${CLAIMS[@]}"; do
  res="$(attrs "$c" service.name=forged-label-test test_id="$ID")"
  pt="$(attrs "$c" test_id="$ID" claimed="$c")"
  code=$(jq -cn --argjson r "$res" --argjson a "$pt" --arg t "$NOW_NS" \
    '{resourceMetrics:[{resource:{attributes:$r},scopeMetrics:[{scope:{name:"forged-label-test"},metrics:[{name:"querdian_forged_label_test",gauge:{dataPoints:[{asDouble:1,timeUnixNano:$t,attributes:$a}]}}]}]}]}' |
    node_post "ingest-$S" "$GW_PORT" /v1/metrics)
  accepted "metric claiming cluster=$c" "$code"
  code=$(jq -cn --argjson r "$res" --argjson a "$pt" --arg t "$NOW_NS" --arg b "forged-label-test $ID claimed=$c" \
    '{resourceLogs:[{resource:{attributes:$r},scopeLogs:[{scope:{name:"forged-label-test"},logRecords:[{timeUnixNano:$t,severityText:"INFO",body:{stringValue:$b},attributes:$a}]}]}]}' |
    node_post "ingest-$S" "$GW_PORT" /v1/logs)
  accepted "log line claiming cluster=$c" "$code"
  code=$(jq -cn --argjson r "$res" --argjson a "$pt" --arg t "$NOW_NS" --arg tid "$(hex 16)" --arg sid "$(hex 8)" \
    '{resourceSpans:[{resource:{attributes:$r},scopeSpans:[{scope:{name:"forged-label-test"},spans:[{traceId:$tid,spanId:$sid,name:"forged-label-test",kind:1,startTimeUnixNano:$t,endTimeUnixNano:$t,attributes:$a}]}]}]}' |
    node_post "ingest-$S" "$GW_PORT" /v1/traces)
  accepted "span claiming cluster=$c" "$code"
done

# --- 2. read the stored labels directly from the backends ------------------------------------------------
N=${#CLAIMS[@]}
backends_pf
printf -- '--- %s: stored cluster labels (direct backend queries, expected: %s)\n' "$S" "$S"
stored() {
  M=$(prom_direct "querdian_forged_label_test{test_id=\"$ID\"}") || M='{}'
  L=$(loki_direct_range "{service_name=\"forged-label-test\"} |= \"$ID\"" 600) || L='{}'
  T=$(tempo_direct "{ resource.service.name = \"forged-label-test\" && span.test_id = \"$ID\" }" 600) || T='{}'
  [ "$(jq -r '.data.result | length' <<<"$M")" = "$N" ] &&
    [ "$(jq -r '[.data.result[].values[]] | length' <<<"$L")" = "$N" ] &&
    [ "$(jq -r '.traces | length' <<<"$T")" = "$N" ]
}
for _ in $(seq 1 30); do stored && break; sleep 2; done
expect() { if [ "$2" = "$3" ]; then pass "$1 ($3)"; else failed "$1: expected '$2', got '$3'"; fi; }

expect "metrics: forged samples stored" "$N" "$(jq -r '.data.result | length' <<<"$M")"
expect "metrics: label cluster of each sample" "$S" "$(jq -r '[.data.result[].metric.cluster] | unique | join(",")' <<<"$M")"
expect "metrics: no label k8s_cluster_name (the gateway deletes it)" "" "$(jq -r '[.data.result[].metric.k8s_cluster_name // empty] | unique | join(",")' <<<"$M")"
expect "metrics: the claims are kept only in the label claimed" "$(printf '%s\n' "${CLAIMS[@]}" | sort | paste -sd, -)" \
  "$(jq -r '[.data.result[].metric.claimed] | sort | join(",")' <<<"$M")"
lines() { jq -r '[.data.result[].values[]] | length' <<<"$(loki_direct_range "$1" 600 || echo '{}')"; }
TI=$(prom_direct "target_info{test_id=\"$ID\"}") || TI='{}'
expect "metrics: label cluster of target_info (a copy of the resource attributes)" "$S" \
  "$(jq -r '[.data.result[].metric.cluster] | unique | join(",")' <<<"$TI")"
expect "metrics: no label k8s_cluster_name on target_info" "" \
  "$(jq -r '[.data.result[].metric.k8s_cluster_name // empty] | unique | join(",")' <<<"$TI")"
expect "logs: forged lines stored" "$N" "$(jq -r '[.data.result[].values[]] | length' <<<"$L")"
expect "logs: label k8s_cluster_name of each stream" "$S" "$(jq -r '[.data.result[].stream.k8s_cluster_name] | unique | join(",")' <<<"$L")"
# The queries go into variables first: bash 3.2 (macOS) can parse an escaped quote inside a quoted
# argument inside "$(...)" in a different way from later versions.
q="{service_name=\"forged-label-test\", k8s_cluster_name=\"$S\"} |= \"$ID\""
expect "logs: lines in the stream selector {k8s_cluster_name=\"$S\"}" "$N" "$(lines "$q")"
q="{service_name=\"forged-label-test\"} |= \"$ID\" | cluster=\"$S\""
expect "logs: lines with the structured metadata cluster=\"$S\"" "$N" "$(lines "$q")"
# The stream label k8s_cluster_name and a structured metadata field of the same name look the same in a
# label filter. With categorize-labels, Loki gives the structured metadata of each entry separately.
LC=$(loki_direct_categorized "{service_name=\"forged-label-test\"} |= \"$ID\"" 600) || LC='{}'
expect "logs: no structured metadata k8s_cluster_name (the gateway deletes the attribute)" "" \
  "$(jq -r '[.data.result[].values[][2].structuredMetadata.k8s_cluster_name // empty] | unique | join(",")' <<<"$LC")"
expect "traces: forged spans stored" "$N" "$(jq -r '.traces | length' <<<"$T")"
T2=$(tempo_direct "{ resource.service.name = \"forged-label-test\" && span.test_id = \"$ID\" && resource.k8s.cluster.name = \"$S\" && resource.cluster = \"$S\" }" 600) || T2='{}'
expect "traces: spans with resource.k8s.cluster.name and resource.cluster = $S" "$N" "$(jq -r '.traces | length' <<<"$T2")"
T3=$(tempo_direct "{ span.test_id = \"$ID\" && span.k8s.cluster.name != \"\" }" 600) || T3='{}'
expect "traces: no span attribute k8s.cluster.name (the gateway deletes it)" 0 "$(jq -r '.traces | length' <<<"$T3")"

# --- 3. the wrong certs are refused ----------------------------------------------------------------------
printf -- '--- %s: wrong certs\n' "$S"
refused() { # description cert port path: the TLS handshake must fail (curl status 000)
  local code
  code=$(printf '{}' | node_post "$2" "$3" "$4")
  if [ "$code" = 000 ]; then pass "$1: TLS handshake refused"; else failed "$1: expected a refused handshake, got HTTP '$code'"; fi
}
[ -z "$OTHER_PORT" ] || refused "ingest cert of $S on the gateway port of $OTHER" "ingest-$S" "$OTHER_PORT" /v1/metrics
refused "agent cert of $S on the gateway" "agent-$S" "$GW_PORT" /v1/metrics
refused "no client cert on the gateway" none "$GW_PORT" /v1/metrics
refused "ingest cert of $S on the receiver" "ingest-$S" "$RECEIVER_PORT" /v1alpha1/sync

echo
if [ "$FAILN" = 0 ]; then echo "forged-label test of $S: passed"; else echo "forged-label test of $S: $FAILN checks failed" >&2; fi
[ "$FAILN" = 0 ]
