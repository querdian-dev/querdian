#!/usr/bin/env bash
# Shared helpers for tenant-federation experiments. Requires docker (or podman shim) and curl.
# Black-box only: behaviour is observed over HTTP; no upstream source is consulted.
set -euo pipefail
LOKI_IMAGE=${LOKI_IMAGE:-docker.io/grafana/loki:3.5.0}
TEMPO_IMAGE=${TEMPO_IMAGE:-docker.io/grafana/tempo:2.8.1}
MIMIR_IMAGE=${MIMIR_IMAGE:-docker.io/grafana/mimir:2.16.0}
HERE=$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)

wait_ready() { # url
  for _ in $(seq 1 90); do curl -fs "$1" >/dev/null 2>&1 && return 0; sleep 2; done
  echo "timeout waiting for $1" >&2
  return 1
}
show() { echo "--- $*"; }
