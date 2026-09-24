# GroutPort IPAM: Per-Node Address Assignment Options

## Problem

`Underlay.Spec.Interfaces[].GroutPort.IPAM.Addresses` holds literal CIDRs
(e.g. `"10.0.0.5/24"`) that are applied **verbatim** to every node matching
the Underlay's `NodeSelector`. When the same Underlay targets multiple nodes,
all nodes receive the same IP address --- a conflict.

Unlike VTEP or router-ID addresses, underlay IPs are fabric-assigned and
cannot be derived from a sequential pool. The user must specify them.

---

## Option A: Per-node address map in the Underlay spec

Add a `perNode` map keyed by node name. The controller looks up the current
node's name and uses the corresponding addresses.

### API

```go
type GroutPortIPAM struct {
    // perNode maps Kubernetes node names to the list of CIDRs to assign
    // to the grout port on that node. At most one IPv4 and one IPv6 CIDR
    // per node (dual-stack).
    // +required
    PerNode map[string][]string `json:"perNode"`
}
```

### Example YAML

```yaml
apiVersion: network.openperouter.io/v1alpha1
kind: Underlay
metadata:
  name: fabric
spec:
  interfaces:
  - type: GroutPort
    groutPort:
      pfName: enp1s0f0
      vfIndex: 0
      ipam:
        perNode:
          worker-0: ["10.0.0.1/24"]
          worker-1: ["10.0.0.2/24"]
          worker-2: ["10.0.0.3/24", "fd00::3/64"]
```

### Changes required

| Area | Change |
|------|--------|
| `api/v1alpha1/underlay_types.go` | Replace `Addresses []string` with `PerNode map[string][]string` in `GroutPortIPAM` |
| `internal/conversion/host_conversion.go` | `groutPortInterfaceToHost()` receives node name, looks up `PerNode[nodeName]` |
| Validation webhook | Validate each per-node entry (CIDR format, at most 1 IPv4 + 1 IPv6) |
| CRD schema | Map type with string keys, array-of-string values |

### Pros

- Full explicit control --- each node gets exactly the address the user chose
- Single Underlay CR for the whole cluster
- Simple mental model: "this node gets this address"
- Self-contained: the CR alone describes the full addressing plan

### Cons

- **Node names baked into the CR** --- node replacements/renames require CR
  updates, the CR must be updated before a new node can join
- CR grows linearly with cluster size
- Maps are awkward in Kubernetes CRD validation (CEL cost, OpenAPI limitations)
- Breaks consistency with every other IPAM pattern in the project (none use
  per-node maps)
- Validation webhook must cross-reference cluster nodes to warn about stale
  entries

---

## Option B: Address sourced from a node annotation

Remove addresses from the Underlay spec for GroutPort. Instead, the controller
reads a well-known node annotation set by infrastructure tooling. This follows
the same pattern as `openpe.io/nodeindex`.

### API

```go
type GroutPortIPAM struct {
    // source indicates where to read addresses from.
    // When "NodeAnnotation", the controller reads addresses from the
    // annotation `openperouter.io/grout-addresses` on the node.
    // +kubebuilder:validation:Enum=NodeAnnotation
    // +required
    Source string `json:"source"`
}
```

Or, simpler: make IPAM optional and when it is absent (or empty), the
controller falls back to the node annotation.

### Node annotation format

```yaml
apiVersion: v1
kind: Node
metadata:
  name: worker-0
  annotations:
    openperouter.io/grout-addresses: '["10.0.0.1/24"]'
    # or split by family:
    # openperouter.io/grout-ipv4: "10.0.0.1/24"
    # openperouter.io/grout-ipv6: "fd00::1/64"
```

### Example Underlay YAML

```yaml
apiVersion: network.openperouter.io/v1alpha1
kind: Underlay
metadata:
  name: fabric
spec:
  interfaces:
  - type: GroutPort
    groutPort:
      pfName: enp1s0f0
      vfIndex: 0
      ipam:
        source: NodeAnnotation
```

### Changes required

