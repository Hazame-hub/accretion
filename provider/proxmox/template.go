package proxmox

import (
	"context"
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

// TemplateBlueprint is Sybil's blueprint for the template a provisioning run
// clones, built from a distribution's cloud image. accrete builds it too, with
// the same inputs, so one manifest builds the same template from the command
// line or from Sybil:
//
//	create    a VM whose disk is a copy of the image (fetched and checked
//	          first when the storage does not have it), never started
//	template  the provider's cloud-init drive attached, the console on the
//	          first serial port, and the VM converted into a template
//	confirm   read back as a template with the declared size
//
// It is never booted: cloud-init in the image has then claimed no instance,
// so every clone runs it afresh with its own config. The QEMU guest agent is
// not baked in for the same reason; a clone's first-boot config installs it
// (Sybil's enrolment does), and images that ship it run it from the start.
const TemplateBlueprint = "build-proxmox-template"

// templateDiskSize is the disk_size input: whole GiB, with or without a G.
var templateDiskSize = regexp.MustCompile(`^([1-9][0-9]{0,5})\s*(G|GB|GiB)?$`)

// templateImageFile is the image_filename input: a plain file name in import
// content.
var templateImageFile = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._+-]*\.(qcow2|raw|vmdk|img)$`)

// templateRecipe is Recipe for TemplateBlueprint. Inputs only Sybil's run
// reads (the connector, the rollback policy, a notification) are accepted
// and ignored.
func templateRecipe(inputs map[string]any, str func(string, string) string, num func(string, int) (int, error)) (provider.Recipe, error) {
	known := map[string]bool{"integration": true, "on_enrolment_failure": true, "notify_destination": true}
	for _, bp := range Blueprints() {
		if bp.ID != TemplateBlueprint {
			continue
		}
		for _, in := range bp.Inputs {
			known[in.Name] = true
		}
	}
	for k := range inputs {
		if !known[k] {
			return provider.Recipe{}, fmt.Errorf("input %s: %s takes no such input", k, TemplateBlueprint)
		}
	}
	node := str("pve_node", "pve")
	name := str("template_name", "debian-12-cloud")
	from := str("image_url", "")
	sum := strings.ToLower(str("image_sha256", ""))
	file := str("image_filename", "")
	store := str("download_storage", "local")
	disk := str("disk_storage", "local-lvm")
	cpu := str("cpu", "x86-64-v2-AES")
	size := str("disk_size", "20G")
	if file == "" && from != "" {
		if u, err := url.Parse(from); err == nil {
			file = u.Path[strings.LastIndex(u.Path, "/")+1:]
		}
	}
	// Proxmox keeps a downloaded .img under import content only as .raw or
	// .qcow2; a cloud .img is qcow2 inside, as Ubuntu's are.
	if strings.HasSuffix(file, ".img") {
		file = strings.TrimSuffix(file, ".img") + ".qcow2"
	}
	m := templateDiskSize.FindStringSubmatch(size)
	switch {
	case !nodeName.MatchString(node):
		return provider.Recipe{}, fmt.Errorf("input pve_node: %q is not a node name", node)
	case !nodeName.MatchString(name) || strings.Contains(name, "."):
		return provider.Recipe{}, fmt.Errorf("input template_name: %q is not a DNS label (letters, digits and hyphens)", name)
	case from == "":
		return provider.Recipe{}, errors.New("input image_url: required, the cloud image the template is made from")
	case !imageURL.MatchString(from):
		return provider.Recipe{}, fmt.Errorf("input image_url: %q is not an http or https URL", from)
	case sum != "" && !sha256Hex.MatchString(sum):
		return provider.Recipe{}, fmt.Errorf("input image_sha256: %q is not a SHA-256 in hex", sum)
	case !templateImageFile.MatchString(file):
		return provider.Recipe{}, fmt.Errorf("input image_filename: %q is not a .qcow2, .raw or .vmdk file name", file)
	case !storageName.MatchString(store):
		return provider.Recipe{}, fmt.Errorf("input download_storage: %q is not a storage name", store)
	case !storageName.MatchString(disk):
		return provider.Recipe{}, fmt.Errorf("input disk_storage: %q is not a storage name", disk)
	case !cpuModel.MatchString(cpu):
		return provider.Recipe{}, fmt.Errorf("input cpu: %q is not a model name", cpu)
	case m == nil:
		return provider.Recipe{}, fmt.Errorf("input disk_size: %q is not a size in GiB, like 20G", size)
	}
	gb, _ := strconv.Atoi(m[1])
	cores, err := num("cores", 2)
	if err != nil {
		return provider.Recipe{}, err
	}
	memory, err := num("memory_mb", 2048)
	if err != nil {
		return provider.Recipe{}, err
	}
	if cores < 1 || memory < 512 {
		return provider.Recipe{}, fmt.Errorf("inputs cores and memory_mb: %d cores and %d MB are below the 1 core and 512 MB a machine needs", cores, memory)
	}
	nic := str("net0", "virtio,bridge=vmbr0")
	if KVField(nic, "bridge") == "" {
		return provider.Recipe{}, fmt.Errorf("input net0: %q names no bridge", nic)
	}
	image := store + ":import/" + file
	if !importVolume.MatchString(image) {
		return provider.Recipe{}, fmt.Errorf("inputs download_storage and image_filename: %q is not a disk image in a storage's import content", image)
	}
	return provider.Recipe{
		Hostname: name,
		Image: &provider.ImageRecipe{
			Create: provider.CreateRequest{Name: name, Placement: node, DiskStorage: disk, Image: image,
				Shape: provider.Shape{Cores: cores, MemoryMB: memory, CPU: cpu, DiskGB: gb, NICs: []string{nic}}},
			ImageSHA256: sum,
			URL:         from,
			Template:    &provider.TemplateFinish{CloudInitStorage: disk},
		},
	}, nil
}

var _ provider.TemplateMaker = (*session)(nil)

// templateWait bounds the conversion: Proxmox renames the disk to its base
// volume, which on LVM-thin and ZFS is quick, and on a directory storage
// rewrites nothing either.
const templateWait = 5 * time.Minute

// MakeTemplate implements provider.TemplateMaker. The guards hold before
// anything is sent; a guest that is running, or carries something other than
// a cloud-init drive on ide2, is refused rather than changed.
func (s *session) MakeTemplate(ctx context.Context, m provider.Target, f provider.TemplateFinish) (bool, error) {
	if !storageName.MatchString(f.CloudInitStorage) {
		return false, provider.Refused("proxmox: %q is not a storage name for the cloud-init drive", f.CloudInitStorage)
	}
	node, vmid, cfg, absent, err := s.guard(ctx, m)
	if err != nil {
		return false, err
	}
	if absent {
		return false, provider.Refused("proxmox: nothing at %s", m.Ref.ID)
	}
	ide2 := cfg.str("ide2")
	hasDrive := strings.Contains(ide2, "cloudinit")
	if cfg.str("template") == "1" {
		if !hasDrive {
			return false, provider.Refused("proxmox: %s is already a template, without a cloud-init drive; it is left as it is", m.Ref.ID)
		}
		return false, nil
	}
	if ide2 != "" && !hasDrive {
		return false, provider.Refused("proxmox: %s has %q on ide2, where its cloud-init drive goes; refusing to replace it", m.Ref.ID, ide2)
	}
	st, err := s.c.ReadPower(ctx, node, "qemu", vmid)
	if err != nil {
		return false, fmt.Errorf("proxmox: read the state of %s: %w", m.Ref.ID, err)
	}
	if st.Status != "stopped" {
		return false, provider.Refused("proxmox: %s is %s; only a machine that never ran is made a template", m.Ref.ID, st.State())
	}
	set := url.Values{}
	if !hasDrive {
		set.Set("ide2", f.CloudInitStorage+":cloudinit")
	}
	// Cloud images write their console to the first serial port; the
	// screen stays blank after the boot loader without this.
	if cfg.str("serial0") == "" {
		set.Set("serial0", "socket")
	}
	if cfg.str("vga") == "" {
		set.Set("vga", "serial0")
	}
	if len(set) > 0 {
		data, err := s.c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, vmid), set)
		if err == nil {
			err = s.c.WaitIfTask(ctx, node, data, 2*time.Minute)
		}
		if err != nil {
			return false, unknownAfter("proxmox: attach the cloud-init drive of "+m.Ref.ID, err)
		}
	}
	// The guards again: the config was changed under them, and the
	// conversion is not undone by any verb.
	if _, _, _, absent, err = s.guard(ctx, m); err != nil || absent {
		if err == nil {
			err = provider.Refused("proxmox: %s is gone", m.Ref.ID)
		}
		return true, fmt.Errorf("proxmox: %s was given its cloud-init drive, and no longer passes the guards for its conversion: %w", m.Ref.ID, err)
	}
	data, err := s.c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%s/template", node, vmid), url.Values{})
	if err == nil {
		err = s.c.WaitIfTask(ctx, node, data, templateWait)
	}
	if err != nil {
		return true, unknownAfter("proxmox: convert "+m.Ref.ID+" into a template", err)
	}
	after, err := s.c.readConfig(ctx, node, vmid)
	if err != nil {
		return true, unknownAfter("proxmox: read "+m.Ref.ID+" back after its conversion", err)
	}
	if after.str("template") != "1" || !strings.Contains(after.str("ide2"), "cloudinit") {
		return true, fmt.Errorf("proxmox: %s was converted, but reads back as template=%q with ide2 %q", m.Ref.ID, after.str("template"), after.str("ide2"))
	}
	return true, nil
}
