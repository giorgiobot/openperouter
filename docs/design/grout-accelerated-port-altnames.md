# Selecting the accelerated underlay port by netlink alternative name

## Problem

An accelerated (DPDK) underlay port is selected today by the kernel netlink
name of the device:

```yaml
interfaces:
- type: NetworkDevice
  networkDevice:
    interfaceName: enp3s0f0v0
    acceleratedConfig:
      rxQueues: 2
```

`interfaceName` is resolved to a PCI address by reading
`/sys/class/net/<interfaceName>/device`
(`sriov.ResolveNetlinkName`, `internal/sriov/vf.go`), and the resolved
address is what is eventually handed to grout as `devargs`.

That works, but the primary netlink name is the least stable and least
expressive identifier the kernel offers:

1. **It is capped at 15 characters** (`IFNAMSIZ - 1`). udev's predictable
   names routinely exceed that on multi-port and SR-IOV NICs (for example
   `enp65s0f0npf0vf12`, 17 characters). When they do, udev cannot install
   the predictable name as the primary name; it installs it as an
   *alternative name* and the device keeps a short kernel-assigned name
   (`eth3`, `enp65s0f0v12`). The stable identifier for those NICs is exactly
   the one the API cannot express.

2. **It is not uniform across nodes.** An `Underlay` is applied to every node
   matching its `nodeSelector`, but the primary name depends on bus
   enumeration order and firmware. Two nodes with the same role but
   different slot population get different names, which forces one
   `Underlay` object per node shape.

3. **It is mutable.** Renaming a device (`ip link set ... name`) drops the
   old primary name; alternative names survive the rename, and survive the
   move into the router network namespace.

Alternative names ("altnames", `IFLA_PROP_LIST` / `IFLA_ALT_IFNAME`, kernel
>= 5.5) address all three: a device can carry any number of them, each up to
127 characters, and an administrator can stamp a role-based one on the right
NIC of every node with a single udev rule:

```
# /etc/udev/rules.d/70-perouter.rules
SUBSYSTEM=="net", ACTION=="add", ATTRS{address}=="b8:ce:f6:*", \
  PROGRAM="/sbin/ip link property add dev $name altname perouter-uplink0"
```

after which one `Underlay` selects the correct NIC fleet-wide.

This document proposes how to let the accelerated port be selected by an
alternative name.

---

## What the current code assumes

The primary name is not only the selector; it is threaded through the whole
accelerated path as an identity. Anything that changes the selector has to
account for each of these.

| Location | Assumption |
|---|---|
| `api/v1alpha1/underlay_types.go` — `NetworkDevice.InterfaceName` | `MaxLength=15`, pattern `^[a-zA-Z][a-zA-Z0-9._-]*$` |
| `conversion.isValidInterfaceName` (`internal/conversion/validate_vni.go:446`) | rejects names longer than 15 characters |
| `grout.PortName` (`internal/grout/underlay.go:32`) | grout port name is `u_<InterfaceName>` |
| `conversion.ValidateGroutUnderlay` (`internal/conversion/validate_grout.go`) | that port name must stay under `IFNAMSIZ` |
| `sriov.ResolveNetlinkName` (`internal/sriov/vf.go:25`) | the name is a directory under `/sys/class/net` |
| `devicestate.filePath` (`internal/grout/devicestate/devicestate.go:32`) | state file is `<NetlinkName>.json` |
| `grout.groutPortToUnderlayInterface` (`internal/grout/underlay.go:219`) | reconstructs `InterfaceName` from the saved state, and the result is compared against the requested list by `hostnetwork.UnderlayInterfacesToRemove` |
| `grout.restoreIPAddresses`, `teardownGroutPortUnderlay` | `netlink.LinkByName(state.NetlinkName)` on teardown |
| `prepareGroutPortDriver` (mlx5 branch) | moves the kernel netdev named `netlinkName` into the router netns |

Two of these deserve to be called out before any option is chosen, because a
naive implementation gets them wrong:

**Reconcile churn.** `SetupUnderlay` diffs the requested interfaces against
the ones it reconstructs from grout, matching on `InterfaceName`. The
reconstruction for a PCI-backed port goes through
`devicestate.LoadByPCI(...).NetlinkName`. If the user configures an altname
but the state file records the primary name, every reconcile sees the
configured interface as "new" and the existing one as "removed", and tears
the port down and rebuilds it — flapping the underlay on every sync. The
persisted state must round-trip *the selector the user wrote*, not just the
resolved name.

