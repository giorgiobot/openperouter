// SPDX-License-Identifier:Apache-2.0

package devicestate

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDefaultDirMatchesGroutSocketMount(t *testing.T) {
	assert.Equal(t, "/var/run/grout", Dir,
		"device state must live on the grout-socket mountPath so it persists across pod restarts")
}

func TestSaveLoadDelete_PCIWithNetlink(t *testing.T) {
	origDir := Dir
	Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { Dir = origDir })

	state := Entry{
		PCIAddress:     "0000:01:00.0",
		NetlinkName:    "eth0",
		OriginalDriver: "iavf",
		Addresses:      []string{"10.0.0.1/24", "fd00::1/64"},
		MTU:            9000,
		AltNames:       []string{"pe-uplink0", "enp65s0f0npf0vf12"},
	}

	require.NoError(t, Save(state))

	loaded, err := Load(Entry{NetlinkName: "eth0"})
	require.NoError(t, err)
	assert.Equal(t, state, *loaded)
	assert.Equal(t, int32(9000), loaded.MTU)
	assert.Equal(t, []string{"pe-uplink0", "enp65s0f0npf0vf12"}, loaded.AltNames)

	require.NoError(t, Delete(Entry{NetlinkName: "eth0"}))
	_, err = os.Stat(filePath(Entry{NetlinkName: "eth0"}))
	assert.True(t, os.IsNotExist(err), "state file should be removed")

	loaded, err = Load(Entry{NetlinkName: "eth0"})
	require.NoError(t, err)
	assert.Equal(t, "eth0", loaded.NetlinkName)
	assert.Empty(t, loaded.PCIAddress)
}

func TestSaveLoadDelete_NetlinkName(t *testing.T) {
	origDir := Dir
	Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { Dir = origDir })

	state := Entry{
		NetlinkName: "toswitch1",
		Addresses:   []string{"192.168.11.3/24", "2001:db8:11::3/64"},
	}

	require.NoError(t, Save(state))

	path := filePath(Entry{NetlinkName: "toswitch1"})
	_, err := os.Stat(path)
	require.NoError(t, err, "state file should exist")

	loaded, err := Load(Entry{NetlinkName: "toswitch1"})
	require.NoError(t, err)
	assert.Equal(t, "toswitch1", loaded.NetlinkName)
	assert.Equal(t, []string{"192.168.11.3/24", "2001:db8:11::3/64"}, loaded.Addresses)

	require.NoError(t, Delete(Entry{NetlinkName: "toswitch1"}))
	_, err = os.Stat(path)
	assert.True(t, os.IsNotExist(err), "state file should be removed")
}

func TestOverwrite(t *testing.T) {
	origDir := Dir
	Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { Dir = origDir })

	require.NoError(t, Save(Entry{
		NetlinkName: "toswitch2",
		Addresses:   []string{"10.0.0.1/24"},
	}))

	require.NoError(t, Save(Entry{
		NetlinkName: "toswitch2",
		Addresses:   []string{"192.168.1.1/24", "fd00::1/64"},
	}))

	loaded, err := Load(Entry{NetlinkName: "toswitch2"})
	require.NoError(t, err)
	assert.Equal(t, []string{"192.168.1.1/24", "fd00::1/64"}, loaded.Addresses)
}

func TestDeleteNonExistent(t *testing.T) {
	origDir := Dir
	Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { Dir = origDir })

	err := Delete(Entry{NetlinkName: "does_not_exist"})
	assert.NoError(t, err)

	err = Delete(Entry{})
	assert.Error(t, err)
}

func TestLoadByPCI(t *testing.T) {
	origDir := Dir
	Dir = filepath.Join(t.TempDir(), "grout-state")
	t.Cleanup(func() { Dir = origDir })

	require.NoError(t, Save(Entry{
		PCIAddress:     "0000:01:00.0",
		NetlinkName:    "ens1f0",
		OriginalDriver: "iavf",
		Addresses:      []string{"10.0.0.1/24"},
		MTU:            9000,
	}))
	require.NoError(t, Save(Entry{
		PCIAddress:     "0000:02:00.0",
		NetlinkName:    "ens2f0",
		OriginalDriver: "mlx5_core",
	}))
	require.NoError(t, Save(Entry{
		NetlinkName: "toswitch1",
		Addresses:   []string{"192.168.11.3/24"},
	}))

	t.Run("finds matching PCI address", func(t *testing.T) {
		loaded, err := LoadByPCI("0000:02:00.0")
		require.NoError(t, err)
		assert.Equal(t, "ens2f0", loaded.NetlinkName)
		assert.Equal(t, "mlx5_core", loaded.OriginalDriver)
	})

	t.Run("preserves MTU", func(t *testing.T) {
		loaded, err := LoadByPCI("0000:01:00.0")
		require.NoError(t, err)
		assert.Equal(t, int32(9000), loaded.MTU)
	})

	t.Run("errors when PCI address is unknown", func(t *testing.T) {
		_, err := LoadByPCI("0000:ff:00.0")
		assert.Error(t, err)
		assert.Contains(t, err.Error(), "no device state for PCI address")
	})

	t.Run("errors when PCI address is empty", func(t *testing.T) {
		_, err := LoadByPCI("")
		assert.Error(t, err)
	})
}
