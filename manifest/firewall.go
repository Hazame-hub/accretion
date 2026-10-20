package manifest

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// A resource's firewall, declared in the manifest.
//
// The hypervisor's guest firewall, not one inside the machine: on Proxmox it
// filters on the tap interface, below the guest, so a compromised guest cannot
// switch it off. The actions that set it already existed; this is what lets a
// manifest say what it should be, so a plan compares it and an apply converges
// it like any other declared property.
//
// The rules list is the WHOLE list, as with the actions it is realized by: a
// rule removed from the manifest is removed from the machine, and an apply that
// runs twice writes each rule once. A resource with no firewall block leaves
// the machine's firewall alone, which is not the same as a block with no
// rules: that one converges to no rules.

// MaxFirewallRules bounds one resource's rule list. A list this long is a
// policy that belongs in a security group, referenced by one rule.
const MaxFirewallRules = 100

// EnvironmentFirewall is a resource's declared firewall.
type EnvironmentFirewall struct {
	// Enabled is the guest's own firewall switch. Defaults to true: a block
	// that declares rules and leaves them filtering nothing is never what it
	// was written to mean.
	Enabled bool
	// PolicyIn and PolicyOut are what happens to traffic no rule matches:
	// ACCEPT, DROP or REJECT. They default to Proxmox's own defaults, DROP in
	// and ACCEPT out, so leaving them out declares what a guest has anyway.
	PolicyIn  string
	PolicyOut string
	// Rules are evaluated in order, first match wins.
	Rules []FirewallRule
}

// FirewallRule is one rule. Exactly one of Group and (Direction, Action) is
// set: a rule either references a security group, or filters itself.
type FirewallRule struct {
	// Direction is "in" or "out".
	Direction string
	// Action is ACCEPT, DROP or REJECT.
	Action string
	// Group names a security group defined on the cluster, whose rules apply
	// here in this rule's place.
	Group string
	// Macro is a provider's named service (SSH, HTTP, DNS…), standing for
	// its protocol and ports.
	Macro   string
	Proto   string
	Source  string
	Dest    string
	DPort   string
	SPort   string
	Comment string
}

// Default policies, Proxmox's own.
const (
	DefaultFirewallPolicyIn  = "DROP"
	DefaultFirewallPolicyOut = "ACCEPT"
)

type environmentFirewallWire struct {
	Enabled   *bool              `yaml:"enabled,omitempty"`
	PolicyIn  string             `yaml:"policy_in,omitempty"`
	PolicyOut string             `yaml:"policy_out,omitempty"`
	Rules     []firewallRuleWire `yaml:"rules"`
}

type firewallRuleWire struct {
	Direction string `yaml:"direction,omitempty"`
	Action    string `yaml:"action,omitempty"`
	Group     string `yaml:"group,omitempty"`
	Macro     string `yaml:"macro,omitempty"`
	Proto     string `yaml:"proto,omitempty"`
	Source    string `yaml:"source,omitempty"`
	Dest      string `yaml:"dest,omitempty"`
	DPort     string `yaml:"dport,omitempty"`
	SPort     string `yaml:"sport,omitempty"`
	Comment   string `yaml:"comment,omitempty"`
}

var (
	// firewallGroupName is what Proxmox accepts as a security group's name.
	firewallGroupName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{1,17}$`)
	firewallMacro     = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,31}$`)
	firewallProto     = regexp.MustCompile(`^[a-z0-9-]{1,16}$`)
	// An address, a CIDR, a range, an ipset (+name) or an alias, or a
	// comma-separated list of them. The provider says which of those it
	// knows; this only keeps out what could never be one.
	firewallAddress = regexp.MustCompile(`^[A-Za-z0-9.:/+_,-]{1,512}$`)
	// A port, a range (8000:8080) or a list of them, or a service name.
	firewallPort = regexp.MustCompile(`^[A-Za-z0-9:,-]{1,128}$`)
)

func decodeFirewall(w *environmentFirewallWire) (*EnvironmentFirewall, error) {
	if w == nil {
		return nil, nil
	}
	fw := &EnvironmentFirewall{
		Enabled:   true,
		PolicyIn:  strings.ToUpper(strings.TrimSpace(w.PolicyIn)),
		PolicyOut: strings.ToUpper(strings.TrimSpace(w.PolicyOut)),
		Rules:     []FirewallRule{},
	}
	if w.Enabled != nil {
		fw.Enabled = *w.Enabled
	}
	if fw.PolicyIn == "" {
		fw.PolicyIn = DefaultFirewallPolicyIn
	}
	if fw.PolicyOut == "" {
		fw.PolicyOut = DefaultFirewallPolicyOut
	}
	for _, r := range w.Rules {
		rule := FirewallRule{
			Direction: strings.ToLower(strings.TrimSpace(r.Direction)),
			Action:    strings.ToUpper(strings.TrimSpace(r.Action)),
			Group:     strings.TrimSpace(r.Group),
			Macro:     strings.TrimSpace(r.Macro),
			Proto:     strings.ToLower(strings.TrimSpace(r.Proto)),
			Source:    strings.TrimSpace(r.Source),
			Dest:      strings.TrimSpace(r.Dest),
			DPort:     strings.TrimSpace(r.DPort),
			SPort:     strings.TrimSpace(r.SPort),
			Comment:   strings.TrimSpace(r.Comment),
		}
		// A filtering rule with no direction is inbound: that is what nearly
		// every rule a manifest declares is ("let 443 in").
		if rule.Group == "" && rule.Direction == "" {
			rule.Direction = "in"
		}
		fw.Rules = append(fw.Rules, rule)
	}
	return fw, fw.validate()
}

