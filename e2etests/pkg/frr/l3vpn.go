// SPDX-License-Identifier:Apache-2.0

package frr

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"

	"github.com/openperouter/openperouter/api/v1alpha1"
	"github.com/openperouter/openperouter/e2etests/pkg/executor"
	"github.com/openperouter/openperouter/e2etests/pkg/ipfamily"
	"github.com/openperouter/openperouter/e2etests/pkg/openperouter"
)

const (
	HEncaps    EncapMode = "encap"
	HEncapsRed EncapMode = "encap.red"
)

type L3VPNData struct {
	VRFID         int    `json:"vrfId"`
	VRFName       string `json:"vrfName"`
	TableVersion  int    `json:"tableVersion"`
	RouterId      string `json:"routerId"`
	DefaultLocPrf int    `json:"defaultLocPrf"`
	LocalAS       int    `json:"localAS"`
	Routes        Route  `json:"routes"`
	TotalRoutes   int    `json:"totalRoutes"`
	TotalPaths    int    `json:"totalPaths"`
}

type Route struct {
	RouteDistinguishers RouteDistinguisherMap `json:"routeDistinguishers"`
}

type RouteDistinguisher string

type RouteDistinguisherMap map[RouteDistinguisher]BGPPathMap

type BGPPrefix string

type BGPPathMap map[BGPPrefix][]BGPPath

type BGPPath struct {
	ASPath            ASPath            `json:"aspath"`
	Origin            string            `json:"origin"`
	Metric            int               `json:"metric"`
	Valid             bool              `json:"valid"`
	Bestpath          BestPath          `json:"bestpath"`
	ExtendedCommunity ExtendedCommunity `json:"extendedCommunity"`
	Nexthops          []Nexthop         `json:"nexthops"`
	Peer              Peer              `json:"peer"`
}

type Peer struct {
	PeerID   string `json:"peerId"`
	RouterID string `json:"routerId"`
	Hostname string `json:"hostname"`
	Type     string `json:"type"`
}

type ASPath struct {
	String string `json:"string"`
}

type BestPath struct {
	Overall         bool   `json:"overall"`
	SelectionReason string `json:"selectionReason"`
}

type ipRoute struct {
	Destination string      `json:"dst"`
	Encap       *ipEncap    `json:"encap,omitempty"`
	Nexthops    []ipNexthop `json:"nexthops,omitempty"`
}

type ipNexthop struct {
	Encap ipEncap `json:"encap"`
}

type ipEncap struct {
	EncapType string `json:"encap_type"`
	EncapMode string `json:"mode"`
}

type EncapMode string

func L3VPNInfo(exec executor.Executor, family ipfamily.Family) (L3VPNData, error) {
	res, err := exec.Exec("vtysh", "-c", fmt.Sprintf("show bgp %s vpn detail json", family))
	if err != nil {
		return L3VPNData{}, fmt.Errorf("failed to query `show bgp %s vpn detail json`: %w. Output: %s",
			family, err, res)
	}

	l3vpnInfo, err := parseBGPVPNtoL3VPN([]byte(res))
	if err != nil {
		return L3VPNData{}, fmt.Errorf("failed to parse output of `show bgp %s vpn detail json`: %w. Output: %s",
			family, err, res)
	}
	return l3vpnInfo, nil
}

func (l3 L3VPNData) ContainsBGPRouteForL3VPN(prefix string, routerID string, importRTs []v1alpha1.RouteTarget) bool {
	for _, pathMap := range l3.Routes.RouteDistinguishers {
		for bgpPrefix, bgpPaths := range pathMap {
			if string(bgpPrefix) != prefix {
				continue
			}
			for _, bgpPath := range bgpPaths {
				rt := v1alpha1.RouteTarget(strings.TrimPrefix(bgpPath.ExtendedCommunity.String, "RT:"))
				if !slices.Contains(importRTs, rt) {
					continue
				}
				if bgpPath.Peer.RouterID != routerID {
					continue
				}
				return true
			}
		}
	}
	return false
}

