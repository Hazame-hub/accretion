package plan

import (
	"strings"
	"testing"

	"github.com/google/uuid"
)

func poweredWorld(states map[int]string) (Snapshot, []uuid.UUID) {
	bp := sizedBlueprint
	bp.PoweredOnBy, bp.PoweredOffBy = "start-vm", "shutdown-vm"
	w := world()
	w.Blueprints = []Blueprint{bp, reconfigureBP, destroyBP,
		{ID: "start-vm"}, {ID: "shutdown-vm"}}
	w.Power = map[uuid.UUID]string{}
	var assets []uuid.UUID
	for i := 0; i < len(states); i++ {
		r := builtMachine(&w, i, built("web-"+string(rune('1'+i)), 2048))
		w.Realizations = append(w.Realizations, r)
		assets = append(assets, *r.AssetID)
		if s := states[i]; s != "" {
			w.Power[*r.AssetID] = s
		}
	}
	return w, assets
}

func TestAMachineNotPoweredAsDeclaredIsAChangeAnApplyMakes(t *testing.T) {
	w, _ := poweredWorld(map[int]string{0: "running", 1: "stopped", 2: "suspended"})
	e := manifest(3, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048})
	e.Spec.Resources[0].Power = "off"
	p := Build(e, w)

	if en := p.Entries[0]; en.Action != ActionChange || en.Apply != ApplyPower || en.Power != "off" ||
		en.LifecycleBlueprint != "shutdown-vm" || en.MintedUUID != minted {
		t.Fatalf("running, declared off: %+v", en)
	}
	if en := p.Entries[1]; en.Action != ActionNoOp {
		t.Fatalf("stopped, declared off: %s", en.Action)
	}
	// Suspended is neither: not rounded to off, and the plan says it did not compare.
	if en := p.Entries[2]; en.Action != ActionNoOp || en.Apply != "" {
		t.Fatalf("suspended: %+v", en)
	}
	if !strings.Contains(strings.Join(p.Limits, " "), "power state of some machines was never observed") {
		t.Fatalf("limits = %v", p.Limits)
	}
}

func TestPowerOnUsesTheStartBlueprintAndNeedsTheIdentity(t *testing.T) {
	w, _ := poweredWorld(map[int]string{0: "stopped"})
	w.Realizations[0].MintedUUID = ""
	e := manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048})
	e.Spec.Resources[0].Power = "on"
	en := Build(e, w).Entries[0]
	if en.Action != ActionChange || en.Apply != "" || !strings.Contains(en.Blocked, "no identity") {
		t.Fatalf("without identity: %+v", en)
	}
	w, _ = poweredWorld(map[int]string{0: "stopped"})
	if en := Build(e, w).Entries[0]; en.Apply != ApplyPower || en.LifecycleBlueprint != "start-vm" {
		t.Fatalf("with identity: %+v", en)
	}
	_ = AcceptanceAccepted
}
