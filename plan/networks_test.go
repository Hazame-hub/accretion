package plan

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

var netBlueprint = Blueprint{
	ID:   "provision-net",
	Role: BlueprintRoleCreateNetwork,
	Inputs: []BlueprintInput{
		{Name: "integration", Type: "string", Required: true, Source: InputSourceIntegrations},
		{Name: "vnet", Type: "string", Required: true},
	},
}

var nicBlueprint = Blueprint{
	ID: "provision-vm",
	Inputs: []BlueprintInput{
		{Name: "integration", Type: "string", Required: true, Source: InputSourceIntegrations},
		{Name: "hostname", Type: "string", Required: true},
		{Name: "net0", Type: "string", Default: "virtio,bridge=vmbr0"},
	},
}

func okdNet() envmanifest.EnvironmentNetwork {
	return envmanifest.EnvironmentNetwork{Name: "okd", Zone: "sybil", Subnet: "10.20.0.0/24", Gateway: "10.20.0.1", SNAT: true,
		DHCPRange: "10.20.0.128-10.20.0.254"}
}

func netEnv(nets []envmanifest.EnvironmentNetwork, resources ...envmanifest.EnvironmentResource) envmanifest.Environment {
	e := env(resources...)
	e.Spec.Networks = nets
	return e
}

func netWorld(sdn *ObservedSDN) Snapshot {
	w := world()
	w.Blueprints = []Blueprint{nicBlueprint, netBlueprint}
	w.SDN = sdn
	return w
}

func node(count int) envmanifest.EnvironmentResource {
	return envmanifest.EnvironmentResource{Name: "node", Count: count, Blueprint: "provision-vm", Network: "okd",
		Inputs: map[string]any{"hostname": "okd-{{instance.number}}"}}
}

func live(subnet, gw string, snat bool) *ObservedSDN {
	return &ObservedSDN{Vnets: map[string]ObservedVnet{
		"okd": {Zone: "sybil", Subnets: []ObservedSubnet{{CIDR: subnet, Gateway: gw, SNAT: snat, DHCPRange: "10.20.0.128-10.20.0.254"}}},
	}, ZoneDHCP: map[string]string{"sybil": "dnsmasq"}}
}

func TestAMissingNetworkPlansACreateAndHoldsItsMachines(t *testing.T) {
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}, node(2)), netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}}))
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	if len(p.Networks) != 1 || p.Networks[0].Action != ActionCreate {
		t.Fatalf("networks = %+v", p.Networks)
	}
	n := p.Networks[0]
	if n.Inputs["zone_dhcp"] != "dnsmasq" || n.Inputs["dhcp_range"] != "10.20.0.128-10.20.0.254" {
		t.Fatalf("network DHCP inputs = %v", n.Inputs)
	}
	if n.Blueprint != "provision-net" || n.Inputs["vnet"] != "okd" || n.Inputs["integration"] != labID.String() {
		t.Fatalf("network launch = %s %v", n.Blueprint, n.Inputs)
	}
	for _, e := range p.Entries {
		if e.Action != ActionUnknown || !strings.Contains(e.Reason, `waits for network "okd"`) {
			t.Fatalf("machine %s/%d = %s (%s), want held for the network", e.Resource, e.Index, e.Action, e.Reason)
		}
	}
	if !contains(p.Limits, NetworksNotOwned) {
		t.Fatalf("limits = %v", p.Limits)
	}
}

func TestALiveNetworkLetsItsMachinesBeCreatedOnIt(t *testing.T) {
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}, node(2)), netWorld(live("10.20.0.0/24", "10.20.0.1", true)))
	if p.Networks[0].Action != ActionNoOp {
		t.Fatalf("network = %+v", p.Networks[0])
	}
	if got := actions(p); got != "node/0=create node/1=create" {
		t.Fatalf("actions = %s", got)
	}
	if nic := p.Entries[0].Inputs["net0"]; nic != "virtio,bridge=okd" {
		t.Fatalf("net0 = %v", nic)
	}
}

func TestANetworkThatDiffersIsAChangeAndNeverACreate(t *testing.T) {
	cases := map[string]*ObservedSDN{
		"gateway": live("10.20.0.0/24", "10.20.0.254", true),
		"snat":    live("10.20.0.0/24", "10.20.0.1", false),
		"subnet":  live("10.30.0.0/24", "10.30.0.1", true),
		"zone":    {Vnets: map[string]ObservedVnet{"okd": {Zone: "other"}}},
		"dhcp": {Vnets: map[string]ObservedVnet{"okd": {Zone: "sybil", Subnets: []ObservedSubnet{
			{CIDR: "10.20.0.0/24", Gateway: "10.20.0.1", SNAT: true}}}}},
	}
	for name, sdn := range cases {
		p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}, node(1)), netWorld(sdn))
		if p.Networks[0].Action != ActionChange {
			t.Errorf("%s: network = %s (%s)", name, p.Networks[0].Action, p.Networks[0].Reason)
		}
		if p.Entries[0].Action != ActionUnknown {
			t.Errorf("%s: machine on a changed network = %s", name, p.Entries[0].Action)
		}
	}
}

// Could not look is never "does not exist": that is the answer that creates.
func TestUnreadableNetworksAreUnknown(t *testing.T) {
	w := netWorld(nil)
	w.SDNError = "403 Forbidden"
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}, node(1)), w)
	if p.Networks[0].Action != ActionUnknown || !strings.Contains(p.Networks[0].Reason, "403") {
		t.Fatalf("network = %+v", p.Networks[0])
	}
}

