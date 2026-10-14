#!/usr/bin/env bash
# Querdian quickstart: builds a single-node kind cluster with Keycloak, Prometheus, Loki, Tempo,
# an OpenTelemetry Collector, Grafana, two sample workloads and Querdian, and applies example
# policies. DEMO ONLY (dev-mode Keycloak, plain HTTP, known passwords). See docs/quickstart.md.
#
#   hack/quickstart/up.sh
#   CLUSTER=foo KEYCLOAK_PORT=8081 hack/quickstart/up.sh
#   SKIP_BUILD=1 hack/quickstart/up.sh            # reuse an already built image (IMAGE_TAG)
#   QUICKSTART_NETWORKPOLICY=1 hack/quickstart/up.sh   # also enable NetworkPolicies
#   CLEANUP_ON_FAILURE=1 hack/quickstart/up.sh    # delete the cluster if the script fails
#
# Requires: kind, kubectl, helm, docker (or podman with a docker shim), make, curl.
set -Eeuo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

SKIP_BUILD="${SKIP_BUILD:-0}"
QUICKSTART_NETWORKPOLICY="${QUICKSTART_NETWORKPOLICY:-0}"
CLEANUP_ON_FAILURE="${CLEANUP_ON_FAILURE:-0}"
WORK="$(mktemp -d)"
T0=$SECONDS
BUILD_PID=""

on_exit() {
  local rc=$?
  set +e
  [ -z "$BUILD_PID" ] || kill "$BUILD_PID" 2>/dev/null
  rm -rf "$WORK"
  if [ $rc -ne 0 ]; then
    echo "--- up.sh failed (exit $rc) ---" >&2
    if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
      k get pods -A --no-headers 2>/dev/null | grep -v -E 'Running|Completed' >&2
      if [ "$CLEANUP_ON_FAILURE" = 1 ]; then
        "$QS_DIR/down.sh" >&2
      else
        echo "The cluster was kept for debugging; remove it with hack/quickstart/down.sh" >&2
      fi
    fi
  fi
  exit $rc
}
trap on_exit EXIT

wait_for() { # description, timeout-seconds, command...
  local d="$1" t="$2" i=0
  shift 2
  until "$@" >/dev/null 2>&1; do
    i=$((i + 2))
    [ $i -le "$t" ] || die "timeout waiting for: $d"
    sleep 2
  done
}

need kind kubectl helm docker make curl

for p in "$KEYCLOAK_PORT" 3000; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$p") 2>/dev/null; then
    die "host port $p is already in use (set KEYCLOAK_PORT=... for Keycloak; Grafana needs 3000)"
  fi
done
if kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
  die "kind cluster '$CLUSTER' already exists; run hack/quickstart/down.sh first (or choose CLUSTER=...)"
fi

# --- 1. image (built in the background while the cluster starts) --------------------------
if [ "$SKIP_BUILD" != 1 ]; then
  step "building $IMAGE_REPO:$IMAGE_TAG (background)"
  (cd "$ROOT" && make docker-build VERSION="$IMAGE_TAG" >"$WORK/build.log" 2>&1) &
  BUILD_PID=$!
fi

# --- 2. kind cluster ------------------------------------------------------------------------
step "kind cluster $CLUSTER"
cat >"$WORK/kind.yaml" <<YAML
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
nodes:
  - role: control-plane
    extraPortMappings:
      # Keycloak: host port == in-cluster Service port, so the issuer URL is the same everywhere.
      - {containerPort: 30080, hostPort: $KEYCLOAK_PORT, listenAddress: "127.0.0.1", protocol: TCP}
      # Grafana
      - {containerPort: 30030, hostPort: $GRAFANA_PORT, listenAddress: "127.0.0.1", protocol: TCP}
YAML
T_CLUSTER=$SECONDS
kind create cluster --name "$CLUSTER" --config "$WORK/kind.yaml" --wait 120s
info "cluster created in $((SECONDS - T_CLUSTER))s"

