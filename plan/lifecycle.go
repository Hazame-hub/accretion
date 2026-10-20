package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// What an apply does about a change or a delete.
//
// A plan always said change and delete; an apply acted on neither. Now an
// apply changes a machine in place when every input that differs is one its
// blueprint can change in place, and removes a machine the manifest no longer
// declares. Both are refused, and the entry says why, whenever Sybil cannot
// prove it would be acting on the machine it built: no identity was recorded
// for it, or it was built through another connector. A change no blueprint can
// make in place would mean replacing the machine, which an apply does not do.
const (
	// ApplyReconfigure changes the machine in place with Entry.LifecycleBlueprint.
	ApplyReconfigure = "reconfigure"
	// ApplyDestroy removes the machine with Entry.LifecycleBlueprint.
	ApplyDestroy = "destroy"
	// ApplyPower starts or shuts down the machine (Entry.Power) with
	// Entry.LifecycleBlueprint.
	ApplyPower = "power"
)

// decideLifecycle sets Apply or Blocked on every change and delete entry.
func decideLifecycle(entries []Entry, world Snapshot) {
	byID := map[uuid.UUID]Realization{}
	for _, r := range world.Realizations {
		byID[r.ID] = r
	}
	for i := range entries {
		e := &entries[i]
		if (e.Action != ActionChange && e.Action != ActionDelete) || e.RealizationID == nil {
			continue
		}
		r, ok := byID[*e.RealizationID]
		if !ok {
			e.Blocked = "the machine's record could not be read"
			continue
		}
		creator := builtBy(r, world.Blueprints)
		if e.Action == ActionChange {
			decideChange(e, r, creator, world)
		} else {
			decideDelete(e, r, creator, world)
		}
	}
}

// builtBy is the blueprint that built a machine, by its provenance: the
// runbook a shipped blueprint becomes carries its name. Not the blueprint the
// manifest declares now — that is what the machine should be, not what made
// it, and an agentless machine changed by the agent's blueprint would wait
// for an agent that never comes. Nil when the provenance names none; the
// entry is then blocked, never guessed.
//
// An adopted machine was not built by the run its provenance names; it is
// changed and removed as the blueprint it was adopted as.
func builtBy(r Realization, bps []Blueprint) *Blueprint {
	if r.AdoptedAs != "" {
		return blueprintByID(bps, r.AdoptedAs)
	}
	if r.Provenance.RunbookName == "" {
		return nil
	}
	for i := range bps {
		if bps[i].Name == r.Provenance.RunbookName {
			return &bps[i]
		}
	}
	return nil
}

func decideChange(e *Entry, r Realization, creator *Blueprint, world Snapshot) {
	if e.Power != "" && len(e.Diff) == 0 {
		decidePower(e, r, creator, world)
		return
	}
	if len(e.Diff) == 0 {
		// Rejected by acceptance, another connector, not in its group: none
		// of these is a size an apply can set.
		e.Blocked = "an apply changes a machine only when inputs it can change in place differ; this one needs a person"
		return
	}
	// Drift on a machine that is not running: the change in place reboots it
	// and waits for it to come back, which a stopped machine never does, so
	// the sizes would be set and the run would fail. Said, not attempted.
	if e.Drift && e.AssetID != nil {
		if st := world.Observed[*e.AssetID].State; st != "" && st != ObservedRunning {
			e.Blocked = fmt.Sprintf("changed outside Sybil while %s; a change in place restarts the machine, so start it first", st)
			return
		}
	}
	if creator == nil {
		e.Blocked = "Sybil cannot tell which blueprint built this machine, so it does not change it"
		return
	}
	if e.Blueprint != "" && e.Blueprint != creator.ID {
		e.Blocked = fmt.Sprintf("built by %s, and the manifest now names %s; that means replacing the machine, "+
			"which an apply does not do yet", creator.ID, e.Blueprint)
		return
	}
	if creator.ReconfiguredBy == "" || blueprintByID(world.Blueprints, creator.ReconfiguredBy) == nil {
		e.Blocked = "the blueprint that built this machine cannot change it in place"
		return
	}
	var notInPlace, shrinks, unknownNow []string
	for _, d := range e.Diff {
		in := inputByName(creator.Inputs, d.Input)
		if in == nil || !in.InPlace {
			notInPlace = append(notInPlace, d.Input)
			continue
		}
		if !in.GrowOnly {
			continue
		}
		// What the change will actually ask for: an input dropped from the
		// manifest falls back to the blueprint's default, which may well be
		// smaller than what the machine has.
		to := d.To
		if to == nil {
			to = in.Default
		}
		switch {
		case d.From == nil && !isZero(to):
			// Built without declaring it: what the machine has now is not
			// recorded, so a "grow" could be a shrink nobody can see.
			unknownNow = append(unknownNow, d.Input)
		case shrinksValue(d.From, to):
			shrinks = append(shrinks, fmt.Sprintf("%s (%v to %v)", d.Input, d.From, to))
		}
	}
	if len(notInPlace) > 0 {
		sort.Strings(notInPlace)
		e.Blocked = fmt.Sprintf("%s cannot change in place; that means replacing the machine, which an apply does not do yet",
			strings.Join(notInPlace, ", "))
		return
	}
	if len(shrinks) > 0 {
		e.Blocked = "only ever grown, never shrunk: " + strings.Join(shrinks, ", ")
		return
	}
	if len(unknownNow) > 0 {
		e.Blocked = fmt.Sprintf("%s was not declared when the machine was built, so what it has now is not known; "+
			"declare it at its current value first", strings.Join(unknownNow, ", "))
		return
	}
	if r.MintedUUID == "" {
		e.Blocked = "no identity was recorded when this machine was built, so Sybil cannot prove a change would reach it"
		return
	}
	e.Apply = ApplyReconfigure
	e.LifecycleBlueprint = creator.ReconfiguredBy
	e.MintedUUID = r.MintedUUID
	e.BuiltName = builtName(r, e)
}

