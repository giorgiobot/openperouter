// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/openperouter/openperouter/internal/grout/devicestate"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestFirstUnderlayPortName(t *testing.T) {
	tests := []struct {
		name       string
		interfaces []hostnetwork.UnderlayInterface
		want       string
		wantErr    string
	}{
		{
			name: "uses the first underlay interface",
			interfaces: []hostnetwork.UnderlayInterface{
				{InterfaceName: "eth0"},
				{InterfaceName: "eth1"},
			},
			want: "u_eth0",
		},
		{
			name: "uses the first underlay interface accelerated port name",
			interfaces: []hostnetwork.UnderlayInterface{
				{
					InterfaceName: "eth0",
					AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{
						PortName: new("p0"),
					},
				},
				{InterfaceName: "eth1"},
			},
			want: "p0",
		},
		{
			name:    "requires an underlay interface",
			wantErr: "tunnel endpoint requires at least one underlay interface",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := firstUnderlayPortName(tt.interfaces)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestSetupTunnelEndpoint(t *testing.T) {
	defer mockCmdExec(
		cmdCall{cmd: "grcli --err-exit --json --socket sock address add 192.0.2.1/32 iface u_eth0"},
		cmdCall{cmd: "grcli --err-exit --json --socket sock address add 2001:db8::1/128 iface u_eth0"},
	)()

	err := setupTunnelEndpoint(context.Background(), NewClient("sock"), "u_eth0", hostnetwork.UnderlayTunnelEndpointParams{
		IPv4CIDR: "192.0.2.1/32",
		IPv6CIDR: "2001:db8::1/128",
	})
	require.NoError(t, err)
}

func TestPortName(t *testing.T) {
	t.Run("default prefix", func(t *testing.T) {
		assert.Equal(t, "u_ens1f0", PortName(hostnetwork.UnderlayInterface{
			InterfaceName: "ens1f0",
		}))
	})

	t.Run("accelerated without override still uses prefix", func(t *testing.T) {
		assert.Equal(t, "u_ens1f0", PortName(hostnetwork.UnderlayInterface{
			InterfaceName:     "ens1f0",
			AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{},
		}))
	})

	t.Run("port name override", func(t *testing.T) {
		assert.Equal(t, "p0", PortName(hostnetwork.UnderlayInterface{
			InterfaceName: "ens1f0",
			AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{
				PortName: new("p0"),
			},
		}))
	})
}

func TestGroutPortToUnderlayInterface(t *testing.T) {
	origDir := devicestate.Dir
	devicestate.Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { devicestate.Dir = origDir })

	require.NoError(t, devicestate.Save("ens1f0", devicestate.Entry{
		PCIAddress:     "0000:01:00.0",
		InterfaceName:  "ens1f0",
		OriginalDriver: "iavf",
		Addresses:      []string{"10.0.0.1/24"},
	}))

	t.Run("tap port uses grout name without prefix", func(t *testing.T) {
		got, err := groutPortToUnderlayInterface(
			groutInterface{Name: "u_eth0"},
			&groutInterfaceDetails{
				Devargs:     "net_tap0,remote=eth0,iface=tap_eth0",
				Description: UnderlayInterfaceDescriptionMarker,
			},
		)
		require.NoError(t, err)
		assert.Equal(t, "eth0", got.InterfaceName)
		assert.Equal(t, hostnetwork.UnderlayInterfaceNetDev, got.Kind)
		assert.Nil(t, got.AcceleratedConfig)
	})

	t.Run("pci port loads interface name from device state", func(t *testing.T) {
		got, err := groutPortToUnderlayInterface(
			groutInterface{Name: "p0"},
			&groutInterfaceDetails{
				Devargs:     "0000:01:00.0",
				Description: UnderlayInterfaceDescriptionMarker,
			},
		)
		require.NoError(t, err)
		assert.Equal(t, "ens1f0", got.InterfaceName)
		assert.Equal(t, hostnetwork.UnderlayInterfaceNetDev, got.Kind)
		require.NotNil(t, got.AcceleratedConfig)
		require.NotNil(t, got.AcceleratedConfig.PortName)
		assert.Equal(t, "p0", *got.AcceleratedConfig.PortName)
	})

	t.Run("pci port with default grout name still uses device state", func(t *testing.T) {
		got, err := groutPortToUnderlayInterface(
			groutInterface{Name: "u_ens1f0"},
			&groutInterfaceDetails{
				Devargs:     "0000:01:00.0",
				Description: UnderlayInterfaceDescriptionMarker,
			},
		)
		require.NoError(t, err)
		assert.Equal(t, "ens1f0", got.InterfaceName)
		require.NotNil(t, got.AcceleratedConfig)
		require.NotNil(t, got.AcceleratedConfig.PortName)
		assert.Equal(t, "u_ens1f0", *got.AcceleratedConfig.PortName)
	})

	t.Run("pci port without device state returns error", func(t *testing.T) {
		_, err := groutPortToUnderlayInterface(
			groutInterface{Name: "p1"},
			&groutInterfaceDetails{
				Devargs:     "0000:ff:00.0",
				Description: UnderlayInterfaceDescriptionMarker,
			},
		)
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "failed to load device state")
	})
}
