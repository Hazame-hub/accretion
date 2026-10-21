package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
)

// notRead is what the plan says about a fact accrete does not read yet. It
// lands in the Snapshot's error fields, so the plan states the limit rather
// than planning as if the fact were known.
const notRead = "accrete does not read this yet"

// sdnReader and capacityReader are the typed reads a provider's session may
// offer. A session without them has its SDN and capacity reported as not
// read.
type sdnReader interface {
	ObservedSDN(ctx context.Context) (plan.ObservedSDN, error)
}

type capacityReader interface {
	ObservedCapacity(ctx context.Context) (plan.Capacity, error)
}

// fileWorld is plan.World over a State and, when it has a session, what the
// provider reports now. Read does all the provider's reading, so a plan sees
// one moment, and it never fails: what it could not read is left unread,
// and said.
type fileWorld struct {
	ctx        context.Context
	state      State
	env        envmanifest.Environment
	blueprints []plan.Blueprint
	session    provider.Session // nil: the provider is not asked
	// notAsked says why session is nil.
	notAsked string
	// explain says what to check for a provider error; nil adds nothing.
	explain func(error) string
	now     func() time.Time

	// Notes are what Read could not do, for the operator, beside the plan.
	Notes []string
	// Unconfirmed is every recorded machine the provider did not confirm,
	// by realization, with why. Such a machine is never planned as no-op
	// or change: see confirm.
	Unconfirmed map[uuid.UUID]string
	// Refused is the first error that was the provider refusing the
	// credential itself; nil when none was.
	Refused error
}

// inFlight is what the planner sees as being built: the runs an embedder
// recorded, and the machines accrete itself started building and has not
// recorded yet, so a half-built machine plans as unknown, never as create.
func (w *fileWorld) inFlight() []plan.Run {
	out := append([]plan.Run(nil), w.state.InFlight...)
	for _, a := range w.state.Applying {
		intent := a.Intent
		out = append(out, plan.Run{ID: a.RunID, Status: "running", Intent: &intent, IntentInputs: a.IntentInputs})
	}
	return out
}

func (w *fileWorld) note(format string, args ...any) {
	w.Notes = append(w.Notes, fmt.Sprintf(format, args...))
}

// why is err with what the provider says to check for it. It also notes a
// refused credential, which the command stops on (Refused).
func (w *fileWorld) why(err error) string {
	if w.Refused == nil && errors.Is(err, provider.ErrUnauthorized) {
		w.Refused = err
	}
	if w.explain != nil {
		if hint := w.explain(err); hint != "" {
			return err.Error() + " (" + hint + ")"
		}
	}
	return err.Error()
}

func (w *fileWorld) unconfirmed(r plan.Realization, why string) {
	if w.Unconfirmed == nil {
		w.Unconfirmed = map[uuid.UUID]string{}
	}
	w.Unconfirmed[r.ID] = why
}

