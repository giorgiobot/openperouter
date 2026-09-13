// SPDX-License-Identifier:Apache-2.0

package grout

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"slices"
	"strings"
	"syscall"

	"github.com/openperouter/openperouter/internal/grout/devicestate"
	"github.com/openperouter/openperouter/internal/hostnetwork"
	"github.com/openperouter/openperouter/internal/netnamespace"
	"github.com/openperouter/openperouter/internal/sriov"
	"github.com/openperouter/openperouter/internal/sysctl"
	"github.com/vishvananda/netlink"
	"github.com/vishvananda/netns"
)

const (
	UnderlayPortNamePrefix             = "u_"
	UnderlayInterfaceDescriptionMarker = "underlay"
	BadConfigurationPortName           = "bad_config"
)

// PortName returns the grout port name for the given underlay interface.
// If the interface has an AcceleratedConfig with a PortName override, that
// value is used directly; otherwise the name is "u_<InterfaceName>".
func PortName(iface hostnetwork.UnderlayInterface) string {
	if iface.AcceleratedConfig != nil && iface.AcceleratedConfig.PortName != nil {
		return *iface.AcceleratedConfig.PortName
	}
	return UnderlayPortNamePrefix + iface.InterfaceName
}

// SetupUnderlay configures the underlay interfaces via the grout dataplane.
// Every interface is provisioned in the router namespace according to its
// kind (host network devices are moved in, CNI interfaces are added by
// invoking their plugin); then a grout TAP port with remote= is created so
// TC ingress rules redirect incoming packets from the interface to grout,
// and the underlay IPs are moved to the grout port. Grout handles all L2
// (ARP) and L3 forwarding; it also creates a NOARP kernel interface for
// kernel TCP (used by FRR bgpd for BGP sessions).
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

	// If any existing underlay interfaces were removed from the new list,
	// clean them up before setting up the new ones, tearing down their
	// grout state first.
	existing, err := UnderlayInterfaces(ctx, client, params.TargetNS)
	if err != nil {
		return fmt.Errorf("failed to check existing underlay interfaces: %w", err)
	}
	if toRemove := hostnetwork.UnderlayInterfacesToRemove(existing, params.UnderlayInterfaces); len(toRemove) > 0 {
		slog.InfoContext(ctx, "underlay interfaces changed, removing old interfaces before setup",
			"toRemove", toRemove, "requested", params.UnderlayInterfaces)
		if err := RestoreUnderlay(ctx, client, params.TargetNS, toRemove); err != nil {
			return fmt.Errorf("failed to remove old underlay interfaces: %w", err)
		}
	}

	for _, iface := range params.UnderlayInterfaces {
		switch iface.Kind {
		case hostnetwork.UnderlayInterfaceNetDev:
			if iface.AcceleratedConfig == nil {
				err := setupTapUnderlay(ctx, client, perouterNetNS, params.TargetNS, iface)
				if err != nil {
					return err
				}
			} else {
				err := setupGroutPortUnderlay(ctx, client, perouterNetNS, iface)
				if err != nil {
					return err
				}
			}
		case hostnetwork.UnderlayInterfaceCNIDev:
			if err := setupTapUnderlay(ctx, client, perouterNetNS, params.TargetNS, iface); err != nil {
				return err
			}
		default:
			return fmt.Errorf("underlay interface has unsupported kind %q", iface.Kind)
		}
	}

	if params.TunnelEndpoint != nil {
		if len(params.UnderlayInterfaces) == 0 {
			return fmt.Errorf("tunnel endpoint configured but no underlay interfaces available")
		}
		firstPortName := PortName(params.UnderlayInterfaces[0])
		if err := setupTunnelEndpoint(ctx, client, firstPortName, *params.TunnelEndpoint); err != nil {
			return err
		}
	}

	return nil
}

