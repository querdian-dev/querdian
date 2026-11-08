#!/usr/bin/env bash
# Removes the multi-cluster quickstart: the three kind clusters, the work directory with the certs and
# keys, and the locally built Querdian image.
#   hack/quickstart-multicluster/down.sh
#   KEEP_IMAGE=1 hack/quickstart-multicluster/down.sh   # keep the image for the next up.sh (SKIP_BUILD=1)
set -uo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

need kind docker
for c in $(all_clusters); do
  step "deleting kind cluster $c"
  kind delete cluster --name "$c"
done
step "removing the work directory $MC_WORK_DIR"
case "$MC_WORK_DIR" in
  "" | / | "$HOME" | "$HOME/") die "refusing to remove '$MC_WORK_DIR'" ;;
esac
rm -rf "$MC_WORK_DIR"
if [ "${KEEP_IMAGE:-0}" != 1 ]; then
  step "removing image $IMAGE_REPO:$IMAGE_TAG"
  docker rmi -f "$IMAGE_REPO:$IMAGE_TAG" >/dev/null 2>&1 || true
fi
exit 0