| Area | Change |
|------|--------|
| `api/v1alpha1/underlay_types.go` | New `Source` field (or make `Addresses` optional) in `GroutPortIPAM` |
| `internal/conversion/host_conversion.go` | `groutPortInterfaceToHost()` receives the Node object, reads annotation |
| Controller | Watch `Node` objects for annotation changes (trigger re-reconciliation) |
| Validation webhook | Validate annotation format on reconcile (not at admission time) |

### Pros

- Per-node state lives where it belongs --- on the node object
- Underlay CR stays clean, topology-agnostic, and truly shared
- Annotations can be set by infrastructure tooling (Ansible, NMState,
  MachineConfig, Terraform) that already knows node-level details
- Consistent with how `nodeIndex` is already managed (node annotation)
- New nodes can be prepared independently of the Underlay CR

### Cons

- **Splits source of truth** --- the Underlay alone doesn't describe the full
  config; you need to inspect each node to see its address
- Annotations are untyped strings; validation can only happen at reconcile
  time, not at admission
- Users must set annotations on every node --- more operational steps
- If the annotation is missing, the controller must degrade gracefully (partial
  config or error status)
- Controller needs a watch on Node objects for annotation changes (already
  watches nodes for `nodeIndex`, so likely minimal overhead)

---

## Option C: Read addresses from the kernel netdevice before driver binding

The user pre-configures IP addresses on the kernel netdevice associated with
the VF (via NMState, NetworkManager, `ip addr add`, or infrastructure
tooling). The controller reads those addresses from the netdevice before
binding the vfio-pci driver, then applies them to the grout DPDK port.

This mirrors how `NetworkDevice` interfaces already work: the addresses
originate on the kernel interface and are migrated to grout
(`migrateAddressesToGrout()` at `internal/grout/underlay.go:344`).

### The vfio-pci problem

For Intel NICs (igb, iavf, ice, i40e), `prepareGroutPortDriver()` unbinds the
kernel driver and binds vfio-pci. This **destroys the kernel netdevice** and
all its addresses. The addresses must be captured before driver rebind.

For mlx5_core (bifurcated driver), the kernel netdevice persists --- addresses
can be read at any time.

### Proposed flow

```
   User configures IP on VF netdevice (e.g. via NMState)
                     |
                     v
   Controller reconciles Underlay
                     |
                     v
   SetupUnderlay() for GroutPort interface:
     1. Resolve PCI address from pfName+vfIndex or pciAddress
     2. Find kernel netdevice via sriov.GetPCINetDevice(pciAddr)
     3. Read addresses from netdevice via netlink (netlink.AddrList)
     4. Store captured addresses in GroutPortParams.Addresses
     5. prepareGroutPortDriver()  <-- kernel netdevice may disappear here
     6. configureGroutPort()      <-- uses the captured addresses
```

### API

```go
type GroutPortIPAM struct {
    // source indicates where to read addresses from.
    // "Inline" uses the addresses list below (single-node / testing).
    // "Interface" reads addresses from the kernel netdevice before
    //   driver binding (default for multi-node).
    // +kubebuilder:validation:Enum=Inline;Interface
    // +optional
    Source *string `json:"source,omitempty"`

    // addresses is a list of CIDRs to assign to the grout port.
    // Only used when source is "Inline" or omitted with addresses present.
    // +optional
    Addresses []string `json:"addresses,omitempty"`
}
```

Or even simpler: keep the current API shape but make `addresses` optional.
When `addresses` is empty, the controller reads from the kernel interface
automatically.

### Example YAML

```yaml
apiVersion: network.openperouter.io/v1alpha1
kind: Underlay
metadata:
  name: fabric
spec:
  interfaces:
  - type: GroutPort
    groutPort:
      pfName: enp1s0f0
      vfIndex: 0
      ipam: {}          # empty = read from kernel netdevice
      # ipam:
      #   addresses: ["10.0.0.5/24"]   # still works for single-node/testing
```

### Code changes

