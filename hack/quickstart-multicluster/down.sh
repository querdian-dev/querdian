#!/usr/bin/env bash
# Removes the multi-cluster quickstart: the three kind clusters, the work directory with the certs and
# keys, and the locally built Querdian image.
#   hack/quickstart-multicluster/down.sh
#   KEEP_IMAGE=1 hack/quickstart-multicluster/down.sh   # keep the image for the next up.sh (SKIP_BUILD=1)
# The single-cluster quickstart uses the same image tag (IMAGE_TAG, default quickstart). Use KEEP_IMAGE=1
# if it still runs.
set -uo pipefail
# shellcheck source=lib.sh
source "$(dirname "${BASH_SOURCE[0]}")/lib.sh"

need kind docker
for c in $(all_clusters); do
  step "deleting kind cluster $c"
  kind delete cluster --name "$c"
done
# MC_WORK_DIR can be any path. remove_work_dir deletes it only if up.sh made it (marker file).
remove_work_dir || true
if [ "${KEEP_IMAGE:-0}" != 1 ]; then
  step "removing image $IMAGE_REPO:$IMAGE_TAG"
  docker rmi -f "$IMAGE_REPO:$IMAGE_TAG" >/dev/null 2>&1 || true
fi
exit 0
