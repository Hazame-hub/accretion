package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// observingWorld is sizedWorld with a blueprint whose sizes an observation can
// confirm, as the Proxmox one now declares.
func observingWorld() Snapshot {
	w := sizedWorld()
	bp := sizedBlueprint
	bp.Inputs = append([]BlueprintInput(nil), sizedBlueprint.Inputs...)
	for i := range bp.Inputs {
		switch bp.Inputs[i].Name {
		case "memory_mb":
			bp.Inputs[i].Observes = ObservedMemoryMiB
		case "disk_gb":
			bp.Inputs[i].Observes = ObservedBootDiskGiB
		}
	}
	bp.Inputs = append(bp.Inputs, BlueprintInput{Name: "cores", Type: "int", Default: 2, InPlace: true, Observes: ObservedCPUCores})
	w.Blueprints = []Blueprint{bp, reconfigureBP, destroyBP}
	return w
}

func observedAs(w *Snapshot, r Realization, values map[string]float64) {
	if w.Observed == nil {
		w.Observed = map[uuid.UUID]ObservedMachine{}
	}
	w.Observed[*r.AssetID] = ObservedMachine{Values: values, State: ObservedRunning}
}

// A machine resized on the hypervisor plans as a change back to the manifest,
// made in place like any other.
func TestAMachineResizedOutsideSybilPlansAsAChangeBack(t *testing.T) {
	w := observingWorld()
	r := builtMachine(&w, 0, built("web-1", 2048))
	w.Realizations = []Realization{r}
	observedAs(&w, r, map[string]float64{ObservedMemoryMiB: 4096, ObservedCPUCores: 2})

	p := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048}), w)
	e := p.Entries[0]
	if e.Action != ActionChange || !e.Drift || e.Apply != ApplyReconfigure {
		t.Fatalf("entry = %+v; want a drift change made in place", e)
	}
	if len(e.Diff) != 1 || e.Diff[0].Input != "memory_mb" || e.Diff[0].From != float64(4096) {
		t.Fatalf("diff = %+v; want memory_mb from the observed 4096", e.Diff)
	}
	if !strings.Contains(e.Reason, "changed outside Sybil") {
		t.Errorf("reason = %q", e.Reason)
	}
}

// The change in place sets undeclared inputs to the blueprint's default. When
// the machine drifted on those too, the plan says it will put them back,
// rather than doing it unannounced.
func TestADriftChangeSaysWhatElseItWillSet(t *testing.T) {
	w := observingWorld()
	r := builtMachine(&w, 0, built("web-1", 2048))
	w.Realizations = []Realization{r}
	observedAs(&w, r, map[string]float64{ObservedMemoryMiB: 4096, ObservedCPUCores: 6})

	e := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048}), w).Entries[0]
	if len(e.Diff) != 2 || e.Diff[0].Input != "cores" || e.Diff[0].From != float64(6) || e.Diff[0].To != 2 {
		t.Fatalf("diff = %+v; want cores 6 -> default 2 listed beside memory_mb", e.Diff)
	}
	if !strings.Contains(e.Reason, "cores is 6 and would be set to the default 2") {
		t.Errorf("reason = %q", e.Reason)
	}

	// Undeclared and different, with nothing declared drifting: no change at
	// all — the manifest does not ask for cores, and nothing is being changed.
	w2 := observingWorld()
	r2 := builtMachine(&w2, 0, built("web-1", 2048))
	w2.Realizations = []Realization{r2}
	observedAs(&w2, r2, map[string]float64{ObservedMemoryMiB: 2048, ObservedCPUCores: 6})
	if e := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048}), w2).Entries[0]; e.Action != ActionNoOp {
		t.Fatalf("entry = %+v; an undeclared input alone is not drift", e)
	}
}

