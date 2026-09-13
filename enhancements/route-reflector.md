# Enhancement: Internal iBGP Route Reflector

## Summary

In environments where the external network does not support distributing routes between router pods via the fabric or can't be changed to do so, the only option is a full-mesh iBGP topology, which does not scale.

This enhancement adds an internal iBGP Route Reflector for route distribution between router pods:

- **East/West**: iBGP via an internal RR. Data plane goes directly between nodes.
- **North/South**: eBGP with the ToR for IPv4/IPv6 (unchanged).

No dedicated FRR pods are added. The hostcontroller natively configures the local FRR process as a route reflector when its Underlay CR contains a `routeReflector` section. Users add the RR node IPs as iBGP neighbors in client Underlay CRs to enable east/west — no new CRDs.

## Motivation

- **Cloud environments**: the ToR cannot be used for east/west traffic
- **Hybrid deployments**: depending on the on-prem ToR for route reflection adds latency and cost to every East/West flow
- **Full-mesh iBGP**: N*(N-1)/2 sessions, does not scale

### Goals

- Enable East/West without relying on the ToR
- Keep East/West data plane traffic within the cluster (no hairpin through the ToR)
- HA via multiple RR nodes (typically control plane nodes, 3 in production)
- No new CRDs — opt-in by adding a `routeReflector` section to the RR nodes' Underlay CR and adding RR node IPs as iBGP neighbors in client Underlay CRs

### Non-Goals

