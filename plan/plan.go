// Package plan says what applying an Environment manifest would do.
//
// It is the read-only half of the Environment lifecycle: INTENT → PLAN. The
// manifest (package manifest, sybil/v1alpha1) says which machines should
// exist; the embedder's realizations say which ones it has built for it; the
// latest machine acceptance says whether what was built is what was asked
// for. A plan lines the three up, one entry per declared instance plus one per
// realization the manifest no longer declares, and nothing here writes.
//
// The package is pure on purpose. Everything it needs is read once from a
// World into a Snapshot, so the rules for what counts as CREATE, NO-OP,
// CHANGE, DELETE or UNKNOWN are tested as plain functions, and there is
// exactly one place those rules live.
//
// UNKNOWN is a real answer, not an error. It is what the plan says whenever the
// evidence does not support a stronger one — a machine built but never
// adopted, one nobody has evaluated, one the embedder disclaims authority
// over — and it is never rounded up to NO-OP to make a plan look finished.
package plan

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"

	"github.com/google/uuid"
	"github.com/hazame-hub/accretion/bootstrap"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// Action is what applying the manifest would do to one instance.
type Action string

const (
	// ActionCreate is a declared instance with no current realization.
	ActionCreate Action = "create"
	// ActionNoOp is a declared instance realized through the declared
	// connector, managed, accepted, and built from the inputs the manifest
	// declares — or built before inputs were recorded, which Plan.Limits says.
	ActionNoOp Action = "no-op"
	// ActionChange is a declared instance whose realization is known not to
	// satisfy the manifest: acceptance rejected it, it was built through a
	// different connector than the manifest names, or it was built from inputs
	// the manifest no longer declares (Entry.Diff says which).
	ActionChange Action = "change"
	// ActionDelete is a realization of this environment that the manifest no
	// longer declares — a resource removed, or a count lowered.
	ActionDelete Action = "delete"
	// ActionUnknown is an instance the evidence cannot place. Reason says what
	// is missing.
	ActionUnknown Action = "unknown"
)

// IntentKind is the realization intent kind an Environment writes. It is the
// manifest's own kind string, so a realization row reads as "Environment
// staging, resource web, instance 0" without a lookup table.
const IntentKind = envmanifest.KindEnvironment

// Placeholders a string input may use to tell the instances of one resource
// apart. Index matches the realization's instance_index; Number is the same
// instance counted from one, which is how people name machines.
const (
	placeholderIndex  = "{{instance.index}}"
	placeholderNumber = "{{instance.number}}"
	placeholderPrefix = "{{instance."
)

// InputsNotCompared is stated when some existing instance was built before
// Sybil recorded the inputs a machine was built from (migration 000112). For
// those the plan can say "built, managed, accepted" but not "built from these
// exact inputs", and a no-op that silently skipped the comparison is a line
// somebody acts on.
const InputsNotCompared = "inputs not compared for some instances: they were built before Sybil recorded the inputs " +
	"a machine was built from, so an edited input on them plans as no-op"

// AdoptedInputsAssumed is stated when some instance is a machine Sybil
// adopted rather than built. Its name, cores, memory and disk were read off
// the machine when it was adopted; what else the manifest declares for it (a
// template, an image, a bootstrap profile) was taken as declared, because
// nothing observes it, so a difference there is not seen.
const AdoptedInputsAssumed = "some machines were adopted rather than built: inputs Sybil cannot observe on a machine " +
	"(such as its template, image or bootstrap) were recorded as the manifest declared them when it was adopted, so they are not compared"

// Connector is the integration the manifest names, resolved.
type Connector struct {
	ID   uuid.UUID
	Name string
	Type string
}

