package manifest

import (
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// An Environment is a statement of intent — "these machines should exist" —
// and not a stored configuration object. What it describes is provider
// objects to be provisioned, whose record of existence is the embedder's
// (Sybil Continuum keeps it in orbit.provider_realizations), not a copy of
// the manifest.
//
// What this file provides is the envelope half only: decode, validate,
// normalize, encode, and say what the manifest points at. Resolving those
// pointers, expanding count, and planning against what was built belong to
// package plan.
const (
	// KindEnvironment is the envelope's `kind`.
	KindEnvironment = "Environment"

	// EnvironmentV1Alpha1 is the only apiVersion this codec accepts. It
	// keeps the name it was born with inside Sybil, so every manifest
	// written so far still means what it meant.
	// Unversioned Environment documents are rejected: the shape is expected
	// to move, and a manifest that does not say which shape it was written
	// for cannot be migrated safely later.
	EnvironmentV1Alpha1 = "sybil/v1alpha1"

	// MaxResourceCount bounds a single resource's count. It is a typo and
	// overflow guard, not a quota: without it `count: 9223372036854775807`
	// parses happily and two such resources make Total() overflow to a
	// negative, so a manifest asking for an impossible number of machines
	// would report asking for fewer than none.
	//
	// How many machines an operator may *actually* have is the provider's
	// question, answered at provision time by the licence ceiling that
	// produces RefusedCeiling — not something a YAML parser can know. So this
	// is deliberately far above any plausible real environment.
	MaxResourceCount = 1000
)

// Environment is a validated, normalized manifest. Anything DecodeEnvironment
// or DecodeStandalone returns satisfies Validate — Name is non-empty, exactly
// one of Spec.Integration and Spec.Provider is set, every resource has a name
// unique within the environment and a blueprint, and every Count is between 0
// and MaxResourceCount — so a consumer of a decoded value does not
// re-validate.
//
// An Environment may declare nothing at all, and a resource may have count 0.
// Both mean "none of these should exist": the plan lists every machine built
// for what is no longer declared as delete, which is how an Environment is
// emptied and then deleted (see Emptied).
//
// The struct is exported and has no unexported fields, so nothing stops a
// caller building one directly; that is deliberate, because the planner will.
// Validate is what such a caller owes, and EncodeEnvironment calls it rather
// than trusting the claim.
type Environment struct {
	Name string
	Spec EnvironmentSpec
}

// EnvironmentSpec is the body of the manifest.
type EnvironmentSpec struct {
	// Integration names the connector the machines are provisioned through,
	// in a sybil/v1alpha1 manifest.
	Integration string
	// Provider is what an accretion/v1alpha1 manifest builds through, in
	// Integration's place. Exactly one of the two is set.
	Provider *ProviderRef
	// Group optionally names an asset group the results should join. It is
	// optional because an environment is useful before anyone has decided how
	// to organize what it produces.
	Group string
	// Networks are the networks the environment's machines share, declared
	// beside them so "these machines can reach each other" is part of the
	// intent and not a step somebody did by hand first. Optional: a resource
	// can name a bridge that already exists instead.
	Networks []EnvironmentNetwork
	// Variables are values named once and used as {{var.name}} in resources'
	// inputs and images. Scalars only: a string, a number or a boolean.
	Variables map[string]any
	// ControlPlane is the address pair the Environment's machines are told to
	// reach Sybil at, nil for the server's own default. A network may say
	// otherwise for the machines whose first card joins it. Sybil only, like
	// Images: an accretion/v1alpha1 manifest may not set it, and nothing in
	// this module resolves it.
	ControlPlane *ControlPlane
	// Images is how Sybil treats an image whose content it cannot know: ""
	// (approved-only, the default) refuses it, ImagesKnownOrWarn plans it
	// with a warning. A known image that is not approved is refused either
	// way. Sybil only: the planner reads it when its caller asks for approved
	// images, which accrete on its own never does, and an accretion/v1alpha1
	// manifest may not set it.
	Images string
	// Resources is the set of machines to bring into being, in the order the
	// manifest declared them. Order is preserved rather than sorted: it is the
	// operator's reading order, and re-emitting it differently would make a
	// round-trip show up as a diff.
	Resources []EnvironmentResource
}

// EnvironmentResource is one declared group of identical machines.
type EnvironmentResource struct {
	// Name is the intent identity within this environment. Renaming a resource
	// is declaring a different one, not renaming the machine it produced.
	Name string
	// Count is how many to make, always at least 1 in a validated Environment.
	Count int
	// Blueprint names the provisioning blueprint to run.
	Blueprint string
	// Network optionally names the network the machine's first NIC joins:
	// one of spec.networks, or a bridge that already exists on the provider.
	// It is shorthand the planner expands into the blueprint's own NIC input,
	// so it cannot be combined with that input.
	Network string
	// Power is whether the machines should be running: "on" (the default) or
	// "off". A machine that is not as declared plans as a change an apply
	// makes, under the identity it was built with.
	Power string
	// IP is how the machine's first NIC is addressed: "" or "dhcp" for DHCP,
	// "static" for an address the planner allocates from its network's
	// static range and keeps for the life of the machine. Static needs the
	// resource's network to be one of spec.networks, with dns servers.
	IP string
	// ExtraNICs are the machine's second and later network cards, in order:
	// net1, net2, net3 on Proxmox. Written in a manifest as `nics:`, whose
	// first entry is Network and IP above, so a machine with one card reads
	// exactly as it did before there could be more. The first card alone
	// carries the default route and the name servers; the others only an
	// address on their own network.
	ExtraNICs []EnvironmentNIC
	// Image names what the machine is built from — a template, on Proxmox —
	// by its name, optionally pinned to the sha256 of the image Sybil built it
	// from ("rocky-9@sha256:…"), instead of a provider id such as a VMID. The
	// planner resolves it into the blueprint input that takes an image.
	Image string
	// Firewall is the hypervisor firewall each machine should have, nil to
	// leave it as it is. See firewall.go.
	Firewall *EnvironmentFirewall
	// Placement says where the machines may run; nil leaves the node to the
	// blueprint's node input, as before placement existed.
	Placement *Placement
	// Inputs are the blueprint's parameters, carried as a generic map: the
	// shapes are the blueprint's to define, and enumerating them here would make this package need an edit
	// every time a blueprint gained a field.
	Inputs map[string]any
}

// EnvironmentNIC is one network card of a machine past the first.
type EnvironmentNIC struct {
	// Network is one of spec.networks, or a bridge that already exists.
	Network string
	// IP is "" or "dhcp", or "static" for an address from Network's static
	// pool, as for the first card.
	IP string
}

// MaxNICs is how many network cards a resource may declare. The machine
// blueprint names a device input for each (net0 to net3); a fifth would be a
// manifest asking for a card no blueprint can give it.
const MaxNICs = 4

// NICInputN is the blueprint input the resource's nth network card (0-based)
// expands into: net0, net1, …
func NICInputN(n int) string { return fmt.Sprintf("net%d", n) }

// NICs is every network card the resource declares, first one first, or nil
// when it names no network.
func (r EnvironmentResource) NICs() []EnvironmentNIC {
	if r.Network == "" {
		return nil
	}
	return append([]EnvironmentNIC{{Network: r.Network, IP: r.IP}}, r.ExtraNICs...)
}

// EnvironmentNetwork is one declared network: a routed subnet machines on it
// share, with the provider's node as their gateway.
type EnvironmentNetwork struct {
	// Name is the network's identity and, on Proxmox, the vnet — which is also
	// the bridge a NIC names. NetworkNamePattern says what it may be.
	Name string
	// Zone is the SDN zone it lives in, DefaultNetworkZone when not declared.
	Zone string
	// Subnet is the network's prefix, canonical (10.20.0.0/24, never
	// 10.20.0.7/24).
	Subnet string
	// Gateway is the address in Subnet the node answers on. Defaults to the
	// subnet's first address.
	Gateway string
	// SNAT gives the machines outbound access through the node. Defaults to
	// true: a network whose machines cannot fetch a package is rarely what
	// anybody meant.
	SNAT bool
	// DHCPRange is the START-END range the network's DHCP server hands out,
	// or empty for no DHCP.
	//
	// Off unless the manifest says `dhcp: true`. It was on by default, and on
	// flk-vega-01 that broke the cluster: a zone with dhcp=dnsmasq on a node
	// without the dnsmasq package makes EVERY SDN reload fail, not only this
	// network's — and Proxmox gives no way to check for the package first. So
	// it is the operator's statement that dnsmasq is installed. With it on and
	// no range given, the range is the upper half of an IPv4 subnet below the
	// gateway; a machine Sybil builds asks for its address by DHCP.
	DHCPRange string
	// StaticRange is the START-END range fixed addresses are allocated
	// from, for resources with `ip: static`. Defaults to the addresses
	// between the gateway and the DHCP range (or the end of the subnet).
	StaticRange string
	// DNS are the name servers a machine with a fixed address is given.
	// Required for one: with no DHCP to hand them out, a machine without
	// them cannot resolve a package mirror.
	DNS []string
	// ControlPlane is how the machines whose first card joins this network
	// reach Sybil, when that is not the Environment's: a LAN address, a VPN
	// or tailnet one. Nil defers to spec.control_plane. Sybil only.
	ControlPlane *ControlPlane
}

// ControlPlane is the pair of addresses a provisioned machine is told to reach
// Sybil at: the REST base it fetches its installer from, and the gRPC address
// its agent dials. Always both: they are one listener as one network sees it,
// and half of a route is no route.
//
// It is a statement of how the network reaches Sybil, not a request to build
// that path — no VPN, tailnet or router is configured from it.
type ControlPlane struct {
	// URL is http(s)://host[:port], with no path, query or credentials.
	URL string
	// GRPC is host:port.
	GRPC string
}

type controlPlaneWire struct {
	URL  string `yaml:"url"`
	GRPC string `yaml:"grpc"`
}

func decodeControlPlane(w *controlPlaneWire) (*ControlPlane, error) {
	if w == nil {
		return nil, nil
	}
	cp := &ControlPlane{URL: strings.TrimSpace(w.URL), GRPC: strings.TrimSpace(w.GRPC)}
	return cp, cp.validate()
}

func encodeControlPlane(cp *ControlPlane) *controlPlaneWire {
	if cp == nil {
		return nil
	}
	return &controlPlaneWire{URL: cp.URL, GRPC: cp.GRPC}
}

// validate checks the shape only. Whether the address is one the machine can
// use — not its own loopback, covered by the server's certificate, reachable
// from the network — is the embedder's to judge: it knows the server.
func (cp *ControlPlane) validate() error {
	if cp == nil {
		return nil
	}
	if cp.URL == "" || cp.GRPC == "" {
		return fmt.Errorf("control_plane needs both url and grpc; they are one listener as this network sees it")
	}
	u, err := url.Parse(cp.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" {
		return fmt.Errorf("control_plane.url %q must be http(s)://host[:port]", cp.URL)
	}
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return fmt.Errorf("control_plane.url %q must be only a scheme, a host and a port", cp.URL)
	}
	if err := checkPort(u.Port(), true); err != nil {
		return fmt.Errorf("control_plane.url %q: %w", cp.URL, err)
	}
	host, port, err := net.SplitHostPort(cp.GRPC)
	if err != nil || host == "" {
		return fmt.Errorf("control_plane.grpc %q must be host:port", cp.GRPC)
	}
	if err := checkPort(port, false); err != nil {
		return fmt.Errorf("control_plane.grpc %q: %w", cp.GRPC, err)
	}
	return nil
}

