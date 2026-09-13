# API Improvements

## Summary

This enhancement covers API improvements to the OpenPERouter CRDs. It
consolidates breaking redesigns (acceptable in `v1alpha1`), important fixes,
and quality improvements into a single document. Items already handled by the
kube-api-linter ([PR #313](https://github.com/openperouter/openperouter/pull/313))
are out of scope. Status subresources are tracked in a
[separate enhancement](status-crd.md).

## Motivation

### Goals

- Redesign API fields whose current shape would lock in bad semantics.
- Fix security, usability, and validation gaps.
- Establish a stable API group name before it becomes immutable.

### Non-Goals

- Status reporting and configuration resilience (see [status-crd.md](status-crd.md)).
- Linter-enforceable fixes: SSA markers, JSON tag casing, required/optional
  consistency, integer type widths, duration types, godoc field-name prefixes
  (all covered by [PR #313](https://github.com/openperouter/openperouter/pull/313)).

## Proposal

This document focuses on the **shape and semantics** of the API. Where a field
or struct needs validation, the proposal states the *intent* (what must be
enforced) rather than a concrete mechanism. Whether a given rule is implemented
as CEL (`+kubebuilder:validation:XValidation`), schema markers
(`Pattern`, `Minimum`, `MaxItems`, …), a Go webhook, or a combination is an
implementation detail decided during implementation — cross-resource and
conditional/parsing-heavy checks typically land in Go, simple declarative ones
in the schema. The Go snippets below show field types and document the intended
constraints in comments; they are illustrative, not the final marker set.

### API Redesign (Breaking Changes)

These are breaking changes to `v1alpha1` fields whose current design would
create permanent API debt if left unchanged.

#### Routing Domain / VRF Semantics on L2VNI

**Problem:** [#332](https://github.com/openperouter/openperouter/pull/332) and
[#346](https://github.com/openperouter/openperouter/pull/346) already stopped the
runtime from forcing a VRF on disconnected L2VNIs, so a VNI without `spec.vrf`
now behaves as a pure east-west L2 overlay. The remaining problem is the API
shape: intent (attached-to-VRF vs pure-L2-overlay) is encoded by the *absence*
of the free-form `VRF *string`, `VRFName()` still falls back to `metadata.name`,
and the L2VNI-to-L3VNI association relies on fragile string matching rather than
an explicit, validated reference. The replacement reference must work both for
Kubernetes CRs and for the file-based (static) configuration, and it must be
able to grow beyond L3VNI to other routing-domain provider kinds (SRv6 L3VPN is
already in flight, see
[#316](https://github.com/openperouter/openperouter/pull/316)).

**Proposal:** Replace `VRF *string` + `L2GatewayIPs` with a `routingDomain`
discriminated union. A `type` discriminator selects the kind of resource that
provides the routing domain (today only `L3VNI`) and the matching sub-struct
carries the reference, e.g. `l3vni.name` points at the L3VNI CR by
`metadata.name`. Omitting `routingDomain` explicitly means "disconnected
overlay". `L2GatewayIPs` is renamed to `GatewayIPs` and stays on `L2VNISpec`
(it is a property of the L2 segment's IRB, not of the routing-domain
reference), guarded by a rule that it may only be set when `routingDomain` is
present. If the referenced resource does not exist, the controller rejects the
L2VNI (`Ready=False, Reason=L3VNINotFound`).

```go
// Validation intent: gatewayIPs may only be set when routingDomain is set.
type L2VNISpec struct {
    // RoutingDomain optionally attaches this L2VNI to a routing domain
    // provided by a backing resource (today an L3VNI). When omitted, the
    // L2VNI is a disconnected overlay (east-west L2 only, no VRF, no
    // gateway).
    // +optional
    RoutingDomain *RoutingDomain `json:"routingDomain,omitempty"`

    // GatewayIPs is a list of IP addresses in CIDR notation for the
    // distributed anycast gateway on this L2 segment's bridge (IRB
    // interface). It is a property of the L2 segment itself, so it lives on
    // the L2VNI rather than inside the routing-domain reference.
    // Validation intent: only valid when routingDomain is set; each entry a
    // valid CIDR; at most 2 entries with at most one of each IP family;
    // immutable once set.
    // +optional
    // +listType=atomic
    GatewayIPs []string `json:"gatewayIPs,omitempty"`

    // ... other fields unchanged
}

// RoutingDomain is a discriminated union over the resource kinds that can
// provide a routing domain. Exactly one sub-struct must match the type
// discriminator.
//
// +union
type RoutingDomain struct {
    // type selects the kind of resource that provides this routing domain.
    // Only L3VNI is defined today; the union is designed to be extended
    // with future provider kinds (e.g. SRv6L3VPN) that may carry their own
    // divergent fields.
    // +kubebuilder:validation:Enum=L3VNI
    // +required
    // +unionDiscriminator
    Type string `json:"type,omitempty"`

    // l3vni references the L3VNI (metadata.name) in the same namespace that
    // provides the routing domain for this L2VNI. The VRF configuration
    // (name, VNI, route targets) is owned entirely by the referenced L3VNI
    // -- the L2VNI does not duplicate it. Set only when type is L3VNI.
    // +optional
    L3VNI *L3VNIReference `json:"l3vni,omitempty"`
}

// L3VNIReference references an L3VNI by name. It is a struct rather than a
// bare string so future provider kinds can add their own reference fields
// without a breaking reshape.
type L3VNIReference struct {
    // Name is the metadata.name of the L3VNI in the same namespace.
    // +required
    Name string `json:"name,omitempty"`
}
```

`GatewayIPs` is a property of the L2 segment (the IRB anycast gateway), so it
lives on `L2VNISpec` rather than inside the routing-domain reference. The fact
that it is only meaningful once a routing domain is attached is a validation
constraint, not an ownership relationship. Because the field sits at the top of
the spec (not nested inside `routingDomain`), its immutability can be enforced
independently of whether the routing domain is attached or detached, with no
special parent-level rule needed.

`RoutingDomain` is a discriminated union (`+union`): exactly one provider
sub-struct is set, selected by `type`. The validation that the discriminator and
the populated sub-struct agree is part of the union's definition.

**Examples:**

```yaml
# L2VNI attached to a routing domain provided by an L3VNI
spec:
  vni: 100
  routingDomain:
    type: L3VNI
    l3vni:
      name: tenant-a       # references L3VNI by metadata.name
  gatewayIPs:              # property of the L2 segment, sibling of routingDomain
    - 10.100.0.1/24
    - fd10:100::1/64
```

Adding a future provider kind is additive and not a breaking change: append the
new kind to the `type` enum (e.g. `L3VNI;SRv6L3VPN`) and add the matching
sub-struct pointer (e.g. `srv6L3VPN *SRv6L3VPNReference`), with the
discriminator/sub-struct coupling validated as part of the union.

```yaml
# Future SRv6 L3VPN provider (illustrative)
spec:
  vni: 100
  routingDomain:
    type: SRv6L3VPN
    srv6L3VPN:
      name: tenant-a
  gatewayIPs:
    - 10.100.0.1/24
```

**File-based (static) configuration:**

The same `spec` schema is consumed both from Kubernetes CRs and from the
file-based (static) configuration (see
[running-on-the-host.md](running-on-the-host.md)). Static config entries carry
no `metadata`, so each entry has a **required `name`** field that becomes
`metadata.name`. The previous index-based synthetic name generation
(`static-l3vni-0`, `static-l2vni-0`, etc.) is removed. `name` is a
static-config-only field: it lives on the static wrapper alongside the embedded
`*Spec` and never appears in the CRD schema -- CRs are unaffected.

Two distinct `name`s are involved and should not be confused:

- `l3vnis[].name` -- the required static-config field that becomes the L3VNI's
  `metadata.name`.
- `l2vnis[].routingDomain.l3vni.name` -- the union reference (part of the real
  API) that points at an L3VNI's `metadata.name`.

```yaml
# static configuration file (e.g. openpe_node.yaml)
l3vnis:
  - name: tenant-a          # required -- becomes metadata.name
    vni: 100
    vrf: tenant-a
l2vnis:
  - name: tenant-a-l2       # required -- becomes metadata.name
    vni: 200
    routingDomain:
      type: L3VNI
      l3vni:
        name: tenant-a      # references the L3VNI above by metadata.name
    gatewayIPs:
      - 10.100.0.1/24
```

Validation for static configuration:

- `name` is required and must be a valid DNS-1123 subdomain (it becomes
  `metadata.name`).
- Names must be unique across the merged set of `openpe_*.yaml` files; the
  loader rejects duplicate names after merging.
- `routingDomain.l3vni.name` is resolved against the merged, post-naming set of
  L3VNIs (CR-sourced and static-sourced alike). A dangling reference is rejected
  (`Ready=False, Reason=L3VNINotFound`).

#### API Group Name

**Problem:** The current API group `openpe.openperouter.github.io` uses a
`github.io` domain, which looks provisional. Changing it later is a massive
breaking change.

**Proposal:** Rename to `network.openperouter.io` via find-and-replace
(`groupversion_info.go`, webhooks, RBAC, Helm, docs). No conversion webhook
needed in `v1alpha1`; release notes must document the rename.

#### Replace `nics []string` with `interfaces`

**Problem:** Underlay connectivity nics `UnderlaySpec.Nics []string` field is 
very concrete and related to moving exising nics into the network namespace, 
we should support extending the api in the future for other mechanism like
ipvlan or macvtap.

**Proposal:** Replace both with `interfaces`, a discriminated-union slice on
`UnderlaySpec`. Currently one mode (`networkDevice`) is defined; macvlan/ipvlan modes
will be added in a follow-up API enhancement.

```go
type UnderlaySpec struct {
    // ...

    // interfaces is the list of interfaces the router uses for underlay
    // connectivity. Each entry is a discriminated union describing how the
    // interface is obtained.
    // +kubebuilder:validation:MinItems=1
    // +required
    Interfaces []UnderlayInterface `json:"interfaces,omitempty"`
}

// UnderlayInterface defines how the router obtains a single underlay link.
// Exactly one of the sub-structs must match the type field (validation intent;
// enforced as part of the union definition).
// The union is designed to be extended with future modes (e.g. macvlan,
// ipvlan) for controller-provisioned interfaces.
//
// +union
type UnderlayInterface struct {
    // type selects how the router obtains this underlay link.
    // +kubebuilder:validation:Enum=NetworkDevice
    // +required
    // +unionDiscriminator
    Type string `json:"type,omitempty"`

    // networkDevice moves an existing host network device into the router netns.
    // The device can be of any kind (physical NIC, bridge, macvlan, etc.).
    // +optional
    NetworkDevice *NetworkDevice `json:"networkDevice,omitempty"`
}

type NetworkDevice struct {
    // interfaceName is the name of the host network device to move into
    // the router netns.
    // +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9._-]*$`
    // +kubebuilder:validation:MaxLength=15
    // +required
    InterfaceName string `json:"interfaceName,omitempty"`
}
```

**Example:**

```yaml
interfaces:
  - type: NetworkDevice
    networkDevice:
      interfaceName: eth1
```

#### Remove Non-Inclusive Terminology

**Problem:** Several API types and comments use non-inclusive language:

- `RunOnMaster` / `runOnMaster` on the operator CRD
  (`OpenPERouterSpec`) uses "master" instead of "control-plane" and is
  a boolean, which violates K8s API conventions. Replace with native
  K8s scheduling primitives (nodeSelector, tolerations, affinity)
  following the [MetalLB operator](https://github.com/metallb/metallb-operator)
  pattern.
- Godoc comments on `L2VNISpec.HostMaster` and `L2GatewayIPs` use
  "enslaved to" instead of "attached to".

**Proposal:**

Rename types and fields:

```go
// Operator CRD -- before
RunOnMaster *bool `json:"runOnMaster,omitempty"`

// After — replace boolean with native K8s scheduling primitives,
// following the MetalLB operator pattern. A single set of fields
// applies to all components (router, controller, nodemarker,
// hostbridge). Per-component scheduling can be added later without
// breaking the API.

// nodeSelector constrains all openperouter pods to nodes with
// matching labels.
// +optional
NodeSelector map[string]string `json:"nodeSelector,omitempty"`

// tolerations for all openperouter pods.
// +optional
Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

// affinity scheduling rules for all openperouter pods.
// +optional
Affinity *corev1.Affinity `json:"affinity,omitempty"`
```

Replace "enslaved" with "attached" in all API godoc comments (e.g.
"the veth should be attached to" instead of "the veth should be
enslaved to").

#### Align Enum Values

**Problem:** Several enum values use lowercase or kebab-case, align them to 
PascalCase.

**Proposal:** Rename all enum values to PascalCase:

| Field | Current | Proposed |
|-------|---------|----------|
| `Neighbor.Type` | `external`, `internal` | `External`, `Internal` |
| `HostSession.HostType` | `external`, `internal` | `External`, `Internal` |
| `HostMaster.Type` | `linux-bridge`, `ovs-bridge` | `LinuxBridge`, `OVSBridge` |
| `UnderlayInterface.Type` | _(new)_ | `NetworkDevice` |

The `NeighborPropertyType` (`ebgpMultiHop`, `routeReflectorClient`) and
`AddressFamilyType` (`ipv4unicast`, `evpn`, …) enums are an intentional
exception: their values are protocol/FRR-config tokens, kept verbatim so they
map directly to the rendered stanzas (`ebgp-multihop`, `route-reflector-client`,
`address-family ipv4 unicast`). These two enums are owned by the
[route reflector enhancement](route-reflector.md) and
[PR #316](https://github.com/openperouter/openperouter/pull/316); this
enhancement does not re-case them.

#### Replace Boolean Fields

**Problem:** Several boolean fields violate the
[Kubernetes convention against booleans](https://github.com/kubernetes/community/blob/main/contributors/devel/sig-architecture/api-conventions.md#primitive-types).
Booleans cannot be extended to a third state and their `true`/`false` values
do not convey intent.

**Proposal:**

Replace `EBGPMultiHop bool` on `Neighbor` with a session-level
`properties` list. Rather than a bare enum set, properties use a
discriminated **struct** list (`[]NeighborProperty`, keyed by `type`) so that
valued properties can carry typed parameters — `ebgpMultiHop` needs a `ttl`,
which a plain string-enum cannot express, and Kubernetes structural schemas
forbid mixing enum and struct items in one list. This also keeps the neighbor
configuration close to the FRR/Cisco model (toggles and their parameters live
on the neighbor), which is easier for users.

The `NeighborProperty` type is **owned by the
[route reflector enhancement](route-reflector.md)**, which defines the shared
`NeighborPropertyType` enum, the session/per-address-family placement guards,
and ships the `routeReflectorClient` property (per address family only). The
same `[]NeighborProperty` list appears at two tiers, mirroring FRR's config
structure: `Neighbor.properties` for session-level stanzas (rendered under
`router bgp`, e.g. `neighbor X ebgp-multihop <ttl>`) and
`NeighborAddressFamily.properties` for per-AF stanzas (rendered inside an
`address-family` block, e.g. `neighbor X route-reflector-client`). PR #341 only
folds `ebgpMultiHop` (session-level) into that shared list; it does not redefine
the broader structure. The relevant shape:

```go
// NeighborPropertyType defines an optional feature on a Neighbor.
// Owned by the route reflector enhancement; ebgpMultiHop folded in here.
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

// NeighborProperty is an optional feature applied to a neighbor session. The
// type field selects the property; typed sub-fields hold parameters for
// properties that require them.
// Validation intent: a property's parameter sub-field may only be set when the
// matching type is selected (e.g. ebgpMultiHop parameters require type
// ebgpMultiHop).
type NeighborProperty struct {
    // type selects the property.
    // +required
    Type NeighborPropertyType `json:"type"`

    // ebgpMultiHop holds parameters for the ebgpMultiHop property.
    // May only be set when type is ebgpMultiHop.
    // +optional
    EBGPMultiHop *EBGPMultiHopProperties `json:"ebgpMultiHop,omitempty"`
}

type Neighbor struct {
    // ...

    // properties is the set of optional session-level features for this
    // neighbor (e.g. ebgpMultiHop).
    // +optional
    // +listType=map
    // +listMapKey=type
    Properties []NeighborProperty `json:"properties,omitempty"`
}
```

In YAML, session-level features go under `neighbors[].properties`, per-AF
features (such as `routeReflectorClient`) go under
`neighbors[].addressFamilies[].properties` (the `addressFamilies` list itself is
owned by [PR #316](https://github.com/openperouter/openperouter/pull/316)), and
BFD attributes stay under `neighbors[].bfd` as a flat struct (BFD knobs are
mostly valued scalars, not toggles, so they are not modelled as a properties
list):

```yaml
neighbors:
  - asn: 64512
    address: 10.0.0.5
    properties:                        # BGP session-level features
      - type: ebgpMultiHop
        ebgpMultiHop:
          ttl: 5
    bfd:                               # BFD attributes (per-neighbor profile)
      receiveInterval: 300
      transmitInterval: 300
      sessionMode: Passive             # Active (default) | Passive
    addressFamilies:
      - type: evpn
        properties:                    # per-address-family features
          - type: routeReflectorClient
```

On `BFDSettings`, remove `EchoMode` (and its companion `EchoInterval`) and
replace `PassiveMode *bool` with a `sessionMode` enum. Passive stays a BFD
attribute (it is BFD `passive-mode`, which FRR accepts per-peer; the
hostcontroller already emits one BFD profile per neighbor, so all BFD knobs are
per-neighbor) and is not a BGP neighbor property — it is distinct from BGP
`neighbor X passive`. Echo mode relies on the peer looping echo packets through
its forwarding plane and only works between FRR instances; against a physical
fabric / ToR it is inert, and it is restricted to single-hop sessions anyway —
so it is dropped rather than carried forward. `EchoInterval` only has meaning
with echo enabled and is removed with it. Passive is the only remaining BFD
mode, so it is modelled as a small enum (active is the default) rather than an
over-engineered single-element set:

```go
// BFDSessionMode selects whether the local system initiates the BFD session.
// +kubebuilder:validation:Enum=Active;Passive
type BFDSessionMode string

type BFDSettings struct {
    // ...

    // sessionMode marks the session active or passive. Active (the default
    // when omitted) initiates the session. Passive waits for the peer to
    // initiate before replying (RFC 5880 Section 6.1).
    // +optional
    SessionMode *BFDSessionMode `json:"sessionMode,omitempty"`
}
```

Replace `AutoCreate bool` with a `lifecycle` enum (`Managed`/`External`):


```go
// BridgeLifecycle determines how the bridge is provisioned.
// +kubebuilder:validation:Enum=Managed;External
type BridgeLifecycle string

const (
    // BridgeLifecycleManaged means the controller creates and owns
    // the bridge, and deletes it when the L2VNI is removed. If Name is
    // omitted, the bridge is auto-named br-hs-<VNI>. If Name is
    // provided, the controller creates the bridge with that name.
    BridgeLifecycleManaged BridgeLifecycle = "Managed"

    // BridgeLifecycleExternal means the user provides a
    // pre-existing bridge via the Name field. The controller does not
    // create or delete it; only veth ports are attached/detached.
    BridgeLifecycleExternal BridgeLifecycle = "External"
)

// LinuxBridgeConfig contains configuration for Linux bridge type.
// Validation intent: name is required when lifecycle is External.
type LinuxBridgeConfig struct {
    // lifecycle determines if the bridge is managed by the
    // controller or provided by the user.
    // +required
    Lifecycle BridgeLifecycle `json:"lifecycle,omitempty"`

    // name of the Linux bridge interface.
    // Optional when Managed (defaults to br-hs-<VNI>).
    // Required when External.
    // +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9_-]*$`
    // +kubebuilder:validation:MaxLength=15
    // +optional
    Name *string `json:"name,omitempty"`
}

// OVSBridgeConfig contains configuration for OVS bridge type.
// Validation intent: name is required when lifecycle is External.
type OVSBridgeConfig struct {
    // lifecycle determines if the OVS bridge is managed by the
    // controller or provided by the user.
    // +required
    Lifecycle BridgeLifecycle `json:"lifecycle,omitempty"`

    // name of the OVS bridge interface.
    // Optional when Managed (defaults to br-hs-<VNI>).
    // Required when External.
    // +kubebuilder:validation:Pattern=`^[a-zA-Z][a-zA-Z0-9_-]*$`
    // +kubebuilder:validation:MaxLength=15
    // +optional
    Name *string `json:"name,omitempty"`
}
```

#### Consolidate Split IP Family Fields

**Problem:** `LocalCIDRConfig` uses separate `IPv4 *string` / `IPv6 *string`
fields. The current implementation already supports dual-stack, but using two
distinct fields is unnecessarily rigid — it requires a struct with a cross-field
rule to enforce "at least one set", and cannot be extended to multiple CIDRs of
the same family without adding more fields. Both Kubernetes
(e.g. `ClusterIPs`, `PodCIDRs`) and OpenShift use ordered `[]string` slices
for this pattern.

**Affected fields:**

**`LocalCIDRConfig` -- breaking change.** Replace the `LocalCIDRConfig`
struct with an ordered `localCIDRs` slice directly on `HostSession`.
This collapses two fields into one and follows the K8s convention:

```go
// After
type HostSession struct {
    // ...

    // localCIDRs is the list of CIDRs for the veth pair connecting to the
    // default namespace. The router side uses the first IP of each CIDR.
    // The first element is the primary address family.
    // Validation intent: at least one CIDR; each entry a valid CIDR; at most
    // two entries with at most one of each IP family; immutable once set.
    // +listType=atomic
    // +required
    LocalCIDRs []string `json:"localCIDRs,omitempty"`
}
```

**`VTEPCIDR` -- covered by `TunnelEndpoint` redesign above.** After
[PR #461](https://github.com/openperouter/openperouter/pull/461) removed
`VTEPInterface`, `VTEPCIDR` is the only field in `EVPNConfig` (optional
`*string`). The field becomes `TunnelEndpointConfig.CIDRs []string`,
restricted to a single IPv4 CIDR.

**`RouterIDCIDR` -- not split.** Router IDs are IPv4-only by
definition across routing protocols (BGP per RFC 4271, OSPF, IS-IS), so there
is no IP-family split to make here; the field keeps its `*string` IPv4 CIDR
shape.

**`L2VNISpec.GatewayIPs` -- covered by the Routing Domain / VRF Semantics
on L2VNI redesign above.** Its validation intent (each entry a valid CIDR; at
most two entries with at most one of each IP family) is described there.

#### BGP Password Handling on Neighbor

**Problem:** `Neighbor` has two fields for BGP session authentication:

- `Password *string` — stores the password as **plaintext in the CR**,
  which means it is stored unencrypted in etcd and visible to anyone
  with read access to the resource. This is the only field actually
  wired into the FRR configuration.
- `PasswordSecret *string` — intended to reference a
  `kubernetes.io/basic-auth` Secret by name, but **never implemented**.
  No controller code reads it, no Secret lookup exists, no RBAC grants
  access to Secrets, and no tests cover it. Setting this field has no
  effect.

**Proposal:** Remove both `Password` and the unimplemented
`PasswordSecret`. Replace with a `passwordSecret` field using a
`SecretKeyRef` struct that references a Secret by name and key. The
controller must be updated to fetch the Secret and inject the password
into the FRR configuration:

```go
type Neighbor struct {
    // ...

    // passwordSecret references a key in a Kubernetes Secret
    // containing the BGP session password.
    // +optional
    PasswordSecret *SecretKeyRef `json:"passwordSecret,omitempty"`
}

type SecretKeyRef struct {
    // name is the name of the Secret in the same namespace.
    // +required
    // +kubebuilder:validation:MinLength=1
    Name string `json:"name,omitempty"`

    // key is the key within the Secret's data to select.
    // The controller defaults this to "password" when unset.
    // +kubebuilder:validation:MinLength=1
    // +optional
    Key *string `json:"key,omitempty"`
}
```


### Important Fixes

These are non-breaking changes that should be addressed.

#### Normalize JSON Tag Casing

**Problem:** Several JSON tags on existing API types are flat-lowercase
instead of `lowerCamelCase`, which violates the
[Kubernetes API conventions](https://github.com/kubernetes/community/blob/master/contributors/devel/sig-architecture/api-conventions.md#naming-conventions)
and is inconsistent with neighbouring fields on the same types
(e.g. `nodeSelector`, `linuxBridge`, `holdTimeSeconds`). The
kube-api-linter on `main` does not flag these because some are silenced
with `//nolint:kubeapilinter` and others slip past its acronym rules.

**Proposal:** Rename the offending JSON tags. Go field names stay the
same; only the serialized name changes. Items already covered by other
redesigns in this enhancement (e.g.
`L2GatewayIPs` → `GatewayIPs`, `LocalCIDR` → `LocalCIDRs`
) inherit the corrected tag and are not
listed again here.

| Type / Field | Current tag | Proposed tag |
|--------------|-------------|--------------|
| `L2VNISpec.HostMaster` | `hostmaster` | `hostMaster` |
| `HostSession.HostASN` | `hostasn` | `hostASN` |
| `HostSession.HostType` | `hosttype` | `hostType` |
| `L2VNISpec.VXLanPort` | `vxlanport` | `vxlanPort` |
| `L3VNISpec.VXLanPort` | `vxlanport` | `vxlanPort` |
| `L3PassthroughSpec.HostSession` | `hostsession` | `hostSession` |

This is a breaking serialization change but is grouped with Important
Fixes because the Go API surface (field names, types) is unchanged.

#### Print Columns

**Problem:** `kubectl get` only shows NAME and AGE for all CRDs.

**Proposal:** Add `+kubebuilder:printcolumn` markers:

| CRD | Columns |
|-----|---------|
| Underlay | ASN, Router ID Pool, VTEP CIDR, Age |
| L2VNI | VNI, L3VNI, Gateway IPs, Age |
| L3VNI | VNI, VRF, Age |
| L3Passthrough | ASN, HostASN, Age |

### Quality and Correctness

#### Format Validation for IP / CIDR / Route Target Fields

**Problem:** IP, CIDR, and Route Target fields lack a documented validation
intent; today the checks exist only in webhooks. Simple format checks should
ideally also be discoverable via `kubectl explain` / OpenAPI.

**Proposal:** Define the validation intent for each field below. Whether each
check is implemented in the schema, a Go webhook, or both is decided during
implementation (simple format checks lend themselves to the schema; the
conditional Route Target rules stay in Go — see below).

| Field | Type | Current webhook | Validation intent |
|-------|------|-----------------|-------------------|
| `UnderlaySpec.RouterIDCIDR` | IPv4 CIDR | `validate_underlay.go` | valid IPv4 CIDR |
| `TunnelEndpointConfig.CIDRs` | IPv4 CIDR | `validate_underlay.go` | valid IPv4 CIDR (covered by `TunnelEndpoint` redesign) |
| `Neighbor.Address` | IP address | Not validated | valid IPv4 or IPv6 |
| `L2VNISpec.GatewayIPs` | IP/CIDR list | `validate_vni.go` | valid CIDR, max 1 IPv4 + 1 IPv6 |
| `HostSession.LocalCIDRs` | IP/CIDR list | `validate_hostsession.go` | valid CIDR, max 1 IPv4 + 1 IPv6 |
| `L3VNISpec.ExportRTs` / `ImportRTs` | Route Target | `validate_vni.go` | `RouteTarget` named type (see below) |

Introduce a `RouteTarget` named type to replace the plain `[]string` on
`L3VNISpec.ExportRTs` and `ImportRTs`. The SRv6 enhancement
([PR #316](https://github.com/openperouter/openperouter/pull/316)) reuses the
same type for `L3VPN`.

```go
// RouteTarget defines a BGP Extended Community for route filtering.
// Supports two formats per RFC 4360:
//   - ASN:NN   (e.g. "64514:100")        where ASN is a 2- or 4-byte ASN (<= 4294967295)
//   - IP:NN    (e.g. "192.168.1.1:100")  where IP is a valid IPv4 address
type RouteTarget string
```

Cross-resource validations (duplicate VNI, subnet overlaps, singleton-per-node)
must remain in webhooks, even if it's not perfect is better than not having it
also controllers will check it and report it to future node statuses.


#### Immutability Rules

**Problem:** Several fields that are dangerous to change at runtime are not
enforced as immutable. `L2GatewayIPs` and `LocalCIDR` already are, but the
following are not.

Immutability should only be enforced where a change would leave
**orphaned or un-cleanable state**, or break an **identity / cross-resource
reference** — cases the controller cannot reconcile away. A change merely being
disruptive to the data plane is not, on its own, a reason for immutability:
fields the controller reconciles in place (rebuilding the affected interfaces or
re-establishing sessions) should remain mutable to avoid forcing users to delete
and recreate resources.

**Proposal:** Enforce immutability on the following fields (validation intent;
mechanism decided during implementation):

- `L3VNISpec.VRF` — renaming orphans L2VNI references that bind to the L3VNI by
  VRF name.
- `HostMaster.Type` — switching type orphans the previous type's bridge
  artifacts.
- `LinuxBridgeConfig.Name` and `OVSBridgeConfig.Name` — changing the name
  orphans the old user-provided bridge.

The following fields are **intentionally mutable** — changing them does not
orphan state or break references, so the controller reconciles them in place
(rebuilding the affected interfaces where needed):

- `L2VNISpec.VNI` and `L3VNISpec.VNI` — every host interface is named by VNI,
  so the controller tears down the old VNI's interfaces and builds the new ones;
  no stale state is left behind.
- `L2VNISpec.VXLanPort` and `L3VNISpec.VXLanPort` — the VXLAN interface keeps its
  (VNI-derived) name, so it is recreated in place with the new port.
- `TunnelEndpointConfig.CIDRs` — the controller reconciles the loopback address
  (removing the old, adding the new); disruptive but no orphaned state.
- `TunnelEndpointConfig.Interface` — an underlay interface change is reconciled
  by rebuilding the router namespace, not by leaving the old interface attached.
- `UnderlaySpec.ASN` — reconciled in place; no orphaned state.
- `UnderlaySpec.RouterIDCIDR` — same as ASN; reconciled in place.
- `Neighbors` (address, ASN, timers, etc.) — the controller reconciles
  neighbor changes without requiring resource deletion.

`NodeSelector` on every CRD remains mutable. Operators need to be able
to expand or shrink the set of nodes the configuration applies to
(e.g. rolling out to additional nodes, draining a node) without having
to delete and recreate the resource.

#### VNI Range Validation

**Problem:** `L2VNISpec.VNI` and `L3VNISpec.VNI` have `Minimum=0,
Maximum=4294967295`. VNI 0 is reserved and VXLAN VNIs are 24-bit (RFC 7348).

**Proposal:** Change to `Minimum=1, Maximum=16777215`.

#### VXLanPort Range Validation

**Problem:** `VXLanPort` on both `L2VNISpec` and `L3VNISpec` has no range
validation. Valid UDP ports are 1-65535.

**Proposal:** Add `Minimum=1, Maximum=65535`.

#### Neighbor.Port Maximum

**Problem:** `Neighbor.Port` has `Maximum=16384` and `Minimum=0`. The TCP
port ceiling is 65535, and port 0 is invalid.

**Proposal:** Change to `Minimum=1, Maximum=65535`.

#### Neighbors MaxItems

**Problem:** `UnderlaySpec.Neighbors` has no `MaxItems` bound. Unbounded
lists are a Kubernetes API anti-pattern — they cause etcd bloat and slow
admission validation.

**Proposal:** Add `+kubebuilder:validation:MaxItems=100`. This aligns with
the SRv6 enhancement ([PR #316](https://github.com/openperouter/openperouter/pull/316))
which adds the same bound.

#### BGP Timer Validation

**Problem:** `HoldTimeSeconds` and `KeepaliveTimeSeconds` on `Neighbor`
have no range or relational validation (unlike `ConnectTimeSeconds` which
does). Per RFC 4271, hold time must be 0 or >= 3s, and keepalive must be
less than hold time. Additionally, setting one without the other creates
undefined FRR behavior — they must be both set or both unset.

**Proposal:** Enforce these constraints on `Neighbor` (validation intent;
mechanism decided during implementation):

- `holdTimeSeconds` and `keepaliveTimeSeconds` must be both set or both unset.
- `holdTimeSeconds` must be 0 or at least 3 (RFC 4271).
- `keepaliveTimeSeconds` must be less than `holdTimeSeconds`.

#### HostMaster Union Markers

**Problem:** `HostMaster` is a discriminated union but lacks `+union` and
`+unionDiscriminator` markers. These markers are the canonical way to express
the pattern and enable tooling (`kubectl explain`, OpenAPI discriminator).

**Proposal:** Mark `HostMaster` as a discriminated union (`+union` on the
struct, `+unionDiscriminator` on `Type`), consistent with the other unions in
this document.

## References

- [PR #313](https://github.com/openperouter/openperouter/pull/313) -- kube-api-linter fixes (out of scope)
- [PR #316](https://github.com/openperouter/openperouter/pull/316) -- SRv6 enhancement (introduces `TunnelEndpoint` concept)
- [PR #329](https://github.com/openperouter/openperouter/pull/329) -- Controller-provisioned underlay interfaces enhancement
- [PR #461](https://github.com/openperouter/openperouter/pull/461) -- Remove VTEPInterface from EVPNConfig ([#458](https://github.com/openperouter/openperouter/issues/458))
- [router-resiliency.md](router-resiliency.md) -- Persistent named netns (motivates underlay interface redesign)
- [status-crd.md](status-crd.md) -- Status reporting (separate enhancement)
