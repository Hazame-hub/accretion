package fake

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/hazame-hub/accretion/provider"
)

// The Fake's write half: provider.Writer over the in-memory estate, with the
// same rules the write contract asks of a real provider, so
// providertest.WriteContract can be passed by something before Proxmox.

// writeState is what the estate knows beyond Resources: the owner mark of each
// machine, the seeds on its storage and the one attached to each machine.
type writeState struct {
	marks    map[provider.Ref]string
	seeds    map[string]bool
	attached map[provider.Ref]string
}

func (f *Fake) ws() *writeState {
	if f.write == nil {
		f.write = &writeState{marks: map[provider.Ref]string{}, seeds: map[string]bool{}, attached: map[provider.Ref]string{}}
	}
	return f.write
}

// InterruptNextClone makes the next Clone stop where a real one is most
// fragile: the machine exists and carries its owner mark, but its identity
// was never written, and the caller gets an OutcomeUnknownError as if the
// answer had been lost.
func (f *Fake) InterruptNextClone() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.interruptClone = true
}

// Mark returns the owner mark of a machine, "" when none, for assertions.
func (f *Fake) Mark(ref provider.Ref) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ws().marks[ref]
}

// Seeds reports whether a seed volume is on the storage, for assertions.
func (f *Fake) Seeds(volume string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ws().seeds[volume]
}

func (f *Fake) find(ref provider.Ref) int {
	for i, r := range f.Estate {
		if r.Ref == ref {
			return i
		}
	}
	return -1
}

func (s *session) Clone(_ context.Context, req provider.CloneRequest) (provider.Machine, error) {
	f := s.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	if provider.UUIDForms(req.MintUUID) == nil {
		return provider.Machine{}, provider.Refused("fake: clone %q: mint_uuid %q is not a usable UUID", req.Name, req.MintUUID)
	}
	if req.Name == "" || req.OwnerMark == "" {
		return provider.Machine{}, provider.Refused("fake: clone needs a name and an owner mark")
	}
	ti := f.find(req.Template)
	if ti < 0 || f.Estate[ti].Ref.Kind != provider.KindTemplate {
		return provider.Machine{}, fmt.Errorf("fake: no template %v", req.Template)
	}
	tmpl := f.Estate[ti]
	ws := f.ws()

	// An earlier attempt's machine, found by its id or by name and mark.
	at := -1
	if req.ID != "" {
		at = f.find(provider.Ref{Kind: provider.KindVM, ID: req.ID})
		if at >= 0 && (ws.marks[f.Estate[at].Ref] != req.OwnerMark || f.Estate[at].Name != req.Name) {
			return provider.Machine{}, provider.Refused("fake: id %s is taken by %q without this clone's mark", req.ID, f.Estate[at].Name)
		}
	} else {
		for i, r := range f.Estate {
			if r.Ref.Kind == provider.KindVM && r.Name == req.Name && ws.marks[r.Ref] == req.OwnerMark {
				at = i
				break
			}
		}
	}
	if at >= 0 {
		r := &f.Estate[at]
		switch got := r.Identity.SMBIOSUUID; {
		case sameUUID(got, req.MintUUID):
			return provider.Machine{Ref: r.Ref, Identity: r.Identity}, nil
		case got == "" || got == tmpl.Identity.SMBIOSUUID:
			// Cloned, never stamped: finish what the first attempt began.
			r.Identity.SMBIOSUUID = req.MintUUID
			shape(r, req.Shape)
			return provider.Machine{Ref: r.Ref, Identity: r.Identity, Applied: true}, nil
		default:
			return provider.Machine{}, provider.Refused("fake: %q carries SMBIOS UUID %s, not %s; someone changed it", r.Name, got, req.MintUUID)
		}
	}

	id := req.ID
	if id == "" {
		next := 100
		for _, r := range f.Estate {
			if n, err := strconv.Atoi(r.Ref.ID); err == nil && n >= next {
				next = n + 1
			}
		}
		id = strconv.Itoa(next)
	}
	parent := tmpl.Parent
	if req.Placement != "" {
		parent = &provider.Ref{Kind: provider.KindHost, ID: req.Placement}
	}
	mac := fmt.Sprintf("bc:24:11:fa:%02x:%02x", len(f.Estate)/256, len(f.Estate)%256)
	r := provider.Resource{
		Ref: provider.Ref{Kind: provider.KindVM, ID: id}, Name: req.Name, Parent: parent,
		State: "stopped", Tags: []string{},
		Identity: provider.Identity{MACs: []string{mac}, IPs: []string{}, HypervisorGuestID: id},
		Facts: provider.Facts{CPUCores: tmpl.Facts.CPUCores, RAMBytes: tmpl.Facts.RAMBytes, Disks: []provider.Disk{},
			NetworkInterfaces: []provider.Interface{{Name: "net0", Addresses: []string{}, HardwareAddr: mac}}},
		Memory: tmpl.Memory,
		Raw:    []byte(fmt.Sprintf(`{"vmid":%s,"name":%q}`, id, req.Name)),
	}
	ws.marks[r.Ref] = req.OwnerMark
	if f.interruptClone {
		f.interruptClone = false
		f.Estate = append(f.Estate, r)
		return provider.Machine{}, &provider.OutcomeUnknownError{Op: "fake: clone " + req.Name, Err: errors.New("answer lost")}
	}
	r.Identity.SMBIOSUUID = req.MintUUID
	shape(&r, req.Shape)
	f.Estate = append(f.Estate, r)
	return provider.Machine{Ref: r.Ref, Identity: r.Identity, Applied: true}, nil
}

