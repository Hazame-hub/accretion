package plan

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

var firewallBP = Blueprint{ID: "set-firewall"}

func firewallWorld() Snapshot {
	w := sizedWorld()
	bp := sizedBlueprint
	bp.FirewalledBy = firewallBP.ID
	w.Blueprints = []Blueprint{bp, reconfigureBP, destroyBP, firewallBP}
	w.Firewalls = map[uuid.UUID]ObservedFirewall{}
	w.FirewallErrors = map[uuid.UUID]string{}
	return w
}

func walled(count int, fw *envmanifest.EnvironmentFirewall) envmanifest.Environment {
	r := web(count)
	r.Firewall = fw
	return env(r)
}

func httpsOnly() *envmanifest.EnvironmentFirewall {
	return &envmanifest.EnvironmentFirewall{Enabled: true, PolicyIn: "DROP", PolicyOut: "ACCEPT", Rules: []envmanifest.FirewallRule{
		{Direction: "in", Action: "ACCEPT", Proto: "tcp", DPort: "443"},
		{Group: "monitoring"},
	}}
}

// asDeclared is what fw_guest_get reports for a guest set to httpsOnly.
func asDeclared() ObservedFirewall {
	return ObservedFirewall{Enabled: true, PolicyIn: "DROP", PolicyOut: "ACCEPT", ClusterKnown: true, ClusterOn: true,
		Rules: []map[string]string{
			{"type": "in", "action": "ACCEPT", "proto": "tcp", "dport": "443", "enable": "1"},
			{"type": "group", "action": "monitoring", "enable": "1"},
		}}
}

func TestAFirewallAsDeclaredPlansAsUnchanged(t *testing.T) {
	w := firewallWorld()
	r := builtMachine(&w, 0, built("web-1", 2048))
	w.Realizations = []Realization{r}
	w.Firewalls[r.ID] = asDeclared()

	p := Build(walled(1, httpsOnly()), w)
	if len(p.Firewalls) != 1 || p.Firewalls[0].Action != ActionNoOp {
		t.Fatalf("firewalls = %+v", p.Firewalls)
	}
	if len(p.Warnings) != 0 {
		t.Errorf("warnings = %v", p.Warnings)
	}
	if !strings.Contains(p.Summary(), "firewalls: 1 unchanged") {
		t.Errorf("summary = %q", p.Summary())
	}
}

func TestAFirewallThatDiffersIsSetUnderTheMintedIdentity(t *testing.T) {
	w := firewallWorld()
	r := builtMachine(&w, 0, built("web-1", 2048))
	w.Realizations = []Realization{r}
	seen := asDeclared()
	seen.PolicyIn = "ACCEPT"
	// Somebody opened 22 by hand, above the declared rules.
	seen.Rules = append([]map[string]string{{"type": "in", "action": "ACCEPT", "proto": "tcp", "dport": "22", "enable": "1"}}, seen.Rules...)
	w.Firewalls[r.ID] = seen

	fw := httpsOnly()
	p := Build(walled(1, fw), w)
	f := p.Firewalls[0]
	if f.Action != ActionChange || f.Apply != ApplyFirewall || f.Blueprint != "set-firewall" || f.Blocked != "" {
		t.Fatalf("entry = %+v", f)
	}
	if f.MintedUUID != minted || f.ResourceRefID != r.ResourceRefID || f.BuiltName != "web-1" {
		t.Errorf("guards = %q %q %q", f.MintedUUID, f.ResourceRefID, f.BuiltName)
	}
	diff := strings.Join(f.Diff, "\n")
	for _, want := range []string{
		"inbound policy is ACCEPT, declared DROP",
		"rule 1 (in ACCEPT tcp port 22) is not declared",
	} {
		if !strings.Contains(diff, want) {
			t.Errorf("diff lacks %q:\n%s", want, diff)
		}
	}
	if len(f.Diff) != 2 {
		t.Errorf("one rule added by hand is one line, not every rule below it:\n%s", diff)
	}
	if f.Inputs["firewall_digest"] != envmanifest.FirewallDigest(fw) || f.Inputs["rule_count"] != 2 {
		t.Errorf("inputs = %v", f.Inputs)
	}
	var rules []map[string]string
	if err := json.Unmarshal([]byte(f.Inputs["rules"].(string)), &rules); err != nil || len(rules) != 2 ||
		rules[1]["type"] != "group" || rules[1]["action"] != "monitoring" {
		t.Errorf("rules = %v (%v)", rules, err)
	}
	if f.Declared["hostname"] != "web-1" {
		t.Errorf("declared = %v; intent_check compares the machine's declaration", f.Declared)
	}
}

