#!/usr/bin/env bash
# Verifies the multi-cluster quickstart end to end, through the Querdian proxy of the hub with real
# Keycloak tokens.
#
#   hack/quickstart-multicluster/verify.sh            # full verification
#   hack/quickstart-multicluster/verify.sh --smoke    # quick "is data flowing" check, used by up.sh
#
# 1. Gets tokens from Keycloak with the password grant (demo only) for alice (team-a, spoke-a), bob
#    (team-b, spoke-b), hana (team-hub, hub), sam (sre, both spokes) and carol (platform-admins, all).
# 2. Checks metrics, logs and traces: each team sees only the namespace order-service of its own
#    cluster, although all three clusters have that namespace. The pods that a team sees are pods of its
#    own cluster, although the collector of each spoke claims a false cluster label. sam sees both spokes
#    and not the hub; carol sees all clusters.
# 3. Checks the SpokeCluster and SpokePolicy objects on the hub, the Ready condition that the agent
#    writes in each spoke, and the origin of each policy in the policy dashboard API.
# 4. Suspends spoke-a: alice gets 403 (policy source expired) on all three backends, the dashboard shows
#    the expired state, bob and sam keep their access. Then unsuspends spoke-a and waits for the access
#    of alice to return. The script unsuspends spoke-a also if it stops early.
#
# Requires: curl, jq, kubectl. Exit status 1 if any assertion fails.
set -uo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

SMOKE=0
[ "${1:-}" = "--smoke" ] && SMOKE=1
QPORT="${QUERDIAN_LOCAL_PORT:-18080}"
WORK="$(mktemp -d)"
PASS=0
FAILN=0
SUSPENDED=""
S1="${SPOKES[0]}" # spoke-a
S2="${SPOKES[1]}" # spoke-b

cleanup() {
  if [ -n "$SUSPENDED" ]; then
    k patch spokecluster "$SUSPENDED" --type merge -p '{"spec":{"suspended":false}}' >/dev/null 2>&1 &&
      echo "NOTE: $SUSPENDED was unsuspended again; its grants return after the next sync" >&2
  fi
  pf_stop
  rm -rf "$WORK"
}
trap cleanup EXIT

need curl jq kubectl

# The same helpers as hack/quickstart/verify.sh.
pass() { PASS=$((PASS + 1)); [ $SMOKE = 1 ] || printf 'PASS: %s\n' "$*"; }
failed() { FAILN=$((FAILN + 1)); printf 'FAIL: %s\n' "$*" >&2; }
expect() { # description, expected, actual
  if [ "$2" = "$3" ]; then pass "$1 ($3)"; else failed "$1: expected '$2', got '$3'"; fi
}
expect_ge() { # description, minimum, actual
  if [ "$3" -ge "$2" ] 2>/dev/null; then pass "$1 ($3)"; else failed "$1: expected >= $2, got '$3'"; fi
}
section() { [ $SMOKE = 1 ] || printf '\n--- %s\n' "$*"; }
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

# Values of one label in a PromQL / Loki streams result, sorted and comma separated.
prom_vals() { jq -r --arg l "$1" '[.data.result[].metric[$l] // empty] | unique | join(",")' <<<"$BODY"; }
prom_n() { jq -r '.data.result | length' <<<"$BODY"; }
loki_vals() { jq -r --arg l "$1" '[.data.result[].stream[$l] // empty] | unique | join(",")' <<<"$BODY"; }
loki_n() { jq -r '.data.result | length' <<<"$BODY"; }
tempo_n() { jq -r '.traces | length' <<<"$BODY"; }
# Values of a resource attribute of the matched spans (Querdian adds the policy matchers to the query,
# so Tempo returns these attributes with each span).
tempo_vals() { jq -r --arg k "$1" '[.traces[].spanSet.spans[].attributes[]? | select(.key==$k) | .value.stringValue] | unique | join(",")' <<<"$BODY"; }
# Values of a resource attribute in a trace-by-ID result.
trace_vals() { jq -r --arg k "$1" '[.batches[].resource.attributes[] | select(.key==$k) | .value.stringValue] | unique | join(",")' <<<"$BODY"; }
joined() { printf '%s\n' "$@" | sort -u | paste -sd, -; }

