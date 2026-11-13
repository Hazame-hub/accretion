package fake

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/hazame-hub/accretion/provider"
)

// The Fake's Installer: provider.Installer over the in-memory estate, under
// the rules providertest.InstallContract asks of a real provider.

var _ provider.Installer = (*session)(nil)

// LoseNextCreateAnswer makes the next Create be carried out and its answer
// lost: the machine exists, born with its identity, and the caller gets an
// OutcomeUnknownError.
func (f *Fake) LoseNextCreateAnswer() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.loseCreate = true
}

// Discs returns the installer and answer volumes in a machine's CD drives,
// "" for an empty drive, for assertions.
func (f *Fake) Discs(ref provider.Ref) (installer, answer string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.ws().installer[ref], f.ws().answer[ref]
}

func answerVolume(storage, instanceID string) string {
	return storage + ":iso/accrete-" + instanceID + ".iso"
}

// Media implements provider.Installer.
func (s *session) Media(_ context.Context, placement, volume string) (provider.Media, error) {
	f := s.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.find(provider.Ref{Kind: provider.KindHost, ID: placement}) < 0 {
		return provider.Media{}, provider.Refused("fake: no node %q", placement)
	}
	size, ok := f.ISOs[volume]
	if !ok {
		return provider.Media{}, provider.Refused("fake: %s is not on %s's storage", volume, placement)
	}
	return provider.Media{Volume: volume, Size: size}, nil
}