func setupTapUnderlay(ctx context.Context, client *Client, perouterNetNS netns.NsHandle, targetNS string, iface hostnetwork.UnderlayInterface) error {
	switch iface.Kind {
	case hostnetwork.UnderlayInterfaceNetDev:
		if err := hostnetwork.SetupUnderlayNetDevInterface(ctx, perouterNetNS, iface); err != nil {
			return err
		}
	case hostnetwork.UnderlayInterfaceCNIDev:
		if err := hostnetwork.SetupUnderlayCNIDevInterface(ctx, targetNS, iface); err != nil {
			return err
		}
	}
	return netnamespace.In(perouterNetNS, func() error {
		return configureUnderlayPort(ctx, client, iface.InterfaceName, PortName(iface))
	})
}

func setupGroutPortUnderlay(ctx context.Context, client *Client, perouterNetNS netns.NsHandle, iface hostnetwork.UnderlayInterface) error {
	netlinkName := iface.InterfaceName

	slog.DebugContext(ctx, "setupGroutPortUnderlay", "netlinkName", netlinkName)

	devState, err := devicestate.Load(devicestate.Entry{NetlinkName: netlinkName})
	if err != nil {
		return fmt.Errorf("failed to load device state for %s: %w", netlinkName, err)
	}

	if devState.PCIAddress == "" {
		devState.PCIAddress, err = sriov.ResolveNetlinkName(netlinkName)
		if err != nil {
			return fmt.Errorf("failed to resolve PCI address for %s: %w", netlinkName, err)
		}
		if err := devicestate.Save(*devState); err != nil {
			return fmt.Errorf("failed to save device state for %s: %w", netlinkName, err)
		}
		devState.OriginalDriver, err = sriov.GetPCIDriver(devState.PCIAddress)
		if err != nil {
			return fmt.Errorf("failed to read driver for %s: %w", devState.PCIAddress, err)
		}

		link, err := netlink.LinkByName(netlinkName)
		if err != nil {
			return fmt.Errorf("failed to find kernel interface %s: %w", netlinkName, err)
		}
		devState.MTU = int32(link.Attrs().MTU)
		devState.AltNames = link.Attrs().AltNames

		netlinkAddrs, err := hostnetwork.AddressesForInterface(netlinkName, hostnetwork.ExcludeLinkLocal())
		if err != nil {
			return fmt.Errorf("failed to read addresses from %s: %w", netlinkName, err)
		}
		for _, a := range netlinkAddrs {
			devState.Addresses = append(devState.Addresses, a.IPNet.String())
		}
		if err := devicestate.Save(*devState); err != nil {
			return fmt.Errorf("failed to save device state for %s: %w", netlinkName, err)
		}
		slog.DebugContext(ctx, "saved device state", "state", fmt.Sprintf("%+v", devState))
	}

	if err := prepareGroutPortDriver(ctx, perouterNetNS, devState.PCIAddress, devState.NetlinkName); err != nil {
		return fmt.Errorf("failed to prepare grout port driver for %s: %w", devState.PCIAddress, err)
	}
	return netnamespace.In(perouterNetNS, func() error {
		return configureGroutPort(ctx, client, iface, devState.PCIAddress)
	})
}

func setupTunnelEndpoint(ctx context.Context, client *Client, portName string, ep hostnetwork.UnderlayTunnelEndpointParams) error {
	if err := assignIPsToGroutPort(ctx, client, portName,
		ep.IPv4CIDR, ep.IPv6CIDR); err != nil {
		return fmt.Errorf("failed to assign tunnel endpoint IPs to grout port %s: %w", portName, err)
	}

	return nil
}

func UnderlayInterfaces(ctx context.Context, client *Client, namespace string) ([]hostnetwork.UnderlayInterface, error) {
	ret, err := hostnetwork.UnderlayInterfaces(namespace)
	if err != nil {
		return nil, err
	}

	groutInterfaces, err := client.listInterfaces(ctx)
	if err != nil {
		return nil, err
	}

	for _, groutIface := range groutInterfaces {
		details, err := client.getInterfaceDetails(ctx, groutIface.Name)
		if err != nil {
			return nil, err
		}
		if !strings.Contains(details.Description, UnderlayInterfaceDescriptionMarker) {
			continue
		}
		iface, err := groutPortToUnderlayInterface(groutIface, details)
		if err != nil {
			return nil, err
		}
		ret = append(ret, iface)
	}
	return ret, nil
}

