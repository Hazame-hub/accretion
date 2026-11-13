package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/hazame-hub/accretion/provider"
)

// What accrete's Writer needs, and where Proxmox checks it.
//
// Each list below follows the calls the Writer and the plan make: a full
// clone of the template (VM.Clone on the template, VM.Allocate on the new
// VMID, Datastore.AllocateSpace where the template's disks are copied), the
// config POST that sets size, CPU, NIC and SMBIOS (VM.Config.*), the seed ISO
// upload, attach and removal (Datastore.AllocateTemplate and Datastore.Allocate
// on the seed storage, VM.Config.CDROM, and VM.Config.Cloudinit: Proxmox checks
// it when ide2 is written on a clone whose template carries a cloud-init
// drive, as rocky-9-cloud does; seen on the lab 2026-11-02), power (VM.PowerMgmt), the delete
// (VM.Allocate), and the plan's capacity read (Sys.Audit on the node). The new
// machine's VMID is not known before apply, so its grants are on /vms.
var (
	privsTemplate = []string{"VM.Audit", "VM.Clone"}
	privsMachines = []string{
		"VM.Allocate", "VM.Audit", "VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Cloudinit",
		"VM.Config.Disk", "VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options", "VM.PowerMgmt",
	}
	privsDiskStorage = []string{"Datastore.AllocateSpace", "Datastore.Audit"}
	privsSeedStorage = []string{"Datastore.Allocate", "Datastore.AllocateTemplate", "Datastore.Audit"}
	privsNode        = []string{"Sys.Audit"}
	privsBridge      = []string{"SDN.Use"}
	privsGuestAgent  = []string{"VM.GuestAgent.Audit"}
	privsSDNListing  = []string{"SDN.Audit"}
	// Creating a network: POST /cluster/sdn/zones, vnets and subnets, and
	// PUT /cluster/sdn, which Proxmox checks as SDN.Allocate on /sdn and its
	// zones; SDN.Audit for the reads before and after. Bringing the bridge up
	// is PUT /nodes/<node>/network, checked as Sys.Modify on the node.
	privsSDNCreate   = []string{"SDN.Allocate", "SDN.Audit"}
	privsNodeNetwork = []string{"Sys.Modify"}
)