func checkPort(p string, optional bool) error {
	if p == "" && optional {
		return nil
	}
	n, err := strconv.Atoi(p)
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("port %q is not 1-65535", p)
	}
	return nil
}

// DefaultNetworkZone is the zone a network without one is created in.
const DefaultNetworkZone = "sybil"

// NetworkNamePattern is what a network or zone name may be: Proxmox uses both
// as interface names, which caps them at eight characters and rules out
// anything but lowercase letters and digits, starting with a letter.
var NetworkNamePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,7}$`)

type environmentNetworkWire struct {
	Name    string `yaml:"name"`
	Zone    string `yaml:"zone,omitempty"`
	Subnet  string `yaml:"subnet"`
	Gateway string `yaml:"gateway,omitempty"`
	SNAT    *bool  `yaml:"snat,omitempty"`
	// DHCP false turns DHCP off; DHCPRange overrides the default range.
	DHCP        *bool    `yaml:"dhcp,omitempty"`
	DHCPRange   string   `yaml:"dhcp_range,omitempty"`
	StaticRange string   `yaml:"static_range,omitempty"`
	DNS         []string `yaml:"dns,omitempty"`
	// ControlPlane is Sybil only; see EnvironmentNetwork.ControlPlane.
	ControlPlane *controlPlaneWire `yaml:"control_plane,omitempty"`
}

// environmentSpecWire is the on-disk shape, which is not quite the validated
// shape. Count is a pointer so that "absent" and "count: 0" are different
// things: absent means the default of one, while an explicit zero is a
// resource that would make nothing and is an error worth reporting rather
// than quietly promoting to one.
type environmentSpecWire struct {
	Integration string `yaml:"integration"`
	Group       string `yaml:"group,omitempty"`
	Images      string `yaml:"images,omitempty"`
	// ControlPlane is Sybil only; see EnvironmentSpec.ControlPlane.
	ControlPlane *controlPlaneWire         `yaml:"control_plane,omitempty"`
	Variables    map[string]any            `yaml:"variables,omitempty"`
	Networks     []environmentNetworkWire  `yaml:"networks,omitempty"`
	Resources    []environmentResourceWire `yaml:"resources,omitempty"`
}

type environmentResourceWire struct {
	Name      string `yaml:"name"`
	Count     *int   `yaml:"count,omitempty"`
	Blueprint string `yaml:"blueprint"`
	Network   string `yaml:"network,omitempty"`
	IP        string `yaml:"ip,omitempty"`
	// NICs is the long form of network and ip, for a machine with more than
	// one card. A manifest uses one form or the other.
	NICs      []environmentNICWire     `yaml:"nics,omitempty"`
	Power     string                   `yaml:"power,omitempty"`
	Image     string                   `yaml:"image,omitempty"`
	Placement *placementWire           `yaml:"placement,omitempty"`
	Firewall  *environmentFirewallWire `yaml:"firewall,omitempty"`
	Inputs    map[string]any           `yaml:"inputs,omitempty"`
}

type environmentNICWire struct {
	Network string `yaml:"network"`
	IP      string `yaml:"ip,omitempty"`
}

// VariableName is what a spec.variables key may be.
var VariableName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Values of spec.images. ImagesApprovedOnly is the default and is written as
// nothing.
const (
	ImagesApprovedOnly = "approved-only"
	ImagesKnownOrWarn  = "known-or-warn"
)

// ImagePattern is what a resource's image may be: a name, optionally pinned
// to a sha256 digest.
var ImagePattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}(@sha256:[0-9a-f]{64})?$`)

