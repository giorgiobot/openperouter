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
`/sys/class/net/<interfaceName>/device` (`sriov.ResolveNetlinkName`,
`internal/sriov/vf.go:25`), and the resolved address is what is eventually
handed to grout as `devargs`.

That works, but the primary netlink name is the least stable and least
expressive identifier the kernel offers:

1. **It is capped at 15 characters** (`IFNAMSIZ - 1`). udev's predictable
   names routinely exceed that on multi-port and SR-IOV NICs (for example
   `enp65s0f0npf0vf12`, 17 characters). When they do, udev cannot install the
   predictable name as the primary name; it installs it as an *alternative
   name* and the device keeps a short kernel-assigned name. The stable
   identifier for those NICs is exactly the one the API cannot express.

2. **It is not uniform across nodes.** An `Underlay` is applied to every node
   matching its `nodeSelector`, but the primary name depends on bus
   enumeration order and firmware. Two nodes with the same role but different
   slot population get different names, which forces one `Underlay` object
   per node shape.

3. **It is mutable.** Renaming a device drops the old primary name.
   Alternative names survive the rename.

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

---

## How the kernel treats alternative names

The design below rests on four properties. All four were verified against
Linux 6.18 with a throwaway veth in a private netns, rather than taken from
documentation.

**1. Primary names and alternative names share one flat namespace per netns.**
The kernel keeps altname nodes in the same `dev_name_hash` as primary names,
so a name is either free or taken, regardless of which kind it is. Every
collision is rejected with `EEXIST`, in all four directions:

| Attempt | Result |
|---|---|
| Give device B an altname equal to device A's *primary* name | `EEXIST` |
| Give device B an altname equal to device A's *altname* | `EEXIST` |
| Create a device whose *primary* name equals device A's altname | `EEXIST` |
| Rename device B to device A's altname | `EEXIST` |

**A name therefore cannot be ambiguous.** Resolving a string against primary
names and altnames together can match at most one device — there is no
lookup-order question to answer, because the kernel already made the answer
unique.

**2. `netlink.LinkByName` already resolves altnames, at any length.** This is
not obvious from the library. `LinkByName` sends the name as `IFLA_IFNAME`,
switching to `IFLA_ALT_IFNAME` only above 15 characters
(`link_linux.go:1949` in vishvananda/netlink v1.3.1) — but that switch exists
only because `IFLA_IFNAME` truncates at `IFNAMSIZ`, not because the two
attributes search different namespaces. Both land in `__dev_get_by_name`,
which searches the shared hash. Verified: a 7-character altname and a
37-character altname both resolve through plain `netlink.LinkByName`, each
returning the device under its *primary* name.

**3. sysfs is keyed by the primary name only.** `/sys/class/net/<altname>`
does not exist; altnames have no sysfs representation. This is the one place
where the two kinds of name genuinely differ, and it is the only place the
current code needs to change.

**4. Altnames survive both a rename and a netns move.** After renaming the
primary name, the altname still resolves, now reporting the new primary name.
After `LinkSetNsFd` into another namespace, the altname resolves in the target
namespace with its property list intact.

---

## What the current code assumes

| Location | Assumption |
|---|---|
| `api/v1alpha1/underlay_types.go` — `NetworkDevice.InterfaceName` | `MaxLength=15` |
| `conversion.isValidInterfaceName` (`internal/conversion/validate_vni.go:446`) | rejects names longer than 15 characters |
| `grout.PortName` (`internal/grout/underlay.go:32`) | grout port name is `u_<InterfaceName>` |
| `conversion.ValidateGroutUnderlay` (`internal/conversion/validate_grout.go`) | that port name must stay under `IFNAMSIZ` |
| `sriov.ResolveNetlinkName` (`internal/sriov/vf.go:25`) | the name is a directory under `/sys/class/net` |
| `devicestate.filePath` (`internal/grout/devicestate/devicestate.go:32`) | state file is `<NetlinkName>.json` |
| `grout.groutPortToUnderlayInterface` (`internal/grout/underlay.go:219`) | reconstructs `InterfaceName` from saved state; the result is diffed against the requested list by `hostnetwork.UnderlayInterfacesToRemove` |
| `grout.restoreIPAddresses`, `teardownGroutPortUnderlay` | `netlink.LinkByName(state.NetlinkName)` on teardown |

Two of these deserve to be called out up front, because they are the parts a
naive implementation gets wrong:

