#!/usr/bin/env bash
# Multi-cluster quickstart: three kind clusters on one laptop. DEMO ONLY (dev-mode Keycloak, known
# passwords, all CA keys on the laptop). See docs/quickstart-multicluster.md.
#
#  * hub (querdian-hub): Keycloak, Prometheus, Loki, Tempo, Grafana, the hub collector, a demo ingest
#    gateway, two sample workloads, Querdian with the multicluster block, and the receiver.
#  * two spokes (querdian-spoke-a, querdian-spoke-b): the Querdian agent only, the same two sample
#    workloads in the same namespaces, and a collector that sends to the gateway of the hub.
#
# The setup follows the ordered migration steps of docs/multicluster.md: hub telemetry carries
# cluster="hub" (step 2), the multicluster block (step 3), the receiver and the spokes registered with
# suspended: true (step 4), the forged-label test of the gateway while suspended (step 5), and only then
# suspended: false (step 6).
#
#   hack/quickstart-multicluster/up.sh
#   SKIP_BUILD=1 hack/quickstart-multicluster/up.sh          # reuse an already built image (IMAGE_TAG)
#   CLEANUP_ON_FAILURE=1 hack/quickstart-multicluster/up.sh  # delete the clusters if the script fails
#
# Requires: kind, kubectl, helm, docker (or podman with a docker shim), make, curl, jq, openssl.
set -Eeuo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

SKIP_BUILD="${SKIP_BUILD:-0}"
CLEANUP_ON_FAILURE="${CLEANUP_ON_FAILURE:-0}"
WORK="$(mktemp -d)"
T0=$SECONDS
BUILD_PID=""

on_exit() {
  local rc=$? c
  set +e
  pf_stop
  [ -z "$BUILD_PID" ] || kill "$BUILD_PID" 2>/dev/null
  rm -rf "$WORK"
  if [ $rc -ne 0 ]; then
    echo "--- up.sh failed (exit $rc) ---" >&2
    for c in $(all_clusters); do
      kind get clusters 2>/dev/null | grep -qx "$c" || continue
      kubectl --context "kind-$c" get pods -A --no-headers 2>/dev/null | grep -v -E 'Running|Completed' | sed "s/^/$c: /" >&2
    done
    if [ "$CLEANUP_ON_FAILURE" = 1 ]; then
      "$MC_DIR/down.sh" >&2
    else
      echo "The clusters were kept for debugging; remove them with hack/quickstart-multicluster/down.sh" >&2
    fi
  fi
  exit $rc
}
trap on_exit EXIT

need kind kubectl helm docker make curl jq openssl

for p in "$KEYCLOAK_PORT" "$GRAFANA_PORT" "$UI_PORT"; do
  if port_open "$p"; then
    die "host port $p is already in use (is the single-cluster quickstart running? KEYCLOAK_PORT=... changes Keycloak's port)"
  fi
done
for c in $(all_clusters); do
  if kind get clusters 2>/dev/null | grep -qx "$c"; then
    die "kind cluster '$c' already exists; run hack/quickstart-multicluster/down.sh first (or choose CLUSTER_PREFIX=...)"
  fi
done

# The spoke that each spoke claims to be in its collector (hostile on purpose, see
# manifests/spoke-collector.yaml): the first spoke claims the hub, each other spoke claims the first.
claim_of() { if [ "$1" = "${SPOKES[0]}" ]; then echo "$HUB_NAME"; else echo "${SPOKES[0]}"; fi; }
team_of() { echo "team-${1#spoke-}"; } # spoke-a -> team-a
spoke_index() { local i=0 s; for s in "${SPOKES[@]}"; do i=$((i + 1)); [ "$s" = "$1" ] && echo "$i"; done; }
nodeport() { k -n "$1" get svc "$2" -o jsonpath="{.spec.ports[?(@.name==\"$3\")].nodePort}"; }

