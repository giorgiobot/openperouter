#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

# Confirms the substrate (no dataplane pods involved) is in the state later
# consumers will expect: VF0 bound to vfio-pci, hugepages reserved.
VF_DRIVER=$(ssh_cmd 'for d in /sys/bus/pci/devices/*/; do
  [ -L "$d/virtfn0" ] || continue
  vf=$(basename "$(readlink -f "$d/virtfn0")")
  basename "$(readlink -f /sys/bus/pci/devices/$vf/driver)"
done')
if [ "$VF_DRIVER" != "vfio-pci" ]; then
  echo "Expected VF0 bound to vfio-pci, got: $VF_DRIVER" >&2
  exit 1
fi

FREE_HUGEPAGES=$(ssh_cmd cat /proc/sys/vm/nr_hugepages)
if [ "$FREE_HUGEPAGES" -lt 1536 ]; then
  echo "Expected >=1536 2Mi hugepages reserved, got: $FREE_HUGEPAGES" >&2
  exit 1
fi
