package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hazame-hub/accretion/provider"
)

// The Proxmox Writer: provider.Writer on a session, built on the guards
// (guards.go) and on the clone, power, delete and seed code Continuum's
// actions run (write_core.go). What the verbs add is the contract: a Target
// whose identity is checked before anything is sent, typed refusals, and
// "the answer was lost" told apart from "it failed".

// writeCapabilities is what a Proxmox session's Writer can do.
var writeCapabilities = provider.WriteCapabilities{
	Clone: true, GuestInit: true, Power: true, Delete: true,
	MintUUID: true, OwnerMark: true, GracefulShutdown: true, GrowDisk: true,
}

// cpuModel is a CPU model name and nothing after it, as vm_set_config
// accepts it.
var cpuModel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// unknownAfter wraps an error from a call that may have been carried out: a
// task that outlived its wait, or a request whose answer never came (a
// transport error). Anything else is a known outcome (an API refusal, a task
// that ran and failed, a guard that said no) and is returned as is.
func unknownAfter(op string, err error) error {
	if err == nil {
		return nil
	}
	var timeout *TaskTimeoutError
	var transport *url.Error
	if (errors.As(err, &timeout) || errors.As(err, &transport)) &&
		!errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		return &provider.OutcomeUnknownError{Op: op, Err: err}
	}
	return err
}

// guestRef splits a guest ref "node/qemu/vmid". Only qemu guests can be
// written: a container has no SMBIOS identity to guard on.
func guestRef(ref provider.Ref) (node, vmid string, err error) {
	parts := strings.Split(ref.ID, "/")
	if len(parts) != 3 || !nodeName.MatchString(parts[0]) || !vmidValue.MatchString(parts[2]) {
		return "", "", provider.Refused("proxmox: ref %q is not node/qemu/vmid", ref.ID)
	}
	if parts[1] != "qemu" {
		return "", "", provider.Refused("proxmox: ref %q is a %s guest; only qemu guests carry an SMBIOS identity to guard on", ref.ID, parts[1])
	}
	return parts[0], parts[2], nil
}

func refOf(node, vmid string) provider.Ref {
	return provider.Ref{Kind: provider.KindVM, ID: node + "/qemu/" + vmid}
}

// guestConfig is the part of a guest's config the verbs read.
type guestConfig struct {
	raw map[string]json.RawMessage
}

func (g guestConfig) str(key string) string { return ConfigString(g.raw, key) }
func (g guestConfig) uuid() string          { return KVField(g.str("smbios1"), "uuid") }

func (c *Client) readConfig(ctx context.Context, node, vmid string) (guestConfig, error) {
	var raw map[string]json.RawMessage
	if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, vmid), &raw); err != nil {
		return guestConfig{}, err
	}
	return guestConfig{raw: raw}, nil
}

// awaitUnlocked reads a guest's config until no task holds it (a clone still
// copying its disk locks the new guest's config), within ctx.
func (c *Client) awaitUnlocked(ctx context.Context, node, vmid string) (guestConfig, error) {
	for {
		cfg, err := c.readConfig(ctx, node, vmid)
		if err != nil {
			return guestConfig{}, err
		}
		if cfg.str("lock") == "" {
			return cfg, nil
		}
		if !SleepCtx(ctx, c.TaskPoll) {
			return guestConfig{}, fmt.Errorf("proxmox: %s/qemu/%s is still locked (%s): %w", node, vmid, cfg.str("lock"), ctx.Err())
		}
	}
}

