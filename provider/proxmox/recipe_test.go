package proxmox

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
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

func installInputs(k string, v any) map[string]any {
	in := map[string]any{"hostname": "ks-1", "iso": "local:iso/rhel-9.6-x86_64-dvd.iso", "pve_node": "pve1"}
	if k != "" {
		in[k] = v
	}
	return in
}

func TestAnInstallRecipeTakesTheBlueprintDefaults(t *testing.T) {
	r, err := Recipe(InstallBlueprint, installInputs("", nil))
	if err != nil {
		t.Fatal(err)
	}
	i := r.Install
	if i == nil || r.Clone.Template.ID != "" {
		t.Fatalf("recipe = %+v", r)
	}
	c := i.Create
	if c.Name != "ks-1" || c.Placement != "pve1" || c.Installer != "local:iso/rhel-9.6-x86_64-dvd.iso" || c.DiskStorage != "local-lvm" ||
		c.Shape.Cores != 2 || c.Shape.MemoryMB != 2048 || c.Shape.DiskGB != 32 || c.Shape.NICs[0] != "virtio,bridge=vmbr0" {
		t.Errorf("create = %+v", c)
	}
	if i.AnswerFormat != AnswerFormatKickstart || i.Timeout != DefaultInstallTimeout || i.ISOSHA256 != "" || r.SeedStorage != "local" {
		t.Errorf("install = %+v", i)
	}
	if pl := r.Seed("6F2C9A10-3B4D-4E5F-8A9B-0C1D2E3F4A5B"); pl.Drive != "ide3" || pl.Volume != "local:iso/accrete-6f2c9a10-3b4d-4e5f-8a9b-0c1d2e3f4a5b.iso" {
		t.Errorf("answer disc = %+v", pl)
	}
}

// Everything an install cannot take is refused before any request.
func TestAnInstallRecipeRefusesWhatItCannotBuild(t *testing.T) {
	for name, c := range map[string]struct {
		inputs map[string]any
		want   string
	}{
		"another format": {installInputs("answer_format", "autoinstall"), `"autoinstall" is not a format`},
		"bad node":       {installInputs("pve_node", "pve 1"), "not a node name"},
		"bad hostname":   {installInputs("hostname", "ks_1"), "DNS label"},
		"no iso":         {installInputs("iso", ""), "input iso"},
		"not an iso":     {installInputs("iso", "local:vztmpl/x.tar.zst"), "input iso"},
		"bad hash":       {installInputs("iso_sha256", "abc"), "SHA-256"},
		"small memory":   {installInputs("memory_mb", 1024), "below the 1536"},
		"zero deadline":  {installInputs("install_timeout_minutes", 0), "install_timeout_minutes"},
		"a clone input":  {installInputs("template_vmid", "9000"), "takes no such input"},
	} {
		if _, err := Recipe(InstallBlueprint, c.inputs); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	r, err := Recipe(InstallBlueprint, installInputs("iso_sha256", strings.Repeat("AB", 32)))
	if err != nil || r.Install.ISOSHA256 != strings.Repeat("ab", 32) {
		t.Errorf("declared hash: %v %+v", err, r.Install)
	}
}

// Sybil's Continuum ships Blueprints(); the install blueprint is accrete's
// own (design D2).
func TestTheInstallBlueprintIsNotInSybilsList(t *testing.T) {
	for _, bp := range Blueprints() {
		if bp.ID == InstallBlueprint {
			t.Fatal("Blueprints() has the install blueprint")
		}
	}
	found := false
	for _, bp := range AccreteBlueprints() {
		found = found || bp.ID == InstallBlueprint
	}
	if !found {
		t.Fatal("AccreteBlueprints() lacks the install blueprint")
	}
}

// Planned against the blueprints Sybil ships, a manifest using the install
// blueprint is refused by name; against accrete's, it plans a create.
func TestSybilsPlannerRefusesTheInstallBlueprint(t *testing.T) {
	env := envmanifest.Environment{Name: "lab", Spec: envmanifest.EnvironmentSpec{Integration: "lab", Resources: []envmanifest.EnvironmentResource{{
		Name: "ks", Count: 1, Blueprint: InstallBlueprint, Inputs: map[string]any{"hostname": "ks-1", "iso": dvd}}}}}
	world := func(bps []plan.Blueprint) plan.Snapshot {
		return plan.Snapshot{Connector: &plan.Connector{ID: uuid.New(), Name: "lab", Type: "proxmox"}, Blueprints: bps, Verdicts: map[uuid.UUID]string{}}
	}
	if p := plan.Build(env, world(Blueprints())); p.OK() || !strings.Contains(strings.Join(p.Errors, "\n"), `blueprint "install-proxmox-vm" is not one this connector offers`) {
		t.Fatalf("Sybil's list: ok %v, errors %v", p.OK(), p.Errors)
	}
	if p := plan.Build(env, world(AccreteBlueprints())); !p.OK() || p.Entries[0].Action != plan.ActionCreate {
		t.Fatalf("accrete's list: errors %v, entries %+v", p.Errors, p.Entries)
	}
}

func TestRecipeCarriesThePool(t *testing.T) {
	r, err := Recipe(BuildableBlueprint, map[string]any{"template_vmid": "9000", "hostname": "web-1", "pool": "sybil-lab"})
	if err != nil || r.Clone.Pool != "sybil-lab" {
		t.Fatalf("pool = %q, %v", r.Clone.Pool, err)
	}
	if r, _ := Recipe(BuildableBlueprint, map[string]any{"template_vmid": "9000", "hostname": "web-1"}); r.Clone.Pool != "" {
		t.Errorf("no pool declared, got %q", r.Clone.Pool)
	}
	if _, err := Recipe(BuildableBlueprint, map[string]any{"template_vmid": "9000", "hostname": "web-1", "pool": "a b"}); err == nil || !strings.Contains(err.Error(), "resource pool") {
		t.Errorf("bad pool name: %v", err)
	}
}
