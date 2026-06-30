// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"

	"github.com/openperouter/openperouter/internal/grout/sriov"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	UnderlayPortNamePrefix = "u_"
)

func HasUnderlayInterface(ctx context.Context, client *Client) (bool, error) {
	interfaces, err := client.listInterfaces(ctx)
	if err != nil {
		return false, fmt.Errorf("HasUnderlayInterface: Failed to list interfaces: %w", err)
	}

	for _, iface := range interfaces {
		if strings.HasPrefix(iface.Name, UnderlayPortNamePrefix) {
			return true, nil
		}
	}
	return false, nil
}

// SetupUnderlay configures the underlay interface via the grout dataplane.
// It moves the kernel interface into the router namespace, creates a grout
// TAP port with remote= so TC ingress rules redirect incoming packets from
// the physical interface to grout, and assigns the underlay IPs to the grout
// port. Grout handles all L2 (ARP) and L3 forwarding; it also creates a
// NOARP kernel interface for kernel TCP (used by FRR bgpd for BGP sessions).
func SetupUnderlay(ctx context.Context, client *Client, params hostnetwork.UnderlayParams) error {
	slog.DebugContext(ctx, "setup underlay", "params", params)
	defer slog.DebugContext(ctx, "setup underlay done")

	perouterNetNS, err := netns.GetFromPath(params.TargetNS)
	if err != nil {
		return fmt.Errorf("setupUnderlay: Failed to find network namespace %s: %w", params.TargetNS, err)
	}
	defer func() {
		if err := perouterNetNS.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", params.TargetNS, "error", err)
		}
	}()

	defaultNetNS, err := netns.Get()
	if err != nil {
		return fmt.Errorf("SetupUnderlay: failed to get netns handle for default namespace: %w", err)
	}
	defer func() {
		if err := defaultNetNS.Close(); err != nil {
			slog.Error("failed to close default namespace", "error", err)
		}
	}()

	if err := validateNoStaleUnderlays(perouterNetNS, params.UnderlayInterfaces); err != nil {
		return err
	}

	perouterNetNSHandle, err := netlink.NewHandleAt(perouterNetNS)
	if err != nil {
		return fmt.Errorf("SetupUnderlay: failed to get netlink handle for namespace %s: %w", perouterNetNS.String(), err)
	}
	defer perouterNetNSHandle.Close()
	defaultNetNSHandle, err := netlink.NewHandleAt(defaultNetNS)
	if err != nil {
		return fmt.Errorf("SetupUnderlay: failed to get netlink handle for default namespace: %w", err)
	}
	defer defaultNetNSHandle.Close()

	for _, underlayInterface := range params.UnderlayInterfaces {
		if sriov.IsVF(underlayInterface) {
			if err := configureVFUnderlayPort(ctx, client, underlayInterface); err != nil {
				return err
			}
		} else {
			if err := hostnetwork.MoveInterfaceToNamespace(ctx, underlayInterface, defaultNetNSHandle, perouterNetNSHandle, perouterNetNS,
				hostnetwork.UnderlayGroupID); err != nil {
				return err
			}
			if err := netnamespace.In(perouterNetNS, func() error {
				return configureTAPUnderlayPort(ctx, client, underlayInterface)
			}); err != nil {
				return err
			}
		}
	}

	if params.TunnelEndpoint != nil {
		if err := setupTunnelEndpoint(ctx, client, *params.TunnelEndpoint); err != nil {
			return err
		}
	}

	return nil
}

func setupTunnelEndpoint(ctx context.Context, client *Client, ep hostnetwork.UnderlayTunnelEndpointParams) error {
	if err := assignIPsToGroutPort(ctx, client, "main",
		ep.IPv4CIDR, ep.IPv6CIDR); err != nil {
		return fmt.Errorf("failed to assign tunnel endpoint IPs to grout underlay: %w", err)
	}

	return nil
}

// RemoveUnderlay tears down the grout underlay ports and resets the kernel
// group IDs on moved NICs so HasUnderlayInterface returns false on the next
// reconcile.
func RemoveUnderlay(ctx context.Context, client *Client, targetNS string) error {
	interfaces, err := client.listInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("HasUnderlayInterface: Failed to list interfaces: %w", err)
	}

	ns, err := netns.GetFromPath(targetNS)
	if err != nil {
		return fmt.Errorf("setupUnderlay: Failed to find network namespace %s: %w", targetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", targetNS, "error", err)
		}
	}()

	for _, iface := range interfaces {
		if !strings.HasPrefix(iface.Name, UnderlayPortNamePrefix) {
			continue
		}

		pciAddr := iface.Devargs
		isVF := sriov.IsPCIAddress(pciAddr)

		if !isVF {
			if err := netnamespace.In(ns, func() error {
				if err := migrateAddressesToKernel(ctx, client, iface.Name); err != nil {
					return fmt.Errorf("RemoveUnderlay: failed to migrate addresses back to kernel: %w", err)
				}
				return nil
			}); err != nil {
				return err
			}
		}

		if err := client.deletePort(ctx, iface.Name); err != nil {
			return fmt.Errorf("RemoveUnderlay: failed to delete grout port %s: %w", iface.Name, err)
		}

		if isVF {
			slog.InfoContext(ctx, "restoring VF default kernel driver",
				"port", iface.Name, "pci", pciAddr)
			if err := sriov.RestoreDefaultDriver(pciAddr); err != nil {
				return fmt.Errorf("RemoveUnderlay: failed to restore default driver for %s: %w", pciAddr, err)
			}
		}
	}

	if err := hostnetwork.RestoreUnderlay(ctx, targetNS); err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to clean kernel underlay state: %w", err)
	}

	return nil
}

