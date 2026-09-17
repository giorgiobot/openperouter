# L2VNI with SR-IOV VF-to-VF Communication

## Summary

This enhancement adds a new SR-IOV VF-to-VF communication mode to the grout
L2VNI data path. In this mode, a trunk VF is bound directly to grout as a DPDK
port and bridged with the VXLAN VNI. Workload pods receive other VFs from the
same PF, tagged with a VLAN that maps to the L2VNI. The NIC's embedded switch
handles local VF-to-VF forwarding in hardware; grout handles VXLAN
encapsulation/decapsulation for remote nodes.

The existing TAP + host bridge mode is preserved unchanged and remains the
default. It is the only mode available without SR-IOV hardware and is used by
E2E CI lanes that run on virtual environments.

This corresponds to milestone **M3a** in the
[grout dataplane enhancement](grout-dataplane.md).

## Motivation

### Goals

- **Hardware-accelerated L2VNI forwarding**: offer an alternative to the
  TAP/bridge path that leverages the NIC's embedded switch for local L2
  traffic between VFs, avoiding the software bridge on the host.
- **End-to-end DPDK path**: the trunk VF is a DPDK port in grout, so both
  overlay (VXLAN) and local (VF-to-VF via trunk) forwarding bypass the kernel.
- **VLAN-to-VNI mapping**: introduce a VLAN ID on the L2VNI that maps tagged
  VF traffic to the corresponding VXLAN VNI, enabling multiple L2VNIs to share
  a single trunk VF.
- **Compatible with sriov-device-plugin / sriov-network-operator**: workload
  pods consume VFs via standard `sriov` NetworkAttachmentDefinitions; the
  perouter does not manage workload VFs.

### Non-Goals

- Managing or configuring workload VFs (VLAN assignment, driver binding). The
  user or the sriov-network-operator handles this.
- Supporting the kernel data path. VF-to-VF communication requires DPDK/grout
  to bind the trunk VF.
- Replacing the bridge-based L2VNI mode. Both modes coexist; the bridge path
  remains the default and is available for environments without SR-IOV.

## Proposal

### Overview

The existing grout L2VNI data path (TAP mode) creates a TAP port pair
connecting grout to the host namespace, and optionally attaches the host-side
TAP to a Linux or OVS bridge. Workloads connect to the bridge. This mode
remains unchanged and is the default.

```
TAP mode (existing, unchanged):

Grout:    pe-<VNI> (TAP grout-side) ── br-pe-<VNI> (bridge) ── vni<VNI> (VXLAN)
                    │
Host:     host-<VNI> (TAP host-side) ── br-hs-<VNI> (linux/OVS bridge) ── workload pods
```

The new VF-pair mode adds an alternative path using direct VF binding:

```
VF-pair mode (new):

Grout:    vlan<VLAN>.trunk-<PF> (VLAN sub-if) ── br-pe-<VNI> (bridge) ── vni<VNI> (VXLAN)
                    │
NIC:      trunk VF (VLAN 0, all tags) ◄──embedded switch──► workload VF (VLAN X)
                                                                    │
Host:                                                         workload pod
```

