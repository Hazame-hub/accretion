package proxmox

// Discovery: what the cluster holds, with each guest's identity.
//
// The walk is /nodes, then each node's qemu and lxc lists, then per guest the
// config (where the SMBIOS UUID, the MACs, the sizing and the template flag
// live) and status/current (where the current memory allocation lives). A
// node that does not answer costs that node's guests and nothing else: the
// walk returns what it got together with an error naming the node, which a
// caller treats as an incomplete list, never as proof of absence.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hazame-hub/accretion/provider"
)

// Provider is the discovery half of Proxmox: what the cluster holds, with
// each guest's identity.
type Provider struct{}

// New returns the Proxmox provider. Stateless: every Connect builds its own
// client from the resolved Config.
func New() Provider { return Provider{} }

// Connect implements provider.Provider. It does not dial: the first Discover
// finds out whether the cluster answers. A preflight, where an embedder has
// one, is the place that proves a credential before it is stored.
func (Provider) Connect(_ context.Context, cfg provider.Config) (provider.Session, error) {
	token, _ := cfg.Params["api_token"].(string)
	c, err := NewClient(cfg.Endpoint, token, cfg.VerifyTLS)
	if err != nil {
		return nil, err
	}
	return NewSession(c), nil
}

// NewSession is a discovery session over a client the caller already has.
func NewSession(c *Client) provider.Session { return &session{c: c} }

type session struct {
	c *Client
}

// capabilities is what a Proxmox session can say. GuestUUID holds for qemu
// guests only — a container has no DMI — which is why the contract checks it
// per resource and why LXC identity falls to the MAC tier.
var capabilities = provider.Capabilities{
	Kinds:        []provider.Kind{provider.KindHost, provider.KindVM, provider.KindContainer, provider.KindTemplate},
	GuestUUID:    true,
	GuestMACs:    true,
	GuestNetwork: true,
	Hierarchy:    true,
	Tags:         true,
	PowerState:   true,
	Templates:    true,
	Describe:     true,
	Memory:       true,
}

func (s *session) Capabilities(context.Context) (provider.Capabilities, error) {
	return capabilities, nil
}

func (s *session) Close() error { return nil }

// nodeRow is one row of GET /nodes.
type nodeRow struct {
	Node   string `json:"node"`
	Status string `json:"status"`
}

// guestRow is one row of GET /nodes/{node}/qemu or /lxc. The two lists share
// these fields; anything else is read from the guest's config.
type guestRow struct {
	VMID     int     `json:"vmid"`
	Name     string  `json:"name"`
	Status   string  `json:"status"`
	Template int     `json:"template"`
	Tags     string  `json:"tags"`
	MaxMem   int64   `json:"maxmem"`
	MaxDisk  int64   `json:"maxdisk"`
	CPUs     float64 `json:"cpus"`
}

// statusRow is GET /nodes/{node}/{type}/{vmid}/status/current, reduced to
// what memory reconciliation needs.
type statusRow struct {
	Status  string `json:"status"`
	MaxMem  int64  `json:"maxmem"`
	Balloon int64  `json:"balloon"`
}

func (s *session) Discover(ctx context.Context) ([]provider.Resource, error) {
	var nodes []nodeRow
	if err := s.c.Get(ctx, "/nodes", &nodes); err != nil {
		return nil, fmt.Errorf("proxmox: list nodes: %w", err)
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].Node < nodes[j].Node })

	var out []provider.Resource
	var errs []error
	for _, n := range nodes {
		raw, _ := json.Marshal(n)
		out = append(out, provider.Resource{
			Ref:      provider.Ref{Kind: provider.KindHost, ID: n.Node},
			Name:     n.Node,
			Tags:     []string{},
			State:    nodeState(n.Status),
			Identity: provider.Identity{MACs: []string{}, IPs: []string{}},
			Facts:    provider.Facts{Disks: []provider.Disk{}, NetworkInterfaces: []provider.Interface{}},
			Raw:      raw,
		})
		for _, kind := range []string{"qemu", "lxc"} {
			var guests []guestRow
			if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/%s", n.Node, kind), &guests); err != nil {
				errs = append(errs, fmt.Errorf("proxmox: node %s: list %s: %w", n.Node, kind, err))
				continue
			}
			for _, g := range guests {
				res, err := s.describeGuest(ctx, n.Node, kind, g)
				if err != nil {
					errs = append(errs, err)
				}
				out = append(out, res)
			}
		}
	}
	return out, errors.Join(errs...)
}