func validateNoStaleUnderlays(ns netns.NsHandle, wanted []string) error {
	existingIfaces, err := hostnetwork.FindInterfacesInGroup(ns, hostnetwork.UnderlayGroupID)
	if err != nil {
		return fmt.Errorf("failed to check existing underlay interfaces: %w", err)
	}
	for _, name := range existingIfaces {
		if !slices.Contains(wanted, name) {
			return hostnetwork.UnderlayExistsError(fmt.Sprintf(
				"existing underlay found: %s, new interfaces are %v", name, wanted))
		}
	}
	return nil
}

func configureTAPUnderlayPort(ctx context.Context, client *Client, underlayInterface string) error {
	underlayAddrs, err := readUnderlayAddresses(underlayInterface)
	if err != nil {
		return fmt.Errorf("failed to read underlay interface addresses: %w", err)
	}

	devargs := fmt.Sprintf("net_tap%d,remote=%s,iface=%s", makeTapIndex(), underlayInterface, "tap_"+underlayInterface)
	if err := client.ensurePort(ctx, UnderlayPortNamePrefix+underlayInterface, devargs); err != nil {
		return fmt.Errorf("failed to create grout underlay port: %w", err)
	}

	if err := migrateAddressesToGrout(ctx, client, underlayInterface, underlayAddrs); err != nil {
		return err
	}

	return nil
}

// configureVFUnderlayPort binds a VF to its DPDK driver and plugs it into
// Grout using the PCI BDF address as devargs. The kernel netdev disappears
// after driver binding, so addresses are read beforehand and assigned directly
// to the grout port.
func configureVFUnderlayPort(ctx context.Context, client *Client, underlayInterface string) error {
	pciAddr, err := sriov.PCIAddress(underlayInterface)
	if err != nil {
		return fmt.Errorf("failed to get PCI address for VF %s: %w", underlayInterface, err)
	}

	vendorID, err := sriov.Vendor(underlayInterface)
	if err != nil {
		return fmt.Errorf("failed to get vendor for VF %s: %w", underlayInterface, err)
	}

	targetDriver, err := sriov.DriverForVendor(vendorID)
	if err != nil {
		return fmt.Errorf("unsupported VF vendor for %s: %w", underlayInterface, err)
	}

	underlayAddrs, err := readUnderlayAddresses(underlayInterface)
	if err != nil {
		return fmt.Errorf("failed to read underlay interface addresses: %w", err)
	}

	slog.InfoContext(ctx, "binding VF to DPDK driver",
		"iface", underlayInterface, "pci", pciAddr, "driver", targetDriver)

	if err := sriov.BindDriver(pciAddr, targetDriver); err != nil {
		return fmt.Errorf("failed to bind VF %s to %s: %w", underlayInterface, targetDriver, err)
	}

	portName := UnderlayPortNamePrefix + underlayInterface
	if err := client.ensurePort(ctx, portName, pciAddr); err != nil {
		return fmt.Errorf("failed to create grout underlay port for VF: %w", err)
	}

	for _, addr := range underlayAddrs {
		cidr := addr.IPNet.String()
		if err := client.ensureAddress(ctx, portName, cidr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout VF port: %w", cidr, err)
		}
		slog.InfoContext(ctx, "assigned underlay address to grout VF port", "cidr", cidr, "iface", portName)
	}

	return nil
}

func migrateAddressesToGrout(ctx context.Context, client *Client, underlayInterface string, addrs []netlink.Addr) error {
	for _, addr := range addrs {
		cidr := addr.IPNet.String()

		if err := client.ensureAddress(ctx, UnderlayPortNamePrefix+underlayInterface, cidr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout underlay port: %w", cidr, err)
		}

		if err := hostnetwork.DeleteAddressFromInterface(underlayInterface, addr); err != nil {
			return fmt.Errorf("failed to remove address %s from underlay interface: %w", cidr, err)
		}

		if err := ensureKernelSubnetRoute("main", addr.IPNet.String()); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}

		slog.InfoContext(ctx, "migrated underlay address to grout", "cidr", cidr, "iface", UnderlayPortNamePrefix+underlayInterface)
	}
	return nil
}