// Snapshot is everything the planner reads, as of one moment. See World.
type Snapshot struct {
	// Connector is the manifest's spec.integration resolved in the org, or nil
	// when no connector has that name.
	Connector *Connector
	// Blueprints are the blueprints the connector's provider ships. Empty when
	// Connector is nil or the provider ships none.
	Blueprints []Blueprint
	// GroupExists answers spec.group. Ignored when the manifest names none.
	GroupExists bool
	// GroupMembers are the assets in spec.group now. An accepted machine
	// missing from it plans as change: the group is part of what the manifest
	// declares, and an apply puts each machine it builds there once accepted.
	GroupMembers map[uuid.UUID]bool
	// GroupID is spec.group resolved, which the apply hands each run it
	// launches so the machine can join once accepted.
	GroupID *uuid.UUID
	// Realizations are the org's current realizations. The planner keeps the
	// ones whose intent is this environment, so the caller does not have to
	// know how an Environment intent is spelled.
	Realizations []Realization
	// Verdicts is the latest acceptance verdict per asset, for the assets the
	// realizations name. An asset absent from the map was never evaluated.
	Verdicts map[uuid.UUID]string
	// InFlight are runs still running, held or queued that were launched for
	// an intent. Like Realizations, the planner keeps this environment's.
	InFlight []Run
	// SDN is the provider's networks, read only when the manifest declares
	// some. Nil means they were not read; SDNError then says why.
	SDN      *ObservedSDN
	SDNError string
	// Capacity is the cluster's memory, read when the manifest declares
	// machines. Nil means it was not read; CapacityError then says why.
	Capacity      *Capacity
	CapacityError string
	// Selectors are the connector's pickers, and Images the answers to the
	// manifest's ImageQueries. Read only when a resource names an image.
	Selectors []Selector
	Images    map[ImageQuery]ImageAnswer
	// Power is each machine's last observed state ("running", "stopped",
	// "suspended", "unknown"), by asset. Read only when a resource declares
	// power; a machine absent from it was never observed.
	Power map[uuid.UUID]string
	// Observed is each machine's size as the provider last reported it, by
	// asset and fact (Observed* below). A declared input whose blueprint input
	// names a fact (BlueprintInput.Observes) is checked against it, so a machine
	// resized outside Sybil plans as change. A machine or fact absent from it
	// was not observed, and is not compared.
	Observed map[uuid.UUID]ObservedMachine
	// NetworkAccess is what the connector's token lacks for the networks the
	// manifest declares and the bridges its NICs name, read only when there
	// are some. Nil means it was not read; NetworkAccessError then says why.
	NetworkAccess      *NetworkAccess
	NetworkAccessError string
	// Firewalls are the observed firewalls of this environment's machines,
	// by realization, read only when a resource declares one. A machine
	// absent from it was not read; FirewallErrors then says why, when known.
	Firewalls      map[uuid.UUID]ObservedFirewall
	FirewallErrors map[uuid.UUID]string
}

// Entry is one instance's line in the plan.
type Entry struct {
	Resource string `json:"resource"`
	Index    int    `json:"index"`
	Action   Action `json:"action"`
	// Reason says why, in a sentence, for every action but create.
	Reason string `json:"reason,omitempty"`
	// Blueprint and Inputs are what a create would run: the blueprint id and
	// the instance's expanded inputs. Set for declared instances only.
	Blueprint string         `json:"blueprint,omitempty"`
	Inputs    map[string]any `json:"inputs,omitempty"`
	// RealizationID, ResourceRefID and AssetID name what already exists, when
	// something does.
	RealizationID *uuid.UUID `json:"realization_id,omitempty"`
	ResourceRefID string     `json:"resource_ref_id,omitempty"`
	AssetID       *uuid.UUID `json:"asset_id,omitempty"`
	// Verdict is the latest acceptance verdict of AssetID, when there is one.
	Verdict string `json:"verdict,omitempty"`
	// RunID is the run already building this instance, when one is in flight.
	RunID *uuid.UUID `json:"run_id,omitempty"`
	// Diff is how the manifest's inputs differ from the ones this instance was
	// built from, when that is why it plans as change.
	Diff []InputDiff `json:"diff,omitempty"`
	// Drift says the change is the machine moving away from the manifest —
	// resized outside Sybil — rather than the manifest changing. Diff's From
	// is then what the provider observed.
	Drift bool `json:"drift,omitempty"`
	// Apply is what an apply does about a change or a delete (ApplyReconfigure
	// or ApplyDestroy, with LifecycleBlueprint the blueprint it runs). Empty
	// on those actions means it does nothing, and Blocked says why.
	Apply              string `json:"apply,omitempty"`
	LifecycleBlueprint string `json:"lifecycle_blueprint,omitempty"`
	Blocked            string `json:"blocked,omitempty"`
	// MintedUUID and BuiltName are the guards that run carries: the identity
	// the machine was built with and the name it was given. Server-side only.
	MintedUUID string `json:"-"`
	BuiltName  string `json:"-"`
	// Power is the state a power change sets ("on" or "off").
	Power string `json:"power,omitempty"`
	// Placement is the node the instance is or will be on, for resources that
	// declare a placement.
	Placement *PlacementNote `json:"placement,omitempty"`
}

