// SPDX-License-Identifier:Apache-2.0

package sriov

import (
	"fmt"
	"os"
	"path/filepath"
)

const (
	VendorIntel = "0x8086"

	DriverVfioPCI = "vfio-pci"
)

// sysfsPCIDir is the base path for PCI device sysfs entries.
// Override in tests to point at a fake tree.
var sysfsPCIDir = "/sys/bus/pci"

// DriverForVendor returns the DPDK driver to use for a given PCI vendor ID.
func DriverForVendor(vendorID string) (string, error) {
	switch vendorID {
	case VendorIntel:
		return DriverVfioPCI, nil
	default:
		return "", fmt.Errorf("unsupported SR-IOV vendor %s", vendorID)
	}
}

// CurrentDriver returns the name of the kernel driver currently bound to the
// PCI device, or "" if no driver is bound.
func CurrentDriver(pciAddr string) (string, error) {
	driverPath := filepath.Join(sysfsPCIDir, "devices", pciAddr, "driver")
	resolved, err := filepath.EvalSymlinks(driverPath)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("failed to resolve driver for %s: %w", pciAddr, err)
	}
	return filepath.Base(resolved), nil
}

// BindDriver unbinds a PCI device from its current driver and binds it to the
// target DPDK-compatible driver.
func BindDriver(pciAddr, targetDriver string) error {
	current, err := CurrentDriver(pciAddr)
	if err != nil {
		return fmt.Errorf("failed to read current driver for %s: %w", pciAddr, err)
	}

	if current == targetDriver {
		return nil
	}

	if current != "" {
		unbindPath := filepath.Join(sysfsPCIDir, "devices", pciAddr, "driver", "unbind")
		if err := os.WriteFile(unbindPath, []byte(pciAddr), 0o200); err != nil {
			return fmt.Errorf("failed to unbind %s from %s: %w", pciAddr, current, err)
		}
	}

	overridePath := filepath.Join(sysfsPCIDir, "devices", pciAddr, "driver_override")
	if err := os.WriteFile(overridePath, []byte(targetDriver), 0o200); err != nil {
		return fmt.Errorf("failed to set driver_override for %s: %w", pciAddr, err)
	}

	bindPath := filepath.Join(sysfsPCIDir, "drivers", targetDriver, "bind")
	if err := os.WriteFile(bindPath, []byte(pciAddr), 0o200); err != nil {
		return fmt.Errorf("failed to bind %s to %s: %w", pciAddr, targetDriver, err)
	}

	return nil
}

// RestoreDefaultDriver unbinds a PCI device from its current (DPDK) driver,
// clears driver_override, and triggers a kernel re-probe so the device binds
// back to its default kernel driver.
func RestoreDefaultDriver(pciAddr string) error {
	current, err := CurrentDriver(pciAddr)
	if err != nil {
		return fmt.Errorf("failed to read current driver for %s: %w", pciAddr, err)
	}

	if current != "" {
		unbindPath := filepath.Join(sysfsPCIDir, "devices", pciAddr, "driver", "unbind")
		if err := os.WriteFile(unbindPath, []byte(pciAddr), 0o200); err != nil {
			return fmt.Errorf("failed to unbind %s from %s: %w", pciAddr, current, err)
		}
	}

	overridePath := filepath.Join(sysfsPCIDir, "devices", pciAddr, "driver_override")
	if err := os.WriteFile(overridePath, []byte("\x00"), 0o200); err != nil {
		return fmt.Errorf("failed to clear driver_override for %s: %w", pciAddr, err)
	}

	probePath := filepath.Join(sysfsPCIDir, "drivers_probe")
	if err := os.WriteFile(probePath, []byte(pciAddr), 0o200); err != nil {
		return fmt.Errorf("failed to probe driver for %s: %w", pciAddr, err)
	}

	return nil
}