**Reconcile churn.** `SetupUnderlay` diffs the requested interfaces against
the ones it reconstructs from grout, matching on `InterfaceName`. The
reconstruction for a PCI-backed port goes through
`devicestate.LoadByPCI(...).NetlinkName`. If the user configures an altname
but the state file records the resolved primary name, every reconcile sees the
configured interface as "new" and the existing one as "removed", and tears the
port down and rebuilds it — flapping the underlay on every sync. The persisted
state must round-trip *the name the user wrote*, not only the one it resolved
to.

**Resolution has a deadline.** Altnames are properties of the netdev, not of
the PCI device. Once the device is bound to `vfio-pci` the netdev is gone and
so are its altnames; from then on nothing on the node can resolve
`perouter-uplink0`. Resolution must happen once, on first setup, before
`prepareGroutPortDriver` rebinds the driver — which is exactly where
`setupGroutPortUnderlay` already resolves and caches the PCI address. The
existing caching shape is right; it needs to cache one more field.

---

## Option A — accept an alternative name in `interfaceName` (recommended)

Keep one field. Resolve it through netlink, which searches primary names and
altnames together, and use the resolved primary name wherever sysfs is
involved. Raise `MaxLength` to 127.

**Pros.** No new API surface, and no new concept for users: this is exactly
how `ip link show dev X` already behaves, so the field matches the tool
operators reach for. Existing manifests are unaffected — a primary name still
resolves to the same device, because property 1 guarantees no altname can
shadow it. The same change to `sriov.ResolveNetlinkName` gives the L2VNI
`sriovVFPair.netlinkName` selector the same capability for free.

**Cons.** Raising `MaxLength` is a one-way door: it cannot be tightened later
without breaking whoever relied on it. A name that resolves on one node and
not another produces a per-node failure that the spec alone cannot explain —
though that is already true of `interfaceName` today.

## Option B — add a separate `altName` selector

A sibling field to `interfaceName`, mutually exclusive via CEL, mirroring the
`pciAddress` | `pfName`+`vfIndex` | `netlinkName` union that
`SRIOVVFPairConfig` already uses for the L2VNI trunk VF.

This was the original recommendation of this document, on the grounds that
overloading one field made the selector ambiguous. **Property 1 disproves
that**: the kernel will not let the ambiguous case exist, so the explicit
selector buys nothing but a second field to document, validate, convert and
test. Its only remaining advantage is that a distinct field makes the
127-character bound apply only to the new selector, leaving `interfaceName`'s
15-character bound intact for the kernel-name case — which matters only if
that bound is worth preserving for its own sake.

Rejected, unless the `MaxLength` widening in Option A is judged unacceptable.

## Option C — a full `deviceSelector` union

Replace `interfaceName` with a nested union offering `interfaceName`,
`pciAddress` and `pfName`+`vfIndex`, unifying underlay device selection with
the L2VNI VF-pair selector. `pciAddress` is genuinely valuable for accelerated
ports because it is the only identifier that survives a `vfio-pci` rebind and
so needs no cached state at all.

Worth doing eventually. It breaks every existing `Underlay` and needs a
conversion webhook, and none of that is coupled to the altname question, so it
should not ride along with this change. Option A does not obstruct it.

## Recommendation

**Option A.** With ambiguity ruled out by the kernel, the explicit selector of
Option B is cost without benefit, and the behaviour users get from Option A is
the behaviour `ip link` already taught them to expect.

---

## Implementation

### 1. Resolve through netlink, then use the primary name for sysfs

No altname-specific lookup helper is needed (property 2). The only real change
is that `sriov.ResolveNetlinkName` must stop assuming its argument is a sysfs
directory name:

```go
// ResolveNetlinkName resolves a kernel network device to its PCI address.
// The name may be the device's primary name or any of its netlink
// alternative names; the kernel resolves both from a single namespace.
// sysfs is keyed by the primary name only, so the name is resolved through
// netlink first.
func ResolveNetlinkName(name string) (string, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", fmt.Errorf("failed to find network device %q: %w", name, err)
	}
	return resolvePCIForKernelName(link.Attrs().Name)
}
```

where `resolvePCIForKernelName` is today's body, reading the `device` symlink
under `/sys/class/net/<primary>`.

This also removes a latent failure that has nothing to do with altnames: a
device whose primary name is longer than 15 characters cannot be named in the
API at all today, and after this change it can.

Two caveats worth recording rather than working around:

- Altname *creation* needs kernel >= 5.5. On an older kernel the resolution
  simply finds nothing, and the error should name the kernel requirement so
  the failure is self-explanatory.
