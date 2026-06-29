// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"fmt"
	"log/slog"
	"net"

	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
	"k8s.io/utils/ptr"
)

// CTODO: refactor with internal/hostnetwork/veth.go
const HostVethPrefix = "host-"
const PEVethPrefix = "pe-"

// vethNamesFromVNI returns the names of the veth legs
// corresponding to the default namespace and the target namespace, based on VNI.
func vethNamesFromVNI(vni int32) hostnetwork.VethNames {
	hostSide := fmt.Sprintf("%s%d", HostVethPrefix, vni)
	peSide := fmt.Sprintf("%s%d", PEVethPrefix, vni)
	return hostnetwork.VethNames{HostSide: hostSide, NamespaceSide: peSide}
}

func SetupL3VNI(ctx context.Context, client *Client, params hostnetwork.L3VNIParams) error {
	slog.DebugContext(ctx, "setup L3VNI", "vrf", params.VRF, "vni", params.VNI)
	defer slog.DebugContext(ctx, "setup L3VNI done", "vrf", params.VRF, "vni", params.VNI)

	if err := setupVNI(ctx, client, params.VNIParams); err != nil {
		return fmt.Errorf("SetupL3VNI: failed to setup VNI: %w", err)
	}

	if params.HostVeth == nil {
		slog.DebugContext(ctx, "no host veth configured, skipping setup")
		return nil
	}

	vethNames := vethNamesFromVNI(params.VNI)

	ns, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("SetupPassthrough: failed to find namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	if err := ensureTapPortInHostNamespace(ctx, client, vethNames.NamespaceSide, vethNames.HostSide, params.VRF, ns); err != nil {
		return err
	}

	hostTap, err := netlink.LinkByName(vethNames.HostSide)
	if err != nil {
		return fmt.Errorf("host TAP %s not found after move: %w", vethNames.HostSide, err)
	}
	if err := hostnetwork.AssignIPsToInterface(hostTap, params.HostVeth.HostIPv4, params.HostVeth.HostIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to host TAP: %w", err)
	}

	// CTODO: handle MTU fix for the vxlan

	if err := ensurePortAddresses(ctx, client, vethNames.NamespaceSide, params.HostVeth.NSIPv4, params.HostVeth.NSIPv6); err != nil {
		return fmt.Errorf("failed to assign IPs to grout port: %w", err)
	}

	return nil
}

func setupVNI(ctx context.Context, client *Client, params hostnetwork.VNIParams) error {
	if err := client.ensureVRF(ctx, params.VRF); err != nil {
		return fmt.Errorf("SetupL3VNI: failed to create VRF %s: %w", params.VRF, err)
	}

	vtepIP, _, err := net.ParseCIDR(params.VTEPIP)
	if err != nil {
		return fmt.Errorf("failed to parse vtep ip %v: %w", params.VTEPIP, err)
	}

	if err := client.ensureVXLAN(ctx,
		fmt.Sprintf("vni%d", params.VNI),
		vtepIP.String(),
		params.VRF,
		params.VNI,
		ptr.Deref(params.VXLanPort, 4789),
	); err != nil {
		return fmt.Errorf("failed to create vxlan interface: %w", err)
	}

	return nil
}

/*
func createL3VNITap(ctx context.Context, client *Client, ns netns.NsHandle, params hostnetwork.L3VNIParams, portName, tapName string) error {
	_ = removeLinkByName(tapName)

	devargs := fmt.Sprintf("net_tap%d,iface=%s", params.VNI, tapName)
	if err := client.ensurePortInVRF(ctx, portName, devargs, params.VRF); err != nil {
		return fmt.Errorf("failed to create host session port: %w", err)
	}

	// if err := assignIPsWithRetry(ctx, client, portName, params.HostVeth.NSIPv4, params.HostVeth.NSIPv6); err != nil {
	// 	return fmt.Errorf("failed to assign IPs to grout port: %w", err)
	// }

	// // Grout creates a kernel VRF device and a TUN (gr-loop) when the
	// // first port joins a custom VRF. Assign the PE-side IPs to the
	// // TUN so FRR's kernel TCP stack can bind to them in this VRF.
	// if err := assignIPsToVRFLoopback(ctx, ns, params.VRF,
	// 	params.HostVeth.NSIPv4, params.HostVeth.NSIPv6); err != nil {
	// 	return fmt.Errorf("failed to assign IPs to VRF loopback: %w", err)
	// }

	if err := moveLinkToHostNamespace(ctx, tapName, ns); err != nil {
		return fmt.Errorf("failed to move TAP to host namespace: %w", err)
	}

	return setUniqueMAC(tapName)
}
*/

func RemoveAllVNIs(ctx context.Context, client *Client) error {
	return RemoveNonConfiguredVNIs(ctx, client, []hostnetwork.L3VNIParams{})
}

func RemoveNonConfiguredVNIs(ctx context.Context, client *Client, configured []hostnetwork.L3VNIParams) error {
	slog.DebugContext(ctx, "removing stale L3VNIs")
	defer slog.DebugContext(ctx, "removing stale L3VNIs done")

	configuredVNIs := make(map[int32]bool)
	configuredVRFs := make(map[string]bool)

	for _, p := range configured {
		configuredVNIs[p.VNI] = true
		configuredVRFs[p.VRF] = true
	}

	ifaces, err := client.listInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("RemoveL3VNIs: failed to list interfaces: %w", err)
	}

	staleVNIs := findStaleVNIs(ifaces, configuredVNIs)
	for _, vni := range staleVNIs {
		slog.InfoContext(ctx, "removing stale L3VNI", "vni", vni)
		if err := removeL3VNI(ctx, client, vni); err != nil {
			return fmt.Errorf("RemoveL3VNIs: failed to remove VNI %d: %w", vni, err)
		}
	}

	staleVRFs := findStaleVRFs(ifaces, configuredVRFs)
	for _, vrf := range staleVRFs {
		if vrf == "main" {
			continue
		}
		slog.InfoContext(ctx, "removing stale L3VNI", "vrf", vrf)
		if err := client.deleteInterface(ctx, vrf); err != nil {
			return fmt.Errorf("failed to delete VRF %s: %w", vrf, err)
		}
	}

	return nil
}

func removeL3VNI(ctx context.Context, client *Client, vni int32) error {
	vethNames := vethNamesFromVNI(vni)

	if err := client.deletePort(ctx, vethNames.NamespaceSide); err != nil {
		return fmt.Errorf("failed to delete grout port %s: %w", vethNames.NamespaceSide, err)
	}

	vxlanName := fmt.Sprintf("vni%d", vni)
	if err := client.deleteInterface(ctx, vxlanName); err != nil {
		return fmt.Errorf("failed to delete VXLAN %s: %w", vxlanName, err)
	}

	return nil
}

func findStaleVNIs(ifaces []groutInterface, configuredVNIs map[int32]bool) []int32 {
	var stale []int32
	for _, iface := range ifaces {
		var vni int32
		if n, _ := fmt.Sscanf(iface.Name, "vni%d", &vni); n == 1 {
			if !configuredVNIs[vni] {
				stale = append(stale, vni)
			}
		}
	}
	return stale
}

func findStaleVRFs(ifaces []groutInterface, configuredVRFs map[string]bool) []string {
	var stale []string
	for _, iface := range ifaces {
		if iface.Type != "vrf" {
			continue
		}

		if !configuredVRFs[iface.Name] {
			stale = append(stale, iface.Name)
		}

	}
	return stale
}
