package proxmox

import (
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
)

// ImageBlueprint builds a machine from a disk image on a storage's import
// content: a cloud image of any OS that reads its first-boot config from a
// NoCloud "cidata" disc. That is cloud-init for most distributions, and
// Ignition for Fedora CoreOS, whose proxmoxve image reads user-data from the
// same disc. No template is made by hand and no installer runs.
//
// Like InstallBlueprint it is accrete's own and stays out of Blueprints().
const ImageBlueprint = "image-proxmox-vm"

// DefaultFirstBootTimeout is how long one apply waits for a machine's first
// boot to power it off, unless the manifest says otherwise.
const DefaultFirstBootTimeout = 15 * time.Minute

// importVolume is a disk image on a storage's import content, as the image
// input names it. Proxmox VE 8.2 and later keep qcow2, raw and vmdk there.
var importVolume = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9._-]*):import/([A-Za-z0-9][A-Za-z0-9._+-]*\.(qcow2|raw|vmdk))$`)

// ImageBlueprints is the blueprints that build from a disk image.
func ImageBlueprints() []plan.Blueprint {
	return []plan.Blueprint{{
		ID:   ImageBlueprint,
		Name: "Build a VM from a disk image, cloud-init or Ignition (Proxmox)",
		Description: "Create a VM whose disk is a copy of a cloud image (Fedora CoreOS, Debian, Ubuntu, Rocky...), seed its first boot with " +
			"cloud-config or an Ignition config, and accept it only once that first boot powered it off by itself and it runs again from its disk.",
		Role:         "create_machine",
		DestroyedBy:  "destroy-proxmox-vm",
		PoweredOnBy:  "start-proxmox-vm",
		PoweredOffBy: "shutdown-proxmox-vm",
		Inputs: []plan.BlueprintInput{
			{Name: "integration", Type: "string", Required: true, Label: "Proxmox connector", Help: "Which Proxmox cluster to build on",
				Source: "integrations", Filter: map[string]string{"type": "proxmox"}},
			{Name: "pve_node", Type: "string", Required: true, Default: "pve", Label: "Proxmox node",
				Help: "The cluster node to create the VM on", Selector: "node"},
			{Name: "hostname", Type: "string", Required: true, Label: "Hostname",
				Help: "DNS-label characters only: the VM's name and the hostname its seed sets.", Observes: "name"},
			{Name: "image", Type: "string", Required: true, Label: "Disk image",
				Help: "Volume id of the image in a storage's import content, e.g. local:import/fedora-coreos-44.20261023.3.1-proxmoxve.x86_64.qcow2. accrete never removes it."},
			{Name: "image_sha256", Type: "string", Label: "Disk image SHA-256 (declared)",
				Help: "Recorded as declared. accrete cannot hash a file on the node's storage, so it never says it checked it."},
			{Name: "seed_format", Type: "string", Default: provider.SeedCloudInit, Label: "First-boot config format",
				Help: "cloud-init for images that run cloud-init; ignition for Fedora CoreOS and other images that run Ignition."},
			{Name: "first_boot_timeout_minutes", Type: "int", Default: 15, Label: "First boot deadline (minutes)",
				Help: "How long one apply waits for the first boot to power the machine off. A machine still in its first boot is kept, and the next apply waits again."},
			{Name: "seed_storage", Type: "string", Required: true, Default: "local", Label: "Seed storage",
				Help: "A storage that allows iso content: carries the machine's own cidata disc.", Selector: "snippet_storage"},
			{Name: "disk_storage", Type: "string", Required: true, Default: "local-lvm", Label: "Disk storage",
				Help: "Where the copy of the image is made", Selector: "image_storage"},
			{Name: "disk_gb", Type: "int", Default: 0, Label: "Disk (GiB)", Help: "The boot disk is grown to this size; 0 keeps the image's size.",
				Observes: plan.ObservedBootDiskGiB, GrowOnly: true},
			{Name: "cores", Type: "int", Default: 2, Label: "CPU cores", Observes: plan.ObservedCPUCores},
			{Name: "memory_mb", Type: "int", Default: 2048, Label: "Memory (MB)", Help: "Fedora CoreOS needs at least 2048 MB.",
				Observes: plan.ObservedMemoryMiB},
			{Name: "cpu", Type: "string", Default: "x86-64-v2-AES", Label: "CPU type",
				Help: "Fedora CoreOS and EL9 need x86-64-v2 at least; host is faster and pins the VM to this hardware."},
			{Name: "net0", Type: "string", Default: "virtio,bridge=vmbr0", Label: "Network device",
				Help: "The one card; the machine takes DHCP on it."},
			{Name: "bootstrap", Type: "bootstrap", Label: "Bootstrap profile",
				Help: "Users with their SSH keys and sudo, timezone and files, written into the seed. With ignition, packages and commands are refused. Secrets are refused: the disc is readable from storage."},
		},
	}}
}

// imageRecipe is Recipe for ImageBlueprint.
func imageRecipe(inputs map[string]any, str func(string, string) string, num func(string, int) (int, error)) (provider.Recipe, error) {
	known := map[string]bool{}
	for _, in := range ImageBlueprints()[0].Inputs {
		known[in.Name] = true
	}
	for k := range inputs {
		if !known[k] {
			return provider.Recipe{}, fmt.Errorf("input %s: %s takes no such input", k, ImageBlueprint)
		}
	}
	node := str("pve_node", "pve")
	hostname := str("hostname", "")
	image := str("image", "")
	seed := str("seed_storage", "local")
	disk := str("disk_storage", "local-lvm")
	format := str("seed_format", provider.SeedCloudInit)
	sum := str("image_sha256", "")
	cpu := str("cpu", "x86-64-v2-AES")
	switch {
	case format != provider.SeedCloudInit && format != provider.SeedIgnition:
		return provider.Recipe{}, fmt.Errorf("input seed_format: %q is not a format accrete writes; it writes %s and %s", format, provider.SeedCloudInit, provider.SeedIgnition)
	case !nodeName.MatchString(node):
		return provider.Recipe{}, fmt.Errorf("input pve_node: %q is not a node name", node)
	case !nodeName.MatchString(hostname):
		return provider.Recipe{}, fmt.Errorf("input hostname: %q is not a DNS label", hostname)
	case !importVolume.MatchString(image):
		return provider.Recipe{}, fmt.Errorf("input image: %q is not a disk image in a storage's import content, such as local:import/fedora-coreos-proxmoxve.x86_64.qcow2 (qcow2, raw or vmdk)", image)
	case !storageName.MatchString(seed):
		return provider.Recipe{}, fmt.Errorf("input seed_storage: %q is not a storage name", seed)
	case !storageName.MatchString(disk):
		return provider.Recipe{}, fmt.Errorf("input disk_storage: %q is not a storage name", disk)
	case sum != "" && !sha256Hex.MatchString(sum):
		return provider.Recipe{}, fmt.Errorf("input image_sha256: %q is not a SHA-256 in hex", sum)
	case !cpuModel.MatchString(cpu):
		return provider.Recipe{}, fmt.Errorf("input cpu: %q is not a model name", cpu)
	}
	sizes := map[string]int{}
	for _, in := range []struct {
		key      string
		def, min int
	}{{"cores", 2, 1}, {"memory_mb", 2048, 512}, {"disk_gb", 0, 0}, {"first_boot_timeout_minutes", 15, 1}} {
		n, err := num(in.key, in.def)
		if err != nil {
			return provider.Recipe{}, err
		}
		if n < in.min {
			return provider.Recipe{}, fmt.Errorf("input %s: %d is below the %d a machine needs", in.key, n, in.min)
		}
		sizes[in.key] = n
	}
	shape := provider.Shape{Cores: sizes["cores"], MemoryMB: sizes["memory_mb"], CPU: cpu, DiskGB: sizes["disk_gb"],
		NICs: []string{str("net0", "virtio,bridge=vmbr0")}}
	return provider.Recipe{
		Hostname:    hostname,
		SeedStorage: seed,
		Bootstrap:   inputs["bootstrap"],
		Seed: func(instanceID string) provider.Placement {
			_, volume := seedVolume(seed, instanceID)
			return provider.Placement{Drive: "ide2", Volume: volume, Storage: seed, Node: node}
		},
		Image: &provider.ImageRecipe{
			Create:      provider.CreateRequest{Name: hostname, Placement: node, Shape: shape, DiskStorage: disk, Image: image},
			SeedFormat:  format,
			Timeout:     time.Duration(sizes["first_boot_timeout_minutes"]) * time.Minute,
			ImageSHA256: strings.ToLower(sum),
		},
	}, nil
}
