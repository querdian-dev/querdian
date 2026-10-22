#!/usr/bin/env bash
# Verifies the quickstart end to end: two users see different data, admins see everything.
#
#   hack/quickstart/verify.sh            # full verification (CLUSTER=querdian-demo by default)
#   hack/quickstart/verify.sh --smoke    # quick "is data flowing" check, used by up.sh
#
# 1. Gets access tokens from Keycloak with the password grant (direct access grants are enabled
#    on the demo client `grafana` ONLY FOR THIS DEMO) for alice, bob and carol.
# 2. Queries the Querdian proxy directly (kubectl port-forward) with PromQL, LogQL, TraceQL
#    search and trace-by-ID and asserts who sees what, including the redacted-label example and the
#    spoofed-namespace attempts of the order-service sample.
# 3. Checks the backfilled history (DEMO_HISTORY_HOURS, default 24): the earliest metric sample, log
#    line and trace are about a day old, each user only sees the history of their own namespace,
#    the incidents are in the data, error logs link to traces, the denied label stays hidden.
# 4. Checks Grafana: health, provisioned data sources and dashboards, and (headless) the real
#    browser flow: Keycloak login form -> Grafana session -> data source proxy queries with OAuth
#    pass-through. Then runs every query of every provisioned dashboard as alice, bob and carol
#    through Grafana (POST /api/ds/query with the user's session, so Querdian sees their token).
#
# Requires: curl, jq, kubectl. Exit status 1 if any assertion fails.
set -uo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

SMOKE=0
[ "${1:-}" = "--smoke" ] && SMOKE=1
QPORT="${QUERDIAN_LOCAL_PORT:-18080}"
WORK="$(mktemp -d)"
PF_PID=""
PASS=0
FAILN=0

cleanup() {
  [ -z "$PF_PID" ] || kill "$PF_PID" 2>/dev/null
  rm -rf "$WORK"
}
trap cleanup EXIT

need curl jq kubectl

pass() { PASS=$((PASS + 1)); [ $SMOKE = 1 ] || printf 'PASS: %s\n' "$*"; }
failed() { FAILN=$((FAILN + 1)); printf 'FAIL: %s\n' "$*" >&2; }
expect() { # description, expected, actual
  if [ "$2" = "$3" ]; then pass "$1 ($3)"; else failed "$1: expected '$2', got '$3'"; fi
}
expect_ge() { # description, minimum, actual
  if [ "$3" -ge "$2" ] 2>/dev/null; then pass "$1 ($3)"; else failed "$1: expected >= $2, got '$3'"; fi
}
section() { [ $SMOKE = 1 ] || printf '\n--- %s\n' "$*"; }

# Keycloak is reached through the host port; --resolve makes this work even where
# *.localtest.me DNS is filtered.
CURL_KC=(--resolve "$KC_HOST:$KEYCLOAK_PORT:127.0.0.1")

token() { # user password
  curl -fsS "${CURL_KC[@]}" "$ISSUER/protocol/openid-connect/token" \
    -d grant_type=password -d client_id=grafana -d client_secret=grafana-demo-secret \
    -d scope=openid -d "username=$1" -d "password=$2" | jq -r .access_token
}
claims() { printf '%s' "$1" | cut -d. -f2 | jq -R 'gsub("-";"+") | gsub("_";"/") | @base64d | fromjson'; }

# call TOKEN PATH [curl args]: GET against the proxy. Sets STATUS and BODY.
call() {
  local tok="$1" path="$2"
  shift 2
  STATUS=$(curl -sS -o "$WORK/body" -w '%{http_code}' ${tok:+-H "Authorization: Bearer $tok"} \
    -G "http://127.0.0.1:$QPORT$path" "$@" 2>/dev/null) || STATUS=000
  BODY="$(cat "$WORK/body" 2>/dev/null)"
}

# Namespaces seen in a PromQL result / a Loki streams result, comma separated.
prom_ns() { jq -r '[.data.result[].metric.namespace] | unique | join(",")' <<<"$BODY"; }
prom_n() { jq -r '.data.result | length' <<<"$BODY"; }
loki_ns() { jq -r '[.data.result[].stream.k8s_namespace_name] | unique | join(",")' <<<"$BODY"; }
loki_n() { jq -r '.data.result | length' <<<"$BODY"; }
tempo_n() { jq -r '.traces | length' <<<"$BODY"; }
# Namespaces of the matched spans (the policy-relevant attribute is returned with each span).
tempo_ns() { jq -r '[.traces[].spanSet.spans[].attributes[]? | select(.key=="k8s.namespace.name") | .value.stringValue] | unique | join(",")' <<<"$BODY"; }
tempo_roots() { jq -r '[.traces[].rootServiceName] | unique | join(",")' <<<"$BODY"; }

