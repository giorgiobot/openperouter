#!/usr/bin/env bash
# SPDX-License-Identifier: Apache-2.0
#
# Creates "fake VF" VLAN sub-interfaces inside the QEMU VM to simulate
# workload VFs for VF-to-VF E2E testing.
#
# The 3rd igb NIC (PCI 0000:03:00.0) is used as the fake VF host; its
# VLAN sub-interfaces stand in for the workload VFs that would normally
# be set up by the NIC embedded switch. The 2nd igb NIC (0000:02:00.0)
# is reserved as the trunk VF that grout will bind via DPDK.
#
# All four igb NICs share the br-underlay bridge on the host side, so
# VLAN-tagged frames from the fake VFs reach the trunk VF just as they
# would through a real NIC embedded switch.

set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
source "${SCRIPT_DIR}/qemu-common.sh"

FAKEVF_PCI="0000:03:00.0"
VLAN_A=500
VLAN_B=600

echo "Setting up fake VFs on PCI ${FAKEVF_PCI} with VLANs ${VLAN_A} and ${VLAN_B}..."

ssh_vm "sudo bash -s" <<REMOTE
set -euo pipefail

IFACE=\$(ls /sys/bus/pci/devices/${FAKEVF_PCI}/net/ 2>/dev/null | head -1)
if [ -z "\$IFACE" ]; then
    echo 'ERROR: No network interface found for PCI ${FAKEVF_PCI}' >&2
    exit 1
fi
echo "Found NIC: \$IFACE at PCI ${FAKEVF_PCI}"

ip link set "\$IFACE" up

ip link add link "\$IFACE" name fakevf${VLAN_A} type vlan id ${VLAN_A} 2>/dev/null || true
ip link add link "\$IFACE" name fakevf${VLAN_B} type vlan id ${VLAN_B} 2>/dev/null || true
ip link set fakevf${VLAN_A} up
ip link set fakevf${VLAN_B} up

echo 'Fake VF interfaces:'
ip -br link show fakevf${VLAN_A}
ip -br link show fakevf${VLAN_B}
REMOTE

echo "Fake VF setup complete."