// Describe re-reads one guest. A host ref describes as empty facts: the
// walk already said everything /nodes knows about a node.
func (s *session) Describe(ctx context.Context, ref provider.Ref) (provider.Facts, error) {
	if ref.Kind == provider.KindHost {
		var nodes []nodeRow
		if err := s.c.Get(ctx, "/nodes", &nodes); err != nil {
			return provider.Facts{}, err
		}
		for _, n := range nodes {
			if n.Node == ref.ID {
				return provider.Facts{Disks: []provider.Disk{}, NetworkInterfaces: []provider.Interface{}}, nil
			}
		}
		return provider.Facts{}, fmt.Errorf("proxmox: no node %q", ref.ID)
	}
	node, kind, vmid, err := splitRef(ref.ID)
	if err != nil {
		return provider.Facts{}, err
	}
	var guests []guestRow
	if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/%s", node, kind), &guests); err != nil {
		return provider.Facts{}, fmt.Errorf("proxmox: node %s: list %s: %w", node, kind, err)
	}
	for _, g := range guests {
		if strconv.Itoa(g.VMID) == vmid {
			res, err := s.describeGuest(ctx, node, kind, g)
			if err != nil {
				return provider.Facts{}, err
			}
			return res.Facts, nil
		}
	}
	return provider.Facts{}, fmt.Errorf("proxmox: no %s %s on node %s", kind, vmid, node)
}

// describeGuest turns a list row plus its config and status into a Resource.
// A config or status that does not answer is an error returned *with* the
// resource, so the caller keeps the row and knows it is thinner than it
// should be.
func (s *session) describeGuest(ctx context.Context, node, kind string, g guestRow) (provider.Resource, error) {
	id := fmt.Sprintf("%s/%s/%d", node, kind, g.VMID)
	name := g.Name
	if name == "" {
		name = fmt.Sprintf("%s-%d", map[string]string{"qemu": "vm", "lxc": "ct"}[kind], g.VMID)
	}
	res := provider.Resource{
		Ref:    provider.Ref{Kind: guestKind(kind, g.Template), ID: id},
		Name:   name,
		Parent: &provider.Ref{Kind: provider.KindHost, ID: node},
		Tags:   splitTags(g.Tags),
		State:  guestState(g.Status),
		Identity: provider.Identity{
			MACs:              []string{},
			IPs:               []string{},
			HypervisorGuestID: strconv.Itoa(g.VMID),
		},
		Facts: provider.Facts{
			CPUCores:          int32(g.CPUs),
			RAMBytes:          g.MaxMem,
			Disks:             []provider.Disk{},
			NetworkInterfaces: []provider.Interface{},
		},
		Memory: provider.MemoryAllocation{MaxBytes: g.MaxMem},
	}

	var errs []error
	var config map[string]json.RawMessage
	if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/%s/%d/config", node, kind, g.VMID), &config); err != nil {
		errs = append(errs, fmt.Errorf("proxmox: %s: config: %w", id, err))
	} else {
		applyConfig(&res, kind, config)
	}

	var st statusRow
	if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/%s/%d/status/current", node, kind, g.VMID), &st); err != nil {
		errs = append(errs, fmt.Errorf("proxmox: %s: status: %w", id, err))
	} else {
		if st.MaxMem > 0 {
			res.Memory.MaxBytes = st.MaxMem
			res.Facts.RAMBytes = st.MaxMem
		}
		if res.Memory.Balloon && st.Balloon > 0 {
			res.Memory.CurrentBytes = st.Balloon
		} else {
			res.Memory.CurrentBytes = res.Memory.MaxBytes
		}
		if st.Status != "" {
			res.State = guestState(st.Status)
		}
	}

	// The guest agent, when one runs, is the only way a hypervisor learns the
	// hostname and addresses the guest itself uses. Its absence is the normal
	// case for a container or a VM without qemu-guest-agent, not an error.
	if kind == "qemu" && res.State == "running" {
		s.applyGuestAgent(ctx, node, g.VMID, &res)
	}

	raw, _ := json.Marshal(map[string]any{"resource": g, "config": config, "status": st})
	res.Raw = raw
	return res, errors.Join(errs...)
}

