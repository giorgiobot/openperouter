// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net"
	"strconv"
	"strings"

	"github.com/openperouter/openperouter/internal/grout/devicestate"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/pci"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/utils/ptr"
)

func SetupL2VNI(ctx context.Context, client *Client, params hostnetwork.L2VNIParams) error {
	slog.DebugContext(ctx, "setup L2VNI", "vni", params.VNI, "vrf", params.VRF)
	defer slog.DebugContext(ctx, "setup L2VNI done", "vni", params.VNI)

	slog.DebugContext(ctx, "setup L2VNI", "params", params)

	vrf := params.VRF
	if vrf != "" {
		if err := client.ensureVRF(ctx, vrf); err != nil {
			return fmt.Errorf("SetupL2VNI: failed to create VRF %s: %w", vrf, err)
		}
	} else {
		vrf = "main"
	}

	vtepIP, _, err := net.ParseCIDR(params.VTEPIP)
	if err != nil {
		return fmt.Errorf("SetupL2VNI: failed to parse vtep ip %v: %w", params.VTEPIP, err)
	}

	vxlanName := fmt.Sprintf("vni%d", params.VNI)
	// TODO: create the vxlan as a member
	if err := client.ensureVXLAN(ctx, vxlanName, vtepIP.String(), "", params.VNI, ptr.Deref(params.VXLanPort, 4789)); err != nil {
		return fmt.Errorf("SetupL2VNI: failed to create VXLAN: %w", err)
	}

	bridgeName := hostnetwork.BridgeName(params.VNI)
	if err := client.ensureBridge(ctx, bridgeName, vrf); err != nil {
		return fmt.Errorf("SetupL2VNI: failed to create bridge %s: %w", bridgeName, err)
	}

	if err := client.ensureBridgeMember(ctx, "vxlan", bridgeName, vxlanName); err != nil {
		return fmt.Errorf("SetupL2VNI: failed to attach VXLAN %s to bridge %s: %w", vxlanName, bridgeName, err)
	}

	if params.VFPair != nil {
		return setupL2VNIVFPair(ctx, client, params, bridgeName)
	}
	return setupL2VNITAP(ctx, client, params, vrf, bridgeName)
}

func setupL2VNITAP(ctx context.Context, client *Client, params hostnetwork.L2VNIParams, vrf, bridgeName string) error {
	linkPair := linkPairFromVNI(params.VNI)
	ns, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("SetupL2VNI: failed to find namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	if err := ensureTapPortInHostNamespace(ctx, client, linkPair.NamespaceSide, linkPair.HostSide, vrf, ns); err != nil {
		return fmt.Errorf("SetupL2VNI: failed to create TAP port: %w", err)
	}

	if err := client.ensureBridgeMember(ctx, "port", bridgeName, linkPair.NamespaceSide); err != nil {
		return fmt.Errorf("SetupL2VNI: failed to attach TAP %s to bridge %s: %w", linkPair.NamespaceSide, bridgeName, err)
	}

	if err := client.setPortUp(ctx, linkPair.NamespaceSide); err != nil {
		return fmt.Errorf("SetupL2VNI: failed to set grout port up: %w", err)
	}

	hostTap, err := netlink.LinkByName(linkPair.HostSide)
	if err != nil {
		return fmt.Errorf("SetupL2VNI: host TAP %s not found after move: %w", linkPair.HostSide, err)
	}

	if params.HostMaster != nil {
		if err := hostnetwork.SetupHostMaster(ctx, params, hostTap); err != nil {
			return fmt.Errorf("SetupL2VNI: failed to setup host master: %w", err)
		}
	}

	if len(params.L2GatewayIPs) > 0 {
		if err := setupL2Gateway(ctx, client, bridgeName, params); err != nil {
			return fmt.Errorf("SetupL2VNI: failed to setup L2 gateway: %w", err)
		}
	}

	return nil
}

// pciAddressToIfName returns a 6-character hash of the PCI address using
// FNV-1a, encoded in base-36 (0-9a-z). Used for VF pair trunk port names.
func pciAddressToIfName(pciAddr string) string {
	h := fnv.New32a()
	h.Write([]byte(pciAddr))
	s := strconv.FormatUint(uint64(h.Sum32()), 36)
	for len(s) < 6 {
		s = "0" + s
	}
	return s[len(s)-6:]
}

func resolveVFPairPCI(cfg *hostnetwork.VFPairParams) (string, error) {
	if cfg.PCIAddress != nil {
		if err := pci.ResolvePCIAddress(*cfg.PCIAddress); err != nil {
			return "", err
		}
		return *cfg.PCIAddress, nil
	}
	if cfg.PFName != nil && cfg.VFIndex != nil {
		return pci.ResolvePFVFIndex(*cfg.PFName, int(*cfg.VFIndex))
	}
	if cfg.NetlinkName != nil {
		devState, err := devicestate.Load(*cfg.NetlinkName)
		if err == nil {
			return devState.PCIAddress, nil
		}

		if errors.Is(err, devicestate.ErrDeviceStateNotFound) {
			slog.Info("device state not found, resolving PCI address from netlink name", "netlinkName", *cfg.NetlinkName)
			pciAddr, err := pci.ResolveNetlinkName(*cfg.NetlinkName)
			if err != nil {
				return "", fmt.Errorf("failed to resolve PCI address for %s: %w", *cfg.NetlinkName, err)
			}
			slog.Info("resolved PCI address from netlink name", "pciAddr", pciAddr)
			devState = &devicestate.Entry{
				InterfaceName: *cfg.NetlinkName,
				PCIAddress:    pciAddr,
			}
			if err := devicestate.Save(*cfg.NetlinkName, *devState); err != nil {
				return "", fmt.Errorf("failed to save device state for %s: %w", *cfg.NetlinkName, err)
			}

			return pciAddr, nil
		}

		return "", fmt.Errorf("failed to load device state for %s: %w", *cfg.NetlinkName, err)
	}
	return "", fmt.Errorf("sriovVFPair must specify pciAddress, pfName+vfIndex, or netlinkName")
}

