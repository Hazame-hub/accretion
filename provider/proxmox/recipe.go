package proxmox

import (
	"fmt"
	"net/netip"
	"regexp"
	"strconv"
	"strings"

	"github.com/hazame-hub/accretion/provider"
)

// poolName is a Proxmox resource pool id.
var poolName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// BuildableBlueprint is the clone blueprint accrete builds with a Writer.
// It also builds ISOBlueprint, InstallBlueprint and ImageBlueprint; the
// template build is refused by name.
const BuildableBlueprint = "provision-proxmox-vm"

// ISOBlueprint is Sybil's blueprint for a VM that boots an installer which
// needs no answer file from accrete (an OKD agent ISO carries its own).
const ISOBlueprint = "provision-proxmox-iso-vm"

// Recipe turns a provision-proxmox-vm instance's inputs into what a Writer
// builds: the clone, the seed's storage and the hostname. Unset inputs take
// the blueprint's defaults. Up to four cards (net0..net3) are built, each
// taking DHCP or the fixed address its ip_address input gives; anything
// malformed there is refused here, before anything is sent. Inputs that only matter to Sybil's own run (the
// connector, the OS check, what to do on a failed enrolment, a
// notification) are ignored.
//
// See openspec accretion-apply, D4 and D8.
func Recipe(blueprint string, inputs map[string]any) (provider.Recipe, error) {
	if blueprint != BuildableBlueprint && blueprint != InstallBlueprint && blueprint != ImageBlueprint && blueprint != ISOBlueprint {
		return provider.Recipe{}, fmt.Errorf("blueprint %q is not one accrete builds; it builds %s, %s, %s and %s", blueprint, BuildableBlueprint, ISOBlueprint, InstallBlueprint, ImageBlueprint)
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
	if blueprint == InstallBlueprint {
		return installRecipe(inputs, str, num)
	}
	if blueprint == ImageBlueprint {
		return imageRecipe(inputs, str, num)
	}
	if blueprint == ISOBlueprint {
		return isoRecipe(inputs, str, num)
	}
	nics, network, err := recipeNetwork(str)
	if err != nil {
		return provider.Recipe{}, err
	}
	node := str("pve_node", "pve")
	template := str("template_vmid", "")
	hostname := str("hostname", "")
	storage := str("snippet_storage", "local")
	pool := str("pool", "")
	switch {
	case !nodeName.MatchString(node):
		return provider.Recipe{}, fmt.Errorf("input pve_node: %q is not a node name", node)
	case !vmidValue.MatchString(template):
		return provider.Recipe{}, fmt.Errorf("input template_vmid: %q is not a VMID", template)
	case !nodeName.MatchString(hostname):
		return provider.Recipe{}, fmt.Errorf("input hostname: %q is not a DNS label", hostname)
	case !storageName.MatchString(storage):
		return provider.Recipe{}, fmt.Errorf("input snippet_storage: %q is not a storage name", storage)
	case pool != "" && !poolName.MatchString(pool):
		return provider.Recipe{}, fmt.Errorf("input pool: %q is not a resource pool name", pool)
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
			Name: hostname, Full: true, Pool: pool,
			Shape: provider.Shape{Cores: cores, MemoryMB: memory, CPU: str("cpu", "x86-64-v2-AES"), DiskGB: disk,
				NICs: nics},
		},
		NetworkConfig: network,
		Hostname:      hostname,
		SeedStorage:   storage,
		Bootstrap:     inputs["bootstrap"],
		Seed: func(instanceID string) provider.Placement {
			_, volume := seedVolume(storage, instanceID)
			return provider.Placement{Drive: "ide2", Volume: volume, Storage: storage, Node: node}
		},
	}, nil
}

// macPlaceholder stands in a recipe's network config for the MAC Proxmox
// gives card i at clone time; GuestInit puts the real one in before the seed
// is written, so each card's addressing matches that card and no other.
func macPlaceholder(i int) string { return fmt.Sprintf("{{accrete.mac.net%d}}", i) }

