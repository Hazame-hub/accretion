package plan

import (
	"strings"
	"testing"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

const minted = "0f9c1c9e-4b6a-4d8e-9b1e-3c2d1a0b9f8e"

// sizedBlueprint builds machines whose size can change in place, and names
// the blueprints that change and remove them.
var sizedBlueprint = Blueprint{
	ID: "provision-vm", Name: "Provision VM", ReconfiguredBy: "reconfigure-vm", DestroyedBy: "destroy-vm",
	Inputs: []BlueprintInput{
		{Name: "integration", Type: "string", Required: true, Source: InputSourceIntegrations},
		{Name: "hostname", Type: "string", Required: true},
		{Name: "memory_mb", Type: "int", Default: 2048, InPlace: true},
		{Name: "disk_gb", Type: "int", Default: 120, InPlace: true, GrowOnly: true},
		{Name: "template", Type: "string"},
	},
}

var (
	reconfigureBP = Blueprint{ID: "reconfigure-vm"}
	destroyBP     = Blueprint{ID: "destroy-vm"}
)

func sizedWorld(rs ...Realization) Snapshot {
	w := world(rs...)
	w.Blueprints = []Blueprint{sizedBlueprint, reconfigureBP, destroyBP}
	return w
}

// builtMachine is an accepted machine built with these inputs and the minted identity.
func builtMachine(w *Snapshot, index int, inputs map[string]any) Realization {
	asset := uuid.New()
	r := realized("web", index, &asset)
	r.IntentInputs = inputs
	r.MintedUUID = minted
	r.Provenance.RunbookName = "Provision VM"
	w.Verdicts[asset] = AcceptanceAccepted
	return r
}

func manifest(count int, inputs map[string]any) envmanifest.Environment {
	r := web(count)
	r.Inputs = inputs
	return env(r)
}

func built(hostname string, memory int) map[string]any {
	return map[string]any{"integration": labID.String(), "hostname": hostname, "memory_mb": float64(memory)}
}

func TestAMemoryChangeIsMadeInPlaceUnderTheMintedIdentity(t *testing.T) {
	w := sizedWorld()
	w.Realizations = []Realization{builtMachine(&w, 0, built("web-1", 2048))}
	p := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 4096}), w)

	e := p.Entries[0]
	if e.Action != ActionChange || e.Apply != ApplyReconfigure || e.LifecycleBlueprint != "reconfigure-vm" {
		t.Fatalf("entry = %+v", e)
	}
	if e.MintedUUID != minted || e.BuiltName != "web-1" || e.Blocked != "" {
		t.Fatalf("guards = %q %q, blocked %q", e.MintedUUID, e.BuiltName, e.Blocked)
	}
}

func TestAChangeAnApplyCannotMakeSaysWhy(t *testing.T) {
	cases := []struct {
		name    string
		inputs  map[string]any
		mutate  func(*Realization)
		blocked string
	}{
		{"not in place", map[string]any{"hostname": "web-{{instance.number}}", "template": "rocky-10"}, nil,
			"template cannot change in place"},
		{"a shrink", map[string]any{"hostname": "web-{{instance.number}}", "disk_gb": 10},
			func(r *Realization) { r.IntentInputs["disk_gb"] = float64(20) },
			"never shrunk: disk_gb (20 to 10)"},
		{"no identity", map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 4096},
			func(r *Realization) { r.MintedUUID = "" },
			"no identity was recorded"},
		{"disk never declared", map[string]any{"hostname": "web-{{instance.number}}", "disk_gb": 20}, nil,
			"disk_gb was not declared when the machine was built"},
		{"disk dropped to a smaller default", map[string]any{"hostname": "web-{{instance.number}}"},
			func(r *Realization) { r.IntentInputs["disk_gb"] = float64(200) },
			"never shrunk: disk_gb (200 to"},
		{"unknown builder", map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 4096},
			func(r *Realization) { r.Provenance.RunbookName = "hand-made" },
			"cannot tell which blueprint built"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			w := sizedWorld()
			r := builtMachine(&w, 0, built("web-1", 2048))
			if c.mutate != nil {
				c.mutate(&r)
			}
			w.Realizations = []Realization{r}
			e := Build(manifest(1, c.inputs), w).Entries[0]
			if e.Action != ActionChange || e.Apply != "" || !strings.Contains(e.Blocked, c.blocked) {
				t.Fatalf("entry action=%s apply=%q blocked=%q, want blocked containing %q", e.Action, e.Apply, e.Blocked, c.blocked)
			}
		})
	}
}

// A disk grown to "0" means leave it as it is, which is never a shrink.
func TestADiskOfZeroIsNotAShrink(t *testing.T) {
	if shrinksValue(float64(20), 0) || shrinksValue(nil, 30) || !shrinksValue(float64(20), 10) {
		t.Fatal("shrinksValue disagrees with grow-only")
	}
}