# --- connectivity --------------------------------------------------------------------------------
k -n querdian get svc querdian >/dev/null 2>&1 || die "cluster $CLUSTER is not up (run hack/quickstart/up.sh)"
kubectl --context "$KCTX" -n querdian port-forward svc/querdian "$QPORT:8080" >/dev/null 2>&1 &
PF_PID=$!
disown "$PF_PID" 2>/dev/null
for _ in $(seq 1 30); do
  curl -s -o /dev/null "http://127.0.0.1:$QPORT/" && break
  sleep 0.5
done

ALICE=$(token alice alice-demo) || ALICE=""
BOB=$(token bob bob-demo) || BOB=""
CAROL=$(token carol carol-demo) || CAROL=""
if [ -z "$ALICE" ] || [ -z "$BOB" ] || [ -z "$CAROL" ] || [ "$ALICE" = null ]; then
  failed "could not get tokens from $ISSUER"
  [ $SMOKE = 1 ] || echo "Summary: $PASS passed, $FAILN failed"
  exit 1
fi

LOGS_Q='{k8s_namespace_name=~".+"}'
HOURS="${DEMO_HISTORY_HOURS:-24}"
NOW=$(date +%s)
MIN_AGE=$((HOURS * 3600 * 23 / 24)) # the history must reach back at least this far (23 h of 24)

if [ $SMOKE = 1 ]; then
  # Data must have arrived everywhere (carol sees all namespaces).
  call "$CAROL" /prom/api/v1/query --data-urlencode 'query=demo_requests_total'
  [[ "$(prom_ns)" == *order-service* && "$(prom_ns)" == *payment-service* ]] || exit 1
  call "$CAROL" /loki/loki/api/v1/query_range --data-urlencode "query=$LOGS_Q" --data-urlencode limit=100
  [[ "$(loki_ns)" == *order-service* && "$(loki_ns)" == *payment-service* ]] || exit 1
  call "$CAROL" /tempo/api/search --data-urlencode 'q={}' --data-urlencode limit=50
  [[ "$(tempo_roots)" == *order-service* && "$(tempo_roots)" == *payment-service* ]] || exit 1
  # the history (Prometheus blocks, Loki and Tempo backfill) must be queryable as well
  call "$CAROL" /prom/api/v1/query --data-urlencode 'query=http_requests_total' --data-urlencode "time=$((NOW - MIN_AGE))"
  [[ "$(prom_ns)" == *order-service* && "$(prom_ns)" == *payment-service* ]] || exit 1
  call "$CAROL" /tempo/api/search --data-urlencode 'q={}' --data-urlencode "start=$((NOW - (HOURS + 2) * 3600))" \
    --data-urlencode "end=$((NOW - MIN_AGE))" --data-urlencode limit=100
  [[ "$(tempo_roots)" == *order-service* && "$(tempo_roots)" == *payment-service* ]] || exit 1
  exit 0
fi

# --- tokens ---------------------------------------------------------------------------------------
section "tokens (Keycloak, password grant, demo only)"
for u in alice:"$ALICE":order-team bob:"$BOB":payment-team carol:"$CAROL":platform-admins; do
  IFS=: read -r name tok grp <<<"$u"
  c=$(claims "$tok")
  expect "$name: aud contains querdian" true "$(jq -r '(.aud|if type=="array" then . else [.] end)|index("querdian")!=null' <<<"$c")"
  expect "$name: groups claim" "$grp" "$(jq -r '.groups|join(",")' <<<"$c")"
  expect "$name: issuer" "$ISSUER" "$(jq -r .iss <<<"$c")"
done

# --- authentication ---------------------------------------------------------------------------------
section "authentication"
call "" /prom/api/v1/query --data-urlencode 'query=up'
expect "no token -> 401" 401 "$STATUS"
call "garbage" /prom/api/v1/query --data-urlencode 'query=up'
expect "invalid token -> 401" 401 "$STATUS"
call "$ALICE" /prom/api/v1/admin/tsdb/snapshot
expect "unknown endpoint -> 403" 403 "$STATUS"

# --- metrics (PromQL) ----------------------------------------------------------------------------------
section "metrics: PromQL"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=up'
expect "alice: up -> namespaces" order-service "$(prom_ns)"
call "$BOB" /prom/api/v1/query --data-urlencode 'query=up'
expect "bob: up -> namespaces" payment-service "$(prom_ns)"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=demo_requests_total'
expect "alice: demo_requests_total -> namespaces" order-service "$(prom_ns)"
expect_ge "alice: demo_requests_total series" 3 "$(prom_n)"
call "$BOB" /prom/api/v1/query --data-urlencode 'query=demo_requests_total'
expect "bob: demo_requests_total -> namespaces" payment-service "$(prom_ns)"
expect_ge "bob: demo_requests_total series" 3 "$(prom_n)"
call "$CAROL" /prom/api/v1/query --data-urlencode 'query=count by (namespace) (up)'
ns=$(jq -r '[.data.result[].metric.namespace] | sort | join(",")' <<<"$BODY")
[[ "$ns" == *order-service* && "$ns" == *payment-service* && "$ns" == *observability* && "$ns" == *querdian* ]] \
  && pass "carol: up covers all namespaces ($ns)" || failed "carol: up namespaces: $ns"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=demo_requests_total{namespace="payment-service"}'
