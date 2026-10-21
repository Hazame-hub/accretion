package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/hazame-hub/accretion/bootstrap"
	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
)

// applier carries out a plan with the provider's Writer: apply and destroy.
// What each plan line becomes is openspec accretion-apply, design D8:
//
//	create                                 clone, seed, start, confirm, record
//	unknown, half-built by an earlier run  resume it from its step
//	delete with apply: destroy             delete (stopping it first), retire
//	change with apply: power               power it
//	change, anything else                  not done, said; exit 1
//	unknown, no-op                         untouched
//
// One machine at a time, in plan order. The state is written before the
// first request for a machine and after every step, so a crash leaves a
// record a later apply resumes from with the same identity and owner mark.
type applier struct {
	ctx         context.Context
	in          io.Reader
	out         io.Writer
	cmd         string
	autoApprove bool

	statePath  string
	state      State
	env        envmanifest.Environment
	session    provider.Session
	writer     provider.Writer
	provider   uuid.UUID
	blueprints []plan.Blueprint
	recipe     func(blueprint string, inputs map[string]any) (provider.Recipe, error)
	explain    func(error) string
	// gone are recorded machines the plan would delete and the provider no
	// longer has: only their record is retired.
	gone map[uuid.UUID]bool
	// forgetGone retires the records of gone machines (--forget-gone).
	forgetGone bool
}

// errState is a state that could not be written. The run stops on it: what
// it did next would not be recorded.
var errState = errors.New("the state could not be written")

// step is one machine's work, decided before anything is asked.
type step struct {
	label string
	do    func() error
}

func (a *applier) printf(format string, args ...any) { fmt.Fprintf(a.out, format, args...) }

