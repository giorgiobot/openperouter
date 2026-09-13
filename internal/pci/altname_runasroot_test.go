//go:build runasroot

// SPDX-License-Identifier:Apache-2.0

package pci

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// TestResolveNetlinkNameAcceptsAltNames covers the reason the netlink lookup
// exists at all: sysfs is keyed by the primary name, so an alternative name
// only resolves if the device is looked up through netlink first. Both a
// short alternative name, which the netlink library sends as IFLA_IFNAME,
// and a long one, which it sends as IFLA_ALT_IFNAME, must work.
func TestResolveNetlinkNameAcceptsAltNames(t *testing.T) {
	const (
		primaryName = "probe0"
		shortAlt    = "pe-uplink0"
		longAlt     = "enp65s0f0npf0vf12-uplink"
		pciAddress  = "0000:01:00.0"
	)

	defer inProbeNetns(t)()

	require.NoError(t, netlink.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: primaryName},
		PeerName:  "probe0peer",
	}))
	link, err := netlink.LinkByName(primaryName)
	require.NoError(t, err)
	for _, altName := range []string{shortAlt, longAlt} {
		require.NoError(t, netlink.LinkAddAltName(link, altName))
	}

	fakeSysfsWithPCIDevice(t, primaryName, pciAddress)

	// Not subtests: a network namespace is a property of the OS thread, and
	// t.Run would hand the body to a goroutine free to run on another one.
	for _, name := range []string{primaryName, shortAlt, longAlt} {
		resolved, err := ResolveNetlinkName(name)
		require.NoError(t, err, "resolving %q", name)
		assert.Equal(t, pciAddress, resolved, "resolving %q", name)
	}
}

func TestResolveNetlinkNameUnknownDevice(t *testing.T) {
	defer inProbeNetns(t)()

	_, err := ResolveNetlinkName("does-not-exist")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does-not-exist")
}

// fakeSysfsWithPCIDevice points SysfsRoot at a tree where the given netdev
// name is backed by the given PCI address, mirroring the "device" symlink
// the kernel exposes under /sys/class/net.
func fakeSysfsWithPCIDevice(t *testing.T, netdevName, pciAddress string) {
	t.Helper()

	root := t.TempDir()
	deviceDir := filepath.Join(root, "devices", "pci0000:00", pciAddress)
	require.NoError(t, os.MkdirAll(deviceDir, 0o755))
	netDir := filepath.Join(root, "class", "net", netdevName)
	require.NoError(t, os.MkdirAll(netDir, 0o755))
	require.NoError(t, os.Symlink(deviceDir, filepath.Join(netDir, "device")))

	origRoot := SysfsRoot
	SysfsRoot = root
	t.Cleanup(func() { SysfsRoot = origRoot })
}

// inProbeNetns moves the calling goroutine into a fresh network namespace and
// returns the function that puts it back.
func inProbeNetns(t *testing.T) func() {
	t.Helper()

	runtime.LockOSThread()
	origNS, err := netns.Get()
	require.NoError(t, err)

	probeNS, err := netns.New()
	require.NoError(t, err)

	return func() {
		assert.NoError(t, netns.Set(origNS))
		assert.NoError(t, probeNS.Close())
		assert.NoError(t, origNS.Close())
		runtime.UnlockOSThread()
	}
}