expect "alice: cross-namespace query returns nothing" 0 "$(prom_n)"
call "$BOB" /prom/api/v1/query --data-urlencode 'query=demo_requests_total{namespace="order-service"}'
expect "bob: cross-namespace query returns nothing" 0 "$(prom_n)"
call "$BOB" /prom/api/v1/query --data-urlencode 'query=demo_requests_total{namespace=~"order-service|payment-service"}'
expect "bob: regex over both namespaces still only his" payment-service "$(prom_ns)"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query={namespace!=""}'
expect "alice: selector with only a namespace matcher" order-service "$(prom_ns)"

section "metrics: namespace-label integrity (spoofed namespace label of order-service)"
call "$BOB" /prom/api/v1/query --data-urlencode 'query=demo_spoof_attempt'
expect "bob does not see the order-service series that claims namespace=payment-service" 0 "$(prom_n)"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=demo_spoof_attempt'
expect "alice: the spoofed series sits in her namespace" order-service "$(prom_ns)"
expect "alice: the claim was kept as exported_namespace" payment-service "$(jq -r '.data.result[0].metric.exported_namespace' <<<"$BODY")"

section "metrics: redacted label example (order-team redacts customer_id)"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=sum by (customer_id) (demo_requests_total)'
expect "alice: grouping by customer_id -> 403" 403 "$STATUS"
expect "alice: message" "label customer_id denied by policy (by/without)" "$(jq -r .error <<<"$BODY")"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=demo_requests_total{customer_id="c-1"}'
expect "alice: filtering on customer_id -> 403" 403 "$STATUS"
call "$ALICE" /prom/api/v1/label/customer_id/values
expect "alice: label values of customer_id -> 403" 403 "$STATUS"
call "$ALICE" /prom/api/v1/labels
expect "alice: customer_id hidden from the label list" false "$(jq -r '.data|index("customer_id")!=null' <<<"$BODY")"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=demo_requests_total'
expect "alice: raw query with customer_id in the result -> 200" 200 "$STATUS"
expect "alice: customer_id values are redacted" true \
  "$(jq -r '[.data.result[].metric.customer_id // empty] | length > 0 and all(test("^redacted-[0-9]+$"))' <<<"$BODY")"
expect "alice: redaction notice for Grafana" true \
  "$(jq -r '[.warnings[]? | startswith("querdian: label values redacted by policy: customer_id")] | any' <<<"$BODY")"
call "$BOB" /prom/api/v1/query --data-urlencode 'query=sum by (customer_id) (demo_requests_total)'
expect "bob (no deny): grouping by customer_id works" 200 "$STATUS"
call "$BOB" /prom/api/v1/labels
expect "bob: customer_id listed" true "$(jq -r '.data|index("customer_id")!=null' <<<"$BODY")"

# --- logs (LogQL) -----------------------------------------------------------------------------------------
section "logs: LogQL"
call "$ALICE" /loki/loki/api/v1/query_range --data-urlencode "query=$LOGS_Q" --data-urlencode limit=100
expect "alice: namespaces in logs" order-service "$(loki_ns)"
expect_ge "alice: log streams" 1 "$(loki_n)"
call "$BOB" /loki/loki/api/v1/query_range --data-urlencode "query=$LOGS_Q" --data-urlencode limit=100
expect "bob: namespaces in logs" payment-service "$(loki_ns)"
call "$CAROL" /loki/loki/api/v1/label/k8s_namespace_name/values
ns=$(jq -r '.data | join(",")' <<<"$BODY")
[[ "$ns" == *order-service* && "$ns" == *payment-service* && "$ns" == *querdian* ]] \
  && pass "carol: logs of all namespaces ($ns)" || failed "carol: log namespaces: $ns"
call "$ALICE" /loki/loki/api/v1/query_range --data-urlencode 'query={k8s_namespace_name="payment-service"}' --data-urlencode limit=100
expect "alice: cross-namespace log query returns nothing" 0 "$(loki_n)"
call "$BOB" /loki/loki/api/v1/query_range --data-urlencode 'query={k8s_namespace_name="order-service"} |= "request"' --data-urlencode limit=100
expect "bob: cross-namespace log query returns nothing" 0 "$(loki_n)"
call "$ALICE" /loki/loki/api/v1/query --data-urlencode 'query=sum by (k8s_namespace_name) (count_over_time({k8s_namespace_name=~".+"}[10m]))'
expect "alice: metric LogQL only counts her namespace" order-service "$(jq -r '[.data.result[].metric.k8s_namespace_name]|unique|join(",")' <<<"$BODY")"
call "$ALICE" /loki/loki/api/v1/label/k8s_namespace_name/values
expect "alice: label values" order-service "$(jq -r '.data|join(",")' <<<"$BODY")"
call "$BOB" /loki/loki/api/v1/label/k8s_namespace_name/values
expect "bob: label values" payment-service "$(jq -r '.data|join(",")' <<<"$BODY")"

