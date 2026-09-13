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
`/sys/class/net/<interfaceName>/device` (`pci.ResolveNetlinkName`,
`internal/pci/pci.go`), and the resolved address is handed to grout as
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

Properties 2 and 3 settle the read path: every name lookup in the accelerated
path already goes through `netlink.LinkByName` and therefore already accepts
altnames, and sysfs is the single exception. What they do not cover is that
none of this survives the driver rebind — the names live on the netdev, which
`vfio-pci` destroys — so the names have to be saved and put back, which is
the other half of the change.

---

## The change

Two parts: resolving a name that may be an alternative name, and putting the
alternative names back when the underlay is torn down.

### 1. Resolve through netlink, then use the primary name for sysfs

`pci.ResolveNetlinkName` stops assuming its argument names a sysfs
directory:

```go
func ResolveNetlinkName(name string) (string, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", fmt.Errorf("failed to find network device %q (alternative names need kernel >= 5.5): %w", name, err)
	}
	return pciAddressForKernelName(link.Attrs().Name)
}
```

`pciAddressForKernelName` is the previous body, reading the `device` symlink
under `/sys/class/net/<primary>`. No altname-specific lookup helper is needed
(property 2).

This also lifts a limitation unrelated to altnames: a device whose *primary*
name is longer than 15 characters could not be resolved before, even though
the kernel allows the lookup. It stays unusable through the API's own bound,
but the resolver no longer adds a second reason.

`sriovVFPair.netlinkName` on L2VNI (`internal/grout/l2vni.go`) calls the
same function and inherits the capability with no further work.

### 2. Save the alternative names, and restore them on teardown

Alternative names are properties of the netdev, not of the PCI device.
Binding to `vfio-pci` destroys the netdev and its whole property list; the
rebind on teardown builds a brand new one. udev re-applies only the names
that come from one of its rules, so a name added by hand — or by a rule that
has since changed — would be lost for good on the first teardown, and the
device could no longer be selected by the name the `Underlay` names.

So the names are treated exactly like the driver binding and the IP
addresses, which this code already saves and restores: `devicestate.Entry`
gains an `AltNames []string`, scraped from `link.Attrs().AltNames` in
`initializeAcceleratedDeviceState` next to the MTU, and teardown puts them
back. The TAP underlay path is untouched: it never rebinds the driver, so its
netdev — and its property list — survives.

Ordering matters on the way out. The configured name may itself be one of the
alternative names, so nothing can find the device by it until the names are
back. That makes the PCI address the only usable handle at that moment, which
is why `restoredLink` looks the device up with `pci.GetPCINetDevice` rather
than by name:

```go
func restoreKernelDevice(ctx context.Context, state devicestate.Entry) error {
	if len(state.AltNames) == 0 && len(state.Addresses) == 0 {
		return nil
	}
	link, err := restoredLink(state)
	if err != nil {
		return err
	}
	if err := restoreAltNames(ctx, link, state.AltNames); err != nil {
		return err
	}
	if state.MTU > 0 {
		if err := netlink.LinkSetMTU(link, int(state.MTU)); err != nil {
			return fmt.Errorf(...)
		}
	}
	return restoreIPAddresses(ctx, link, state.Addresses)
}
```

Two properties fall out of the kernel behaviour above. A name already present
on the device is skipped, so a udev rule that got there first is not fought
over. And because names are unique across both kinds (property 1), an
`EEXIST` means some other device claimed the name while this one was bound to
grout: that is logged and skipped rather than failing the teardown, since the
remaining names and the addresses still need restoring.

No wait loop guards the lookup. The netdev may not be probed the instant the
rebind returns, and the reconcile loop is the retry: returning the error
leaves the state file in place and the next pass tries again.

Restoring the addresses now takes the link rather than looking it up again by
`state.InterfaceName`, which fixes the same latent bug for them — after a
rebind that name is not guaranteed to resolve.

### What deliberately does not change

Each of these looked like it needed work and does not. Recording why, so the
next reader does not redo the analysis:

