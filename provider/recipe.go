package provider

// Recipe is how one declared machine is built with a Writer: the clone it
// starts from, where its seed goes and what the seed says. A provider turns a
// blueprint and its inputs into one (proxmox.Recipe), so a command can build
// a machine without knowing which inputs a provider's blueprint takes.
type Recipe struct {
	// Clone is the request without its per-machine guards: the caller sets
	// MintUUID and OwnerMark, and leaves ID empty so a retry finds the
	// machine an earlier attempt made.
	Clone CloneRequest
	// Hostname is the machine's name, the one cloud-init sets.
	Hostname string
	// SeedStorage is where the cloud-init seed is placed.
	SeedStorage string
	// Bootstrap is the blueprint's bootstrap input as declared, nil for
	// none. Rendering it is the caller's.
	Bootstrap any
	// Seed is where the seed of a machine minted with the given UUID is put,
	// for its removal once the machine is gone.
	Seed func(instanceID string) Placement
}
