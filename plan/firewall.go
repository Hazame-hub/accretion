package plan

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// Firewalls in a plan.
//
// A resource's `firewall:` is planned per machine, against the guest's
// firewall as the provider reports it at plan time — like a network, and for
// the same reason: the provider's answer is the whole truth for "is it what
// the manifest says", and a record of what Sybil last wrote would only be a
// copy that can go stale the moment somebody edits a rule by hand.
//
// It is planned only for a machine that exists. One the same apply creates
// gets its firewall from the next apply, once it is built: the create runs
// through its own approval and acceptance, and a firewall step tacked onto it
// would be one more thing a failed build has to undo.

// ApplyFirewall sets the machine's firewall with FirewallEntry.Blueprint. The
// run is launched for the machine's own instance, so while it runs the machine
// plans as "a run is already working on this machine" and no second run is
// started for it.
const ApplyFirewall = "firewall"

// ClusterFirewallOff is the warning a plan carries when it declares an enabled
// firewall on a cluster whose firewall is off.
const ClusterFirewallOff = "the cluster firewall is off, so no machine's firewall filters anything until it is switched on " +
	"(Datacenter → Firewall → Options → Firewall); Sybil does not switch it, because it applies to every guest on the cluster"

// ClusterFirewallUnknown is the limit stated when its state could not be read.
const ClusterFirewallUnknown = "whether the cluster firewall is on could not be read, so declared firewalls may be filtering nothing"

// ObservedFirewall is one guest's firewall as the provider reported it.
type ObservedFirewall struct {
	Enabled   bool
	PolicyIn  string
	PolicyOut string
	// Rules in evaluation order, each in the provider's own keys (type,
	// action, macro, proto, source, dest, dport, sport, comment, enable…).
	Rules        []map[string]string
	ClusterKnown bool
	ClusterOn    bool
}

// FirewallEntry is one machine's firewall line in the plan.
type FirewallEntry struct {
	Resource string `json:"resource"`
	Index    int    `json:"index"`
	Action   Action `json:"action"`
	Reason   string `json:"reason,omitempty"`
	// Rules is how many rules the manifest declares for it.
	Rules int `json:"rules"`
	// Diff says, a line each, how the machine's firewall differs from the
	// declaration, when that is why it plans as change.
	Diff []string `json:"diff,omitempty"`
	// Apply is ApplyFirewall when an apply sets it, with Blueprint the
	// blueprint it runs; empty on a change means it does not, and Blocked
	// says why.
	Apply     string `json:"apply,omitempty"`
	Blueprint string `json:"blueprint,omitempty"`
	Blocked   string `json:"blocked,omitempty"`

	RealizationID *uuid.UUID `json:"realization_id,omitempty"`
	ResourceRefID string     `json:"resource_ref_id,omitempty"`
	// Server-side only: what the run is launched with.
	MintedUUID string         `json:"-"`
	BuiltName  string         `json:"-"`
	Inputs     map[string]any `json:"-"`
	Declared   map[string]any `json:"-"`
}

// RuleParams is a declared rule in the provider's keys, as the firewall
// actions take it and as the provider reports it back.
func RuleParams(r envmanifest.FirewallRule) map[string]string {
	out := map[string]string{"enable": "1"}
	if r.Group != "" {
		out["type"], out["action"] = "group", r.Group
	} else {
		out["type"], out["action"] = r.Direction, r.Action
	}
	for k, v := range map[string]string{
		"macro": r.Macro, "proto": r.Proto, "source": r.Source, "dest": r.Dest,
		"dport": r.DPort, "sport": r.SPort, "comment": r.Comment,
	} {
		if v != "" {
			out[k] = v
		}
	}
	return out
}

