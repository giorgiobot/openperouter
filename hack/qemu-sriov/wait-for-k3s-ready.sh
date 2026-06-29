#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

for i in $(seq 1 60); do
  if ssh_cmd sudo k3s kubectl get nodes 2>/dev/null | grep -q Ready; then
    ssh_cmd sudo k3s kubectl get nodes -o wide

    # Hand the node's own kubeconfig to the caller so later steps talk to k3s
    # directly over the port-forwarded API server (its "server:" entry is
    # already https://127.0.0.1:6443), instead of through ssh.
    ssh_cmd sudo chmod 644 /etc/rancher/k3s/k3s.yaml
    scp_cmd "$SSH_USER@$SSH_HOST:/etc/rancher/k3s/k3s.yaml" kubeconfig
    chmod 600 kubeconfig
    [ -n "${GITHUB_ENV:-}" ] && echo "KUBECONFIG=$(pwd)/kubeconfig" >> "$GITHUB_ENV"
    exit 0
  fi
  sleep 10
done

echo "Timed out waiting for k3s node to be Ready"
exit 1
