package plan

import (
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hazame-hub/accretion/provider"
)

// The facts a blueprint input can be checked against (BlueprintInput.Observes),
// in the units the inputs that set them use.
const (
	ObservedCPUCores    = "cpu_cores"
	ObservedMemoryMiB   = "memory_mib"
	ObservedBootDiskGiB = "boot_disk_gib"
)

// ObservedName is the input that names the machine. A name is not a size,
// so drift never compares it; it is read when a machine made by hand is
// adopted, whose recorded inputs are what was seen of it.
const ObservedName = "name"

// ObservedRunning is the provider state in which a machine can be changed in
// place: the reconfigure blueprints reboot it and wait for it to come back.
const ObservedRunning = "running"

// ObservedMachine is a machine's size as the provider last reported it.
type ObservedMachine struct {
	// Values holds the facts that were reported, by Observed* name. A fact
	// the observation did not carry, or carried ambiguously, is absent.
	Values map[string]float64
	// State is the provider's power state ("running", "stopped", ...), ""
	// when it was not reported.
	State string
	At    time.Time
}

// ObservedFromProvider reads an asset's stored provider observation into the
// facts the planner compares. ok is false when there is nothing usable.
//
// The boot disk is read only when exactly one disk is reported: with several,
// which one an input sizes is the provider's convention, and guessing it would
// manufacture a difference — or hide one.
func ObservedFromProvider(raw json.RawMessage) (ObservedMachine, bool) {
	if len(raw) == 0 {
		return ObservedMachine{}, false
	}
	var o provider.Observation
	if err := json.Unmarshal(raw, &o); err != nil {
		return ObservedMachine{}, false
	}
	m := ObservedMachine{Values: map[string]float64{}, State: o.State, At: o.ObservedAt}
	if o.Facts.CPUCores > 0 {
		m.Values[ObservedCPUCores] = float64(o.Facts.CPUCores)
	}
	mem := o.Memory.MaxBytes
	if mem == 0 {
		mem = o.Facts.RAMBytes
	}
	if mem > 0 {
		m.Values[ObservedMemoryMiB] = float64(mem) / (1 << 20)
	}
	if len(o.Facts.Disks) == 1 && o.Facts.Disks[0].Total > 0 {
		m.Values[ObservedBootDiskGiB] = float64(o.Facts.Disks[0].Total) / (1 << 30)
	}
	if len(m.Values) == 0 {
		return ObservedMachine{}, false
	}
	return m, true
}

// observedDrift compares an instance's declared inputs with what the provider
// last observed of its machine, for the inputs its blueprint says an
// observation can confirm. Each difference is an InputDiff from the observed
// value to the declared one, sorted by input.
//
// Values are compared rounded to whole units, as they are declared: a 2 GiB
// machine reported as 2047.9 MiB is not drift. A grow-only input means "at
// least" — a template disk already bigger than declared satisfies it — so only
// a smaller observed value is drift. An input declared as zero ("keep the
// template's size") is not compared.
//
// When there is drift, the inputs the manifest leaves undeclared are listed
// too where the change would set them: the change in place runs with the
// blueprint's default for each, so a machine resized outside Sybil on two
// counts, of which the manifest declares one, would have the other put back
// to the default without the plan saying so.
func observedDrift(e Entry, world Snapshot) (drift, alsoSet []InputDiff) {
	if e.AssetID == nil || world.Observed == nil {
		return nil, nil
	}
	obs, ok := world.Observed[*e.AssetID]
	if !ok {
		return nil, nil
	}
	bp := blueprintByID(world.Blueprints, e.Blueprint)
	if bp == nil {
		return nil, nil
	}
	for _, in := range bp.Inputs {
		if in.Observes == "" {
			continue
		}
		have, observed := obs.Values[in.Observes]
		if !observed {
			continue
		}
		raw, declared := e.Inputs[in.Name]
		if !declared {
			if def, isNumber := asNumber(in.Default); isNumber && def != 0 && !satisfies(have, def, in.GrowOnly) {
				alsoSet = append(alsoSet, InputDiff{Input: in.Name, From: math.Round(have), To: in.Default})
			}
			continue
		}
		want, isNumber := asNumber(raw)
		if !isNumber || want == 0 || satisfies(have, want, in.GrowOnly) {
			continue
		}
		drift = append(drift, InputDiff{Input: in.Name, From: math.Round(have), To: raw})
	}
	if len(drift) == 0 {
		return nil, nil
	}
	return drift, alsoSet
}

// driftDiff is a drift entry's Diff: what drifted and what the change would
// also set, sorted by input.
func driftDiff(drift, alsoSet []InputDiff) []InputDiff {
	out := append(append([]InputDiff(nil), drift...), alsoSet...)
	sort.Slice(out, func(i, j int) bool { return out[i].Input < out[j].Input })
	return out
}

// satisfies says whether an observed value meets a declared one: equal in
// whole units, or for a grow-only input at least as big.
func satisfies(have, want float64, growOnly bool) bool {
	if growOnly {
		return math.Round(have) >= math.Round(want)
	}
	return math.Round(have) == math.Round(want)
}

// describeDrift says what moved, and what else the change would set, for an
// entry's reason.
func describeDrift(drift, alsoSet []InputDiff) string {
	parts := make([]string, 0, len(drift)+len(alsoSet))
	for _, d := range drift {
		parts = append(parts, fmt.Sprintf("%s is %v, the manifest declares %v", d.Input, d.From, d.To))
	}
	for _, d := range alsoSet {
		parts = append(parts, fmt.Sprintf("%s is %v and would be set to the default %v, as the manifest does not declare it", d.Input, d.From, d.To))
	}
	return strings.Join(parts, "; ")
}

// asNumber reads a declared input as a number: an int from the manifest, a
// float64 from JSON, or a string that is a number and nothing else.
func asNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case float64:
		return n, true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return f, err == nil
	}
	return 0, false
}