func (a *applier) run(p plan.Plan, notes []string) int {
	printPlan(a.out, apiPlan{Plan: p, Summary: p.Summary(), OK: p.OK(), Notes: notes})

	var refusals, notDone []string
	refuse := func(format string, args ...any) { refusals = append(refusals, fmt.Sprintf(format, args...)) }
	if !p.OK() {
		refuse("the plan has errors")
	}
	for _, n := range p.Networks {
		if n.Action != plan.ActionNoOp {
			refuse("network %s plans as %s; accrete v0.1 does not write networks, and a machine on a network that does not exist would fail half-built", n.Name, n.Action)
		}
	}
	for _, r := range a.env.Spec.Resources {
		if r.Firewall != nil && a.cmd == "apply" {
			refuse("resource %s declares a firewall; accrete v0.1 does not write firewalls", r.Name)
		}
	}
	if w, ok := a.session.(provider.Writer); ok {
		a.writer = w
	}
	if caps, err := a.session.Capabilities(a.ctx); a.writer == nil || err != nil ||
		!caps.Write.Clone || !caps.Write.GuestInit || !caps.Write.Power || !caps.Write.Delete || !caps.Write.MintUUID || !caps.Write.OwnerMark {
		refuse("the provider cannot clone, seed, power and delete machines under a minted identity, which apply needs")
	}

	applying := map[uuid.UUID]int{}
	for i, rec := range a.state.Applying {
		applying[rec.RunID] = i
	}
	var steps []step
	resumed := map[uuid.UUID]bool{}
	for _, e := range p.Entries {
		e := e
		name := fmt.Sprintf("%s/%d", e.Resource, e.Index)
		switch {
		case e.Action == plan.ActionCreate:
			rec, err := a.buildable(e.Blueprint, e.Inputs)
			if err != nil {
				refuse("%s: %v", name, err)
				continue
			}
			steps = append(steps, step{"create " + name, func() error { return a.create(e, rec) }})
		case e.Action == plan.ActionUnknown && e.RunID != nil:
			i, ours := applying[*e.RunID]
			if !ours || a.cmd == "destroy" {
				// A run an embedder recorded is not accrete's; and destroy
				// removes a half-built machine (below), never finishes it.
				continue
			}
			rec, err := a.buildable(a.state.Applying[i].Blueprint, a.state.Applying[i].IntentInputs)
			if err != nil {
				refuse("%s: %v", name, err)
				continue
			}
			resumed[*e.RunID] = true
			id := *e.RunID
			steps = append(steps, step{"finish " + name + " (half-built by an earlier apply)", func() error { return a.build(id, rec) }})
		case e.Action == plan.ActionDelete && e.Apply == plan.ApplyDestroy:
			steps = append(steps, step{"delete " + name + " (" + e.ResourceRefID + ")", func() error { return a.delete(e) }})
		case e.Action == plan.ActionChange && e.Apply == plan.ApplyPower:
			steps = append(steps, step{"power " + e.Power + " " + name, func() error { return a.power(e) }})
		case e.Action == plan.ActionUnknown && e.RealizationID != nil && a.gone[*e.RealizationID] && a.forgetGone:
			steps = append(steps, step{"retire " + name + " (" + e.ResourceRefID + " is already gone)", func() error { return a.delete(e) }})
		case e.Action == plan.ActionUnknown && e.RealizationID != nil && a.gone[*e.RealizationID]:
			// The provider lists nothing at the ref, but it leaves out guests
			// the token may not see, so this is no proof the machine is gone.
			notDone = append(notDone, fmt.Sprintf("%s %s: %s; if it was removed, run again with --forget-gone to retire its record", e.Action, name, e.Reason))
		case e.Action == plan.ActionUnknown && e.RealizationID != nil && a.cmd == "destroy":
			// Not confirmed: left alone, and destroy has not removed it.
			notDone = append(notDone, fmt.Sprintf("%s %s: %s", e.Action, name, e.Reason))
		case e.Action == plan.ActionChange, e.Action == plan.ActionDelete:
			why := e.Blocked
			if why == "" {
				why = "accrete v0.1 does not " + e.Apply + " machines"
			}
			notDone = append(notDone, fmt.Sprintf("%s %s: %s", e.Action, name, why))
		}
	}
	for _, rec := range a.state.Applying {
		if rec.Intent.Kind != plan.IntentKind || rec.Intent.Name != a.env.Name || resumed[rec.RunID] {
			continue
		}
		rec := rec
		name := fmt.Sprintf("%s/%d", rec.Intent.ResourceName, rec.Intent.InstanceIndex)
		if a.cmd == "destroy" {
			steps = append(steps, step{"remove " + name + " (half-built by an earlier apply)", func() error { return a.abandon(rec.RunID) }})
		} else {
			notDone = append(notDone, fmt.Sprintf("%s was half-built by an earlier apply and is no longer declared; accrete destroy removes it", name))
		}
	}

	for _, msg := range notDone {
		a.printf("  not done: %s\n", msg)
	}
	if len(refusals) > 0 {
		for _, msg := range refusals {
			a.printf("  refused: %s\n", msg)
		}
		a.printf("Nothing was sent.\n")
		return 1
	}
	if len(steps) == 0 {
		a.printf("Nothing to %s.\n", a.cmd)
		if len(notDone) > 0 {
			return 1
		}
		return 0
	}
	if !a.autoApprove {
		a.printf("\n%s these %d actions?\n", strings.ToUpper(a.cmd[:1])+a.cmd[1:], len(steps))
		for _, s := range steps {
			a.printf("  %s\n", s.label)
		}
		a.printf("Only 'yes' is accepted: ")
		line, _ := bufio.NewReader(a.in).ReadString('\n')
		if strings.TrimSpace(line) != "yes" {
			a.printf("\nCancelled; nothing was sent.\n")
			return 1
		}
	}

	failed := 0
	for i, s := range steps {
		a.printf("%s\n", s.label)
		if err := s.do(); err != nil {
			failed++
			a.printf("  failed: %s\n", a.describe(err))
			if errors.Is(err, errState) {
				a.printf("Stopped: the state could not be written, so nothing more is done; %d actions not started.\n", len(steps)-i-1)
				return 1
			}
		}
	}
	a.printf("%s: %d done, %d failed, %d not done.\n", a.cmd, len(steps)-failed, failed, len(notDone))
	if failed > 0 || len(notDone) > 0 {
		return 1
	}
	return 0
}

// describe says what an error from a step means for the operator.
func (a *applier) describe(err error) string {
	msg := err.Error()
	if a.explain != nil {
		if hint := a.explain(err); hint != "" {
			msg += " (" + hint + ")"
		}
	}
	var refused *provider.RefusedError
	var unknown *provider.OutcomeUnknownError
	switch {
	case errors.As(err, &unknown):
		msg += "; the answer was lost, so the provider may or may not have done it: run accrete " + a.cmd + " again, it carries on from the state"
	case errors.As(err, &refused):
		msg += "; refused, nothing was sent for this step"
	}
	return msg
}

// buildable is the recipe for an instance, and why it cannot be built.
func (a *applier) buildable(blueprint string, inputs map[string]any) (provider.Recipe, error) {
	if a.recipe == nil {
		return provider.Recipe{}, errors.New("the provider has no way to build machines")
	}
	rec, err := a.recipe(blueprint, inputs)
	if err != nil {
		return provider.Recipe{}, err
	}
	if _, err := userData(rec); err != nil {
		return provider.Recipe{}, err
	}
	return rec, nil
}

