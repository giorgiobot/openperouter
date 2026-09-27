// SPDX-License-Identifier:Apache-2.0

// Package pci resolves kernel netdevs to PCI addresses and manages DPDK
// driver binding for grout-accelerated underlay ports.
package pci

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"

	"github.com/vishvananda/netlink"
)

const (
	DriverVFIOPCI  = "vfio-pci"
	DriverMlx5Core = "mlx5_core"
)

var IntelKernelDrivers = map[string]bool{
	"igb":  true,
	"iavf": true,
	"ice":  true,
	"i40e": true,
}

// SysfsRoot can be overridden in tests.
var SysfsRoot = "/sys"

var pciAddressRegex = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// IsPCIAddress reports whether s is a PCI BDF address (DDDD:BB:DD.F).
func IsPCIAddress(s string) bool {
	return pciAddressRegex.MatchString(s)
}

// IsBifurcated reports whether the kernel driver shares the device with
// DPDK (e.g. mlx5) instead of requiring a vfio-pci rebind.
func IsBifurcated(driver string) bool {
	return driver == DriverMlx5Core
}

// ResolveNetlinkName resolves a kernel network device to its PCI address.
// The name may be the device's primary name or any of its netlink
// alternative names: the kernel resolves both out of a single per-namespace
// name space, so at most one device can match. sysfs is keyed by the primary
// name only, hence the netlink lookup before the sysfs read.
func ResolveNetlinkName(name string) (string, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", fmt.Errorf("failed to find network device %q: %w", name, err)
	}
	return pciAddressForKernelName(link.Attrs().Name)
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