// guard proves a Target is the machine meant, reading the cluster listing and
// the guest's config, and sending nothing. absent is true when no guest has
// the VMID.
func (s *session) guard(ctx context.Context, m provider.Target) (node, vmid string, cfg guestConfig, absent bool, err error) {
	want := provider.UUIDForms(m.ExpectUUID)
	if want == nil {
		return "", "", guestConfig{}, false, provider.Refused(
			"proxmox: %s: expected UUID %q is not a usable UUID; refusing rather than acting unguarded", m.Ref.ID, m.ExpectUUID)
	}
	if node, vmid, err = guestRef(m.Ref); err != nil {
		return "", "", guestConfig{}, false, err
	}
	g, err := s.c.FindGuest(ctx, vmid)
	if err != nil {
		return "", "", guestConfig{}, false, fmt.Errorf("proxmox: look up %s: %w", m.Ref.ID, err)
	}
	if g == nil {
		return node, vmid, guestConfig{}, true, nil
	}
	if g.Node != node || g.Type != "qemu" {
		return "", "", guestConfig{}, false, provider.Refused(
			"proxmox: vmid %s is a %s on %s, not a qemu guest on %s; refusing", vmid, g.Type, g.Node, node)
	}
	if cfg, err = s.c.readConfig(ctx, node, vmid); err != nil {
		return "", "", guestConfig{}, false, fmt.Errorf("proxmox: read %s to confirm it is the guest meant: %w", m.Ref.ID, err)
	}
	if got := cfg.uuid(); !SameUUID(got, want) {
		if got == "" {
			got = "none"
		}
		return "", "", guestConfig{}, false, provider.Refused(
			"proxmox: vmid %s carries SMBIOS UUID %s, not %s; refusing to act on a guest this call was not about", vmid, got, want[0])
	}
	if m.Name != "" && cfg.str("name") != m.Name {
		return "", "", guestConfig{}, false, provider.Refused(
			"proxmox: vmid %s is %q, not %q; refusing", vmid, cfg.str("name"), m.Name)
	}
	if m.OwnerMark != "" && strings.TrimSpace(cfg.str("description")) != m.OwnerMark {
		return "", "", guestConfig{}, false, provider.Refused(
			"proxmox: vmid %s does not carry this owner mark; refusing", vmid)
	}
	return node, vmid, cfg, false, nil
}

// Clone implements provider.Writer. See openspec accretion-apply, design D3.
func (s *session) Clone(ctx context.Context, req provider.CloneRequest) (provider.Machine, error) {
	if provider.UUIDForms(req.MintUUID) == nil {
		return provider.Machine{}, provider.Refused("proxmox: clone %q: mint UUID %q is not a usable UUID", req.Name, req.MintUUID)
	}
	if req.Name == "" || strings.TrimSpace(req.OwnerMark) == "" {
		return provider.Machine{}, provider.Refused("proxmox: a clone needs a name and an owner mark, so a retry can find its own work")
	}
	if req.Shape.CPU != "" && !cpuModel.MatchString(req.Shape.CPU) {
		return provider.Machine{}, provider.Refused("proxmox: cpu %q is not a model name", req.Shape.CPU)
	}
	if len(req.Shape.NICs) > 4 {
		return provider.Machine{}, provider.Refused("proxmox: %d network cards; a machine has at most 4", len(req.Shape.NICs))
	}
	tnode, tvmid, err := templateRef(req.Template, req.Placement)
	if err != nil {
		return provider.Machine{}, err
	}
	node := req.Placement
	if node == "" {
		node = tnode
	}
	tcfg, err := s.c.readConfig(ctx, tnode, tvmid)
	if err != nil {
		return provider.Machine{}, fmt.Errorf("proxmox: read template %s/qemu/%s: %w", tnode, tvmid, err)
	}

	vmid, cloned := req.ID, false
	if vmid == "" {
		foundNode, found, err := s.findByMark(ctx, req.Name, req.OwnerMark)
		if err != nil {
			return provider.Machine{}, err
		}
		if found != "" {
			node, vmid = foundNode, found
		}
	}
	if vmid == "" {
		// Allocate. /cluster/nextid does not reserve: a guest made between
		// the read and the clone takes the id, and the next one is tried.
		for attempt := 0; ; attempt++ {
			var next string
			if err := s.c.Get(ctx, "/cluster/nextid", &next); err != nil {
				return provider.Machine{}, fmt.Errorf("proxmox: allocate a vmid: %w", err)
			}
			_, _, err := s.c.CloneGuest(ctx, s.cloneParams(req, tnode, tvmid, node, next))
			if err == nil {
				vmid, cloned = next, true
				break
			}
			if g, lookErr := s.c.FindGuest(ctx, next); attempt < 2 && lookErr == nil && g != nil && g.Name != req.Name {
				continue // taken by somebody else in the meantime
			}
			return provider.Machine{}, unknownAfter("proxmox: clone "+req.Name, err)
		}
	} else {
		// A given id, or the one found by mark: the clone marker says
		// whether the guest there is this clone's.
		done, err := s.c.CheckCloneTarget(ctx, node, vmid, req.Name, req.OwnerMark)
		if err != nil {
			return provider.Machine{}, provider.Refused("%s", err.Error())
		}
		if !done {
			if _, _, err := s.c.CloneGuest(ctx, s.cloneParams(req, tnode, tvmid, node, vmid)); err != nil {
				return provider.Machine{}, unknownAfter("proxmox: clone "+req.Name, err)
			}
			cloned = true
		}
	}

	cfg, err := s.c.awaitUnlocked(ctx, node, vmid)
	if err != nil {
		return provider.Machine{}, unknownAfter("proxmox: clone "+req.Name, err)
	}
	// The identity: written now if the guest still has the template's, or
	// none (a clone whose identity write was lost); left alone if it is
	// already the minted one; anything else was changed by somebody else.
	got := cfg.uuid()
	stamp := false
	switch {
	case SameUUID(got, provider.UUIDForms(req.MintUUID)):
	case got == "" || SameUUID(got, provider.UUIDForms(tcfg.uuid())):
		stamp = true
	default:
		return provider.Machine{}, provider.Refused(
			"proxmox: vmid %s (%s) carries SMBIOS UUID %s, neither the minted %s nor its template's; somebody changed it, refusing",
			vmid, req.Name, got, req.MintUUID)
	}
	form := url.Values{}
	if stamp {
		form.Set("smbios1", "uuid="+strings.ToLower(req.MintUUID))
	}
	set := func(key, want string) {
		if want != "" && cfg.str(key) != want {
			form.Set(key, want)
		}
	}
	if req.Shape.Cores > 0 {
		set("cores", strconv.Itoa(req.Shape.Cores))
	}
	if req.Shape.MemoryMB > 0 {
		set("memory", strconv.Itoa(req.Shape.MemoryMB))
	}
	set("cpu", req.Shape.CPU)
	for i, nic := range req.Shape.NICs {
		if nic != "" && !sameNIC(cfg.str(fmt.Sprintf("net%d", i)), nic) {
			form.Set(fmt.Sprintf("net%d", i), nic)
		}
	}
	shaped := len(form) > 0
	if shaped {
		data, err := s.c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, vmid), form)
		if err == nil {
			err = s.c.WaitIfTask(ctx, node, data, 2*time.Minute)
		}
		if err != nil {
			return provider.Machine{}, unknownAfter("proxmox: shape "+req.Name, err)
		}
	}
	grown, err := s.growDisk(ctx, node, vmid, cfg, req.Shape.DiskGB)
	if err != nil {
		return provider.Machine{}, unknownAfter("proxmox: grow the disk of "+req.Name, err)
	}
	return provider.Machine{
		Ref:      refOf(node, vmid),
		Identity: provider.Identity{SMBIOSUUID: strings.ToLower(req.MintUUID), HypervisorGuestID: vmid, MACs: []string{}, IPs: []string{}},
		Applied:  cloned || shaped || grown,
	}, nil
}