// Agreeing, rounding included, is no drift; nor is a fact never observed, nor
// an input declared as zero ("keep the template's size").
func TestNoDriftWhenTheMachineAgreesOrWasNotObserved(t *testing.T) {
	cases := map[string]struct {
		inputs map[string]any
		obs    map[string]float64
	}{
		"agrees":            {map[string]any{"memory_mb": 2048}, map[string]float64{ObservedMemoryMiB: 2047.9}},
		"not observed":      {map[string]any{"memory_mb": 2048}, map[string]float64{ObservedBootDiskGiB: 20}},
		"disk kept at zero": {map[string]any{"memory_mb": 2048, "disk_gb": 0}, map[string]float64{ObservedMemoryMiB: 2048, ObservedBootDiskGiB: 24}},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			w := observingWorld()
			r := builtMachine(&w, 0, built("web-1", 2048))
			if d, ok := tc.inputs["disk_gb"]; ok {
				r.IntentInputs["disk_gb"] = d
			}
			w.Realizations = []Realization{r}
			observedAs(&w, r, tc.obs)
			in := map[string]any{"hostname": "web-{{instance.number}}"}
			for k, v := range tc.inputs {
				in[k] = v
			}
			e := Build(manifest(1, in), w).Entries[0]
			if e.Action != ActionNoOp || e.Drift {
				t.Fatalf("entry = %+v; want no-op", e)
			}
		})
	}
}

// disk_gb means "at least": a disk bigger than declared — a bigger template,
// or one grown outside Sybil — satisfies it. Only a smaller one is drift, and
// growing it back is a change an apply can make.
func TestABootDiskIsAtLeastNotExactly(t *testing.T) {
	inputs := built("web-1", 2048)
	inputs["disk_gb"] = float64(20)
	declared := map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048, "disk_gb": 20}

	w := observingWorld()
	r := builtMachine(&w, 0, inputs)
	w.Realizations = []Realization{r}
	observedAs(&w, r, map[string]float64{ObservedMemoryMiB: 2048, ObservedBootDiskGiB: 24})
	if e := Build(manifest(1, declared), w).Entries[0]; e.Action != ActionNoOp || e.Drift {
		t.Fatalf("bigger disk: entry = %+v; want no-op", e)
	}

	w = observingWorld()
	r = builtMachine(&w, 0, inputs)
	w.Realizations = []Realization{r}
	observedAs(&w, r, map[string]float64{ObservedMemoryMiB: 2048, ObservedBootDiskGiB: 16})
	e := Build(manifest(1, declared), w).Entries[0]
	if e.Action != ActionChange || !e.Drift || e.Apply != ApplyReconfigure {
		t.Fatalf("smaller disk: entry = %+v; want a drift change that grows it back", e)
	}
}

// A stopped machine that drifted is not changed in place: the change reboots
// it and waits for it, which a stopped machine never answers.
func TestDriftOnAStoppedMachineIsSaidNotAttempted(t *testing.T) {
	w := observingWorld()
	r := builtMachine(&w, 0, built("web-1", 2048))
	w.Realizations = []Realization{r}
	w.Observed = map[uuid.UUID]ObservedMachine{*r.AssetID: {Values: map[string]float64{ObservedMemoryMiB: 4096}, State: "stopped"}}

	e := Build(manifest(1, map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": 2048}), w).Entries[0]
	if e.Action != ActionChange || !e.Drift || e.Apply != "" || !strings.Contains(e.Blocked, "while stopped") {
		t.Fatalf("entry = %+v; want drift that is blocked until the machine runs", e)
	}
}

func TestObservedFromProviderReadsTheFacts(t *testing.T) {
	raw := json.RawMessage(`{"facts":{"cpu_cores":2,"ram_bytes":3221225472,"disks":[{"mount":"scsi0","total":21474836480}]},
		"memory":{"max_bytes":3221225472},"state":"running","observed_at":"2026-10-17T09:21:35Z"}`)
	m, ok := ObservedFromProvider(raw)
	if !ok || m.Values[ObservedCPUCores] != 2 || m.Values[ObservedMemoryMiB] != 3072 || m.Values[ObservedBootDiskGiB] != 20 || m.State != "running" {
		t.Fatalf("observed = %+v, %v", m, ok)
	}
	two := json.RawMessage(`{"facts":{"cpu_cores":2,"disks":[{"total":1},{"total":2}]}}`)
	if m, _ := ObservedFromProvider(two); m.Values[ObservedBootDiskGiB] != 0 {
		t.Errorf("with two disks the boot disk was guessed: %+v", m)
	}
	if _, ok := ObservedFromProvider(nil); ok {
		t.Error("no observation read as one")
	}
	if _, ok := asNumber("4G"); ok {
		t.Error(`"4G" read as a number`)
	}
}
