package proxmox

import (
	"strings"
	"testing"
)

func TestTemplateRecipeReadsSybilsInputs(t *testing.T) {
	rec, err := Recipe(TemplateBlueprint, map[string]any{
		"integration": "lab", "pve_node": "flk-vega-01", "template_name": "ubuntu-24-04-cloud",
		"image_url":      "https://cloud-images.ubuntu.com/noble/current/noble-server-cloudimg-amd64.img",
		"image_filename": "noble-server-cloudimg-amd64.img", "download_storage": "local", "disk_storage": "local-lvm",
		"disk_size": "32G", "cores": 4, "memory_mb": "4096", "on_enrolment_failure": "destroy", "notify_destination": "",
	})
	if err != nil {
		t.Fatal(err)
	}
	c := rec.Image.Create
	if rec.Hostname != "ubuntu-24-04-cloud" || c.Placement != "flk-vega-01" || c.Image != "local:import/noble-server-cloudimg-amd64.qcow2" ||
		c.Shape.DiskGB != 32 || c.Shape.Cores != 4 || c.Shape.MemoryMB != 4096 || c.Shape.NICs[0] != "virtio,bridge=vmbr0" ||
		rec.Image.Template == nil || rec.Image.Template.CloudInitStorage != "local-lvm" || rec.Seed != nil {
		t.Errorf("recipe %+v create %+v", rec, c)
	}
}

func TestTemplateRecipeRefusesBadInputs(t *testing.T) {
	base := func() map[string]any {
		return map[string]any{"template_name": "debian-12-cloud",
			"image_url": "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"}
	}
	for name, c := range map[string]struct {
		key  string
		val  any
		want string
	}{
		"underscore name": {"template_name", "debian_12", "not a DNS label"},
		"dotted name":     {"template_name", "debian.12", "not a DNS label"},
		"no url":          {"image_url", "", "required"},
		"ftp url":         {"image_url", "ftp://x/y.qcow2", "not an http"},
		"bad size":        {"disk_size", "20T", "not a size in GiB"},
		"iso file":        {"image_filename", "debian.iso", "not a .qcow2"},
		"bad hash":        {"image_sha256", "abc", "not a SHA-256"},
		"unknown":         {"hostname", "x", "takes no such input"},
		"no bridge":       {"net0", "virtio", "names no bridge"},
	} {
		in := base()
		in[c.key] = c.val
		if _, err := Recipe(TemplateBlueprint, in); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	rec, err := Recipe(TemplateBlueprint, base())
	if err != nil || rec.Image.Create.Image != "local:import/debian-12-genericcloud-amd64.qcow2" || rec.Image.Create.Shape.DiskGB != 20 {
		t.Errorf("defaults: %v %+v", err, rec.Image)
	}
}
