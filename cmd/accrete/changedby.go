package main

import (
	"fmt"
	"strings"
	"time"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider"
)

// attribute says, on each line that reports a machine changed outside
// accrete (drift) or powered differently than declared, who made that change,
// from the provider's own record. The rule never guesses:
//
//   - the newest recorded change of the kind that would explain the line is
//     taken, failed ones skipped;
//   - when that newest one was made by accrete's own credential, the drift
//     came after it without being recorded, so the author is unknown;
//   - a power line is attributed only to a change that left the machine in
//     the state observed (a stop for a stopped machine). A guest shut down
//     from its own console leaves no record, so the author is then unknown.
//
// It reads the record once per machine and sends GET requests only.
func (w *fileWorld) attribute(p *plan.Plan) {
	var hist provider.History
	if w.session != nil {
		hist, _ = w.session.(provider.History)
	}
	observes := map[string]map[string]string{} // blueprint → input → fact
	for _, bp := range w.blueprints {
		m := map[string]string{}
		for _, in := range bp.Inputs {
			if in.Observes != "" {
				m[in.Name] = in.Observes
			}
		}
		observes[bp.ID] = m
	}
	cache := map[string][]provider.Change{}
	errs := map[string]error{}
	for i := range p.Entries {
		e := &p.Entries[i]
		// Only a line the provider confirmed: an unknown one is not this
		// machine's evidence to explain.
		if e.Action != plan.ActionChange || e.ResourceRefID == "" {
			continue
		}
		power := e.Power != ""
		if !e.Drift && !power {
			continue
		}
		if hist == nil {
			e.ChangedBy = &plan.ChangeAuthor{Unknown: "the provider keeps no record of who changes a machine that accrete can read"}
			continue
		}
		ref := e.ResourceRefID
		if _, done := cache[ref]; !done && errs[ref] == nil {
			changes, err := hist.Changes(w.ctx, provider.Ref{Kind: provider.KindVM, ID: ref})
			if err != nil {
				errs[ref] = err
			} else {
				cache[ref] = changes
			}
		}
		if err := errs[ref]; err != nil {
			e.ChangedBy = &plan.ChangeAuthor{Unknown: "the provider's record of " + ref + " could not be read: " + w.why(err)}
			continue
		}
		if power {
			e.ChangedBy = byPower(cache[ref], e.Power)
			continue
		}
		e.ChangedBy = byKind(cache[ref], driftKinds(e, observes[e.Blueprint]))
	}
}

// driftKinds are the change kinds that could explain a drift line's diff.
func driftKinds(e *plan.Entry, observes map[string]string) []string {
	seen := map[string]bool{}
	var out []string
	for _, d := range e.Diff {
		kind := provider.ChangeConfig
		if observes[d.Input] == plan.ObservedBootDiskGiB {
			kind = provider.ChangeDisk
		}
		if !seen[kind] {
			seen[kind] = true
			out = append(out, kind)
		}
	}
	if len(out) == 0 {
		out = []string{provider.ChangeConfig}
	}
	return out
}

func byKind(changes []provider.Change, kinds []string) *plan.ChangeAuthor {
	var authors []string
	var newest *provider.Change
	for _, kind := range kinds {
		c := newestOf(changes, func(c provider.Change) bool { return c.Kind == kind })
		if c == nil {
			return &plan.ChangeAuthor{Unknown: fmt.Sprintf("no %s change is recorded on this machine; the provider does not log every change (on some it keeps no record of a change applied at once)", kind)}
		}
		if c.Self {
			return &plan.ChangeAuthor{Unknown: fmt.Sprintf("the last recorded %s change is accrete's own (%s at %s), so this one came after it and was not recorded",
				kind, c.What, c.At.Format(time.RFC3339))}
		}
		authors = append(authors, c.By)
		if newest == nil || c.At.After(newest.At) {
			newest = c
		}
	}
	if len(authors) > 1 && !allSame(authors) {
		return &plan.ChangeAuthor{Unknown: "several people made the recorded changes: " + strings.Join(authors, ", ")}
	}
	at := newest.At
	return &plan.ChangeAuthor{By: newest.By, At: &at, What: newest.What}
}

func byPower(changes []provider.Change, declared string) *plan.ChangeAuthor {
	c := newestOf(changes, func(c provider.Change) bool { return c.Kind == provider.ChangePower })
	if c == nil {
		return &plan.ChangeAuthor{Unknown: "no power change is recorded on this machine; a shutdown from inside the guest leaves none"}
	}
	explains := (declared == "on" && c.PowerState != "running") || (declared == "off" && c.PowerState == "running")
	switch {
	case c.Self:
		return &plan.ChangeAuthor{Unknown: fmt.Sprintf("the last recorded power change is accrete's own (%s at %s); what came after was not recorded, as a shutdown from inside the guest is not",
			c.What, c.At.Format(time.RFC3339))}
	case !explains:
		return &plan.ChangeAuthor{Unknown: fmt.Sprintf("the last recorded power change (%s by %s at %s) does not explain it; a shutdown from inside the guest leaves no record",
			c.What, c.By, c.At.Format(time.RFC3339))}
	}
	at := c.At
	return &plan.ChangeAuthor{By: c.By, At: &at, What: c.What}
}

func newestOf(changes []provider.Change, match func(provider.Change) bool) *provider.Change {
	for i := range changes {
		if !changes[i].Failed && match(changes[i]) {
			return &changes[i]
		}
	}
	return nil
}

func allSame(xs []string) bool {
	for _, x := range xs[1:] {
		if x != xs[0] {
			return false
		}
	}
	return true
}
