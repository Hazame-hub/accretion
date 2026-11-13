package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/hazame-hub/accretion/provider"
)

// The Proxmox Installer: provider.Installer on a session. See openspec
// accrete-installer-iso, design D3 and D4. The installer goes on ide2, the
// answer disc on ide3, the empty disk on scsi0, and the VM boots scsi0 first:
// an empty disk falls through to the installer, and once installed the disk
// boots even before the discs are removed.

var _ provider.Installer = (*session)(nil)

// Media implements provider.Installer.
func (s *session) Media(ctx context.Context, node, volume string) (provider.Media, error) {
	content, kind := "iso", "ISO Images"
	m := isoVolume.FindStringSubmatch(volume)
	if m == nil {
		m = importVolume.FindStringSubmatch(volume)
		content, kind = "import", "Import"
	}
	if m == nil {
		return provider.Media{}, provider.Refused("proxmox: %q is not an ISO volume nor a disk image in a storage's import content", volume)
	}
	if !nodeName.MatchString(node) {
		return provider.Media{}, provider.Refused("proxmox: %q is not a node name", node)
	}
	var listed []struct {
		VolID string `json:"volid"`
		Size  int64  `json:"size"`
		CTime int64  `json:"ctime"`
	}
	if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=%s", node, m[1], content), &listed); err != nil {
		return provider.Media{}, fmt.Errorf("proxmox: list %s on %s to find %s: %w", m[1], node, volume, err)
	}
	for _, c := range listed {
		if c.VolID == volume {
			out := provider.Media{Volume: volume, Size: c.Size}
			if c.CTime > 0 {
				out.Created = time.Unix(c.CTime, 0).UTC()
			}
			return out, nil
		}
	}
	return provider.Media{}, provider.Refused("proxmox: %s is not on %s's storage %s; upload it first (Datacenter → %s → %s → %s)", volume, node, m[1], node, m[1], kind)
}

// Create implements provider.Installer.
func (s *session) Create(ctx context.Context, req provider.CreateRequest) (provider.Machine, error) {
	switch {
	case provider.UUIDForms(req.MintUUID) == nil:
		return provider.Machine{}, provider.Refused("proxmox: create %q: mint UUID %q is not a usable UUID", req.Name, req.MintUUID)
	case req.Name == "" || strings.TrimSpace(req.OwnerMark) == "" || strings.TrimSpace(req.OwnerMark) != req.OwnerMark:
		return provider.Machine{}, provider.Refused("proxmox: a machine needs a name and an owner mark without surrounding spaces, so a retry can find its own work")
	case !nodeName.MatchString(req.Placement):
		return provider.Machine{}, provider.Refused("proxmox: create %q: %q is not a node name", req.Name, req.Placement)
	case !storageName.MatchString(req.DiskStorage) || req.Shape.DiskGB <= 0 && req.Image == "":
		return provider.Machine{}, provider.Refused("proxmox: create %q needs a disk storage and a disk size", req.Name)
	case req.Image == "" && !isoVolume.MatchString(req.Installer):
		return provider.Machine{}, provider.Refused("proxmox: create %q: %q is not an ISO volume", req.Name, req.Installer)
	case req.Image != "" && (req.Installer != "" || !importVolume.MatchString(req.Image)):
		return provider.Machine{}, provider.Refused("proxmox: create %q: %q is not a disk image in a storage's import content, or an installer is set too", req.Name, req.Image)
	case req.Shape.CPU != "" && !cpuModel.MatchString(req.Shape.CPU):
		return provider.Machine{}, provider.Refused("proxmox: cpu %q is not a model name", req.Shape.CPU)
	case len(req.Shape.NICs) > 4:
		return provider.Machine{}, provider.Refused("proxmox: %d network cards; a machine has at most 4", len(req.Shape.NICs))
	}
	node, vmid, created := req.Placement, req.ID, false
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
		// /cluster/nextid does not reserve: an id taken in the meantime is
		// told apart from this create's own by the guest found there.
		for attempt := 0; ; attempt++ {
			var next string
			if err := s.c.Get(ctx, "/cluster/nextid", &next); err != nil {
				return provider.Machine{}, fmt.Errorf("proxmox: allocate a vmid: %w", err)
			}
			err := s.createGuest(ctx, req, node, next)
			if err == nil {
				vmid, created = next, true
				break
			}
			ours, taken, lookErr := s.ours(ctx, node, next, req.Name, req.OwnerMark)
			if lookErr == nil && ours {
				vmid, created = next, true
				break
			}
			if lookErr == nil && taken && attempt < 2 {
				continue
			}
			return provider.Machine{}, unknownAfter("proxmox: create "+req.Name, err)
		}
	} else if req.ID != "" {
		ours, taken, err := s.ours(ctx, node, vmid, req.Name, req.OwnerMark)
		if err != nil {
			return provider.Machine{}, fmt.Errorf("proxmox: check vmid %s before creating: %w", vmid, err)
		}
		switch {
		case taken && !ours:
			return provider.Machine{}, provider.Refused("proxmox: vmid %s is another guest; refusing", vmid)
		case !taken:
			if err := s.createGuest(ctx, req, node, vmid); err != nil {
				return provider.Machine{}, unknownAfter("proxmox: create "+req.Name, err)
			}
			created = true
		}
	}
	cfg, err := s.c.awaitUnlocked(ctx, node, vmid)
	if err != nil {
		return provider.Machine{}, unknownAfter("proxmox: create "+req.Name, err)
	}
	// Created with the minted identity, so one found by its mark carries it
	// too; anything else is not this build's machine any more.
	if !SameUUID(cfg.uuid(), provider.UUIDForms(req.MintUUID)) {
		return provider.Machine{}, fmt.Errorf("proxmox: vmid %s (%s) carries this build's name and mark but SMBIOS UUID %q, not the minted %s; it is left alone",
			vmid, req.Name, cfg.uuid(), req.MintUUID)
	}
	// A disk copied from an image is the image's size; it is grown to the
	// declared size here, so a retry grows it too and never shrinks it.
	if req.Image != "" {
		grown, err := s.growDisk(ctx, node, vmid, cfg, req.Shape.DiskGB)
		if err != nil {
			return provider.Machine{}, unknownAfter("proxmox: grow the disk of "+req.Name, err)
		}
		created = created || grown
	}
	return provider.Machine{
		Ref:      refOf(node, vmid),
		Identity: provider.Identity{SMBIOSUUID: strings.ToLower(req.MintUUID), HypervisorGuestID: vmid, MACs: []string{}, IPs: []string{}},
		Applied:  created,
		Found:    !created,
	}, nil
}