// InputDiff is one input the manifest now declares differently. From is what
// the machine was built with, To is what the manifest says; either is absent
// when the input was added or removed.
type InputDiff struct {
	Input string `json:"input"`
	From  any    `json:"from,omitempty"`
	To    any    `json:"to,omitempty"`
}

// Plan is the whole answer for one Environment.
type Plan struct {
	Environment string  `json:"environment"`
	Entries     []Entry `json:"entries"`
	// Networks is one line per declared network, omitted when none are.
	Networks []NetworkEntry `json:"networks,omitempty"`
	// Firewalls is one line per declared machine whose resource declares a
	// firewall, omitted when none does.
	Firewalls []FirewallEntry `json:"firewalls,omitempty"`
	// Errors are what is wrong with the manifest against this org: a missing
	// connector, an unknown blueprint, inputs the blueprint refuses. A plan
	// with errors still lists its entries, because "what exists" is useful
	// while the manifest is being fixed — but it is not one to apply.
	Errors []string `json:"errors,omitempty"`
	// Limits are what the plan did not assess, stated whenever they apply.
	Limits []string `json:"limits"`
	// Warnings are things the plan did assess and the operator should weigh
	// before approving, such as a cluster this apply would overcommit. They
	// never stop an apply.
	Warnings []string `json:"warnings,omitempty"`
}

// Empty is a plan with no entries yet. Entries is an empty list rather than
// nil, because nil encodes as JSON null and a client iterating it breaks.
func Empty(environment string) Plan {
	return Plan{Environment: environment, Entries: []Entry{}, Limits: []string{}}
}

// OK reports whether the manifest is valid against the org.
func (p Plan) OK() bool { return len(p.Errors) == 0 }

// Counts tallies the entries by action.
func (p Plan) Counts() map[Action]int {
	out := map[Action]int{}
	for _, e := range p.Entries {
		out[e.Action]++
	}
	return out
}

// Summary renders the counts in a fixed order.
func (p Plan) Summary() string {
	c := p.Counts()
	out := fmt.Sprintf("%d to create, %d unchanged, %d to change, %d to delete, %d unknown",
		c[ActionCreate], c[ActionNoOp], c[ActionChange], c[ActionDelete], c[ActionUnknown])
	if len(p.Networks) > 0 {
		n := map[Action]int{}
		for _, e := range p.Networks {
			n[e.Action]++
		}
		out += fmt.Sprintf("; networks: %d to create, %d unchanged, %d to change, %d unknown",
			n[ActionCreate], n[ActionNoOp], n[ActionChange], n[ActionUnknown])
	}
	if len(p.Firewalls) > 0 {
		f := map[Action]int{}
		for _, e := range p.Firewalls {
			f[e.Action]++
		}
		out += fmt.Sprintf("; firewalls: %d unchanged, %d to change, %d unknown",
			f[ActionNoOp], f[ActionChange], f[ActionUnknown])
	}
	return out
}