func shape(r *provider.Resource, s provider.Shape) {
	if s.Cores > 0 {
		r.Facts.CPUCores = int32(s.Cores)
	}
	if s.MemoryMB > 0 {
		r.Facts.RAMBytes = int64(s.MemoryMB) << 20
		r.Memory = provider.MemoryAllocation{MaxBytes: r.Facts.RAMBytes, CurrentBytes: r.Facts.RAMBytes}
	}
	if s.DiskGB > 0 {
		r.Facts.Disks = []provider.Disk{{Total: uint64(s.DiskGB) << 30}}
	}
}

func sameUUID(got, want string) bool {
	for _, g := range provider.UUIDForms(got) {
		for _, w := range provider.UUIDForms(want) {
			if g == w {
				return true
			}
		}
	}
	return false
}

// guard finds the target and proves it is the machine meant. absent is true
// when nothing is at the ref.
func (f *Fake) guard(m provider.Target) (idx int, absent bool, err error) {
	if provider.UUIDForms(m.ExpectUUID) == nil {
		return -1, false, provider.Refused("fake: %v: expected UUID %q is not usable; refusing rather than acting unguarded", m.Ref, m.ExpectUUID)
	}
	i := f.find(m.Ref)
	if i < 0 {
		return -1, true, nil
	}
	r := f.Estate[i]
	if !sameUUID(r.Identity.SMBIOSUUID, m.ExpectUUID) {
		got := r.Identity.SMBIOSUUID
		if got == "" {
			got = "none"
		}
		return -1, false, provider.Refused("fake: %v carries SMBIOS UUID %s, not %s", m.Ref, got, m.ExpectUUID)
	}
	if m.Name != "" && r.Name != m.Name {
		return -1, false, provider.Refused("fake: %v is %q, not %q", m.Ref, r.Name, m.Name)
	}
	if m.OwnerMark != "" && f.ws().marks[m.Ref] != m.OwnerMark {
		return -1, false, provider.Refused("fake: %v does not carry this owner mark", m.Ref)
	}
	return i, false, nil
}

func (s *session) GuestInit(_ context.Context, m provider.Target, seed provider.Seed) (provider.Placement, error) {
	f := s.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	i, absent, err := f.guard(m)
	if err != nil {
		return provider.Placement{}, err
	}
	if absent {
		return provider.Placement{}, provider.Refused("fake: nothing at %v", m.Ref)
	}
	if seed.Storage == "" || seed.InstanceID == "" {
		return provider.Placement{}, provider.Refused("fake: a seed needs a storage and an instance id")
	}
	node := ""
	if p := f.Estate[i].Parent; p != nil {
		node = p.ID
	}
	vol := seed.Storage + ":iso/accrete-" + seed.InstanceID + ".iso"
	ws := f.ws()
	pl := provider.Placement{Drive: "ide2", Volume: vol, Storage: seed.Storage, Node: node}
	if !ws.seeds[vol] || ws.attached[m.Ref] != vol {
		pl.Applied = true
	}
	ws.seeds[vol] = true
	ws.attached[m.Ref] = vol
	return pl, nil
}

func (s *session) Power(_ context.Context, m provider.Target, want provider.PowerState) (provider.PowerResult, error) {
	f := s.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	if want != provider.PowerRunning && want != provider.PowerStopped {
		return provider.PowerResult{}, provider.Refused("fake: power state %q is not running or stopped", want)
	}
	i, absent, err := f.guard(m)
	if err != nil {
		return provider.PowerResult{}, err
	}
	if absent {
		return provider.PowerResult{}, provider.Refused("fake: nothing at %v", m.Ref)
	}
	r := &f.Estate[i]
	if r.State == "suspended" {
		return provider.PowerResult{}, provider.Refused("fake: %v is suspended", m.Ref)
	}
	if r.State == string(want) {
		return provider.PowerResult{State: want}, nil
	}
	r.State = string(want)
	return provider.PowerResult{State: want, Applied: true}, nil
}

func (s *session) Delete(_ context.Context, m provider.Target, opts provider.DeleteOptions) (provider.Deleted, error) {
	f := s.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	i, absent, err := f.guard(m)
	if err != nil {
		return provider.Deleted{}, err
	}
	ws := f.ws()
	if !absent {
		if f.Estate[i].State != "stopped" {
			if !opts.StopFirst {
				return provider.Deleted{}, provider.Refused("fake: %v is %s; stop it first", m.Ref, f.Estate[i].State)
			}
			f.Estate[i].State = "stopped"
		}
		f.Estate = append(f.Estate[:i], f.Estate[i+1:]...)
		delete(ws.marks, m.Ref)
		delete(ws.attached, m.Ref)
	}
	for _, p := range opts.RemovePlacements {
		delete(ws.seeds, p.Volume)
	}
	return provider.Deleted{AlreadyAbsent: absent, RemovedRef: m.Ref}, nil
}
