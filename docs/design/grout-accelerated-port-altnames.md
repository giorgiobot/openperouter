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
`internal/sriov/vf.go:25`), and the resolved address is handed to grout as
`devargs`.

The primary kernel name is a poor fleet-wide identifier. An `Underlay` applies
to every node matching its `nodeSelector`, but the primary name depends on bus
enumeration order and firmware, so two nodes with the same role but different
slot population get different names — forcing one `Underlay` object per node
shape. The name is also mutable: renaming a device drops it.

Netlink alternative names ("altnames", `IFLA_PROP_LIST` / `IFLA_ALT_IFNAME`,
kernel >= 5.5) fix both. A device can carry any number of them, they survive
renames, and an administrator can stamp a role-based one on the right NIC of
every node with a single udev rule:

```
# /etc/udev/rules.d/70-perouter.rules
SUBSYSTEM=="net", ACTION=="add", ATTRS{address}=="b8:ce:f6:*", \
  PROGRAM="/sbin/ip link property add dev $name altname pe-uplink0"
```

after which one `Underlay` selects the correct NIC fleet-wide:

```yaml
interfaces:
- type: NetworkDevice
  networkDevice:
    interfaceName: pe-uplink0
    acceleratedConfig:
      rxQueues: 2
```

**Scope.** `interfaceName` keeps its 15-character bound. Altnames may be up to
127 characters, so altnames longer than 15 characters remain unselectable.
This is an accepted limitation: the uniform-naming problem above is solved by
a short administrator-chosen name, and keeping the bound leaves the API's
existing length guarantees — and everything downstream that depends on
them — untouched. See "Alternatives considered".

---

## How the kernel treats alternative names

The design rests on four properties. All four were verified against Linux 6.18
with a throwaway veth in a private netns, rather than taken from
documentation.

**1. Primary names and alternative names share one flat namespace per netns.**
The kernel keeps altname nodes in the same `dev_name_hash` as primary names,
so a name is either free or taken regardless of its kind. Every collision is
rejected with `EEXIST`, in all four directions:

| Attempt | Result |
|---|---|
| Give device B an altname equal to device A's *primary* name | `EEXIST` |
| Give device B an altname equal to device A's *altname* | `EEXIST` |
| Create a device whose *primary* name equals device A's altname | `EEXIST` |
| Rename device B to device A's altname | `EEXIST` |

A name therefore cannot be ambiguous: resolving a string against primary names
and altnames together matches at most one device. There is no lookup-order
question to answer, because the kernel already made the answer unique.

**2. `netlink.LinkByName` already resolves altnames, at any length.** This is
not obvious from the library. `LinkByName` sends the name as `IFLA_IFNAME`,
switching to `IFLA_ALT_IFNAME` only above 15 characters (`link_linux.go:1949`
in vishvananda/netlink v1.3.1) — but that switch exists only because
`IFLA_IFNAME` truncates at `IFNAMSIZ`, not because the two attributes search
different namespaces. Both land in `__dev_get_by_name`, which searches the
shared hash. Verified: a 7-character and a 37-character altname both resolve
through plain `netlink.LinkByName`, each returning the device under its
*primary* name.

**3. sysfs is keyed by the primary name only.** `/sys/class/net/<altname>`
does not exist; altnames have no sysfs representation.

**4. Altnames survive a rename and a netns move.** After renaming the primary
name the altname still resolves, reporting the new primary name. After
`LinkSetNsFd` the altname resolves in the target namespace with its property
list intact.

Properties 2 and 3 together are the whole design: every name lookup in the
accelerated path already goes through `netlink.LinkByName` and therefore
already accepts altnames. sysfs is the single exception.

---

## The change

One function. `sriov.ResolveNetlinkName` must stop assuming its argument names
a sysfs directory:

```go
// ResolveNetlinkName resolves a kernel network device to its PCI address.
// The name may be the device's primary name or any of its netlink
// alternative names; the kernel resolves both from a single namespace.
// sysfs is keyed by the primary name only, so the device is resolved
// through netlink first.
func ResolveNetlinkName(name string) (string, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", fmt.Errorf("failed to find network device %q: %w", name, err)
	}
	return pciAddressForKernelName(link.Attrs().Name)
}
```

where `pciAddressForKernelName` is today's body, reading the `device` symlink
under `/sys/class/net/<primary>`.

This also lifts a limitation unrelated to altnames: a device whose *primary*
name is longer than 15 characters is unusable today even though the kernel
allows the lookup. It stays unusable through the API's own bound, but the
resolver no longer adds a second reason.

`sriovVFPair.netlinkName` on L2VNI (`internal/grout/l2vni.go:129`) calls the
same function and inherits the capability with no further work.

### What deliberately does not change

Each of these looked like it needed work and does not. Recording why, so the
next reader does not redo the analysis:

