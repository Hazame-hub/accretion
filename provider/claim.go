package provider

// Claim is what another tool's record says one resource is: a Terraform
// state's entry for a VM, for one. It is the expected side of a verify, and
// it names the owner, so that accrete can tell a resource another tool owns
// from a stranger.
//
// The provider that maps a record into a Claim also observes the same fields
// on what it discovers (see ClaimFields), so the comparison itself knows no
// provider: a field is the same string on both sides or it is not.
type Claim struct {
	// Key matches the claim to a discovered resource. It is what the
	// provider's ClaimFields returns for the same resource: on Proxmox,
	// "qemu/<vmid>", since a VMID is unique in the cluster and stays the
	// same when the guest moves to another node.
	Key string `json:"key"`
	// Kind is the kind of resource the record is about.
	Kind Kind `json:"kind"`
	// Name is the resource's name as the record has it.
	Name string `json:"name"`
	// Fields are what the record says, in the order a reader expects them.
	Fields []Field `json:"fields"`
	// Open names a set field the record lists every member of (on Proxmox,
	// "nics": the network cards), so that a member the resource has and the
	// record does not is reported too. Each name is the Field's.
	Open []string `json:"open,omitempty"`
	// Notes are what the reader should know about what was not claimed.
	Notes []string `json:"notes,omitempty"`
}

// Field is one value a record claims.
type Field struct {
	// Name is the field's name, the same on both sides (cores, memory_mib,
	// net1.bridge).
	Name string `json:"name"`
	// Value is the claimed value, normalised as the provider observes it.
	Value string `json:"value"`
	// Change is the kind of provider change that would explain a
	// difference (ChangeConfig, ChangeDisk, ChangePower), "" when none can
	// (the machine's identity, the node it runs on).
	Change string `json:"change,omitempty"`
	// Identity marks a field that names the machine: a difference there means
	// a different machine, not a changed one.
	Identity bool `json:"identity,omitempty"`
}

// Observed is what a provider reads off one discovered resource, for the
// fields its claims name.
type Observed struct {
	// Key is the same as a Claim's Key for the same resource.
	Key string
	// Fields maps a field's name to its value. A name that is absent was not
	// read (the provider did not say); "" is a value the resource has empty.
	Fields map[string]string
	// Members maps an open set field to the members the resource has.
	Members map[string][]string
	// Notes are what the reader should know about this observation (a
	// change waiting for the next restart).
	Notes []string
}
