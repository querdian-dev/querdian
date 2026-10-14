#!/usr/bin/env bash
# Verifies charts/querdian on a throw-away kind cluster (T5.1 acceptance criteria).
# Not part of CI. Rerunnable: an old cluster of the same name is deleted first and everything
# this script creates is removed on exit (cluster, built image, pulled helper image unless it
# existed before).
#
#   hack/kind/verify-chart.sh                       # CLUSTER=querdian-t51 by default
#   CLUSTER=foo KEEP=1 hack/kind/verify-chart.sh    # keep the cluster for debugging
#
# Requires: kind, helm, kubectl, docker (or podman with a docker shim), a CNI that enforces
# NetworkPolicy (kindnet does in recent kind versions; otherwise that part is skipped and reported).
# When `docker` is a podman shim the script sets KIND_EXPERIMENTAL_PROVIDER=podman.
set -Eeuo pipefail

CLUSTER="${CLUSTER:-querdian-t51}"
NS="${NS:-querdian}"
BACKEND_NS="${BACKEND_NS:-backends}"
CLIENT_NS="${CLIENT_NS:-clients}"
KEEP="${KEEP:-0}"
VERSION="${VERSION:-kind-verify}"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/querdian-dev/querdian}"
HELPER_IMAGE="${HELPER_IMAGE:-docker.io/library/busybox:1.37}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
CHART="$ROOT/charts/querdian"
WORK="$(mktemp -d)"
KCTX="kind-$CLUSTER"

if docker --version 2>/dev/null | grep -qi podman; then export KIND_EXPERIMENTAL_PROVIDER=podman; fi

step() { printf '\n=== %s\n' "$*"; }
ok() { printf 'PASS: %s\n' "$*"; }
fail() { printf 'FAIL: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$KCTX" "$@"; }
h() { helm --kube-context "$KCTX" "$@"; }

helper_preexisting=0
docker image inspect "$HELPER_IMAGE" >/dev/null 2>&1 && helper_preexisting=1

cleanup() {
  local rc=$?
  set +e
  if [ $rc -ne 0 ] && kind get clusters 2>/dev/null | grep -qx "$CLUSTER"; then
    echo "--- diagnostics (script failed) ---" >&2
    k -n "$NS" get pods,lease -o wide >&2
    k -n "$NS" logs -l app.kubernetes.io/name=querdian --tail=30 >&2
  fi
  if [ "$KEEP" != 1 ]; then
    kind delete cluster --name "$CLUSTER" >/dev/null 2>&1
    docker rmi -f "$IMAGE_REPO:$VERSION" >/dev/null 2>&1
    [ "$helper_preexisting" = 1 ] || docker rmi -f "$HELPER_IMAGE" >/dev/null 2>&1
  fi
  rm -rf "$WORK"
  exit $rc
}
trap cleanup EXIT

wait_for() { # description, timeout-seconds, command...
  local d="$1" t="$2" i=0
  shift 2
  until "$@" >/dev/null 2>&1; do
    i=$((i + 2))
    [ $i -le "$t" ] || fail "timeout waiting for: $d"
    sleep 2
  done
}

phase_done() { # namespace pod
  case "$(k -n "$1" get pod "$2" -o jsonpath='{.status.phase}')" in Succeeded | Failed) return 0 ;; *) return 1 ;; esac
}

# Runs a one-shot busybox pod and prints its exit code. Args: namespace name labels command...
run_probe() {
  local ns="$1" name="$2" labels="$3"
  shift 3
  k -n "$ns" delete pod "$name" --ignore-not-found --wait=true >/dev/null
  k -n "$ns" run "$name" --image="$HELPER_IMAGE" --image-pull-policy=IfNotPresent --restart=Never \
    ${labels:+--labels="$labels"} --command -- "$@" >/dev/null
  wait_for "$name finished" 90 phase_done "$ns" "$name"
  k -n "$ns" get pod "$name" -o jsonpath='{.status.containerStatuses[0].state.terminated.exitCode}'
  k -n "$ns" delete pod "$name" --wait=false >/dev/null
}
# TCP connect check (any HTTP answer counts as reachable).
# The pod waits first: the CNI programs a new pod's policies a moment after it starts, and a probe
# in that window would be (wrongly) admitted.
tcp() { run_probe "$1" "$2" "${5:-}" sh -c "sleep 5; nc -z -w 5 $3 $4"; }
expect_reachable() { [ "$(tcp "$@")" = 0 ] || fail "expected reachable: $2 -> $3:$4"; ok "reachable: $2 -> $3:$4"; }
expect_blocked() { [ "$(tcp "$@")" != 0 ] || fail "expected BLOCKED: $2 -> $3:$4"; ok "blocked: $2 -> $3:$4"; }

