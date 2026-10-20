package plan

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

var addressedBlueprint = Blueprint{
	ID: "provision-vm",
	Inputs: []BlueprintInput{
		{Name: "integration", Type: "string", Required: true, Source: InputSourceIntegrations},
		{Name: "hostname", Type: "string", Required: true},
		{Name: "ip_address", Type: "string", Default: "", Address: AddressIP},
		{Name: "gateway", Type: "string", Default: "", Address: AddressGateway},
		{Name: "nameservers", Type: "string", Default: "", Address: AddressNameservers},
		{Name: "net0", Type: "string", Default: "virtio,bridge=vmbr0"},
	},
}

func staticEnv(count int, staticRange string) envmanifest.Environment {
	r := web(count)
	r.Network = "okd"
	r.IP = "static"
	return envmanifest.Environment{Name: "staging", Spec: envmanifest.EnvironmentSpec{
		Integration: "lab",
		Networks: []envmanifest.EnvironmentNetwork{{Name: "okd", Zone: "sybil", Subnet: "10.20.0.0/24", Gateway: "10.20.0.1",
			SNAT: true, DHCPRange: "10.20.0.128-10.20.0.254", StaticRange: staticRange, DNS: []string{"10.20.0.1", "1.1.1.1"}}},
		Resources: []envmanifest.EnvironmentResource{r},
	}}
}

func addressWorld(rs ...Realization) Snapshot {
	w := world(rs...)
	w.Blueprints = []Blueprint{addressedBlueprint, netBlueprint}
	w.SDN = live("10.20.0.0/24", "10.20.0.1", true)
	return w
}

func addressesOf(p Plan) []string {
	var out []string
	for _, e := range p.Entries {
		if a, ok := e.Inputs["ip_address"].(string); ok {
			out = append(out, a)
		}
	}
	return out
}

func TestStaticAddressesComeFromThePoolLowestFirst(t *testing.T) {
	p := Build(staticEnv(3, ""), addressWorld())
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	if got := strings.Join(addressesOf(p), " "); got != "10.20.0.2/24 10.20.0.3/24 10.20.0.4/24" {
		t.Fatalf("addresses = %s", got)
	}
	e := p.Entries[0]
	if e.Inputs["gateway"] != "10.20.0.1" || e.Inputs["nameservers"] != "10.20.0.1,1.1.1.1" {
		t.Fatalf("gateway/dns = %v %v", e.Inputs["gateway"], e.Inputs["nameservers"])
	}
}

// A machine keeps its address; a new one takes the lowest no machine holds,
// whichever environment holds it.
func TestAnAddressIsKeptAndNeverHandedOutTwice(t *testing.T) {
	asset := uuid.New()
	mine := realized("web", 1, &asset)
	mine.IntentInputs = map[string]any{"hostname": "web-2", "ip_address": "10.20.0.50/24", "gateway": "10.20.0.1",
		"nameservers": "10.20.0.1,1.1.1.1", "integration": labID.String(), "net0": "virtio,bridge=okd"}
	other := Realization{ID: uuid.New(), IntegrationID: labID, ResourceRefID: "pve/qemu/900", Managed: true,
		IntentInputs: map[string]any{"ip_address": "10.20.0.10/24"},
		Intent:       &RealizationIntent{Kind: IntentKind, Name: "prod", ResourceName: "db", InstanceIndex: 0}}
	building := Run{ID: uuid.New(), IntentInputs: map[string]any{"ip_address": "10.20.0.11/24"}}
	w := addressWorld(mine, other)
	w.InFlight = []Run{building}
	w.Verdicts[asset] = AcceptanceAccepted

	p := Build(staticEnv(3, "10.20.0.10-10.20.0.60"), w)
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	if got := strings.Join(addressesOf(p), " "); got != "10.20.0.12/24 10.20.0.50/24 10.20.0.13/24" {
		t.Fatalf("addresses = %s", got)
	}
	if p.Entries[1].Action != ActionNoOp {
		t.Fatalf("the machine that kept its address plans %s: %+v", p.Entries[1].Action, p.Entries[1].Diff)
	}
}

func TestAnExhaustedPoolIsAPlanError(t *testing.T) {
	p := Build(staticEnv(3, "10.20.0.10-10.20.0.11"), addressWorld())
	if !strings.Contains(strings.Join(p.Errors, " "), "no free address left in 10.20.0.10-10.20.0.11") {
		t.Fatalf("errors = %v", p.Errors)
	}
}

func TestStaticNeedsANetworkWithDNSAndABlueprintThatTakesIt(t *testing.T) {
	e := staticEnv(1, "")
	e.Spec.Networks[0].DNS = nil
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "declares no dns servers") {
		t.Fatalf("no dns: %v", err)
	}
	e = staticEnv(1, "")
	e.Spec.Resources[0].Network = "vmbr0"
	if err := e.Validate(); err == nil || !strings.Contains(err.Error(), "needs network:") {
		t.Fatalf("undeclared network: %v", err)
	}
	bare := world()
	bare.Blueprints = []Blueprint{nicBlueprint, netBlueprint}
	bare.SDN = live("10.20.0.0/24", "10.20.0.1", true)
	if p := Build(staticEnv(1, ""), bare); !strings.Contains(strings.Join(p.Errors, " "), "cannot be given a fixed address") {
		t.Fatalf("a blueprint without address inputs: %v", p.Errors)
	}
}
