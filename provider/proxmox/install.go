package proxmox

import (
	"fmt"
	"regexp"

	"strings"
	"time"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
)

// InstallBlueprint builds a machine from an installer ISO with no keyboard:
// an empty VM, the ISO, and a kickstart on a disc labelled OEMDRV that
// Anaconda reads by itself. See openspec accrete-installer-iso.
//
// It is accrete's own and stays out of Blueprints(), the list Sybil's
// Continuum ships (design D2): a Continuum that does not know it refuses it,
// rather than booting an installer that has no answer file.
const InstallBlueprint = "install-proxmox-vm"

// AnswerFormatKickstart is the one answer format this slice writes.
const AnswerFormatKickstart = "kickstart"

// DefaultInstallTimeout is how long one apply waits for an install to power
// the machine off, unless the manifest says otherwise.
const DefaultInstallTimeout = 60 * time.Minute

// AccreteBlueprints is the blueprints accrete plans and builds with: the ones
// Sybil ships, InstallBlueprints and ImageBlueprints. Each call returns a
// fresh copy.
func AccreteBlueprints() []plan.Blueprint {
	return append(append(Blueprints(), InstallBlueprints()...), ImageBlueprints()...)
}

// InstallBlueprints is accrete's own blueprints, which Sybil does not run.
func InstallBlueprints() []plan.Blueprint {
	return []plan.Blueprint{{
		ID:   InstallBlueprint,
		Name: "Install a VM from an installer ISO, unattended (Proxmox)",
		Description: "Create an empty VM, boot a vendor installer ISO with a kickstart on a disc of its own, and accept the machine " +
			"only once it powered off, booted from its own disk with the discs removed, and its guest agent reports the declared hostname.",
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
				Help:     "DNS-label characters only: the VM's name and the hostname the kickstart sets, which the guest agent must report before the machine is accepted.",
				Observes: "name"},
			{Name: "iso", Type: "string", Required: true, Label: "Installer ISO",
				Help: "Volume id of the vendor installer, e.g. local:iso/rhel-9.6-x86_64-dvd.iso. accrete never removes it."},
			{Name: "iso_sha256", Type: "string", Label: "Installer ISO SHA-256 (declared)",
				Help: "Recorded as declared. accrete cannot hash a file on the node's storage, so it never says it checked it."},
			{Name: "answer_format", Type: "string", Default: AnswerFormatKickstart, Label: "Answer file format",
				Help: "kickstart: RHEL and its rebuilds. The only format this release writes."},
			{Name: "install_timeout_minutes", Type: "int", Default: 60, Label: "Install deadline (minutes)",
				Help: "How long one apply waits for the installer to power the machine off. A machine still installing is kept, and the next apply waits again."},
			{Name: "seed_storage", Type: "string", Required: true, Default: "local", Label: "Answer disc storage",
				Help: "A storage that allows iso content: carries the machine's own OEMDRV disc, removed once the install is done.", Selector: "snippet_storage"},
			{Name: "disk_storage", Type: "string", Required: true, Default: "local-lvm", Label: "Disk storage",
				Help: "Where the empty install disk is created", Selector: "image_storage"},
			{Name: "disk_gb", Type: "int", Default: 32, Label: "Disk (GiB)", Help: "The install disk. The kickstart partitions this disk and no other.",
				Observes: plan.ObservedBootDiskGiB, GrowOnly: true},
			{Name: "cores", Type: "int", Default: 2, Label: "CPU cores", Observes: plan.ObservedCPUCores},
			{Name: "memory_mb", Type: "int", Default: 2048, Label: "Memory (MB)", Help: "RHEL 9's installer needs at least 1536 MB.",
				Observes: plan.ObservedMemoryMiB},
			{Name: "cpu", Type: "string", Default: "x86-64-v2-AES", Label: "CPU type",
				Help: "RHEL 9 needs x86-64-v2 at least; host is faster and pins the VM to this hardware."},
			{Name: "net0", Type: "string", Default: "virtio,bridge=vmbr0", Label: "Network device",
				Help: "The one card; the kickstart takes DHCP on it."},
			{Name: "bootstrap", Type: "bootstrap", Label: "Bootstrap profile",
				Help: "Users with their SSH keys and sudo, packages, timezone and files, written into the kickstart. Root stays locked. Commands to run and secrets are refused: the disc is readable from storage."},
		},
	}}
}

