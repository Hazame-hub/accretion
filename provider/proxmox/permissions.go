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
// on the seed storage, VM.Config.CDROM), power (VM.PowerMgmt), the delete
// (VM.Allocate), and the plan's capacity read (Sys.Audit on the node). The new
// machine's VMID is not known before apply, so its grants are on /vms.
var (
	privsTemplate = []string{"VM.Audit", "VM.Clone"}
	privsMachines = []string{
		"VM.Allocate", "VM.Audit", "VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Disk",
		"VM.Config.HWType", "VM.Config.Memory", "VM.Config.Network", "VM.Config.Options", "VM.PowerMgmt",
	}
	privsDiskStorage = []string{"Datastore.AllocateSpace", "Datastore.Audit"}
	privsSeedStorage = []string{"Datastore.Allocate", "Datastore.AllocateTemplate", "Datastore.Audit"}
	privsNode        = []string{"Sys.Audit"}
	privsBridge      = []string{"SDN.Use"}
	privsGuestAgent  = []string{"VM.GuestAgent.Audit"}
)

// Permissions says what a token needs to apply and destroy the machines the
// recipes describe: one role, the paths to grant it on, and how. When s is a
// live session it also reads what the token itself may do on each path (GET
// /access/permissions, and the templates' configs to find their disks'
// storage) and says what is missing. It never writes.
func Permissions(ctx context.Context, s provider.Session, role string, recipes []provider.Recipe) provider.Permissions {
	var c *Client
	if ps, ok := s.(*session); ok {
		c = ps.c
	}
	out := provider.Permissions{Role: role}
	b := grantBuilder{byPath: map[string]*provider.Grant{}}

	type tmpl struct{ node, vmid string }
	templates := map[tmpl]bool{}
	for _, r := range recipes {
		node, vmid := r.Clone.Placement, r.Clone.Template.ID
		if n, v, ok := strings.Cut(vmid, "/qemu/"); ok {
			node, vmid = n, v
		}
		if !templates[tmpl{node, vmid}] {
			templates[tmpl{node, vmid}] = true
			b.add("/vms/"+vmid, privsTemplate, fmt.Sprintf("clone template %s", vmid), false)
		}
		b.add("/vms", privsMachines, "create, configure, start, stop and delete the new machines (their VMIDs are only known at apply)", false)
		b.add("/storage/"+r.SeedStorage, privsSeedStorage, "upload, attach and remove each machine's cloud-init seed ISO", false)
		b.add("/nodes/"+node, privsNode, "read the node's capacity for the plan", false)
		for _, nic := range r.Clone.Shape.NICs {
			if br := KVField(nic, "bridge"); br != "" {
				b.add("/sdn/zones/localnetwork/"+br, privsBridge, fmt.Sprintf("attach the network card to %s (Proxmox VE 8 and 9)", br), false)
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
			b.add("/storage", privsDiskStorage, why, false)
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
			b.add("/storage", privsDiskStorage, why, false)
			out.Notes = append(out.Notes, fmt.Sprintf("template %s's config could not be read on %s: %s; the disk grant is on /storage", t.vmid, t.node, reason))
			continue
		}
		for _, st := range stores {
			b.add("/storage/"+st, privsDiskStorage, why, false)
		}
	}
	if len(recipes) > 0 {
		b.add("/vms", privsGuestAgent, "optional: let plan show each machine's hostname and addresses from the guest agent (on Proxmox VE 8 the privilege is VM.Monitor)", true)
	}

	out.Grants = b.list()
	union := map[string]bool{}
	for _, g := range out.Grants {
		if g.Optional {
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
	for _, g := range p.Grants {
		if g.Optional {
			optional = append(optional, g.Privileges...)
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
	out = append(out,
		"A token with Privilege Separation on gets only what both it and its user have: grant the user the same (Add → User Permission), or use a token without separation.",
		fmt.Sprintf("Or on a node's shell: pveum role add %s --privs %q", p.Role, strings.Join(p.Privileges, ",")),
	)
	for _, path := range paths {
		out = append(out, fmt.Sprintf("  pveum acl modify %s --tokens '%s' --roles %s", path, token, p.Role))
	}
	if len(optional) > 0 {
		out = append(out, fmt.Sprintf("Optional: add %s to the role (VM.Monitor on Proxmox VE 8) so plan shows guest hostnames and addresses.", strings.Join(optional, ", ")))
	}
	return out
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