// SplitImage separates an image's name from its digest (hex, without the
// sha256: prefix; empty when the image is not pinned).
func SplitImage(image string) (name, digest string) {
	name, pin, found := strings.Cut(image, "@sha256:")
	if !found {
		return image, ""
	}
	return name, pin
}

// normalizeInputs makes the decoded form of a resource's inputs already equal
// to what re-encoding would produce. That is what lets the round trip be a
// fixed point rather than merely reversible, and two ordinary manifests break
// it without this:
//
//   - `inputs: {}` decodes to a non-nil empty map, which `omitempty` then drops
//     on the way out, so a second read yields nil and the two disagree.
//     Normalizing an empty map to nil makes the first read agree with every
//     later one.
//   - A value written `1.0` decodes to float64(1) and re-emits as `1`, which
//     reads back as an int — so both the type and the file changed. Carrying
//     the inputs through one encode/decode cycle applies that conversion once,
//     here, instead of leaving the first re-encode to apply it silently.
//
// The cost is that an integral float in a manifest becomes an int in the
// intent. No blueprint can observe the difference — `1.0` and `1` are the same
// number in YAML and in JSON — and it is far more predictable than a value
// whose type depends on whether anything happened to re-encode it.
//
// Key order is not preserved, because a Go map has none and re-emission
// alphabetizes. That is acceptable only because nothing rewrites an operator's
// manifest: an Environment is not an exportable kind, so the diff is one no
// one ever sees. If Environment ever gains an export path, this is the comment
// that was wrong.
func normalizeInputs(in map[string]any) (map[string]any, error) {
	if len(in) == 0 {
		return nil, nil
	}
	raw, err := yaml.Marshal(in)
	if err != nil {
		return nil, fmt.Errorf("re-encode inputs: %w", err)
	}
	var out map[string]any
	if err := yaml.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("re-decode inputs: %w", err)
	}
	return out, nil
}