// isoVolume is an ISO volume id on a storage, as the installer input names it.
var isoVolume = regexp.MustCompile(`^([A-Za-z][A-Za-z0-9._-]*):iso/([A-Za-z0-9][A-Za-z0-9._+-]*\.iso)$`)

var sha256Hex = regexp.MustCompile(`^[0-9a-fA-F]{64}$`)

// installRecipe is Recipe for InstallBlueprint.
func installRecipe(inputs map[string]any, str func(string, string) string, num func(string, int) (int, error)) (provider.Recipe, error) {
	known := map[string]bool{}
	for _, in := range InstallBlueprints()[0].Inputs {
		known[in.Name] = true
	}
	for k := range inputs {
		if !known[k] {
			return provider.Recipe{}, fmt.Errorf("input %s: %s takes no such input", k, InstallBlueprint)
		}
	}
	node := str("pve_node", "pve")
	hostname := str("hostname", "")
	iso := str("iso", "")
	seed := str("seed_storage", "local")
	disk := str("disk_storage", "local-lvm")
	format := str("answer_format", AnswerFormatKickstart)
	sum := str("iso_sha256", "")
	switch {
	case format != AnswerFormatKickstart:
		return provider.Recipe{}, fmt.Errorf("input answer_format: %q is not a format accrete writes; it writes %s only", format, AnswerFormatKickstart)
	case !nodeName.MatchString(node):
		return provider.Recipe{}, fmt.Errorf("input pve_node: %q is not a node name", node)
	case !nodeName.MatchString(hostname):
		return provider.Recipe{}, fmt.Errorf("input hostname: %q is not a DNS label", hostname)
	case !isoVolume.MatchString(iso):
		return provider.Recipe{}, fmt.Errorf("input iso: %q is not an ISO volume such as local:iso/rhel-9.6-x86_64-dvd.iso", iso)
	case !storageName.MatchString(seed):
		return provider.Recipe{}, fmt.Errorf("input seed_storage: %q is not a storage name", seed)
	case !storageName.MatchString(disk):
		return provider.Recipe{}, fmt.Errorf("input disk_storage: %q is not a storage name", disk)
	case sum != "" && !sha256Hex.MatchString(sum):
		return provider.Recipe{}, fmt.Errorf("input iso_sha256: %q is not a SHA-256 in hex", sum)
	}
	cpu := str("cpu", "x86-64-v2-AES")
	if !cpuModel.MatchString(cpu) {
		return provider.Recipe{}, fmt.Errorf("input cpu: %q is not a model name", cpu)
	}
	sizes := map[string]int{}
	for _, in := range []struct {
		key      string
		def, min int
	}{{"cores", 2, 1}, {"memory_mb", 2048, 1536}, {"disk_gb", 32, 10}, {"install_timeout_minutes", 60, 1}} {
		n, err := num(in.key, in.def)
		if err != nil {
			return provider.Recipe{}, err
		}
		if n < in.min {
			return provider.Recipe{}, fmt.Errorf("input %s: %d is below the %d an install needs", in.key, n, in.min)
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
			return provider.Placement{Drive: answerDrive, Volume: volume, Storage: seed, Node: node}
		},
		Install: &provider.InstallRecipe{
			Create:       provider.CreateRequest{Name: hostname, Placement: node, Shape: shape, DiskStorage: disk, Installer: iso},
			AnswerFormat: format,
			Disk:         KickstartDisk,
			Timeout:      time.Duration(sizes["install_timeout_minutes"]) * time.Minute,
			ISOSHA256:    strings.ToLower(sum),
		},
	}, nil
}

// The drives an install uses: the empty disk, the installer, the answer disc.
const (
	installDisk    = "scsi0"
	installerDrive = "ide2"
	answerDrive    = "ide3"
)

// KickstartDisk is the device name the install disk has in the guest, for
// the kickstart to partition it and nothing else: the first disk on the
// VirtIO SCSI controller Create gives the VM.
const KickstartDisk = "sda"
