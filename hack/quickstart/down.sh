#!/usr/bin/env bash
# Removes the quickstart: the kind cluster and the locally built Querdian image.
#   hack/quickstart/down.sh                 # CLUSTER=querdian-demo by default
#   KEEP_IMAGE=1 hack/quickstart/down.sh    # keep the built image for the next up.sh (SKIP_BUILD=1)
set -uo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

need kind docker
step "deleting kind cluster $CLUSTER"
kind delete cluster --name "$CLUSTER"
if [ "${KEEP_IMAGE:-0}" != 1 ]; then
  step "removing image $IMAGE_REPO:$IMAGE_TAG"
  docker rmi -f "$IMAGE_REPO:$IMAGE_TAG" >/dev/null 2>&1 || true
fi
exit 0