In the VF-pair mode, the trunk VF is bound to grout as a DPDK port. Grout
creates a VLAN sub-interface on it (filtering for the L2VNI's VLAN ID) and
bridges that sub-interface with the VXLAN VNI. Local VF-to-VF traffic on the
same VLAN is switched in hardware by the NIC; remote traffic is
VXLAN-encapsulated by grout. No TAP port pair or host bridge is created.

### User Stories

#### Story 1: High-Performance L2 Overlay with SR-IOV

As a network operator with SR-IOV-capable NICs, I want workload pods to
communicate over an L2 VXLAN overlay without any software bridge on the host.
I configure a trunk VF for the perouter and assign workload VFs to the desired
VLAN. Local pod-to-pod traffic on the same node is switched in hardware by
the NIC; cross-node traffic is VXLAN-encapsulated by grout at line rate.

#### Story 2: Multiple L2VNIs Sharing a Trunk VF

As an operator running multiple tenants, I define several L2VNIs, each with a
different VLAN ID, all pointing to the same trunk VF. Grout creates one VLAN
sub-interface per L2VNI and bridges each with its VXLAN VNI. Tenant isolation
is enforced by VLAN tags at the NIC level.

## Design Details

### API Changes

A new `sriovVFPair` field is added to `L2VNISpec`, mutually exclusive with the
existing `hostMaster` field. When `sriovVFPair` is set, the VF-pair path is
used instead of the TAP + bridge path. When neither is set (or only
`hostMaster` is set), the existing TAP mode is used as before.

`sriovVFPair` is a distinct field rather than a new `HostMaster` type because
it controls both the grout side (direct VF binding instead of TAP) and the
host side (no bridge needed), while `HostMaster` only controls the host-side
bridge attachment.

```go
// L2VNISpec defines the desired state of VNI.
// Validation intent: hostMaster and sriovVFPair are mutually exclusive.
// +kubebuilder:validation:XValidation:rule="!(has(self.hostmaster) && has(self.sriovVFPair))",message="hostmaster and sriovVFPair are mutually exclusive"
type L2VNISpec struct {
    // ... existing fields (nodeSelector, routingDomain, vni, vxlanport,
    //     underlayAddressFamily, gatewayIPs) unchanged ...

    // hostmaster is the interface on the host the veth should be attached to.
    // Mutually exclusive with sriovVFPair.
    // +optional
    HostMaster *HostMaster `json:"hostmaster,omitempty"`

    // sriovVFPair enables SR-IOV VF-to-VF communication for this L2VNI,
    // replacing the host bridge and TAP/veth pair with direct VF binding.
    // The specified trunk VF is bound to grout as a DPDK port. Workloads
    // connect via other VFs on the same PF, tagged with the specified VLAN.
    // The NIC's embedded switch handles local VF-to-VF forwarding; grout
    // handles VXLAN encap/decap for remote nodes.
    // Only valid when grout is enabled. Mutually exclusive with hostmaster.
    // +optional
    SRIOVVFPair *SRIOVVFPairConfig `json:"sriovVFPair,omitempty"`
}
```

The `SRIOVVFPairConfig` struct identifies the trunk VF and the VLAN that maps
to this L2VNI:

```go
// SRIOVVFPairConfig specifies the SR-IOV trunk VF and VLAN for VF-to-VF
// communication on an L2VNI.
// Exactly one VF selector must be used: either pciAddress alone, or pfName +
// vfIndex together.
// +kubebuilder:validation:XValidation:rule="has(self.pciAddress) != (has(self.pfName) && has(self.vfIndex))",message="specify either pciAddress or both pfName and vfIndex, not both"
// +kubebuilder:validation:XValidation:rule="!has(self.pfName) || has(self.vfIndex)",message="vfIndex is required when pfName is set"
// +kubebuilder:validation:XValidation:rule="!has(self.vfIndex) || has(self.pfName)",message="pfName is required when vfIndex is set"
type SRIOVVFPairConfig struct {
    // pciAddress is the PCI Bus:Device.Function address of the trunk VF to
    // bind to grout (e.g. "0000:03:02.0"). The trunk VF must have no VLAN
    // configured (VLAN 0) so it receives all tagged frames from other VFs.
    // Mutually exclusive with pfName/vfIndex.
    // +kubebuilder:validation:Pattern=`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`
    // +optional
    PCIAddress *string `json:"pciAddress,omitempty"`

    // pfName is the name of the Physical Function whose VF will be the
    // trunk port. Must be used together with vfIndex.
    // Mutually exclusive with pciAddress.
    // +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9._-]*$`
    // +kubebuilder:validation:MaxLength=15
    // +optional
    PFName *string `json:"pfName,omitempty"`

    // vfIndex is the index of the Virtual Function on the PF to use as
    // the trunk port. Must be used together with pfName.
    // Mutually exclusive with pciAddress.
    // +kubebuilder:validation:Minimum=0
    // +optional
    VFIndex *int `json:"vfIndex,omitempty"`

    // vlan is the 802.1Q VLAN ID that maps to this L2VNI. Workload VFs
    // on the same PF configured with this VLAN ID will participate in
    // this L2VNI's VXLAN overlay. Traffic from workload VFs tagged with
    // this VLAN arrives at the trunk VF, where grout matches it to this
    // L2VNI and handles VXLAN encap/decap.
    // +kubebuilder:validation:Minimum=1
    // +kubebuilder:validation:Maximum=4094
    // +required
    VLAN int32 `json:"vlan"`

    // portOptions specifies optional DPDK port parameters for the trunk VF.
    // +optional
    PortOptions *GroutPortOptions `json:"portOptions,omitempty"`
}
```

**Example:**

```yaml
apiVersion: network.openperouter.io/v1alpha1
kind: L2VNI
metadata:
  name: tenant-a-l2
  namespace: openperouter-system
spec:
  vni: 100
  routingDomain:
    type: L3VNI
    l3vni:
      name: tenant-a
  gatewayIPs:
    - 10.100.0.1/24
  sriovVFPair:
    pfName: ens8f0
    vfIndex: 0
    vlan: 10
```

In this example:
- VF 0 of `ens8f0` is the trunk VF, bound to grout.
- VLAN 10 maps to VNI 100.
- The user configures other VFs on `ens8f0` with VLAN 10 and gives them to
  workload pods via `sriov` NetworkAttachmentDefinitions.

**Multiple L2VNIs sharing a trunk VF:**

```yaml
# Tenant A: VNI 100 on VLAN 10
apiVersion: network.openperouter.io/v1alpha1
kind: L2VNI
metadata:
  name: tenant-a-l2
spec:
  vni: 100
  sriovVFPair:
    pfName: ens8f0
    vfIndex: 0
    vlan: 10