// Create implements provider.Installer.
func (s *session) Create(_ context.Context, req provider.CreateRequest) (provider.Machine, error) {
	f := s.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	switch {
	case provider.UUIDForms(req.MintUUID) == nil:
		return provider.Machine{}, provider.Refused("fake: create %q: mint_uuid %q is not a usable UUID", req.Name, req.MintUUID)
	case req.Name == "" || req.OwnerMark == "" || strings.TrimSpace(req.OwnerMark) != req.OwnerMark:
		return provider.Machine{}, provider.Refused("fake: create needs a name and an owner mark without surrounding spaces")
	case f.find(provider.Ref{Kind: provider.KindHost, ID: req.Placement}) < 0:
		return provider.Machine{}, provider.Refused("fake: create %q: no node %q", req.Name, req.Placement)
	case req.DiskStorage == "" || req.Shape.DiskGB <= 0 && req.Image == "":
		return provider.Machine{}, provider.Refused("fake: create %q needs a disk storage and a disk size", req.Name)
	case (req.Image == "") == (req.Installer == ""):
		return provider.Machine{}, provider.Refused("fake: create %q needs an installer image or a disk image, not both", req.Name)
	}
	if req.Image != "" {
		if _, ok := f.ISOs[req.Image]; !ok || !strings.Contains(req.Image, ":import/") {
			return provider.Machine{}, provider.Refused("fake: create %q: no disk image %s", req.Name, req.Image)
		}
	} else if _, ok := f.ISOs[req.Installer]; !ok {
		return provider.Machine{}, provider.Refused("fake: create %q: no installer image %s", req.Name, req.Installer)
	}
	ws := f.ws()

	// An earlier call's machine, by its id or by name and mark.
	for i, r := range f.Estate {
		if r.Ref.Kind != provider.KindVM {
			continue
		}
		byID := req.ID != "" && r.Ref.ID == req.ID
		if !byID && (req.ID != "" || r.Name != req.Name || ws.marks[r.Ref] != req.OwnerMark) {
			continue
		}
		if ws.marks[r.Ref] != req.OwnerMark || r.Name != req.Name {
			return provider.Machine{}, provider.Refused("fake: id %s is taken by %q without this create's mark", req.ID, r.Name)
		}
		if !sameUUID(r.Identity.SMBIOSUUID, req.MintUUID) {
			return provider.Machine{}, fmt.Errorf("fake: %s (%s) carries this create's name and mark but SMBIOS UUID %q, not the minted %s; it is left alone",
				r.Ref.ID, r.Name, r.Identity.SMBIOSUUID, req.MintUUID)
		}
		return provider.Machine{Ref: f.Estate[i].Ref, Identity: f.Estate[i].Identity, Found: true}, nil
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
	mac := fmt.Sprintf("bc:24:11:fb:%02x:%02x", len(f.Estate)/256, len(f.Estate)%256)
	r := provider.Resource{
		Ref: provider.Ref{Kind: provider.KindVM, ID: id}, Name: req.Name,
		Parent: &provider.Ref{Kind: provider.KindHost, ID: req.Placement},
		State:  "stopped", Tags: []string{},
		Identity: provider.Identity{SMBIOSUUID: req.MintUUID, MACs: []string{mac}, IPs: []string{}, HypervisorGuestID: id},
		Facts: provider.Facts{Disks: []provider.Disk{},
			NetworkInterfaces: []provider.Interface{{Name: "net0", Addresses: []string{}, HardwareAddr: mac}}},
		Raw: []byte(fmt.Sprintf(`{"vmid":%s,"name":%q}`, id, req.Name)),
	}
	shape(&r, req.Shape)
	f.Estate = append(f.Estate, r)
	ws.marks[r.Ref] = req.OwnerMark
	if req.Installer != "" {
		ws.installer[r.Ref] = req.Installer
	}
	if f.loseCreate {
		f.loseCreate = false
		return provider.Machine{}, &provider.OutcomeUnknownError{Op: "fake: create " + req.Name, Err: errors.New("answer lost")}
	}
	return provider.Machine{Ref: r.Ref, Identity: r.Identity, Applied: true}, nil
}

// PlaceAnswer implements provider.Installer.
func (s *session) PlaceAnswer(_ context.Context, m provider.Target, a provider.Answer) (provider.Placement, error) {
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
	if a.Storage == "" || a.InstanceID == "" || a.Label == "" {
		return provider.Placement{}, provider.Refused("fake: an answer disc needs a label, a storage and an instance id")
	}
	node := ""
	if p := f.Estate[i].Parent; p != nil {
		node = p.ID
	}
	vol := answerVolume(a.Storage, a.InstanceID)
	ws := f.ws()
	pl := provider.Placement{Drive: "ide3", Volume: vol, Storage: a.Storage, Node: node}
	if !ws.seeds[vol] || ws.answer[m.Ref] != vol {
		pl.Applied = true
	}
	ws.seeds[vol] = true
	ws.answer[m.Ref] = vol
	return pl, nil
}

// RemoveMedia implements provider.Installer.
func (s *session) RemoveMedia(_ context.Context, m provider.Target, answer provider.Placement) (bool, error) {
	f := s.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	if answer.Volume != answerVolume(answer.Storage, m.ExpectUUID) {
		return false, provider.Refused("fake: %q is not the answer disc of the machine minted %s", answer.Volume, m.ExpectUUID)
	}
	i, absent, err := f.guard(m)
	if err != nil {
		return false, err
	}
	if absent {
		return false, provider.Refused("fake: nothing at %v", m.Ref)
	}
	if st := f.Estate[i].State; st != "stopped" {
		return false, provider.Refused("fake: %v is %s; its discs are removed only once the install has powered it off", m.Ref, st)
	}
	ws := f.ws()
	applied := ws.installer[m.Ref] != "" || ws.answer[m.Ref] != "" || ws.seeds[answer.Volume]
	delete(ws.installer, m.Ref)
	delete(ws.answer, m.Ref)
	delete(ws.seeds, answer.Volume)
	return applied, nil
}

var _ provider.ImageUploader = (*session)(nil)

// UploadImage implements provider.ImageUploader: the image joins ISOs with
// the size of what was read.
func (s *session) UploadImage(_ context.Context, placement, volume string, body io.Reader) error {
	n, err := io.Copy(io.Discard, body)
	if err != nil {
		return err
	}
	f := s.fake
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.find(provider.Ref{Kind: provider.KindHost, ID: placement}) < 0 {
		return provider.Refused("fake: no node %q", placement)
	}
	if !strings.Contains(volume, ":import/") {
		return provider.Refused("fake: %q is not a disk image volume", volume)
	}
	if _, ok := f.ISOs[volume]; ok {
		return provider.Refused("fake: %s is already there; an image is never overwritten", volume)
	}
	f.ISOs[volume] = n
	return nil
}
