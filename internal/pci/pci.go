// SPDX-License-Identifier:Apache-2.0

// Package pci resolves kernel netdevs to PCI addresses and manages DPDK
// driver binding for grout-accelerated underlay ports.
package pci

import (
	"fmt"
	"path/filepath"
	"regexp"

	"github.com/vishvananda/netlink"
)

const (
	DriverVFIOPCI  = "vfio-pci"
	DriverMlx5Core = "mlx5_core"
)

// SysfsRoot can be overridden in tests.
var SysfsRoot = "/sys"

var pciAddressRegex = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)
var virtioDeviceRegex = regexp.MustCompile(`^virtio[0-9]+$`)

// IsPCIAddress reports whether s is a PCI BDF address (DDDD:BB:DD.F).
func IsPCIAddress(s string) bool {
	return pciAddressRegex.MatchString(s)
}

// IsBifurcated reports whether the kernel driver shares the device with
// DPDK (e.g. mlx5) instead of requiring a vfio-pci rebind.
func IsBifurcated(driver string) bool {
	return driver == DriverMlx5Core
}

// GetPCIAddressForNetlinkName takes a kernel netlink device name and returns its PCI
// address by reading the "device" symlink under the device's sysfs
// class/net directory. Virtio netdevs have a virtio device below the PCI device.
func GetPCIAddressForNetlinkName(name string) (string, error) {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return "", fmt.Errorf("failed to find network device %q: %w", name, err)
	}
	return pciAddressForKernelName(link.Attrs().Name)
}

func pciAddressForKernelName(name string) (string, error) {
	deviceLink := filepath.Join(SysfsRoot, "class", "net", name, "device")
	target, err := filepath.EvalSymlinks(deviceLink)
	if err != nil {
		return "", fmt.Errorf("failed to resolve netlink device %q to PCI address: %w", name, err)
	}
	pciAddr := filepath.Base(target)
	if virtioDeviceRegex.MatchString(pciAddr) {
		pciAddr = filepath.Base(filepath.Dir(target))
	}
	if !IsPCIAddress(pciAddr) {
		return "", fmt.Errorf("resolved device symlink target %q for %q does not look like a PCI address", pciAddr, name)
	}
	return pciAddr, nil
}
