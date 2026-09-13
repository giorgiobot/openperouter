// SPDX-License-Identifier:Apache-2.0

package sriov

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/vishvananda/netlink"
)

var pciAddressRegex = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// SysfsRoot can be overridden in tests.
var SysfsRoot = "/sys"

// IsPCIAddress reports whether s is a PCI BDF address (DDDD:BB:DD.F).
func IsPCIAddress(s string) bool {
	return pciAddressRegex.MatchString(s)
}

// ResolveNetlinkName resolves a kernel network device to its PCI address.
// The name may be the device's primary name or any of its netlink
// alternative names: the kernel resolves both out of a single per-namespace
// name space, so at most one device can match. sysfs is keyed by the primary
// name only, hence the netlink lookup before the sysfs read.
func ResolveNetlinkName(name string) (string, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", fmt.Errorf("failed to find network device %q (alternative names need kernel >= 5.5): %w", name, err)
	}
	return pciAddressForKernelName(link.Attrs().Name)
}

// ResolvePCIAddress validates the PCI address format and checks that the
// device exists in sysfs.
func ResolvePCIAddress(pciAddr string) error {
	if !IsPCIAddress(pciAddr) {
		return fmt.Errorf("invalid PCI address format %q, expected DDDD:BB:DD.F", pciAddr)
	}
	devicePath := filepath.Join(SysfsRoot, "bus", "pci", "devices", pciAddr)
	if _, err := os.Stat(devicePath); err != nil {
		return fmt.Errorf("PCI device %q not found in sysfs: %w", pciAddr, err)
	}
	return nil
}

// ResolvePFVFIndex resolves a PF name and VF index to a PCI address by
// reading the virtfn symlink under the PF's sysfs device directory.
func ResolvePFVFIndex(pfName string, vfIndex int) (string, error) {
	virtfnLink := filepath.Join(SysfsRoot, "class", "net", pfName, "device", fmt.Sprintf("virtfn%d", vfIndex))
	target, err := os.Readlink(virtfnLink)
	if err != nil {
		return "", fmt.Errorf("failed to resolve VF %d on PF %q: %w", vfIndex, pfName, err)
	}
	pciAddr := filepath.Base(target)
	if !IsPCIAddress(pciAddr) {
		return "", fmt.Errorf("resolved VF symlink target %q does not look like a PCI address", pciAddr)
	}
	return pciAddr, nil
}

func pciAddressForKernelName(name string) (string, error) {
	deviceLink := filepath.Join(SysfsRoot, "class", "net", name, "device")
	target, err := os.Readlink(deviceLink)
	if err != nil {
		return "", fmt.Errorf("failed to resolve netlink device %q to PCI address: %w", name, err)
	}
	pciAddr := filepath.Base(target)
	if !IsPCIAddress(pciAddr) {
		return "", fmt.Errorf("resolved device symlink target %q for %q does not look like a PCI address", pciAddr, name)
	}
	return pciAddr, nil
}