// Read implements plan.World.
func (w *fileWorld) Read() plan.Snapshot {
	ref := w.env.Spec.Provider
	snap := plan.Snapshot{
		Connector:    &plan.Connector{ID: ProviderID(*ref), Name: ref.Type, Type: ref.Type},
		Blueprints:   w.blueprints,
		Realizations: w.state.Realizations,
		Verdicts:     w.state.Verdicts,
		InFlight:     w.inFlight(),
	}
	if snap.Verdicts == nil {
		snap.Verdicts = map[uuid.UUID]string{}
	}
	if g := w.env.Spec.Group; g != "" {
		if members, ok := w.state.Groups[g]; ok {
			snap.GroupExists = true
			snap.GroupMembers = map[uuid.UUID]bool{}
			for _, m := range members {
				snap.GroupMembers[m] = true
			}
		}
	}

	// What accrete does not read yet is said, never assumed: each of these
	// makes the plan state a limit or plan the affected line as unknown.
	if nets, bridges := plan.NetworkAccessQuery(w.env); len(nets)+len(bridges) > 0 {
		snap.NetworkAccessError = notRead
	}
	for _, q := range plan.ImageQueries(w.env, snapshotWorld(snap)) {
		if snap.Images == nil {
			snap.Images = map[plan.ImageQuery]plan.ImageAnswer{}
		}
		snap.Images[q] = plan.ImageAnswer{Err: notRead}
	}
	firewalled := map[string]bool{}
	powered := false
	for _, r := range w.env.Spec.Resources {
		firewalled[r.Name] = r.Firewall != nil
		powered = powered || r.Power != ""
	}
	mine := w.currentRealizations()
	for _, r := range mine {
		if firewalled[r.Intent.ResourceName] {
			if snap.FirewallErrors == nil {
				snap.FirewallErrors = map[uuid.UUID]string{}
			}
			snap.FirewallErrors[r.ID] = notRead
		}
	}

	if w.session == nil {
		why := "the provider was not asked: " + w.notAsked
		if len(w.env.Spec.Networks) > 0 {
			snap.SDNError = why
		}
		if len(w.env.Spec.Resources) > 0 {
			snap.CapacityError = why
		}
		for _, r := range mine {
			w.unconfirmed(r, why)
		}
		w.note("%s", why)
		return snap
	}

	if len(w.env.Spec.Networks) > 0 {
		if rd, ok := w.session.(sdnReader); !ok {
			snap.SDNError = notRead
		} else if sdn, err := rd.ObservedSDN(w.ctx); err != nil {
			snap.SDNError = w.why(err)
			w.note("the provider's networks could not be read: %s", w.why(err))
		} else {
			snap.SDN = &sdn
		}
	}
	if len(w.env.Spec.Resources) > 0 {
		if rd, ok := w.session.(capacityReader); !ok {
			snap.CapacityError = notRead
		} else if c, err := rd.ObservedCapacity(w.ctx); err != nil {
			snap.CapacityError = w.why(err)
			w.note("the provider's capacity could not be read: %s", w.why(err))
		} else {
			snap.Capacity = &c
		}
	}
	if len(mine) == 0 {
		return snap
	}

	resources, err := w.session.Discover(w.ctx)
	if err != nil && len(resources) == 0 {
		why := "the provider could not be read: " + w.why(err)
		w.note("%s", why)
		for _, r := range mine {
			w.unconfirmed(r, why)
		}
		return snap
	}
	if err != nil {
		// Partial is still evidence for what it did list; a machine it did
		// not list is unconfirmed, below.
		w.note("the provider's listing is incomplete: %s", w.why(err))
	}
	byRef := map[string]provider.Resource{}
	for _, res := range resources {
		byRef[res.Ref.ID] = res
	}
	at := w.now()
	for _, r := range mine {
		res, found := byRef[r.ResourceRefID]
		if !found {
			w.unconfirmed(r, fmt.Sprintf("nothing is at %s on the provider", r.ResourceRefID))
			continue
		}
		// The ref says where to look; the identity says it is still the
		// machine that was built there. A VMID given to somebody else's
		// guest is not this machine.
		forms := provider.UUIDForms(r.MintedUUID)
		if forms == nil {
			w.unconfirmed(r, "the state records no SMBIOS UUID for it, so the guest at "+r.ResourceRefID+" cannot be told apart from another")
			continue
		}
		if !sameUUID(res.Identity.SMBIOSUUID, forms) {
			got := res.Identity.SMBIOSUUID
			if got == "" {
				got = "none"
			}
			w.unconfirmed(r, fmt.Sprintf("identity mismatch: the guest at %s carries SMBIOS UUID %s, not the %s it was built with",
				r.ResourceRefID, got, r.MintedUUID))
			continue
		}
		if r.AssetID == nil {
			continue
		}
		if powered {
			if snap.Power == nil {
				snap.Power = map[uuid.UUID]string{}
			}
			snap.Power[*r.AssetID] = res.State
		}
		raw, _ := json.Marshal(provider.Observation{Facts: res.Facts, Memory: res.Memory, State: res.State, ObservedAt: at})
		if m, ok := plan.ObservedFromProvider(raw); ok {
			if snap.Observed == nil {
				snap.Observed = map[uuid.UUID]plan.ObservedMachine{}
			}
			snap.Observed[*r.AssetID] = m
		}
	}
	return snap
}

// confirm keeps the plan from claiming what the provider did not show. A
// recorded machine the provider did not confirm (it could not be read,
// refused the token, has nothing at the ref, or has another guest there) is
// planned as unknown, never as no-op, change or delete: its record is all
// there is, and a record is not the machine. apply and destroy act on what
// the plan says, so this is also what keeps them off a guest that is not the
// one recorded.
func (w *fileWorld) confirm(p *plan.Plan) {
	for i := range p.Entries {
		e := &p.Entries[i]
		if e.RealizationID == nil {
			continue
		}
		why, unconfirmed := w.Unconfirmed[*e.RealizationID]
		if !unconfirmed || (e.Action != plan.ActionNoOp && e.Action != plan.ActionChange && e.Action != plan.ActionDelete) {
			continue
		}
		was := e.Action
		e.Action = plan.ActionUnknown
		e.Reason = fmt.Sprintf("%s; the record alone says %s", why, was)
		e.Apply, e.LifecycleBlueprint = "", ""
	}
}

// currentRealizations are the live realizations of this environment.
func (w *fileWorld) currentRealizations() []plan.Realization {
	var out []plan.Realization
	for _, r := range w.state.Realizations {
		if r.Current() && r.Intent != nil && r.Intent.Kind == plan.IntentKind && r.Intent.Name == w.env.Name {
			out = append(out, r)
		}
	}
	return out
}

// snapshotWorld is a Snapshot as a plan.World, for the planner's own
// queries made while the real one is being read.
type snapshotWorld plan.Snapshot

func (s snapshotWorld) Read() plan.Snapshot { return plan.Snapshot(s) }

// sameUUID compares in every byte order a UUID is reported in.
func sameUUID(got string, wantForms []string) bool {
	for _, g := range provider.UUIDForms(got) {
		for _, w := range wantForms {
			if g == w {
				return true
			}
		}
	}
	return false
}