| Area | Change |
|------|--------|
| `api/v1alpha1/underlay_types.go` | Make `Addresses` optional (remove `+required`, MinItems=0) |
| `internal/grout/underlay.go` | In the `GroutPort` case of `SetupUnderlay()`, read addresses from kernel netdevice before `prepareGroutPortDriver()` and pass them to `configureGroutPort()` |
| `internal/sriov/` | Add helper: `GetNetDevAddresses(pciAddr) ([]netlink.Addr, error)` |
| `internal/conversion/host_conversion.go` | `groutPortInterfaceToHost()`: when `Addresses` is empty, leave `GroutPortParams.Addresses` nil (signal to read from device) |
| Validation webhook | Allow empty `Addresses` when source is Interface |

### Detailed implementation in SetupUnderlay

```go
case hostnetwork.UnderlayInterfaceGroutPort:
    if iface.GroutPort == nil {
        return fmt.Errorf("groutPort params missing for interface %s", iface.InterfaceName)
    }

    // Capture addresses from kernel netdevice before driver rebind
    // destroys it (Intel/vfio-pci path).
    if len(iface.GroutPort.Addresses) == 0 {
        netdev, err := sriov.GetPCINetDevice(iface.GroutPort.PCIAddress)
        if err != nil {
            return fmt.Errorf("no kernel netdevice for PCI %s to read addresses from: %w",
                iface.GroutPort.PCIAddress, err)
        }
        addrs, err := hostnetwork.AddressesForInterface(netdev)
        if err != nil {
            return fmt.Errorf("failed to read addresses from %s: %w", netdev, err)
        }
        if len(addrs) == 0 {
            return fmt.Errorf("no IP addresses configured on %s (PCI %s); "+
                "configure addresses on the VF netdevice or use inline ipam.addresses",
                netdev, iface.GroutPort.PCIAddress)
        }
        for _, a := range addrs {
            iface.GroutPort.Addresses = append(iface.GroutPort.Addresses,
                a.IPNet.String())
        }
    }

    if err := prepareGroutPortDriver(ctx, perouterNetNS, iface.GroutPort.PCIAddress); err != nil {
        return ...
    }
    if err := netnamespace.In(perouterNetNS, func() error {
        return configureGroutPort(ctx, client, iface)
    }); err != nil {
        return ...
    }
```

### Pros

- **Most natural model** --- addresses configured where network engineers
  expect them (on the interface), using standard tooling (NMState,
  NetworkManager, ip command)
- Mirrors the existing `NetworkDevice` flow (`migrateAddressesToGrout`)
- Underlay CR is fully topology-agnostic --- no per-node anything in the spec
- Zero new API surface if `addresses` is simply made optional
- Works with existing infrastructure-as-code (NMState `NodeNetworkConfigurationPolicy`, 
  MachineConfig, Ansible) that manages NIC addressing
- Backward compatible: inline `addresses` still works for single-node / testing

### Cons

- **vfio-pci timing dependency**: addresses must be read before driver rebind;
  if the controller restarts after vfio-pci is bound but before grout has the
  addresses, the kernel netdevice is gone and addresses are lost
  - Mitigation: persist captured addresses (e.g. in a file, node annotation, or
    the `RouterNodeConfigurationStatus` CR) for recovery after restart
- Requires the user to configure the VF netdevice address out-of-band
  before the controller starts --- ordering dependency
- mlx5 vs Intel asymmetry: for mlx5, the netdevice survives (bifurcated
  driver), so the timing constraint doesn't apply; for Intel, it does
- If NMState or NetworkManager re-applies config and the VF is already bound
  to vfio-pci, the address config will fail silently (no netdevice)

---

## Comparison

| Criterion | A: Per-node map | B: Node annotation | C: Kernel netdevice |
|-----------|----------------|-------------------|-------------------|
| Source of truth | Underlay CR | Node object | Kernel interface |
| Self-contained CR | Yes | No | No |
| Infrastructure tooling friendly | No | Yes | Yes |
| Node name coupling | Strong | Weak (annotation) | None |
| New API surface | Map field | Source enum | Optional addresses |
| Validation at admission | Full | Partial | No (runtime only) |
| Recovery after restart | Trivial (re-read CR) | Trivial (re-read annotation) | Needs persistence layer |
| Backward compatible | Breaking change | Additive | Additive |
| Consistency with project | Low | Medium | High (mirrors NetworkDevice) |
