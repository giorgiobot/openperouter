#!/usr/bin/env bash
set -euo pipefail
source "$(dirname "$0")/lib.sh"

# Creates an Underlay that targets the first SR-IOV VF of the emulated PF and
# checks that the perouter controller moves that NIC out of the host root netns
# and into the "perouter" named netns.
#
# Kubernetes objects are driven with kubectl against the node's API server
# (via $KUBECONFIG, the port-forwarded 6443). Only the netns/VF inspection
# goes over SSH, since it reads node-local state kubectl can't see.

KUBECTL="${KUBECTL:-kubectl}"
NS="${NAMESPACE:-openperouter-system}"
TIMEOUT="${UNDERLAY_TIMEOUT:-120}" # seconds to wait for the nic move

k() { "$KUBECTL" "$@"; }

# Resolve the PF and its first VF's interface name on the node: the igb PF (the
# device exposing sriov_totalvfs) -> its virtfn0 VF -> that VF's netdev name.
read -r PF_IFACE VF_IFACE < <(ssh_cmd '
  for d in /sys/bus/pci/devices/*/; do
    if [ -f "${d}sriov_totalvfs" ]; then
      pf=$(basename ${d}net/* 2>/dev/null)
      vf=$(basename "$(readlink -f "${d}virtfn0")")
      vfif=$(basename /sys/bus/pci/devices/"$vf"/net/* 2>/dev/null)
      echo "$pf $vfif"
      break
    fi
  done') || true
if [ -z "${VF_IFACE:-}" ] || [ "$VF_IFACE" = "*" ] || [ "${PF_IFACE:-}" = "*" ]; then
  echo "could not determine the PF/VF interface names on the node" >&2
  exit 1
fi
echo "PF interface: $PF_IFACE; first VF interface: $VF_IFACE"

# Sanity: the VF starts in the host root netns.
if ! ssh_cmd "ip link show $VF_IFACE >/dev/null 2>&1"; then
  echo "expected $VF_IFACE to start in the host root netns" >&2
  exit 1
fi

# Apply an Underlay targeting the VF. asn, one neighbor and tunnelEndpoint
# satisfy the CRD's required fields; the neighbor address is a placeholder
# since no BGP session is needed to observe the nic move.
echo "Creating Underlay targeting $VF_IFACE..."
k apply -f - <<EOF
apiVersion: openpe.openperouter.github.io/v1alpha1
kind: Underlay
metadata:
  name: underlay
  namespace: $NS
spec:
  asn: 64514
  tunnelEndpoint:
    cidrs:
      - 100.65.0.0/24
  nics:
    - $VF_IFACE
  neighbors:
    - asn: 64512
      address: 192.168.11.2
EOF

# Wait for the controller to move the VF into the perouter netns.
echo "Waiting up to ${TIMEOUT}s for $VF_IFACE to move into the perouter netns..."
deadline=$((SECONDS + TIMEOUT))
while [ "$SECONDS" -lt "$deadline" ]; do
  if ssh_cmd "sudo ip -n perouter link show $VF_IFACE >/dev/null 2>&1"; then
    echo "OK: $VF_IFACE was moved into the perouter netns:"
    ssh_cmd "sudo ip -n perouter link show $VF_IFACE"
    if ssh_cmd "ip link show $VF_IFACE >/dev/null 2>&1"; then
      echo "unexpected: $VF_IFACE is still present in the host root netns" >&2
      exit 1
    fi
    echo "Underlay test passed: $VF_IFACE is in the perouter netns and gone from the host."
    exit 0
  fi
  sleep 3
done

echo "Timed out waiting for $VF_IFACE to move into the perouter netns" >&2
echo "::group::underlay test diagnostics" >&2
k -n "$NS" get underlay -o yaml >&2 || true
k -n "$NS" get pods -o wide >&2 || true
k -n "$NS" logs -l app.kubernetes.io/component=controller --all-containers --prefix --tail=150 >&2 || true
ssh_cmd "echo '--- host links ---'; ip -br link show; echo '--- perouter netns links ---'; sudo ip -n perouter link show 2>&1 || echo 'perouter netns not found'" >&2 || true
echo "::endgroup::" >&2
exit 1
