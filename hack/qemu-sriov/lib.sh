#!/usr/bin/env bash
# Shared SSH helpers for the QEMU k8s smoke test scripts.

SSH_KEY="${SSH_KEY:-id_ed25519}"
SSH_PORT="${SSH_PORT:-2222}"
SSH_HOST="${SSH_HOST:-127.0.0.1}"
SSH_USER="${SSH_USER:-ubuntu}"

ssh_cmd() {
  ssh -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 \
    -i "$SSH_KEY" -p "$SSH_PORT" "$SSH_USER@$SSH_HOST" "$@"
}

scp_cmd() {
  scp -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null -o ConnectTimeout=5 \
    -i "$SSH_KEY" -P "$SSH_PORT" "$@"
}