// Permissions says what a token needs to apply and destroy the machines the
// recipes describe: one role, the paths to grant it on, and how. When s is a
// live session it also reads what the token itself may do on each path (GET
// /access/permissions, and the templates' configs to find their disks'
// storage) and says what is missing. It never writes.
//
// networks are the networks apply would create: their zone, network and
// subnet definitions, the cluster SDN apply and each node's network reload
// are added, and a card on one of them is granted on its zone's path.
func Permissions(ctx context.Context, s provider.Session, role string, recipes []provider.Recipe, networks []provider.NetworkSpec) provider.Permissions {
	var c *Client
	if ps, ok := s.(*session); ok {
		c = ps.c
	}
	out := provider.Permissions{Role: role}
	b := grantBuilder{byPath: map[string]*provider.Grant{}}

	// With a resource pool, the machines' privileges are granted on the pool
	// and every other path gets a role of its own holding only what is used
	// there: one role everywhere would let the token change or delete the
	// template, and delete any volume on the disk storage.
	pooled := false
	for _, r := range recipes {
		if r.Clone.Pool != "" {
			pooled = true
		}
	}
	req := func(path string, privs []string, why, suffix string) {
		if pooled && suffix != "" {
			b.addRole(path, privs, why, role+suffix)
			return
		}
		b.add(path, privs, why, false)
	}

	zones := map[string]string{}
	for _, n := range networks {
		zones[n.Name] = n.Zone
		req("/sdn", privsSDNCreate, fmt.Sprintf("create network %s (zone %s, network, subnet) and apply the cluster's SDN", n.Name, n.Zone), "Net")
		req("/nodes", privsNodeNetwork, fmt.Sprintf("reload each node's network so network %s's bridge comes up there", n.Name), "Node")
	}

	type tmpl struct{ node, vmid string }
	templates := map[tmpl]bool{}
	installs := false
	for _, r := range recipes {
		if r.Install != nil {
			installs = true
			addInstall(&b, r)
			continue
		}
		if r.Image != nil {
			addImage(&b, r)
			continue
		}
		node, vmid := r.Clone.Placement, r.Clone.Template.ID
		if n, v, ok := strings.Cut(vmid, "/qemu/"); ok {
			node, vmid = n, v
		}
		if !templates[tmpl{node, vmid}] {
			templates[tmpl{node, vmid}] = true
			req("/vms/"+vmid, privsTemplate, fmt.Sprintf("clone template %s", vmid), "Clone")
		}
		if r.Clone.Pool != "" {
			b.add(machinesPath(r.Clone.Pool), privsMachines, fmt.Sprintf("create, configure, start, stop and delete the new machines, which are cloned into pool %s", r.Clone.Pool), false)
		} else {
			b.add("/vms", privsMachines, "create, configure, start, stop and delete the new machines (their VMIDs are only known at apply)", false)
		}
		req("/storage/"+r.SeedStorage, privsSeedStorage, "upload, attach and remove each machine's cloud-init seed ISO", "Seed")
		req("/nodes/"+node, privsNode, "read the node's capacity for the plan", "Node")
		for _, nic := range r.Clone.Shape.NICs {
			br := KVField(nic, "bridge")
			switch zone, declared := zones[br]; {
			case br == "":
			case declared:
				req("/sdn/zones/"+zone+"/"+br, privsBridge, fmt.Sprintf("attach the network card to network %s", br), "Net")
			default:
				req("/sdn/zones/localnetwork/"+br, privsBridge, fmt.Sprintf("attach the network card to %s (Proxmox VE 8 and 9)", br), "Net")
			}
		}
	}
	if len(recipes) == 0 {
		out.Notes = append(out.Notes, "the manifest declares no machine accrete builds, so nothing is needed to apply it")
	}

	// Where each template's disks live is where the full clone copies them.
	tkeys := make([]tmpl, 0, len(templates))
	for t := range templates {
		tkeys = append(tkeys, t)
	}
	sort.Slice(tkeys, func(i, j int) bool { return tkeys[i].vmid < tkeys[j].vmid })
	for _, t := range tkeys {
		why := fmt.Sprintf("copy template %s's disks in the full clone", t.vmid)
		if c == nil {
			req("/storage", privsDiskStorage, why, "Disk")
			out.Notes = appendOnce(out.Notes, fmt.Sprintf("template %s's disk storage was not read (the provider was not asked), so the disk grant is on /storage; grant it on that storage alone once you know it", t.vmid))
			continue
		}
		stores, err := templateStorages(ctx, c, t.node, t.vmid)
		if err != nil || len(stores) == 0 {
			reason := "it lists no disk"
			if err != nil {
				reason = err.Error()
				if x := Explain(err); x != "" {
					reason += " (" + x + ")"
				}
			}
			req("/storage", privsDiskStorage, why, "Disk")
			out.Notes = append(out.Notes, fmt.Sprintf("template %s's config could not be read on %s: %s; the disk grant is on /storage", t.vmid, t.node, reason))
			continue
		}
		for _, st := range stores {
			req("/storage/"+st, privsDiskStorage, why, "Disk")
		}
	}
	if len(recipes) > 0 && !installs {
		for _, r := range recipes {
			b.add(machinesPath(r.Clone.Pool), privsGuestAgent, "optional: let plan show each machine's hostname and addresses from the guest agent (on Proxmox VE 8 the privilege is VM.Monitor)", true)
		}
		// Without it Proxmox lists the SDN as empty instead of refusing, so
		// every plan warns that its listing is incomplete (seen on the lab
		// 2026-11-02 with a token built from this list).
		b.add("/sdn", privsSDNListing, "optional: let plan list the SDN zones and networks; without it every plan warns that its listing is incomplete", true)
	}

	out.Grants = b.list()
	union := map[string]bool{}
	for _, g := range out.Grants {
		if g.Optional || (g.Role != "" && g.Role != role) {
			continue
		}
		for _, p := range g.Privileges {
			union[p] = true
		}
	}
	for p := range union {
		out.Privileges = append(out.Privileges, p)
	}
	sort.Strings(out.Privileges)

	if c != nil {
		for i := range out.Grants {
			g := &out.Grants[i]
			have, err := c.EffectivePrivileges(ctx, g.Path)
			if err != nil {
				g.NotChecked = err.Error()
				if x := Explain(err); x != "" {
					g.NotChecked += " (" + x + ")"
				}
				continue
			}
			g.Checked = true
			for _, p := range g.Privileges {
				if !have[p] {
					g.Missing = append(g.Missing, p)
				}
			}
		}
	} else {
		for i := range out.Grants {
			out.Grants[i].NotChecked = "the provider was not asked"
		}
	}
	out.Setup = setup(out)
	return out
}

