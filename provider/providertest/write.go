package providertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hazame-hub/accretion/provider"
)

// WriteFixture is what WriteContract needs from the far side a provider's
// test stands up: a template to clone, where to put the clone and its seed,
// a guest that is not the suite's, and a way to interrupt the next clone.
type WriteFixture struct {
	// Template is a template the session can clone.
	Template provider.Ref
	// Placement is where clones go ("" for the template's).
	Placement string
	// SeedStorage is a storage that takes seeds.
	SeedStorage string
	// Foreign is a running guest the suite did not make, and ForeignUUID the
	// SMBIOS UUID it carries. Every verb aimed at it under another identity
	// must be refused, and leave it running with its identity.
	Foreign     provider.Ref
	ForeignUUID string
	// InterruptNextClone makes the next Clone lose its answer after the
	// machine exists (with whatever identity cloning gives it) and before the
	// minted identity is written, so the caller sees an
	// *OutcomeUnknownError. The suite then retries and expects one machine,
	// finished.
	InterruptNextClone func()
}

// Two identities the suite mints. Fixed, so a failure is reproducible.
const (
	writeUUID1 = "5a1f0c3e-8d2b-4f6a-9e7c-1b3d5f7a9c0e"
	writeUUID2 = "7c2e1d4f-9a3b-4c5d-8e6f-2a4b6c8d0e1f"
)