// applyConfig reads what the guest's config says about identity and sizing.
func applyConfig(res *provider.Resource, kind string, config map[string]json.RawMessage) {
	str := func(key string) string {
		var s string
		if raw, ok := config[key]; ok {
			if json.Unmarshal(raw, &s) != nil {
				var n json.Number
				if json.Unmarshal(raw, &n) == nil {
					s = n.String()
				}
			}
		}
		return s
	}
	num := func(key string) int64 {
		n, _ := strconv.ParseInt(str(key), 10, 64)
		return n
	}

	if kind == "qemu" {
		if uuid := KVField(str("smbios1"), "uuid"); uuid != "" {
			if forms := provider.UUIDForms(uuid); forms != nil {
				res.Identity.SMBIOSUUID = forms[0]
			}
		}
		cores, sockets := num("cores"), num("sockets")
		if cores > 0 {
			if sockets == 0 {
				sockets = 1
			}
			res.Facts.CPUCores = int32(cores * sockets)
		}
		// balloon=0 disables the device. Absent means enabled with the
		// target equal to memory, which is PVE's default.
		res.Memory.Balloon = !(config["balloon"] != nil && num("balloon") == 0)
	} else {
		if cores := num("cores"); cores > 0 {
			res.Facts.CPUCores = int32(cores)
		}
		res.Memory.Balloon = false
		if h := str("hostname"); h != "" {
			res.Identity.Hostname = h
		}
	}
	if mem := num("memory"); mem > 0 {
		res.Facts.RAMBytes = mem << 20
		res.Memory.MaxBytes = mem << 20
	}
	res.Facts.OS = str("ostype")

	// Interfaces: net0..netN. qemu writes "<model>=<mac>,bridge=..."; lxc
	// writes "name=eth0,hwaddr=<mac>,...".
	var macs []string
	keys := make([]string, 0, len(config))
	for k := range config {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		switch {
		case strings.HasPrefix(k, "net") && isDigits(k[3:]):
			v := str(k)
			mac := KVField(v, "hwaddr")
			ifName := KVField(v, "name")
			if mac == "" {
				for _, part := range strings.Split(v, ",") {
					if kv := strings.SplitN(part, "=", 2); len(kv) == 2 && strings.Count(kv[1], ":") == 5 {
						mac = kv[1]
						break
					}
				}
			}
			if ifName == "" {
				ifName = k
			}
			mac = strings.ToLower(mac)
			if mac != "" {
				macs = append(macs, mac)
			}
			res.Facts.NetworkInterfaces = append(res.Facts.NetworkInterfaces, provider.Interface{
				Name: ifName, Addresses: []string{}, HardwareAddr: mac,
			})
		case IsDiskKey(k):
			// A CD-ROM is not storage the guest has: an installer ISO on ide2
			// would otherwise count toward the machine's disk, and a 117 GiB
			// disk with a 4 GiB ISO attached would pass a 120 GiB request.
			if KVField(str(k), "media") == "cdrom" {
				continue
			}
			if size := ParseSize(KVField(str(k), "size")); size > 0 {
				res.Facts.Disks = append(res.Facts.Disks, provider.Disk{Mount: k, Total: size})
			}
		}
	}
	res.Identity.MACs = provider.NormalizeMACs(macs)
	if t := str("tags"); t != "" && len(res.Tags) == 0 {
		res.Tags = splitTags(t)
	}
}

// applyGuestAgent asks qemu-guest-agent for the hostname and addresses. Any
// failure leaves the resource as it was: no agent is the common case.
func (s *session) applyGuestAgent(ctx context.Context, node string, vmid int, res *provider.Resource) {
	var host struct {
		Result struct {
			HostName string `json:"host-name"`
		} `json:"result"`
	}
	if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/agent/get-host-name", node, vmid), &host); err == nil && host.Result.HostName != "" {
		res.Identity.Hostname = host.Result.HostName
	}
	var ifaces struct {
		Result []struct {
			Name string `json:"name"`
			MAC  string `json:"hardware-address"`
			IPs  []struct {
				Address string `json:"ip-address"`
				Type    string `json:"ip-address-type"`
			} `json:"ip-addresses"`
		} `json:"result"`
	}
	if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%d/agent/network-get-interfaces", node, vmid), &ifaces); err != nil {
		return
	}
	byMAC := map[string][]string{}
	for _, i := range ifaces.Result {
		mac := strings.ToLower(i.MAC)
		for _, ip := range i.IPs {
			if ip.Type != "ipv4" || strings.HasPrefix(ip.Address, "127.") {
				continue
			}
			byMAC[mac] = append(byMAC[mac], ip.Address)
			res.Identity.IPs = append(res.Identity.IPs, ip.Address)
		}
	}
	for i := range res.Facts.NetworkInterfaces {
		if addrs, ok := byMAC[res.Facts.NetworkInterfaces[i].HardwareAddr]; ok {
			res.Facts.NetworkInterfaces[i].Addresses = addrs
		}
	}
}

func guestKind(kind string, template int) provider.Kind {
	switch {
	case template == 1:
		return provider.KindTemplate
	case kind == "lxc":
		return provider.KindContainer
	default:
		return provider.KindVM
	}
}

func guestState(status string) string {
	switch status {
	case "running":
		return "running"
	case "stopped":
		return "stopped"
	case "paused", "suspended":
		return "suspended"
	default:
		return "unknown"
	}
}

func nodeState(status string) string {
	switch status {
	case "online":
		return "running"
	case "offline":
		return "stopped"
	default:
		return "unknown"
	}
}

// splitRef parses "node/qemu/104", the handle the adapter stores as
// external_id and netpolicy already parses the same way.
func splitRef(id string) (node, kind, vmid string, err error) {
	parts := strings.Split(id, "/")
	if len(parts) != 3 || parts[0] == "" || parts[2] == "" || (parts[1] != "qemu" && parts[1] != "lxc") {
		return "", "", "", fmt.Errorf("proxmox: %q is not a node/type/vmid ref", id)
	}
	return parts[0], parts[1], parts[2], nil
}

func splitTags(s string) []string {
	out := []string{}
	for _, t := range strings.FieldsFunc(s, func(r rune) bool { return r == ';' || r == ',' }) {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

// IsDiskKey reports whether a guest config key names a disk slot (scsi0,
// virtio1, rootfs, mp0, ...).
func IsDiskKey(k string) bool {
	for _, p := range []string{"scsi", "virtio", "sata", "ide", "rootfs", "mp"} {
		if strings.HasPrefix(k, p) && (k == p || isDigits(k[len(p):])) {
			return true
		}
	}
	return false
}