**Resolution has a deadline.** Alternative names are properties of the
netdev, not of the PCI device. Once the device is bound to `vfio-pci` the
netdev is gone and so are its altnames; from then on nothing on the node can
resolve `perouter-uplink0` to anything. Resolution must therefore happen
once, on the first setup, before `prepareGroutPortDriver` rebinds the
driver — which is exactly where `setupGroutPortUnderlay` already resolves and
caches the PCI address today. The existing caching shape is right; it only
needs to cache one more thing.

---

## Option A — resolve `interfaceName` against altnames as a fallback

Keep the single field. Try the primary name first; if no device has it, look
for a device carrying it as an alternative name. Relax `MaxLength` to 127 and
widen the pattern.

**Pros.** No new API surface. Existing manifests keep working unchanged. The
same fallback can be dropped into `sriov.ResolveNetlinkName`, so the L2VNI
`sriovVFPair.netlinkName` selector inherits it for free.

**Cons.** The selector becomes ambiguous: a string can legitimately be the
primary name of one device and the alternative name of another, and which one
wins then depends on lookup order rather than on the user's intent. The
field's documented meaning changes silently for existing users. Relaxing
`MaxLength` on a field that is also used to derive an `IFNAMSIZ`-bounded
grout port name is a one-way door — it cannot be tightened again without
breaking whoever relied on it.

## Option B — add an explicit `altName` selector (recommended)

Add a sibling field to `interfaceName` under `networkDevice`, with a CEL rule
making exactly one of them required. This mirrors the selector union that
`SRIOVVFPairConfig` already uses in this branch for the L2VNI trunk VF
(`pciAddress` | `pfName`+`vfIndex` | `netlinkName`).

```go
// +kubebuilder:validation:XValidation:rule="has(self.interfaceName) != has(self.altName)",message="specify exactly one of: interfaceName, altName"
type NetworkDevice struct {
	// interfaceName is the primary kernel name of the host network device
	// to move into the router netns.
	// Mutually exclusive with altName.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9._-]*$`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=15
	// +optional
	InterfaceName string `json:"interfaceName,omitempty"`

	// altName selects the host network device by one of its netlink
	// alternative names (see `ip link property add dev <dev> altname
	// <name>`). Alternative names are stable across renames and across the
	// move into the router netns, and are not limited to 15 characters, so
	// they are the recommended way to select a device uniformly across
	// nodes whose kernel naming differs. Requires kernel >= 5.5.
	// Mutually exclusive with interfaceName.
	// +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9._-]*$`
	// +kubebuilder:validation:MinLength=1
	// +kubebuilder:validation:MaxLength=127
	// +optional
	AltName *string `json:"altName,omitempty"`

	// acceleratedConfig, when set, binds the device as a DPDK port instead
	// of creating a TAP+remote= bridge. Only valid when --datapath=grout.
	// +optional
	AcceleratedConfig *AcceleratedConfig `json:"acceleratedConfig,omitempty"`
}
```

```yaml
interfaces:
- type: NetworkDevice
  networkDevice:
    altName: perouter-uplink0
    acceleratedConfig:
      rxQueues: 2
      portName: u_uplink0     # required, see "Port naming" below