# --- traces (TraceQL) -------------------------------------------------------------------------------------------
section "traces: TraceQL search and trace by ID"
call "$ALICE" /tempo/api/search --data-urlencode 'q={}' --data-urlencode limit=50
expect "alice: namespaces of found spans" order-service "$(tempo_ns)"
expect_ge "alice: traces" 1 "$(tempo_n)"
ALICE_TRACE=$(jq -r '.traces[0].traceID' <<<"$BODY")
call "$BOB" /tempo/api/search --data-urlencode 'q={}' --data-urlencode limit=50
expect "bob: namespaces of found spans" payment-service "$(tempo_ns)"
BOB_TRACE=$(jq -r '.traces[0].traceID' <<<"$BODY")
call "$CAROL" /tempo/api/search --data-urlencode 'q={}' --data-urlencode limit=100
r=$(tempo_roots)
[[ "$r" == *order-service* && "$r" == *payment-service* ]] && pass "carol: traces of both services ($r)" || failed "carol: root services: $r"
call "$ALICE" /tempo/api/search --data-urlencode 'q={ resource.k8s.namespace.name = "payment-service" }' --data-urlencode limit=50
expect "alice: cross-namespace TraceQL returns nothing" 0 "$(tempo_n)"
call "$BOB" /tempo/api/search --data-urlencode 'q={ resource.service.name = "order-service" }' --data-urlencode limit=50
expect "bob: searching order-service returns nothing" 0 "$(tempo_n)"
call "$ALICE" "/tempo/api/traces/$ALICE_TRACE"
expect "alice: own trace by ID" 200 "$STATUS"
call "$BOB" "/tempo/api/traces/$ALICE_TRACE"
expect "bob: alice's trace by ID -> 404 (filtered)" 404 "$STATUS"
call "$ALICE" "/tempo/api/traces/$BOB_TRACE"
expect "alice: bob's trace by ID -> 404 (filtered)" 404 "$STATUS"
call "$BOB" "/tempo/api/traces/$BOB_TRACE"
expect "bob: own trace by ID" 200 "$STATUS"
call "$CAROL" "/tempo/api/traces/$ALICE_TRACE"
expect "carol: any trace by ID" 200 "$STATUS"

section "traces: namespace-label integrity (order-service spans claim k8s.namespace.name=payment-service)"
call "$CAROL" /tempo/api/search --data-urlencode 'q={ resource.service.name = "order-service" && resource.k8s.namespace.name = "payment-service" }' --data-urlencode limit=50
expect "no span of order-service carries the spoofed namespace (collector overwrote it)" 0 "$(tempo_n)"
call "$CAROL" /tempo/api/search --data-urlencode 'q={ resource.service.name = "order-service" && resource.k8s.namespace.name = "order-service" }' --data-urlencode limit=50
expect_ge "order-service spans carry the namespace set by the collector" 1 "$(tempo_n)"

# --- history (backfilled demo data) -------------------------------------------------------------------
section "history: ${HOURS} h of backfilled metrics, logs and traces"
for u in alice:"$ALICE":order-service bob:"$BOB":payment-service; do
  IFS=: read -r name tok want <<<"$u"
  call "$tok" /prom/api/v1/query --data-urlencode 'query=http_requests_total' --data-urlencode "time=$((NOW - MIN_AGE))"
  expect "$name: metrics reach back $((MIN_AGE / 3600)) h, only his namespace" "$want" "$(prom_ns)"
  call "$tok" /prom/api/v1/query --data-urlencode 'query=http_requests_total' --data-urlencode "time=$((NOW - (HOURS + 1) * 3600))"
  expect "$name: no metrics older than the history" 0 "$(prom_n)"
done
call "$CAROL" /prom/api/v1/query --data-urlencode 'query=http_requests_total' --data-urlencode "time=$((NOW - MIN_AGE))"
ns=$(prom_ns)
[[ "$ns" == *order-service* && "$ns" == *payment-service* ]] && pass "carol: old metrics of both services ($ns)" || failed "carol: old metric namespaces: $ns"

# earliest log line: forward direction, limit 1
earliest_log_age() { # token -> ELA_AGE: age in seconds of the oldest visible log line (BODY is kept)
  call "$1" /loki/loki/api/v1/query_range --data-urlencode "query=$LOGS_Q" --data-urlencode direction=forward \
    --data-urlencode limit=1 --data-urlencode "start=${LOG_FROM}000000000" --data-urlencode "end=${NOW}000000000"
  local ts
  ts=$(jq -r '[.data.result[].values[][0] | tonumber] | min // empty' <<<"$BODY")
  ELA_AGE=0
  [ -z "$ts" ] || ELA_AGE=$((NOW - ts / 1000000000))
}
LOG_FROM=$((NOW - (HOURS + 2) * 3600))
for u in alice:"$ALICE":order-service bob:"$BOB":payment-service carol:"$CAROL":all; do
  IFS=: read -r name tok want <<<"$u"
  earliest_log_age "$tok"
  expect_ge "$name: oldest log line is at least $((MIN_AGE / 3600)) h old (s)" "$MIN_AGE" "$ELA_AGE"
  [ "$want" = all ] && continue
  expect "$name: oldest log line belongs to his namespace" "$want" "$(loki_ns)"