// recipeNetwork reads the cards (net0..net3) and their addressing. The
// network config is nil for one card on DHCP, which the seed's default
// covers; otherwise it is cloud-init network config version 2 with each card
// matched by its MAC.
func recipeNetwork(str func(key, def string) string) ([]string, []byte, error) {
	nics := []string{str("net0", "virtio,bridge=vmbr0")}
	for i := 1; i <= 3; i++ {
		v := str(fmt.Sprintf("net%d", i), "")
		if v == "" {
			continue
		}
		if len(nics) != i {
			return nil, nil, fmt.Errorf("input net%d: net%d is empty; cards are numbered from net0 without a gap", i, len(nics))
		}
		nics = append(nics, v)
	}
	for i, n := range nics {
		if KVField(n, "bridge") == "" {
			return nil, nil, fmt.Errorf("input net%d: %q names no bridge", i, n)
		}
	}
	addrs := make([]netip.Prefix, len(nics))
	static := false
	for i := 0; i <= 3; i++ {
		key := "ip_address"
		if i > 0 {
			key = fmt.Sprintf("ip_address%d", i)
		}
		v := str(key, "")
		if v == "" {
			continue
		}
		if i >= len(nics) {
			return nil, nil, fmt.Errorf("input %s: the machine has no card net%d to give it", key, i)
		}
		p, err := netip.ParsePrefix(v)
		if err != nil || !p.Addr().Is4() || p.Bits() == 0 {
			return nil, nil, fmt.Errorf("input %s: %q is not an IPv4 address with its prefix, like 10.20.0.10/24", key, v)
		}
		addrs[i], static = p, true
	}
	var gateway netip.Addr
	if v := str("gateway", ""); v != "" {
		g, err := netip.ParseAddr(v)
		if err != nil || !g.Is4() {
			return nil, nil, fmt.Errorf("input gateway: %q is not an IPv4 address", v)
		}
		if !addrs[0].IsValid() {
			return nil, nil, fmt.Errorf("input gateway: it is the first card's route, and that card has no fixed address (ip_address)")
		}
		if !addrs[0].Masked().Contains(g) {
			return nil, nil, fmt.Errorf("input gateway: %s is not in %s, the first card's network", g, addrs[0].Masked())
		}
		gateway = g
	}
	var dns []string
	if v := str("nameservers", ""); v != "" {
		if !static {
			return nil, nil, fmt.Errorf("input nameservers: they go with a fixed address; a card on DHCP takes the DHCP server's")
		}
		for _, part := range strings.Split(v, ",") {
			a, err := netip.ParseAddr(strings.TrimSpace(part))
			if err != nil {
				return nil, nil, fmt.Errorf("input nameservers: %q is not an address", strings.TrimSpace(part))
			}
			dns = append(dns, a.String())
		}
	}
	if len(nics) == 1 && !static {
		return nics, nil, nil
	}
	var b strings.Builder
	b.WriteString("version: 2\nethernets:\n")
	for i := range nics {
		fmt.Fprintf(&b, "  net%d:\n    match:\n      macaddress: \"%s\"\n", i, macPlaceholder(i))
		if !addrs[i].IsValid() {
			b.WriteString("    dhcp4: true\n")
			continue
		}
		fmt.Fprintf(&b, "    dhcp4: false\n    addresses:\n      - %s\n", addrs[i])
		if i == 0 && gateway.IsValid() {
			fmt.Fprintf(&b, "    routes:\n      - to: 0.0.0.0/0\n        via: %s\n", gateway)
		}
		if i == firstStatic(addrs) && len(dns) > 0 {
			b.WriteString("    nameservers:\n      addresses:\n")
			for _, d := range dns {
				fmt.Fprintf(&b, "        - %s\n", d)
			}
		}
	}
	return nics, []byte(b.String()), nil
}

func firstStatic(addrs []netip.Prefix) int {
	for i, a := range addrs {
		if a.IsValid() {
			return i
		}
	}
	return -1
}

// fillMACs puts the clone's MACs into a recipe's network config. A card the
// config names that the clone does not carry is an error: its addressing
// would match nothing, or the wrong card.
func fillMACs(network []byte, cfg guestConfig) ([]byte, error) {
	out := string(network)
	for i := 0; i <= 3; i++ {
		ph := macPlaceholder(i)
		if !strings.Contains(out, ph) {
			continue
		}
		mac := ""
		for _, part := range strings.Split(cfg.str(fmt.Sprintf("net%d", i)), ",") {
			if kv := strings.SplitN(part, "=", 2); len(kv) == 2 && strings.Count(kv[1], ":") == 5 {
				mac = strings.ToLower(kv[1])
				break
			}
		}
		if mac == "" {
			return nil, fmt.Errorf("proxmox: card net%d has no MAC on the clone, so its network config cannot name it", i)
		}
		out = strings.ReplaceAll(out, ph, mac)
	}
	return []byte(out), nil
}