// addInstall is what an install from an ISO needs (openspec
// accrete-installer-iso, task 6.1): no template, an empty disk made on the
// disk storage, the installer read and attached from its storage (Proxmox
// checks Datastore.Audit or AllocateSpace on an iso volume a VM uses), the
// answer disc uploaded, attached and removed, and the guest agent read,
// which is required here: the hostname it reports is what the install is
// accepted on.
func addInstall(b *grantBuilder, r provider.Recipe) {
	c := r.Install.Create
	b.add("/vms", privsMachines, "create, configure, start, stop and delete the new machines (their VMIDs are only known at apply)", false)
	b.add("/vms", privsGuestAgent, "read the hostname an installed machine's guest agent reports, which its install is accepted on (on Proxmox VE 8 the privilege is VM.Monitor)", false)
	b.add("/storage/"+c.DiskStorage, privsDiskStorage, "create each installed machine's empty disk", false)
	if m := isoVolume.FindStringSubmatch(c.Installer); m != nil {
		b.add("/storage/"+m[1], []string{"Datastore.Audit"}, "find the installer ISO and boot it", false)
	}
	b.add("/storage/"+r.SeedStorage, privsSeedStorage, "upload, attach and remove each installed machine's answer disc", false)
	b.add("/nodes/"+c.Placement, privsNode, "read the node's capacity for the plan", false)
	for _, nic := range c.Shape.NICs {
		if br := KVField(nic, "bridge"); br != "" {
			b.add("/sdn/zones/localnetwork/"+br, privsBridge, fmt.Sprintf("attach the network card to %s (Proxmox VE 8 and 9)", br), false)
		}
	}
}

// addImage is what a build from a disk image needs: no template, the image
// read from its storage's import content and copied to the disk storage
// (Proxmox accepts Datastore.Audit or AllocateSpace on an import volume a new
// disk is copied from), a serial port of type socket (VM.Config.HWType), and
// the seed uploaded, attached and removed. The guest agent is not needed:
// the first boot's power-off is what the build is accepted on.
func addImage(b *grantBuilder, r provider.Recipe) {
	c := r.Image.Create
	b.add("/vms", privsMachines, "create, configure, start, stop and delete the new machines (their VMIDs are only known at apply)", false)
	b.add("/storage/"+c.DiskStorage, privsDiskStorage, "copy each machine's disk from the image and grow it", false)
	if m := importVolume.FindStringSubmatch(c.Image); m != nil {
		b.add("/storage/"+m[1], []string{"Datastore.Audit"}, "find the disk image and copy it", false)
		if r.Image.URL != "" {
			b.add("/storage/"+m[1], []string{"Datastore.AllocateTemplate"}, "upload the disk image accrete fetches when the storage does not have it yet", false)
		}
	}
	b.add("/storage/"+r.SeedStorage, privsSeedStorage, "upload, attach and remove each machine's seed disc", false)
	b.add("/nodes/"+c.Placement, privsNode, "read the node's capacity for the plan", false)
	for _, nic := range c.Shape.NICs {
		if br := KVField(nic, "bridge"); br != "" {
			b.add("/sdn/zones/localnetwork/"+br, privsBridge, fmt.Sprintf("attach the network card to %s (Proxmox VE 8 and 9)", br), false)
		}
	}
}

// templateStorages are the storages a template's disks and cloud-init drive
// are on, sorted. An ISO in a CD drive is not copied by a clone and is left
// out.
func templateStorages(ctx context.Context, c *Client, node, vmid string) ([]string, error) {
	var cfg map[string]json.RawMessage
	if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/config", url.PathEscape(node), url.PathEscape(vmid)), &cfg); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for key := range cfg {
		if !IsDiskKey(key) {
			continue
		}
		v := ConfigString(cfg, key)
		vol, _, _ := strings.Cut(v, ",")
		if vol == "" || vol == "none" {
			continue
		}
		if KVField(v, "media") == "cdrom" && !strings.Contains(vol, "cloudinit") {
			continue
		}
		st, _, ok := strings.Cut(vol, ":")
		if !ok || st == "" {
			continue
		}
		seen[st] = true
	}
	out := make([]string, 0, len(seen))
	for st := range seen {
		out = append(out, st)
	}
	sort.Strings(out)
	return out, nil
}

