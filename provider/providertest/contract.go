package providertest

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/hazame-hub/accretion/provider"
)

// Contract is what every provider — Proxmox, vSphere, the Fake — must
// satisfy. A provider package calls it from its own test with a Config that
// reaches whatever stands in for the far side (an httptest server, vcsim, the
// Fake itself).
//
// The secret is the string the test used as the credential. The suite asserts
// it never appears in anything the session produced: not an error, not a
// resource name, not the raw payload. That is the one rule a provider package
// is trusted with, and the one this suite exists to check.
func Contract(t *testing.T, p provider.Provider, cfg provider.Config, secret string) {
	t.Helper()
	ctx := context.Background()

	// Connect twice is idempotent: the second session sees the same estate.
	s1, err := p.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	s2, err := p.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("second Connect: %v", err)
	}
	t.Cleanup(func() { _ = s2.Close() })

	caps, err := s1.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	if len(caps.Kinds) == 0 {
		t.Error("Capabilities.Kinds is empty; a provider that discovers nothing has no business being one")
	}

	first, err := s1.Discover(ctx)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	second, err := s2.Discover(ctx)
	if err != nil {
		t.Fatalf("second Discover: %v", err)
	}
	if !sameRefs(first, second) {
		t.Errorf("Discover is not stable across two sessions: %v vs %v", refsOf(first), refsOf(second))
	}

	seen := map[provider.Ref]bool{}
	declared := map[provider.Kind]bool{}
	for _, k := range caps.Kinds {
		declared[k] = true
	}
	for _, r := range first {
		if seen[r.Ref] {
			t.Errorf("Ref %v returned twice", r.Ref)
		}
		seen[r.Ref] = true
		if r.Ref.ID == "" || r.Ref.Kind == "" {
			t.Errorf("resource %q has an incomplete Ref %+v", r.Name, r.Ref)
		}
		if !declared[r.Ref.Kind] {
			t.Errorf("resource %q has kind %q, which Capabilities.Kinds did not declare", r.Name, r.Ref.Kind)
		}
		if len(r.Raw) == 0 || !json.Valid(r.Raw) {
			t.Errorf("resource %q has no valid Raw payload; the provider's own record must be kept verbatim", r.Name)
		}
		if r.Tags == nil || r.Identity.MACs == nil || r.Identity.IPs == nil {
			t.Errorf("resource %q has a nil slice (Tags/MACs/IPs); these serialise as null and break consumers", r.Name)
		}
		if caps.GuestUUID && r.Ref.Kind == provider.KindVM && r.Identity.SMBIOSUUID != "" && provider.UUIDForms(r.Identity.SMBIOSUUID) == nil {
			t.Errorf("resource %q reports an SMBIOS UUID %q that UUIDForms rejects; report it canonical or not at all", r.Name, r.Identity.SMBIOSUUID)
		}
		for _, m := range r.Identity.MACs {
			if strings.ToLower(m) != m {
				t.Errorf("resource %q has MAC %q that is not lower-cased; use provider.NormalizeMACs", r.Name, m)
			}
		}
		if caps.Memory && r.Ref.Kind.Importable() && r.Memory.CurrentBytes == 0 && r.State == "running" {
			t.Errorf("resource %q: Capabilities.Memory is true but CurrentBytes is 0 on a running machine", r.Name)
		}
		if !caps.Hierarchy && r.Parent != nil {
			t.Errorf("resource %q has a Parent but Capabilities.Hierarchy is false", r.Name)
		}

		facts, err := s1.Describe(ctx, r.Ref)
		if err != nil {
			t.Errorf("Describe(%v): %v — every discovered ref must be describable", r.Ref, err)
			continue
		}
		if !caps.Describe && !sameFacts(facts, r.Facts) {
			t.Errorf("Describe(%v) differs from the Discover row while Capabilities.Describe is false", r.Ref)
		}
		leak(t, secret, fmt.Sprintf("resource %q", r.Name), r.Name, string(r.Raw), strings.Join(r.Tags, ","), r.Identity.Hostname)
	}

	if _, err := s1.Describe(ctx, provider.Ref{Kind: provider.KindVM, ID: "no-such-ref-for-the-contract"}); err == nil {
		t.Error("Describe of an unknown ref must fail, not invent facts")
	} else {
		leak(t, secret, "Describe error", err.Error())
	}

	if err := s1.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	leak(t, secret, "Config.String", cfg.String())
}

func leak(t *testing.T, secret, where string, texts ...string) {
	t.Helper()
	if secret == "" {
		return
	}
	for _, s := range texts {
		if strings.Contains(s, secret) {
			t.Errorf("%s contains the credential; a provider must never echo what it was connected with", where)
		}
	}
}

func refsOf(rs []provider.Resource) []provider.Ref {
	out := make([]provider.Ref, len(rs))
	for i, r := range rs {
		out[i] = r.Ref
	}
	return out
}

func sameRefs(a, b []provider.Resource) bool {
	if len(a) != len(b) {
		return false
	}
	set := map[provider.Ref]bool{}
	for _, r := range a {
		set[r.Ref] = true
	}
	for _, r := range b {
		if !set[r.Ref] {
			return false
		}
	}
	return true
}

func sameFacts(a, b provider.Facts) bool {
	ja, _ := json.Marshal(a)
	jb, _ := json.Marshal(b)
	return string(ja) == string(jb)
}