// createGuest asks for the empty VM, born with its identity, its mark, its
// shape, its empty disk and the installer, and waits for the task.
func (s *session) createGuest(ctx context.Context, req provider.CreateRequest, node, vmid string) error {
	form := url.Values{
		"vmid":         {vmid},
		"name":         {req.Name},
		"description":  {req.OwnerMark},
		"smbios1":      {"uuid=" + strings.ToLower(req.MintUUID)},
		"ostype":       {"l26"},
		"agent":        {"1"},
		"scsihw":       {"virtio-scsi-single"},
		installDisk:    {fmt.Sprintf("%s:%d", req.DiskStorage, req.Shape.DiskGB)},
		installerDrive: {req.Installer + ",media=cdrom"},
		"boot":         {"order=" + installDisk + ";" + installerDrive},
	}
	if req.Image != "" {
		// Size 0 with import-from: the new disk is a copy of the image.
		form.Set(installDisk, fmt.Sprintf("%s:0,import-from=%s", req.DiskStorage, req.Image))
		form.Set("boot", "order="+installDisk)
		form.Del(installerDrive)
		// Fedora CoreOS and most cloud images write their console to the
		// first serial port as well as the screen.
		form.Set("serial0", "socket")
	}
	if req.Shape.Cores > 0 {
		form.Set("cores", strconv.Itoa(req.Shape.Cores))
	}
	if req.Shape.MemoryMB > 0 {
		form.Set("memory", strconv.Itoa(req.Shape.MemoryMB))
	}
	if req.Shape.CPU != "" {
		form.Set("cpu", req.Shape.CPU)
	}
	for i, nic := range req.Shape.NICs {
		if nic != "" {
			form.Set(fmt.Sprintf("net%d", i), nic)
		}
	}
	if req.Pool != "" {
		form.Set("pool", req.Pool)
	}
	data, err := s.c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu", node), form)
	if err == nil {
		err = s.c.WaitIfTask(ctx, node, data, 5*time.Minute)
	}
	return err
}