done
call "$ALICE" /loki/loki/api/v1/query --data-urlencode "query=sum(count_over_time({k8s_namespace_name=~\".+\"}[${HOURS}h]))" --data-urlencode "time=$NOW"
expect_ge "alice: log lines in the history window" 10000 "$(jq -r '.data.result[0].value[1] // 0 | tonumber | floor' <<<"$BODY")"
call "$BOB" /loki/loki/api/v1/query --data-urlencode "query=sum by (k8s_namespace_name) (count_over_time({k8s_namespace_name=~\".+\"}[${HOURS}h]))" --data-urlencode "time=$NOW"
expect "bob: history log lines are only payment-service" payment-service "$(jq -r '[.data.result[].metric.k8s_namespace_name]|unique|join(",")' <<<"$BODY")"
call "$ALICE" /loki/loki/api/v1/query --data-urlencode "query=sum by (detected_level) (count_over_time({k8s_namespace_name=\"order-service\"}[${HOURS}h]))" --data-urlencode "time=$NOW"
lv=$(jq -r '[.data.result[].metric.detected_level]|sort|join(",")' <<<"$BODY")
[[ "$lv" == *debug* && "$lv" == *error* && "$lv" == *info* && "$lv" == *warn* ]] && pass "alice: logs have the levels debug, error, info, warn ($lv)" || failed "alice: log levels: $lv"

# traces older than MIN_AGE: window [now-H-2h, now-23h]
old_traces() { # token
  call "$1" /tempo/api/search --data-urlencode 'q={}' --data-urlencode "start=$((NOW - (HOURS + 2) * 3600))" \
    --data-urlencode "end=$((NOW - MIN_AGE))" --data-urlencode limit=100
}
for u in alice:"$ALICE":order-service bob:"$BOB":payment-service; do
  IFS=: read -r name tok want <<<"$u"
  old_traces "$tok"
  expect_ge "$name: traces older than $((MIN_AGE / 3600)) h" 1 "$(tempo_n)"
  expect "$name: old traces only in his namespace" "$want" "$(tempo_ns)"
done
old_traces "$CAROL"
r=$(tempo_roots)
[[ "$r" == *order-service* && "$r" == *payment-service* ]] && pass "carol: old traces of both services ($r)" || failed "carol: old trace roots: $r"
call "$BOB" /tempo/api/search --data-urlencode 'q={}' --data-urlencode "start=$((NOW - (HOURS + 48) * 3600))" \
  --data-urlencode "end=$((NOW - (HOURS + 1) * 3600))" --data-urlencode limit=5
expect "bob: no traces older than the history" 0 "$(tempo_n)"