BACKEND_HOST="dummy-backend.$BACKEND_NS"
PROXY_HOST="querdian.$NS.svc"
QUERDIAN_LABELS="app.kubernetes.io/name=querdian,app.kubernetes.io/instance=querdian"

step "cluster $CLUSTER"
kind delete cluster --name "$CLUSTER" >/dev/null 2>&1 || true
kind create cluster --name "$CLUSTER" --wait 120s

step "image $IMAGE_REPO:$VERSION"
(cd "$ROOT" && make docker-build VERSION="$VERSION")
docker image inspect "$HELPER_IMAGE" >/dev/null 2>&1 || docker pull "$HELPER_IMAGE"
# The archive route works with docker and podman alike.
docker save -o "$WORK/querdian.tar" "$IMAGE_REPO:$VERSION"
docker save -o "$WORK/helper.tar" "$HELPER_IMAGE"
kind load image-archive "$WORK/querdian.tar" --name "$CLUSTER"
kind load image-archive "$WORK/helper.tar" --name "$CLUSTER"
rm -f "$WORK"/*.tar

step "namespaces, stub OIDC issuer, dummy backend"
for n in "$NS" "$BACKEND_NS" "$CLIENT_NS"; do k create namespace "$n"; done
k label namespace "$CLIENT_NS" policy.querdian.dev/tenant=tenant-clients
# Stub issuer: issuer == its own in-cluster URL; the JWKS is valid but empty.
k -n "$NS" apply -f - <<YAML
apiVersion: v1
kind: ConfigMap
metadata: {name: oidc-stub}
data:
  openid-configuration: |
    {"issuer":"http://oidc-stub.$NS.svc:8080","jwks_uri":"http://oidc-stub.$NS.svc:8080/jwks.json","id_token_signing_alg_values_supported":["RS256"],"subject_types_supported":["public"],"response_types_supported":["id_token"]}
  jwks.json: '{"keys":[]}'
---
apiVersion: apps/v1
kind: Deployment
metadata: {name: oidc-stub}
spec:
  replicas: 1
  selector: {matchLabels: {app: oidc-stub}}
  template:
    metadata: {labels: {app: oidc-stub}}
    spec:
      containers:
        - name: httpd
          image: $HELPER_IMAGE
          imagePullPolicy: IfNotPresent
          command: [sh, -c, "mkdir -p /www/.well-known && cp /cfg/openid-configuration /www/.well-known/ && cp /cfg/jwks.json /www/ && exec httpd -f -p 8080 -h /www"]
          volumeMounts: [{name: cfg, mountPath: /cfg}]
      volumes: [{name: cfg, configMap: {name: oidc-stub}}]
---
apiVersion: v1
kind: Service
metadata: {name: oidc-stub}
spec:
  selector: {app: oidc-stub}
  ports: [{port: 8080, targetPort: 8080}]
YAML
# Dummy backend: any HTTP server in another namespace.
k -n "$BACKEND_NS" apply -f - <<YAML
apiVersion: apps/v1
kind: Deployment
metadata: {name: dummy-backend}
spec:
  replicas: 1
  selector: {matchLabels: {app: dummy-backend}}
  template:
    metadata: {labels: {app: dummy-backend}}
    spec:
      containers:
        - name: httpd
          image: $HELPER_IMAGE
          imagePullPolicy: IfNotPresent
          command: [sh, -c, "mkdir -p /www && echo ok > /www/index.html && exec httpd -f -p 3100 -h /www"]
---
apiVersion: v1
kind: Service
metadata: {name: dummy-backend}
spec:
  selector: {app: dummy-backend}
  ports: [{port: 3100, targetPort: 3100}]
YAML
k -n "$NS" rollout status deploy/oidc-stub --timeout=120s
k -n "$BACKEND_NS" rollout status deploy/dummy-backend --timeout=120s

cat >"$WORK/values.yaml" <<YAML
image: {repository: $IMAGE_REPO, tag: $VERSION, pullPolicy: Never}
config:
  auth:
    issuer: http://oidc-stub.$NS.svc:8080
    audiences: [querdian]
  backends:
    - {name: metrics, type: prometheus, url: "http://$BACKEND_HOST:3100", pathPrefix: /prometheus, namespaceLabel: namespace, tenantFederation: none}
    - {name: logs, type: loki, url: "http://$BACKEND_HOST:3100", pathPrefix: /loki, namespaceLabel: k8s_namespace_name, tenantHeader: X-Scope-OrgID, tenantFederation: tenantIdLabel}
    - {name: traces, type: tempo, url: "http://$BACKEND_HOST:3100", pathPrefix: /tempo, namespaceLabel: resource.k8s.namespace.name, tenantHeader: X-Scope-OrgID, tenantFederation: none, traceByID: filter}
resources: {requests: {cpu: 50m, memory: 64Mi}}
YAML

step "helm install (CRDs + chart)"
h install querdian "$CHART" -n "$NS" -f "$WORK/values.yaml" --wait --timeout 3m
k wait --for=condition=Ready pod -l app.kubernetes.io/name=querdian -n "$NS" --timeout=120s
[ "$(k -n "$NS" get deploy querdian -o jsonpath='{.status.readyReplicas}')" = 2 ] || fail "expected 2 ready replicas"
ok "2/2 replicas Ready (/readyz green)"
k get crd accesspolicies.policy.querdian.dev clusteraccesspolicies.policy.querdian.dev >/dev/null
ok "CRDs installed"

step "leader election lease"
lease_holder() { k -n "$NS" get lease querdian-leader-election -o jsonpath='{.spec.holderIdentity}'; }
wait_for "lease holder" 60 sh -c "[ -n \"\$(kubectl --context $KCTX -n $NS get lease querdian-leader-election -o jsonpath='{.spec.holderIdentity}')\" ]"
ok "lease querdian-leader-election held by $(lease_holder)"

step "AccessPolicy status written by the leader (proves RBAC)"
reason() { k -n "$1" get accesspolicy sample -o jsonpath='{.status.conditions[0].reason}' 2>/dev/null; }
has_reason() { [ -n "$(reason "$1")" ]; }
for n in "$BACKEND_NS" "$CLIENT_NS"; do
  k apply -f - <<YAML
apiVersion: policy.querdian.dev/v1alpha1
kind: AccessPolicy
metadata: {name: sample, namespace: $n}
spec:
  subjects: {groups: [team-a]}
  rules: [{backends: [logs]}]  # logs has a tenantHeader, so the namespace needs the tenant label
YAML
done
wait_for "status in $BACKEND_NS" 60 has_reason "$BACKEND_NS"
wait_for "status in $CLIENT_NS" 60 has_reason "$CLIENT_NS"
echo "unlabeled namespace: $(reason "$BACKEND_NS")"
echo "labeled namespace:   $(reason "$CLIENT_NS")"
[ "$(reason "$BACKEND_NS")" = TenantNotAssigned ] || fail "expected TenantNotAssigned"
[ "$(reason "$CLIENT_NS")" = Compiled ] || fail "expected Compiled"
ok "status conditions written by the leader"

step "logs contain no errors"
if k -n "$NS" logs -l app.kubernetes.io/name=querdian --tail=-1 | grep -Ei '"level":"(error|warn)"|level=(ERROR|WARN)'; then
  fail "error/warn lines in querdian logs"
fi
ok "no error/warn log lines"

step "NetworkPolicy enforcement on this CNI?"
k -n "$CLIENT_NS" apply -f - <<YAML
apiVersion: networking.k8s.io/v1
kind: NetworkPolicy
metadata: {name: cni-probe-deny-egress}
spec: {podSelector: {matchLabels: {cni-probe: "yes"}}, policyTypes: [Egress]}
YAML
ENFORCED=1
if [ "$(tcp "$CLIENT_NS" cni-probe "$BACKEND_HOST" 3100 cni-probe=yes)" = 0 ]; then
  echo "SKIP: CNI does not enforce NetworkPolicy; the NetworkPolicy criterion stays UNVERIFIED"
  ENFORCED=0
else
  ok "CNI enforces NetworkPolicy"
fi
k -n "$CLIENT_NS" delete networkpolicy cni-probe-deny-egress

if [ "$ENFORCED" = 1 ]; then
  step "backendProtection: only querdian pods reach the backend"
  expect_reachable "$CLIENT_NS" client-before "$BACKEND_HOST" 3100
  cat >"$WORK/np-values.yaml" <<YAML
networkPolicy:
  backendProtection:
    enabled: true
    targets:
      - namespace: $BACKEND_NS
        podSelector: {matchLabels: {app: dummy-backend}}
        ports: [3100]
YAML
  h upgrade querdian "$CHART" -n "$NS" -f "$WORK/values.yaml" -f "$WORK/np-values.yaml" --wait --timeout 3m
  expect_blocked "$CLIENT_NS" client-after "$BACKEND_HOST" 3100
  # Same namespace + labels as the proxy pods (what the policy selects on).
  expect_reachable "$NS" querdian-like "$BACKEND_HOST" 3100 "$QUERDIAN_LABELS"
  # Same namespace, labels missing: must be blocked.
  expect_blocked "$NS" querdian-unlabelled "$BACKEND_HOST" 3100

  step "networkPolicy.enabled: ingress.from empty = nobody reaches the proxy"
  cat >"$WORK/np2-values.yaml" <<YAML
networkPolicy:
  enabled: true
  egress:
    oidc: {ports: [8080]}
    backends:
      to: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: $BACKEND_NS}}}]
      ports: [3100]
  backendProtection:
    enabled: true
    targets:
      - namespace: $BACKEND_NS
        podSelector: {matchLabels: {app: dummy-backend}}
        ports: [3100]
YAML
  h upgrade querdian "$CHART" -n "$NS" -f "$WORK/values.yaml" -f "$WORK/np2-values.yaml" --wait --timeout 3m
  k wait --for=condition=Ready pod -l app.kubernetes.io/name=querdian -n "$NS" --timeout=120s
  ok "querdian stays Ready with its egress policy (DNS, API server, OIDC, backends)"
  expect_blocked "$CLIENT_NS" client-proxy-denied "$PROXY_HOST" 8080
  # The management port is open to nobody either.
  expect_blocked "$CLIENT_NS" client-mgmt-denied "$PROXY_HOST" 9090
  cat >>"$WORK/np2-values.yaml" <<YAML
  ingress:
    from: [{namespaceSelector: {matchLabels: {kubernetes.io/metadata.name: $CLIENT_NS}}}]
YAML
  h upgrade querdian "$CHART" -n "$NS" -f "$WORK/values.yaml" -f "$WORK/np2-values.yaml" --wait --timeout 3m
  expect_reachable "$CLIENT_NS" client-proxy-allowed "$PROXY_HOST" 8080
  expect_blocked "$BACKEND_NS" other-ns-proxy-denied "$PROXY_HOST" 8080
  # Back to the plain values for the upgrade test below.
  h upgrade querdian "$CHART" -n "$NS" -f "$WORK/values.yaml" --wait --timeout 3m
fi

step "helm upgrade with changed config rolls the Deployment"
rs_list() { k -n "$NS" get rs -l app.kubernetes.io/name=querdian -o name | sort | tr '\n' ' '; }
before="$(k -n "$NS" get deploy querdian -o jsonpath='{.spec.template.metadata.annotations.checksum/config}')"
rs_before="$(rs_list)"
cat >"$WORK/upg-values.yaml" <<YAML
config:
  limits: {maxFanout: 8}
YAML
h upgrade querdian "$CHART" -n "$NS" -f "$WORK/values.yaml" -f "$WORK/upg-values.yaml" --wait --timeout 3m
after="$(k -n "$NS" get deploy querdian -o jsonpath='{.spec.template.metadata.annotations.checksum/config}')"
[ "$before" != "$after" ] || fail "checksum/config did not change"
k -n "$NS" rollout status deploy/querdian --timeout=120s
[ "$rs_before" != "$(rs_list)" ] || fail "no new ReplicaSet"
[ "$(k -n "$NS" get deploy querdian -o jsonpath='{.status.readyReplicas}')" = 2 ] || fail "expected 2 ready replicas after upgrade"
ok "checksum ${before:0:12} -> ${after:0:12}, new ReplicaSet, 2/2 Ready"

step "helm uninstall is clean"
h uninstall querdian -n "$NS" --wait
left="$(
  k -n "$NS" get deploy,svc,cm,sa,role,rolebinding,pdb,networkpolicy -l app.kubernetes.io/instance=querdian -o name
  k get clusterrole,clusterrolebinding -l app.kubernetes.io/instance=querdian -o name
  k -n "$BACKEND_NS" get networkpolicy -o name
)"
[ -z "$left" ] || { echo "$left" >&2; fail "resources left after uninstall"; }
k get crd accesspolicies.policy.querdian.dev >/dev/null
ok "uninstall removed all release resources; CRDs are kept (Helm never deletes crds/)"

step "ALL CHECKS PASSED"