func encodeFirewall(fw *EnvironmentFirewall) *environmentFirewallWire {
	if fw == nil {
		return nil
	}
	enabled := fw.Enabled
	w := &environmentFirewallWire{Enabled: &enabled, PolicyIn: fw.PolicyIn, PolicyOut: fw.PolicyOut,
		Rules: []firewallRuleWire{}}
	for _, r := range fw.Rules {
		w.Rules = append(w.Rules, firewallRuleWire{
			Direction: r.Direction, Action: r.Action, Group: r.Group, Macro: r.Macro, Proto: r.Proto,
			Source: r.Source, Dest: r.Dest, DPort: r.DPort, SPort: r.SPort, Comment: r.Comment,
		})
	}
	return w
}

func (fw *EnvironmentFirewall) validate() error {
	if fw == nil {
		return nil
	}
	for key, p := range map[string]string{"policy_in": fw.PolicyIn, "policy_out": fw.PolicyOut} {
		switch p {
		case "ACCEPT", "DROP", "REJECT":
		default:
			return fmt.Errorf("firewall %s is %q; want ACCEPT, DROP or REJECT", key, p)
		}
	}
	if len(fw.Rules) > MaxFirewallRules {
		return fmt.Errorf("firewall has %d rules, at most %d; put a long policy in a security group and reference it",
			len(fw.Rules), MaxFirewallRules)
	}
	for i, r := range fw.Rules {
		if err := r.validate(); err != nil {
			return fmt.Errorf("firewall rules[%d]: %w", i, err)
		}
	}
	return nil
}

func (r FirewallRule) validate() error {
	if r.Group != "" {
		if !firewallGroupName.MatchString(r.Group) {
			return fmt.Errorf("group %q is not a security group name (2-18 letters, digits, - and _)", r.Group)
		}
		// A group rule is the group's rules, in this position. Anything else
		// on it would be silently ignored by the provider.
		if r.Action != "" || r.Macro != "" || r.Proto != "" || r.Source != "" || r.Dest != "" || r.DPort != "" || r.SPort != "" {
			return fmt.Errorf("references group %q, so it takes no action, macro, proto, addresses or ports: the group's own rules say those", r.Group)
		}
		if r.Direction != "" {
			return fmt.Errorf("references group %q, so it takes no direction: each of the group's rules has its own", r.Group)
		}
		return r.validateComment()
	}
	switch r.Direction {
	case "in", "out":
	default:
		return fmt.Errorf("direction %q; want in or out", r.Direction)
	}
	switch r.Action {
	case "ACCEPT", "DROP", "REJECT":
	case "":
		return fmt.Errorf("has no action; want ACCEPT, DROP or REJECT, or group: to reference a security group")
	default:
		return fmt.Errorf("action %q; want ACCEPT, DROP or REJECT", r.Action)
	}
	if r.Macro != "" && !firewallMacro.MatchString(r.Macro) {
		return fmt.Errorf("macro %q is not a macro name such as SSH or HTTPS", r.Macro)
	}
	if r.Macro != "" && r.Proto != "" {
		return fmt.Errorf("sets both macro %q and proto %q; a macro already says its protocol", r.Macro, r.Proto)
	}
	if r.Proto != "" && !firewallProto.MatchString(r.Proto) {
		return fmt.Errorf("proto %q is not a protocol such as tcp, udp or icmp", r.Proto)
	}
	for key, v := range map[string]string{"source": r.Source, "dest": r.Dest} {
		if v != "" && !firewallAddress.MatchString(v) {
			return fmt.Errorf("%s %q is not an address, CIDR, range, +ipset or alias", key, v)
		}
	}
	for key, v := range map[string]string{"dport": r.DPort, "sport": r.SPort} {
		if v == "" {
			continue
		}
		if !firewallPort.MatchString(v) {
			return fmt.Errorf("%s %q is not a port, a range such as 8000:8080, or a list of them", key, v)
		}
		// Proxmox refuses a port without a protocol that has ports, and a
		// macro brings its own.
		if r.Proto != "tcp" && r.Proto != "udp" {
			return fmt.Errorf("%s needs proto: tcp or udp", key)
		}
	}
	return r.validateComment()
}

func (r FirewallRule) validateComment() error {
	if strings.ContainsAny(r.Comment, "\r\n") || len(r.Comment) > 256 {
		return fmt.Errorf("comment must be one line of at most 256 characters")
	}
	return nil
}

// FirewallDigest identifies a declared firewall: the same declaration has the
// same digest, any change to it a different one. A run that sets a firewall
// carries the digest of the one it was planned from, so it can refuse to write
// a declaration that has moved on since its approval. "" for nil.
func FirewallDigest(fw *EnvironmentFirewall) string {
	if fw == nil {
		return ""
	}
	b, err := json.Marshal(fw)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:8])
}