// Instance is one machine a resource declares: the resource expanded by count.
type Instance struct {
	Resource  string
	Index     int
	Blueprint string
	Inputs    map[string]any
	// Unplaced is set when placement found no node for the instance. Its
	// node input is then unset, so nothing that needs the node (its image,
	// its input check) is tried: the placement error already says why.
	Unplaced bool
}

// Expand turns each resource into Count instances, substituting the instance
// placeholders in string inputs. It returns the instances in manifest order,
// index ascending, and every problem it found rather than the first.
//
// A resource whose instances would all be identical is refused when Count is
// above one. Identical inputs make identical machines — on Proxmox the same
// hostname twice, which identity matching then cannot tell apart — and that is
// never what a count was written to mean.
func Expand(env envmanifest.Environment) ([]Instance, []string) {
	var out []Instance
	var errs []string
	for _, r := range env.Spec.Resources {
		// Reported, but the instances are still produced. Dropping them would
		// make every machine already built for this resource look undeclared,
		// and a typo in a placeholder would plan the resource's deletion.
		if bad := unknownPlaceholders(r.Inputs); len(bad) > 0 {
			errs = append(errs, fmt.Sprintf("resource %q uses %s; the instance placeholders are %s and %s",
				r.Name, strings.Join(bad, ", "), placeholderIndex, placeholderNumber))
		}
		withVars, missing := substituteVars(map[string]any(r.Inputs), env.Spec.Variables)
		if len(missing) > 0 {
			errs = append(errs, unknownVarsError(r.Name, missing))
		}
		resourceInputs, _ := withVars.(map[string]any)
		if r.Inputs == nil {
			resourceInputs = nil
		}
		first := len(out)
		for i := 0; i < r.Count; i++ {
			inputs := expandInputs(resourceInputs, i)
			// Expanded here, not at launch, so the input a machine is
			// recorded as built from is the NIC it actually got — and a
			// network renamed in the manifest plans as the change it is.
			for j, nic := range r.NICs() {
				if inputs == nil {
					inputs = map[string]any{}
				}
				inputs[envmanifest.NICInputN(j)] = "virtio,bridge=" + nic.Network
			}
			out = append(out, Instance{
				Resource:  r.Name,
				Index:     i,
				Blueprint: r.Blueprint,
				Inputs:    inputs,
			})
		}
		if r.Count > 1 && reflect.DeepEqual(out[first].Inputs, out[first+1].Inputs) {
			errs = append(errs, fmt.Sprintf("resource %q has count %d but every instance gets the same inputs; "+
				"use %s or %s in an input such as the hostname to tell them apart",
				r.Name, r.Count, placeholderIndex, placeholderNumber))
		}
	}
	return out, errs
}

func expandInputs(in map[string]any, index int) map[string]any {
	if in == nil {
		return nil
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		if s, ok := v.(string); ok {
			s = strings.ReplaceAll(s, placeholderIndex, fmt.Sprint(index))
			s = strings.ReplaceAll(s, placeholderNumber, fmt.Sprint(index+1))
			out[k] = s
			continue
		}
		out[k] = v
	}
	return out
}

