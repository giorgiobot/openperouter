// SPDX-License-Identifier:Apache-2.0

package sriov

import (
	"os"
	"path/filepath"
	"testing"
)

// buildFakeSysfs creates a minimal sysfs tree under t.TempDir() and returns
// the fake /sys/class/net and /sys/bus/pci base paths.
//
// Layout produced:
//
//	<root>/sys/bus/pci/devices/0000:03:10.0/         — VF device dir
//	<root>/sys/bus/pci/devices/0000:03:00.0/         — PF device dir
//	<root>/sys/bus/pci/drivers/iavf/                 — kernel driver dir
//	<root>/sys/bus/pci/drivers/vfio-pci/             — DPDK driver dir
//	<root>/sys/class/net/enp3s0f0v0/device  -> ../../bus/pci/devices/0000:03:10.0
//	<root>/sys/class/net/enp3s0f0v0/device/physfn -> ../0000:03:00.0
//	<root>/sys/class/net/enp3s0f0v0/device/vendor  (contains "0x8086\n")
//	<root>/sys/class/net/enp3s0f0v0/device/driver -> ../../drivers/iavf
//	<root>/sys/class/net/eth0/device -> ../../bus/pci/devices/0000:03:00.0
//	<root>/sys/class/net/eth0/device/vendor  (contains "0x8086\n")
//	(eth0 has NO physfn — it is a PF)
func buildFakeSysfs(t *testing.T) (netDir, pciDir string) {
	t.Helper()
	root := t.TempDir()

	pciDevicesDir := filepath.Join(root, "sys", "bus", "pci", "devices")
	pciDriversDir := filepath.Join(root, "sys", "bus", "pci", "drivers")
	netDir = filepath.Join(root, "sys", "class", "net")

	vfPCIDir := filepath.Join(pciDevicesDir, "0000:03:10.0")
	pfPCIDir := filepath.Join(pciDevicesDir, "0000:03:00.0")
	iavfDriverDir := filepath.Join(pciDriversDir, "iavf")
	vfioPCIDriverDir := filepath.Join(pciDriversDir, "vfio-pci")

	for _, d := range []string{vfPCIDir, pfPCIDir, iavfDriverDir, vfioPCIDriverDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}

	// --- VF interface: enp3s0f0v0 ---
	vfNetDir := filepath.Join(netDir, "enp3s0f0v0")
	if err := os.MkdirAll(vfNetDir, 0o755); err != nil {
		t.Fatal(err)
	}

	// device -> PCI device dir (relative symlink, but we use absolute for simplicity in tests)
	if err := os.Symlink(vfPCIDir, filepath.Join(vfNetDir, "device")); err != nil {
		t.Fatal(err)
	}
	// physfn -> PF PCI device (makes this a VF)
	if err := os.Symlink(pfPCIDir, filepath.Join(vfPCIDir, "physfn")); err != nil {
		t.Fatal(err)
	}
	// vendor file
	if err := os.WriteFile(filepath.Join(vfPCIDir, "vendor"), []byte("0x8086\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// driver -> iavf
	if err := os.Symlink(iavfDriverDir, filepath.Join(vfPCIDir, "driver")); err != nil {
		t.Fatal(err)
	}
	// unbind file (writable in real sysfs)
	if err := os.WriteFile(filepath.Join(iavfDriverDir, "unbind"), nil, 0o200); err != nil {
		t.Fatal(err)
	}
	// bind files for drivers
	if err := os.WriteFile(filepath.Join(vfioPCIDriverDir, "bind"), nil, 0o200); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(iavfDriverDir, "bind"), nil, 0o200); err != nil {
		t.Fatal(err)
	}
	// driver_override
	if err := os.WriteFile(filepath.Join(vfPCIDir, "driver_override"), nil, 0o200); err != nil {
		t.Fatal(err)
	}

	// --- PF interface: eth0 ---
	pfNetDir := filepath.Join(netDir, "eth0")
	if err := os.MkdirAll(pfNetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(pfPCIDir, filepath.Join(pfNetDir, "device")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(pfPCIDir, "vendor"), []byte("0x8086\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	pciDir = filepath.Join(root, "sys", "bus", "pci")
	return netDir, pciDir
}

func TestIsVF(t *testing.T) {
	netDir, _ := buildFakeSysfs(t)
	sysfsNetDir = netDir

	if !IsVF("enp3s0f0v0") {
		t.Error("expected enp3s0f0v0 to be detected as a VF")
	}
	if IsVF("eth0") {
		t.Error("expected eth0 NOT to be detected as a VF")
	}
	if IsVF("nonexistent") {
		t.Error("expected nonexistent interface NOT to be detected as a VF")
	}
}

func TestPCIAddress(t *testing.T) {
	netDir, _ := buildFakeSysfs(t)
	sysfsNetDir = netDir

	addr, err := PCIAddress("enp3s0f0v0")
	if err != nil {
		t.Fatalf("PCIAddress failed: %v", err)
	}
	if addr != "0000:03:10.0" {
		t.Errorf("expected 0000:03:10.0, got %s", addr)
	}
}

func TestVendor(t *testing.T) {
	netDir, _ := buildFakeSysfs(t)
	sysfsNetDir = netDir

	vendor, err := Vendor("enp3s0f0v0")
	if err != nil {
		t.Fatalf("Vendor failed: %v", err)
	}
	if vendor != "0x8086" {
		t.Errorf("expected 0x8086, got %s", vendor)
	}
}

func TestDriverForVendor(t *testing.T) {
	driver, err := DriverForVendor(VendorIntel)
	if err != nil {
		t.Fatalf("DriverForVendor failed: %v", err)
	}
	if driver != DriverVfioPCI {
		t.Errorf("expected %s, got %s", DriverVfioPCI, driver)
	}

	_, err = DriverForVendor("0x15b3")
	if err == nil {
		t.Error("expected error for unsupported vendor")
	}
}

func TestCurrentDriver(t *testing.T) {
	_, pciDir := buildFakeSysfs(t)
	sysfsPCIDir = pciDir

	driver, err := CurrentDriver("0000:03:10.0")
	if err != nil {
		t.Fatalf("CurrentDriver failed: %v", err)
	}
	if driver != "iavf" {
		t.Errorf("expected iavf, got %s", driver)
	}

	// PF has no driver symlink set up in our fake tree
	driver, err = CurrentDriver("0000:03:00.0")
	if err != nil {
		t.Fatalf("CurrentDriver for PF failed: %v", err)
	}
	if driver != "" {
		t.Errorf("expected empty driver for PF without driver link, got %s", driver)
	}
}

func TestIsPCIAddress(t *testing.T) {
	valid := []string{
		"0000:03:10.0",
		"0000:00:1f.7",
		"abcd:ef:01.2",
	}
	for _, s := range valid {
		if !IsPCIAddress(s) {
			t.Errorf("expected %q to be a valid PCI address", s)
		}
	}

	invalid := []string{
		"net_tap0,remote=eth0,iface=tap_eth0",
		"net_tap1,iface=pt-host",
		"not-a-pci-addr",
		"",
		"0000:03:10",
		"0000:03:10.8",
	}
	for _, s := range invalid {
		if IsPCIAddress(s) {
			t.Errorf("expected %q NOT to be a valid PCI address", s)
		}
	}
}