# --- 1. image (built in the background while the clusters start) ------------------------------------------
if [ "$SKIP_BUILD" != 1 ]; then
  step "building $IMAGE_REPO:$IMAGE_TAG (background)"
  (cd "$ROOT" && make docker-build VERSION="$IMAGE_TAG") >"$WORK/build.log" 2>&1 &
  BUILD_PID=$!
fi

# --- 2. kind clusters -------------------------------------------------------------------------------------------
# Each cluster has its own pod and Service ranges, so that addresses in logs and traces are unique. The
# clusters share the Docker network "kind"; the spokes reach the NodePorts of the hub node on it.
step "kind cluster $CLUSTER (hub)"
cat >"$WORK/kind-hub.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking: {podSubnet: 10.244.0.0/16, serviceSubnet: 10.96.0.0/16}
nodes:
  - role: control-plane
    extraPortMappings:
      # Keycloak: host port == in-cluster Service port, so the issuer URL is the same everywhere.
      - {containerPort: 30080, hostPort: $KEYCLOAK_PORT, listenAddress: "127.0.0.1", protocol: TCP}
      # Querdian policy dashboard
      - {containerPort: 30081, hostPort: $UI_PORT, listenAddress: "127.0.0.1", protocol: TCP}
      # Grafana
      - {containerPort: 30030, hostPort: $GRAFANA_PORT, listenAddress: "127.0.0.1", protocol: TCP}
YAML
T_CLUSTER=$SECONDS
kind create cluster --name "$CLUSTER" --config "$WORK/kind-hub.yaml" --wait 120s
for s in "${SPOKES[@]}"; do
  i=$(spoke_index "$s")
  step "kind cluster $(spoke_cluster "$s") (spoke $s)"
  cat >"$WORK/kind-$s.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking: {podSubnet: 10.$((244 + i)).0.0/16, serviceSubnet: 10.$((96 + i)).0.0/16}
nodes:
  - role: control-plane
YAML
  kind create cluster --name "$(spoke_cluster "$s")" --config "$WORK/kind-$s.yaml" --wait 120s
done
info "clusters created in $((SECONDS - T_CLUSTER))s"
kubectl config use-context "$KCTX" >/dev/null

step "CoreDNS rewrite $KC_HOST -> keycloak.keycloak.svc (hub)"
coredns_rewrite "$KCTX" "$WORK"

HUB_IP="$(hub_ip)"
[ -n "$HUB_IP" ] || die "cannot find the address of the hub node"
info "hub node address on the Docker network kind: $HUB_IP"

# --- 3. certs ------------------------------------------------------------------------------------------------------
step "certs (openssl) in $CERT_DIR"
mkdir -p "$MC_WORK_DIR"
rm -rf "$CERT_DIR"
"$MC_DIR/certs.sh" "$CERT_DIR" "$HUB_IP" "$TRUST_DOMAIN" "${SPOKES[@]}" | sed 's/^/    /'

# --- 4. image into the clusters --------------------------------------------------------------------------------
if [ -n "$BUILD_PID" ]; then
  step "waiting for the image build"
  if ! wait "$BUILD_PID"; then BUILD_PID=""; tail -30 "$WORK/build.log" >&2; die "image build failed"; fi
  BUILD_PID=""
fi
docker image inspect "$IMAGE_REPO:$IMAGE_TAG" >/dev/null 2>&1 || die "image $IMAGE_REPO:$IMAGE_TAG not found"
step "loading $IMAGE_REPO:$IMAGE_TAG into the three clusters"
docker save -o "$WORK/image.tar" "$IMAGE_REPO:$IMAGE_TAG"
for c in $(all_clusters); do kind load image-archive "$WORK/image.tar" --name "$c"; done
rm -f "$WORK/image.tar"

# --- 5. hub: the stack of the single-cluster quickstart --------------------------------------------------------
step "hub: namespaces, Keycloak, backends, collector, Grafana, sample workloads"
render "$QS_DIR/manifests/00-base.yaml" | k apply -f - >/dev/null
# The realm of the single-cluster quickstart with the users and groups of this demo.
jq --slurpfile u "$MC_DIR/realm-users.json" '.groups = $u[0].groups | .users = $u[0].users' \
  "$QS_DIR/realm-observability.json" >"$WORK/realm-observability.json"
