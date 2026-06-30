// SPDX-License-Identifier:Apache-2.0

package sriov

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// sysfsNetDir is the base path for network device sysfs entries.
// Override in tests to point at a fake tree.
var sysfsNetDir = "/sys/class/net"

// IsVF reports whether the given network interface is an SR-IOV Virtual Function.
// A VF has a "physfn" symlink under its sysfs device directory.
func IsVF(ifaceName string) bool {
	physfnPath := filepath.Join(sysfsNetDir, ifaceName, "device", "physfn")
	_, err := os.Lstat(physfnPath)
	return err == nil
}

// PCIAddress returns the PCI BDF address (e.g. "0000:03:10.0") for the given
// network interface by resolving its sysfs device symlink.
func PCIAddress(ifaceName string) (string, error) {
	devicePath := filepath.Join(sysfsNetDir, ifaceName, "device")
	resolved, err := filepath.EvalSymlinks(devicePath)
	if err != nil {
		return "", fmt.Errorf("failed to resolve sysfs device path for %s: %w", ifaceName, err)
	}
	return filepath.Base(resolved), nil
}

// pciAddrRe matches a PCI BDF address like "0000:03:10.0".
var pciAddrRe = regexp.MustCompile(`^[0-9a-fA-F]{4}:[0-9a-fA-F]{2}:[0-9a-fA-F]{2}\.[0-7]$`)

// IsPCIAddress reports whether s looks like a PCI BDF address.
func IsPCIAddress(s string) bool {
	return pciAddrRe.MatchString(s)
}

// Vendor returns the PCI vendor ID (e.g. "0x8086") for the given network interface.
func Vendor(ifaceName string) (string, error) {
	vendorPath := filepath.Join(sysfsNetDir, ifaceName, "device", "vendor")
	data, err := os.ReadFile(vendorPath)
	if err != nil {
		return "", fmt.Errorf("failed to read vendor for %s: %w", ifaceName, err)
	}
	return strings.TrimSpace(string(data)), nil
}