// DecodeEnvironment parses and validates one Environment document.
//
// Unknown fields are rejected, by DecodeSpec inside the spec and by
// DecodeDocuments' strict envelope outside it, so a manifest that does not mean
// what it appears to mean is refused rather than partly honoured.
func DecodeEnvironment(doc Document) (Environment, error) {
	if doc.Kind != KindEnvironment {
		return Environment{}, fmt.Errorf("%q: expected kind %s, got %q", doc.Metadata.Name, KindEnvironment, doc.Kind)
	}
	switch strings.TrimSpace(doc.APIVersion) {
	case EnvironmentV1Alpha1:
	case "":
		return Environment{}, fmt.Errorf("%s %q: apiVersion is required and must be %s",
			KindEnvironment, doc.Metadata.Name, EnvironmentV1Alpha1)
	default:
		return Environment{}, fmt.Errorf("%s %q: unsupported apiVersion %q, want %s",
			KindEnvironment, doc.Metadata.Name, doc.APIVersion, EnvironmentV1Alpha1)
	}
	name := strings.TrimSpace(doc.Metadata.Name)
	if name == "" {
		return Environment{}, fmt.Errorf("%s: %w", KindEnvironment, ErrMissingName)
	}

	var wire environmentSpecWire
	if err := DecodeSpec(doc, &wire); err != nil {
		return Environment{}, fmt.Errorf("%s %q: %w", KindEnvironment, name, err)
	}

	vars, err := normalizeInputs(wire.Variables)
	if err != nil {
		return Environment{}, fmt.Errorf("%s %q: spec.variables: %w", KindEnvironment, name, err)
	}
	env := Environment{Name: name, Spec: EnvironmentSpec{
		Integration: strings.TrimSpace(wire.Integration),
		Group:       strings.TrimSpace(wire.Group),
		Variables:   vars,
	}}
	// The default is written as nothing, so a manifest that spells it out
	// decodes to the same value as one that does not.
	if images := strings.TrimSpace(wire.Images); images != ImagesApprovedOnly {
		env.Spec.Images = images
	}
	if env.Spec.Integration == "" {
		return Environment{}, fmt.Errorf("%s %q: spec.integration is required", KindEnvironment, name)
	}
	if env.Spec.ControlPlane, err = decodeControlPlane(wire.ControlPlane); err != nil {
		return Environment{}, fmt.Errorf("%s %q: spec.%w", KindEnvironment, name, err)
	}
	for i, n := range wire.Networks {
		net, err := decodeNetwork(n)
		if err != nil {
			return Environment{}, fmt.Errorf("%s %q: spec.networks[%d]: %w", KindEnvironment, name, i, err)
		}
		env.Spec.Networks = append(env.Spec.Networks, net)
	}

	seen := make(map[string]int, len(wire.Resources))
	for i, r := range wire.Resources {
		rn := strings.TrimSpace(r.Name)
		if rn == "" {
			return Environment{}, fmt.Errorf("%s %q: spec.resources[%d] has no name", KindEnvironment, name, i)
		}
		if first, dup := seen[rn]; dup {
			// Names are intent identity, so two resources sharing one would
			// make two different intents indistinguishable from each other.
			return Environment{}, fmt.Errorf("%s %q: spec.resources[%d] repeats the name %q from resources[%d]",
				KindEnvironment, name, i, rn, first)
		}
		seen[rn] = i

		blueprint := strings.TrimSpace(r.Blueprint)
		if blueprint == "" {
			return Environment{}, fmt.Errorf("%s %q: resource %q has no blueprint", KindEnvironment, name, rn)
		}
		count := 1
		if r.Count != nil {
			// Zero is allowed and means what it says: the resource stays
			// declared and every machine built for it plans as delete.
			if *r.Count < 0 {
				return Environment{}, fmt.Errorf("%s %q: resource %q has count %d, want 0 or more",
					KindEnvironment, name, rn, *r.Count)
			}
			if *r.Count > MaxResourceCount {
				return Environment{}, fmt.Errorf("%s %q: resource %q has count %d, want at most %d",
					KindEnvironment, name, rn, *r.Count, MaxResourceCount)
			}
			count = *r.Count
		}
		inputs, err := normalizeInputs(r.Inputs)
		if err != nil {
			return Environment{}, fmt.Errorf("%s %q: resource %q inputs: %w", KindEnvironment, name, rn, err)
		}
		fw, err := decodeFirewall(r.Firewall)
		if err != nil {
			return Environment{}, fmt.Errorf("%s %q: resource %q: %w", KindEnvironment, name, rn, err)
		}
		res := EnvironmentResource{
			Firewall:  fw,
			Name:      rn,
			Count:     count,
			Blueprint: blueprint,
			Network:   strings.TrimSpace(r.Network),
			IP:        strings.TrimSpace(r.IP),
			Power:     strings.TrimSpace(r.Power),
			Image:     strings.TrimSpace(r.Image),
			Placement: decodePlacement(r.Placement),
			Inputs:    inputs,
		}
		if len(r.NICs) > 0 {
			if res.Network != "" || res.IP != "" {
				return Environment{}, fmt.Errorf("%s %q: resource %q sets both nics and network/ip; nics' first entry is "+
					"the first card, so pick one form", KindEnvironment, name, rn)
			}
			for j, nic := range r.NICs {
				n := EnvironmentNIC{Network: strings.TrimSpace(nic.Network), IP: strings.TrimSpace(nic.IP)}
				if n.Network == "" {
					return Environment{}, fmt.Errorf("%s %q: resource %q nics[%d] names no network", KindEnvironment, name, rn, j)
				}
				if j == 0 {
					res.Network, res.IP = n.Network, n.IP
					continue
				}
				res.ExtraNICs = append(res.ExtraNICs, n)
			}
		}
		env.Spec.Resources = append(env.Spec.Resources, res)
	}
	// The checks above are wire-level: they can say "resources[2]" and can tell
	// an absent count from an explicit zero, which the validated form cannot.
	// Validate is the invariant itself, and running it here too means the
	// decoded path and the planner's hand-built path cannot drift apart. For
	// anything reaching this line it is a no-op.
	if err := env.Validate(); err != nil {
		return Environment{}, err
	}
	return env, nil
}

