package plan

import (
	"strings"
	"testing"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

var placedBlueprint = Blueprint{
	ID: "provision-vm",
	Inputs: []BlueprintInput{
		{Name: "integration", Type: "string", Required: true, Source: InputSourceIntegrations},
		{Name: "pve_node", Type: "string", Required: true, Default: "pve", Selector: NodeSelector},
		{Name: "hostname", Type: "string", Required: true},
		{Name: "memory_mb", Type: "int", Default: 2048},
	},
}

const gi = int64(1) << 30

// A three-node cluster: pve-a has the most free memory, then pve-b, then pve-c.
func cluster(rs ...Realization) Snapshot {
	w := world(rs...)
	w.Blueprints = []Blueprint{placedBlueprint}
	w.Capacity = &Capacity{MemoryBytes: 96 * gi, AllocatedMemoryBytes: 30 * gi, Known: true, Nodes: []NodeCapacity{
		{Name: "pve-a", Online: true, MemoryBytes: 32 * gi, AllocatedMemoryBytes: 4 * gi},
		{Name: "pve-b", Online: true, MemoryBytes: 32 * gi, AllocatedMemoryBytes: 10 * gi},
		{Name: "pve-c", Online: true, MemoryBytes: 32 * gi, AllocatedMemoryBytes: 16 * gi},
	}}
	return w
}

func resource(name string, count int, mib int, p *envmanifest.Placement) envmanifest.EnvironmentResource {
	return envmanifest.EnvironmentResource{Name: name, Count: count, Blueprint: "provision-vm", Placement: p,
		Inputs: map[string]any{"hostname": name + "-{{instance.number}}", "memory_mb": mib}}
}

func auto() *envmanifest.Placement { return &envmanifest.Placement{Node: envmanifest.PlacementAuto} }

func nodes(p Plan) string {
	var parts []string
	for _, e := range p.Entries {
		n, _ := e.Inputs["pve_node"].(string)
		parts = append(parts, instanceName(instanceKey{e.Resource, e.Index})+"="+n)
	}
	return strings.Join(parts, " ")
}

func mustBuild(t *testing.T, e envmanifest.Environment, w Snapshot) Plan {
	t.Helper()
	if err := e.Validate(); err != nil {
		t.Fatalf("manifest: %v", err)
	}
	p := Build(e, w)
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	return p
}

func TestAutoPlacementTakesTheNodeWithTheMostFreeMemory(t *testing.T) {
	p := mustBuild(t, env(resource("web", 1, 4096, auto())), cluster())
	if got := nodes(p); got != "web[0]=pve-a" {
		t.Fatalf("nodes = %s", got)
	}
	note := p.Entries[0].Placement
	if note == nil || !note.Auto || note.Node != "pve-a" || !strings.Contains(note.Reason, "28.0 GiB before this machine, 24.0 GiB after") {
		t.Fatalf("placement = %+v", note)
	}
	if !contains(p.Limits, PlacementByMemory) {
		t.Fatalf("limits = %v", p.Limits)
	}
}

// Each placed machine counts against its node before the next is placed, so
// a batch spreads by memory rather than piling onto the first winner.
func TestEachMachinePlacedCountsAgainstItsNode(t *testing.T) {
	// pve-a 28 free, pve-b 22, pve-c 16. 10 GiB each: a(28→18), b(22→12),
	// a(18→8), c(16→6).
	p := mustBuild(t, env(resource("web", 4, 10240, auto())), cluster())
	if got := nodes(p); got != "web[0]=pve-a web[1]=pve-b web[2]=pve-a web[3]=pve-c" {
		t.Fatalf("nodes = %s", got)
	}
}

func TestSpreadPutsEveryInstanceOnItsOwnNode(t *testing.T) {
	p := mustBuild(t, env(resource("db", 3, 1024, &envmanifest.Placement{Node: envmanifest.PlacementAuto, Spread: true})), cluster())
	if got := nodes(p); got != "db[0]=pve-a db[1]=pve-b db[2]=pve-c" {
		t.Fatalf("nodes = %s", got)
	}
}

// The lab has one node: two instances that must be apart cannot both be
// placed, and the plan says why rather than stacking them.
func TestSpreadWiderThanTheClusterIsAnError(t *testing.T) {
	w := cluster()
	w.Capacity.Nodes = w.Capacity.Nodes[:1]
	p := Build(env(resource("db", 2, 1024, &envmanifest.Placement{Node: envmanifest.PlacementAuto, Spread: true})), w)
	if p.OK() {
		t.Fatal("two instances spread over one node planned without error")
	}
	if len(p.Errors) != 1 || !strings.Contains(p.Errors[0], "db[1]: no node can take it: pve-a holds db[0] (\"db\"'s spread)") {
		t.Fatalf("errors = %v", p.Errors)
	}
}

// apart_from binds both ways: declared on web, it also keeps db off web's node.
func TestApartFromKeepsBothResourcesOffEachOthersNodes(t *testing.T) {
	p := mustBuild(t, env(
		resource("db", 1, 1024, auto()),
		resource("web", 1, 1024, &envmanifest.Placement{Node: envmanifest.PlacementAuto, ApartFrom: []string{"db"}}),
	), cluster())
	if got := nodes(p); got != "db[0]=pve-a web[0]=pve-b" {
		t.Fatalf("nodes = %s", got)
	}
	if !strings.Contains(p.Entries[1].Placement.Reason, "ruled out: pve-a holds db[0]") {
		t.Fatalf("reason = %q", p.Entries[1].Placement.Reason)
	}
}

func TestNodesLimitsWhatAutoMayChoose(t *testing.T) {
	p := mustBuild(t, env(resource("web", 1, 1024, &envmanifest.Placement{Node: envmanifest.PlacementAuto, Nodes: []string{"pve-c"}})), cluster())
	if got := nodes(p); got != "web[0]=pve-c" {
		t.Fatalf("nodes = %s", got)
	}
}

func TestOfflineNodesAreNeverChosen(t *testing.T) {
	w := cluster()
	w.Capacity.Nodes[0].Online = false
	p := mustBuild(t, env(resource("web", 1, 1024, auto())), w)
	if got := nodes(p); got != "web[0]=pve-b" {
		t.Fatalf("nodes = %s", got)
	}
}

// A machine that exists keeps its node, so a plan with placement never shows
// a move — even when another node now has more room.
func TestABuiltMachineKeepsItsNode(t *testing.T) {
	asset := ptr(labID)
	r := realized("web", 0, asset)
	r.IntentInputs = map[string]any{"integration": labID.String(), "hostname": "web-1", "memory_mb": 4096, "pve_node": "pve-c"}
	w := cluster(r)
	w.Verdicts[*asset] = AcceptanceAccepted
	p := mustBuild(t, env(resource("web", 2, 4096, auto())), w)
	if got := actions(p); got != "web/0=no-op web/1=create" {
		t.Fatalf("actions = %s (%v)", got, p.Entries[0].Diff)
	}
	if got := nodes(p); got != "web[0]=pve-c web[1]=pve-a" {
		t.Fatalf("nodes = %s", got)
	}
}

// A machine built before its resource had a placement recorded no node: it is
// on the blueprint's default, and adding placement must not plan a change.
func TestAddingPlacementToABuiltResourcePlansNoChange(t *testing.T) {
	asset := ptr(labID)
	r := realized("web", 0, asset)
	r.IntentInputs = map[string]any{"integration": labID.String(), "hostname": "web-1", "memory_mb": 4096}
	w := cluster(r)
	w.Verdicts[*asset] = AcceptanceAccepted
	p := mustBuild(t, env(resource("web", 1, 4096, auto())), w)
	if got := actions(p); got != "web/0=no-op" {
		t.Fatalf("actions = %s (%v)", got, p.Entries[0].Diff)
	}
	if n := p.Entries[0].Placement; n == nil || n.Node != "pve" || n.Auto {
		t.Fatalf("placement = %+v", n)
	}
}

func TestBuiltMachinesThatBreakARuleWarnAndDoNotFail(t *testing.T) {
	a, b := realized("db", 0, nil), realized("db", 1, nil)
	a.IntentInputs = map[string]any{"pve_node": "pve-a"}
	b.IntentInputs = map[string]any{"pve_node": "pve-a"}
	p := Build(env(resource("db", 2, 1024, &envmanifest.Placement{Node: envmanifest.PlacementAuto, Spread: true})), cluster(a, b))
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	if len(p.Warnings) != 1 || !strings.Contains(p.Warnings[0], "db[0] and db[1] are both on pve-a") ||
		!strings.Contains(p.Warnings[0], "does not move machines") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
}

// A run still building an instance holds its node: the next one is placed
// around it.
func TestARunInFlightHoldsItsNode(t *testing.T) {
	w := cluster()
	w.InFlight = []Run{{
		Intent:       &RealizationIntent{Kind: IntentKind, Name: "staging", ResourceName: "db", InstanceIndex: 0},
		IntentInputs: map[string]any{"pve_node": "pve-a"},
	}}
	p := mustBuild(t, env(resource("db", 2, 1024, &envmanifest.Placement{Node: envmanifest.PlacementAuto, Spread: true})), w)
	if got := nodes(p); got != "db[0]=pve-a db[1]=pve-b" {
		t.Fatalf("nodes = %s", got)
	}
}

func TestAFixedNodeThatBreaksARuleIsAnError(t *testing.T) {
	p := Build(env(
		resource("db", 1, 1024, &envmanifest.Placement{Node: "pve-b"}),
		resource("web", 1, 1024, &envmanifest.Placement{Node: "pve-b", ApartFrom: []string{"db"}}),
	), cluster())
	if len(p.Errors) != 1 || !strings.Contains(p.Errors[0], "db[0] and web[0] would both be on pve-b, which \"web\"'s apart_from forbids") {
		t.Fatalf("errors = %v", p.Errors)
	}
}

// A resource without placement still counts for the rules, on the node its
// inputs or blueprint give it.
func TestAnUnplacedResourceStillCountsForTheRules(t *testing.T) {
	db := resource("db", 1, 1024, nil)
	db.Inputs["pve_node"] = "pve-a"
	p := mustBuild(t, env(db, resource("web", 1, 1024, &envmanifest.Placement{Node: envmanifest.PlacementAuto, ApartFrom: []string{"db"}})), cluster())
	if got := nodes(p); got != "db[0]=pve-a web[0]=pve-b" {
		t.Fatalf("nodes = %s", got)
	}
}

func TestPlacementAndANodeInputTogetherAreRefused(t *testing.T) {
	r := resource("web", 1, 1024, auto())
	r.Inputs["pve_node"] = "pve-a"
	p := Build(env(r), cluster())
	if p.OK() || !strings.Contains(strings.Join(p.Errors, "\n"), "sets inputs.pve_node and a placement") {
		t.Fatalf("errors = %v", p.Errors)
	}
}

func TestAutoPlacementWithoutTheNodesIsAnError(t *testing.T) {
	w := cluster()
	w.Capacity = nil
	w.CapacityError = "this connector does not report its capacity"
	p := Build(env(resource("web", 2, 1024, auto())), w)
	if len(p.Errors) != 1 || !strings.Contains(p.Errors[0], "could not be read: this connector does not report its capacity") {
		t.Fatalf("errors = %v", p.Errors)
	}
}

func TestABlueprintWithoutANodeInputCannotBePlaced(t *testing.T) {
	p := Build(env(resource("web", 1, 1024, auto())), world()) // vmBlueprint has no node input
	if p.OK() || !strings.Contains(strings.Join(p.Errors, "\n"), "has no node input") {
		t.Fatalf("errors = %v", p.Errors)
	}
}

// The chosen node is overcommitted when nothing has room: placed, and said.
func TestAMachineNoNodeHasRoomForIsPlacedAndWarned(t *testing.T) {
	p := mustBuild(t, env(resource("big", 1, 40960, auto())), cluster())
	if got := nodes(p); got != "big[0]=pve-a" {
		t.Fatalf("nodes = %s", got)
	}
	if !strings.Contains(strings.Join(p.Warnings, "\n"), "big[0] goes to pve-a, the node with the most free memory, and overcommits it by 12.0 GiB") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
}

// On a node already promised more than it has, as the lab's is, the warning
// says so rather than blaming the whole overcommit on the new machine.
func TestANodeAlreadyOvercommittedIsSaidToBe(t *testing.T) {
	w := cluster()
	w.Capacity.Nodes = []NodeCapacity{{Name: "pve-a", Online: true, MemoryBytes: 27 * gi, AllocatedMemoryBytes: 69 * gi}}
	p := mustBuild(t, env(resource("db", 1, 2048, auto())), w)
	if !strings.Contains(strings.Join(p.Warnings, "\n"),
		"db[0] goes to pve-a, the least overcommitted node it may use, which is already promised 42.0 GiB more than it has; this machine adds 2.0 GiB") {
		t.Fatalf("warnings = %v", p.Warnings)
	}
	if r := p.Entries[0].Placement.Reason; !strings.Contains(r, "though it has no free memory: it is already promised 42.0 GiB more than it has") {
		t.Fatalf("reason = %q", r)
	}
}

// No placement, no change: a manifest written before placement plans exactly
// as it did.
func TestWithoutPlacementNothingIsChosen(t *testing.T) {
	p := mustBuild(t, env(resource("web", 2, 1024, nil)), cluster())
	for _, e := range p.Entries {
		if _, set := e.Inputs["pve_node"]; set || e.Placement != nil {
			t.Fatalf("entry %+v", e)
		}
	}
	if contains(p.Limits, PlacementByMemory) {
		t.Fatalf("limits = %v", p.Limits)
	}
}

// An image's template is looked up on the node placement chose, not on the
// blueprint's default node: on the lab, the default "pve" is no host at all.
func TestAnImageIsLookedUpOnThePlacedNode(t *testing.T) {
	w := cluster()
	bp := placedBlueprint
	bp.Inputs = append(append([]BlueprintInput(nil), bp.Inputs...),
		BlueprintInput{Name: "template_vmid", Type: "string", Required: true, Selector: "template", TakesImage: true})
	w.Blueprints = []Blueprint{bp}
	w.Selectors = []Selector{templateSelector}
	r := resource("db", 2, 2048, &envmanifest.Placement{Node: envmanifest.PlacementAuto, Spread: true})
	r.Image = "rocky-9"
	e := env(r)

	qs := ImageQueries(e, w)
	var got []string
	for _, q := range qs {
		got = append(got, q.Params)
	}
	if strings.Join(got, " ") != "node=pve-a node=pve-b" {
		t.Fatalf("queries = %v, want one per placed node", got)
	}
	w.Images = map[ImageQuery]ImageAnswer{
		qs[0]: {Value: "9001"},
		qs[1]: {Value: "9002"},
	}
	p := mustBuild(t, e, w)
	if nodes(p) != "db[0]=pve-a db[1]=pve-b" {
		t.Fatalf("nodes = %s", nodes(p))
	}
	for i, want := range []string{"9001", "9002"} {
		if v := p.Entries[i].Inputs["template_vmid"]; v != want {
			t.Errorf("db[%d] template = %v, want %s (the template on its own node)", i, v, want)
		}
	}
}

// A machine no node can take has no node to look its image up on: the plan
// says it could not be placed, and nothing else about it.
func TestAnUnplacedMachineIsOnlyReportedAsUnplaced(t *testing.T) {
	w := cluster()
	w.Capacity.Nodes = w.Capacity.Nodes[:1]
	bp := placedBlueprint
	bp.Inputs = append(append([]BlueprintInput(nil), bp.Inputs...),
		BlueprintInput{Name: "template_vmid", Type: "string", Required: true, Selector: "template", TakesImage: true})
	w.Blueprints = []Blueprint{bp}
	w.Selectors = []Selector{templateSelector}
	r := resource("db", 2, 2048, &envmanifest.Placement{Node: envmanifest.PlacementAuto, Spread: true})
	r.Image = "rocky-9"
	e := env(r)

	qs := ImageQueries(e, w)
	if len(qs) != 1 || qs[0].Params != "node=pve-a" {
		t.Fatalf("queries = %+v, want only db[0]'s, on pve-a", qs)
	}
	w.Images = map[ImageQuery]ImageAnswer{qs[0]: {Value: "9001"}}
	p := Build(e, w)
	if len(p.Errors) != 1 || !strings.Contains(p.Errors[0], "db[1]: no node can take it") {
		t.Fatalf("errors = %q, want only db[1]'s placement", p.Errors)
	}

	// The same for a machine placement.nodes leaves no node for.
	r = resource("db", 1, 2048, &envmanifest.Placement{Node: envmanifest.PlacementAuto, Nodes: []string{"pve-nope"}})
	r.Image = "rocky-9"
	e = env(r)
	if qs := ImageQueries(e, w); len(qs) != 0 {
		t.Fatalf("queries = %+v, want none", qs)
	}
	p = Build(e, w)
	if len(p.Errors) != 1 || !strings.Contains(p.Errors[0], "none of placement.nodes (pve-nope) is a node of this cluster") {
		t.Fatalf("errors = %q, want only db[0]'s placement", p.Errors)
	}
}