// planFirewalls plans the firewall of every declared machine whose resource
// declares one. entries are the plan's machine lines, already classified.
func planFirewalls(env envmanifest.Environment, entries []Entry, world Snapshot) ([]FirewallEntry, []string, []string) {
	declared := map[string]*envmanifest.EnvironmentFirewall{}
	for _, r := range env.Spec.Resources {
		if r.Firewall != nil {
			declared[r.Name] = r.Firewall
		}
	}
	if len(declared) == 0 {
		return nil, nil, nil
	}
	byID := map[uuid.UUID]Realization{}
	for _, r := range world.Realizations {
		byID[r.ID] = r
	}

	var out []FirewallEntry
	clusterOff, clusterUnknown := false, false
	for _, e := range entries {
		fw := declared[e.Resource]
		// A line with no blueprint is not a declared instance: a machine the
		// manifest no longer declares, or a run still building one.
		if fw == nil || e.Blueprint == "" {
			continue
		}
		f := FirewallEntry{Resource: e.Resource, Index: e.Index, Rules: len(fw.Rules)}
		switch {
		case e.Action == ActionCreate:
			f.Action, f.Reason = ActionUnknown, "the machine does not exist yet; the apply after it is built sets its firewall"
		case e.RunID != nil:
			f.Action, f.Reason = ActionUnknown, "a run is already working on this machine; its firewall is compared once it finishes"
		case e.RealizationID == nil:
			f.Action, f.Reason = ActionUnknown, "the machine's record could not be read"
		default:
			r, ok := byID[*e.RealizationID]
			if !ok {
				f.Action, f.Reason = ActionUnknown, "the machine's record could not be read"
				break
			}
			id := r.ID
			f.RealizationID, f.ResourceRefID = &id, r.ResourceRefID
			seen, read := world.Firewalls[r.ID]
			if !read {
				reason := "the machine's firewall could not be read"
				if why := world.FirewallErrors[r.ID]; why != "" {
					reason += ": " + why
				}
				f.Action, f.Reason = ActionUnknown, reason
				break
			}
			if fw.Enabled {
				clusterOff = clusterOff || (seen.ClusterKnown && !seen.ClusterOn)
				clusterUnknown = clusterUnknown || !seen.ClusterKnown
			}
			f.Diff = diffFirewall(*fw, seen)
			if len(f.Diff) == 0 {
				f.Action, f.Reason = ActionNoOp, "as declared"
				break
			}
			f.Action, f.Reason = ActionChange, "differs from the declaration"
			decideFirewall(&f, e, r, *fw, world)
		}
		out = append(out, f)
	}

	var warnings, limits []string
	if clusterOff {
		warnings = append(warnings, ClusterFirewallOff)
	}
	if clusterUnknown {
		limits = append(limits, ClusterFirewallUnknown)
	}
	return out, warnings, limits
}

// decideFirewall sets Apply or Blocked on a firewall that differs.
func decideFirewall(f *FirewallEntry, e Entry, r Realization, fw envmanifest.EnvironmentFirewall, world Snapshot) {
	creator := builtBy(r, world.Blueprints)
	switch {
	case !r.Managed:
		f.Blocked = "Sybil does not claim authority over this machine (managed = false), so it does not change its firewall"
	case world.Connector == nil || r.IntegrationID != world.Connector.ID:
		f.Blocked = "built through another connector than the manifest names"
	case creator == nil:
		f.Blocked = "Sybil cannot tell which blueprint built this machine, so it does not change its firewall"
	case creator.FirewalledBy == "" || blueprintByID(world.Blueprints, creator.FirewalledBy) == nil:
		f.Blocked = "the blueprint that built this machine cannot set its firewall"
	case r.MintedUUID == "":
		f.Blocked = "no identity was recorded when this machine was built, so Sybil cannot prove a change would reach it"
	default:
		f.Apply = ApplyFirewall
		f.Blueprint = creator.FirewalledBy
		f.MintedUUID = r.MintedUUID
		f.BuiltName = builtName(r, &e)
		f.Declared = e.Inputs
		f.Inputs = firewallInputs(fw)
	}
}

// firewallInputs is what the firewall blueprint is run with, besides the
// machine's identity.
func firewallInputs(fw envmanifest.EnvironmentFirewall) map[string]any {
	rules := make([]map[string]string, 0, len(fw.Rules))
	for _, r := range fw.Rules {
		rules = append(rules, RuleParams(r))
	}
	encoded, _ := json.Marshal(rules)
	return map[string]any{
		"rules": string(encoded), "rule_count": len(rules), "enable": fw.Enabled,
		"policy_in": fw.PolicyIn, "policy_out": fw.PolicyOut,
		"firewall_digest": envmanifest.FirewallDigest(&fw),
	}
}

// diffFirewall lists how seen differs from fw, a sentence each. Empty means
// they agree.
func diffFirewall(fw envmanifest.EnvironmentFirewall, seen ObservedFirewall) []string {
	var out []string
	if fw.Enabled != seen.Enabled {
		out = append(out, fmt.Sprintf("firewall is %s, declared %s", onOff(seen.Enabled), onOff(fw.Enabled)))
	}
	if !strings.EqualFold(fw.PolicyIn, seen.PolicyIn) {
		out = append(out, fmt.Sprintf("inbound policy is %s, declared %s", seen.PolicyIn, fw.PolicyIn))
	}
	if !strings.EqualFold(fw.PolicyOut, seen.PolicyOut) {
		out = append(out, fmt.Sprintf("outbound policy is %s, declared %s", seen.PolicyOut, fw.PolicyOut))
	}
	want := make([]map[string]string, len(fw.Rules))
	for i, r := range fw.Rules {
		want[i] = RuleParams(r)
	}
	out = append(out, diffRules(want, seen.Rules)...)
	return out
}