func decidePower(e *Entry, r Realization, creator *Blueprint, world Snapshot) {
	by := ""
	if creator != nil {
		by = creator.PoweredOnBy
		if e.Power == "off" {
			by = creator.PoweredOffBy
		}
	}
	switch {
	case creator == nil:
		e.Blocked = "Sybil cannot tell which blueprint built this machine, so it does not power it"
	case by == "" || blueprintByID(world.Blueprints, by) == nil:
		e.Blocked = "the blueprint that built this machine cannot power it " + e.Power
	case r.MintedUUID == "":
		e.Blocked = "no identity was recorded when this machine was built, so Sybil cannot prove it would reach it"
	default:
		e.Apply = ApplyPower
		e.LifecycleBlueprint = by
		e.MintedUUID = r.MintedUUID
		e.BuiltName = builtName(r, e)
	}
}

func decideDelete(e *Entry, r Realization, creator *Blueprint, world Snapshot) {
	destroyer := ""
	if creator != nil {
		destroyer = creator.DestroyedBy
	}
	switch {
	case creator == nil:
		e.Blocked = "Sybil cannot tell which blueprint built this machine, so it does not remove it"
	case destroyer == "" || blueprintByID(world.Blueprints, destroyer) == nil:
		e.Blocked = "Sybil cannot tell how to remove this machine: the blueprint that built it ships no removal"
	case world.Connector == nil || r.IntegrationID != world.Connector.ID:
		e.Blocked = "built through another connector than the manifest names"
	case r.MintedUUID == "":
		e.Blocked = "no identity was recorded when this machine was built, so Sybil cannot prove a removal would reach it"
	default:
		e.Apply = ApplyDestroy
		e.LifecycleBlueprint = destroyer
		e.MintedUUID = r.MintedUUID
		e.BuiltName = builtName(r, e)
	}
}

// builtName is the name the machine was built with, for the guard that it
// still carries it. Empty when none was recorded; the identity still holds.
func builtName(r Realization, e *Entry) string {
	if s, ok := r.IntentInputs["hostname"].(string); ok {
		return s
	}
	if s, ok := e.Inputs["hostname"].(string); ok && e.Action == ActionChange {
		return s
	}
	return ""
}

func inputByName(inputs []BlueprintInput, name string) *BlueprintInput {
	for i := range inputs {
		if inputs[i].Name == name {
			return &inputs[i]
		}
	}
	return nil
}

func isZero(v any) bool {
	n, ok := number(v)
	return v == nil || (ok && n == 0)
}

// shrinksValue reports whether to is a smaller number than from. 0 (or no
// value) means "leave it as it is", which is never a shrink.
func shrinksValue(from, to any) bool {
	f, okF := number(from)
	t, okT := number(to)
	if !okT || t == 0 {
		return false
	}
	return okF && t < f
}

// PowerNotObserved is stated when a resource declares power and some of its
// machines' state was never observed: their power is not compared.
const PowerNotObserved = "the power state of some machines was never observed, so it is not compared"

// planPower turns an accepted, unchanged machine whose observed power differs
// from its resource's into a change. Only a machine observed running or
// stopped is judged: suspended, unknown or never observed is not rounded to
// either.
func planPower(env envmanifest.Environment, entries []Entry, world Snapshot) {
	want := map[string]string{}
	for _, r := range env.Spec.Resources {
		if r.Power != "" {
			want[r.Name] = r.Power
		}
	}
	if len(want) == 0 {
		return
	}
	for i := range entries {
		e := &entries[i]
		p := want[e.Resource]
		if p == "" || e.Action != ActionNoOp || e.AssetID == nil {
			continue
		}
		state := world.Power[*e.AssetID]
		switch {
		case p == "on" && state == "stopped", p == "off" && state == "running":
			e.Action = ActionChange
			e.Reason = fmt.Sprintf("%s, and the manifest says power: %s", state, p)
			e.Power = p
		}
	}
}

func powerNotObserved(env envmanifest.Environment, entries []Entry, world Snapshot) bool {
	declares := map[string]bool{}
	for _, r := range env.Spec.Resources {
		declares[r.Name] = r.Power != ""
	}
	for _, e := range entries {
		if !declares[e.Resource] || e.AssetID == nil {
			continue
		}
		if s := world.Power[*e.AssetID]; s != "running" && s != "stopped" {
			return true
		}
	}
	return false
}

// adoptedAssumed reports whether a declared instance is an adopted machine
// whose blueprint declares inputs no adoption observes.
func adoptedAssumed(entries []Entry, current map[instanceKey]Realization, world Snapshot) bool {
	for _, e := range entries {
		r, ok := current[instanceKey{e.Resource, e.Index}]
		if !ok || r.AdoptedAs == "" {
			continue
		}
		bp := blueprintByID(world.Blueprints, r.AdoptedAs)
		if bp == nil {
			return true
		}
		for _, in := range bp.Inputs {
			if in.Observes == "" && in.Source != InputSourceIntegrations {
				if _, declared := e.Inputs[in.Name]; declared {
					return true
				}
			}
		}
	}
	return false
}