// GetKernelRoute takes an executor, a vrf and a prefix (CIDR string) and returns
// the ipRoute if found, nil if no route could be found or an error in case of
// an issue with the input.
func GetKernelRoute(exec openperouter.RouterExecutor, vrf string, prefix string) (*ipRoute, error) {
	flag := ""
	switch ipfamily.ForCIDRString(prefix) {
	case ipfamily.IPv4:
		flag = "-4"
	case ipfamily.IPv6:
		flag = "-6"
	default:
		return nil, fmt.Errorf("unknown ip address family for prefix %q", prefix)
	}

	output, err := exec.Exec("ip", flag, "-j", "route", "show", "vrf", vrf, prefix)
	if err != nil {
		return nil, err
	}

	parsedRoutes, err := parseIPRoutes(output)
	if err != nil {
		return nil, err
	}

	if len(parsedRoutes) == 0 {
		return nil, nil
	}

	parsedRoute := parsedRoutes[0]

	// FRR may install a single nexthop or multiple. Either Encap is set (single nexthop) or Nexthops is
	// (multiple nexthops). For a single nexthop, let's build our own []ipNexthop so that the caller can
	// simply check Nexthops for either case.
	if parsedRoute.Encap != nil && len(parsedRoute.Nexthops) == 0 {
		parsedRoute.Nexthops = []ipNexthop{{Encap: *parsedRoute.Encap}}
	}

	return &parsedRoute, nil
}

func parseIPRoutes(input string) ([]ipRoute, error) {
	var routes []ipRoute
	if err := json.Unmarshal([]byte(input), &routes); err != nil {
		return nil, err
	}
	return routes, nil
}

func parseBGPVPNtoL3VPN(data []byte) (L3VPNData, error) {
	res := L3VPNData{}
	if err := json.Unmarshal(data, &res); err != nil {
		return L3VPNData{}, fmt.Errorf("error unmarshalling JSON: %v", err)
	}

	return res, nil
}

func GetGroutRoute(exec openperouter.RouterExecutor, vrf string, prefix string) (*ipRoute, error) {
	output, err := exec.Exec("grcli", "--err-exit", "--json", "route", "show", "vrf", vrf)
	if err != nil {
		return nil, err
	}

	return parseGroutRoutes(output, prefix)
}

func parseGroutRoutes(output string, prefix string) (*ipRoute, error) {
	var routes []groutRoute
	if err := json.Unmarshal([]byte(output), &routes); err != nil {
		return nil, fmt.Errorf("failed to parse grout route output: %w", err)
	}

	for _, route := range routes {
		if route.Destination != prefix {
			continue
		}

		result := &ipRoute{
			Destination: route.Destination,
		}

		nhFields := parseNextHopFields(route.NextHop)
		if nhFields["type"] == "SRv6" {
			result.Nexthops = []ipNexthop{{
				Encap: ipEncap{
					EncapType: "seg6",
					EncapMode: groutEncapToKernel(nhFields["encap"]),
				},
			}}
		}

		return result, nil
	}

	return nil, nil
}

type groutRoute struct {
	VRF         string `json:"vrf"`
	Family      string `json:"family"`
	Destination string `json:"destination"`
	Origin      string `json:"origin"`
	NextHop     string `json:"next_hop"`
}

func parseNextHopFields(nextHop string) map[string]string {
	fields := map[string]string{}
	for _, part := range strings.Fields(nextHop) {
		if key, value, ok := strings.Cut(part, "="); ok {
			fields[key] = value
		}
	}
	return fields
}

var groutEncapModes = map[string]EncapMode{
	"h.encaps":     HEncaps,
	"h.encaps.red": HEncapsRed,
}

func groutEncapToKernel(groutMode string) string {
	if mode, ok := groutEncapModes[groutMode]; ok {
		return string(mode)
	}
	return groutMode
}