func (s *session) cloneParams(req provider.CloneRequest, tnode, tvmid, node, vmid string) map[string]interface{} {
	p := map[string]interface{}{
		"node": tnode, "template_vmid": tvmid, "vmid": vmid, "name": req.Name,
		"description": req.OwnerMark, "full": req.Full,
	}
	if node != tnode {
		p["target_node"] = node
	}
	if req.Storage != "" {
		p["storage"] = req.Storage
	}
	if req.Pool != "" {
		p["pool"] = req.Pool
	}
	return p
}

// templateRef reads a template ref: "node/qemu/vmid" as discovery names it,
// or a bare vmid on the placement node.
func templateRef(ref provider.Ref, placement string) (node, vmid string, err error) {
	if vmidValue.MatchString(ref.ID) {
		if placement == "" {
			return "", "", provider.Refused("proxmox: template %s needs a node, in its ref or as the placement", ref.ID)
		}
		return placement, ref.ID, nil
	}
	return guestRef(ref)
}

// findByMark finds the guest an earlier Clone made: named name, carrying
// mark as its description. One config read per guest with that name.
func (s *session) findByMark(ctx context.Context, name, mark string) (node, vmid string, err error) {
	var guests []ClusterGuest
	if err := s.c.Get(ctx, "/cluster/resources?type=vm", &guests); err != nil {
		return "", "", fmt.Errorf("proxmox: list guests to find %q: %w", name, err)
	}
	for _, g := range guests {
		if g.Name != name || g.Type != "qemu" {
			continue
		}
		id := strconv.Itoa(g.VMID)
		cfg, err := s.c.readConfig(ctx, g.Node, id)
		if err != nil {
			return "", "", fmt.Errorf("proxmox: read %s/qemu/%s to see whether it is this clone: %w", g.Node, id, err)
		}
		if strings.TrimSpace(cfg.str("description")) == mark {
			return g.Node, id, nil
		}
	}
	return "", "", nil
}