- Exposing RR peering to external routers
- Configure no-rib if nodes are pure router reflectors so they are not going to forward traffic, 
  an [issue](https://github.com/openperouter/openperouter/issues/482) has being open to do follow up if needed 

## Design

The primary use case for the internal RR is environments where the ToR cannot be used for east/west traffic. In cloud deployments, control plane and worker nodes are commonly placed on different subnets. The examples throughout this document use this multi-subnet topology as it is the most complex case and the one the RR is originally designed to address.

### How It Works

When the Underlay CR contains a `routeReflector` section, the hostcontroller generates the RR-specific FRR configuration (`bgp listen range`, `route-reflector-client` peer-group, `bgp cluster-id`) through the existing template and conversion pipeline. All RR nodes sharing the same Underlay CR get the same `clusterID` value (default `10.0.1.1`), which is required so that inter-RR CLUSTER_LIST loop detection works correctly and clients do not receive duplicate routes (RFC 4456 §7).

**Deployment**: Any node with a router pod whose Underlay CR contains a `routeReflector` section becomes an RR. The user is responsible for ensuring the router DaemonSet is scheduled on those nodes (e.g., via tolerations).

RR nodes get their own Underlay CR with a `routeReflector` section.

**Client configuration**: Users configure the openperouter instances to connect to the instances configured as route reflectors. The FRR templates already handle iBGP correctly.

**Inter-RR peering**: Standard iBGP — users list all RR node IPs as `type: internal` neighbors with `address` in the RR Underlay CR. FRR explicit `neighbor` takes precedence over a matching `bgp listen range`, so inter-RR sessions are not treated as route-reflector clients.

**`bgp listen range`**: Users configure iBGP neighbors with `listenRange` (instead of `address`) and `routeReflectorClient` — set either at the session level (`properties`), which applies it to every activated address family, or scoped to a specific family (`addressFamilies[].properties`, e.g. EVPN) — in the RR Underlay CR. The hostcontroller generates a `bgp listen range` stanza for each `listenRange` neighbor, and marks the corresponding peer-group as `route-reflector-client` in the corresponding address families. All ranges are explicit — no auto-derivation — which keeps the hostcontroller simple and supports multi-NIC / multi-subnet environments.



### API

Two changes to the existing API:

1. A new `routeReflector` field on `UnderlaySpec` holding the shared `clusterID`.
2. Three new fields on `Neighbor`: `listenRange` for dynamic neighbor acceptance (`bgp listen range`), `properties` for session-level features, and `addressFamilies` for per-address-family activation and features ([PR #316](https://github.com/openperouter/openperouter/pull/316), [PR #341](https://github.com/openperouter/openperouter/pull/341)).

Properties use a discriminated-struct list (`[]NeighborProperty`, `+listType=map +listMapKey=type`) at two tiers: `Neighbor.properties` for session-level settings (e.g. `ebgpMultiHop`) and `NeighborAddressFamily.properties` for per-AF settings (e.g. `routeReflectorClient`). A struct list is required because Kubernetes structural schemas forbid mixed enum-or-struct items, and valued properties like `ebgpMultiHop` need typed sub-fields. The shared `NeighborPropertyType` enum with CEL placement guards keeps one type definition while enforcing valid scope. This enhancement ships `routeReflectorClient` (session or per-AF) and folds `ebgpMultiHop` (session) from [PR #341](https://github.com/openperouter/openperouter/pull/341); the same pattern extends to future properties without breaking changes. `AddressFamilyType` and the `addressFamilies` list structure are owned by [PR #316](https://github.com/openperouter/openperouter/pull/316).

```go
// UnderlaySpec defines the desired state of Underlay.
type UnderlaySpec struct {
	// ...existing fields...

	// routeReflector configures the local FRR process as a BGP route reflector.
	// When set, the hostcontroller generates bgp cluster-id from clusterID
	// and derives bgp listen range and route-reflector-client stanzas from
	// neighbors with listenRange and the routeReflectorClient property.
	// Omit to run as a standard router without route reflection.
	// +optional
	RouteReflector *RouteReflectorConfig `json:"routeReflector,omitempty"`
}

// RouteReflectorConfig holds BGP Route Reflector parameters (RFC 4456).
// Its presence on the Underlay enables route reflection on matching nodes.
type RouteReflectorConfig struct {
	// clusterID is the BGP cluster-id shared by all RR nodes in the same
	// cluster (RFC 4456 §7). All RRs serving the same set of clients must
	// use the same value so that CLUSTER_LIST loop detection prevents
	// duplicate route reflection. The default (10.0.1.1) is outside the
	// default routeridcidr range (10.0.0.0/24) to avoid collisions.
	// +default="10.0.1.1"
	// +kubebuilder:validation:XValidation:rule="isIP(self)",message="must be a valid IPv4 address"
	// +optional
	ClusterID *string `json:"clusterID,omitempty"`
}

// NeighborPropertyType defines an optional feature on a Neighbor.
// Properties are set at the session level (Neighbor.Properties) or per address
// family (NeighborAddressFamily.Properties). CEL rules enforce valid placement.
// +kubebuilder:validation:Enum=ebgpMultiHop;routeReflectorClient
type NeighborPropertyType string

// EBGPMultiHopProperties holds parameters for the ebgpMultiHop property.
type EBGPMultiHopProperties struct {
	// ttl is the maximum number of hops for the eBGP multihop session.
	// When omitted, FRR defaults to 255.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=255
	// +optional
	TTL *int32 `json:"ttl,omitempty"`
}

// NeighborProperty is an optional feature applied to a neighbor session or
// address family. The type field selects the property; typed sub-fields hold
// parameters for properties that require them.
// +kubebuilder:validation:XValidation:rule="!has(self.ebgpMultiHop) || self.type == 'ebgpMultiHop'",message="ebgpMultiHop parameters require type ebgpMultiHop"
type NeighborProperty struct {
	// type selects the property.
	// +required
	Type NeighborPropertyType `json:"type"`

	// ebgpMultiHop holds parameters for the ebgpMultiHop property.
	// May only be set when type is ebgpMultiHop.
	// +optional
	EBGPMultiHop *EBGPMultiHopProperties `json:"ebgpMultiHop,omitempty"`
}

// AddressFamilyType identifies a BGP address family.
// Owned by PR #316; listed here for context.
// +kubebuilder:validation:Enum=ipv4unicast;ipv6unicast;evpn;ipv4vpn;ipv6vpn
type AddressFamilyType string

// NeighborAddressFamily activates an address family on a neighbor with
// optional per-AF features.
// +kubebuilder:validation:XValidation:rule="!has(self.properties) || !self.properties.exists(o, o.type == 'ebgpMultiHop')",message="ebgpMultiHop is session-level only, not per address family"
type NeighborAddressFamily struct {
	// type selects the address family.
	// +required
	Type AddressFamilyType `json:"type"`

	// properties is the set of optional features for this address family.
	// +optional
	// +listType=map
	// +listMapKey=type
	Properties []NeighborProperty `json:"properties,omitempty"`
}

// Neighbor represents a BGP Neighbor we want FRR to connect to.
// +kubebuilder:validation:XValidation:rule="!has(self.listenRange) || !has(self.address)",message="listenRange and address are mutually exclusive"
// +kubebuilder:validation:XValidation:rule="!has(self.listenRange) || (has(self.type) && self.type == 'internal')",message="listenRange requires type internal"
// +kubebuilder:validation:XValidation:rule="!(has(self.properties) && self.properties.exists(o, o.type == 'routeReflectorClient')) && !(has(self.addressFamilies) && self.addressFamilies.exists(af, has(af.properties) && af.properties.exists(o, o.type == 'routeReflectorClient'))) || (has(self.type) && self.type == 'internal')",message="routeReflectorClient requires type internal"
type Neighbor struct {
	// ...existing fields (type, address, asn, etc.)...

	// listenRange is a CIDR range for dynamic neighbor acceptance via FRR
	// bgp listen range. When set, the hostcontroller generates a
	// "bgp listen range <listenRange> peer-group <name>" stanza instead of
	// an explicit neighbor statement. Mutually exclusive with address.
	// +kubebuilder:validation:XValidation:rule="isCIDR(self)",message="must be a valid CIDR"
	// +optional
	ListenRange *string `json:"listenRange,omitempty"`

	// properties is the set of optional session-level features for this
	// neighbor. ebgpMultiHop applies to the BGP session. routeReflectorClient
	// set here is applied to every address family activated in
	// addressFamilies; to enable it for specific families only, set it under
	// addressFamilies[].properties instead.
	// +optional
	// +listType=map
	// +listMapKey=type
	Properties []NeighborProperty `json:"properties,omitempty"`

	// addressFamilies is the list of address families to activate on
	// this neighbor, each with optional per-AF features.
	// Owned by PR #316; extended here with per-AF properties.
	// +optional
	// +listType=map
	// +listMapKey=type
	AddressFamilies []NeighborAddressFamily `json:"addressFamilies,omitempty"`
}
```

The two-tier properties model in YAML:

```yaml
neighbors:
  - type: internal
    address: 10.0.0.5
    properties:                          # session-level (applies to the whole BGP session)
      - type: ebgpMultiHop
        ebgpMultiHop:
          ttl: 3
    addressFamilies:
      - type: evpn
        properties:                      # per-AF (applies only to this address family)
          - type: routeReflectorClient
      - type: ipv4unicast               # no per-AF properties on this family
```

### Session Topology

```
            ToR (eBGP)
               |
  +------------+------------+
  |            |            |
cp-1 (RR) <--iBGP--> cp-2 (RR)       (explicit full mesh between RRs)
  ^  bgp listen range       ^
  |  route-reflector-client  |
  +----------+--------------+
             |
   worker-1 (client)    worker-2 (client)    (connect to all RRs)
```

- **Clients -> RRs**: clients configure RR IPs as explicit iBGP neighbors via the Underlay CR. RRs accept passively via `bgp listen range` covering all underlay subnets.
- **RR <-> RR**: standard iBGP neighbors from the RR Underlay CR. Explicit `neighbor` takes precedence over `bgp listen range`.
- **All nodes -> ToR**: eBGP unchanged.
- **Path selection**: iBGP (local-preference 100) beats the longer eBGP AS path, so VXLAN goes directly between nodes rather than hairpinning through the ToR.
- **RR-only nodes**: when RR nodes do not participate in the data plane, they do NOT have a ToR eBGP session — they only do route reflection.

### Example: 4-Node Dual-Stack Cluster

Setup: ASN 64514, ToR at 192.168.11.2 / fd00:11::2 (ASN 64512). Control plane nodes are on a separate subnet (192.168.10.0/24, fd00:10::/64) from workers (192.168.11.0/24, fd00:11::/64), as is typical in cloud environments.

| Node | Role | Underlay NIC IPs | VTEP IP |
|------|------|-----------------|---------|
| cp-1 | RR (control plane) | 192.168.10.3/24, fd00:10::3/64 | 100.65.0.0 |
| cp-2 | RR (control plane) | 192.168.10.4/24, fd00:10::4/64 | 100.65.0.1 |
| worker-1 | client | 192.168.11.5/24, fd00:11::5/64 | 100.65.0.2 |
| worker-2 | client | 192.168.11.6/24, fd00:11::6/64 | 100.65.0.3 |

#### Underlay CRs — CP nodes with full routing

When control plane nodes participate in the data plane, they get a full Underlay with ToR. Workers include the control plane RR IPs as iBGP neighbors. The control plane Underlay uses `listenRange` neighbors with `routeReflectorClient` in their EVPN address family for dynamic client acceptance, and `address` neighbors for inter-RR peering — each hostcontroller filters out its own IP:

```yaml
# Worker Underlay
apiVersion: openpe.openperouter.github.io/v1alpha1
kind: Underlay
metadata:
  name: underlay-workers
  namespace: openperouter-system
spec:
  nodeSelector:
    matchExpressions:
      - key: node-role.kubernetes.io/control-plane
        operator: DoesNotExist
  asn: 64514
  tunnelEndpoint:
    cidrs:
      - 100.65.0.0/24
  nics:
    - toswitch
  neighbors:
    - asn: 64512                       # ToR (eBGP, IPv4)
      address: 192.168.11.2
    - asn: 64512                       # ToR (eBGP, IPv6)
      address: fd00:11::2
    - type: internal                   # cp-1 RR (iBGP, IPv4)
      address: 192.168.10.3
    - type: internal                   # cp-1 RR (iBGP, IPv6)
      address: fd00:10::3
    - type: internal                   # cp-2 RR (iBGP, IPv4)
      address: 192.168.10.4
    - type: internal                   # cp-2 RR (iBGP, IPv6)
      address: fd00:10::4
---
# Control plane Underlay (full routing + RR)
apiVersion: openpe.openperouter.github.io/v1alpha1
kind: Underlay
metadata:
  name: underlay-controlplane
  namespace: openperouter-system
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/control-plane: ""
  asn: 64514
  tunnelEndpoint:
    cidrs:
      - 100.65.0.0/24
  nics:
    - toswitch
  routeReflector:
    clusterID: 10.0.1.1                # optional, shown for clarity
  neighbors:
    - asn: 64512                       # ToR (eBGP, IPv4)
      address: 192.168.10.2
    - asn: 64512                       # ToR (eBGP, IPv6)
      address: fd00:10::2
    - type: internal                   # dynamic RR clients (CP subnet, IPv4)
      listenRange: 192.168.10.0/24
      addressFamilies:
        - type: evpn
          properties:
            - type: routeReflectorClient
    - type: internal                   # dynamic RR clients (worker subnet, IPv4)
      listenRange: 192.168.11.0/24
      addressFamilies:
        - type: evpn
          properties:
            - type: routeReflectorClient
    - type: internal                   # dynamic RR clients (CP subnet, IPv6)
      listenRange: fd00:10::/64
      addressFamilies:
        - type: evpn
          properties:
            - type: routeReflectorClient
    - type: internal                   # dynamic RR clients (worker subnet, IPv6)
      listenRange: fd00:11::/64
      addressFamilies:
        - type: evpn
          properties:
            - type: routeReflectorClient
    - type: internal                   # cp-1 (inter-RR, filtered on cp-1)
      address: 192.168.10.3
    - type: internal                   # cp-1 (inter-RR, IPv6, filtered on cp-1)
      address: fd00:10::3
    - type: internal                   # cp-2 (inter-RR, filtered on cp-2)
      address: 192.168.10.4
    - type: internal                   # cp-2 (inter-RR, IPv6, filtered on cp-2)
      address: fd00:10::4
```

#### Underlay CRs — CP nodes as RR only

When control plane nodes only do route reflection (no data plane), workers get the ToR + RR neighbors. Control plane nodes get an Underlay with NIC, ASN, `listenRange` neighbors with `routeReflectorClient` in their EVPN address family, and inter-RR `address` neighbors (no ToR, no `tunnelEndpoint`):

```yaml
# Worker Underlay
apiVersion: openpe.openperouter.github.io/v1alpha1
kind: Underlay
metadata:
  name: underlay-workers
  namespace: openperouter-system
spec:
  nodeSelector:
    matchExpressions:
      - key: node-role.kubernetes.io/control-plane
        operator: DoesNotExist
  asn: 64514
  tunnelEndpoint:
    cidrs:
      - 100.65.0.0/24
  nics:
    - toswitch
  neighbors:
    - asn: 64512                       # ToR (eBGP, IPv4)
      address: 192.168.11.2
    - asn: 64512                       # ToR (eBGP, IPv6)
      address: fd00:11::2
    - type: internal                   # cp-1 RR (iBGP, IPv4)
      address: 192.168.10.3
    - type: internal                   # cp-1 RR (iBGP, IPv6)
      address: fd00:10::3
    - type: internal                   # cp-2 RR (iBGP, IPv4)
      address: 192.168.10.4
    - type: internal                   # cp-2 RR (iBGP, IPv6)
      address: fd00:10::4
---
# Control plane Underlay (RR only, no data plane)
apiVersion: openpe.openperouter.github.io/v1alpha1
kind: Underlay
metadata:
  name: underlay-controlplane
  namespace: openperouter-system
spec:
  nodeSelector:
    matchLabels:
      node-role.kubernetes.io/control-plane: ""
  asn: 64514
  nics:
    - toswitch
  routeReflector:
    clusterID: 10.0.1.1
  neighbors:
    - type: internal                   # dynamic RR clients (CP subnet, IPv4)
      listenRange: 192.168.10.0/24
      addressFamilies:
        - type: evpn
          properties:
            - type: routeReflectorClient
    - type: internal                   # dynamic RR clients (worker subnet, IPv4)
      listenRange: 192.168.11.0/24
      addressFamilies:
        - type: evpn
          properties:
            - type: routeReflectorClient
    - type: internal                   # dynamic RR clients (CP subnet, IPv6)
      listenRange: fd00:10::/64
      addressFamilies:
        - type: evpn
          properties:
            - type: routeReflectorClient
    - type: internal                   # dynamic RR clients (worker subnet, IPv6)
      listenRange: fd00:11::/64
      addressFamilies:
        - type: evpn
          properties:
            - type: routeReflectorClient
    - type: internal                   # cp-1 (inter-RR, filtered on cp-1)
      address: 192.168.10.3
    - type: internal                   # cp-1 (inter-RR, IPv6, filtered on cp-1)
      address: fd00:10::3
    - type: internal                   # cp-2 (inter-RR, filtered on cp-2)
      address: 192.168.10.4
    - type: internal                   # cp-2 (inter-RR, IPv6, filtered on cp-2)
      address: fd00:10::4
```

#### Key FRR Additions on RR Nodes

The hostcontroller generates the following RR-specific FRR stanzas through the template and conversion pipeline. These are added on top of the normal router configuration (or standalone for RR-only nodes):

**Cluster ID** — all RR nodes in the same cluster must share the same cluster-id so that CLUSTER_LIST loop detection works and clients do not receive duplicate reflected routes (RFC 4456 §7). The value comes from `routeReflector.clusterID` (default `10.0.1.1`):

```
bgp cluster-id 10.0.1.1
```

**Peer-group name generation** — FRR requires a peer-group for `bgp listen range`. The hostcontroller generates one peer-group per `listenRange` neighbor, deriving the name by sanitizing the CIDR: replace every `.`, `:`, and `/` with `-`, then prefix with `PG-`. The substitution is per character, so an IPv6 `::` becomes `--` and the trailing `/` adds one more `-` (e.g. `fd00:10::/64` → `PG-fd00-10---64`); because each separator maps to its own `-`, distinct CIDRs always produce distinct peer-group names. Examples:

| CIDR | Peer-group name |
|------|----------------|
| `192.168.10.0/24` | `PG-192-168-10-0-24` |
| `192.168.11.0/24` | `PG-192-168-11-0-24` |
| `fd00:10::/64` | `PG-fd00-10---64` |
| `fd00:11::/64` | `PG-fd00-11---64` |

**Dynamic client acceptance** — for each `listenRange` neighbor, the hostcontroller generates a peer-group with a `bgp listen range` stanza. Each CIDR gets its own peer-group so that per-neighbor settings (options, timers, etc.) remain independent:

```
neighbor PG-192-168-10-0-24 peer-group
neighbor PG-192-168-10-0-24 remote-as internal
bgp listen range 192.168.10.0/24 peer-group PG-192-168-10-0-24

neighbor PG-192-168-11-0-24 peer-group
neighbor PG-192-168-11-0-24 remote-as internal
bgp listen range 192.168.11.0/24 peer-group PG-192-168-11-0-24

neighbor PG-fd00-10---64 peer-group
neighbor PG-fd00-10---64 remote-as internal
bgp listen range fd00:10::/64 peer-group PG-fd00-10---64

neighbor PG-fd00-11---64 peer-group
neighbor PG-fd00-11---64 remote-as internal
bgp listen range fd00:11::/64 peer-group PG-fd00-11---64
```

**Route reflection** — because the `listenRange` neighbors have `routeReflectorClient` in their EVPN address family properties, the hostcontroller marks each peer-group as `route-reflector-client` in the corresponding address families (setting `routeReflectorClient` at the session level instead would fan it out to every activated address family):

```
address-family l2vpn evpn
  neighbor PG-192-168-10-0-24 activate
  neighbor PG-192-168-10-0-24 route-reflector-client
  neighbor PG-192-168-11-0-24 activate
  neighbor PG-192-168-11-0-24 route-reflector-client
  neighbor PG-fd00-10---64 activate
  neighbor PG-fd00-10---64 route-reflector-client
  neighbor PG-fd00-11---64 activate
  neighbor PG-fd00-11---64 route-reflector-client
exit-address-family
```

#### Client Nodes

Client nodes use standard iBGP neighbors pointing at the RR IPs (configured via the Underlay CR). No RR-specific FRR stanzas are needed on clients.

#### BGP sessions at runtime — CP node as RR + normal router (cp-1)

```
L2VPN EVPN Summary (BGP router identifier 10.0.0.1, local AS 64514):

Neighbor        V    AS   Up/Down  State/PfxRcd  Desc
*192.168.11.5   4  64514  00:25:56           5   <- dynamic client (worker-1, IPv4)
*192.168.11.6   4  64514  00:25:56           2   <- dynamic client (worker-2, IPv4)
*fd00:11::5     4  64514  00:25:56           5   <- dynamic client (worker-1, IPv6)
*fd00:11::6     4  64514  00:25:56           2   <- dynamic client (worker-2, IPv6)
192.168.10.2    4  64512  00:25:56           9   <- ToR (eBGP, IPv4)
fd00:10::2      4  64512  00:25:56           9   <- ToR (eBGP, IPv6)
192.168.10.4    4  64514  00:25:56           9   <- other RR cp-2 (explicit, IPv4)
fd00:10::4      4  64514  00:25:56           9   <- other RR cp-2 (explicit, IPv6)

* = dynamic neighbor accepted via bgp listen range
```

#### BGP sessions at runtime — CP node as RR only (cp-1)

```
L2VPN EVPN Summary (BGP router identifier 10.0.0.1, local AS 64514):

Neighbor        V    AS   Up/Down  State/PfxRcd  Desc
*192.168.11.5   4  64514  00:25:56           5   <- dynamic client (worker-1, IPv4)
*192.168.11.6   4  64514  00:25:56           2   <- dynamic client (worker-2, IPv4)
*fd00:11::5     4  64514  00:25:56           5   <- dynamic client (worker-1, IPv6)
*fd00:11::6     4  64514  00:25:56           2   <- dynamic client (worker-2, IPv6)
192.168.10.4    4  64514  00:25:56           9   <- other RR cp-2 (explicit, IPv4)
fd00:10::4      4  64514  00:25:56           9   <- other RR cp-2 (explicit, IPv6)

* = dynamic neighbor accepted via bgp listen range
```

#### BGP sessions at runtime — client node (worker-1)

```
L2VPN EVPN Summary (BGP router identifier 10.0.0.2, local AS 64514):

Neighbor        V    AS   Up/Down  State/PfxRcd  Desc
192.168.11.2    4  64512  00:25:56           9   <- ToR (eBGP, IPv4)
fd00:11::2      4  64512  00:25:56           9   <- ToR (eBGP, IPv6)
192.168.10.3    4  64514  00:25:56           9   <- RR cp-1 (IPv4)
fd00:10::3      4  64514  00:25:56           9   <- RR cp-1 (IPv6)
192.168.10.4    4  64514  00:25:56           9   <- RR cp-2 (IPv4)
fd00:10::4      4  64514  00:25:56           9   <- RR cp-2 (IPv6)
```

### Controller Reconcile Logic

**Hostcontroller** (existing, extended with RR logic):

- Watches: Nodes, Underlays, L2VNIs, L3VNIs, Pods (router)
- When the Underlay CR contains a `routeReflector` section, the hostcontroller additionally:
  - Filters out `address` neighbors matching its own IP (self-filtering)
  - Generates `bgp cluster-id` from `routeReflector.clusterID` (default `10.0.1.1`)
  - Collects neighbors with `listenRange` + `routeReflectorClient` in their address family properties → generates one peer-group with a `bgp listen range` per CIDR and `route-reflector-client` in the corresponding AFs
  - All through the existing FRR template and conversion pipeline
- Processes `address` neighbors as before (including inter-RR iBGP neighbors and client iBGP neighbors from client Underlay CRs)

### Failure and Recovery

RR nodes are typically stable (e.g., control plane nodes) and do not get rescheduled. DaemonSets restart pods on the same node.

**RR node goes down** (e.g., cp-1 fails):

1. **Client nodes**: iBGP sessions to cp-1 time out (based on hold-time, or faster with BFD). Sessions to cp-2 (surviving RR) stay up — **no traffic disruption**.
2. **cp-2 RR**: iBGP session to cp-1 drops. cp-2 continues reflecting routes from all clients.
3. **When cp-1 recovers**: DaemonSet recreates the router pod, FRR restarts, sessions re-establish.

**Router pod crash on RR node** (node is fine):

1. DaemonSet restarts the pod. The hostcontroller regenerates the RR configuration on startup.
2. Brief BGP session flap during restart.

Convergence is bounded by BGP hold-time (default 180s, or less with BFD) for session failover, and pod restart time for recovery.

### Systemd Mode

No special handling is needed. All RR logic lives in the Underlay spec, and the static configuration files use the same `spec` schema as the Kubernetes CRs. The hostcontroller processes the `routeReflector` section through the same conversion and template pipeline regardless of whether the Underlay comes from a static file or the API server.

### Testing

**Unit tests** (`internal/frr/`, `internal/conversion/`):

- FRR template rendering: RR stanzas (`bgp cluster-id`, `bgp listen range`, `route-reflector-client`) rendered correctly from `listenRange` + `routeReflectorClient` neighbors (golden file tests)
- Conversion: an Underlay with a `routeReflector` section generates the `bgp cluster-id` and RR peer-group stanzas; an Underlay without it generates none
- Neighbors with `listenRange` produce `bgp listen range` stanzas, neighbors with `address` produce explicit `neighbor` statements

**E2e tests** (`e2etests/tests/`, Ginkgo suite against a 4-node clab cluster):

- **BGP sessions**: all iBGP sessions to RR nodes established, dynamic clients accepted via `bgp listen range` (`*` prefix in vtysh output)
- **Dual stack listen range**: verify that `bgp listen range` covers all `listenRange` neighbors with `routeReflectorClient` in their EVPN address family to support dual stack scenarios
- **Route reflection**: routes on client nodes show `*>i` (iBGP best path from RR), not the eBGP path from the ToR
- **East/West data plane**: ping between workloads on different client nodes, VXLAN packets go directly node-to-node (zero packets via ToR IP)
- **Failover**: delete one router pod on an RR node -> verify DaemonSet recreates it, surviving RR keeps sessions up, clients reconverge

## Alternatives Considered

- **Standalone FRR RR Pods**: requires cluster-to-underlay routing, adds separate FRR processes, pod IPs unstable
- **Full-mesh iBGP**: N*(N-1)/2 sessions, does not scale
- **FRR-K8s as RR**: cross-system coordination, upgrade risk
- **Scheduler-placed RR Deployment**: 2-replica Deployment with anti-affinity landing on scheduler-selected nodes. Required auto-discovery mechanism (hostcontroller listing RR controller pods, reading IP annotations, extending the pod cache). Complex rescheduling logic when pods get evicted. Replaced by Underlay-driven RR configuration for simplicity.
- **Auto-discovery of RR IPs**: hostcontroller listing RR controller pods and reading annotations to discover RR node IPs dynamically. Replaced by explicit Underlay CR neighbor configuration — users list all RR IPs as `type: internal` neighbors, each hostcontroller filters out its own IP. Reduces controller complexity and eliminates cross-pod watches.

## References

- RFC 4456 — BGP Route Reflection
- FRR `bgp listen range` — https://docs.frrouting.org/en/latest/bgp.html
- [PR #341](https://github.com/openperouter/openperouter/pull/341) — API improvements (`ebgpMultiHop` folded into shared `NeighborProperty` struct list)
- [PR #316](https://github.com/openperouter/openperouter/pull/316) — SRv6 enhancement (per-neighbor `addressFamilies` struct with `AddressFamilyType` enum)