- vishvananda/netlink falls back to `linkByNameDump` when `RTM_GETLINK` returns
  `EINVAL`, and that fallback compares primary names only. It triggers on
  kernels far older than 5.5, so it cannot be reached by a working altname
  setup.

### 2. Keep the configured name as the identity

`hostnetwork.UnderlayInterface.InterfaceName` keeps holding whatever the user
configured — that string stays the identity used for state keying and for the
reconcile diff. The resolved primary name is a runtime detail recorded
alongside it:

```go
type UnderlayInterface struct {
	// InterfaceName is the device name as configured: either the primary
	// kernel name or one of the device's netlink alternative names.
	InterfaceName string `json:"interfaceName"`
	// KernelName is the primary kernel name InterfaceName resolved to. It
	// is filled in at setup time and differs from InterfaceName only when
	// the device was selected by an alternative name.
	KernelName string `json:"kernelName,omitempty"`
	...
}
```

`devicestate.Entry` gains the same `KernelName` field, persisted next to the
PCI address. `filePath` stays keyed on `NetlinkName` (the configured string),
so `groutPortToUnderlayInterface` reads back an interface whose
`InterfaceName` equals the configured one and the diff stays empty. That is
what closes the churn hole; no `Key()` indirection is needed.

Teardown (`restoreIPAddresses`, `teardownGroutPortUnderlay`) should keep
looking the device up by `InterfaceName` rather than by the recorded
`KernelName`: after a driver rebind the kernel may pick a different primary
name, and the altname is the identifier that survives it (property 4). For
devices configured by primary name the two are the same string, so this is one
code path, not two.

### 3. Port naming

`PortName` returns `u_<InterfaceName>` and `ValidateGroutUnderlay` requires the
result to fit in `IFNAMSIZ`, because grout creates a kernel-visible NOARP
interface with that name for FRR. A 127-character name cannot feed that.

The webhook cannot resolve names — it runs on the controller, not on the node
holding the NIC — so the rule has to be decidable from the spec alone:

> if `len(interfaceName) > IFNAMSIZ - len("u_") - 1`, then
> `acceleratedConfig.portName` is required.

Short altnames (`uplink0`) keep working with no extra field; only long ones
pay for their length, and they pay at admission time with a clear message
rather than on one node at reconcile time. Hashing the name the way
`pciAddressToIfName` hashes a PCI address (`internal/grout/l2vni.go:108`)
would avoid the extra field at the cost of an opaque `u_a3f91c` in `ip link`
output on every node; the explicit name keeps node-side output readable.

### 4. Validation bounds

`isValidInterfaceName` (`internal/conversion/validate_vni.go:446`) is shared
with VRF-name validation, and VRF names are real kernel interfaces that must
stay within `IFNAMSIZ`. Its 15-character bound must not be relaxed globally;
the underlay device name needs its own check against 127. The
`^[a-zA-Z][a-zA-Z0-9._-]*$` pattern is stricter than the kernel's
`dev_valid_name` and can stay as it is.

---

## Failure modes

| Case | Behaviour |
|---|---|
| Name matches nothing on a node | Node-level reconcile error and an event, as an unknown `interfaceName` produces today. The webhook has no view of node devices and cannot pre-empt it |
| Name matches two devices | Impossible — see property 1 |
| Long name without `acceleratedConfig.portName` | Rejected at admission |
| Kernel older than 5.5 | The device carries no altnames, so resolution fails as "not found". The error should name the kernel requirement |
| Device already bound to `vfio-pci`, state file lost | Unresolvable, exactly as today. The `pciAddress` selector of Option C is the real fix; out of scope here |

## Testing

- `sriov.ResolveNetlinkName` against a netns holding a veth with both a short
  and a long altname, asserting all three names resolve to the same PCI
  address. The probe used to establish the properties above is directly
  reusable as this test.
- A `devicestate` round-trip asserting `KernelName` survives save/load and
  that `filePath` is keyed on the configured name.
- A `UnderlayInterfacesToRemove` case with an interface configured by altname
  and reconstructed from state, asserting an empty diff. Without this the
  churn bug is invisible in a single reconcile.
- An e2e case in the grout suite that stamps an altname on the underlay NIC,
  configures the `Underlay` by that altname, and asserts the grout port comes
  up with the expected `devargs`.

## Open questions

- Is widening `interfaceName` to 127 characters acceptable? It is the only
  irreversible part of Option A, and the only reason to prefer Option B.
- Required `portName` for long names, or a hashed default?
- Should `sriovVFPair.netlinkName` get the same treatment in this change? It
  inherits the capability automatically from §1, so the question is only
  whether to document and test it here or in a follow-up.