// WriteContract is what every provider.Writer must satisfy: create,
// idempotent retry, an interrupted clone resumed without a duplicate, delete
// refused on a running or unmarked guest, delete idempotent, and the
// credential never in an answer.
func WriteContract(t *testing.T, p provider.Provider, cfg provider.Config, secret string, fx WriteFixture) {
	t.Helper()
	ctx := context.Background()
	s, err := p.Connect(ctx, cfg)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	w, ok := s.(provider.Writer)
	if !ok {
		t.Fatal("the session does not implement provider.Writer")
	}
	caps, err := s.Capabilities(ctx)
	if err != nil {
		t.Fatalf("Capabilities: %v", err)
	}
	wc := caps.Write
	if !wc.Clone || !wc.GuestInit || !wc.Power || !wc.Delete {
		t.Fatalf("Capabilities.Write %+v: a Writer must declare the verbs it offers", wc)
	}
	check := func(what string, err error) {
		t.Helper()
		if err != nil {
			leak(t, secret, what, err.Error())
		}
	}
	refused := func(what string, err error) {
		t.Helper()
		check(what, err)
		var r *provider.RefusedError
		if !errors.As(err, &r) {
			t.Errorf("%s: want a *provider.RefusedError, got %v", what, err)
		}
	}

	req := provider.CloneRequest{
		Template: fx.Template, Name: "wc-one", Placement: fx.Placement, Full: true,
		MintUUID: writeUUID1, OwnerMark: "accrete:write-contract-1",
		Shape: provider.Shape{Cores: 2, MemoryMB: 1024},
	}

	// Refused before anything is made.
	bad := req
	bad.MintUUID = "not-a-uuid"
	_, err = w.Clone(ctx, bad)
	refused("Clone with an unusable MintUUID", err)

	// Create.
	m, err := w.Clone(ctx, req)
	check("Clone", err)
	if err != nil {
		t.Fatalf("Clone: %v", err)
	}
	if !m.Applied {
		t.Error("first Clone: Applied is false")
	}
	if wc.MintUUID && !sameUUID(m.Identity.SMBIOSUUID, writeUUID1) {
		t.Errorf("Clone: identity %q, want the minted %s", m.Identity.SMBIOSUUID, writeUUID1)
	}
	if got := count(t, s, "wc-one"); got != 1 {
		t.Fatalf("after Clone, %d machines are named wc-one, want 1", got)
	}
	if r := find(t, s, m.Ref); r == nil {
		t.Fatalf("Discover does not list the clone at %v", m.Ref)
	} else if wc.MintUUID && !sameUUID(r.Identity.SMBIOSUUID, writeUUID1) {
		t.Errorf("Discover: the clone carries %q, want %s", r.Identity.SMBIOSUUID, writeUUID1)
	}
	shaped(t, s, m.Ref, req.Shape)

	// Idempotent retry: the same request finds the same machine.
	again, err := w.Clone(ctx, req)
	check("second Clone", err)
	if err != nil {
		t.Fatalf("second Clone: %v", err)
	}
	if again.Ref != m.Ref || again.Applied {
		t.Errorf("second Clone = %+v, want %v with Applied false", again, m.Ref)
	}
	if got := count(t, s, "wc-one"); got != 1 {
		t.Errorf("after a retried Clone, %d machines are named wc-one, want 1", got)
	}

	target := provider.Target{Ref: m.Ref, ExpectUUID: writeUUID1, Name: "wc-one"}
	seed := provider.Seed{UserData: []byte("#cloud-config\n"), MetaData: []byte("instance-id: " + writeUUID1 + "\n"),
		Storage: fx.SeedStorage, InstanceID: writeUUID1}
	pl, err := w.GuestInit(ctx, target, seed)
	check("GuestInit", err)
	if err != nil {
		t.Fatalf("GuestInit: %v", err)
	}
	if !pl.Applied || pl.Volume == "" {
		t.Errorf("first GuestInit = %+v, want a volume, Applied", pl)
	}
	pl2, err := w.GuestInit(ctx, target, seed)
	check("second GuestInit", err)
	if err != nil || pl2.Applied || pl2.Volume != pl.Volume {
		t.Errorf("second GuestInit = %+v, %v; want the same volume, Applied false", pl2, err)
	}

	on, err := w.Power(ctx, target, provider.PowerRunning)
	check("Power", err)
	if err != nil || on.State != provider.PowerRunning || !on.Applied {
		t.Fatalf("Power(running) = %+v, %v", on, err)
	}
	on2, err := w.Power(ctx, target, provider.PowerRunning)
	check("second Power", err)
	if err != nil || on2.Applied {
		t.Errorf("second Power(running) = %+v, %v; want Applied false", on2, err)
	}

	// Identity is the guard.
	wrong := target
	wrong.ExpectUUID = writeUUID2
	_, err = w.Power(ctx, wrong, provider.PowerStopped)
	refused("Power under another identity", err)
	_, err = w.GuestInit(ctx, provider.Target{Ref: m.Ref}, seed)
	refused("GuestInit with no expected identity", err)
	if r := find(t, s, m.Ref); r == nil || r.State != "running" {
		t.Errorf("a refused Power changed the machine: %+v", r)
	}

	// Delete refuses a running guest, and a guest that is not this identity's.
	_, err = w.Delete(ctx, target, provider.DeleteOptions{})
	refused("Delete of a running guest without StopFirst", err)
	if find(t, s, m.Ref) == nil {
		t.Fatal("a refused Delete removed the machine")
	}
	_, err = w.Delete(ctx, provider.Target{Ref: fx.Foreign, ExpectUUID: writeUUID1}, provider.DeleteOptions{StopFirst: true})
	refused("Delete of a guest that does not carry the identity", err)
	if r := find(t, s, fx.Foreign); r == nil {
		t.Fatal("a refused Delete removed a guest that was not the suite's")
	} else if r.State != "running" || !sameUUID(r.Identity.SMBIOSUUID, fx.ForeignUUID) {
		t.Errorf("a refused Delete(StopFirst) touched a guest that was not the suite's: %s carrying %q", r.State, r.Identity.SMBIOSUUID)
	}
	// A placement that is not this machine's seed is not removed.
	other := pl
	other.Volume = strings.Replace(pl.Volume, strings.ToLower(writeUUID1), strings.ToLower(writeUUID2), 1)
	other.Volume = strings.Replace(other.Volume, writeUUID1, writeUUID2, 1)
	_, err = w.Delete(ctx, target, provider.DeleteOptions{StopFirst: true, RemovePlacements: []provider.Placement{other}})
	refused("Delete removing a seed that is not the machine's", err)
	if r := find(t, s, m.Ref); r == nil || r.State != "running" {
		t.Fatalf("a refused Delete changed the machine: %+v", r)
	}

	// Delete, then delete again.
	gone, err := w.Delete(ctx, target, provider.DeleteOptions{StopFirst: true, RemovePlacements: []provider.Placement{pl}})
	check("Delete", err)
	if err != nil || gone.AlreadyAbsent {
		t.Fatalf("Delete = %+v, %v", gone, err)
	}
	if find(t, s, m.Ref) != nil {
		t.Error("Delete succeeded and Discover still lists the machine")
	}
	gone2, err := w.Delete(ctx, target, provider.DeleteOptions{StopFirst: true, RemovePlacements: []provider.Placement{pl}})
	check("second Delete", err)
	if err != nil || !gone2.AlreadyAbsent {
		t.Errorf("second Delete = %+v, %v; want AlreadyAbsent", gone2, err)
	}

	// An interrupted clone, resumed.
	if fx.InterruptNextClone == nil {
		t.Fatal("WriteFixture.InterruptNextClone is required")
	}
	req2 := req
	req2.Name, req2.MintUUID, req2.OwnerMark = "wc-two", writeUUID2, "accrete:write-contract-2"
	fx.InterruptNextClone()
	_, err = w.Clone(ctx, req2)
	check("interrupted Clone", err)
	var unknown *provider.OutcomeUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("interrupted Clone: want a *provider.OutcomeUnknownError, got %v", err)
	}
	m2, err := w.Clone(ctx, req2)
	check("resumed Clone", err)
	if err != nil {
		t.Fatalf("resumed Clone: %v", err)
	}
	if !m2.Applied {
		t.Error("resumed Clone: Applied is false, but it had to write the identity")
	}
	shaped(t, s, m2.Ref, req2.Shape)
	if got := count(t, s, "wc-two"); got != 1 {
		t.Errorf("after an interrupted and resumed Clone, %d machines are named wc-two, want 1", got)
	}
	if r := find(t, s, m2.Ref); r == nil || (wc.MintUUID && !sameUUID(r.Identity.SMBIOSUUID, writeUUID2)) {
		t.Errorf("the resumed clone is %+v, want it at %v carrying %s", r, m2.Ref, writeUUID2)
	}
	if _, err := w.Delete(ctx, provider.Target{Ref: m2.Ref, ExpectUUID: writeUUID2}, provider.DeleteOptions{StopFirst: true}); err != nil {
		t.Errorf("Delete of the resumed clone: %v", err)
	}

	leak(t, secret, "results", fmt.Sprintf("%+v %+v %+v %+v %+v", m, pl, on, gone, m2))
}

