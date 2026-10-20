package plan

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

func backNet() envmanifest.EnvironmentNetwork {
	return envmanifest.EnvironmentNetwork{Name: "back", Zone: "sybil", Subnet: "10.30.0.0/24", Gateway: "10.30.0.1"}
}

func onBack(count int) envmanifest.EnvironmentResource {
	return envmanifest.EnvironmentResource{Name: "app", Count: count, Blueprint: "provision-vm", Network: "back",
		Inputs: map[string]any{"hostname": "app-{{instance.number}}"}}
}

func liveBack() *ObservedSDN {
	return &ObservedSDN{Vnets: map[string]ObservedVnet{
		"back": {Zone: "sybil", Subnets: []ObservedSubnet{{CIDR: "10.30.0.0/24", Gateway: "10.30.0.1"}}},
	}, ZoneDHCP: map[string]string{"sybil": ""}}
}

func allowed() *NetworkAccess {
	return &NetworkAccess{Networks: map[string]NetworkGrants{}, Bridges: map[string][]MissingPrivilege{}}
}

func TestAMissingSDNUseOnAVnetIsAPlanError(t *testing.T) {
	w := netWorld(liveBack())
	w.NetworkAccess = allowed()
	w.NetworkAccess.Bridges["back"] = []MissingPrivilege{{Privilege: "SDN.Use", Path: "/sdn/zones/sybil/back"}}
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{backNet()}, onBack(2)), w)
	if p.OK() {
		t.Fatal("a plan whose machines cannot be attached to their network must not be applicable")
	}
	want := `token cannot attach machines to network "back" (resource "app"): it lacks SDN.Use on /sdn/zones/sybil/back; grant it to the token's role`
	if !contains(p.Errors, want) {
		t.Fatalf("errors = %v\nwant %q", p.Errors, want)
	}
	if len(p.Errors) != 1 {
		t.Fatalf("two instances on one bridge are one mistake: %v", p.Errors)
	}
}

// What the provider reports as held — directly or through a parent path — is
// no error, and the plan states no limit either: it did check.
func TestHeldNetworkPrivilegesPlanCleanly(t *testing.T) {
	w := netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}, ZoneDHCP: map[string]string{}})
	w.NetworkAccess = allowed()
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{backNet()}, onBack(1)), w)
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	for _, l := range p.Limits {
		if strings.HasPrefix(l, NetworkAccessNotChecked) {
			t.Fatalf("limits = %v", p.Limits)
		}
	}
}

func TestAMissingAllocateIsAPlanErrorForANetworkToCreate(t *testing.T) {
	w := netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}, ZoneDHCP: map[string]string{}})
	w.NetworkAccess = allowed()
	w.NetworkAccess.Networks["back"] = NetworkGrants{
		Zone:    []MissingPrivilege{{Privilege: "SDN.Allocate", Path: "/sdn/zones"}},
		Network: []MissingPrivilege{{Privilege: "SDN.Allocate", Path: "/sdn/zones/sybil"}},
	}
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{backNet()}), w)
	want := `token cannot create network "back": it lacks SDN.Allocate on /sdn/zones and SDN.Allocate on /sdn/zones/sybil; grant them to the token's role`
	if !contains(p.Errors, want) {
		t.Fatalf("errors = %v\nwant %q", p.Errors, want)
	}
}

// The zone exists, so nothing creates it, and what creating it would need is
// not asked of the token.
func TestAnExistingZoneNeedsNoAllocateOnZones(t *testing.T) {
	w := netWorld(&ObservedSDN{Vnets: map[string]ObservedVnet{}, ZoneDHCP: map[string]string{"sybil": ""}})
	w.NetworkAccess = allowed()
	w.NetworkAccess.Networks["back"] = NetworkGrants{Zone: []MissingPrivilege{{Privilege: "SDN.Allocate", Path: "/sdn/zones"}}}
	if p := Build(netEnv([]envmanifest.EnvironmentNetwork{backNet()}), w); !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
}

