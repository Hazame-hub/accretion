package proxmox

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hazame-hub/accretion/provider"
)

// BuildableBlueprint is the one blueprint accrete v0.1 builds with a Writer.
// The others Accretion ships (an installer ISO, a template build) are not a
// clone, and Recipe refuses them by name.
const BuildableBlueprint = "provision-proxmox-vm"

// Recipe turns a provision-proxmox-vm instance's inputs into what a Writer
// builds: the clone, the seed's storage and the hostname. Unset inputs take
// the blueprint's defaults. What v0.1 cannot build is refused here, before
// anything is sent: a fixed address (the seed takes DHCP on the first card)
// and a second card. Inputs that only matter to Sybil's own run (the
// connector, the OS check, what to do on a failed enrolment, a
// notification) are ignored.
//
// See openspec accretion-apply, D4 and D8.
func Recipe(blueprint string, inputs map[string]any) (provider.Recipe, error) {
	if blueprint != BuildableBlueprint {
		return provider.Recipe{}, fmt.Errorf("blueprint %q is not one accrete builds; v0.1 builds %s only", blueprint, BuildableBlueprint)
	}
	str := func(key, def string) string {
		v, ok := inputs[key]
		if !ok || v == nil {
			return def
		}
		s := strings.TrimSpace(fmt.Sprint(v))
		if s == "" {
			return def
		}
		return s
	}
	num := func(key string, def int) (int, error) {
		s := str(key, "")
		if s == "" {
			return def, nil
		}
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 {
			return 0, fmt.Errorf("input %s: %q is not a whole number", key, s)
		}
		return n, nil
	}
	for _, key := range []string{"ip_address", "gateway", "nameservers", "ip_address1", "ip_address2", "ip_address3"} {
		if str(key, "") != "" {
			return provider.Recipe{}, fmt.Errorf("input %s: accrete v0.1 builds machines that take DHCP on their first card; fixed addresses come later", key)
		}
	}
	for _, key := range []string{"net1", "net2", "net3"} {
		if str(key, "") != "" {
			return provider.Recipe{}, fmt.Errorf("input %s: accrete v0.1 builds machines with one network card", key)
		}
	}
	node := str("pve_node", "pve")
	template := str("template_vmid", "")
	hostname := str("hostname", "")
	storage := str("snippet_storage", "local")
	switch {
	case !nodeName.MatchString(node):
		return provider.Recipe{}, fmt.Errorf("input pve_node: %q is not a node name", node)
	case !vmidValue.MatchString(template):
		return provider.Recipe{}, fmt.Errorf("input template_vmid: %q is not a VMID", template)
	case !nodeName.MatchString(hostname):
		return provider.Recipe{}, fmt.Errorf("input hostname: %q is not a DNS label", hostname)
	case !storageName.MatchString(storage):
		return provider.Recipe{}, fmt.Errorf("input snippet_storage: %q is not a storage name", storage)
	}
	cores, err := num("cores", 2)
	if err != nil {
		return provider.Recipe{}, err
	}
	memory, err := num("memory_mb", 2048)
	if err != nil {
		return provider.Recipe{}, err
	}
	disk, err := num("disk_gb", 0)
	if err != nil {
		return provider.Recipe{}, err
	}
	return provider.Recipe{
		Clone: provider.CloneRequest{
			Template: provider.Ref{Kind: provider.KindTemplate, ID: template}, Placement: node,
			Name: hostname, Full: true,
			Shape: provider.Shape{Cores: cores, MemoryMB: memory, CPU: str("cpu", "x86-64-v2-AES"), DiskGB: disk,
				NICs: []string{str("net0", "virtio,bridge=vmbr0")}},
		},
		Hostname:    hostname,
		SeedStorage: storage,
		Bootstrap:   inputs["bootstrap"],
		Seed: func(instanceID string) provider.Placement {
			_, volume := seedVolume(storage, instanceID)
			return provider.Placement{Drive: "ide2", Volume: volume, Storage: storage, Node: node}
		},
	}, nil
}