section "history: labels, routes, denied label"
for u in alice:"$ALICE" bob:"$BOB" carol:"$CAROL"; do
  name=${u%%:*}
  tok=${u#*:}
  call "$tok" /prom/api/v1/labels --data-urlencode "start=$((NOW - HOURS * 3600))" --data-urlencode "end=$((NOW - MIN_AGE + 3600))"
  lbl=$(jq -r '.data | join(",")' <<<"$BODY")
  [[ ",$lbl," == *,route,* && ",$lbl," == *,status,* && ",$lbl," == *,method,* ]] && pass "$name: old series carry route, method and status" || failed "$name: old labels: $lbl"
  want=true; [ "$name" = alice ] && want=false
  expect "$name: customer_id in the label list of the history" "$want" "$([[ ",$lbl," == *,customer_id,* ]] && echo true || echo false)"
done
call "$ALICE" /prom/api/v1/label/route/values
expect "alice: routes" "/cart,/orders,/orders/{id},/orders/{id}/cancel" "$(jq -r '.data|join(",")' <<<"$BODY")"
call "$BOB" /prom/api/v1/label/route/values
expect "bob: routes" "/payment-methods,/payments,/payments/{id},/payments/{id}/refund,/webhooks/provider" "$(jq -r '.data|join(",")' <<<"$BODY")"
call "$ALICE" /prom/api/v1/query_range --data-urlencode 'query=sum by (customer_id) (rate(demo_requests_total[5m]))' \
  --data-urlencode "start=$((NOW - 7200))" --data-urlencode "end=$NOW" --data-urlencode step=300
expect "alice: grouping the history by customer_id -> 403" 403 "$STATUS"
call "$BOB" /prom/api/v1/query_range --data-urlencode 'query=sum by (customer_id) (rate(demo_requests_total[5m]))' \
  --data-urlencode "start=$((NOW - HOURS * 3600 + 3600))" --data-urlencode "end=$((NOW - MIN_AGE + 7200))" --data-urlencode step=600
expect_ge "bob: customer_id series in the history" 3 "$(prom_n)"

section "history: incidents are in the data"
ratio() { # token, selector-extra, age-seconds -> 5xx ratio
  call "$1" /prom/api/v1/query --data-urlencode "query=sum(rate(http_requests_total{status=~\"5..\"$2}[10m])) / sum(rate(http_requests_total{$3}[10m]))" \
    --data-urlencode "time=$((NOW - $4))"
  jq -r '.data.result[0].value[1] // "0"' <<<"$BODY"
}
gt() { awk -v a="$1" -v b="$2" 'BEGIN { exit !(a > b) }'; }
if [ "$HOURS" -ge 8 ]; then
  bad=$(ratio "$ALICE" "" 'namespace="order-service"' $((3 * 3600 + 25 * 60)))
  good=$(ratio "$ALICE" "" 'namespace="order-service"' 3600)
  if gt "$bad" 0.08 && gt 0.03 "$good"; then pass "alice: order-service 5xx ratio during the bad deploy $bad vs $good an hour ago"; else failed "alice: bad deploy not visible ($bad vs $good)"; fi
  p95() { call "$BOB" /prom/api/v1/query --data-urlencode "query=histogram_quantile(0.95, sum by (le) (rate(http_request_duration_seconds_bucket{route=\"/payments\"}[10m])))" --data-urlencode "time=$((NOW - $1))"; jq -r '.data.result[0].value[1] // "0"' <<<"$BODY"; }
  slow=$(p95 $((7 * 3600))); fast=$(p95 $((5 * 3600)))
  if gt "$slow" "$(awk -v f="$fast" 'BEGIN{print f*3}')"; then pass "bob: POST /payments p95 during the provider incident ${slow}s vs ${fast}s"; else failed "bob: latency incident not visible (${slow}s vs ${fast}s)"; fi
fi

section "history: log -> trace links (trace_id in the log line exists in Tempo)"
for u in alice:"$ALICE":order-service:json bob:"$BOB":payment-service:logfmt; do
  IFS=: read -r name tok ns fmt <<<"$u"
  call "$tok" /loki/loki/api/v1/query_range --data-urlencode "query={k8s_namespace_name=\"$ns\"} | detected_level=\"error\" |= \"trace_id\"" \
    --data-urlencode "start=${LOG_FROM}000000000" --data-urlencode "end=${NOW}000000000" --data-urlencode limit=1 --data-urlencode direction=forward
  line=$(jq -r '.data.result[0].values[0][1] // ""' <<<"$BODY")
  tid=$(grep -oE 'trace_id"?[=:]"?[0-9a-f]{32}' <<<"$line" | grep -oE '[0-9a-f]{32}' | head -1)
  if [ -n "$tid" ]; then pass "$name: an old $fmt error log line carries trace_id $tid"; else failed "$name: no error log line with a trace id"; fi
  call "$tok" "/tempo/api/traces/$tid"
  expect "$name: that trace is found via Querdian" 200 "$STATUS"
  expect_ge "$name: the trace has several spans" 2 "$(jq -r '[.batches[].scopeSpans[].spans[]] | length' <<<"$BODY")"
  expect "$name: the error log line and the trace are one namespace" "$ns" "$(jq -r '[.batches[].resource.attributes[] | select(.key=="k8s.namespace.name") | .value.stringValue] | unique | join(",")' <<<"$BODY")"
  other=$([ "$ns" = order-service ] && echo "$BOB" || echo "$ALICE")
  call "$other" "/tempo/api/traces/$tid"
  expect "$name: the other team cannot fetch that trace (404)" 404 "$STATUS"
done

# --- Grafana ------------------------------------------------------------------------------------------------------
section "Grafana"
G="http://localhost:$GRAFANA_PORT"
expect "grafana health" ok "$(curl -fsS "$G/api/health" | jq -r .database)"
ds=$(curl -fsS -u admin:admin "$G/api/datasources")
for u in prometheus:prom loki:loki tempo:tempo; do
  uid=${u%%:*}
  pfx=${u##*:}
  expect "datasource $uid: url" "http://querdian.querdian:8080/$pfx" "$(jq -r --arg u "$uid" '.[]|select(.uid==$u).url' <<<"$ds")"
  expect "datasource $uid: oauthPassThru" true "$(jq -r --arg u "$uid" '.[]|select(.uid==$u).jsonData.oauthPassThru' <<<"$ds")"
done

# Headless browser flow: Grafana -> Keycloak login form -> Grafana session. The token Grafana then
# forwards to Querdian is the one it got in this flow.
gf_login() { # user password -> cookie jar in $WORK/jar-$1
  local jar="$WORK/jar-$1" action
  : >"$jar"
  curl -sSL -c "$jar" -b "$jar" "${CURL_KC[@]}" "$G/login/generic_oauth" -o "$WORK/login.html" || return 1
  action=$(grep -o 'action="[^"]*"' "$WORK/login.html" | head -1 | sed -e 's/^action="//' -e 's/"$//' -e 's/&amp;/\&/g')
  [ -n "$action" ] || return 1
  curl -sSL -c "$jar" -b "$jar" "${CURL_KC[@]}" -o /dev/null -d "username=$1" -d "password=$2" -d credentialId= "$action"
}
gf() { curl -fsS -b "$WORK/jar-$1" "${CURL_KC[@]}" "$G$2" "${@:3}" 2>/dev/null; }

HEADLESS_OK=1
for u in alice:alice-demo bob:bob-demo carol:carol-demo; do
  name=${u%%:*}
  if gf_login "$name" "${u##*:}" && [ "$(gf "$name" /api/user | jq -r .login)" = "$name" ]; then
    pass "grafana: Keycloak login as $name"
  else
    failed "grafana: headless Keycloak login as $name"
    HEADLESS_OK=0
  fi
done
if [ $HEADLESS_OK = 1 ]; then
  gq() { # user, datasource proxy path, curl args
    local u="$1" p="$2"
    shift 2
    curl -sS -b "$WORK/jar-$u" -G "$G/api/datasources/proxy/uid/$p" "$@"
  }
  for u in alice:order-service bob:payment-service; do
    name=${u%%:*}
    want=${u##*:}
    BODY=$(gq "$name" prometheus/api/v1/query --data-urlencode 'query=demo_requests_total')
    expect "grafana as $name: Prometheus data source (OAuth pass-through)" "$want" "$(prom_ns)"
    BODY=$(gq "$name" loki/loki/api/v1/query_range --data-urlencode "query=$LOGS_Q" --data-urlencode limit=100)
    expect "grafana as $name: Loki data source" "$want" "$(loki_ns)"
    BODY=$(gq "$name" tempo/api/search --data-urlencode 'q={}' --data-urlencode limit=50)
    expect "grafana as $name: Tempo data source" "$want" "$(tempo_ns)"
  done
  # The local admin has no OAuth token, so nothing is forwarded and Querdian refuses.
  code=$(curl -sS -o /dev/null -w '%{http_code}' -u admin:admin -G "$G/api/datasources/proxy/uid/prometheus/api/v1/query" --data-urlencode query=up)
  expect "grafana as local admin (no token): query refused" "refused" "$([ "$code" != 200 ] && echo refused || echo "$code")"
fi

if [ $HEADLESS_OK = 1 ]; then
# --- dashboards -----------------------------------------------------------------------------------------------------
section "dashboards: provisioned from hack/quickstart/dashboards"
DASH_DIR="$QS_DIR/dashboards"
shopt -s nullglob
DASH_FILES=("$DASH_DIR"/*.json)
shopt -u nullglob
expect_ge "dashboard files in the repo" 4 "${#DASH_FILES[@]}"
found=$(curl -fsS -u admin:admin "$G/api/search?type=dash-db&tag=querdian-demo" | jq -r '[.[].uid] | sort | join(",")')
want=$(jq -r '.uid' "${DASH_FILES[@]}" | sort | paste -sd, -)
expect "grafana lists exactly the repo's dashboards" "$want" "$found"
for f in "${DASH_FILES[@]}"; do
  uid=$(jq -r .uid "$f")
  d=$(curl -fsS -u admin:admin "$G/api/dashboards/uid/$uid")
  expect "dashboard $uid: provisioned (read-only, from file)" true "$(jq -r '.meta.provisioned' <<<"$d")"
  expect "dashboard $uid: folder" "Querdian demo" "$(jq -r '.meta.folderTitle' <<<"$d")"
  bad=$(jq -r --argjson ds "$(jq -c '[.[].uid]' <<<"$ds")" '[.. | objects | select(has("datasource")) | .datasource | select(type=="object") | .uid | select(. != null and (. as $u | $ds | index($u) | not))] | unique | join(",")' "$f")
  expect "dashboard $uid: every datasource uid is provisioned" "" "$bad"
done
home=$(curl -fsS -u admin:admin "$G/api/dashboards/home" | jq -r '.dashboard.uid // .redirectUri // ""')
[[ "$home" == *demo-service-overview* || "$home" == *service-overview* ]] && pass "home dashboard is the service overview" || failed "home dashboard: '$home'"

# Datasource-resource calls behind the dashboard variables (label values and names).
for u in alice:order-service bob:payment-service carol:order-service,payment-service; do
  name=${u%%:*}
  want=${u#*:}
  BODY=$(gq "$name" prometheus/api/v1/label/namespace/values --data-urlencode 'match[]=http_requests_total')
  expect "grafana as $name: variable namespace (Prometheus label values)" "$want" "$(jq -r '.data|join(",")' <<<"$BODY")"
  BODY=$(gq "$name" loki/loki/api/v1/label/k8s_namespace_name/values)
  ns=$(jq -r '.data|join(",")' <<<"$BODY")
  if [ "$name" = carol ]; then
    [[ "$ns" == *order-service* && "$ns" == *payment-service* ]] && pass "grafana as carol: Loki namespaces ($ns)" || failed "carol Loki namespaces: $ns"
  else
    expect "grafana as $name: variable Loki namespaces" "$want" "$ns"
  fi
  BODY=$(gq "$name" prometheus/api/v1/labels)
  hidden=false
  [ "$name" = alice ] && hidden=true
  expect "grafana as $name: customer_id hidden in the label_names() variable" "$hidden" "$(jq -r '.data|index("customer_id")==null' <<<"$BODY")"
done

section "dashboards: every panel query, as alice, bob and carol (POST /api/ds/query through Grafana)"
# Template variables are replaced the way an "All" selection would: regex ".+".
SUBST='def subst: walk(if type == "string" then
    gsub("\\$\\{?namespace\\}?"; ".+") | gsub("\\$\\{?route\\}?"; ".+") | gsub("\\$\\{?level\\}?"; ".+")
    | gsub("\\$\\{?search\\}?"; "") | gsub("\\$\\{?min_duration\\}?"; "500ms") | gsub("\\$\\{?traceid\\}?"; "") else . end);'
sees() { # user namespace -> true/false
  case "$1:$2" in alice:order-service | bob:payment-service | carol:*) echo true ;; *) echo false ;; esac
}
from_secs() { local h=${1#now-}; echo $(( ${h%h} * 3600 )); } # "now-24h" -> 86400
panel_has_data() { jq -r '[.results[]?.frames[]?.data.values[0] | length] | add // 0' <<<"$1"; }
total=0
for u in alice bob carol; do
  nq=0
  bad=0
  for f in "${DASH_FILES[@]}"; do
    from=$(jq -r .time.from "$f")
    while IFS= read -r row; do
      title=$(jq -r .title <<<"$row")
      q=$(jq -c "$SUBST"' .target | subst | . + {intervalMs: 60000, maxDataPoints: 800}' <<<"$row")
      # skip queries that are empty after substitution (the trace-by-ID panel without an ID)
      [ "$(jq -r '(.expr // .query // "x")' <<<"$q")" = "" ] && continue
      if [ "$(jq -r '.datasource.type' <<<"$q")" = tempo ]; then
        # Grafana runs TraceQL searches in the browser, not in its backend: the data source
        # proxy gets GET /api/search?q=...&limit=...&spss=...&start=...&end=...
        resp=$(curl -sS -b "$WORK/jar-$u" -G "$G/api/datasources/proxy/uid/tempo/api/search" \
          --data-urlencode "q=$(jq -r .query <<<"$q")" --data-urlencode "limit=$(jq -r '.limit // 20' <<<"$q")" \
          --data-urlencode spss=3 --data-urlencode "start=$((NOW - $(from_secs "$from")))" --data-urlencode "end=$NOW")
        err=""
        jq -e '.traces' <<<"$resp" >/dev/null 2>&1 || err="$resp"
        n=$(jq -r '.traces | length' <<<"$resp" 2>/dev/null || echo 0)
      else
        resp=$(curl -sS -b "$WORK/jar-$u" -H 'Content-Type: application/json' -X POST "$G/api/ds/query" \
          -d "{\"queries\":[$q],\"from\":\"$from\",\"to\":\"now\"}")
        err=$(jq -r '(.message // "") as $m | ([.results[]?.error // empty] | first // $m)' <<<"$resp")
        n=$(panel_has_data "$resp")
      fi
      nq=$((nq + 1))
      desc="$u: [$(jq -r .uid "$f")] $title"
      if [[ "$title" == *"denied for order-team"* && "$u" == alice ]]; then
        if [[ "$err" == *customer_id* ]]; then pass "$desc -> refused by Querdian (denied label)"; else failed "$desc: expected a denied-label error, got '$err' / $n values"; bad=$((bad + 1)); fi
      elif [ -n "$err" ]; then
        failed "$desc: error: $err"
        bad=$((bad + 1))
      elif [[ "$title" =~ \[ns:([a-z-]+)\] ]]; then
        if [ "$(sees "$u" "${BASH_REMATCH[1]}")" = true ]; then expect_ge "$desc (own namespace has data)" 1 "$n"; else expect "$desc (other namespace is empty)" 0 "$n"; fi
      elif ! [ "$n" -ge 1 ] 2>/dev/null; then
        failed "$desc: no data"
        bad=$((bad + 1))
      fi
    done < <(jq -c '[.. | objects | select(has("targets") and .type != "row") | . as $p | .targets[] | {title: $p.title, target: .}] | .[]' "$f")
  done
  total=$((total + nq))
  if [ $bad = 0 ]; then pass "$u: all $nq dashboard queries return data (or the expected refusal)"; else failed "$u: $bad of $nq dashboard queries failed"; fi
done
[ $SMOKE = 1 ] || printf '    dashboard queries executed: %s\n' "$total"
fi

echo
echo "Summary: $PASS passed, $FAILN failed"
[ "$FAILN" = 0 ]
