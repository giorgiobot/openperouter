#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

# The openperouter controller talks to the CRI over the upstream containerd
# socket path (/run/containerd/containerd.sock, baked into the helm chart).
# k3s ships its own containerd at /run/k3s/containerd/containerd.sock, so bind
# the k3s socket onto the path the chart expects before deploying.
ssh_cmd 'sudo sh -euc "
  mkdir -p /run/containerd
  if [ ! -S /run/containerd/containerd.sock ]; then
    touch /run/containerd/containerd.sock
    mount --bind /run/k3s/containerd/containerd.sock /run/containerd/containerd.sock
  fi
"'
echo "Host prepared: /run/containerd/containerd.sock bound to the k3s containerd socket"
