// SPDX-License-Identifier:Apache-2.0

package hostnetwork

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
)

func TestExcludeIPv6Autoconfigured(t *testing.T) {
	tests := []struct {
		name  string
		cidr  string
		flags int
		keep  bool
	}{
		{
			name:  "static ipv6 address",
			cidr:  "2001:db8:11::3/64",
			flags: unix.IFA_F_PERMANENT,
			keep:  true,
		},
		{
			name: "slaac ipv6 address",
			cidr: "2001:db8:11:0:a8c1:abff:fe12:fea4/64",
			keep: false,
		},
		{
			name:  "static ipv4 address",
			cidr:  "192.168.11.3/24",
			flags: unix.IFA_F_PERMANENT,
			keep:  true,
		},
		// A leased IPv4 address is not permanent either, but it is still part
		// of the underlay configuration.
		{
			name: "leased ipv4 address",
			cidr: "192.168.11.3/24",
			keep: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			addr, err := netlink.ParseAddr(tt.cidr)
			require.NoError(t, err)
			addr.Flags = tt.flags

			assert.Equal(t, tt.keep, ExcludeIPv6Autoconfigured()(*addr))
		})
	}
}