// unknownPlaceholders lists {{instance.*}} spellings that are not one of the
// two the planner substitutes, sorted. A typo there would otherwise reach the
// machine verbatim — a host literally named "web-{{instance.nubmer}}".
func unknownPlaceholders(in map[string]any) []string {
	seen := map[string]bool{}
	for _, v := range in {
		s, ok := v.(string)
		if !ok {
			continue
		}
		for {
			at := strings.Index(s, placeholderPrefix)
			if at < 0 {
				break
			}
			rest := s[at:]
			end := strings.Index(rest, "}}")
			if end < 0 {
				seen[rest] = true
				break
			}
			token := rest[:end+2]
			if token != placeholderIndex && token != placeholderNumber {
				seen[token] = true
			}
			s = rest[end+2:]
		}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// Build plans env against what world reads.
func Build(env envmanifest.Environment, w World) Plan {
	world := w.Read()
	plan := Empty(env.Name)

	instances, errs := Expand(env)
	plan.Errors = append(plan.Errors, errs...)
	plan.Errors = append(plan.Errors, checkReferences(env, world)...)
	// Before the images and the inputs: the chosen node is an input, and the
	// node is where an image's template is looked up.
	placements, placeErrs, placeWarnings, placeLimits := place(env, instances, world)
	plan.Errors = append(plan.Errors, placeErrs...)
	plan.Warnings = append(plan.Warnings, placeWarnings...)
	plan.Limits = append(plan.Limits, placeLimits...)
	// Before the inputs are checked: an image fills an input the blueprint
	// may require.
	imgErrs, unpinned := resolveImages(env, instances, world)
	plan.Errors = append(plan.Errors, imgErrs...)
	plan.Errors = append(plan.Errors, assignAddresses(env, instances, world)...)
	if unpinned {
		plan.Limits = append(plan.Limits, ImageNotPinned)
	}
	plan.Errors = append(plan.Errors, checkInputs(instances, world)...)

	current := map[instanceKey]Realization{}
	for _, r := range world.Realizations {
		if !r.Current() || r.Intent == nil || r.Intent.Kind != IntentKind || r.Intent.Name != env.Name {
			continue
		}
		current[instanceKey{r.Intent.ResourceName, r.Intent.InstanceIndex}] = r
	}

	building := map[instanceKey]uuid.UUID{}
	buildingNetworks := map[instanceKey]uuid.UUID{}
	for _, run := range world.InFlight {
		if run.Intent == nil || run.Intent.Kind != IntentKind || run.Intent.Name != env.Name {
			continue
		}
		key := instanceKey{run.Intent.ResourceName, run.Intent.InstanceIndex}
		if strings.HasPrefix(key.resource, NetworkResourcePrefix) {
			buildingNetworks[key] = run.ID
			continue
		}
		building[key] = run.ID
	}
	networks, netErrs := planNetworks(env, world, buildingNetworks)
	plan.Networks = networks
	plan.Errors = append(plan.Errors, netErrs...)
	if len(networks) > 0 {
		plan.Limits = append(plan.Limits, NetworksNotOwned)
	}
	for _, n := range networks {
		if n.Action == ActionCreate && n.DHCPRange != "" {
			plan.Limits = append(plan.Limits, DHCPNeedsDnsmasq)
			break
		}
	}
	// A run still creating a network the manifest no longer declares is
	// said, like a machine's: it will still reload the cluster's SDN.
	declaredNet := map[string]bool{}
	for _, n := range env.Spec.Networks {
		declaredNet[NetworkResourcePrefix+n.Name] = true
	}
	var strayNets []instanceKey
	for key := range buildingNetworks {
		if !declaredNet[key.resource] {
			strayNets = append(strayNets, key)
		}
	}
	sortKeys(strayNets)
	for _, key := range strayNets {
		runID := buildingNetworks[key]
		plan.Networks = append(plan.Networks, NetworkEntry{
			Name: strings.TrimPrefix(key.resource, NetworkResourcePrefix), RunID: &runID,
			Action: ActionUnknown, Reason: "no longer declared, but a run is still creating it",
		})
	}

	declared := map[instanceKey]bool{}
	uncompared := false
	for _, in := range instances {
		key := instanceKey{in.Resource, in.Index}
		declared[key] = true
		entry := Entry{Resource: in.Resource, Index: in.Index, Blueprint: in.Blueprint, Inputs: withConnector(in, world),
			Placement: placements[key]}
		r, realized := current[key]
		runID, inFlight := building[key]
		switch {
		case realized && inFlight:
			// Changing or replacing it already: a second run for the same
			// machine would race the first.
			describe(&entry, r, world)
			entry.RunID = &runID
			entry.Action = ActionUnknown
			entry.Reason = "a run is already working on this machine; its outcome is not known yet"
		case realized:
			describe(&entry, r, world)
			entry.Action, entry.Reason = classify(r, entry.Verdict, world)
			// Rejected, and declared differently from what it was built from:
			// most likely a change in place that was applied and judged
			// before the provider caught up, or one that did not take. The
			// diff is what an apply can act on — its re-evaluation decides
			// — where a rejection with no diff needs a person.
			if entry.Action == ActionChange && entry.Verdict == AcceptanceRejected && r.IntentInputs != nil {
				if diff := diffInputs(r.IntentInputs, entry.Inputs); len(diff) > 0 {
					entry.Diff = diff
					entry.Reason = "rejected by acceptance, and built from different inputs than the manifest now declares"
				}
			}
			if entry.Action == ActionNoOp && env.Spec.Group != "" && world.GroupExists &&
				r.AssetID != nil && !world.GroupMembers[*r.AssetID] {
				entry.Action = ActionChange
				entry.Reason = fmt.Sprintf("accepted, but not a member of group %q", env.Spec.Group)
			}
			if entry.Action == ActionNoOp {
				if r.IntentInputs == nil {
					uncompared = true
				} else if diff := diffInputs(r.IntentInputs, entry.Inputs); len(diff) > 0 {
					// Only an accepted machine gets this far, so the only
					// thing wrong with it is that the manifest changed.
					entry.Action = ActionChange
					entry.Reason = "built from different inputs than the manifest now declares"
					entry.Diff = diff
				}
			}
			// The manifest and what the machine was built from agree; the
			// machine itself may not. A size the provider reports differently
			// from the declared one was changed outside Sybil, and an apply
			// puts it back the same way it would apply a manifest change.
			if entry.Action == ActionNoOp {
				if drift, alsoSet := observedDrift(entry, world); len(drift) > 0 {
					entry.Action = ActionChange
					entry.Reason = "changed outside Sybil: " + describeDrift(drift, alsoSet)
					entry.Diff = driftDiff(drift, alsoSet)
					entry.Drift = true
				}
			}
		case inFlight:
			// Not create: a second run for the same instance is a second
			// machine. Unknown until the run finishes and either records a
			// realization or does not.
			entry.RunID = &runID
			entry.Action = ActionUnknown
			entry.Reason = "a run is already building this instance; its outcome is not known yet"
		default:
			entry.Action = ActionCreate
		}
		plan.Entries = append(plan.Entries, entry)
	}

	if uncompared {
		plan.Limits = append(plan.Limits, InputsNotCompared)
	}
	if adoptedAssumed(plan.Entries, current, world) {
		plan.Limits = append(plan.Limits, AdoptedInputsAssumed)
	}
	if powerNotObserved(env, plan.Entries, world) {
		plan.Limits = append(plan.Limits, PowerNotObserved)
	}
	gateOnNetworks(env, plan.Entries, plan.Networks)
	capacityNotes(&plan, world)

	// Runs still building an instance the manifest no longer declares — the
	// count was lowered mid-build. Nothing to delete yet, and not nothing.
	var orphaned []instanceKey
	for key := range building {
		if !declared[key] {
			if _, realized := current[key]; !realized {
				orphaned = append(orphaned, key)
			}
		}
	}
	sortKeys(orphaned)
	for _, key := range orphaned {
		runID := building[key]
		plan.Entries = append(plan.Entries, Entry{Resource: key.resource, Index: key.index, RunID: &runID,
			Action: ActionUnknown, Reason: "no longer declared, but a run is still building it"})
	}

	// What exists and is no longer declared, in a stable order: by resource
	// name, then index.
	var gone []instanceKey
	for key := range current {
		if !declared[key] {
			gone = append(gone, key)
		}
	}
	sortKeys(gone)
	for _, key := range gone {
		r := current[key]
		entry := Entry{Resource: key.resource, Index: key.index}
		describe(&entry, r, world)
		if runID, inFlight := building[key]; inFlight {
			// Being removed already, most likely: never a second removal.
			entry.RunID = &runID
			entry.Action = ActionUnknown
			entry.Reason = "no longer declared, and a run is already working on this machine"
		} else if r.Managed {
			entry.Action = ActionDelete
			entry.Reason = "the manifest no longer declares this instance"
		} else {
			entry.Action = ActionUnknown
			entry.Reason = "the manifest no longer declares this instance, and Sybil does not claim authority over it " +
				"(managed = false), so it would not be removed"
		}
		plan.Entries = append(plan.Entries, entry)
	}
	planPower(env, plan.Entries, world)
	decideLifecycle(plan.Entries, world)
	firewalls, fwWarnings, fwLimits := planFirewalls(env, plan.Entries, world)
	plan.Firewalls = firewalls
	plan.Warnings = append(plan.Warnings, fwWarnings...)
	plan.Limits = append(plan.Limits, fwLimits...)
	// Last: it reads what every entry and network was decided to be.
	checkNetworkAccess(env, &plan, world)
	return plan
}

// diffInputs lists the inputs whose values differ between what a machine was
// built with and what the manifest declares, sorted by name.
//
// Values are compared by their JSON encoding. The recorded side came back from
// a JSONB column, so its numbers are float64 where the manifest's are int, and
// 1024 and 1024.0 are the same input; encoding both is what makes them equal.
func diffInputs(built, declared map[string]any) []InputDiff {
	keys := map[string]bool{}
	for k := range built {
		keys[k] = true
	}
	for k := range declared {
		keys[k] = true
	}
	names := make([]string, 0, len(keys))
	for k := range keys {
		names = append(names, k)
	}
	sort.Strings(names)
	var out []InputDiff
	for _, k := range names {
		was, hadIt := built[k]
		now, hasIt := declared[k]
		if hadIt && hasIt && sameValue(was, now) {
			continue
		}
		d := InputDiff{Input: k}
		if hadIt {
			d.From = was
		}
		if hasIt {
			d.To = now
		}
		out = append(out, d)
	}
	return out
}

func sameValue(a, b any) bool {
	ja, errA := json.Marshal(a)
	jb, errB := json.Marshal(b)
	return errA == nil && errB == nil && string(ja) == string(jb)
}

type instanceKey struct {
	resource string
	index    int
}

// sortKeys orders by resource name, then index.
func sortKeys(keys []instanceKey) {
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].resource != keys[j].resource {
			return keys[i].resource < keys[j].resource
		}
		return keys[i].index < keys[j].index
	})
}

