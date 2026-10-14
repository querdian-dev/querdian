#!/usr/bin/env bash
# Verifies the quickstart end to end: two users see different data, admins see everything.
#
#   hack/quickstart/verify.sh            # full verification (CLUSTER=querdian-demo by default)
#   hack/quickstart/verify.sh --smoke    # quick "is data flowing" check, used by up.sh
#
# 1. Gets access tokens from Keycloak with the password grant (direct access grants are enabled
#    on the demo client `grafana` ONLY FOR THIS DEMO) for alice, bob and carol.
# 2. Queries the Querdian proxy directly (kubectl port-forward) with PromQL, LogQL, TraceQL
#    search and trace-by-ID and asserts who sees what, including the denied-label example and the
#    spoofed-namespace attempts of the order-service sample.
# 3. Checks Grafana: health, provisioned data sources, and (headless) the real browser flow:
#    Keycloak login form -> Grafana session -> data source proxy queries with OAuth pass-through.
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
claims() { printf '%s' "$1" | cut -d. -f2 | tr '_-' '/+' | base64 -d 2>/dev/null; }

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
tempo_roots() { jq -r '[.traces[].rootServiceName] | unique | join(",")' <<<"$BODY"; }

# --- connectivity --------------------------------------------------------------------------------
k -n querdian get svc querdian >/dev/null 2>&1 || die "cluster $CLUSTER is not up (run hack/quickstart/up.sh)"
kubectl --context "$KCTX" -n querdian port-forward svc/querdian "$QPORT:8080" >/dev/null 2>&1 &
PF_PID=$!
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

if [ $SMOKE = 1 ]; then
  # Data must have arrived everywhere (carol sees all namespaces).
  call "$CAROL" /prom/api/v1/query --data-urlencode 'query=demo_requests_total'
  [[ "$(prom_ns)" == *order-service* && "$(prom_ns)" == *payment-service* ]] || exit 1
  call "$CAROL" /loki/loki/api/v1/query_range --data-urlencode "query=$LOGS_Q" --data-urlencode limit=100
  [[ "$(loki_ns)" == *order-service* && "$(loki_ns)" == *payment-service* ]] || exit 1
  call "$CAROL" /tempo/api/search --data-urlencode 'q={}' --data-urlencode limit=50
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

section "metrics: denied label example (order-team denies customer_id)"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=sum by (customer_id) (demo_requests_total)'
expect "alice: grouping by customer_id -> 403" 403 "$STATUS"
expect "alice: reason" denied_label "$(jq -r '.reason // .error' <<<"$BODY" | grep -o 'denied_label' | head -1)"
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=demo_requests_total{customer_id="c-1"}'
expect "alice: filtering on customer_id -> 403" 403 "$STATUS"
call "$ALICE" /prom/api/v1/label/customer_id/values
expect "alice: label values of customer_id -> 403" 403 "$STATUS"
call "$ALICE" /prom/api/v1/labels
expect "alice: customer_id hidden from the label list" false "$(jq -r '.data|index("customer_id")!=null' <<<"$BODY")"
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
call "$CAROL" /loki/loki/api/v1/query_range --data-urlencode "query=$LOGS_Q" --data-urlencode limit=500
ns=$(jq -r '[.data.result[].stream.k8s_namespace_name] | unique | join(",")' <<<"$BODY")
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
expect "alice: root services of found traces" order-service "$(tempo_roots)"
expect_ge "alice: traces" 1 "$(tempo_n)"
ALICE_TRACE=$(jq -r '.traces[0].traceID' <<<"$BODY")
call "$BOB" /tempo/api/search --data-urlencode 'q={}' --data-urlencode limit=50
expect "bob: root services of found traces" payment-service "$(tempo_roots)"
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
for u in alice:alice-demo bob:bob-demo; do
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
    expect "grafana as $name: Tempo data source" "$want" "$(tempo_roots)"
  done
  # The local admin has no OAuth token, so nothing is forwarded and Querdian refuses.
  code=$(curl -sS -o /dev/null -w '%{http_code}' -u admin:admin -G "$G/api/datasources/proxy/uid/prometheus/api/v1/query" --data-urlencode query=up)
  expect "grafana as local admin (no token): Querdian answers 401" 401 "$code"
fi

echo
echo "Summary: $PASS passed, $FAILN failed"
[ "$FAILN" = 0 ]
