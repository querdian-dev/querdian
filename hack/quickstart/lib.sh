#!/usr/bin/env bash
# Shared settings and helpers for up.sh, down.sh and verify.sh. Source it, do not run it.
#
# Environment (all optional):
#   CLUSTER        kind cluster name                        (default querdian-demo)
#   KEYCLOAK_PORT  host AND in-cluster port of Keycloak     (default 8080)
#   IMAGE_TAG      tag of the locally built Querdian image  (default quickstart)
#   DEMO_HISTORY_HOURS  hours of backfilled demo data       (default 24, 1..720)

CLUSTER="${CLUSTER:-querdian-demo}"
KEYCLOAK_PORT="${KEYCLOAK_PORT:-8080}"
UI_PORT=8081 # fixed: the Querdian policy dashboard, kind maps it to the NodePort 30081
GRAFANA_PORT=3000 # fixed: the realm redirect URI and Grafana root_url say http://localhost:3000
IMAGE_TAG="${IMAGE_TAG:-quickstart}"
IMAGE_REPO="${IMAGE_REPO:-ghcr.io/querdian-dev/querdian}"
QS_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
ROOT="$(cd "$QS_DIR/../.." && pwd)"
DEMO_HISTORY_HOURS="${DEMO_HISTORY_HOURS:-24}"
case "$DEMO_HISTORY_HOURS" in
  '' | *[!0-9]* | 0) printf 'ERROR: DEMO_HISTORY_HOURS must be a positive integer\n' >&2; exit 1 ;;
esac
[ "$DEMO_HISTORY_HOURS" -le 720 ] || { printf 'ERROR: DEMO_HISTORY_HOURS must be at most 720\n' >&2; exit 1; }
# Retention of Prometheus and Tempo has to outlive the history (Tempo deletes blocks by age).
DEMO_RETENTION_HOURS=$((DEMO_HISTORY_HOURS + 24))
DEMOGEN_IMAGE="localhost/querdian-demogen:$IMAGE_TAG"
KCTX="kind-$CLUSTER"
KC_HOST="keycloak.localtest.me"
ISSUER="http://$KC_HOST:$KEYCLOAK_PORT/realms/observability"

# podman behind a docker shim needs kind's podman provider.
if docker --version 2>/dev/null | grep -qi podman; then export KIND_EXPERIMENTAL_PROVIDER=podman; fi

step() { printf '\n=== %s\n' "$*"; }
info() { printf '    %s\n' "$*"; }
ok() { printf 'PASS: %s\n' "$*"; }
die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }
k() { kubectl --context "$KCTX" "$@"; }
h() { helm --kube-context "$KCTX" "$@"; }

need() { for c in "$@"; do command -v "$c" >/dev/null 2>&1 || die "missing required tool: $c"; done; }

# Replaces the @@...@@ placeholders of a manifest.
render() {
  sed -e "s/@@KEYCLOAK_PORT@@/$KEYCLOAK_PORT/g" \
    -e "s/@@IMAGE_TAG@@/$IMAGE_TAG/g" \
    -e "s/@@DEMO_HISTORY_HOURS@@/$DEMO_HISTORY_HOURS/g" \
    -e "s/@@RETENTION_HOURS@@/$DEMO_RETENTION_HOURS/g" \
    -e "s#@@DEMOGEN_IMAGE@@#$DEMOGEN_IMAGE#g" "$@"
}