// diffRules lists how the machine's rules (have) differ from the declared ones
// (want), a line per rule that is out of place, so that one rule added or
// removed by hand reads as that one rule and not as every rule below it
// shifted by one.
//
// The two lists are aligned on the longest run of rules they share in order.
// A rule left over on both sides is one that moved; within a stretch where they
// differ, the rest are paired off as "is …, declared …", and what is still left
// is missing or not declared. Numbers are positions: the machine's for a rule
// it has, the manifest's for one it lacks.
func diffRules(want, have []map[string]string) []string {
	// lcs[i][j] is the length of the longest common subsequence of want[i:]
	// and have[j:]. At most MaxFirewallRules on one side, so this is small.
	lcs := make([][]int, len(want)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(have)+1)
	}
	for i := len(want) - 1; i >= 0; i-- {
		for j := len(have) - 1; j >= 0; j-- {
			switch {
			case sameRule(want[i], have[j]):
				lcs[i][j] = lcs[i+1][j+1] + 1
			case lcs[i+1][j] >= lcs[i][j+1]:
				lcs[i][j] = lcs[i+1][j]
			default:
				lcs[i][j] = lcs[i][j+1]
			}
		}
	}
	// The stretches between aligned rules, each with what is left on either
	// side of it.
	type gap struct{ want, have []int }
	var gaps []gap
	cur := gap{}
	i, j := 0, 0
	for i < len(want) || j < len(have) {
		switch {
		case i < len(want) && j < len(have) && sameRule(want[i], have[j]) && lcs[i][j] == lcs[i+1][j+1]+1:
			gaps = append(gaps, cur)
			cur = gap{}
			i, j = i+1, j+1
		case i < len(want) && (j == len(have) || lcs[i+1][j] >= lcs[i][j+1]):
			cur.want = append(cur.want, i)
			i++
		default:
			cur.have = append(cur.have, j)
			j++
		}
	}
	gaps = append(gaps, cur)

	// A rule left over on both sides is in the wrong place, not missing.
	movedWant, movedHave := map[int]bool{}, map[int]bool{}
	var out []string
	for _, g := range gaps {
		for _, h := range g.have {
			for _, other := range gaps {
				for _, w := range other.want {
					if !movedWant[w] && !movedHave[h] && sameRule(want[w], have[h]) {
						movedWant[w], movedHave[h] = true, true
						out = append(out, fmt.Sprintf("rule %d (%s) is declared as rule %d", h+1, describeRule(have[h]), w+1))
					}
				}
			}
		}
	}
	for _, g := range gaps {
		var ws, hs []int
		for _, w := range g.want {
			if !movedWant[w] {
				ws = append(ws, w)
			}
		}
		for _, h := range g.have {
			if !movedHave[h] {
				hs = append(hs, h)
			}
		}
		k := 0
		for ; k < len(ws) && k < len(hs); k++ {
			out = append(out, fmt.Sprintf("rule %d is %s, declared %s", hs[k]+1, describeRule(have[hs[k]]), describeRule(want[ws[k]])))
		}
		for _, h := range hs[k:] {
			out = append(out, fmt.Sprintf("rule %d (%s) is not declared", h+1, describeRule(have[h])))
		}
		for _, w := range ws[k:] {
			out = append(out, fmt.Sprintf("rule %d (%s) is missing", w+1, describeRule(want[w])))
		}
	}
	return out
}

// sameRule compares two rules key by key. A key absent on one side and empty
// on the other is the same; the provider's own bookkeeping (pos, digest,
// ipversion) is never compared, because fw_guest_get does not report it.
func sameRule(want, have map[string]string) bool {
	keys := map[string]bool{}
	for k := range want {
		keys[k] = true
	}
	for k := range have {
		keys[k] = true
	}
	for k := range keys {
		a, b := want[k], have[k]
		if k == "type" || k == "proto" {
			a, b = strings.ToLower(a), strings.ToLower(b)
		}
		if a != b {
			return false
		}
	}
	return true
}

// describeRule renders a rule the way a person reads one: "in ACCEPT tcp
// dport 22 from 10.0.0.0/8".
func describeRule(r map[string]string) string {
	if r["type"] == "group" {
		s := "group " + r["action"]
		if r["enable"] == "0" {
			s += " (disabled)"
		}
		return s
	}
	parts := []string{r["type"], r["action"]}
	for _, k := range []string{"macro", "proto"} {
		if r[k] != "" {
			parts = append(parts, r[k])
		}
	}
	for _, kv := range [][2]string{{"dport", "port"}, {"sport", "sport"}, {"source", "from"}, {"dest", "to"}} {
		if r[kv[0]] != "" {
			parts = append(parts, kv[1]+" "+r[kv[0]])
		}
	}
	if r["iface"] != "" {
		parts = append(parts, "on "+r["iface"])
	}
	if r["enable"] == "0" {
		parts = append(parts, "(disabled)")
	}
	if r["comment"] != "" {
		parts = append(parts, fmt.Sprintf("%q", r["comment"]))
	}
	return strings.Join(parts, " ")
}