// groutPortToUnderlayInterface reconstructs the host underlay interface
// from a grout port. PCI-backed (DPDK) ports store the original netlink
// name in the device state file; TAP ports encode it in the grout name
// as "u_<InterfaceName>".
func groutPortToUnderlayInterface(groutIface groutInterface, details *groutInterfaceDetails) (hostnetwork.UnderlayInterface, error) {
	if !sriov.IsPCIAddress(details.Devargs) {
		return hostnetwork.UnderlayInterface{
			InterfaceName: strings.TrimPrefix(groutIface.Name, UnderlayPortNamePrefix),
			Kind:          hostnetwork.UnderlayInterfaceNetDev,
		}, nil
	}

	state, err := devicestate.LoadByPCI(details.Devargs)
	if err != nil {
		return hostnetwork.UnderlayInterface{}, fmt.Errorf("failed to load device state for grout port %s: %w", groutIface.Name, err)
	}
	if state.NetlinkName == "" {
		return hostnetwork.UnderlayInterface{}, fmt.Errorf("device state for grout port %s has no netlink name", groutIface.Name)
	}
	portName := groutIface.Name
	return hostnetwork.UnderlayInterface{
		InterfaceName: state.NetlinkName,
		Kind:          hostnetwork.UnderlayInterfaceNetDev,
		AcceleratedConfig: &hostnetwork.AcceleratedConfigParams{
			PortName: &portName,
		},
	}, nil
}

// RestoreUnderlay removes the given underlay interfaces: it tears
// down their grout ports first (migrating the underlay addresses back to the
// kernel interfaces), then removes the interfaces from the namespace
// according to how they were provisioned, exactly like the kernel datapath
// does.
func RestoreUnderlay(
	ctx context.Context,
	client *Client,
	targetNS string,
	toRemove []hostnetwork.UnderlayInterface,
) error {
	if len(toRemove) == 0 {
		return nil
	}

	interfaces, err := client.listInterfaces(ctx)
	if err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to list grout interfaces: %w", err)
	}
	existingPorts := map[string]struct{}{}
	for _, iface := range interfaces {
		existingPorts[iface.Name] = struct{}{}
	}

	ns, err := netns.GetFromPath(targetNS)
	if err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to find network namespace %s: %w", targetNS, err)
	}
	defer func() {
		if err := ns.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", targetNS, "error", err)
		}
	}()

	for _, iface := range toRemove {
		portName := PortName(iface)
		if _, found := existingPorts[portName]; !found {
			slog.Debug("RestoreUnderlay: port already removed", "namespace", targetNS, "port", portName)
			continue
		}

		switch iface.Kind {
		case hostnetwork.UnderlayInterfaceNetDev:
			if iface.AcceleratedConfig != nil {
				if err := teardownGroutPortUnderlay(ctx, client, ns, iface); err != nil {
					return err
				}
				continue
			}
			if err := teardownTapUnderlay(ctx, client, targetNS, ns, iface); err != nil {
				return err
			}
		case hostnetwork.UnderlayInterfaceCNIDev:
			if err := teardownTapUnderlay(ctx, client, targetNS, ns, iface); err != nil {
				return err
			}
		default:
			return fmt.Errorf("underlay interface has unsupported kind %q", iface.Kind)
		}
	}

	return nil
}

func removeGroutPortAddresses(ctx context.Context, client *Client, ns netns.NsHandle, portName string) error {
	return netnamespace.In(ns, func() error {
		addrs, err := client.getAddresses(ctx, portName)
		if err != nil {
			return fmt.Errorf("RestoreUnderlay: failed to get addresses for grout port %s: %w", portName, err)
		}
		for _, addr := range addrs {
			if err := removeKernelSubnetRoute("main", addr); err != nil {
				return fmt.Errorf("RestoreUnderlay: failed to remove kernel route for %s: %w", addr, err)
			}
			if err := client.deleteAddress(ctx, portName, addr); err != nil {
				return fmt.Errorf("RestoreUnderlay: failed to delete address %s from grout port %s: %w", addr, portName, err)
			}
		}
		return nil
	})
}