// shaped checks the machine reports the cores and memory it was cloned with.
func shaped(t *testing.T, s provider.Session, ref provider.Ref, want provider.Shape) {
	t.Helper()
	r := find(t, s, ref)
	if r == nil {
		t.Fatalf("Discover does not list %v", ref)
	}
	if want.Cores > 0 && int(r.Facts.CPUCores) != want.Cores {
		t.Errorf("%v has %d cores, want the %d it was cloned with", ref, r.Facts.CPUCores, want.Cores)
	}
	if want.MemoryMB > 0 && r.Facts.RAMBytes != int64(want.MemoryMB)<<20 {
		t.Errorf("%v has %d bytes of memory, want the %d MiB it was cloned with", ref, r.Facts.RAMBytes, want.MemoryMB)
	}
}

func count(t *testing.T, s provider.Session, name string) int {
	t.Helper()
	rs, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	n := 0
	for _, r := range rs {
		if r.Name == name && r.Ref.Kind == provider.KindVM {
			n++
		}
	}
	return n
}

func find(t *testing.T, s provider.Session, ref provider.Ref) *provider.Resource {
	t.Helper()
	rs, err := s.Discover(context.Background())
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	for i := range rs {
		if rs[i].Ref == ref {
			return &rs[i]
		}
	}
	return nil
}

func sameUUID(got, want string) bool {
	for _, g := range provider.UUIDForms(got) {
		for _, w := range provider.UUIDForms(want) {
			if strings.EqualFold(g, w) {
				return true
			}
		}
	}
	return false
}