k -n keycloak create cm keycloak-realm --from-file=realm-observability.json="$WORK/realm-observability.json"
for ns in order-service payment-service; do
  k -n "$ns" create cm workload --from-file=workload.sh="$QS_DIR/workload.sh"
done
dash_args=()
for f in "$QS_DIR"/dashboards/*.json; do dash_args+=(--from-file="$f"); done
k -n observability create cm grafana-dashboards "${dash_args[@]}"
for f in keycloak loki tempo grafana workloads; do
  render "$QS_DIR/manifests/$f.yaml" | k apply -f - >/dev/null
done
subst "$MC_DIR/manifests/hub-prometheus.yaml" HUB_NAME="$HUB_NAME" | k apply -f - >/dev/null
subst "$MC_DIR/manifests/hub-otel-collector.yaml" HUB_NAME="$HUB_NAME" TENANT="$TENANT" | k apply -f - >/dev/null
for d in prometheus loki tempo; do k -n observability rollout status "deploy/$d" --timeout=300s; done
k -n observability rollout status ds/otel-collector --timeout=300s

# --- 6. migration step 2: hub telemetry carries cluster="hub" -------------------------------------------------
# Before the first spoke sends data, the direct queries of docs/multicluster.md (step 2) must find no
# data without the hub cluster label.
step "migration step 2: hub telemetry carries cluster=\"$HUB_NAME\" (direct backend queries)"
backends_pf
n_prom() { prom_direct "$1" | jq -r '.data.result | length'; }
n_loki() { loki_direct "$1" | jq -r '.data.result | length'; }
n_tempo() { tempo_direct "$1" 600 | jq -r '.traces | length'; }
gt0() { [ "$("$@")" -gt 0 ] 2>/dev/null; }
wait_for "hub metrics with cluster=$HUB_NAME" 180 gt0 n_prom "count(demo_requests_total{cluster=\"$HUB_NAME\"})"
wait_for "hub logs with k8s_cluster_name=$HUB_NAME" 180 gt0 n_loki "sum(count_over_time({k8s_namespace_name=~\".+\", k8s_cluster_name=\"$HUB_NAME\"}[10m]))"
wait_for "hub traces with resource.k8s.cluster.name=$HUB_NAME" 180 gt0 n_tempo "{ resource.k8s.cluster.name = \"$HUB_NAME\" }"
[ "$(n_prom "count by (__name__) (last_over_time({__name__=~\".+\", cluster!=\"$HUB_NAME\"}[1h]))")" = 0 ] ||
  die "metrics: hub series without cluster=$HUB_NAME exist"
for sel in k8s_namespace_name service_name; do
  [ "$(n_loki "sum by ($sel) (count_over_time({$sel=~\".+\", k8s_cluster_name!=\"$HUB_NAME\"}[1h]))")" = 0 ] ||
    die "logs: hub streams without k8s_cluster_name=$HUB_NAME exist (selector $sel)"
done
all_t=$(n_tempo '{}')
hub_t=$(n_tempo "{ resource.k8s.cluster.name = \"$HUB_NAME\" }")
[ "$all_t" = "$hub_t" ] || die "traces: $all_t traces in 10 minutes, but only $hub_t with resource.k8s.cluster.name=$HUB_NAME"
pf_stop
ok "metrics, logs and traces of the hub carry the hub cluster label ($hub_t traces in 10 minutes)"

# --- 7. the ingest gateway (before a spoke connects) -----------------------------------------------------------
step "hub: demo ingest gateway (one mTLS OTLP port and one ingest CA for each spoke)"
gw_args=(--from-file=tls.crt="$CERT_DIR/gateway.crt" --from-file=tls.key="$CERT_DIR/gateway.key")
for s in "${SPOKES[@]}"; do gw_args+=(--from-file="ca-$s.crt=$CERT_DIR/ingest-ca-$s.crt"); done
k -n observability create secret generic ingest-gateway-tls "${gw_args[@]}"
subst "$MC_DIR/manifests/ingest-gateway.yaml" TENANT="$TENANT" | k apply -f - >/dev/null
k -n observability rollout status deploy/ingest-gateway --timeout=300s

step "waiting for Keycloak (the slowest part), Grafana and the sample workloads"
k -n keycloak rollout status deploy/keycloak --timeout=480s
k -n observability rollout status deploy/grafana --timeout=300s
for ns in order-service payment-service; do k -n "$ns" rollout status "deploy/$ns" --timeout=300s; done

# --- 8. Querdian on the hub: migration steps 0, 1, 3 and 4 ------------------------------------------------------
# This hub is new: it has no old policies and no old data, so the chart installs the CRDs and the
# multicluster block at once. The hub ClusterAccessPolicy objects state their cluster scope (step 1).
step "hub: helm install querdian (multicluster block, receiver)"
k -n querdian create secret tls querdian-receiver-tls --cert="$CERT_DIR/receiver.crt" --key="$CERT_DIR/receiver.key"
k -n querdian create secret generic querdian-agent-ca --from-file=ca.crt="$CERT_DIR/agent-ca.crt"
render "$QS_DIR/querdian-values.yaml" >"$WORK/values.yaml"
h upgrade --install querdian "$ROOT/charts/querdian" -n querdian -f "$WORK/values.yaml" -f "$MC_DIR/hub-values.yaml" \
  --wait --timeout 4m
RECEIVER_PORT="$(nodeport querdian querdian-receiver receiver)"
[ -n "$RECEIVER_PORT" ] || die "the receiver Service has no nodePort"
RECEIVER_URL="https://$HUB_IP:$RECEIVER_PORT"
info "receiver: $RECEIVER_URL"

step "hub policies"
k apply -f "$MC_DIR/manifests/hub-policies.yaml"
hub_policies_ready() {
  [ "$(k get accesspolicies,clusteraccesspolicies -A \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c '^True$')" = 3 ]
}
wait_for "3 hub policies Ready (none InvalidClusterScope)" 120 hub_policies_ready
k get accesspolicies,clusteraccesspolicies -A

step "migration step 4: register the spokes with suspended: true"
for s in "${SPOKES[@]}"; do
  subst "$MC_DIR/manifests/spokecluster.yaml" SPOKE="$s" IDENTITY="$(agent_identity "$s")" TENANT="$TENANT" | k apply -f -
done

# --- 9. spokes: agent, policy, workloads, collector --------------------------------------------------------------
for s in "${SPOKES[@]}"; do
  step "spoke $s: agent (chart mode spoke), AccessPolicy for $(team_of "$s"), workloads, collector"
  for ns in observability querdian order-service payment-service; do ks "$s" create ns "$ns" >/dev/null; done
  ks "$s" label ns order-service payment-service "policy.querdian.dev/tenant=$TENANT" >/dev/null
  ks "$s" -n querdian create secret generic querdian-agent-tls --type=kubernetes.io/tls \
    --from-file=tls.crt="$CERT_DIR/agent-$s.crt" --from-file=tls.key="$CERT_DIR/agent-$s.key" \
    --from-file=ca.crt="$CERT_DIR/hub-server-ca.crt"
  subst "$MC_DIR/spoke-values.yaml" RECEIVER_URL="$RECEIVER_URL" >"$WORK/values-$s.yaml"
  hs "$s" upgrade --install querdian "$ROOT/charts/querdian" -n querdian -f "$WORK/values-$s.yaml" --wait --timeout 3m
  subst "$MC_DIR/manifests/spoke-policies.yaml" TEAM="$(team_of "$s")" SPOKE="$s" | ks "$s" apply -f -
  for ns in order-service payment-service; do
    ks "$s" -n "$ns" create cm workload --from-file=workload.sh="$QS_DIR/workload.sh" >/dev/null
  done
  render "$QS_DIR/manifests/workloads.yaml" | ks "$s" apply -f - >/dev/null
  # Migration step 5: the spoke connects to the gateway only now, after the multicluster block (step 3).
  ks "$s" -n observability create secret generic ingest-tls \
    --from-file=tls.crt="$CERT_DIR/ingest-$s.crt" --from-file=tls.key="$CERT_DIR/ingest-$s.key" \
    --from-file=ca.crt="$CERT_DIR/hub-server-ca.crt" >/dev/null
  gw_port="$(nodeport observability ingest-gateway "$s")"
  [ -n "$gw_port" ] || die "the ingest gateway Service has no port for $s"
  subst "$MC_DIR/manifests/spoke-collector.yaml" CLAIM="$(claim_of "$s")" GATEWAY_URL="https://$HUB_IP:$gw_port" |
    ks "$s" apply -f - >/dev/null
done
for s in "${SPOKES[@]}"; do
  ks "$s" -n observability rollout status ds/otel-collector --timeout=300s
  for ns in order-service payment-service; do ks "$s" -n "$ns" rollout status "deploy/$ns" --timeout=300s; done
  [ -z "$(k get spokecluster "$s" -o jsonpath='{.status.lastSyncTime}')" ] ||
    die "spoke $s synced while it is suspended; the receiver must reject its syncs"
  info "$s: suspended, the receiver rejects its syncs (no lastSyncTime)"
done

# --- 10. migration step 5: the forged-label test, while the spokes are suspended ---------------------------------
for s in "${SPOKES[@]}"; do
  step "migration step 5: forged-label test of the gateway for $s (suspended)"
  "$MC_DIR/forged-label-test.sh" "$s" || die "the forged-label test of $s failed; $s stays suspended"
done

# --- 11. migration step 6: unsuspend each tested spoke -----------------------------------------------------------
synced() { # spoke: a valid sync of the current generation
  [ -n "$(k get spokecluster "$1" -o jsonpath='{.status.lastSyncTime}')" ] &&
    [ "$(k get spokecluster "$1" -o jsonpath='{.status.observedGeneration}')" = "$(k get spokecluster "$1" -o jsonpath='{.metadata.generation}')" ]
}
spoke_policy_ready() { # spoke: the agent wrote the Ready condition from the sync reply
  [ "$(ks "$1" -n order-service get accesspolicy "$(team_of "$1")" -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ]
}
for s in "${SPOKES[@]}"; do
  step "migration step 6: unsuspend $s"
  k patch spokecluster "$s" --type merge -p '{"spec":{"suspended":false}}' >/dev/null
  wait_for "first valid sync of $s" 120 synced "$s"
  wait_for "the AccessPolicy of $s is Ready in the spoke" 120 spoke_policy_ready "$s"
done
k get spokeclusters
k get spokepolicies

step "waiting for the data of all clusters through Querdian"
wait_for "telemetry of the hub and the spokes" 300 "$MC_DIR/verify.sh" --smoke

cat <<EOF

=== Querdian multi-cluster quickstart is up after $((SECONDS - T0))s
  Clusters  $(all_clusters | paste -sd, - | sed 's/,/, /g')   (hub node $HUB_IP, receiver port $RECEIVER_PORT)
  Grafana   http://localhost:$GRAFANA_PORT    log in with Keycloak:
              alice / alice-demo (team-a: spoke-a)   bob / bob-demo (team-b: spoke-b)
              hana / hana-demo (team-hub: hub)       sam / sam-demo (sre: both spokes)
              carol / carol-demo (platform-admins: all)
  Querdian  http://localhost:$UI_PORT    policy dashboard: origin of each policy, expired state
  Keycloak  http://$KC_HOST:$KEYCLOAK_PORT   admin / admin (demo only)
  Certs     $CERT_DIR
  Verify    hack/quickstart-multicluster/verify.sh
  Remove    hack/quickstart-multicluster/down.sh
EOF
