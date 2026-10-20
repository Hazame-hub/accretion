package plan

import (
	"strings"
	"testing"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

var twoCardBlueprint = Blueprint{
	ID: "provision-vm",
	Inputs: []BlueprintInput{
		{Name: "integration", Type: "string", Required: true, Source: InputSourceIntegrations},
		{Name: "hostname", Type: "string", Required: true},
		{Name: "ip_address", Type: "string", Default: "", Address: AddressIP},
		{Name: "gateway", Type: "string", Default: "", Address: AddressGateway},
		{Name: "nameservers", Type: "string", Default: "", Address: AddressNameservers},
		{Name: "ip_address1", Type: "string", Default: "", Address: AddressIPN(1)},
		{Name: "net0", Type: "string", Default: "virtio,bridge=vmbr0"},
		{Name: "net1", Type: "string", Default: ""},
	},
}

func storageNet() envmanifest.EnvironmentNetwork {
	return envmanifest.EnvironmentNetwork{Name: "storage", Zone: "sybil", Subnet: "10.30.0.0/24", Gateway: "10.30.0.1", SNAT: true}
}

func twoCardEnv(secondIP string) envmanifest.Environment {
	e := staticEnv(2, "")
	e.Spec.Networks = append(e.Spec.Networks, storageNet())
	e.Spec.Resources[0].ExtraNICs = []envmanifest.EnvironmentNIC{{Network: "storage", IP: secondIP}}
	return e
}

func twoCardWorld(sdn *ObservedSDN) Snapshot {
	w := world()
	w.Blueprints = []Blueprint{twoCardBlueprint, netBlueprint}
	w.SDN = sdn
	return w
}

func bothLive() *ObservedSDN {
	sdn := live("10.20.0.0/24", "10.20.0.1", true)
	sdn.Vnets["storage"] = ObservedVnet{Zone: "sybil", Subnets: []ObservedSubnet{{CIDR: "10.30.0.0/24", Gateway: "10.30.0.1", SNAT: true}}}
	return sdn
}

func TestEachCardExpandsIntoItsOwnDevice(t *testing.T) {
	p := Build(twoCardEnv(""), twoCardWorld(bothLive()))
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	e := p.Entries[0]
	if e.Inputs["net0"] != "virtio,bridge=okd" || e.Inputs["net1"] != "virtio,bridge=storage" {
		t.Fatalf("devices = %v %v", e.Inputs["net0"], e.Inputs["net1"])
	}
	if _, ok := e.Inputs["ip_address1"]; ok {
		t.Fatalf("a DHCP card was given an address: %v", e.Inputs["ip_address1"])
	}
}

// A later card's address comes from its own network's pool, and it gets no
// gateway or name servers: those are the first card's.
func TestALaterStaticCardGetsAnAddressFromItsOwnNetwork(t *testing.T) {
	p := Build(twoCardEnv("static"), twoCardWorld(bothLive()))
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	var got []string
	for _, e := range p.Entries {
		got = append(got, e.Inputs["ip_address"].(string)+" "+e.Inputs["ip_address1"].(string))
	}
	if strings.Join(got, ", ") != "10.20.0.2/24 10.30.0.2/24, 10.20.0.3/24 10.30.0.3/24" {
		t.Fatalf("addresses = %v", got)
	}
	if p.Entries[0].Inputs["gateway"] != "10.20.0.1" {
		t.Fatalf("gateway = %v, want the first card's", p.Entries[0].Inputs["gateway"])
	}
}

// A machine keeps the second card's address it was built with.
func TestALaterCardKeepsItsAddress(t *testing.T) {
	built := realized("web", 0, nil)
	built.IntentInputs = map[string]any{"ip_address": "10.20.0.40/24", "ip_address1": "10.30.0.40/24"}
	w := twoCardWorld(bothLive())
	w.Realizations = []Realization{built}
	p := Build(twoCardEnv("static"), w)
	if got := p.Entries[0].Inputs["ip_address1"]; got != "10.30.0.40/24" {
		t.Fatalf("second card = %v, want the address it was built with", got)
	}
	if got := p.Entries[1].Inputs["ip_address1"]; got != "10.30.0.2/24" {
		t.Fatalf("new machine's second card = %v", got)
	}
}

func TestABlueprintWithoutTheCardIsAPlanError(t *testing.T) {
	w := twoCardWorld(bothLive())
	w.Blueprints = []Blueprint{addressedBlueprint, netBlueprint}
	p := Build(twoCardEnv("static"), w)
	msg := strings.Join(p.Errors, " ")
	if !strings.Contains(msg, `has no input "net1"`) || !strings.Contains(msg, "cannot give network card 1 a fixed address") {
		t.Fatalf("errors = %v", p.Errors)
	}
}

// A machine waits for its second card's network as it does for its first.
func TestAMachineWaitsForEveryCardsNetwork(t *testing.T) {
	p := Build(twoCardEnv(""), twoCardWorld(live("10.20.0.0/24", "10.20.0.1", true)))
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	for _, e := range p.Entries {
		if e.Action != ActionUnknown || !strings.Contains(e.Reason, `waits for network "storage"`) {
			t.Fatalf("%s/%d = %s (%s)", e.Resource, e.Index, e.Action, e.Reason)
		}
	}
}