func describe(e *Entry, r Realization, world Snapshot) {
	id := r.ID
	e.RealizationID = &id
	e.ResourceRefID = r.ResourceRefID
	e.AssetID = r.AssetID
	if r.AssetID != nil {
		e.Verdict = world.Verdicts[*r.AssetID]
	}
}

// classify places a declared instance that already has a realization.
func classify(r Realization, verdict string, world Snapshot) (Action, string) {
	if !r.Managed {
		return ActionUnknown, "Sybil does not claim authority over this machine (managed = false)"
	}
	if world.Connector != nil && r.IntegrationID != world.Connector.ID {
		return ActionChange, fmt.Sprintf("built through a different connector than %q; moving it means replacing it",
			world.Connector.Name)
	}
	if r.AssetID == nil {
		return ActionUnknown, "the machine exists on the provider but was never adopted into inventory, " +
			"so nothing has evaluated it"
	}
	switch verdict {
	case AcceptanceAccepted:
		return ActionNoOp, "realized and accepted"
	case AcceptanceRejected:
		return ActionChange, "realized but rejected by acceptance: the machine does not satisfy its intent"
	case AcceptanceIncomplete:
		return ActionUnknown, "acceptance could not reach a verdict for this machine"
	case "":
		return ActionUnknown, "realized, but this machine has never been evaluated for acceptance"
	default:
		return ActionUnknown, fmt.Sprintf("acceptance verdict %q is not one the planner knows", verdict)
	}
}