// userData is the cloud-config of a machine: its hostname and the
// resource's bootstrap profile. accrete has no secret store, so a profile
// that references one is refused.
func userData(rec provider.Recipe) (string, error) {
	var profile *bootstrap.Profile
	if rec.Bootstrap != nil {
		p, err := bootstrap.Parse(rec.Bootstrap)
		if err != nil {
			return "", err
		}
		if err := p.Strings(func(s string) (string, error) {
			if strings.Contains(s, "{{secret.") {
				return "", errors.New("references a secret; accrete v0.1 has no secret store, so write the value in or leave it out")
			}
			return s, nil
		}); err != nil {
			return "", err
		}
		profile = p
	}
	return bootstrap.Render(bootstrap.Base{Hostname: rec.Hostname}, profile)
}

func (a *applier) save() error {
	if err := saveState(a.statePath, a.state); err != nil {
		return fmt.Errorf("%w: %s: %v", errState, a.statePath, err)
	}
	return nil
}

// create records a new machine as being built, before the first request for
// it, then builds it.
func (a *applier) create(e plan.Entry, rec provider.Recipe) error {
	run := uuid.New()
	a.state.Applying = append(a.state.Applying, Applying{
		RunID:        run,
		Intent:       plan.RealizationIntent{Kind: plan.IntentKind, Name: a.env.Name, ResourceName: e.Resource, InstanceIndex: e.Index},
		IntentInputs: e.Inputs,
		Blueprint:    e.Blueprint,
		MintedUUID:   uuid.NewString(),
		OwnerMark:    "accrete:" + run.String(),
		Name:         rec.Hostname,
		Step:         stepClone,
	})
	if err := a.save(); err != nil {
		return err
	}
	return a.build(run, rec)
}

func (a *applier) record(run uuid.UUID) *Applying {
	for i := range a.state.Applying {
		if a.state.Applying[i].RunID == run {
			return &a.state.Applying[i]
		}
	}
	return nil
}

func (a *applier) target(r *Applying) provider.Target {
	return provider.Target{Ref: provider.Ref{Kind: provider.KindVM, ID: r.ResourceRefID}, ExpectUUID: r.MintedUUID, Name: r.Name, OwnerMark: r.OwnerMark}
}