func teardownGroutPortUnderlay(ctx context.Context, client *Client, ns netns.NsHandle, iface hostnetwork.UnderlayInterface) error {
	slog.InfoContext(ctx, "tearing down grout port underlay", "interface", iface.InterfaceName)
	portName := PortName(iface)
	if err := removeGroutPortAddresses(ctx, client, ns, portName); err != nil {
		return err
	}

	if err := client.deletePort(ctx, portName); err != nil {
		slog.ErrorContext(ctx, "failed to delete grout port", "port", portName, "error", err)
	}

	netlinkName := iface.InterfaceName
	state, err := devicestate.Load(devicestate.Entry{NetlinkName: netlinkName})
	if err != nil {
		slog.WarnContext(ctx, "no saved device state, cannot restore driver/IPs",
			"netlinkName", netlinkName, "error", err)
		return nil
	}

	if state.PCIAddress != "" && state.OriginalDriver != "" && state.OriginalDriver != sriov.DriverVFIOPCI {
		if err := sriov.RestoreDriver(state.PCIAddress, state.OriginalDriver); err != nil {
			return fmt.Errorf("failed to restore driver %s on %s: %w",
				state.OriginalDriver, state.PCIAddress, err)
		}
		slog.InfoContext(ctx, "restored original driver",
			"pciAddress", state.PCIAddress, "driver", state.OriginalDriver)
	}

	if err := restoreKernelDevice(ctx, *state); err != nil {
		return err
	}

	if err := devicestate.Delete(devicestate.Entry{NetlinkName: netlinkName}); err != nil {
		return fmt.Errorf("failed to delete device state file for %s: %w", netlinkName, err)
	}

	return nil
}

func teardownTapUnderlay(ctx context.Context, client *Client, targetNS string, ns netns.NsHandle, iface hostnetwork.UnderlayInterface) error {
	err := netnamespace.In(ns, func() error {
		return migrateAddressesToKernel(ctx, client, PortName(iface), iface.InterfaceName)
	})
	if err != nil {
		return err
	}

	if err := client.deletePort(ctx, PortName(iface)); err != nil {
		slog.ErrorContext(ctx, "failed to delete grout port", "port", PortName(iface), "error", err)
	}

	if err := hostnetwork.RestoreUnderlay(ctx, targetNS, []hostnetwork.UnderlayInterface{iface}); err != nil {
		return fmt.Errorf("RestoreUnderlay: failed to clean kernel underlay state: %w", err)
	}

	// Clean up the saved device state now that addresses have been
	// successfully restored to the kernel interface and the underlay is
	// fully torn down.
	if err := devicestate.Delete(devicestate.Entry{NetlinkName: iface.InterfaceName}); err != nil {
		slog.WarnContext(ctx, "failed to delete saved device state",
			"interface", iface.InterfaceName, "error", err)
	}

	return nil
}

// restoreKernelDevice puts the kernel netdev back the way it was found:
// first the alternative names it carried, then its addresses. Restoring the
// names first matters because the configured name may itself be one of them,
// and nothing can look the device up by it until it is back.
func restoreKernelDevice(ctx context.Context, state devicestate.Entry) error {
	if len(state.AltNames) == 0 && len(state.Addresses) == 0 {
		return nil
	}

	link, err := restoredLink(state)
	if err != nil {
		return err
	}

	if err := restoreAltNames(ctx, link, state.AltNames); err != nil {
		return err
	}
	return restoreIPAddresses(ctx, link, state.Addresses)
}

