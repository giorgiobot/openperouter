//go:build runasroot

// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

// TestRestoreAltNames covers the teardown guarantee: rebinding the original
// driver builds a fresh netdev with an empty property list, so the names the
// device carried before it was handed to grout have to be put back by hand.
func TestRestoreAltNames(t *testing.T) {
	const deviceName = "probe0"

	t.Run("restores names the device no longer has", func(t *testing.T) {
		defer inProbeNetns(t)()
		link := addProbeVeth(t, deviceName)

		saved := []string{"pe-uplink0", "enp65s0f0npf0vf12-uplink"}
		require.NoError(t, restoreAltNames(context.Background(), link, saved))

		assert.ElementsMatch(t, saved, altNamesOf(t, deviceName))
	})

	t.Run("is idempotent when udev already restored them", func(t *testing.T) {
		defer inProbeNetns(t)()
		link := addProbeVeth(t, deviceName)

		saved := []string{"pe-uplink0"}
		require.NoError(t, netlink.LinkAddAltName(link, saved[0]))

		link, err := netlink.LinkByName(deviceName)
		require.NoError(t, err)
		require.NoError(t, restoreAltNames(context.Background(), link, saved))

		assert.Equal(t, saved, altNamesOf(t, deviceName))
	})

	// Names are unique across primary and alternative names alike, so a name
	// taken by another device cannot be restored. That must not abort the
	// teardown of everything else.
	t.Run("skips a name another device has taken", func(t *testing.T) {
		defer inProbeNetns(t)()
		link := addProbeVeth(t, deviceName)
		addProbeVeth(t, "squatter0")

		squatter, err := netlink.LinkByName("squatter0")
		require.NoError(t, err)
		require.NoError(t, netlink.LinkAddAltName(squatter, "pe-uplink0"))

		require.NoError(t, restoreAltNames(context.Background(), link,
			[]string{"pe-uplink0", "pe-uplink1"}))

		assert.Equal(t, []string{"pe-uplink1"}, altNamesOf(t, deviceName))
		assert.Equal(t, []string{"pe-uplink0"}, altNamesOf(t, "squatter0"))
	})

	t.Run("does nothing when no names were saved", func(t *testing.T) {
		defer inProbeNetns(t)()
		link := addProbeVeth(t, deviceName)

		require.NoError(t, restoreAltNames(context.Background(), link, nil))

		assert.Empty(t, altNamesOf(t, deviceName))
	})
}

func addProbeVeth(t *testing.T, name string) netlink.Link {
	t.Helper()

	require.NoError(t, netlink.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		PeerName:  name + "peer",
	}))
	link, err := netlink.LinkByName(name)
	require.NoError(t, err)
	return link
}

func altNamesOf(t *testing.T, name string) []string {
	t.Helper()

	link, err := netlink.LinkByName(name)
	require.NoError(t, err)
	return link.Attrs().AltNames
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
