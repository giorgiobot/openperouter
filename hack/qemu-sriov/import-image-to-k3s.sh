#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

# Streams a local docker image into the VM's k3s containerd image store, so the
# helm deploy can run with pullPolicy=IfNotPresent against an image (main-grout)
# that is built in CI and never published to a registry.
IMAGE="${1:?usage: import-image-to-k3s.sh <image-ref>}"

echo "Importing $IMAGE into the VM's k3s..."
docker save "$IMAGE" | ssh_cmd 'sudo k3s ctr images import -'
echo "Imported $IMAGE"
