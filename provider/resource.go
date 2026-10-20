package provider

import (
	"encoding/json"
	"time"
)

// Kind is what a discovered resource is. vm and container become assets;
// template, host, cluster, pool, storage and network are context — they are
// staged and shown, never imported as machines.
type Kind string

const (
	KindVM        Kind = "vm"
	KindContainer Kind = "container"
	KindTemplate  Kind = "template"
	KindHost      Kind = "host"
	KindCluster   Kind = "cluster"
	KindPool      Kind = "pool"
	KindStorage   Kind = "storage"
	KindNetwork   Kind = "network"
)

// Importable reports whether a resource of this kind becomes an asset.
func (k Kind) Importable() bool { return k == KindVM || k == KindContainer }

// Ref is stable within one provider connection: a vmid, a moRef, a domain
// UUID. It is what Describe takes and what external_id stores.
type Ref struct {
	Kind Kind   `json:"kind"`
	ID   string `json:"id"`
}

// Resource is one thing the far side listed, normalised. Raw is the
// provider's own record, verbatim, so a question the normalisation did not
// anticipate can be answered without a re-discover — and so the operator can
// see what the hypervisor actually said.
type Resource struct {
	Ref      Ref              `json:"ref"`
	Name     string           `json:"name"`
	Parent   *Ref             `json:"parent,omitempty"`
	Tags     []string         `json:"tags"`
	State    string           `json:"state"` // "running" | "stopped" | "suspended" | "unknown"
	Identity Identity         `json:"identity"`
	Facts    Facts            `json:"facts"`
	Memory   MemoryAllocation `json:"memory"`
	Raw      json.RawMessage  `json:"raw"`
}

// Identity is what the provider knows that names the machine — the same
// signals the agent collects on the other side, so the two can be matched.
type Identity struct {
	// SMBIOSUUID is the canonical lower-case UUID the guest reads at
	// /sys/class/dmi/id/product_uuid; UUIDForms gives the matcher both forms.
	SMBIOSUUID string `json:"smbios_uuid,omitempty"`
	// MACs are lower-case, normalised with NormalizeMACs.
	MACs []string `json:"mac_addresses"`
	// HypervisorGuestID is the provider's own id for the guest — vSphere
	// instanceUuid, Proxmox vmid — distinct from Ref.ID only where they differ.
	HypervisorGuestID string `json:"hypervisor_guest_id,omitempty"`
	// Hostname and IPs come from a guest agent when the provider has one.
	Hostname string   `json:"hostname,omitempty"`
	IPs      []string `json:"ips"`
}

// MemoryAllocation is a provider fact, not a capability and not a comparator
// constant: whether ballooning is on and what the current target is are
// things only the hypervisor knows, and they change at runtime.
// CurrentBytes is what the guest should be seeing now; 0 means the provider
// could not say, and the memory comparison is suppressed for that resource
// rather than run against a number known to be wrong.
type MemoryAllocation struct {
	MaxBytes     int64 `json:"max_bytes"`
	CurrentBytes int64 `json:"current_bytes"`
	Balloon      bool  `json:"balloon"`
}

// Observation is what one provider reported about one resource at one time.
// It is the document persisted per asset (orbit.assets.provider_observation)
// and per staged row, named for what it holds rather than for one of its
// fields.
type Observation struct {
	Facts      Facts            `json:"facts"`
	Memory     MemoryAllocation `json:"memory"`
	State      string           `json:"state"`
	ObservedAt time.Time        `json:"observed_at"`
}