// sameNIC compares two net lines ignoring the MAC Proxmox adds to a model
// given without one ("virtio,bridge=vmbr0" is "virtio=BC:…,bridge=vmbr0").
func sameNIC(have, want string) bool {
	norm := func(s string) string {
		parts := strings.Split(s, ",")
		if len(parts) > 0 {
			parts[0] = strings.SplitN(parts[0], "=", 2)[0]
		}
		return strings.Join(parts, ",")
	}
	return norm(have) == norm(want)
}

// growDisk grows the boot disk to at least gb GiB. Never shrinks.
func (s *session) growDisk(ctx context.Context, node, vmid string, cfg guestConfig, gb int) (bool, error) {
	if gb <= 0 {
		return false, nil
	}
	disk := bootDisk(cfg)
	if disk == "" {
		return false, fmt.Errorf("proxmox: %s/qemu/%s has no boot disk to grow", node, vmid)
	}
	if ParseSize(KVField(cfg.str(disk), "size")) >= uint64(gb)<<30 {
		return false, nil
	}
	data, err := s.c.Call(ctx, http.MethodPut, fmt.Sprintf("/nodes/%s/qemu/%s/resize", node, vmid),
		url.Values{"disk": {disk}, "size": {fmt.Sprintf("%dG", gb)}})
	if err == nil {
		err = s.c.WaitIfTask(ctx, node, data, 2*time.Minute)
	}
	return err == nil, err
}

// bootDisk is the first disk of the boot order, or the first of scsi0,
// virtio0, sata0, ide0 that the config has.
func bootDisk(cfg guestConfig) string {
	if order := KVField(cfg.str("boot"), "order"); order != "" {
		for _, d := range strings.Split(order, ";") {
			if isDriveKey(d) && cfg.str(d) != "" && !strings.Contains(cfg.str(d), "media=cdrom") {
				return d
			}
		}
	}
	for _, d := range []string{"scsi0", "virtio0", "sata0", "ide0"} {
		if cfg.str(d) != "" {
			return d
		}
	}
	return ""
}

// noCloudTime is the timestamp every seed is built with, so the same seed
// is the same bytes.
var noCloudTime = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)

// dhcpNetworkConfig is DHCP on the first Ethernet interface, whatever the
// image names it.
const dhcpNetworkConfig = `version: 2
ethernets:
  primary:
    match:
      name: "e*"
    dhcp4: true
`

// GuestInit implements provider.Writer. See openspec accretion-apply, D4.
func (s *session) GuestInit(ctx context.Context, m provider.Target, seed provider.Seed) (provider.Placement, error) {
	node, vmid, cfg, absent, err := s.guard(ctx, m)
	if err != nil {
		return provider.Placement{}, err
	}
	if absent {
		return provider.Placement{}, provider.Refused("proxmox: nothing at %s", m.Ref.ID)
	}
	name := "accrete-" + strings.ToLower(seed.InstanceID) + ".iso"
	if seed.Storage == "" || seed.InstanceID == "" || !PlainFilename.MatchString(name) || !nodeName.MatchString(seed.Storage) {
		return provider.Placement{}, provider.Refused("proxmox: a seed needs a storage and an instance id that make a plain filename")
	}
	volume := seed.Storage + ":iso/" + name
	pl := provider.Placement{Drive: "ide2", Volume: volume, Storage: seed.Storage, Node: node}

	var content []struct {
		VolID string `json:"volid"`
	}
	if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=iso", node, seed.Storage), &content); err != nil {
		return provider.Placement{}, fmt.Errorf("proxmox: list %s on %s: %w", seed.Storage, node, err)
	}
	present := false
	for _, v := range content {
		present = present || v.VolID == volume
	}
	if !present {
		network := seed.NetworkConfig
		if network == nil {
			network = []byte(dhcpNetworkConfig)
		}
		iso, err := BuildNoCloudISO(map[string][]byte{
			"user-data": seed.UserData, "meta-data": seed.MetaData, "network-config": network,
		}, noCloudTime)
		if err != nil {
			return provider.Placement{}, err
		}
		if err := s.c.StorageUpload(ctx, node, seed.Storage, "iso", name, iso, 2*time.Minute); err != nil {
			return provider.Placement{}, unknownAfter("proxmox: upload seed "+name, err)
		}
		pl.Applied = true
	}
	if line := cfg.str("ide2"); line != volume && !strings.HasPrefix(line, volume+",") {
		data, err := s.c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, vmid),
			url.Values{"ide2": {volume + ",media=cdrom"}})
		if err == nil {
			err = s.c.WaitIfTask(ctx, node, data, 2*time.Minute)
		}
		if err != nil {
			return provider.Placement{}, unknownAfter("proxmox: attach seed "+name, err)
		}
		pl.Applied = true
	}
	return pl, nil
}