// Validate re-checks the invariants the Environment doc comment promises. A
// value that came out of DecodeEnvironment always passes it; the reason it
// exists is that the planner will construct Environments itself, and a
// hand-built one has had nothing checked. Without it EncodeEnvironment
// cheerfully emitted `integration: ""` with an empty resource list — a document
// its own decoder rejects.
func (e Environment) Validate() error {
	if strings.TrimSpace(e.Name) == "" {
		return fmt.Errorf("%s: %w", KindEnvironment, ErrMissingName)
	}
	switch {
	case e.Spec.Provider != nil:
		if strings.TrimSpace(e.Spec.Integration) != "" {
			return fmt.Errorf("%s %q: spec.integration and spec.provider are both set; a manifest names one", KindEnvironment, e.Name)
		}
		if err := e.Spec.Provider.Validate(); err != nil {
			return fmt.Errorf("%s %q: spec.provider: %w", KindEnvironment, e.Name, err)
		}
	case strings.TrimSpace(e.Spec.Integration) == "":
		return fmt.Errorf("%s %q: spec.integration is required", KindEnvironment, e.Name)
	}
	if err := e.Spec.ControlPlane.validate(); err != nil {
		return fmt.Errorf("%s %q: spec.%w", KindEnvironment, e.Name, err)
	}
	if e.Spec.Images != "" && e.Spec.Images != ImagesKnownOrWarn {
		return fmt.Errorf("%s %q: spec.images is %q; want %s (the default) or %s",
			KindEnvironment, e.Name, e.Spec.Images, ImagesApprovedOnly, ImagesKnownOrWarn)
	}
	for k, v := range e.Spec.Variables {
		if !VariableName.MatchString(k) {
			return fmt.Errorf("%s %q: variable %q must be letters, digits and _, starting with a letter or _", KindEnvironment, e.Name, k)
		}
		switch v.(type) {
		case string, int, int64, float64, bool:
		default:
			return fmt.Errorf("%s %q: variable %q must be a string, a number or a boolean", KindEnvironment, e.Name, k)
		}
	}
	nets := make(map[string]bool, len(e.Spec.Networks))
	zoneDHCP := map[string]bool{}
	for i, n := range e.Spec.Networks {
		// A zone's DHCP server is the zone's, not a network's: two networks in
		// one zone that disagree would leave one of them misdescribed.
		if prev, seen := zoneDHCP[n.Zone]; seen && prev != (n.DHCPRange != "") {
			return fmt.Errorf("%s %q: spec.networks[%d] (%s) and an earlier network share zone %q but disagree on DHCP; "+
				"DHCP is a zone setting", KindEnvironment, e.Name, i, n.Name, n.Zone)
		}
		zoneDHCP[n.Zone] = n.DHCPRange != ""
		if err := n.validate(); err != nil {
			return fmt.Errorf("%s %q: spec.networks[%d]: %w", KindEnvironment, e.Name, i, err)
		}
		if nets[n.Name] {
			return fmt.Errorf("%s %q: spec.networks[%d] repeats the name %q", KindEnvironment, e.Name, i, n.Name)
		}
		nets[n.Name] = true
	}
	seen := make(map[string]bool, len(e.Spec.Resources))
	for i, r := range e.Spec.Resources {
		if strings.TrimSpace(r.Name) == "" {
			return fmt.Errorf("%s %q: spec.resources[%d] has no name", KindEnvironment, e.Name, i)
		}
		if seen[r.Name] {
			return fmt.Errorf("%s %q: spec.resources[%d] repeats the name %q", KindEnvironment, e.Name, i, r.Name)
		}
		seen[r.Name] = true
		if strings.TrimSpace(r.Blueprint) == "" {
			return fmt.Errorf("%s %q: resource %q has no blueprint", KindEnvironment, e.Name, r.Name)
		}
		if r.Count < 0 || r.Count > MaxResourceCount {
			return fmt.Errorf("%s %q: resource %q has count %d, want 0..%d",
				KindEnvironment, e.Name, r.Name, r.Count, MaxResourceCount)
		}
		switch r.Power {
		case "", "on", "off":
		default:
			return fmt.Errorf("%s %q: resource %q has power %q; want on or off", KindEnvironment, e.Name, r.Name, r.Power)
		}
		if len(r.ExtraNICs) > 0 && r.Network == "" {
			return fmt.Errorf("%s %q: resource %q has more network cards but no first one", KindEnvironment, e.Name, r.Name)
		}
		if len(r.NICs()) > MaxNICs {
			return fmt.Errorf("%s %q: resource %q has %d network cards; at most %d", KindEnvironment, e.Name, r.Name, len(r.NICs()), MaxNICs)
		}
		onNet := map[string]bool{}
		for j, nic := range r.NICs() {
			// Two cards on one network is two addresses on one subnet, and
			// which of them answers is up to the guest's routing.
			if onNet[nic.Network] {
				return fmt.Errorf("%s %q: resource %q has two network cards on %q", KindEnvironment, e.Name, r.Name, nic.Network)
			}
			onNet[nic.Network] = true
			card := "ip"
			if j > 0 {
				card = fmt.Sprintf("nics[%d].ip", j)
			}
			switch nic.IP {
			case "", "dhcp":
			case "static":
				n, ok := e.NetworkByName(nic.Network)
				if !ok && j == 0 {
					return fmt.Errorf("%s %q: resource %q has ip: static, which needs network: to name one of spec.networks",
						KindEnvironment, e.Name, r.Name)
				}
				if !ok {
					return fmt.Errorf("%s %q: resource %q has %s: static, which needs that card's network to be one of spec.networks",
						KindEnvironment, e.Name, r.Name, card)
				}
				// Only the first card is given name servers; a later one only
				// an address, so it needs none declared.
				if j == 0 && len(n.DNS) == 0 {
					return fmt.Errorf("%s %q: resource %q has ip: static on network %q, which declares no dns servers; "+
						"with no DHCP to hand them out the machine could not resolve anything", KindEnvironment, e.Name, r.Name, n.Name)
				}
				if _, _, ok := n.StaticPool(); !ok {
					return fmt.Errorf("%s %q: network %q has no addresses for ip: static; give it a static_range",
						KindEnvironment, e.Name, n.Name)
				}
			default:
				return fmt.Errorf("%s %q: resource %q has %s %q; want dhcp or static", KindEnvironment, e.Name, r.Name, card, nic.IP)
			}
		}
		// An image built from variables is checked once they are filled in, by
		// the planner.
		if r.Image != "" && !strings.Contains(r.Image, "{{var.") && !ImagePattern.MatchString(r.Image) {
			return fmt.Errorf("%s %q: resource %q has image %q; want a name, optionally @sha256:<64 hex digits>",
				KindEnvironment, e.Name, r.Name, r.Image)
		}
		if err := r.Firewall.validate(); err != nil {
			return fmt.Errorf("%s %q: resource %q: %w", KindEnvironment, e.Name, r.Name, err)
		}
		if err := e.validatePlacement(r); err != nil {
			return err
		}
		for j := range r.NICs() {
			if _, both := r.Inputs[NICInputN(j)]; both {
				return fmt.Errorf("%s %q: resource %q declares network card %d and sets inputs.%s; the card is shorthand for that input, so pick one",
					KindEnvironment, e.Name, r.Name, j, NICInputN(j))
			}
		}
	}
	return nil
}