```

**Pros.** Unambiguous — the user says which namespace of names they mean.
`interfaceName` keeps its current meaning, length bound and validation. It is
forward-compatible with Option C: further selectors (`pciAddress`,
`pfName`+`vfIndex`) can be added later as siblings under the same "exactly
one of" rule without restructuring anything, which is how
`SRIOVVFPairConfig` grew.

**Cons.** Two fields to document and to handle in conversion. The grout port
name can no longer be derived from the selector (see below).

## Option C — a full `deviceSelector` union

Replace `interfaceName` with a nested union offering `interfaceName`,
`altName`, `pciAddress` and `pfName`+`vfIndex`, unifying underlay device
selection with the L2VNI VF-pair selector.

**Pros.** One selector concept in the whole API. `pciAddress` is genuinely
useful for accelerated ports because it is the only identifier that survives
a `vfio-pci` rebind, so it needs no cached state at all.

**Cons.** Breaks every existing `Underlay`, needs a conversion webhook and a
deprecation cycle, and most of its value is unrelated to the altname problem
this document is about. Worth doing eventually; not worth coupling to this.

## Recommendation

**Option B.** It solves the stated problem without touching the semantics of
an existing field, it matches a selector pattern this branch already
established, and it leaves the door open to Option C. Option A's ambiguity is
the deciding factor: on a host where udev assigns both schemes, "did this
match a primary name or an altname?" is not a question an operator should
have to answer from logs.

---

## Implementation

### 1. Resolving an alternative name

`netlink.LinkByName` cannot be used directly. It sends `IFLA_ALT_IFNAME`
**only when the name exceeds 15 characters**
(`link_linux.go:1949` in vishvananda/netlink v1.3.1); for a shorter altname
such as `uplink0` it sends `IFLA_IFNAME` and the lookup fails. A dedicated
helper is needed. Matching against a link dump keeps it to library calls
already used in this repo:

```go
// internal/hostnetwork/altname.go

