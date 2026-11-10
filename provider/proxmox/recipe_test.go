package proxmox

import (
	"encoding/json"
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
		"no prefix":         {BuildableBlueprint, base("ip_address", "10.0.0.5"), "with its prefix"},
		"gateway on dhcp":   {BuildableBlueprint, base("gateway", "10.0.0.1"), "has no fixed address"},
		"dns on dhcp":       {BuildableBlueprint, base("nameservers", "10.0.0.1"), "go with a fixed address"},
		"address, no card":  {BuildableBlueprint, base("ip_address2", "10.0.2.5/24"), "no card net2"},
		"gap in cards":      {BuildableBlueprint, base("net2", "virtio,bridge=vmbr2"), "without a gap"},
		"card, no bridge":   {BuildableBlueprint, base("net1", "virtio"), "names no bridge"},
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

func TestRecipeAddressesEachCardByItsMAC(t *testing.T) {
	r, err := Recipe(BuildableBlueprint, map[string]any{
		"template_vmid": "9000", "hostname": "mgmt-01",
		"net0": "virtio,bridge=vmgmt", "ip_address": "10.10.0.10/24", "gateway": "10.10.0.1", "nameservers": "10.10.1.11, 10.10.1.12",
		"net1": "virtio,bridge=vk8sx,tag=30", "net2": "virtio,bridge=vobsv", "ip_address2": "10.10.7.10/24",
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(r.Clone.Shape.NICs, " "); got != "virtio,bridge=vmgmt virtio,bridge=vk8sx,tag=30 virtio,bridge=vobsv" {
		t.Errorf("cards = %s", got)
	}
	want := `version: 2
ethernets:
  net0:
    match:
      macaddress: "{{accrete.mac.net0}}"
    dhcp4: false
    addresses:
      - 10.10.0.10/24
    routes:
      - to: 0.0.0.0/0
        via: 10.10.0.1
    nameservers:
      addresses:
        - 10.10.1.11
        - 10.10.1.12
  net1:
    match:
      macaddress: "{{accrete.mac.net1}}"
    dhcp4: true
  net2:
    match:
      macaddress: "{{accrete.mac.net2}}"
    dhcp4: false
    addresses:
      - 10.10.7.10/24
`
	if string(r.NetworkConfig) != want {
		t.Errorf("network config:\n%s\nwant:\n%s", r.NetworkConfig, want)
	}
	filled, err := fillMACs(r.NetworkConfig, testConfig(map[string]string{
		"net0": "virtio=BC:24:11:AA:00:01,bridge=vmgmt", "net1": "virtio=BC:24:11:AA:00:02,bridge=vk8sx,tag=30", "net2": "virtio=BC:24:11:AA:00:03,bridge=vobsv",
	}))
	if err != nil || !strings.Contains(string(filled), `macaddress: "bc:24:11:aa:00:03"`) || strings.Contains(string(filled), "{{") {
		t.Errorf("filled (%v):\n%s", err, filled)
	}
	if _, err := fillMACs(r.NetworkConfig, testConfig(map[string]string{"net0": "virtio=BC:24:11:AA:00:01,bridge=vmgmt"})); err == nil {
		t.Error("a card the clone lacks must stop the seed")
	}
}

func TestOneDHCPCardKeepsTheDefaultSeed(t *testing.T) {
	r, err := Recipe(BuildableBlueprint, map[string]any{"template_vmid": "9000", "hostname": "web-1", "net1": ""})
	if err != nil || r.NetworkConfig != nil {
		t.Errorf("one card on DHCP must keep the seed's default network config: %v %q", err, r.NetworkConfig)
	}
}

func testConfig(m map[string]string) guestConfig {
	raw := map[string]json.RawMessage{}
	for k, v := range m {
		b, _ := json.Marshal(v)
		raw[k] = b
	}
	return guestConfig{raw: raw}
}