// sdn_apply publishes every pending edit, so a create waits until there are none.
func TestSomeoneElsesPendingSDNEditBlocksACreate(t *testing.T) {
	sdn := &ObservedSDN{Vnets: map[string]ObservedVnet{}, Pending: []string{"vnet vlab (new)"}}
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}), netWorld(sdn))
	if p.Networks[0].Action != ActionUnknown || !strings.Contains(p.Networks[0].Reason, "vnet vlab (new)") {
		t.Fatalf("network = %+v", p.Networks[0])
	}
}

func TestANetworkBeingCreatedIsNotCreatedTwice(t *testing.T) {
	runID := uuid.New()
	w := netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}})
	w.InFlight = []Run{{ID: runID, Intent: &RealizationIntent{
		Kind: IntentKind, Name: "staging", ResourceName: NetworkResourcePrefix + "okd"}}}
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}, node(1)), w)
	if p.Networks[0].Action != ActionUnknown || p.Networks[0].RunID == nil || *p.Networks[0].RunID != runID {
		t.Fatalf("network = %+v", p.Networks[0])
	}
	// The network's run is not a machine instance that is no longer declared.
	for _, e := range p.Entries {
		if strings.HasPrefix(e.Resource, NetworkResourcePrefix) {
			t.Fatalf("network run planned as a machine: %+v", e)
		}
	}
}

func TestANetworkOnlyEnvironmentPlans(t *testing.T) {
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}), netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}}))
	if !p.OK() || len(p.Entries) != 0 || p.Networks[0].Action != ActionCreate {
		t.Fatalf("plan = %+v", p)
	}
	if !strings.Contains(p.Summary(), "networks: 1 to create") {
		t.Fatalf("summary = %s", p.Summary())
	}
}

func TestAConnectorWithoutANetworkBlueprintIsAnError(t *testing.T) {
	w := netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}})
	w.Blueprints = []Blueprint{nicBlueprint}
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}), w)
	if p.OK() {
		t.Fatal("planned a network nothing can create")
	}
}

// A machine on a bridge the manifest does not declare is not held: the
// bridge is the operator's, and only declared networks are Sybil's to wait on.
func TestAnUndeclaredBridgeIsJustTheNIC(t *testing.T) {
	r := node(1)
	r.Network = "vmbr1"
	p := Build(netEnv(nil, r), netWorld(nil))
	if p.Entries[0].Action != ActionCreate || p.Entries[0].Inputs["net0"] != "virtio,bridge=vmbr1" {
		t.Fatalf("entry = %+v", p.Entries[0])
	}
}

func contains(list []string, s string) bool {
	for _, x := range list {
		if x == s {
			return true
		}
	}
	return false
}

// Configured is not live: a vnet whose bridge is down on a node holds its machines.
func TestANetworkThatIsNotUpOnItsNodeIsNotLive(t *testing.T) {
	sdn := live("10.20.0.0/24", "10.20.0.1", true)
	v := sdn.Vnets["okd"]
	v.NotUp = []string{"pve: error (missing bridge)"}
	sdn.Vnets["okd"] = v
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}, node(1)), netWorld(sdn))
	if p.Networks[0].Action != ActionUnknown || !strings.Contains(p.Networks[0].Reason, "not up on pve") {
		t.Fatalf("network = %+v", p.Networks[0])
	}
	if p.Entries[0].Action != ActionUnknown {
		t.Fatalf("machine = %s", p.Entries[0].Action)
	}
}

// DHCP is a zone setting: a declared range in a zone that serves none is never
// handed out, and a new network cannot join a zone whose DHCP disagrees.
func TestAZoneWithoutDHCPCannotServeADeclaredRange(t *testing.T) {
	sdn := live("10.20.0.0/24", "10.20.0.1", true)
	sdn.ZoneDHCP["sybil"] = ""
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}, node(1)), netWorld(sdn))
	if p.Networks[0].Action != ActionChange || !strings.Contains(p.Networks[0].Reason, "serves no DHCP") {
		t.Fatalf("existing vnet: %+v", p.Networks[0])
	}
	empty := &ObservedSDN{Vnets: map[string]ObservedVnet{}, ZoneDHCP: map[string]string{"sybil": ""}}
	q := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}), netWorld(empty))
	if q.Networks[0].Action != ActionChange || !strings.Contains(q.Networks[0].Reason, "zone setting") {
		t.Fatalf("new vnet in a no-DHCP zone: %+v", q.Networks[0])
	}
}

func TestCreatingADHCPNetworkStatesTheDnsmasqRequirement(t *testing.T) {
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}), netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}}))
	if !contains(p.Limits, DHCPNeedsDnsmasq) {
		t.Fatalf("limits = %v", p.Limits)
	}
	n := okdNet()
	n.DHCPRange = ""
	q := Build(netEnv([]envmanifest.EnvironmentNetwork{n}), netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}}))
	if contains(q.Limits, DHCPNeedsDnsmasq) {
		t.Fatal("stated the dnsmasq requirement for a network without DHCP")
	}
}

func TestARunCreatingANetworkNoLongerDeclaredIsShown(t *testing.T) {
	runID := uuid.New()
	w := netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}})
	w.InFlight = []Run{{ID: runID, Intent: &RealizationIntent{
		Kind: IntentKind, Name: "staging", ResourceName: NetworkResourcePrefix + "gone"}}}
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{okdNet()}), w)
	if len(p.Networks) != 2 || p.Networks[1].Name != "gone" || p.Networks[1].Action != ActionUnknown || *p.Networks[1].RunID != runID {
		t.Fatalf("networks = %+v", p.Networks)
	}
}