// LinkByAltName returns the link carrying altName as one of its netlink
// alternative names (IFLA_PROP_LIST). netlink.LinkByName only queries
// IFLA_ALT_IFNAME for names longer than IFNAMSIZ-1, so alternative names
// short enough to be primary names need an explicit lookup.
func LinkByAltName(altName string) (netlink.Link, error) {
	links, err := netlink.LinkList()
	if err != nil {
		return nil, fmt.Errorf("failed to list links while resolving alternative name %q: %w", altName, err)
	}
	var found netlink.Link
	for _, link := range links {
		if !slices.Contains(link.Attrs().AltNames, altName) {
			continue
		}
		if found != nil {
			return nil, fmt.Errorf("alternative name %q matches more than one device: %s and %s",
				altName, found.Attrs().Name, link.Attrs().Name)
		}
		found = link
	}
	if found == nil {
		return nil, fmt.Errorf("no device carries the alternative name %q", altName)
	}
	return found, nil
}
```

The dump costs one `RTM_GETLINK` over the host namespace and runs once per
interface per setup, on the path that is already doing sysfs reads and a
driver rebind. If that ever matters, it can be replaced by a direct
`RTM_GETLINK` carrying an `IFLA_ALT_IFNAME` attribute built with
`netlink/nl`; the ambiguity check above is worth keeping either way, since
the kernel enforces altname uniqueness per netns but the check makes a
misconfiguration legible instead of arbitrary.

`sriov.ResolveNetlinkName` stays as it is — it takes a primary name, which is
what `/sys/class/net` is keyed by. The altname is resolved to a primary name
first, then handed to it.

### 2. Carrying the selector through

`hostnetwork.UnderlayInterface` gains the selector alongside the name it
resolves to:

```go
type UnderlayInterface struct {
	// InterfaceName is the primary kernel name of the device. For an
	// interface selected by AltName it is filled in at setup time, once
	// the alternative name has been resolved.
	InterfaceName string `json:"interfaceName"`
	// AltName, when set, is the netlink alternative name the device was
	// selected by. It is the identity used for state keying and for
	// reconcile diffing, because InterfaceName is not known before
	// resolution and not stable after it.
	AltName string `json:"altName,omitempty"`
	...
}
```

and a single accessor decides which one identifies the interface:

```go
// Key returns the stable identity of the underlay interface: the
// alternative name it was selected by, or its primary kernel name.
func (u UnderlayInterface) Key() string {
	if u.AltName != "" {
		return u.AltName
	}
	return u.InterfaceName
}
```

`UnderlayInterfacesToRemove` keys its map on `Key()` instead of
`InterfaceName`, and `devicestate.Entry` gains an `AltName` field that is
persisted and used for `filePath`. That is what closes the reconcile-churn
hole: `groutPortToUnderlayInterface` reads `AltName` back out of the state
file and reconstructs an interface whose `Key()` equals the configured one.

`conversion.underlayInterfacesToHost` already de-duplicates and validates
names; it moves to `Key()` for the duplicate check, and validates the
alternative name against the 127-character bound rather than
`isValidInterfaceName`'s 15.

### 3. Port naming

`PortName` currently returns `u_<InterfaceName>`, and
`ValidateGroutUnderlay` requires the result to fit in `IFNAMSIZ` because
grout creates a kernel-visible NOARP interface with that name for FRR. A
127-character altname cannot feed that.

The proposal is to keep the derivation on the *primary* name and require an
explicit `acceleratedConfig.portName` when the selector is an altname:

- At setup time the primary name is known (resolution has happened), so
  `u_<primary>` remains available as the default and nothing changes for
  existing configurations.
- But the port name must also be known to the validating webhook, which runs
  on the controller and cannot resolve an altname on a node it is not
  running on. So for `altName` selectors `ValidateGroutUnderlay` requires
  `acceleratedConfig.portName` to be set, and validates *that* against
  `IFNAMSIZ`.

The alternative — hashing the altname the way `pciAddressToIfName` hashes a
PCI address for VF-pair trunk ports (`internal/grout/l2vni.go:108`) — would
avoid the extra required field at the cost of an opaque `u_a3f91c` interface
in `ip link` output on every node. Requiring an explicit name keeps the
node-side output readable; the hash is the fallback if the extra field turns
out to be a nuisance in practice.

### 4. mlx5 and teardown

`prepareGroutPortDriver`'s mlx5 branch moves the kernel netdev into the
router netns. It receives the primary name and keeps doing so — the netdev
still exists for bifurcated drivers, and its alternative names move with it
into the namespace, so a later lookup inside the router netns resolves
identically.

Teardown (`teardownGroutPortUnderlay`, `restoreIPAddresses`) looks the device
up with `netlink.LinkByName(state.NetlinkName)` after the driver has been
restored. The primary name the kernel picks after a rebind is not guaranteed
to be the one recorded before it, and this is precisely the case altnames fix:
these two call sites should prefer `LinkByAltName(state.AltName)` when an
altname is recorded, falling back to the saved primary name. This is worth
doing even for interfaces selected by `interfaceName`, by recording any
altname the device happened to carry at setup time.

---

## Validation and failure modes

| Case | Behaviour |
|---|---|
| Both `interfaceName` and `altName` set, or neither | Rejected by CEL at admission |
| `altName` set with `acceleratedConfig` but no `portName` | Rejected by `ValidateGroutUnderlay` |
| `altName` does not match any device on a node | Node-level failure at setup. The webhook cannot check this — it has no view of the node's devices — so it surfaces as a reconcile error and an event on the router pod, the same way an unknown `interfaceName` does today |
| `altName` matches two devices | Explicit error naming both primary names, rather than an arbitrary pick |
| Kernel older than 5.5 | `AltNames` comes back empty for every link and the lookup fails with "no device carries the alternative name". Worth a distinct error message that names the kernel requirement |
| Device already bound to `vfio-pci`, state file lost | Unresolvable, as it is today for `interfaceName`. The `pciAddress` selector from Option C is the real fix; out of scope here |

## Testing

- Unit tests for `LinkByAltName` against a netns with `veth` devices carrying
  altnames — `internal/hostnetwork` already runs namespaced tests of this
  shape.
- A `devicestate` round-trip test asserting `AltName` survives save/load and
  that `filePath` is keyed by it.
- A `UnderlayInterfacesToRemove` test with an interface configured by altname
  and reconstructed from state, asserting an empty diff. This is the
  regression test for the churn hole described above; without it the bug is
  invisible in a single reconcile.
- An e2e case in the grout suite that stamps an altname on the underlay NIC
  with `ip link property add`, configures the `Underlay` by altname, and
  asserts the grout port comes up with the expected `devargs`.

## Open questions

- Should `interfaceName` selection also record and prefer altnames on
  teardown, as suggested in §4? It makes teardown more robust at the cost of
  making the two selectors behave slightly differently from what the user
  wrote.
- Is the required `portName` for altname selectors acceptable, or is the
  hashed default preferable? This is the only user-visible ergonomic cost of
  Option B.
- Does the same selector belong on `sriovVFPair` for L2VNI trunk VFs? It has
  a `netlinkName` selector with the same 15-character limit and the same
  cross-node instability, so the argument carries over — but it can follow
  once the underlay side has settled.
