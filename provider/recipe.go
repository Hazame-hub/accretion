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
	// NetworkConfig is the cloud-init network config the seed carries, in
	// the provider's own form (it may stand for values only the built
	// machine has, which its Writer fills in); nil for its default, DHCP on
	// the first card.
	NetworkConfig []byte
	// Seed is where the seed of a machine minted with the given UUID is put,
	// for its removal once the machine is gone.
	Seed func(instanceID string) Placement
	// Install is set when the machine is installed from installer media
	// with an Installer rather than cloned: Clone is then unused.
	Install *InstallRecipe
	// Image is set when the machine's disk is copied from a disk image with
	// an Installer's Create rather than cloned: Clone is then unused.
	Image *ImageRecipe
}

// The formats of the seed a machine built from a disk image reads at its
// first boot.
const (
	// SeedCloudInit is cloud-config, for images that run cloud-init.
	SeedCloudInit = "cloud-init"
	// SeedIgnition is an Ignition config, for Fedora CoreOS and other
	// images that run Ignition.
	SeedIgnition = "ignition"
)

// ImageRecipe is how a machine is built from a disk image: its disk is a
// copy of the image, its seed is read at the first boot, and the first boot
// ends with a power-off the seed asks for, which is what the build waits
// for before it starts the machine again.
type ImageRecipe struct {
	// Create is the request without its per-machine guards, as Clone is.
	Create CreateRequest
	// SeedFormat is SeedCloudInit or SeedIgnition.
	SeedFormat string
	// Timeout bounds one apply's wait for the first boot to power the
	// machine off.
	Timeout time.Duration
	// ImageSHA256 is the image's SHA-256 as the manifest declares it, ""
	// for none. Recorded as declared unless accrete fetched the image from
	// URL, in which case it is the hash of the downloaded file, checked.
	ImageSHA256 string
	// URL, when set, is where accrete fetches the image from when the
	// provider does not have it yet. ImageSHA256 is then required. A URL
	// ending in .xz or .gz is decompressed before upload.
	URL string
}

// InstallRecipe is how a machine is installed from installer media.
type InstallRecipe struct {
	// Create is the request without its per-machine guards, as Clone is.
	Create CreateRequest
	// AnswerFormat is the answer file's format: "kickstart", or "" for an
	// installer that carries its own answers (an OKD agent ISO, say), which
	// gets no answer disc and is accepted on the hypervisor's evidence only.
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