# --- connectivity ---------------------------------------------------------------------------------------------
k -n querdian get svc querdian >/dev/null 2>&1 || die "cluster $CLUSTER is not up (run hack/quickstart-multicluster/up.sh)"
pf "$KCTX" querdian querdian "$QPORT" 8080

ALICE=$(token alice alice-demo) || ALICE=""
BOB=$(token bob bob-demo) || BOB=""
HANA=$(token hana hana-demo) || HANA=""
SAM=$(token sam sam-demo) || SAM=""
CAROL=$(token carol carol-demo) || CAROL=""
for t in "$ALICE" "$BOB" "$HANA" "$SAM" "$CAROL"; do
  if [ -z "$t" ] || [ "$t" = null ]; then
    failed "could not get tokens from $ISSUER"
    [ $SMOKE = 1 ] || echo "Summary: $PASS passed, $FAILN failed"
    exit 1
  fi
done
ALL="$(joined "$HUB_NAME" "${SPOKES[@]}")"
BOTH_SPOKES="$(joined "${SPOKES[@]}")"
LOGS_Q='{k8s_namespace_name=~".+"}'

if [ $SMOKE = 1 ]; then
  # Data of all three clusters arrived (carol is unrestricted), and the spoke grants are active.
  call "$CAROL" /prom/api/v1/query --data-urlencode 'query=count by (cluster) (demo_requests_total)'
  [ "$(prom_vals cluster)" = "$ALL" ] || exit 1
  call "$CAROL" /loki/loki/api/v1/label/k8s_cluster_name/values
  [ "$(jq -r '.data // [] | sort | join(",")' <<<"$BODY")" = "$ALL" ] || exit 1
  for c in "$HUB_NAME" "${SPOKES[@]}"; do
    call "$CAROL" /tempo/api/search --data-urlencode "q={ resource.k8s.cluster.name = \"$c\" }" --data-urlencode limit=5
    [ "$(tempo_n)" -ge 1 ] 2>/dev/null || exit 1
  done
  for u in "$ALICE:$S1" "$BOB:$S2"; do
    call "${u%%:*}" /prom/api/v1/query --data-urlencode 'query=demo_requests_total'
    [ "$(prom_vals cluster)" = "${u##*:}" ] || exit 1
  done
  exit 0
fi

