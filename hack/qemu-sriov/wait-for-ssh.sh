#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

for i in $(seq 1 60); do
  if ssh_cmd true 2>/dev/null; then
    echo "SSH is up"
    exit 0
  fi
  sleep 5
done

echo "Timed out waiting for SSH"
exit 1
