package provider

import "context"

// Provider is what an infrastructure provider — a hypervisor, a cloud, a
// cluster manager — implements by hand. Four methods; everything else Sybil
// does with the result is Sybil's.
//
// The connection contract (how a provider is configured, which form the
// operator fills in, which privileges it needs) is integrations.ProviderSpec
// and stays there. This is the discovery contract: what exists on the far
// side, and what the far side knows about each thing.
type Provider interface {
	Connect(ctx context.Context, cfg Config) (Session, error)
}

// Session is one open connection. Capabilities is read once after Connect
// and is the only thing outside a provider's own package that anything may
// branch on. Discover enumerates; Describe deepens one resource; Close ends.
type Session interface {
	Capabilities(ctx context.Context) (Capabilities, error)
	Discover(ctx context.Context) ([]Resource, error)
	Describe(ctx context.Context, ref Ref) (Facts, error)
	Close() error
}

// Config is a resolved connection. The engine has already exchanged every
// {{secret.*}} reference for its value; a provider reads it, dials with it,
// and must never log, persist or echo it. Params carries the provider's own
// keys as its ProviderSpec declared them.
type Config struct {
	Endpoint  string
	Params    map[string]any
	VerifyTLS bool
}

// String hides the parameters. A Config that reaches a log line by accident
// shows where it points and nothing else.
func (c Config) String() string { return "provider.Config{" + c.Endpoint + "}" }

// Capabilities is what this session can tell about its resources. It is
// session-level and can differ between two connections of the same kind —
// tools installed or not, a tagging API licensed or not — which is why it is
// read from a connection rather than declared by an author.
//
// A gap a new provider exposes is fixed by adding a field here, never by a
// call site asking which provider it is talking to.
type Capabilities struct {
	// Kinds is what Discover can return.
	Kinds []Kind `json:"kinds"`
	// GuestUUID: Resource.Identity.SMBIOSUUID is populated for vm kinds.
	GuestUUID bool `json:"guest_uuid"`
	// GuestMACs: Resource.Identity.MACs is populated.
	GuestMACs bool `json:"guest_macs"`
	// GuestNetwork: hostname and IPs are available when a guest agent or
	// tools run inside the machine.
	GuestNetwork bool `json:"guest_network"`
	// Hierarchy: Resource.Parent is set (node, host, cluster, pool).
	Hierarchy bool `json:"hierarchy"`
	Tags      bool `json:"tags"`
	// PowerState: Resource.State is more than "unknown".
	PowerState bool `json:"power_state"`
	// Templates: templates are Kind "template" and never imported as machines.
	Templates bool `json:"templates"`
	// Describe: Describe returns more than the Discover row already held.
	// When false, Describe answers with the Discover row's facts.
	Describe bool `json:"describe"`
	// Memory: Resource.Memory.CurrentBytes is reported, so the memory
	// comparison can run against the current effective allocation.
	Memory bool `json:"memory"`
}