// restoredLink finds the kernel netdev the saved state was scraped from.
// Rebinding the original driver builds a brand new netdev, so neither the
// configured name nor the primary name recorded at setup is guaranteed to
// resolve yet; the PCI address is the only identifier that survives the
// rebind. Callers run inside the reconcile loop, which retries, so a device
// the kernel has not finished probing is reported as an error rather than
// waited for.
func restoredLink(state devicestate.Entry) (netlink.Link, error) {
	name := state.NetlinkName
	if state.PCIAddress != "" {
		netDev, err := sriov.GetPCINetDevice(state.PCIAddress)
		if err != nil {
			return nil, fmt.Errorf("kernel netdev for PCI device %s not found, cannot restore it: %w",
				state.PCIAddress, err)
		}
		name = netDev
	}

	link, err := netlink.LinkByName(name)
	if err != nil {
		return nil, fmt.Errorf("kernel netdev [%s] not found, cannot restore it: %w", name, err)
	}
	return link, nil
}

// restoreAltNames re-adds the alternative names the device carried before it
// was handed to grout. udev only re-applies the names that come from one of
// its rules, so a name added by hand would otherwise be lost for good on the
// first teardown.
func restoreAltNames(ctx context.Context, link netlink.Link, altNames []string) error {
	for _, altName := range altNames {
		if slices.Contains(link.Attrs().AltNames, altName) {
			continue
		}
		if err := netlink.LinkAddAltName(link, altName); err != nil {
			// Names are unique across primary and alternative names alike,
			// so EEXIST means something else claimed it while the device was
			// bound to grout: report it rather than failing the teardown.
			if errors.Is(err, syscall.EEXIST) {
				slog.WarnContext(ctx, "alternative name is taken, not restoring",
					"altName", altName, "netlinkName", link.Attrs().Name)
				continue
			}
			return fmt.Errorf("failed to restore alternative name %s on kernel netdev [%s]: %w",
				altName, link.Attrs().Name, err)
		}
		slog.InfoContext(ctx, "restored alternative name to kernel netdev",
			"altName", altName, "netlinkName", link.Attrs().Name)
	}
	return nil
}

func restoreIPAddresses(ctx context.Context, link netlink.Link, addresses []string) error {
	for _, addr := range addresses {
		if err := hostnetwork.AssignIPToInterface(link, addr); err != nil {
			return fmt.Errorf("failed to restore IP address %s to kernel netdev [%s]: %w",
				addr, link.Attrs().Name, err)
		}
		slog.InfoContext(ctx, "restored IP addresses to kernel netdev",
			"netlinkName", link.Attrs().Name, "addresses", addr)
	}
	return nil
}

func configureUnderlayPort(ctx context.Context, client *Client, underlayInterface, portName string) error {
	underlayAddrs, err := hostnetwork.AddressesForInterface(underlayInterface, hostnetwork.ExcludeLinkLocal())
	if err != nil {
		return fmt.Errorf("failed to read underlay interface addresses: %w", err)
	}

	// If the kernel interface has global addresses, save them to a state
	// file before migration. If it doesn't (addresses were already
	// migrated to grout and then lost due to a grout crash), fall back to
	// restoring addresses from the saved state file and re-assigning them
	// to the kernel interface so they can be migrated again.
	if len(underlayAddrs) > 0 {
		var addrStrings []string
		for _, a := range underlayAddrs {
			addrStrings = append(addrStrings, a.IPNet.String())
		}
		if err := devicestate.Save(devicestate.Entry{
			NetlinkName: underlayInterface,
			Addresses:   addrStrings,
		}); err != nil {
			return fmt.Errorf("failed to save device state for %s: %w", underlayInterface, err)
		}
	} else {
		saved, err := devicestate.Load(devicestate.Entry{NetlinkName: underlayInterface})
		if err == nil && len(saved.Addresses) > 0 {
			slog.InfoContext(ctx, "kernel interface has no global addresses, "+
				"but there are saved addresses",
				"interface", underlayInterface, "addresses", saved.Addresses)

			underlayAddrs = make([]netlink.Addr, len(saved.Addresses))
			for i, addr := range saved.Addresses {
				parsed, err := netlink.ParseAddr(addr)
				if err != nil {
					return fmt.Errorf("failed to parse saved address %s: %w", addr, err)
				}
				underlayAddrs[i] = *parsed
			}
		}
	}

	link, err := netlink.LinkByName(underlayInterface)
	if err != nil {
		return fmt.Errorf("failed to find underlay interface %s: %w", underlayInterface, err)
	}
	mtu := int32(link.Attrs().MTU)

	devargs := fmt.Sprintf("net_tap%s,remote=%s,iface=%s", makeTapRandomString(), underlayInterface, "tap_"+underlayInterface)
	opts := PortOptions{MTU: &mtu, Description: UnderlayInterfaceDescriptionMarker}
	if err := client.ensurePortWithOptions(ctx, portName, devargs, opts); err != nil {
		return fmt.Errorf("failed to create grout underlay port: %w", err)
	}

	// Disable SLAAC on the underlying netlink interface: once the intended
	// addresses are migrated to the grout port, the kernel must not
	// autoconfigure new ones from Router Advertisements. A SLAAC address
	// on the netlink device would be picked up on the next reconcile and
	// clash with the existing connected route in grout's RIB (EBUSY).
	if err := sysctl.Ensure(sysctl.DisableAcceptRA(underlayInterface)); err != nil {
		return fmt.Errorf("failed to disable accept_ra on underlay interface %s: %w", underlayInterface, err)
	}

	if err := migrateAddressesToGrout(ctx, client, underlayInterface, portName, underlayAddrs); err != nil {
		return err
	}

	return nil
}