---
# Tenant B: VNI 200 on VLAN 20
apiVersion: network.openperouter.io/v1alpha1
kind: L2VNI
metadata:
  name: tenant-b-l2
spec:
  vni: 200
  sriovVFPair:
    pfName: ens8f0
    vfIndex: 0
    vlan: 20
```

Both L2VNIs reference the same trunk VF. The controller binds it once; grout
creates two VLAN sub-interfaces (VLAN 10, VLAN 20), each bridged with its
VXLAN VNI.

### Grout Data Path

When `sriovVFPair` is set, the controller binds the trunk VF to grout as a
DPDK port (once per VF, even if multiple L2VNIs share it) and creates a VLAN
sub-interface on it for the L2VNI's VLAN ID. Grout supports VLAN
sub-interfaces on DPDK ports, which filter incoming frames by VLAN ID and
strip/add the 802.1Q tag. The VLAN sub-interface is then bridged with the
VXLAN VNI, just as the TAP port is in the existing mode.

The VRF, VXLAN, and bridge setup are the same as the TAP mode. The difference
is what gets bridged: a VLAN sub-interface on the trunk VF instead of a TAP
port.

### Prerequisites

- **SR-IOV enabled NIC**: the PF must have VFs enabled (`echo N > /sys/class/net/<pf>/device/sriov_numvfs`).
- **Trunk VF**: one VF must be left at VLAN 0 (default, no VLAN configured)
  so it receives all 802.1Q-tagged frames. This is the VF given to perouter.
- **Workload VFs**: other VFs are assigned to specific VLANs by the user or
  the sriov-network-operator (`ip link set <pf> vf <n> vlan <vid>`). These
  VFs are consumed by workload pods via `sriov` NetworkAttachmentDefinitions
  and the sriov-device-plugin.
- **Grout enabled**: the VF-pair mode requires grout (`openperouter.grout.enabled: true`).
- **DPDK driver support**: the trunk VF must be compatible with a DPDK PMD
  (vfio-pci for Intel NICs, mlx5 bifurcated driver for Mellanox).

### Validation

- `sriovVFPair` and `hostMaster` are mutually exclusive (CEL on `L2VNISpec`).
- VF selector rules: same as `GroutPortConfig` (exactly one of `pciAddress` or
  `pfName + vfIndex`).
- VLAN range: 1-4094.
- VLAN uniqueness per trunk VF: if two L2VNIs reference the same trunk VF, they
  must have distinct VLAN IDs. Enforced by the validation webhook (cross-resource
  check).
- Grout-only: the controller rejects `sriovVFPair` at runtime when grout is not
  enabled (`Ready=False, Reason=GroutRequired`).

### Test Plan

- **Unit tests**: conversion of `sriovVFPair` API types to internal params,
  PCI address resolution, driver binding logic (reuses existing `sriov` package
  tests).
- **E2E tests**: require SR-IOV hardware. A new test suite with label
  `sriov-support` exercises:
  - Single L2VNI with VF-pair: pod-to-pod on the same node (VF-to-VF), pod-to-pod
    across nodes (VXLAN).
  - Multiple L2VNIs sharing a trunk VF with different VLANs: tenant isolation.
  - L2VNI with VF-pair + routing domain (L3VNI): distributed gateway reachability.
  - Cleanup: delete L2VNI and verify trunk VF is unbound and resources are cleaned.

## Drawbacks

- **Hardware dependency**: requires SR-IOV-capable NICs, limiting
  portability. Operators without SR-IOV (and E2E CI on virtual
  environments) continue to use the TAP + bridge path.
- **VF scarcity**: each trunk VF consumes one of the PF's limited VF slots
  (typically 64-128). When sharing a trunk VF across L2VNIs, this is
  amortized.
- **External VF management**: the user (or sriov-network-operator) must
  correctly configure workload VFs with the right VLAN. Misconfigured VLANs
  result in silent connectivity failures.

## Alternatives Considered

### Extend HostMaster Union

Instead of a separate `sriovVFPair` field, add `SRIOVVFPair` as a new type in
the `HostMaster` discriminated union. This was rejected because the VF-pair
mode controls both the grout side (direct VF binding, no TAP) and the host
side (no bridge), while `HostMaster` only controls the host-side bridge
attachment. A separate field makes the scope of the change explicit and keeps
the TAP + bridge path untouched.

### Separate Trunk VF Resource

Define the trunk VF once in a dedicated CRD (or on the Underlay) and have
L2VNIs reference it by name, carrying only the VLAN. This avoids duplicating
the VF selector across L2VNIs that share a trunk. Rejected for the initial
implementation in favor of simplicity: the VF selector is inlined on each
L2VNI, and the controller deduplicates internally. A dedicated resource can be
introduced later if the duplication becomes a usability issue.

## Implementation History

- 2026-07-28: Initial proposal drafted
