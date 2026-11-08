#!/usr/bin/env bash
# Shared settings and helpers of the multi-cluster quickstart (up.sh, down.sh, verify.sh). Source it, do
# not run it. It sources hack/quickstart/lib.sh, so KEYCLOAK_PORT, IMAGE_TAG and IMAGE_REPO apply here
# too, and the helpers k and h operate on the hub cluster.
#
# Environment (all optional):
#   CLUSTER_PREFIX  prefix of the kind cluster names               (default querdian)
#   MC_WORK_DIR     work directory for the certs                    (default ~/.cache/querdian-quickstart-multicluster)
#
# The scripts are for bash 3.2 (macOS) too: no associative arrays, no mapfile, and each expansion of an
# array that can be empty uses ${a[@]+"${a[@]}"}.
# shellcheck disable=SC2034 # the scripts that source this file use the variables

CLUSTER_PREFIX="${CLUSTER_PREFIX:-querdian}"
case "$CLUSTER_PREFIX" in
  '' | *[!a-z0-9-]*) printf 'ERROR: CLUSTER_PREFIX must contain only a-z, 0-9 and -\n' >&2; exit 1 ;;
esac
# The hub is the kind cluster of hack/quickstart/lib.sh: k, h and KCTX refer to it.
CLUSTER="$CLUSTER_PREFIX-hub"
# shellcheck source=../quickstart/lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/../quickstart/lib.sh"
MC_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

# Cluster label values. A SpokeCluster name is the cluster label value that the ingest gateway writes,
# and the suffix of the kind cluster name. The gateway (manifests/ingest-gateway.yaml) and the policies
# (manifests/*-policies.yaml) contain these names, so a change here needs a change there.
HUB_NAME=hub
SPOKES=(spoke-a spoke-b)
TRUST_DOMAIN=quickstart.local
TENANT=demo

MC_WORK_DIR="${MC_WORK_DIR:-${XDG_CACHE_HOME:-$HOME/.cache}/querdian-quickstart-multicluster}"
CERT_DIR="$MC_WORK_DIR/certs"
MC_WORK_MARKER=.querdian-quickstart-multicluster

spoke_cluster() { printf '%s-%s' "$CLUSTER_PREFIX" "$1"; }      # kind cluster name of a spoke
spoke_ctx() { printf 'kind-%s-%s' "$CLUSTER_PREFIX" "$1"; }     # kube context of a spoke
ks() { local s="$1"; shift; kubectl --context "$(spoke_ctx "$s")" "$@"; }
hs() { local s="$1"; shift; helm --kube-context "$(spoke_ctx "$s")" "$@"; }
all_clusters() {
  local s
  echo "$CLUSTER"
  for s in "${SPOKES[@]}"; do spoke_cluster "$s"; echo; done
}

# The identity of the agent of a spoke: one URI SAN, the last path segment is the SpokeCluster name.
# The middle segment is the cert generation (docs/multicluster.md, "the agent certificate").
agent_identity() { printf 'spiffe://%s/agent/1/%s' "$TRUST_DOMAIN" "$1"; }
# The identity of the ingest cert of a spoke. It differs from the agent identity (PLAN 18.5).
ingest_identity() { printf 'spiffe://%s/ingest/%s' "$TRUST_DOMAIN" "$1"; }

# The address of the hub node on the Docker network "kind". The spokes reach the NodePorts of the hub
# (receiver and ingest gateway) at this address.
hub_ip() {
  k get nodes -o jsonpath='{.items[0].status.addresses[?(@.type=="InternalIP")].address}'
}

# subst FILE KEY=VALUE...: render() of hack/quickstart/lib.sh, then @@KEY@@ -> VALUE. Stops if a
# placeholder is left.
subst() {
  local f="$1" a out
  local -a args=(-e '')
  shift
  for a in "$@"; do args+=(-e "s#@@${a%%=*}@@#${a#*=}#g"); done
  out="$(render "$f" | sed "${args[@]}")" || die "cannot render $f"
  if grep -q '@@[A-Z_]*@@' <<<"$out"; then
    die "$f: placeholder left: $(grep -o '@@[A-Z_]*@@' <<<"$out" | sort -u | tr '\n' ' ')"
  fi
  printf '%s\n' "$out"
}

# pf CONTEXT NAMESPACE SERVICE LOCAL_PORT REMOTE_PORT: a background kubectl port-forward. The PID is
# appended to PF_PIDS; the caller kills them.
PF_PIDS=()
port_open() { (exec 3<>"/dev/tcp/127.0.0.1/$1") 2>/dev/null; }
pf() {
  ! port_open "$4" || die "local port $4 is in use; it is necessary for a port-forward to $2/$3"
  kubectl --context "$1" -n "$2" port-forward "svc/$3" "$4:$5" >/dev/null 2>&1 &
  PF_PIDS+=($!)
  disown $! 2>/dev/null
  local i
  for i in $(seq 1 40); do
    if port_open "$4"; then return 0; fi
    [ "$i" -lt 40 ] && sleep 0.5
  done
  die "port-forward to $2/$3 on 127.0.0.1:$4 did not start"
}
pf_stop() {
  local p
  for p in ${PF_PIDS[@]+"${PF_PIDS[@]}"}; do kill "$p" 2>/dev/null; done
  PF_PIDS=()
}

# --- direct queries to the hub backends (as the hub operator, NOT through Querdian) -------------------
# The migration checks of docs/multicluster.md (steps 2 and 5) query the backends directly. Each
# function prints the JSON body.
BPF_PROM="${BPF_PROM:-19090}"
BPF_LOKI="${BPF_LOKI:-13100}"
BPF_TEMPO="${BPF_TEMPO:-13200}"
backends_pf() {
  pf "$KCTX" observability prometheus "$BPF_PROM" 9090
  pf "$KCTX" observability loki "$BPF_LOKI" 3100
  pf "$KCTX" observability tempo "$BPF_TEMPO" 3200
}
prom_direct() { # promql
  curl -fsS -G "http://127.0.0.1:$BPF_PROM/api/v1/query" --data-urlencode "query=$1"
}
loki_direct() { # logql-metric-query: instant query
  curl -fsS -G -H "X-Scope-OrgID: $TENANT" "http://127.0.0.1:$BPF_LOKI/loki/api/v1/query" --data-urlencode "query=$1"
}
loki_direct_range() { # logql seconds-back: log query
  curl -fsS -G -H "X-Scope-OrgID: $TENANT" "http://127.0.0.1:$BPF_LOKI/loki/api/v1/query_range" \
    --data-urlencode "query=$1" --data-urlencode limit=1000 \
    --data-urlencode "start=$(($(date +%s) - $2))000000000" --data-urlencode "end=$(($(date +%s) + 60))000000000"
}
tempo_direct() { # traceql seconds-back: search
  local now
  now=$(date +%s)
  curl -fsS -G -H "X-Scope-OrgID: $TENANT" "http://127.0.0.1:$BPF_TEMPO/api/search" \
    --data-urlencode "q=$1" --data-urlencode limit=1000 --data-urlencode "start=$((now - $2))" --data-urlencode "end=$((now + 60))"
}