// configureGroutPort creates a DPDK port in grout directly from a PCI
// device address, loads the scraped IP addresses from the saved device
// state, and sets up the kernel routes needed by FRR.
func configureGroutPort(ctx context.Context, client *Client, iface hostnetwork.UnderlayInterface, pciAddr string) error {
	portName := PortName(iface)

	state, err := devicestate.Load(devicestate.Entry{NetlinkName: iface.InterfaceName})
	if err != nil {
		return fmt.Errorf("failed to load device state for %s: %w", iface.InterfaceName, err)
	}

	opts := PortOptions{
		RXQueues:    iface.AcceleratedConfig.RXQueues,
		QSize:       iface.AcceleratedConfig.QSize,
		Promiscuous: iface.AcceleratedConfig.Promiscuous,
		MAC:         iface.AcceleratedConfig.MAC,
		Description: UnderlayInterfaceDescriptionMarker,
	}
	if state.MTU > 0 {
		opts.MTU = &state.MTU
	}

	if err := client.ensurePortWithOptions(ctx, portName, pciAddr, opts); err != nil {
		return fmt.Errorf("failed to create grout DPDK port %s: %w", portName, err)
	}

	for _, addr := range state.Addresses {
		if err := client.ensureAddress(ctx, portName, addr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout port %s: %w", addr, portName, err)
		}

		if err := ensureKernelSubnetRoute("main", addr); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}

		slog.InfoContext(ctx, "configured grout DPDK port address", "cidr", addr, "port", portName)
	}

	if err := sysctl.Ensure(sysctl.DisableRPFilter(portName)); err != nil {
		return fmt.Errorf("failed to disable rp_filter on %s: %w", portName, err)
	}

	return nil
}