| Area | Why it is already correct |
|---|---|
| `interfaceName` bounds and pattern | Unchanged at `MaxLength=15`; `isValidInterfaceName` (`internal/conversion/validate_vni.go:446`) is shared with VRF-name validation and must keep its bound anyway |
| Grout port naming | `PortName` returns `u_<InterfaceName>` and `ValidateGroutUnderlay` already rejects a result reaching `IFNAMSIZ`. A name over 13 characters already requires `acceleratedConfig.portName`, altname or not |
| `devicestate` keying | `setupGroutPortUnderlay` keys the entry on `iface.InterfaceName`, the configured string, and `LoadByPCI` returns it unchanged. The configured name already round-trips |
| Address scraping, MTU read, mlx5 netns move, teardown lookups | `AddressesForInterface`, `MoveInterfaceToNamespace` and `restoreIPAddresses` all resolve via `netlink.LinkByName` (property 2), and the altname survives the move (property 4) |
| `sysctl` calls in the accelerated path | `configureGroutPort` passes the *grout port* name, which is a real primary name, not the configured one |

---

## Two traps

**Do not overwrite `NetlinkName` with the resolved primary name.** The
reconcile diff depends on it. `SetupUnderlay` compares requested interfaces
against ones reconstructed by `groutPortToUnderlayInterface`
(`internal/grout/underlay.go:219`), which reads `NetlinkName` back out of the
state file and matches on it. Today that field holds the configured string, so
the diff is empty and nothing churns. The natural-looking "improvement" of
storing the resolved primary name there instead would make every reconcile see
the configured interface as new and the existing one as removed, tearing the
port down and rebuilding it — flapping the underlay on every sync. If the
primary name is ever needed in the state file, it belongs in a new field.

**Altnames must come from a udev rule, not an imperative command.** Teardown
restores the original driver, which destroys the vfio-bound device and creates
a fresh netdev. A fresh netdev has no altnames; udev re-applies its rules on
the `ADD` uevent, but a one-shot `ip link property add` run by an
administrator is never re-run. An imperatively-added altname therefore
disappears on the first driver-restore cycle and `restoreIPAddresses` can no
longer find the device. The udev rule shown above is not only a convenience
for fleet uniformity — it is what makes the name survive the rebind. This
belongs in the user-facing documentation, not only here.

Relatedly, resolution has a deadline: altnames are netdev properties, not PCI
properties, so once the device is bound to `vfio-pci` nothing on the node can
resolve the name. Resolution must happen on first setup, before
`prepareGroutPortDriver` rebinds the driver. That is already where
`setupGroutPortUnderlay` resolves and caches the PCI address, so no change is
needed — but it is why the cache exists and must not be bypassed.

---

## Failure modes

| Case | Behaviour |
|---|---|
| Name matches nothing on a node | Node-level reconcile error and an event, exactly as an unknown `interfaceName` produces today |
| Name matches two devices | Impossible — property 1 |
| Altname longer than 15 characters | Rejected at admission by the existing `MaxLength`. Accepted limitation |
| Name over 13 characters without `acceleratedConfig.portName` | Rejected by `ValidateGroutUnderlay`, as today |
| Kernel older than 5.5 | The device carries no altnames, so resolution fails as "not found". The error should name the kernel requirement so the failure explains itself |
| Altname added imperatively, then a driver-restore cycle | Device unfindable on teardown. See the second trap |

## Testing

- `sriov.ResolveNetlinkName` against a netns holding a veth with an altname,
  asserting both names resolve to the same device. The probe used to establish
  the four properties above is directly reusable here.
- A `UnderlayInterfacesToRemove` case with an interface configured by altname
  and reconstructed from device state, asserting an empty diff. This is the
  regression test for the first trap; without it the bug is invisible in a
  single reconcile.
- An e2e case in the grout suite that stamps an altname on the underlay NIC,
  configures the `Underlay` by that altname, and asserts the grout port comes
  up with the expected `devargs`.

## Alternatives considered

**A separate `altName` field**, mutually exclusive with `interfaceName` via
CEL, mirroring the selector union in `SRIOVVFPairConfig`. This was the first
proposal here, justified by ambiguity between the two kinds of name — which
property 1 disproves. Its one surviving advantage is that a distinct field
could carry a 127-character bound without touching `interfaceName`'s. Rejected
together with the widening itself.

**Widening `interfaceName` to 127 characters**, which would make long altnames
selectable. Rejected: it is irreversible, and it would push the length
question into `isValidInterfaceName`, the grout port-name derivation and the
webhook, in exchange for a case a short administrator-chosen altname already
covers.

**A full `deviceSelector` union** offering `interfaceName`, `pciAddress` and
`pfName`+`vfIndex`, unifying underlay selection with the L2VNI VF-pair
selector. Worth doing eventually — `pciAddress` is the only identifier that
survives a `vfio-pci` rebind, so it would need no cached state at all — but it
breaks every existing `Underlay` and needs a conversion webhook, and none of
that is coupled to the altname question. This change does not obstruct it.
