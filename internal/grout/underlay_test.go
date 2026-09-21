// SPDX-License-Identifier:Apache-2.0

//go:build runasroot

package grout

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"testing"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

func TestMigrateAddressToGrout(t *testing.T) {
	tests := []struct {
		name string
		// address is the underlay address to hand over to the grout port.
		address string
		// withMainIface tells whether the main TAP device exists. Without it
		// the kernel route cannot be installed and the migration fails.
		withMainIface bool
		// stillOnUnderlay tells whether the address is expected on the underlay
		// interface once the migration returned.
		stillOnUnderlay bool
	}{
		{
			name:            "ipv4 address is handed over to grout",
			address:         "192.168.11.3/24",
			withMainIface:   true,
			stillOnUnderlay: false,
		},
		{
			name:            "ipv6 address is handed over to grout",
			address:         "2001:db8:11::3/64",
			withMainIface:   true,
			stillOnUnderlay: false,
		},
		// The underlay interface is the only place the next reconcile reads the
		// underlay addresses from. Leaving the address off it after a failed
		// migration loses it for good: the port is then configured without it
		// and the node still reports itself as ready.
		{
			name:            "ipv4 address is restored when the kernel route fails",
			address:         "192.168.11.3/24",
			withMainIface:   false,
			stillOnUnderlay: true,
		},
		{
			name:            "ipv6 address is restored when the kernel route fails",
			address:         "2001:db8:11::3/64",
			withMainIface:   false,
			stillOnUnderlay: true,
		},
	}

	const underlayInterface = "toswitch1"

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			skipWithoutIPv6(t, tt.address)
			defer enterTestNamespace(t)()

			addr := addAddress(t, addLink(t, underlayInterface), tt.address)
			if tt.withMainIface {
				// Grout mirrors the addresses of its ports on the kernel side,
				// which is what makes the address usable as a route source.
				addAddress(t, addLink(t, defaultVRFName), tt.address)
			}

			defer mockCmdExec(cmdCall{
				cmd: fmt.Sprintf("grcli --err-exit --json --socket sock address add %s iface %s%s",
					addr.IPNet.String(), UnderlayPortNamePrefix, underlayInterface),
			})()

			err := migrateAddressToGrout(context.Background(), NewClient("sock"), underlayInterface, addr)
			if tt.withMainIface {
				assert.NoError(t, err)
			} else {
				assert.Error(t, err)
			}

			assert.Equal(t, tt.stillOnUnderlay, hasAddress(t, underlayInterface, addr))
		})
	}
}

// skipWithoutIPv6 skips the test when cidr is an IPv6 address and the kernel
// was built without IPv6 support.
func skipWithoutIPv6(t *testing.T, cidr string) {
	t.Helper()

	addr, err := netlink.ParseAddr(cidr)
	require.NoError(t, err)
	if addr.IP.To4() != nil {
		return
	}
	if _, err := os.Stat("/proc/net/if_inet6"); err != nil {
		t.Skip("kernel has no IPv6 support")
	}
}

// enterTestNamespace moves the calling thread into a fresh network namespace
// and returns the function restoring the original one.
func enterTestNamespace(t *testing.T) func() {
	t.Helper()

	runtime.LockOSThread()
	origin, err := netns.Get()
	require.NoError(t, err)

	testNS, err := netns.New()
	require.NoError(t, err)

	return func() {
		require.NoError(t, testNS.Close())
		require.NoError(t, netns.Set(origin))
		require.NoError(t, origin.Close())
		runtime.UnlockOSThread()
	}
}

// addLink creates an up veth pair and returns its named end. The underlay
// interfaces are veths too, and both ends must be up for the link to carry the
// routes the migration installs.
func addLink(t *testing.T, name string) netlink.Link {
	t.Helper()

	peerName := name + "-peer"
	require.NoError(t, netlink.LinkAdd(&netlink.Veth{
		LinkAttrs: netlink.LinkAttrs{Name: name},
		PeerName:  peerName,
	}))

	peer, err := netlink.LinkByName(peerName)
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(peer))

	link, err := netlink.LinkByName(name)
	require.NoError(t, err)
	require.NoError(t, netlink.LinkSetUp(link))
	return link
}

func addAddress(t *testing.T, link netlink.Link, cidr string) netlink.Addr {
	t.Helper()

	addr, err := netlink.ParseAddr(cidr)
	require.NoError(t, err)
	// Skip duplicate address detection, which would otherwise leave an IPv6
	// address tentative for as long as it runs.
	addr.Flags |= unix.IFA_F_NODAD
	require.NoError(t, netlink.AddrAdd(link, addr))

	addrs, err := hostnetwork.AddressesForInterface(link.Attrs().Name, hostnetwork.ExcludeLinkLocal())
	require.NoError(t, err)
	require.Len(t, addrs, 1)
	return addrs[0]
}

func hasAddress(t *testing.T, ifaceName string, addr netlink.Addr) bool {
	t.Helper()

	addrs, err := hostnetwork.AddressesForInterface(ifaceName, hostnetwork.ExcludeLinkLocal())
	require.NoError(t, err)
	for _, a := range addrs {
		if a.IPNet.String() == addr.IPNet.String() {
			return true
		}
	}
	return false
}