# --- 3. CoreDNS: make the Keycloak host name resolve inside the cluster ---------------------
step "CoreDNS rewrite $KC_HOST -> keycloak.keycloak.svc"
k -n kube-system get cm coredns -o jsonpath='{.data.Corefile}' >"$WORK/Corefile"
sed -i.bak "s/^\( *\)kubernetes /\1rewrite name $KC_HOST keycloak.keycloak.svc.cluster.local\n&/" "$WORK/Corefile"
grep -q "rewrite name $KC_HOST" "$WORK/Corefile" || die "could not patch the CoreDNS Corefile"
k -n kube-system create cm coredns --from-file=Corefile="$WORK/Corefile" --dry-run=client -o yaml | k replace -f - >/dev/null
k -n kube-system rollout restart deploy/coredns >/dev/null

# --- 4. image into the cluster ---------------------------------------------------------------
if [ "$SKIP_BUILD" != 1 ]; then
  step "waiting for the image build"
  if ! wait "$BUILD_PID"; then BUILD_PID=""; tail -30 "$WORK/build.log" >&2; die "image build failed"; fi
  BUILD_PID=""
fi
docker image inspect "$IMAGE_REPO:$IMAGE_TAG" >/dev/null 2>&1 || die "image $IMAGE_REPO:$IMAGE_TAG not found"
step "loading the image into kind"
docker save -o "$WORK/querdian.tar" "$IMAGE_REPO:$IMAGE_TAG"
kind load image-archive "$WORK/querdian.tar" --name "$CLUSTER"
rm -f "$WORK/querdian.tar"

# --- 5. the stack ------------------------------------------------------------------------------
step "namespaces, Keycloak, backends, collector, Grafana, sample workloads"
render "$QS_DIR/manifests/00-base.yaml" | k apply -f - >/dev/null
k -n keycloak create cm keycloak-realm --from-file=realm-observability.json="$QS_DIR/realm-observability.json"
for ns in order-service payment-service; do
  k -n "$ns" create cm workload --from-file=workload.sh="$QS_DIR/workload.sh"
done
for f in keycloak prometheus loki tempo otel-collector grafana workloads; do
  render "$QS_DIR/manifests/$f.yaml" | k apply -f - >/dev/null
done

step "waiting for Keycloak (the slowest part)"
k -n keycloak rollout status deploy/keycloak --timeout=480s
for d in prometheus loki tempo grafana; do k -n observability rollout status "deploy/$d" --timeout=300s; done
k -n observability rollout status ds/otel-collector --timeout=300s
k -n order-service rollout status deploy/order-service --timeout=300s
k -n payment-service rollout status deploy/payment-service --timeout=300s

# --- 6. Querdian ---------------------------------------------------------------------------------
step "helm install querdian"
render "$QS_DIR/querdian-values.yaml" >"$WORK/values.yaml"
args=(-f "$WORK/values.yaml")
if [ "$QUICKSTART_NETWORKPOLICY" = 1 ]; then args+=(-f "$QS_DIR/querdian-networkpolicy-values.yaml"); fi
h upgrade --install querdian "$ROOT/charts/querdian" -n querdian "${args[@]}" --wait --timeout 4m

step "example policies"
k apply -f "$QS_DIR/manifests/policies.yaml"
policies_ready() {
  [ "$(k get accesspolicies,clusteraccesspolicies -A \
    -o jsonpath='{range .items[*]}{.status.conditions[?(@.type=="Ready")].status}{"\n"}{end}' | grep -c '^True$')" = 3 ]
}
wait_for "3 policies Ready" 120 policies_ready
k get accesspolicies,clusteraccesspolicies -A

step "waiting for telemetry to arrive (first scrape, logs, traces)"
telemetry_ready() { "$QS_DIR/verify.sh" --smoke; }
wait_for "telemetry in the backends" 240 telemetry_ready

cat <<EOF

=== Querdian quickstart is up after $((SECONDS - T0))s
  Grafana   http://localhost:$GRAFANA_PORT      log in with Keycloak: alice / alice-demo, bob / bob-demo, carol / carol-demo
  Keycloak  http://$KC_HOST:$KEYCLOAK_PORT   admin / admin (demo only)
  Verify    hack/quickstart/verify.sh
  Remove    hack/quickstart/down.sh
EOF
