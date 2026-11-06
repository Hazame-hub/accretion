package provider

import "time"

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
	// Install is set when the machine is installed from installer media
	// with an Installer rather than cloned: Clone is then unused.
	Install *InstallRecipe
}

// InstallRecipe is how a machine is installed from installer media.
type InstallRecipe struct {
	// Create is the request without its per-machine guards, as Clone is.
	Create CreateRequest
	// AnswerFormat is the answer file's format; "kickstart" is the only one.
	AnswerFormat string
	// Disk is the guest's name for the disk Create makes, the only one the
	// answer file may partition.
	Disk string
	// Timeout bounds one apply's wait for the installer to power the machine
	// off.
	Timeout time.Duration
	// ISOSHA256 is the installer image's SHA-256 as the manifest declares it,
	// "" for none. Recorded as declared: nothing checks it.
	ISOSHA256 string
}