// build runs a machine's steps from the one its record names, saving the
// state after each.
func (a *applier) build(run uuid.UUID, rec provider.Recipe) error {
	for {
		r := a.record(run)
		if r == nil {
			return nil
		}
		switch r.Step {
		case stepClone:
			req := rec.Clone
			req.Name, req.MintUUID, req.OwnerMark, req.ID = r.Name, r.MintedUUID, r.OwnerMark, ""
			m, err := a.writer.Clone(a.ctx, req)
			if err != nil {
				return fmt.Errorf("clone: %w", err)
			}
			r.ResourceRefID, r.Step = m.Ref.ID, stepGuestInit
			a.printf("  cloned as %s, SMBIOS UUID %s\n", m.Ref.ID, r.MintedUUID)
		case stepGuestInit:
			ud, err := userData(rec)
			if err != nil {
				return fmt.Errorf("cloud-init: %w", err)
			}
			pl, err := a.writer.GuestInit(a.ctx, a.target(r), provider.Seed{
				UserData:   []byte(ud),
				MetaData:   []byte(fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", r.MintedUUID, rec.Hostname)),
				Storage:    rec.SeedStorage,
				InstanceID: r.MintedUUID,
			})
			if err != nil {
				return fmt.Errorf("cloud-init seed: %w", err)
			}
			r.Step = stepPower
			a.printf("  seeded with %s\n", pl.Volume)
		case stepPower:
			if _, err := a.writer.Power(a.ctx, a.target(r), provider.PowerRunning); err != nil {
				return fmt.Errorf("start: %w", err)
			}
			r.Step = stepConfirm
			a.printf("  started\n")
		case stepConfirm:
			return a.confirmBuilt(run, rec)
		default:
			return fmt.Errorf("the state names step %q, which this accrete does not know", r.Step)
		}
		if err := a.save(); err != nil {
			return err
		}
	}
}

// confirmBuilt reads the machine back and records it. Acceptance in accrete
// is what the provider observes (the identity, running, the declared size),
// not an agent's report: v0.1 enrols no agent.
func (a *applier) confirmBuilt(run uuid.UUID, rec provider.Recipe) error {
	r := a.record(run)
	resources, err := a.session.Discover(a.ctx)
	if err != nil && len(resources) == 0 {
		return fmt.Errorf("confirm: read the provider: %w", err)
	}
	var found *provider.Resource
	for i := range resources {
		if resources[i].Ref.ID == r.ResourceRefID {
			found = &resources[i]
		}
	}
	if found == nil {
		return fmt.Errorf("confirm: the provider does not list %s", r.ResourceRefID)
	}
	if !sameUUID(found.Identity.SMBIOSUUID, provider.UUIDForms(r.MintedUUID)) {
		return fmt.Errorf("confirm: %s carries SMBIOS UUID %q, not the minted %s", r.ResourceRefID, found.Identity.SMBIOSUUID, r.MintedUUID)
	}
	var wrong []string
	if found.State != "running" {
		wrong = append(wrong, "it is "+found.State+", not running")
	}
	if want := rec.Clone.Shape.Cores; want > 0 && int(found.Facts.CPUCores) != want {
		wrong = append(wrong, fmt.Sprintf("it has %d cores, not %d", found.Facts.CPUCores, want))
	}
	if want := rec.Clone.Shape.MemoryMB; want > 0 && found.Facts.RAMBytes != int64(want)<<20 {
		wrong = append(wrong, fmt.Sprintf("it has %d MiB of memory, not %d", found.Facts.RAMBytes>>20, want))
	}

	asset, id := uuid.New(), uuid.New()
	runbook := ""
	for _, bp := range a.blueprints {
		if bp.ID == r.Blueprint {
			runbook = bp.Name
		}
	}
	intent := r.Intent
	a.state.Realizations = append(a.state.Realizations, plan.Realization{
		ID: id, IntegrationID: a.provider, AssetID: &asset, ResourceRefID: r.ResourceRefID, ResourceKind: "vm",
		Managed: true, Intent: &intent, Provenance: plan.Provenance{RunbookName: runbook},
		IntentInputs: r.IntentInputs, MintedUUID: r.MintedUUID,
	})
	if a.state.Verdicts == nil {
		a.state.Verdicts = map[uuid.UUID]string{}
	}
	verdict := plan.AcceptanceAccepted
	if len(wrong) > 0 {
		verdict = plan.AcceptanceRejected
	} else if g := a.env.Spec.Group; g != "" {
		if a.state.Groups == nil {
			a.state.Groups = map[string][]uuid.UUID{}
		}
		a.state.Groups[g] = append(a.state.Groups[g], asset)
	}
	a.state.Verdicts[asset] = verdict
	a.dropApplying(run)
	if err := a.save(); err != nil {
		return err
	}
	if len(wrong) > 0 {
		return fmt.Errorf("confirm: %s is built and recorded, but rejected: %s", r.ResourceRefID, strings.Join(wrong, "; "))
	}
	a.printf("  confirmed and recorded: running, as declared\n")
	return nil
}

func (a *applier) dropApplying(run uuid.UUID) {
	out := a.state.Applying[:0]
	for _, r := range a.state.Applying {
		if r.RunID != run {
			out = append(out, r)
		}
	}
	a.state.Applying = out
	if len(out) == 0 {
		a.state.Applying = nil
	}
}

func (a *applier) realization(id *uuid.UUID) (*plan.Realization, error) {
	if id == nil {
		return nil, errors.New("the plan line names no record")
	}
	for i := range a.state.Realizations {
		if a.state.Realizations[i].ID == *id {
			return &a.state.Realizations[i], nil
		}
	}
	return nil, fmt.Errorf("realization %s is not in the state", *id)
}

// seedOf is where a recorded machine's seed was put, from the inputs it was
// built from; nil when they cannot say.
func (a *applier) seedOf(r *plan.Realization) []provider.Placement {
	for _, bp := range a.blueprints {
		if bp.Name != r.Provenance.RunbookName || a.recipe == nil {
			continue
		}
		if rec, err := a.recipe(bp.ID, r.IntentInputs); err == nil && rec.Seed != nil {
			return []provider.Placement{rec.Seed(r.MintedUUID)}
		}
	}
	return nil
}

// delete removes a recorded machine and retires its record.
func (a *applier) delete(e plan.Entry) error {
	r, err := a.realization(e.RealizationID)
	if err != nil {
		return err
	}
	seeds := a.seedOf(r)
	d, err := a.writer.Delete(a.ctx, provider.Target{Ref: provider.Ref{Kind: provider.KindVM, ID: r.ResourceRefID}, ExpectUUID: r.MintedUUID, Name: e.BuiltName},
		provider.DeleteOptions{StopFirst: true, RemovePlacements: seeds})
	if err != nil {
		return fmt.Errorf("delete: %w", err)
	}
	now := time.Now().UTC()
	r.RetiredAt = &now
	if r.AssetID != nil {
		for g, members := range a.state.Groups {
			kept := members[:0]
			for _, m := range members {
				if m != *r.AssetID {
					kept = append(kept, m)
				}
			}
			a.state.Groups[g] = kept
		}
	}
	if err := a.save(); err != nil {
		return err
	}
	if d.AlreadyAbsent {
		a.printf("  already gone; record retired\n")
	} else {
		a.printf("  deleted; record retired\n")
	}
	return nil
}

// power starts or shuts down a recorded machine.
func (a *applier) power(e plan.Entry) error {
	r, err := a.realization(e.RealizationID)
	if err != nil {
		return err
	}
	want := provider.PowerRunning
	if e.Power == "off" {
		want = provider.PowerStopped
	}
	res, err := a.writer.Power(a.ctx, provider.Target{Ref: provider.Ref{Kind: provider.KindVM, ID: r.ResourceRefID}, ExpectUUID: r.MintedUUID, Name: e.BuiltName}, want)
	if err != nil {
		return fmt.Errorf("power: %w", err)
	}
	a.printf("  %s\n", res.State)
	return nil
}

// abandon removes what an interrupted apply left of a machine the manifest
// no longer declares, or that destroy is removing: the guest, when it is
// found carrying the minted identity, and its record. A guest that carries
// the mark but not yet the identity is left for apply to finish: deleting
// it would mean trusting a name.
func (a *applier) abandon(run uuid.UUID) error {
	r := a.record(run)
	resources, err := a.session.Discover(a.ctx)
	if err != nil && len(resources) == 0 {
		return fmt.Errorf("read the provider: %w", err)
	}
	if r.ResourceRefID == "" {
		// The clone never answered: it may have been made, and may still be
		// copying, while the provider lists it with no identity yet (a clone
		// in progress has neither name nor SMBIOS UUID). Then nothing proves
		// the record can be dropped.
		if err != nil {
			return fmt.Errorf("the provider's listing is incomplete (%v), so a clone of %s still in progress could not be ruled out; run accrete destroy again", err, r.Name)
		}
		for _, res := range resources {
			// A clone in progress has no name of its own yet (the provider
			// lists it as vm-<id>) or already has this machine's; any other
			// guest without an identity is somebody else's.
			unnamed := res.Name == "vm-"+res.Identity.HypervisorGuestID
			if res.Ref.Kind == provider.KindVM && res.Identity.SMBIOSUUID == "" && (unnamed || res.Name == r.Name) {
				return fmt.Errorf("%s has no identity yet, so it may be %s's clone still copying; run accrete apply to finish it, or accrete destroy again once it is done", res.Ref.ID, r.Name)
			}
		}
	}
	forms := provider.UUIDForms(r.MintedUUID)
	var found, named *provider.Resource
	for i := range resources {
		res := &resources[i]
		switch {
		case r.ResourceRefID != "" && res.Ref.ID == r.ResourceRefID, r.ResourceRefID == "" && res.Name == r.Name:
			if sameUUID(res.Identity.SMBIOSUUID, forms) {
				found = res
			} else {
				named = res
			}
		}
	}
	switch {
	case found != nil:
		var seeds []provider.Placement
		if rec, err := a.recipe(r.Blueprint, r.IntentInputs); err == nil && rec.Seed != nil {
			seeds = []provider.Placement{rec.Seed(r.MintedUUID)}
		}
		if _, err := a.writer.Delete(a.ctx, provider.Target{Ref: found.Ref, ExpectUUID: r.MintedUUID, Name: r.Name, OwnerMark: r.OwnerMark},
			provider.DeleteOptions{StopFirst: true, RemovePlacements: seeds}); err != nil {
			return fmt.Errorf("delete: %w", err)
		}
		a.printf("  deleted %s\n", found.Ref.ID)
	case named != nil:
		return fmt.Errorf("%s may be the half-built machine, but it does not carry its identity %s yet; run accrete apply to finish it, or remove it by hand", named.Ref.ID, r.MintedUUID)
	case r.ResourceRefID != "":
		// Gone from its ref: only the seed may be left.
		var seeds []provider.Placement
		if rec, err := a.recipe(r.Blueprint, r.IntentInputs); err == nil && rec.Seed != nil {
			seeds = []provider.Placement{rec.Seed(r.MintedUUID)}
		}
		if _, err := a.writer.Delete(a.ctx, a.target(r), provider.DeleteOptions{RemovePlacements: seeds}); err != nil {
			return fmt.Errorf("remove its seed: %w", err)
		}
		a.printf("  nothing is at %s any more; record dropped\n", r.ResourceRefID)
	default:
		a.printf("  nothing was built; record dropped\n")
	}
	a.dropApplying(run)
	return a.save()
}