func setupL2VNIVFPair(ctx context.Context, client *Client, params hostnetwork.L2VNIParams, bridgeName string) error {
	vfPair := params.VFPair

	pciAddr, err := resolveVFPairPCI(vfPair)
	if err != nil {
		return fmt.Errorf("SetupL2VNI VF-pair: failed to resolve PCI address: %w", err)
	}
	trunkPortName := "t_" + pciAddressToIfName(pciAddr)

	opts := PortOptions{
		RXQueues: vfPair.RXQueues,
		QSize:    vfPair.QSize,
	}
	if err := PrepareAndBindTrunkVF(ctx, client, params.TargetNS, pciAddr, trunkPortName, opts); err != nil {
		return fmt.Errorf("SetupL2VNI VF-pair: failed to prepare trunk VF %s: %w", pciAddr, err)
	}

	vlanIfName := VLANSubInterfaceName(vfPair.VLAN, trunkPortName)
	if err := client.ensureVLANSubInterface(ctx, vlanIfName, trunkPortName, vfPair.VLAN); err != nil {
		return fmt.Errorf("SetupL2VNI VF-pair: failed to create VLAN sub-interface: %w", err)
	}

	if err := client.ensureBridgeMember(ctx, "vlan", bridgeName, vlanIfName); err != nil {
		return fmt.Errorf("SetupL2VNI VF-pair: failed to attach VLAN %s to bridge %s: %w",
			vlanIfName, bridgeName, err)
	}

	if len(params.L2GatewayIPs) > 0 {
		if err := setupL2Gateway(ctx, client, bridgeName, params); err != nil {
			return fmt.Errorf("SetupL2VNI VF-pair: failed to setup L2 gateway: %w", err)
		}
	}

	return nil
}

// VLANSubInterfaceName returns the grout VLAN sub-interface name for a given
// VLAN ID and trunk port name.
func VLANSubInterfaceName(vlan int32, trunkPortName string) string {
	return fmt.Sprintf("%s.%d", trunkPortName, vlan)
}

// RemoveStaleVFPairResources removes VLAN sub-interfaces and trunk ports
// that are not referenced by any configured L2VNI.
func RemoveStaleVFPairResources(ctx context.Context, client *Client, configuredL2VNIs []hostnetwork.L2VNIParams) error {
	expectedVLANIfs := map[string]bool{}
	referencedTrunks := map[string]bool{}
	for _, l2 := range configuredL2VNIs {
		if l2.VFPair == nil {
			continue
		}
		pciAddr, err := resolveVFPairPCI(l2.VFPair)
		if err != nil {
			slog.ErrorContext(ctx, "failed to resolve PCI address for L2VNI VF-pair during cleanup", "l2vni", l2.Name, "error", err)
			continue
		}
		trunkPortName := "t_" + pciAddressToIfName(pciAddr)
		vlanIfName := VLANSubInterfaceName(l2.VFPair.VLAN, trunkPortName)
		expectedVLANIfs[vlanIfName] = true
		referencedTrunks[trunkPortName] = true
	}

	ifaces, err := client.listInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("failed to list interfaces for VF-pair cleanup: %w", err)
	}

	for _, iface := range ifaces {
		if !strings.HasPrefix(iface.Name, "t_") || !strings.Contains(iface.Name, ".") {
			continue
		}
		if expectedVLANIfs[iface.Name] {
			continue
		}
		slog.InfoContext(ctx, "removing stale VLAN sub-interface", "name", iface.Name)
		if err := client.deleteInterface(ctx, iface.Name); err != nil {
			return fmt.Errorf("failed to delete stale VLAN sub-interface %s: %w", iface.Name, err)
		}
	}

	for _, iface := range ifaces {
		if !strings.HasPrefix(iface.Name, "t_") || strings.Contains(iface.Name, ".") {
			continue
		}
		if referencedTrunks[iface.Name] {
			continue
		}
		slog.InfoContext(ctx, "removing stale trunk port", "name", iface.Name)
		if err := client.deletePort(ctx, iface.Name); err != nil {
			return fmt.Errorf("failed to delete stale trunk port %s: %w", iface.Name, err)
		}
	}
	return nil
}

func setupL2Gateway(ctx context.Context, client *Client, bridgeName string, params hostnetwork.L2VNIParams) error {
	slog.DebugContext(ctx, "setting up L2 gateway", "bridge", bridgeName, "ips", params.L2GatewayIPs)

	mac, err := hostnetwork.BridgeFixedMAC(params.VNI)
	if err != nil {
		return fmt.Errorf("failed to compute fixed MAC for VNI %d: %w", params.VNI, err)
	}
	if err := client.setBridgeMAC(ctx, bridgeName, net.HardwareAddr(mac).String()); err != nil {
		return fmt.Errorf("failed to set fixed MAC on bridge %s: %w", bridgeName, err)
	}

	for _, ip := range params.L2GatewayIPs {
		if err := client.ensureAddress(ctx, bridgeName, ip); err != nil {
			return fmt.Errorf("failed to assign L2 gateway IP %s to bridge %s: %w", ip, bridgeName, err)
		}
	}

	return nil
}