// checkReferences resolves what the manifest names, reporting every miss.
func checkReferences(env envmanifest.Environment, world Snapshot) []string {
	var errs []string
	if world.Connector == nil {
		errs = append(errs, fmt.Sprintf("no connector named %q in this organisation", env.Spec.Integration))
	}
	if env.Spec.Group != "" && !world.GroupExists {
		errs = append(errs, fmt.Sprintf("no asset group named %q in this organisation", env.Spec.Group))
	}
	if world.Connector == nil {
		// Without a connector there is no provider, and so no blueprint list
		// to look in; one error says that better than one per resource.
		return errs
	}
	for _, r := range env.Spec.Resources {
		if blueprintByID(world.Blueprints, r.Blueprint) == nil {
			errs = append(errs, fmt.Sprintf("resource %q: connector %q (%s) ships no blueprint %q",
				r.Name, world.Connector.Name, world.Connector.Type, r.Blueprint))
		}
	}
	return errs
}

// checkInputs validates each instance against its blueprint's inputs: no key
// the blueprint does not declare, every required input present.
//
// Undeclared keys are refused, not passed through. The run engine tolerates
// them because a hand-launched runbook may reference ad-hoc keys; a manifest
// is reviewed intent, and a misspelled `memroy_mb` silently dropped is a
// machine with the default memory that the manifest says otherwise about.
//
// One message per resource and problem, not per instance: ten instances
// missing the same input are one mistake.
func checkInputs(instances []Instance, world Snapshot) []string {
	if world.Connector == nil {
		return nil
	}
	var errs []string
	reported := map[string]bool{}
	for _, in := range instances {
		bp := blueprintByID(world.Blueprints, in.Blueprint)
		if bp == nil || in.Unplaced {
			continue
		}
		specs := bp.Inputs
		declared := map[string]BlueprintInput{}
		for _, s := range specs {
			declared[s.Name] = s
		}
		var keys []string
		for k := range in.Inputs {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			spec, ok := declared[k]
			switch {
			case !ok:
				report(&errs, reported, fmt.Sprintf("resource %q: blueprint %q has no input %q", in.Resource, in.Blueprint, k))
			case spec.Source == InputSourceIntegrations:
				report(&errs, reported, fmt.Sprintf("resource %q: input %q is the connector, which spec.integration already names; remove it",
					in.Resource, k))
			case spec.Type == BootstrapInputType:
				// Checked here, so a profile with a typo is a plan error and
				// not a machine that booted without its users.
				if _, err := bootstrap.Parse(in.Inputs[k]); err != nil {
					report(&errs, reported, fmt.Sprintf("resource %q: input %q: %v", in.Resource, k, err))
				}
			}
		}
		full := withConnector(in, world)
		for _, s := range specs {
			if !s.Required || s.Default != nil {
				continue
			}
			if v, ok := full[s.Name]; !ok || v == nil || v == "" {
				report(&errs, reported, fmt.Sprintf("resource %q: blueprint %q requires input %q", in.Resource, in.Blueprint, s.Name))
			}
		}
	}
	return errs
}

// BootstrapInputType is the blueprint input type that holds a bootstrap
// profile (see package bootstrap).
const BootstrapInputType = "bootstrap"

func report(errs *[]string, seen map[string]bool, msg string) {
	if seen[msg] {
		return
	}
	seen[msg] = true
	*errs = append(*errs, msg)
}

// withConnector is the instance's inputs as a run would receive them: the
// blueprint's connector input filled with spec.integration's id. The manifest
// names its connector once, in the envelope, rather than once per resource.
func withConnector(in Instance, world Snapshot) map[string]any {
	out := make(map[string]any, len(in.Inputs)+1)
	for k, v := range in.Inputs {
		out[k] = v
	}
	if world.Connector != nil {
		if bp := blueprintByID(world.Blueprints, in.Blueprint); bp != nil {
			for _, s := range bp.Inputs {
				if s.Source == InputSourceIntegrations {
					out[s.Name] = world.Connector.ID.String()
				}
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func blueprintByID(bps []Blueprint, id string) *Blueprint {
	for i := range bps {
		if bps[i].ID == id {
			return &bps[i]
		}
	}
	return nil
}