// Power implements provider.Writer. See openspec accretion-apply, D5.
func (s *session) Power(ctx context.Context, m provider.Target, want provider.PowerState) (provider.PowerResult, error) {
	action := map[provider.PowerState]string{provider.PowerRunning: "vm_start", provider.PowerStopped: "vm_shutdown"}[want]
	if action == "" {
		return provider.PowerResult{}, provider.Refused("proxmox: power state %q is not running or stopped", want)
	}
	node, vmid, _, absent, err := s.guard(ctx, m)
	if err != nil {
		return provider.PowerResult{}, err
	}
	if absent {
		return provider.PowerResult{}, provider.Refused("proxmox: nothing at %s", m.Ref.ID)
	}
	if st, err := s.c.ReadPower(ctx, node, "qemu", vmid); err == nil {
		switch st.State() {
		case string(want):
			return provider.PowerResult{State: want}, nil
		case "paused":
			return provider.PowerResult{}, provider.Refused("proxmox: %s is suspended; resume or stop it in Proxmox first", m.Ref.ID)
		}
	}
	out, err := s.c.PowerGuest(ctx, action, map[string]interface{}{"node": node, "type": "qemu", "vmid": vmid, "force_stop": true})
	if err != nil {
		return provider.PowerResult{}, unknownAfter("proxmox: power "+m.Ref.ID, err)
	}
	if !out.OK {
		if timedOut, _ := out.Data["timed_out"].(bool); timedOut {
			return provider.PowerResult{}, &provider.OutcomeUnknownError{Op: "proxmox: power " + m.Ref.ID, Err: errors.New(out.Message)}
		}
		return provider.PowerResult{}, fmt.Errorf("proxmox: power %s: %s", m.Ref.ID, out.Message)
	}
	return provider.PowerResult{State: want, Applied: true}, nil
}

// Delete implements provider.Writer. See openspec accretion-apply, D6.
func (s *session) Delete(ctx context.Context, m provider.Target, opts provider.DeleteOptions) (provider.Deleted, error) {
	node, vmid, _, absent, err := s.guard(ctx, m)
	if err != nil {
		return provider.Deleted{}, err
	}
	out := provider.Deleted{AlreadyAbsent: absent, RemovedRef: m.Ref}
	if !absent {
		st, err := s.c.ReadPower(ctx, node, "qemu", vmid)
		if err != nil {
			return provider.Deleted{}, fmt.Errorf("proxmox: read the state of %s: %w", m.Ref.ID, err)
		}
		if st.Status != "stopped" {
			if !opts.StopFirst {
				return provider.Deleted{}, provider.Refused("proxmox: %s is %s; a running guest is not deleted", m.Ref.ID, st.State())
			}
			stop, err := s.c.PowerGuest(ctx, "vm_stop", map[string]interface{}{"node": node, "type": "qemu", "vmid": vmid})
			if err != nil {
				return provider.Deleted{}, unknownAfter("proxmox: stop "+m.Ref.ID, err)
			}
			if !stop.OK {
				return provider.Deleted{}, fmt.Errorf("proxmox: stop %s before deleting it: %s", m.Ref.ID, stop.Message)
			}
			// The guards again: the guest was stopped under them, and is
			// deleted under them.
			if _, _, _, absent, err = s.guard(ctx, m); err != nil {
				return provider.Deleted{}, err
			}
		}
		if !absent {
			_, data, err := s.c.DeleteGuest(ctx, map[string]interface{}{
				"confirm": true, "node": node, "type": "qemu", "vmid": vmid, "name": m.Name, "expected_uuid": m.ExpectUUID,
			})
			if err != nil {
				return provider.Deleted{}, unknownAfter("proxmox: delete "+m.Ref.ID, err)
			}
			out.AlreadyAbsent = data["outcome"] == "already_absent"
		} else {
			out.AlreadyAbsent = true
		}
	}
	for _, p := range opts.RemovePlacements {
		file := strings.TrimPrefix(p.Volume, p.Storage+":iso/")
		if p.Node == "" || p.Storage == "" || file == p.Volume {
			return out, fmt.Errorf("proxmox: placement %q does not name a node, a storage and an iso volume", p.Volume)
		}
		if _, _, err := s.c.RemoveSeed(ctx, map[string]interface{}{"node": p.Node, "storage": p.Storage, "filename": file}); err != nil {
			return out, fmt.Errorf("proxmox: %s is deleted; removing its seed %s failed: %w", m.Ref.ID, p.Volume, err)
		}
	}
	return out, nil
}