func migrateAddressesToKernel(ctx context.Context, client *Client, underlayPortName string) error {
	addrs, err := client.getAddresses(ctx, underlayPortName)
	if err != nil {
		return fmt.Errorf("failed to read addresses from grout port %s: %w", underlayPortName, err)
	}
	if len(addrs) == 0 {
		return nil
	}

	underlayLinkName := strings.Replace(underlayPortName, UnderlayPortNamePrefix, "", 1)
	err = setAddresses(ctx, underlayLinkName, addrs)
	if err != nil {
		return fmt.Errorf("failed to set addresses to link %s: %w", underlayLinkName, err)
	}

	for _, addr := range addrs {
		err := client.deleteAddress(ctx, underlayPortName, addr)
		if err != nil {
			return fmt.Errorf("failed to delete addresses on grout port %s: %w", underlayPortName, err)
		}

		if err := removeKernelSubnetRoute("main", addr); err != nil {
			return fmt.Errorf("failed to remove kernel route for %s: %w", addr, err)
		}
	}

	return nil
}

func readUnderlayAddresses(ifaceName string) ([]netlink.Addr, error) {
	var addrs []netlink.Addr

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to find underlay interface %s: %w", ifaceName, err)
	}
	all, err := netlink.AddrList(link, netlink.FAMILY_ALL)
	if err != nil {
		return nil, fmt.Errorf("failed to list addresses on %s: %w", ifaceName, err)
	}
	for _, addr := range all {
		if addr.IP.IsLinkLocalMulticast() {
			continue
		}
		if addr.IP.IsLinkLocalUnicast() {
			continue
		}
		addrs = append(addrs, addr)
	}

	return addrs, nil
}

func setAddresses(ctx context.Context, linkName string, addrs []string) error {
	link, err := netlink.LinkByName(linkName)
	if err != nil {
		return fmt.Errorf("failed to get link %s: %w", linkName, err)
	}

	for _, cidr := range addrs {
		parsed, err := netlink.ParseAddr(cidr)
		if err != nil {
			return fmt.Errorf("failed to parse address %s: %w", cidr, err)
		}

		if parsed.IP.IsLinkLocalUnicast() {
			continue
		}

		if err := netlink.AddrAdd(link, parsed); err != nil {
			return fmt.Errorf("failed to assign address %s to %s: %w", cidr, linkName, err)
		}
		slog.InfoContext(ctx, "migrated address back to kernel interface", "cidr", cidr, "iface", linkName)
	}

	return nil
}

func ensureKernelSubnetRoute(ifaceName, addr string) error {
	srcAddr, ipNet, err := net.ParseCIDR(addr)
	if err != nil {
		return fmt.Errorf("failed to parse CIDR %s: %w", addr, err)
	}
	ones, bits := ipNet.Mask.Size()
	if ones == bits {
		return nil
	}

	if srcAddr.IsLinkLocalUnicast() {
		return nil
	}

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("failed to find interface %s: %w", ifaceName, err)
	}

	existing, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, &netlink.Route{
		Dst:       ipNet,
		LinkIndex: link.Attrs().Index,
		Src:       srcAddr,
	}, netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF)
	if err != nil {
		return fmt.Errorf("failed to list routes for %s dev %s: %w", ipNet, ifaceName, err)
	}
	if len(existing) > 0 {
		return nil
	}

	if err := netlink.RouteAdd(&netlink.Route{
		Dst:       ipNet,
		LinkIndex: link.Attrs().Index,
		Src:       srcAddr,
	}); err != nil {
		return fmt.Errorf("failed to add route for %s dev %s: %w", ipNet, ifaceName, err)
	}

	slog.Info("added kernel route for subnet", "cidr", addr, "src", srcAddr, "ipnet", ipNet, "iface", ifaceName)
	return nil
}

func removeKernelSubnetRoute(ifaceName, addr string) error {
	srcAddr, ipNet, err := net.ParseCIDR(addr)
	if err != nil {
		return fmt.Errorf("failed to parse CIDR %s: %w", addr, err)
	}
	ones, bits := ipNet.Mask.Size()
	if ones == bits {
		return nil
	}

	if srcAddr.IsLinkLocalUnicast() {
		return nil
	}

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return fmt.Errorf("failed to find interface %s: %w", ifaceName, err)
	}

	if err := netlink.RouteDel(&netlink.Route{
		Dst:       ipNet,
		LinkIndex: link.Attrs().Index,
		Src:       srcAddr,
	}); err != nil {
		return fmt.Errorf("failed to delete route for %s dev %s: %w", ipNet, ifaceName, err)
	}

	slog.Info("removed kernel route for subnet", "cidr", addr, "src", srcAddr, "ipnet", ipNet, "iface", ifaceName)
	return nil
}

// assignIPsToGroutPort assigns IPv4 and IPv6 addresses to a grout port via grcli.
func assignIPsToGroutPort(ctx context.Context, client *Client, portName string, ipv4, ipv6 string) error {
	if ipv4 == "" && ipv6 == "" {
		return fmt.Errorf("at least one IP address must be provided (IPv4 or IPv6)")
	}

	for _, addr := range []string{ipv4, ipv6} {
		if addr == "" {
			continue
		}
		slog.DebugContext(ctx, "assigning IP to grout port", "port", portName, "addr", addr)
		if err := client.ensureAddress(ctx, portName, addr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout port %s: %w", addr, portName, err)
		}
	}
	return nil
}