# Pods of each cluster, from the API servers. A team must see only pods of its own cluster.
pods_of() { # cluster-name namespace...
  local c="$1" ns
  shift
  for ns in "$@"; do
    if [ "$c" = "$HUB_NAME" ]; then k -n "$ns" get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'
    else ks "$c" -n "$ns" get pods -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}'; fi
  done
}
for c in "$HUB_NAME" "${SPOKES[@]}"; do pods_of "$c" order-service payment-service >"$WORK/pods-$c"; done
# only_pods_of CLUSTER "pod,pod,...": every pod is a pod of CLUSTER, and none is a pod of another cluster.
only_pods_of() {
  local c="$1" p o bad=""
  [ -n "$2" ] || { echo "no pods"; return; }
  for p in ${2//,/ }; do
    grep -qx "$p" "$WORK/pods-$c" || bad="$bad $p(not in $c)"
    for o in "$HUB_NAME" "${SPOKES[@]}"; do
      [ "$o" = "$c" ] || ! grep -qx "$p" "$WORK/pods-$o" || bad="$bad $p(of $o)"
    done
  done
  echo "${bad:-ok}"
}

# --- tokens -----------------------------------------------------------------------------------------------------
section "tokens (Keycloak, password grant, demo only)"
for u in alice:"$ALICE":team-a bob:"$BOB":team-b hana:"$HANA":team-hub sam:"$SAM":sre carol:"$CAROL":platform-admins; do
  IFS=: read -r name tok grp <<<"$u"
  c=$(claims "$tok")
  expect "$name: groups claim" "$grp" "$(jq -r '.groups|join(",")' <<<"$c")"
  expect "$name: aud contains querdian" true "$(jq -r '(.aud|if type=="array" then . else [.] end)|index("querdian")!=null' <<<"$c")"
done

# Each team: the user, the token and the cluster of its grant. All three grants are for the namespace
# order-service; only the cluster differs.
TEAMS=("alice:$ALICE:$S1" "bob:$BOB:$S2" "hana:$HANA:$HUB_NAME")

# --- metrics ----------------------------------------------------------------------------------------------------
section "metrics: one namespace name in three clusters"
for u in "${TEAMS[@]}"; do
  IFS=: read -r name tok own <<<"$u"
  call "$tok" /prom/api/v1/query --data-urlencode 'query=demo_requests_total'
  expect "$name: demo_requests_total -> clusters" "$own" "$(prom_vals cluster)"
  expect "$name: demo_requests_total -> namespaces" order-service "$(prom_vals namespace)"
  expect_ge "$name: demo_requests_total series" 3 "$(prom_n)"
  expect "$name: the pods are pods of $own" ok "$(only_pods_of "$own" "$(prom_vals pod)")"
  for c in "$HUB_NAME" "${SPOKES[@]}"; do
    [ "$c" = "$own" ] && continue
    call "$tok" /prom/api/v1/query --data-urlencode "query=demo_requests_total{cluster=\"$c\"}"
    expect "$name: explicit cluster=\"$c\" returns nothing" 0 "$(prom_n)"
  done
  call "$tok" /prom/api/v1/label/cluster/values
  expect "$name: label values of cluster" "$own" "$(jq -r '.data | join(",")' <<<"$BODY")"
done
call "$ALICE" /prom/api/v1/query --data-urlencode 'query=demo_requests_total{namespace="payment-service"}'
expect "alice: other namespace of her own cluster returns nothing" 0 "$(prom_n)"
call "$SAM" /prom/api/v1/query --data-urlencode 'query=count by (cluster, namespace) (demo_requests_total)'
expect "sam (sre): clusters" "$BOTH_SPOKES" "$(prom_vals cluster)"
expect "sam (sre): namespaces" "order-service,payment-service" "$(prom_vals namespace)"
call "$SAM" /prom/api/v1/query --data-urlencode "query=demo_requests_total{cluster=\"$HUB_NAME\"}"
expect "sam (sre): the hub is not in his fixed cluster set" 0 "$(prom_n)"
call "$CAROL" /prom/api/v1/query --data-urlencode 'query=count by (cluster) (demo_requests_total)'
expect "carol (unrestricted): clusters" "$ALL" "$(prom_vals cluster)"

section "metrics: cluster-label integrity (each spoke collector claims a false cluster)"
for c in "$HUB_NAME" "${SPOKES[@]}"; do
  call "$CAROL" /prom/api/v1/query --data-urlencode "query=count by (pod) (demo_requests_total{cluster=\"$c\"})"
  expect "carol: the series with cluster=\"$c\" come from pods of $c" ok "$(only_pods_of "$c" "$(prom_vals pod)")"
done
call "$CAROL" /prom/api/v1/query --data-urlencode 'query=count({k8s_cluster_name!=""})'
expect "carol: no metric has the label k8s_cluster_name (the gateway deletes it)" 0 "$(prom_n)"

# --- logs -------------------------------------------------------------------------------------------------------
section "logs: one namespace name in three clusters"
for u in "${TEAMS[@]}"; do
  IFS=: read -r name tok own <<<"$u"
  call "$tok" /loki/loki/api/v1/query_range --data-urlencode "query=$LOGS_Q" --data-urlencode limit=200
  expect "$name: log streams -> clusters" "$own" "$(loki_vals k8s_cluster_name)"
  expect "$name: log streams -> namespaces" order-service "$(loki_vals k8s_namespace_name)"
  expect "$name: the pods are pods of $own" ok "$(only_pods_of "$own" "$(loki_vals k8s_pod_name)")"
  for c in "$HUB_NAME" "${SPOKES[@]}"; do
    [ "$c" = "$own" ] && continue
    call "$tok" /loki/loki/api/v1/query_range --data-urlencode "query={k8s_cluster_name=\"$c\"}" --data-urlencode limit=10
    expect "$name: explicit k8s_cluster_name=\"$c\" returns nothing" 0 "$(loki_n)"
  done
  call "$tok" /loki/loki/api/v1/label/k8s_cluster_name/values
  expect "$name: label values of k8s_cluster_name" "$own" "$(jq -r '.data // [] | join(",")' <<<"$BODY")"
done
call "$SAM" /loki/loki/api/v1/label/k8s_cluster_name/values
expect "sam (sre): log clusters" "$BOTH_SPOKES" "$(jq -r '.data // [] | sort | join(",")' <<<"$BODY")"
call "$CAROL" /loki/loki/api/v1/label/k8s_cluster_name/values
expect "carol (unrestricted): log clusters" "$ALL" "$(jq -r '.data // [] | sort | join(",")' <<<"$BODY")"
for c in "$HUB_NAME" "${SPOKES[@]}"; do
  call "$CAROL" /loki/loki/api/v1/label/k8s_pod_name/values --data-urlencode "query={k8s_cluster_name=\"$c\", k8s_namespace_name=~\"order-service|payment-service\"}"
  expect "carol: the log streams with k8s_cluster_name=\"$c\" come from pods of $c" ok \
    "$(only_pods_of "$c" "$(jq -r '.data // [] | join(",")' <<<"$BODY")")"
done

# --- traces -----------------------------------------------------------------------------------------------------
section "traces: one namespace name in three clusters"
for u in "${TEAMS[@]}"; do
  IFS=: read -r name tok own <<<"$u"
  call "$tok" /tempo/api/search --data-urlencode 'q={}' --data-urlencode limit=50
  expect_ge "$name: traces" 1 "$(tempo_n)"
  expect "$name: clusters of the found spans" "$own" "$(tempo_vals k8s.cluster.name)"
  expect "$name: namespaces of the found spans" order-service "$(tempo_vals k8s.namespace.name)"
  tid=$(jq -r '.traces[0].traceID // empty' <<<"$BODY")
  printf '%s\n' "$tid" >"$WORK/trace-$name"
  call "$tok" "/tempo/api/traces/$tid"
  expect "$name: own trace by ID" 200 "$STATUS"
  expect "$name: the trace comes from pods of $own" ok "$(only_pods_of "$own" "$(trace_vals k8s.pod.name)")"
  for c in "$HUB_NAME" "${SPOKES[@]}"; do
    [ "$c" = "$own" ] && continue
    call "$tok" /tempo/api/search --data-urlencode "q={ resource.k8s.cluster.name = \"$c\" }" --data-urlencode limit=10
    expect "$name: explicit resource.k8s.cluster.name = \"$c\" returns nothing" 0 "$(tempo_n)"
  done
done
ALICE_TRACE=$(cat "$WORK/trace-alice")
call "$BOB" "/tempo/api/traces/$ALICE_TRACE"
expect "bob: the trace of alice (spoke-a) by ID -> 404 (filtered)" 404 "$STATUS"
call "$HANA" "/tempo/api/traces/$ALICE_TRACE"
expect "hana: the trace of alice (spoke-a) by ID -> 404 (filtered)" 404 "$STATUS"
call "$SAM" "/tempo/api/traces/$ALICE_TRACE"
expect "sam (sre): the trace of alice by ID" 200 "$STATUS"
for c in "$HUB_NAME" "${SPOKES[@]}"; do
  want=1
  [ "$c" = "$HUB_NAME" ] && want=0
  call "$SAM" /tempo/api/search --data-urlencode "q={ resource.k8s.cluster.name = \"$c\" }" --data-urlencode limit=5
  if [ $want = 1 ]; then expect_ge "sam (sre): traces of $c" 1 "$(tempo_n)"; else expect "sam (sre): no traces of $c" 0 "$(tempo_n)"; fi
  call "$CAROL" /tempo/api/search --data-urlencode "q={ resource.k8s.cluster.name = \"$c\" }" --data-urlencode limit=5
  expect_ge "carol (unrestricted): traces of $c" 1 "$(tempo_n)"
done

# --- spoke state ------------------------------------------------------------------------------------------------
section "spoke state on the hub and in the spokes"
NOW=$(date +%s)
iso_age() { # RFC 3339 time -> age in seconds (GNU date or BSD date)
  local t
  t=$(date -u -d "$1" +%s 2>/dev/null || date -u -j -f '%Y-%m-%dT%H:%M:%SZ' "$1" +%s 2>/dev/null) || { echo 999999; return; }
  echo $((NOW - t))
}
for s in "${SPOKES[@]}"; do
  sc=$(k get spokecluster "$s" -o json)
  expect "SpokeCluster $s: suspended" false "$(jq -r '.spec.suspended' <<<"$sc")"
  expect "SpokeCluster $s: identity" "$(agent_identity "$s")" "$(jq -r '.spec.identity' <<<"$sc")"
  expect "SpokeCluster $s: observedGeneration is the generation" "$(jq -r '.metadata.generation' <<<"$sc")" \
    "$(jq -r '.status.observedGeneration' <<<"$sc")"
  age=$(iso_age "$(jq -r '.status.lastSyncTime // ""' <<<"$sc")")
  if [ "$age" -le 120 ]; then pass "SpokeCluster $s: lastSyncTime is recent (${age}s)"; else failed "SpokeCluster $s: lastSyncTime is ${age}s old"; fi
  sp=$(k get spokepolicies -l "policy.querdian.dev/cluster=$s" -o json)
  expect "SpokePolicy objects of $s" 1 "$(jq -r '.items | length' <<<"$sp")"
  expect "SpokePolicy of $s: source" "AccessPolicy/order-service/team-${s#spoke-}" \
    "$(jq -r '.items[0].spec | "\(.kind)/\(.namespace)/\(.name)"' <<<"$sp")"
  expect "SpokePolicy of $s: Ready" True "$(jq -r '.items[0].status.conditions[]? | select(.type=="Ready") | .status' <<<"$sp")"
  ap=$(ks "$s" -n order-service get accesspolicy "team-${s#spoke-}" -o json)
  expect "spoke $s: the agent wrote Ready on the AccessPolicy" True "$(jq -r '.status.conditions[]? | select(.type=="Ready") | .status' <<<"$ap")"
  expect "spoke $s: no standalone proxy in the spoke" 0 "$(ks "$s" get deploy -A -l app.kubernetes.io/name=querdian -o name | wc -l | tr -d ' ')"
done

section "policy dashboard: the origin of each policy (http://localhost:$UI_PORT)"
UI="http://127.0.0.1:$UI_PORT"
pol=$(curl -fsS "$UI/api/policies" 2>/dev/null) || pol='{}'
expect "dashboard: hub AccessPolicy team-hub has origin hub" true \
  "$(jq -r '[.policies[]? | select(.kind=="AccessPolicy" and .name=="team-hub") | .origin.hub] | first // "absent"' <<<"$pol")"
for s in "${SPOKES[@]}"; do
  expect "dashboard: the spoke policy of $s has origin cluster $s, AccessPolicy order-service/team-${s#spoke-}" \
    "$s/AccessPolicy/order-service/team-${s#spoke-}/false" \
    "$(jq -r --arg s "$s" '[.policies[]? | select(.origin.cluster==$s) | "\(.origin.cluster)/\(.origin.kind)/\(.origin.namespace)/\(.origin.name)/\(.expired // false)"] | join(" ")' <<<"$pol")"
done

# --- suspend and unsuspend --------------------------------------------------------------------------------------
section "suspend $S1: its grants stop, hub grants and other spokes stay"
EXPIRED_MSG="the policy source of this grant has expired"
k patch spokecluster "$S1" --type merge -p '{"spec":{"suspended":true}}' >/dev/null
SUSPENDED="$S1"
alice_status() { call "$ALICE" /prom/api/v1/query --data-urlencode 'query=demo_requests_total'; [ "$STATUS" = "$1" ]; }
for _ in $(seq 1 30); do alice_status 403 && break; sleep 1; done
for p in "/prom/api/v1/query:query=demo_requests_total" "/loki/loki/api/v1/query_range:query=$LOGS_Q" "/tempo/api/search:q={}"; do
  call "$ALICE" "${p%%:*}" --data-urlencode "${p#*:}"
  expect "alice: ${p%%:*} while $S1 is suspended -> 403" 403 "$STATUS"
  expect "alice: message (does not name the cluster)" "$EXPIRED_MSG" "$(jq -r '.error // empty' <<<"$BODY")"
done
acc=$(curl -fsS "$UI/api/access?groups=team-a" 2>/dev/null) || acc='{}'
expect "dashboard access of team-a: the reason on each backend" "policy_source_expired" \
  "$(jq -r '[.access[]?.reason] | unique | join(",")' <<<"$acc")"
pol=$(curl -fsS "$UI/api/policies" 2>/dev/null) || pol='{}'
expect "dashboard: the spoke policy of $S1 is expired" true \
  "$(jq -r --arg s "$S1" '[.policies[]? | select(.origin.cluster==$s) | .expired] | first // "absent"' <<<"$pol")"
call "$BOB" /prom/api/v1/query --data-urlencode 'query=demo_requests_total'
expect "bob: $S2 is not affected" "$S2" "$(prom_vals cluster)"
call "$SAM" /prom/api/v1/query --data-urlencode 'query=count by (cluster) (demo_requests_total)'
expect "sam (sre): the hub ClusterAccessPolicy still grants $S1 (hub policies do not expire)" "$BOTH_SPOKES" "$(prom_vals cluster)"
expired_metric() {
  call "$CAROL" /prom/api/v1/query --data-urlencode "query=max(querdian_cluster_policy_expired{exported_cluster=\"$S1\"})"
  [ "$(jq -r '.data.result[0].value[1] // ""' <<<"$BODY")" = 1 ]
}
for _ in $(seq 1 20); do expired_metric && break; sleep 2; done
expect "metric querdian_cluster_policy_expired for $S1 (scraped as exported_cluster)" 1 "$(jq -r '.data.result[0].value[1] // ""' <<<"$BODY")"

section "unsuspend $S1: the next valid sync restores the grants"
k patch spokecluster "$S1" --type merge -p '{"spec":{"suspended":false}}' >/dev/null
SUSPENDED=""
T_UN=$SECONDS
for _ in $(seq 1 60); do alice_status 200 && break; sleep 2; done
expect "alice: access restored after the unsuspend ($((SECONDS - T_UN))s)" 200 "$STATUS"
expect "alice: clusters" "$S1" "$(prom_vals cluster)"

# --- Grafana ----------------------------------------------------------------------------------------------------
section "Grafana"
G="http://localhost:$GRAFANA_PORT"
expect "grafana health" ok "$(curl -fsS "$G/api/health" | jq -r .database)"
ds=$(curl -fsS -u admin:admin "$G/api/datasources")
for u in prometheus:prom loki:loki tempo:tempo; do
  expect "datasource ${u%%:*}: url" "http://querdian.querdian:8080/${u##*:}" "$(jq -r --arg u "${u%%:*}" '.[]|select(.uid==$u).url' <<<"$ds")"
done

echo
echo "Summary: $PASS passed, $FAILN failed"
[ "$FAILN" = 0 ]
