package manifest

import (
	"reflect"
	"strings"
	"testing"
)

const firewallManifest = `apiVersion: sybil/v1alpha1
kind: Environment
metadata:
  name: web
spec:
  integration: pve-lab
  resources:
    - name: web
      blueprint: provision-proxmox-vm
      firewall:
        policy_in: drop
        rules:
          - action: accept
            proto: TCP
            dport: 443
          - direction: in
            action: ACCEPT
            macro: SSH
            source: 10.0.0.0/8
            comment: admins
          - group: monitoring
`

func TestAFirewallDecodesWithProxmoxDefaults(t *testing.T) {
	env := mustDecodeEnv(t, firewallManifest)
	fw := env.Spec.Resources[0].Firewall
	if fw == nil {
		t.Fatal("firewall not decoded")
	}
	if !fw.Enabled || fw.PolicyIn != "DROP" || fw.PolicyOut != "ACCEPT" {
		t.Errorf("options = %+v", fw)
	}
	want := []FirewallRule{
		// An integer port is a port; a rule with no direction is inbound.
		{Direction: "in", Action: "ACCEPT", Proto: "tcp", DPort: "443"},
		{Direction: "in", Action: "ACCEPT", Macro: "SSH", Source: "10.0.0.0/8", Comment: "admins"},
		{Group: "monitoring"},
	}
	if !reflect.DeepEqual(fw.Rules, want) {
		t.Errorf("rules =\n%+v\nwant\n%+v", fw.Rules, want)
	}
}

func TestAResourceWithoutAFirewallLeavesItAlone(t *testing.T) {
	env := mustDecodeEnv(t, envManifest)
	if env.Spec.Resources[0].Firewall != nil {
		t.Fatal("a resource with no firewall block must decode to nil, not an empty firewall")
	}
	empty := mustDecodeEnv(t, strings.Replace(envManifest, "      blueprint: provision-from-schema\n      inputs:",
		"      blueprint: provision-from-schema\n      firewall:\n        rules: []\n      inputs:", 1))
	if fw := empty.Spec.Resources[0].Firewall; fw == nil || len(fw.Rules) != 0 || !fw.Enabled {
		t.Fatalf("an empty rule list is a declaration of no rules, got %+v", fw)
	}
}

func TestAFirewallSurvivesTheRoundTrip(t *testing.T) {
	first := mustDecodeEnv(t, firewallManifest)
	doc, err := EncodeEnvironment(first)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	if err := EncodeDocuments(&buf, []Document{doc}); err != nil {
		t.Fatal(err)
	}
	second := mustDecodeEnv(t, buf.String())
	if !reflect.DeepEqual(first, second) {
		t.Errorf("round trip drifted:\n%s", buf.String())
	}
	if FirewallDigest(first.Spec.Resources[0].Firewall) != FirewallDigest(second.Spec.Resources[0].Firewall) {
		t.Error("the same declaration must have the same digest")
	}
}

func TestTheDigestMovesWithTheDeclaration(t *testing.T) {
	a := mustDecodeEnv(t, firewallManifest).Spec.Resources[0].Firewall
	b := mustDecodeEnv(t, strings.Replace(firewallManifest, "dport: 443", "dport: 8443", 1)).Spec.Resources[0].Firewall
	if FirewallDigest(a) == FirewallDigest(b) || FirewallDigest(a) == "" {
		t.Errorf("digests %q and %q", FirewallDigest(a), FirewallDigest(b))
	}
	if FirewallDigest(nil) != "" {
		t.Error("no firewall has no digest")
	}
}

func TestAFirewallThatCannotMeanWhatItSaysIsRefused(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"bad policy":         {"policy_in: drop", "policy_in: allow", "policy_in"},
		"no action":          {"          - action: accept\n", "          - comment: x\n", "has no action"},
		"bad action":         {"action: accept\n", "action: permit\n", "action \"PERMIT\""},
		"port without proto": {"            proto: TCP\n", "", "dport needs proto"},
		"macro and proto":    {"            macro: SSH\n", "            macro: SSH\n            proto: tcp\n", "a macro already says"},
		"group with action":  {"          - group: monitoring\n", "          - group: monitoring\n            action: ACCEPT\n", "takes no action"},
		"group direction":    {"          - group: monitoring\n", "          - group: monitoring\n            direction: in\n", "takes no direction"},
		"bad direction":      {"direction: in", "direction: both", "direction \"both\""},
		"bad address":        {"source: 10.0.0.0/8", "source: 10.0.0.0/8; rm", "not an address"},
		"two-line comment":   {"comment: admins", "comment: \"a\\nb\"", "one line"},
		"unknown key":        {"comment: admins", "comment: admins\n            log: info", "log"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			src := strings.Replace(firewallManifest, c.from, c.to, 1)
			if src == firewallManifest {
				t.Fatalf("the edit %q did not apply", c.from)
			}
			_, err := decodeOneEnv(t, src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}
