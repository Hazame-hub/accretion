package plan

import (
	"strings"
	"testing"
)

// adoptedMachine is a machine Sybil did not build, adopted as provision-vm by
// a run of the adoption runbook: its provenance names that runbook, which is
// not the blueprint that changes it.
func adoptedMachine(w *Snapshot, inputs map[string]any) Realization {
	r := builtMachine(w, 0, inputs)
	r.Provenance.RunbookName = "Adopt an existing VM"
	r.AdoptedAs = sizedBlueprint.ID
	return r
}

// The adopted machine was seen with 2048 MB; the manifest wants 4096. That is
// a change in place, made by the blueprint it was adopted as, under the
// identity the adoption recorded.
func TestAnAdoptedMachineIsChangedAsTheBlueprintItWasAdoptedAs(t *testing.T) {
	w := sizedWorld()
	w.Realizations = []Realization{adoptedMachine(&w, built("legacy-web", 2048))}
	p := Build(manifest(1, map[string]any{"hostname": "legacy-web", "memory_mb": 4096}), w)

	e := p.Entries[0]
	if e.Action != ActionChange || e.Apply != ApplyReconfigure || e.LifecycleBlueprint != "reconfigure-vm" {
		t.Fatalf("entry = %+v", e)
	}
	if e.MintedUUID != minted || e.BuiltName != "legacy-web" {
		t.Errorf("guards = %q %q", e.MintedUUID, e.BuiltName)
	}
}

// Seen as declared: nothing to do.
func TestAnAdoptedMachineThatMatchesItsDeclarationIsANoOp(t *testing.T) {
	w := sizedWorld()
	w.Realizations = []Realization{adoptedMachine(&w, built("legacy-web", 2048))}
	p := Build(manifest(1, map[string]any{"hostname": "legacy-web", "memory_mb": 2048}), w)
	if e := p.Entries[0]; e.Action != ActionNoOp {
		t.Fatalf("entry = %+v", e)
	}
}

// Its name is part of what it was seen as: a manifest naming it otherwise is
// a difference an apply cannot make in place, said as such.
func TestAnAdoptedMachineNamedOtherwiseIsAChangeNobodyMakesSilently(t *testing.T) {
	w := sizedWorld()
	w.Realizations = []Realization{adoptedMachine(&w, built("legacy-web", 2048))}
	e := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048}), w).Entries[0]
	if e.Action != ActionChange || e.Apply != "" || !strings.Contains(e.Blocked, "hostname cannot change in place") {
		t.Fatalf("entry = %+v", e)
	}
}

// What nothing observes on a machine is said, not passed off as compared.
func TestAPlanSaysWhatItTookOnTrustForAnAdoptedMachine(t *testing.T) {
	w := sizedWorld()
	inputs := built("legacy-web", 2048)
	inputs["template"] = "rocky-9"
	w.Realizations = []Realization{adoptedMachine(&w, inputs)}
	p := Build(manifest(1, map[string]any{"hostname": "legacy-web", "memory_mb": 2048, "template": "rocky-9"}), w)
	if !hasLimit(p, AdoptedInputsAssumed) {
		t.Fatalf("limits = %v", p.Limits)
	}
	// A machine Sybil built says nothing of the kind.
	w2 := sizedWorld()
	w2.Realizations = []Realization{builtMachine(&w2, 0, inputs)}
	if p := Build(manifest(1, map[string]any{"hostname": "legacy-web", "memory_mb": 2048, "template": "rocky-9"}), w2); hasLimit(p, AdoptedInputsAssumed) {
		t.Fatalf("a built machine's plan claims adoption: %v", p.Limits)
	}
}

func hasLimit(p Plan, limit string) bool {
	for _, l := range p.Limits {
		if l == limit {
			return true
		}
	}
	return false
}
