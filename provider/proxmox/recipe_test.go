package proxmox

import (
	"strings"
	"testing"
)

func TestRecipeTakesTheBlueprintDefaults(t *testing.T) {
	r, err := Recipe(BuildableBlueprint, map[string]any{"template_vmid": "9000", "hostname": "web-1", "integration": "x"})
	if err != nil {
		t.Fatal(err)
	}
	c := r.Clone
	if c.Template.ID != "9000" || c.Placement != "pve" || c.Name != "web-1" || !c.Full ||
		c.Shape.Cores != 2 || c.Shape.MemoryMB != 2048 || c.Shape.CPU != "x86-64-v2-AES" || c.Shape.NICs[0] != "virtio,bridge=vmbr0" {
		t.Errorf("clone = %+v", c)
	}
	if r.SeedStorage != "local" || r.Hostname != "web-1" {
		t.Errorf("recipe = %+v", r)
	}
	if pl := r.Seed("6F2C9A10-3B4D-4E5F-8A9B-0C1D2E3F4A5B"); pl.Volume != "local:iso/accrete-6f2c9a10-3b4d-4e5f-8a9b-0c1d2e3f4a5b.iso" || pl.Node != "pve" {
		t.Errorf("seed = %+v", pl)
	}
}

func TestRecipeRefusesWhatV01CannotBuild(t *testing.T) {
	base := func(k string, v any) map[string]any {
		return map[string]any{"template_vmid": "9000", "hostname": "web-1", k: v}
	}
	for name, c := range map[string]struct {
		blueprint string
		inputs    map[string]any
		want      string
	}{
		"another blueprint": {"build-proxmox-template", base("x", 1), "not one accrete builds"},
		"fixed address":     {BuildableBlueprint, base("ip_address", "10.0.0.5/24"), "DHCP"},
		"gateway":           {BuildableBlueprint, base("gateway", "10.0.0.1"), "DHCP"},
		"second card":       {BuildableBlueprint, base("net1", "virtio,bridge=vmbr1"), "one network card"},
		"bad hostname":      {BuildableBlueprint, base("hostname", "web_1"), "DNS label"},
		"bad cores":         {BuildableBlueprint, base("cores", "two"), "whole number"},
		"no template":       {BuildableBlueprint, map[string]any{"hostname": "web-1"}, "template_vmid"},
	} {
		if _, err := Recipe(c.blueprint, c.inputs); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if _, err := Recipe(BuildableBlueprint, base("net1", "")); err != nil {
		t.Errorf("an empty net1 is no card: %v", err)
	}
}