// setup is how to make the role and grant it: the Proxmox UI first, then the
// same as pveum commands for a node's shell.
func setup(p provider.Permissions) []string {
	if len(p.Privileges) == 0 {
		return nil
	}
	const token = "<user@realm!tokenid>"
	var paths []string
	seen := map[string]bool{}
	var optional []string
	var own []provider.Grant
	for _, g := range p.Grants {
		if g.Role != "" && g.Role != p.Role {
			own = append(own, g)
			continue
		}
		if g.Optional {
			optional = append(optional, fmt.Sprintf("Optional: grant %s on %s (%s).", strings.Join(g.Privileges, ", "), g.Path, strings.TrimPrefix(g.Why, "optional: ")))
			continue
		}
		if !seen[g.Path] {
			seen[g.Path] = true
			paths = append(paths, g.Path)
		}
	}
	// One role holds every privilege, granted with propagation, so a path
	// under another listed path is already reached: /vms covers /vms/9000.
	kept := paths[:0]
	for _, path := range paths {
		covered := false
		for _, parent := range paths {
			if parent != path && strings.HasPrefix(path, parent+"/") {
				covered = true
			}
		}
		if !covered {
			kept = append(kept, path)
		}
	}
	paths = kept
	out := []string{
		fmt.Sprintf("Proxmox UI: Datacenter → Permissions → Roles → Create. Name %s, privileges: %s.", p.Role, strings.Join(p.Privileges, ", ")),
	}
	for _, path := range paths {
		out = append(out, fmt.Sprintf("Proxmox UI: Datacenter → Permissions → Add → API Token Permission. Path %s, API token %s, role %s, Propagate on.", path, token, p.Role))
	}
	// Each role of its own is created once, with what all its grants use.
	var ownRoles []string
	ownPrivs := map[string][]string{}
	for _, g := range own {
		if _, ok := ownPrivs[g.Role]; !ok {
			ownRoles = append(ownRoles, g.Role)
		}
		for _, priv := range g.Privileges {
			if !containsStr(ownPrivs[g.Role], priv) {
				ownPrivs[g.Role] = append(ownPrivs[g.Role], priv)
			}
		}
	}
	for _, r := range ownRoles {
		sort.Strings(ownPrivs[r])
		out = append(out, fmt.Sprintf("Proxmox UI: Datacenter → Permissions → Roles → Create. Name %s, privileges: %s.", r, strings.Join(ownPrivs[r], ", ")))
	}
	// A template's grant does not propagate: nothing under /vms/<id> needs it.
	propagate := func(path string) bool { return !strings.HasPrefix(path, "/vms/") }
	onOff := map[bool]string{true: "on", false: "off"}
	for _, g := range own {
		out = append(out, fmt.Sprintf("Proxmox UI: Datacenter → Permissions → Add → API Token Permission. Path %s, API token %s, role %s, Propagate %s.", g.Path, token, g.Role, onOff[propagate(g.Path)]))
	}
	out = append(out,
		"A token with Privilege Separation on gets only what both it and its user have: grant the user the same (Add → User Permission), or use a token without separation.",
		fmt.Sprintf("Or on a node's shell: pveum role add %s --privs %q", p.Role, strings.Join(p.Privileges, ",")),
	)
	for _, path := range paths {
		out = append(out, fmt.Sprintf("  pveum acl modify %s --tokens '%s' --roles %s", path, token, p.Role))
	}
	for _, r := range ownRoles {
		out = append(out, fmt.Sprintf("  pveum role add %s --privs %q", r, strings.Join(ownPrivs[r], ",")))
	}
	for _, g := range own {
		line := fmt.Sprintf("  pveum acl modify %s --tokens '%s' --roles %s", g.Path, token, g.Role)
		if !propagate(g.Path) {
			line += " --propagate 0"
		}
		out = append(out, line)
	}
	out = append(out, optional...)
	return out
}

// machinesPath is where the new machines' grants go: their resource pool,
// whose rights reach every guest in it, or /vms when they join none.
func machinesPath(pool string) string {
	if pool != "" {
		return "/pool/" + pool
	}
	return "/vms"
}

type grantBuilder struct {
	order  []string
	byPath map[string]*provider.Grant
}

// add merges privileges into the grant for path (and optional-ness, which is
// kept apart: an optional privilege never shares a grant with required ones).
func (b *grantBuilder) add(path string, privs []string, why string, optional bool) {
	key := path
	if optional {
		key = "?" + path
	}
	g, ok := b.byPath[key]
	if !ok {
		g = &provider.Grant{Path: path, Optional: optional}
		b.byPath[key] = g
		b.order = append(b.order, key)
	}
	for _, p := range privs {
		if !containsStr(g.Privileges, p) {
			g.Privileges = append(g.Privileges, p)
		}
	}
	sort.Strings(g.Privileges)
	if !strings.Contains(g.Why, why) {
		if g.Why != "" {
			g.Why += "; "
		}
		g.Why += why
	}
}

// addRole adds a required grant of its own role at path, kept apart from the
// grants of the main role there.
func (b *grantBuilder) addRole(path string, privs []string, why, role string) {
	key := "@" + role + path
	if _, ok := b.byPath[key]; !ok {
		b.byPath[key] = &provider.Grant{Path: path, Role: role, Why: why}
		b.order = append(b.order, key)
	}
	g := b.byPath[key]
	for _, p := range privs {
		if !containsStr(g.Privileges, p) {
			g.Privileges = append(g.Privileges, p)
		}
	}
	sort.Strings(g.Privileges)
}

func (b *grantBuilder) list() []provider.Grant {
	out := make([]provider.Grant, 0, len(b.order))
	for _, k := range b.order {
		out = append(out, *b.byPath[k])
	}
	return out
}

func containsStr(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

func appendOnce(list []string, s string) []string {
	if containsStr(list, s) {
		return list
	}
	return append(list, s)
}