// prepareGroutPortDriver inspects the driver bound to a PCI device and
// takes the appropriate action:
//   - Intel kernel drivers (igb, iavf, ice, i40e): rebind to vfio-pci
//   - vfio-pci: already bound, nothing to do
//   - mlx5_core: move the kernel netlink interface to the perouter namespace (bifurcated driver)
//   - unknown/unbound: bind to vfio-pci
func prepareGroutPortDriver(ctx context.Context, perouterNetNS netns.NsHandle, pciAddr, netlinkName string) error {
	driver, err := sriov.GetPCIDriver(pciAddr)
	if err != nil {
		return fmt.Errorf("failed to get PCI driver for %s: %w", pciAddr, err)
	}

	switch {
	case sriov.IntelKernelDrivers[driver]:
		if err := sriov.BindVFIOPCI(pciAddr); err != nil {
			return fmt.Errorf("failed to rebind PCI device %s from %s to vfio-pci: %w",
				pciAddr, driver, err)
		}
		return nil

	case driver == sriov.DriverVFIOPCI:
		return nil

	case driver == sriov.DriverMlx5Core:
		name := netlinkName
		if name == "" {
			name, err = sriov.GetPCINetDevice(pciAddr)
			if err != nil {
				slog.WarnContext(ctx, "mlx5 PCI device has no kernel netlink interface", "pciAddress", pciAddr, "error", err)
				//return fmt.Errorf("mlx5 PCI device %s has no kernel netlink interface: %w", pciAddr, err)
			}
		}
		if err := hostnetwork.SetupUnderlayNetDevInterface(ctx, perouterNetNS, hostnetwork.UnderlayInterface{
			InterfaceName: name,
			Kind:          hostnetwork.UnderlayInterfaceNetDev,
		}); err != nil {
			// TODO: make this better
			slog.WarnContext(ctx, "failed to move mlx5 netlink device to namespace", "name", name, "error", err)
			// return fmt.Errorf("failed to move mlx5 netlink device %s to namespace: %w", name, err)
		}
		return nil

	default:
		slog.Info("binding GroutPort PCI device to vfio-pci",
			"pciAddress", pciAddr, "currentDriver", driver)
		if err := sriov.BindVFIOPCI(pciAddr); err != nil {
			return fmt.Errorf("failed to bind PCI device %s to vfio-pci: %w", pciAddr, err)
		}
		return nil
	}
}

// PrepareAndBindTrunkVF prepares the DPDK driver for a trunk VF and creates
// the grout port. It is idempotent: calling it for a PCI address that is
// already bound is a no-op.
func PrepareAndBindTrunkVF(ctx context.Context, client *Client, targetNS, pciAddr, portName string, opts PortOptions) error {
	perouterNetNS, err := netns.GetFromPath(targetNS)
	if err != nil {
		return fmt.Errorf("failed to get namespace %s: %w", targetNS, err)
	}
	defer func() {
		if err := perouterNetNS.Close(); err != nil {
			slog.Error("failed to close namespace", "namespace", targetNS, "error", err)
		}
	}()
	if err := prepareGroutPortDriver(ctx, perouterNetNS, pciAddr, ""); err != nil {
		return fmt.Errorf("failed to prepare trunk VF driver for %s: %w", pciAddr, err)
	}

	return client.ensurePortWithOptions(ctx, portName, pciAddr, opts)
}

func migrateAddressesToGrout(ctx context.Context, client *Client, kernelDevice, portName string, addrs []netlink.Addr) error {
	for _, addr := range addrs {
		cidr := addr.IPNet.String()

		if err := client.ensureAddress(ctx, portName, cidr); err != nil {
			return fmt.Errorf("failed to assign address %s to grout underlay port: %w", cidr, err)
		}

		if err := hostnetwork.DeleteAddressFromInterface(kernelDevice, addr); err != nil {
			slog.WarnContext(ctx, "failed to remove address from underlay interface", "cidr", cidr, "iface", kernelDevice, "error", err)
		}

		// FRR needs kernel routes to establish BGP connections. Grout requires that all the kernel
		// traffic must enter grout via the `main` TAP device.
		if err := ensureKernelSubnetRoute(defaultVRFName, addr.IPNet.String()); err != nil {
			return fmt.Errorf("failed to add kernel route for underlay subnet %s: %w", addr, err)
		}

		slog.InfoContext(ctx, "migrated underlay address to grout", "cidr", cidr, "iface", portName)
	}

	// for each port, grout creates a NOARP kernel interface to make FRR zebra daemon work.
	// 5: u_enp3s0: <BROADCAST,MULTICAST,NOARP,UP,LOWER_UP> mtu 1500 qdisc fq_codel state UP mode DEFAULT group default qlen 1000
	//    link/ether 00:09:a8:38:8e:3b brd ff:ff:ff:ff:ff:ff promiscuity 0 allmulti 0 minmtu 68 maxmtu 65521
	//    tun type tap ...
	//    alias Grout control plane interface
	// bgpd packets will leave through the `main` interface and will come back on the `u_xxx` interface, hence the
	// need to disable rp_filter on the `u_xxx` interface.
	if err := sysctl.Ensure(sysctl.DisableRPFilter(portName)); err != nil {
		return fmt.Errorf("failed to disable rp_filter on underlay interface %s: %w", portName, err)
	}

	return nil
}