// EncodeEnvironment renders a validated Environment back to a Document.
//
// It emits the normalized form, so an omitted count comes back as an explicit
// one. Together with normalizeInputs on the way in, that makes the round trip a
// fixed point and not merely reversible:
// DecodeEnvironment(EncodeEnvironment(env)) equals env for every env
// DecodeEnvironment produced.
//
// The claim is exactly that and no wider. It is about the *value*, not the
// file: re-emission alphabetizes `inputs` keys and drops the fractional part of
// an integral float, so the YAML text is canonicalized rather than reproduced.
// See normalizeInputs for why that is safe here and what would make it unsafe.
func EncodeEnvironment(env Environment) (Document, error) {
	if err := env.Validate(); err != nil {
		return Document{}, err
	}
	wire := environmentSpecWire{
		Integration:  env.Spec.Integration,
		Group:        env.Spec.Group,
		Images:       env.Spec.Images,
		ControlPlane: encodeControlPlane(env.Spec.ControlPlane),
		Variables:    env.Spec.Variables,
	}
	for _, n := range env.Spec.Networks {
		snat := n.SNAT
		dhcp := n.DHCPRange != ""
		wire.Networks = append(wire.Networks, environmentNetworkWire{
			Name: n.Name, Zone: n.Zone, Subnet: n.Subnet, Gateway: n.Gateway, SNAT: &snat,
			DHCP: &dhcp, DHCPRange: n.DHCPRange, StaticRange: n.StaticRange, DNS: n.DNS,
			ControlPlane: encodeControlPlane(n.ControlPlane),
		})
	}
	for _, r := range env.Spec.Resources {
		count := r.Count
		w := environmentResourceWire{
			Name:      r.Name,
			Count:     &count,
			Blueprint: r.Blueprint,
			Network:   r.Network,
			IP:        r.IP,
			Power:     r.Power,
			Image:     r.Image,
			Firewall:  encodeFirewall(r.Firewall),
			Placement: encodePlacement(r.Placement),
			Inputs:    r.Inputs,
		}
		// One card is written the short way, as it always was; more as the
		// list, which the short form cannot say.
		if len(r.ExtraNICs) > 0 {
			w.Network, w.IP = "", ""
			for _, nic := range r.NICs() {
				w.NICs = append(w.NICs, environmentNICWire{Network: nic.Network, IP: nic.IP})
			}
		}
		wire.Resources = append(wire.Resources, w)
	}
	spec, err := EncodeSpec(wire)
	if err != nil {
		return Document{}, fmt.Errorf("%s %q: %w", KindEnvironment, env.Name, err)
	}
	if env.Spec.Provider != nil {
		return encodeStandalone(env, spec)
	}
	return Document{
		APIVersion: EnvironmentV1Alpha1,
		Kind:       KindEnvironment,
		Metadata:   Metadata{Name: env.Name},
		Spec:       spec,
	}, nil
}

// References lists what the manifest points at, by name and without resolving
// anything, for a consumer that needs to report every missing dependency at
// once instead of failing on the first.
//
// The order is the manifest's own: the integration, then the group if there is
// one, then each distinct blueprint in the order its first resource declared
// it. Blueprints are de-duplicated because ten resources built from one
// blueprint are one dependency, not ten.
func (e Environment) References() []Reference {
	var refs []Reference
	// A standalone manifest names a provider, which is not something to
	// resolve by name.
	if e.Spec.Provider == nil {
		refs = append(refs, Reference{Kind: RefIntegration, Name: e.Spec.Integration})
	}
	if e.Spec.Group != "" {
		refs = append(refs, Reference{Kind: RefGroup, Name: e.Spec.Group})
	}
	seen := make(map[string]bool, len(e.Spec.Resources))
	for _, r := range e.Spec.Resources {
		if seen[r.Blueprint] {
			continue
		}
		seen[r.Blueprint] = true
		refs = append(refs, Reference{Kind: RefProvisionBlueprint, Name: r.Blueprint})
	}
	return refs
}