// Could not read is never "holds none": a limit, and the plan stays applicable.
func TestUnreadablePermissionsAreALimitNotAnError(t *testing.T) {
	w := netWorld(liveBack())
	w.NetworkAccessError = "this connector does not report its token's permissions"
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{backNet()}, onBack(1)), w)
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	found := false
	for _, l := range p.Limits {
		if strings.HasPrefix(l, NetworkAccessNotChecked) && strings.Contains(l, "does not report") {
			found = true
		}
	}
	if !found {
		t.Fatalf("limits = %v", p.Limits)
	}
}

func TestAManifestWithoutNetworksChecksNothing(t *testing.T) {
	if nets, bridges := NetworkAccessQuery(env(web(2))); len(nets) != 0 || len(bridges) != 0 {
		t.Fatalf("query = %v %v", nets, bridges)
	}
	p := Build(env(web(2)), world())
	for _, l := range p.Limits {
		if strings.HasPrefix(l, NetworkAccessNotChecked) {
			t.Fatalf("limits = %v", p.Limits)
		}
	}
}

// A bridge named in the NIC input itself is attached to all the same.
func TestTheQueryReadsBridgesFromTheNICInput(t *testing.T) {
	r := web(1)
	r.Inputs = map[string]any{"hostname": "web", "net0": "virtio,bridge=vmbr1,firewall=1"}
	e := netEnv([]envmanifest.EnvironmentNetwork{backNet()}, r, onBack(1))
	nets, bridges := NetworkAccessQuery(e)
	if len(nets) != 1 || strings.Join(bridges, ",") != "back,vmbr1" {
		t.Fatalf("query = %v %v", nets, bridges)
	}
}

// A machine already built and accepted is attached to nothing by this apply,
// so a privilege the token has lost since does not block the plan.
func TestASettledMachineIsNotHeldToAttachPrivileges(t *testing.T) {
	asset := uuid.New()
	r := realized("app", 0, &asset)
	r.IntentInputs = map[string]any{"hostname": "app-1", "net0": "virtio,bridge=back", "integration": labID.String()}
	w := netWorld(liveBack())
	w.Realizations = []Realization{r}
	w.Verdicts[asset] = AcceptanceAccepted
	w.NetworkAccess = allowed()
	w.NetworkAccess.Bridges["back"] = []MissingPrivilege{{Privilege: "SDN.Use", Path: "/sdn/zones/sybil/back"}}
	p := Build(netEnv([]envmanifest.EnvironmentNetwork{backNet()}, onBack(1)), w)
	if got := actions(p); got != "app/0=no-op" {
		t.Fatalf("actions = %s", got)
	}
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
}

// A machine's second card is attached as surely as its first, so its bridge
// is asked about and held to the same privilege.
func TestALaterCardsBridgeIsCheckedToo(t *testing.T) {
	r := onBack(1)
	r.ExtraNICs = []envmanifest.EnvironmentNIC{{Network: "vmbr1"}}
	env := netEnv([]envmanifest.EnvironmentNetwork{backNet()}, r)
	if _, bridges := NetworkAccessQuery(env); strings.Join(bridges, ",") != "back,vmbr1" {
		t.Fatalf("bridges = %v", bridges)
	}
	w := twoCardWorld(liveBack())
	w.NetworkAccess = allowed()
	w.NetworkAccess.Bridges["vmbr1"] = []MissingPrivilege{{Privilege: "SDN.Use", Path: "/sdn/zones/localnetwork/vmbr1"}}
	p := Build(env, w)
	if !contains(p.Errors, `token cannot attach machines to network "vmbr1" (resource "app"): it lacks SDN.Use on /sdn/zones/localnetwork/vmbr1; grant it to the token's role`) {
		t.Fatalf("errors = %v", p.Errors)
	}
}
