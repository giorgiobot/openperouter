#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

# Confirms the substrate (no dataplane pods involved) is in the state later
# consumers will expect: the SR-IOV VFs are created and hugepages reserved.
NUM_VFS=$(ssh_cmd 'cat /sys/bus/pci/devices/*/sriov_numvfs 2>/dev/null | sort -rn | head -1')
if [ "${NUM_VFS:-0}" -lt 1 ]; then
  echo "Expected at least 1 SR-IOV VF, got: ${NUM_VFS:-0}" >&2
  exit 1
fi

FREE_HUGEPAGES=$(ssh_cmd cat /proc/sys/vm/nr_hugepages)
if [ "$FREE_HUGEPAGES" -lt 1536 ]; then
  echo "Expected >=1536 2Mi hugepages reserved, got: $FREE_HUGEPAGES" >&2
  exit 1
fi