// Total is how many machines the manifest asks for across every resource. It
// is the number a plan has to justify, so it is worth naming rather than
// recomputing at each call site. MaxResourceCount keeps it from overflowing.
func (e Environment) Total() int {
	var n int
	for _, r := range e.Spec.Resources {
		n += r.Count
	}
	return n
}

// Emptied is the same Environment declaring nothing: its name and connector
// only. Applying it plans every machine built for it as delete, which is what
// deleting an Environment is. The connector stays because the removals run
// through it; the group, variables and networks go, because nothing is left
// to join, fill in or attach — and Sybil never removes a network, so dropping
// one from the manifest changes nothing on the provider.
func (e Environment) Emptied() Environment {
	out := Environment{Name: e.Name, Spec: EnvironmentSpec{Integration: e.Spec.Integration}}
	if e.Spec.Provider != nil {
		p := *e.Spec.Provider
		out.Spec.Provider = &p
	}
	return out
}

// Declares reports whether the manifest asks for anything at all: a machine
// or a network.
func (e Environment) Declares() bool {
	return e.Total() > 0 || len(e.Spec.Networks) > 0
}

// ResourceNames returns the declared resource names in sorted order, so an
// error message about a manifest can name what it contains without depending on
// declaration order. Sorted rather than declared order precisely because this
// is for reporting, not for reading the manifest back — Spec.Resources is the
// order to iterate when order matters.
func (e Environment) ResourceNames() []string {
	out := make([]string, 0, len(e.Spec.Resources))
	for _, r := range e.Spec.Resources {
		out = append(out, r.Name)
	}
	sort.Strings(out)
	return out
}

// NICInput is the blueprint input a resource's `network` expands into. The
// stock machine blueprint calls its first NIC this; a blueprint that does not
// declare it is reported by the planner's input check like any other unknown
// input.
const NICInput = "net0"

// NetworkByName returns the declared network called name, if there is one.
func (e Environment) NetworkByName(name string) (EnvironmentNetwork, bool) {
	for _, n := range e.Spec.Networks {
		if n.Name == name {
			return n, true
		}
	}
	return EnvironmentNetwork{}, false
}

// decodeNetwork applies a network's defaults and validates it. The defaults
// are written into the value, so the plan, the apply and an encode all see the
// same zone, gateway and SNAT, and a manifest that leaves them out means the
// same thing as one that spells them out.
func decodeNetwork(w environmentNetworkWire) (EnvironmentNetwork, error) {
	n := EnvironmentNetwork{
		Name:    strings.TrimSpace(w.Name),
		Zone:    strings.TrimSpace(w.Zone),
		Subnet:  strings.TrimSpace(w.Subnet),
		Gateway: strings.TrimSpace(w.Gateway),
		SNAT:    true,
	}
	if w.SNAT != nil {
		n.SNAT = *w.SNAT
	}
	if n.Zone == "" {
		n.Zone = DefaultNetworkZone
	}
	prefix, err := netip.ParsePrefix(n.Subnet)
	if err != nil {
		return EnvironmentNetwork{}, fmt.Errorf("network %q: subnet %q is not a CIDR such as 10.20.0.0/24", n.Name, n.Subnet)
	}
	if prefix.Masked() != prefix {
		// 10.20.0.7/24 is a host written with a mask, not a subnet; taking it
		// as 10.20.0.0/24 silently would hide a typo in the one field the
		// machines' addressing depends on.
		return EnvironmentNetwork{}, fmt.Errorf("network %q: subnet %q has host bits set; the subnet is %s",
			n.Name, n.Subnet, prefix.Masked())
	}
	if n.Gateway == "" && prefix.Addr().Is4() && prefix.Bits() < 31 {
		n.Gateway = prefix.Addr().Next().String()
	}
	dhcp := w.DHCP != nil && *w.DHCP
	switch {
	case !dhcp && strings.TrimSpace(w.DHCPRange) != "":
		return EnvironmentNetwork{}, fmt.Errorf("network %q: dhcp_range is set but dhcp is not true; DHCP needs "+
			"the dnsmasq package on every node, so it is turned on explicitly with dhcp: true", n.Name)
	case dhcp && strings.TrimSpace(w.DHCPRange) != "":
		n.DHCPRange = strings.ReplaceAll(strings.TrimSpace(w.DHCPRange), " ", "")
	case dhcp:
		gw, _ := netip.ParseAddr(n.Gateway)
		if n.DHCPRange = defaultDHCPRange(prefix, gw); n.DHCPRange == "" {
			return EnvironmentNetwork{}, fmt.Errorf("network %q: no default DHCP range for %s; give dhcp_range", n.Name, n.Subnet)
		}
	}
	n.StaticRange = strings.ReplaceAll(strings.TrimSpace(w.StaticRange), " ", "")
	for _, d := range w.DNS {
		n.DNS = append(n.DNS, strings.TrimSpace(d))
	}
	cp, err := decodeControlPlane(w.ControlPlane)
	if err != nil {
		return EnvironmentNetwork{}, fmt.Errorf("network %q: %w", n.Name, err)
	}
	n.ControlPlane = cp
	return n, n.validate()
}