// PlaceAnswer implements provider.Installer.
func (s *session) PlaceAnswer(ctx context.Context, m provider.Target, a provider.Answer) (provider.Placement, error) {
	node, vmid, cfg, absent, err := s.guard(ctx, m)
	if err != nil {
		return provider.Placement{}, err
	}
	if absent {
		return provider.Placement{}, provider.Refused("proxmox: nothing at %s", m.Ref.ID)
	}
	name, volume := seedVolume(a.Storage, a.InstanceID)
	if a.Storage == "" || a.InstanceID == "" || !PlainFilename.MatchString(name) || !storageName.MatchString(a.Storage) {
		return provider.Placement{}, provider.Refused("proxmox: an answer disc needs a storage and an instance id that make a plain filename")
	}
	pl := provider.Placement{Drive: answerDrive, Volume: volume, Storage: a.Storage, Node: node}
	var content []struct {
		VolID string `json:"volid"`
	}
	if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=iso", node, a.Storage), &content); err != nil {
		return provider.Placement{}, fmt.Errorf("proxmox: list %s on %s: %w", a.Storage, node, err)
	}
	present := false
	for _, v := range content {
		present = present || v.VolID == volume
	}
	if !present {
		iso, err := BuildSeedISO(a.Label, a.Files, noCloudTime)
		if err != nil {
			return provider.Placement{}, provider.Refused("proxmox: %v", err)
		}
		if err := s.c.StorageUpload(ctx, node, a.Storage, "iso", name, iso, 2*time.Minute); err != nil {
			return provider.Placement{}, unknownAfter("proxmox: upload answer disc "+name, err)
		}
		pl.Applied = true
	}
	if line := cfg.str(answerDrive); line != volume && !strings.HasPrefix(line, volume+",") {
		data, err := s.c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, vmid),
			url.Values{answerDrive: {volume + ",media=cdrom"}})
		if err == nil {
			err = s.c.WaitIfTask(ctx, node, data, 2*time.Minute)
		}
		if err != nil {
			return provider.Placement{}, unknownAfter("proxmox: attach answer disc "+name, err)
		}
		pl.Applied = true
	}
	return pl, nil
}

// RemoveMedia implements provider.Installer.
func (s *session) RemoveMedia(ctx context.Context, m provider.Target, answer provider.Placement) (bool, error) {
	if _, own := seedVolume(answer.Storage, m.ExpectUUID); answer.Volume != own || answer.Node == "" || !storageName.MatchString(answer.Storage) {
		return false, provider.Refused("proxmox: %q is not the answer disc of the machine minted %s; refusing to remove it", answer.Volume, m.ExpectUUID)
	}
	node, vmid, cfg, absent, err := s.guard(ctx, m)
	if err != nil {
		return false, err
	}
	if absent {
		return false, provider.Refused("proxmox: nothing at %s", m.Ref.ID)
	}
	st, err := s.c.ReadPower(ctx, node, "qemu", vmid)
	if err != nil {
		return false, fmt.Errorf("proxmox: read the state of %s: %w", m.Ref.ID, err)
	}
	if st.Status != "stopped" {
		return false, provider.Refused("proxmox: %s is %s; its discs are removed only once the install has powered it off", m.Ref.ID, st.State())
	}
	form := url.Values{}
	if line := cfg.str(installerDrive); line != "" && !strings.HasPrefix(line, "none,") && line != "none" {
		form.Set(installerDrive, "none,media=cdrom")
	}
	if cfg.str(answerDrive) != "" {
		form.Set("delete", answerDrive)
	}
	applied := false
	if len(form) > 0 {
		data, err := s.c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, vmid), form)
		if err == nil {
			err = s.c.WaitIfTask(ctx, node, data, 2*time.Minute)
		}
		if err != nil {
			return false, unknownAfter("proxmox: eject the discs of "+m.Ref.ID, err)
		}
		applied = true
	}
	file := strings.TrimPrefix(answer.Volume, answer.Storage+":iso/")
	_, data, err := s.c.RemoveSeed(ctx, map[string]interface{}{"node": answer.Node, "storage": answer.Storage, "filename": file})
	if err != nil {
		var api *APIError
		if errors.As(err, &api) {
			return applied, fmt.Errorf("proxmox: the discs of %s are ejected; removing the answer disc %s failed: %w", m.Ref.ID, answer.Volume, err)
		}
		return applied, unknownAfter("proxmox: remove the answer disc "+answer.Volume, err)
	}
	if deleted, _ := data["deleted"].(bool); deleted {
		applied = true
	}
	return applied, nil
}