func migrateAddressesToKernel(ctx context.Context, client *Client, underlayPortName, netlinkName string) error {
	addrs, err := client.getAddresses(ctx, underlayPortName)
	if err != nil {
		return fmt.Errorf("failed to read addresses from grout port %s: %w", underlayPortName, err)
	}
	if len(addrs) == 0 {
		return nil
	}

	link, err := netlink.LinkByName(netlinkName)
	if err != nil {
		return fmt.Errorf("failed to find link %s: %w", netlinkName, err)
	}

	for _, addr := range addrs {
		ip, _, err := net.ParseCIDR(addr)
		if err != nil {
			return fmt.Errorf("failed to parse address %s: %w", addr, err)
		}
		if ip.IsLinkLocalMulticast() || ip.IsLinkLocalUnicast() {
			continue
		}

		if err := hostnetwork.AssignIPToInterface(link, addr); err != nil {
			return fmt.Errorf("failed to assign address %s to link %s: %w", addr, netlinkName, err)
		}
	}

	for _, addr := range addrs {
		if err := removeKernelSubnetRoute(defaultVRFName, addr); err != nil {
			return fmt.Errorf("failed to remove kernel route for %s: %w", addr, err)
		}

		if err := client.deleteAddress(ctx, underlayPortName, addr); err != nil {
			return fmt.Errorf("failed to delete addresses on grout port %s: %w", underlayPortName, err)
		}

	}

	return nil
}

func ensureKernelSubnetRoute(ifaceName, addr string) error {
	route, err := connectedRouteForAddress(ifaceName, addr)
	if err != nil {
		return err
	}
	if route == nil {
		return nil
	}

	existing, err := netlink.RouteListFiltered(netlink.FAMILY_ALL, route, netlink.RT_FILTER_DST|netlink.RT_FILTER_OIF)
	if err != nil {
		return fmt.Errorf("failed to list routes for %s dev %s: %w", route.Dst, ifaceName, err)
	}
	if len(existing) > 0 {
		return nil
	}

	if err := netlink.RouteAdd(route); err != nil {
		return fmt.Errorf("failed to add route for %s dev %s: %w", route.Dst, ifaceName, err)
	}

	slog.Info("added kernel route for subnet", "cidr", addr, "src", route.Src, "ipnet", route.Dst, "iface", ifaceName)
	return nil
}

func removeKernelSubnetRoute(ifaceName, addr string) error {
	route, err := connectedRouteForAddress(ifaceName, addr)
	if err != nil {
		return err
	}
	if route == nil {
		return nil
	}

	if err := netlink.RouteDel(route); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			return nil
		}
		return fmt.Errorf("failed to delete route for %s dev %s: %w", route.Dst, ifaceName, err)
	}

	slog.Info("removed kernel route for subnet", "cidr", addr, "src", route.Src, "ipnet", route.Dst, "iface", ifaceName)
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

func connectedRouteForAddress(ifaceName, addr string) (*netlink.Route, error) {
	srcAddr, ipNet, err := net.ParseCIDR(addr)
	if err != nil {
		return nil, fmt.Errorf("failed to parse CIDR %s: %w", addr, err)
	}
	ones, bits := ipNet.Mask.Size()
	if ones == bits {
		return nil, nil
	}
	if srcAddr.IsLinkLocalUnicast() {
		return nil, nil
	}

	link, err := netlink.LinkByName(ifaceName)
	if err != nil {
		return nil, fmt.Errorf("failed to find interface %s: %w", ifaceName, err)
	}

	return &netlink.Route{
		Dst:       ipNet,
		LinkIndex: link.Attrs().Index,
		Src:       srcAddr,
	}, nil
}