func (n EnvironmentNetwork) validate() error {
	if !NetworkNamePattern.MatchString(n.Name) {
		return fmt.Errorf("network name %q must be 1-8 lowercase letters and digits, starting with a letter "+
			"(Proxmox uses it as an interface name)", n.Name)
	}
	if !NetworkNamePattern.MatchString(n.Zone) {
		return fmt.Errorf("network %q: zone %q must be 1-8 lowercase letters and digits, starting with a letter", n.Name, n.Zone)
	}
	prefix, err := netip.ParsePrefix(n.Subnet)
	if err != nil || prefix.Masked() != prefix {
		return fmt.Errorf("network %q: subnet %q is not a canonical CIDR", n.Name, n.Subnet)
	}
	gw, err := netip.ParseAddr(n.Gateway)
	if err != nil {
		return fmt.Errorf("network %q: gateway %q is not an address", n.Name, n.Gateway)
	}
	if !prefix.Contains(gw) || gw == prefix.Addr() || (gw.Is4() && prefix.Bits() < 31 && gw == lastAddr(prefix)) {
		return fmt.Errorf("network %q: gateway %s must be a host address inside %s", n.Name, gw, prefix)
	}
	if n.DHCPRange != "" {
		s, e, ok := strings.Cut(n.DHCPRange, "-")
		start, err1 := netip.ParseAddr(s)
		end, err2 := netip.ParseAddr(e)
		switch {
		case !ok || err1 != nil || err2 != nil:
			return fmt.Errorf("network %q: dhcp_range %q must be START-END, two addresses", n.Name, n.DHCPRange)
		case !prefix.Contains(start) || !prefix.Contains(end) || end.Less(start):
			return fmt.Errorf("network %q: dhcp_range %s must run forwards inside %s", n.Name, n.DHCPRange, prefix)
		case !gw.Less(start) && !end.Less(gw):
			return fmt.Errorf("network %q: dhcp_range %s contains the gateway %s", n.Name, n.DHCPRange, gw)
		}
	}
	if n.StaticRange != "" {
		start, end, err := parseRange(n.StaticRange)
		switch {
		case err != nil:
			return fmt.Errorf("network %q: static_range %q must be START-END, two addresses", n.Name, n.StaticRange)
		case !prefix.Contains(start) || !prefix.Contains(end) || end.Less(start):
			return fmt.Errorf("network %q: static_range %s must run forwards inside %s", n.Name, n.StaticRange, prefix)
		case !gw.Less(start) && !end.Less(gw):
			return fmt.Errorf("network %q: static_range %s contains the gateway %s", n.Name, n.StaticRange, gw)
		}
		if n.DHCPRange != "" {
			ds, de, _ := parseRange(n.DHCPRange)
			if !(end.Less(ds) || de.Less(start)) {
				return fmt.Errorf("network %q: static_range %s overlaps dhcp_range %s", n.Name, n.StaticRange, n.DHCPRange)
			}
		}
	}
	for _, d := range n.DNS {
		if _, err := netip.ParseAddr(d); err != nil {
			return fmt.Errorf("network %q: dns %q is not an address", n.Name, d)
		}
	}
	if err := n.ControlPlane.validate(); err != nil {
		return fmt.Errorf("network %q: %w", n.Name, err)
	}
	return nil
}

// parseRange reads START-END.
func parseRange(r string) (netip.Addr, netip.Addr, error) {
	s, e, ok := strings.Cut(r, "-")
	if !ok {
		return netip.Addr{}, netip.Addr{}, fmt.Errorf("not START-END")
	}
	start, err := netip.ParseAddr(s)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	end, err := netip.ParseAddr(e)
	if err != nil {
		return netip.Addr{}, netip.Addr{}, err
	}
	return start, end, nil
}

// StaticPool is where `ip: static` addresses come from on n: the declared
// static_range, else the addresses after the gateway and before the DHCP
// range (or the subnet's last host). Empty when there are none.
func (n EnvironmentNetwork) StaticPool() (start, end netip.Addr, ok bool) {
	if n.StaticRange != "" {
		s, e, err := parseRange(n.StaticRange)
		return s, e, err == nil
	}
	prefix, err := netip.ParsePrefix(n.Subnet)
	gw, gerr := netip.ParseAddr(n.Gateway)
	if err != nil || gerr != nil || !prefix.Addr().Is4() || prefix.Bits() > 30 {
		return netip.Addr{}, netip.Addr{}, false
	}
	start = gw.Next()
	end = lastAddr(prefix).Prev()
	if n.DHCPRange != "" {
		if ds, _, err := parseRange(n.DHCPRange); err == nil && start.Less(ds) {
			end = ds.Prev()
		}
	}
	if !prefix.Contains(start) || end.Less(start) {
		return netip.Addr{}, netip.Addr{}, false
	}
	return start, end, true
}

// defaultDHCPRange is the upper half of an IPv4 subnet, less the broadcast
// address, ending below the gateway when the gateway falls in it. Empty for
// anything it cannot halve sensibly (IPv6, or a subnet smaller than /29), where
// the manifest has to say.
func defaultDHCPRange(p netip.Prefix, gw netip.Addr) string {
	if !p.Addr().Is4() || p.Bits() > 29 {
		return ""
	}
	base := p.Addr().As4()
	n := uint32(base[0])<<24 | uint32(base[1])<<16 | uint32(base[2])<<8 | uint32(base[3])
	size := uint32(1) << (32 - p.Bits())
	start, end := n+size/2, n+size-2
	if gw.Is4() {
		g4 := gw.As4()
		g := uint32(g4[0])<<24 | uint32(g4[1])<<16 | uint32(g4[2])<<8 | uint32(g4[3])
		if g >= start && g <= end {
			end = g - 1
		}
	}
	if end < start {
		return ""
	}
	ip := func(v uint32) string {
		return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)}).String()
	}
	return ip(start) + "-" + ip(end)
}

// lastAddr is a prefix's last address: the broadcast address of IPv4.
func lastAddr(p netip.Prefix) netip.Addr {
	a := p.Masked().Addr().As4()
	n := uint32(a[0])<<24 | uint32(a[1])<<16 | uint32(a[2])<<8 | uint32(a[3])
	n |= (uint32(1) << (32 - p.Bits())) - 1
	return netip.AddrFrom4([4]byte{byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)})
}