| Area | Why it is already correct |
|---|---|
| `interfaceName` bounds and pattern | Unchanged at `MaxLength=15`; `isValidInterfaceName` (`internal/conversion/validate_vni.go:446`) is shared with VRF-name validation and must keep its bound anyway |
| Grout port naming | `PortName` returns `u_<InterfaceName>` and `ValidateGroutUnderlay` already rejects a result reaching `IFNAMSIZ`. A name over 13 characters already requires `acceleratedConfig.portName`, altname or not |
| `devicestate` keying | `initializeAcceleratedDeviceState` keys the entry on `iface.InterfaceName`, the configured string, and `LoadByPCI` returns it unchanged. The configured name already round-trips. `AltNames` is added as a new field; `InterfaceName` keeps holding the configured string |
| Address scraping, MTU read, mlx5 netns move | `AddressesForInterface` and `MoveInterfaceToNamespace` resolve via `netlink.LinkByName` (property 2), and the altname survives the move (property 4), so the bifurcated mlx5 path needs no restore at all |
| `sysctl` calls in the accelerated path | `configureAcceleratedPort` passes the *grout port* name, which is a real primary name, not the configured one |

---

## The trap

**Do not overwrite `InterfaceName` with the resolved primary name.** The
reconcile diff depends on it. `SetupUnderlay` compares requested interfaces
against ones reconstructed by `groutPortToUnderlayInterface`
(`internal/grout/underlay.go`), which reads `InterfaceName` back out of the
state file and matches on it. Today that field holds the configured string, so
the diff is empty and nothing churns. The natural-looking "improvement" of
storing the resolved primary name there instead would make every reconcile see
the configured interface as new and the existing one as removed, tearing the
port down and rebuilding it — flapping the underlay on every sync. The primary
name is not needed there: teardown reaches the device by PCI address.

Relatedly, resolution has a deadline: once the device is bound to `vfio-pci`
nothing on the node can resolve the name, so it must be resolved on first
setup, before `prepareAcceleratedDriver` rebinds the driver. That is already
where `initializeAcceleratedDeviceState` resolves and caches the PCI address, so no
change is needed — but it is why the cache exists and must not be bypassed.

---

## Failure modes

| Case | Behaviour |
|---|---|
| Name matches nothing on a node | Node-level reconcile error and an event, exactly as an unknown `interfaceName` produces today |
| Name matches two devices | Impossible — property 1 |
| Altname longer than 15 characters | Rejected at admission by the existing `MaxLength`. Accepted limitation |
| Name over 13 characters without `acceleratedConfig.portName` | Rejected by `ValidateGroutUnderlay`, as today |
| Kernel older than 5.5 | The device carries no altnames, so resolution fails as "not found"; the error names the kernel requirement |
| Netdev not yet probed when teardown restores it | Teardown returns an error, the state file survives, and the next reconcile retries |
| Saved altname taken by another device during teardown | Logged and skipped; the remaining names and the addresses are still restored |

## Testing

- `TestResolveNetlinkNameAcceptsAltNames` (`internal/pci/altname_runasroot_test.go`) builds a veth with a
  short and a long alternative name in a private netns, points `SysfsRoot` at
  a tree where the primary name is backed by a PCI address, and asserts all
  three names resolve to it.
- `TestRestoreAltNames` (`internal/grout/altname_runasroot_test.go`) covers restoring names the device lost,
  idempotency when udev got there first, and skipping a name another device
  has taken.
- The `devicestate` round-trip test asserts `AltNames` survives save/load.
- Still worth adding: a `UnderlayInterfacesToRemove` case with an interface
  configured by altname and reconstructed from device state, asserting an
  empty diff — the regression test for the trap above; and an e2e case in the
  grout suite that stamps an altname on the underlay NIC, configures the
  `Underlay` by it, and asserts the grout port comes up with the expected
  `devargs`.

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

**Requiring the altname to come from a udev rule** instead of saving and
restoring it, on the grounds that udev re-applies its rules when the netdev
comes back. Rejected: it silently downgrades any hand-added name to something
that works until the first teardown, and it makes correctness depend on host
configuration this code cannot see. A udev rule is still the right way to get
the *same* name onto every node — it is just no longer load-bearing for
teardown.

**A full `deviceSelector` union** offering `interfaceName`, `pciAddress` and
`pfName`+`vfIndex`, unifying underlay selection with the L2VNI VF-pair
selector. Worth doing eventually — `pciAddress` is the only identifier that
survives a `vfio-pci` rebind, so it would need no cached state at all — but it
breaks every existing `Underlay` and needs a conversion webhook, and none of
that is coupled to the altname question. This change does not obstruct it.