func TestAMachineNoLongerDeclaredIsRemovedOnlyWhenItsIdentityIsKnown(t *testing.T) {
	w := sizedWorld()
	keep := builtMachine(&w, 0, built("web-1", 2048))
	gone := builtMachine(&w, 1, built("web-2", 2048))
	unproven := builtMachine(&w, 2, built("web-3", 2048))
	unproven.MintedUUID = ""
	elsewhere := builtMachine(&w, 3, built("web-4", 2048))
	elsewhere.IntegrationID = otherID
	w.Realizations = []Realization{keep, gone, unproven, elsewhere}

	p := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048}), w)
	byIndex := map[int]Entry{}
	for _, e := range p.Entries {
		byIndex[e.Index] = e
	}
	if e := byIndex[1]; e.Action != ActionDelete || e.Apply != ApplyDestroy || e.LifecycleBlueprint != "destroy-vm" ||
		e.MintedUUID != minted || e.BuiltName != "web-2" {
		t.Fatalf("web/1 = %+v", e)
	}
	if e := byIndex[2]; e.Apply != "" || !strings.Contains(e.Blocked, "no identity was recorded") {
		t.Fatalf("web/2 without identity = %+v", e)
	}
	if e := byIndex[3]; e.Apply != "" || !strings.Contains(e.Blocked, "another connector") {
		t.Fatalf("web/3 elsewhere = %+v", e)
	}
}

// A run already working on a machine is never joined by a second one.
func TestAMachineARunIsWorkingOnIsUnknownNotChangedAgain(t *testing.T) {
	w := sizedWorld()
	changing := builtMachine(&w, 0, built("web-1", 2048))
	removing := builtMachine(&w, 1, built("web-2", 2048))
	w.Realizations = []Realization{changing, removing}
	for i := 0; i < 2; i++ {
		w.InFlight = append(w.InFlight, Run{ID: uuid.New(),
			Intent: &RealizationIntent{Kind: IntentKind, Name: "staging", ResourceName: "web", InstanceIndex: i}})
	}
	p := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 4096}), w)
	for _, e := range p.Entries {
		if e.Action != ActionUnknown || e.Apply != "" || e.RunID == nil {
			t.Fatalf("entry %s/%d = %s apply %q, want unknown with its run", e.Resource, e.Index, e.Action, e.Apply)
		}
	}
}

// Without provenance there is no telling how the machine was built, and so
// how to remove it: blocked, never guessed from the catalogue.
func TestAMachineOfUnknownMakingIsNotRemoved(t *testing.T) {
	w := sizedWorld()
	keep := builtMachine(&w, 0, built("web-1", 2048))
	gone := builtMachine(&w, 1, built("web-2", 2048))
	gone.Provenance.RunbookName = ""
	w.Realizations = []Realization{keep, gone}
	p := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048}), w)
	for _, e := range p.Entries {
		if e.Index == 1 && (e.Apply != "" || !strings.Contains(e.Blocked, "cannot tell which blueprint built")) {
			t.Fatalf("web/1 = %+v", e)
		}
	}
}

// A machine whose change in place was rejected is changed again, not stuck:
// the reconfigure's own evaluation decides.
func TestARejectedMachineDeclaredDifferentlyIsChangedAgain(t *testing.T) {
	w := sizedWorld()
	r := builtMachine(&w, 0, built("web-1", 2048))
	w.Verdicts[*r.AssetID] = AcceptanceRejected
	w.Realizations = []Realization{r}
	e := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 3072}), w).Entries[0]
	if e.Action != ActionChange || e.Apply != ApplyReconfigure {
		t.Fatalf("rejected and declared differently: %+v", e)
	}
	// Rejected with nothing declared differently: a person's call.
	e = Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048}), w).Entries[0]
	if e.Action != ActionChange || e.Apply != "" || e.Blocked == "" {
		t.Fatalf("rejected, same declaration: %+v", e)
	}
}

// Count 0 and an Environment declaring nothing are how one is emptied: every
// machine built for it plans as a removal an apply makes, and nothing else
// stands in the way — no error, no create.
func TestAnEmptiedEnvironmentPlansEveryMachineForRemoval(t *testing.T) {
	for _, tc := range []struct {
		name string
		env  envmanifest.Environment
	}{
		{"count 0", manifest(0, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048})},
		{"nothing declared", manifest(1, nil).Emptied()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := sizedWorld()
			w.Realizations = []Realization{
				builtMachine(&w, 0, built("web-1", 2048)), builtMachine(&w, 1, built("web-2", 2048)),
			}
			p := Build(tc.env, w)
			if !p.OK() {
				t.Fatalf("errors = %v", p.Errors)
			}
			if c := p.Counts(); c[ActionDelete] != 2 || len(p.Entries) != 2 {
				t.Fatalf("counts = %v, entries %+v", c, p.Entries)
			}
			for _, e := range p.Entries {
				if e.Apply != ApplyDestroy || e.LifecycleBlueprint != "destroy-vm" || e.Blocked != "" {
					t.Errorf("%s/%d = apply %q blocked %q", e.Resource, e.Index, e.Apply, e.Blocked)
				}
			}
		})
	}
}