func TestAFirewallIsNotPlannedAgainstWhatCouldNotBeSeen(t *testing.T) {
	w := firewallWorld()
	gone := builtMachine(&w, 0, built("web-1", 2048))
	w.Realizations = []Realization{gone}
	w.FirewallErrors[gone.ID] = "connection refused"

	p := Build(walled(2, httpsOnly()), w)
	if len(p.Firewalls) != 2 {
		t.Fatalf("firewalls = %+v", p.Firewalls)
	}
	if f := p.Firewalls[0]; f.Action != ActionUnknown || !strings.Contains(f.Reason, "connection refused") || f.Apply != "" {
		t.Errorf("unread = %+v", f)
	}
	if f := p.Firewalls[1]; f.Action != ActionUnknown || !strings.Contains(f.Reason, "does not exist yet") {
		t.Errorf("to create = %+v", f)
	}
}

func TestAClusterWithItsFirewallOffIsSaid(t *testing.T) {
	w := firewallWorld()
	r := builtMachine(&w, 0, built("web-1", 2048))
	w.Realizations = []Realization{r}
	seen := asDeclared()
	seen.ClusterOn = false
	w.Firewalls[r.ID] = seen

	p := Build(walled(1, httpsOnly()), w)
	if len(p.Warnings) != 1 || p.Warnings[0] != ClusterFirewallOff {
		t.Errorf("warnings = %v", p.Warnings)
	}
	seen.ClusterKnown = false
	w.Firewalls[r.ID] = seen
	p = Build(walled(1, httpsOnly()), w)
	found := false
	for _, l := range p.Limits {
		found = found || l == ClusterFirewallUnknown
	}
	if !found {
		t.Errorf("limits = %v", p.Limits)
	}
}

func TestAFirewallAnApplyCannotSetSaysWhy(t *testing.T) {
	w := firewallWorld()
	r := builtMachine(&w, 0, built("web-1", 2048))
	w.Realizations = []Realization{r}
	w.Firewalls[r.ID] = ObservedFirewall{PolicyIn: "DROP", PolicyOut: "ACCEPT", ClusterKnown: true, ClusterOn: true}
	w.Blueprints = []Blueprint{sizedBlueprint, reconfigureBP, destroyBP}

	f := Build(walled(1, httpsOnly()), w).Firewalls[0]
	if f.Action != ActionChange || f.Apply != "" || !strings.Contains(f.Blocked, "cannot set its firewall") {
		t.Errorf("entry = %+v", f)
	}
}

func TestAResourceWithoutAFirewallHasNoFirewallLines(t *testing.T) {
	w := firewallWorld()
	w.Realizations = []Realization{builtMachine(&w, 0, built("web-1", 2048))}
	if p := Build(walled(1, nil), w); p.Firewalls != nil {
		t.Errorf("firewalls = %+v", p.Firewalls)
	}
}

// The lab's case: two declared rules, one added by hand at the top and the
// declared ones reordered below it. Each line names one rule that is out of
// place, by the position a person sees in the provider's own list.
func TestARuleDiffNamesOnlyTheRulesThatDiffer(t *testing.T) {
	r := func(port, comment string) map[string]string {
		return map[string]string{"type": "in", "action": "ACCEPT", "proto": "tcp", "dport": port, "comment": comment, "enable": "1"}
	}
	ssh, https, ftp, smtp := r("22", "ssh"), r("443", "https"), r("21", "ftp"), r("25", "smtp")
	for name, tc := range map[string]struct {
		want, have []map[string]string
		lines      []string
	}{
		"as declared": {[]map[string]string{ssh, https}, []map[string]string{ssh, https}, nil},
		"one added on top": {[]map[string]string{ssh, https}, []map[string]string{ftp, ssh, https},
			[]string{`rule 1 (in ACCEPT tcp port 21 "ftp") is not declared`}},
		"one removed": {[]map[string]string{ssh, https}, []map[string]string{https},
			[]string{`rule 1 (in ACCEPT tcp port 22 "ssh") is missing`}},
		"one changed in place": {[]map[string]string{ssh, https}, []map[string]string{ssh, smtp},
			[]string{`rule 2 is in ACCEPT tcp port 25 "smtp", declared in ACCEPT tcp port 443 "https"`}},
		"reversed": {[]map[string]string{ssh, https}, []map[string]string{https, ssh},
			[]string{`rule 2 (in ACCEPT tcp port 22 "ssh") is declared as rule 1`}},
		"added and moved": {[]map[string]string{ssh, https}, []map[string]string{ftp, https, ssh}, []string{
			`rule 3 (in ACCEPT tcp port 22 "ssh") is declared as rule 1`,
			`rule 1 (in ACCEPT tcp port 21 "ftp") is not declared`,
		}},
		"none left": {[]map[string]string{ssh, https}, nil, []string{
			`rule 1 (in ACCEPT tcp port 22 "ssh") is missing`,
			`rule 2 (in ACCEPT tcp port 443 "https") is missing`,
		}},
	} {
		got := diffRules(tc.want, tc.have)
		if strings.Join(got, "\n") != strings.Join(tc.lines, "\n") {
			t.Errorf("%s:\n got %q\nwant %q", name, got, tc.lines)
		}
	}
}
