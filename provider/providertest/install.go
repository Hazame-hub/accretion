package providertest

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hazame-hub/accretion/provider"
)

// InstallFixture is what InstallContract needs from the far side: an
// installer image the provider keeps, where machines and their disks go, a
// storage for answer discs, a guest that is not the suite's, and a way to
// lose the next create's answer.
type InstallFixture struct {
	// Placement is where machines go (a node).
	Placement string
	// Installer is an installer image the provider lists on Placement, and
	// MissingInstaller one it does not.
	Installer, MissingInstaller string
	// DiskStorage takes the empty boot disk; SeedStorage takes answer discs.
	DiskStorage, SeedStorage string
	// Foreign is a running guest the suite did not make, carrying
	// ForeignUUID. Every verb aimed at it under another identity must be
	// refused and leave it running with its identity.
	Foreign     provider.Ref
	ForeignUUID string
	// LoseNextCreateAnswer makes the next Create request be carried out and
	// its answer lost on the way back. The suite expects the call, or its
	// retry, to finish that one machine and make no other.
	LoseNextCreateAnswer func()
	// SetStopped stops a machine the way the end of an install does (from
	// inside the guest, not through the Writer), so RemoveMedia can be
	// asked of a stopped machine. nil uses Writer.Power.
	SetStopped func(provider.Ref)
}

// InstallContract is what every provider.Installer must satisfy, on top of
// WriteContract: an installer image read and a missing one refused; an empty
// machine created with its identity, mark and shape, found again by a repeat
// and by the retry of a lost answer, never duplicated; its own answer disc
// placed and placed again as a no-op; another machine's disc, a running
// machine and another guest refused; the discs removed once stopped, again a
// no-op, with the installer image kept; and the credential never in an answer.
// See openspec accrete-installer-iso, task 4.1.
func InstallContract(t *testing.T, p provider.Provider, cfg provider.Config, secret string, fx InstallFixture) {
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
	in, ok := s.(provider.Installer)
	if !ok {
		t.Fatal("the session does not implement provider.Installer")
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
	foreignUntouched := func(after string) {
		t.Helper()
		if r := find(t, s, fx.Foreign); r == nil || r.State != "running" || !sameUUID(r.Identity.SMBIOSUUID, fx.ForeignUUID) {
			t.Errorf("after %s, the guest that is not the suite's is %+v, want it running carrying %s", after, r, fx.ForeignUUID)
		}
	}

	// The installer image.
	media, err := in.Media(ctx, fx.Placement, fx.Installer)
	check("Media", err)
	if err != nil || media.Volume != fx.Installer || media.Size <= 0 {
		t.Fatalf("Media(%s) = %+v, %v; want it with its size", fx.Installer, media, err)
	}
	_, err = in.Media(ctx, fx.Placement, fx.MissingInstaller)
	refused("Media of an image the provider does not list", err)

	req := provider.CreateRequest{
		Name: "ic-one", Placement: fx.Placement, MintUUID: writeUUID1, OwnerMark: "accrete:install-contract-1",
		Shape:       provider.Shape{Cores: 2, MemoryMB: 2048, DiskGB: 16},
		DiskStorage: fx.DiskStorage, Installer: fx.Installer,
	}

	// Refused before anything is made.
	bad := req
	bad.MintUUID = "not-a-uuid"
	_, err = in.Create(ctx, bad)
	refused("Create with an unusable MintUUID", err)
	bad = req
	bad.OwnerMark = ""
	_, err = in.Create(ctx, bad)
	refused("Create with no owner mark", err)
	if got := count(t, s, "ic-one"); got != 0 {
		t.Fatalf("a refused Create made %d machines", got)
	}

	// Create, then repeat.
	m, err := in.Create(ctx, req)
	check("Create", err)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !m.Applied || m.Found {
		t.Errorf("first Create = %+v, want Applied and not Found", m)
	}
	if r := find(t, s, m.Ref); r == nil {
		t.Fatalf("Discover does not list the machine at %v", m.Ref)
	} else {
		if !sameUUID(r.Identity.SMBIOSUUID, writeUUID1) {
			t.Errorf("the machine carries %q, want the minted %s", r.Identity.SMBIOSUUID, writeUUID1)
		}
		if r.State != "stopped" {
			t.Errorf("a created machine is %s, want stopped until it is started", r.State)
		}
	}
	shaped(t, s, m.Ref, req.Shape)
	again, err := in.Create(ctx, req)
	check("second Create", err)
	if err != nil || again.Ref != m.Ref || again.Applied || !again.Found {
		t.Errorf("second Create = %+v, %v; want %v found, Applied false", again, err, m.Ref)
	}
	if got := count(t, s, "ic-one"); got != 1 {
		t.Errorf("after a repeated Create, %d machines are named ic-one, want 1", got)
	}

	// The answer disc.
	target := provider.Target{Ref: m.Ref, ExpectUUID: writeUUID1, Name: "ic-one"}
	answer := provider.Answer{Label: "OEMDRV", Files: map[string][]byte{"ks.cfg": []byte("text\npoweroff\n")},
		Storage: fx.SeedStorage, InstanceID: writeUUID1}
	pl, err := in.PlaceAnswer(ctx, target, answer)
	check("PlaceAnswer", err)
	if err != nil {
		t.Fatalf("PlaceAnswer: %v", err)
	}
	if !pl.Applied || pl.Volume == "" || pl.Drive == "" {
		t.Errorf("first PlaceAnswer = %+v, want a drive and a volume, Applied", pl)
	}
	if pl.Volume == fx.Installer {
		t.Errorf("the answer disc is the installer image %s", pl.Volume)
	}
	pl2, err := in.PlaceAnswer(ctx, target, answer)
	check("second PlaceAnswer", err)
	if err != nil || pl2.Applied || pl2.Volume != pl.Volume {
		t.Errorf("second PlaceAnswer = %+v, %v; want the same volume, Applied false", pl2, err)
	}

	// Identity is the guard.
	_, err = in.PlaceAnswer(ctx, provider.Target{Ref: fx.Foreign, ExpectUUID: writeUUID1}, answer)
	refused("PlaceAnswer on a guest that does not carry the identity", err)
	_, err = in.RemoveMedia(ctx, provider.Target{Ref: fx.Foreign, ExpectUUID: writeUUID1}, pl)
	refused("RemoveMedia on a guest that does not carry the identity", err)
	_, err = in.PlaceAnswer(ctx, provider.Target{Ref: m.Ref}, answer)
	refused("PlaceAnswer with no expected identity", err)
	foreignUntouched("refused installer verbs")

	// Another machine's disc is never removed.
	other := pl
	other.Volume = strings.Replace(pl.Volume, strings.ToLower(writeUUID1), strings.ToLower(writeUUID2), 1)
	other.Volume = strings.Replace(other.Volume, writeUUID1, writeUUID2, 1)
	_, err = in.RemoveMedia(ctx, target, other)
	refused("RemoveMedia of a disc that is not the machine's", err)

	// The discs come out only once the install has powered it off.
	if _, err := w.Power(ctx, target, provider.PowerRunning); err != nil {
		t.Fatalf("Power(running): %v", err)
	}
	_, err = in.RemoveMedia(ctx, target, pl)
	refused("RemoveMedia of a running machine", err)
	if fx.SetStopped != nil {
		fx.SetStopped(m.Ref)
	} else if _, err := w.Power(ctx, target, provider.PowerStopped); err != nil {
		t.Fatalf("Power(stopped): %v", err)
	}
	done, err := in.RemoveMedia(ctx, target, pl)
	check("RemoveMedia", err)
	if err != nil || !done {
		t.Fatalf("RemoveMedia = %v, %v; want Applied", done, err)
	}
	done2, err := in.RemoveMedia(ctx, target, pl)
	check("second RemoveMedia", err)
	if err != nil || done2 {
		t.Errorf("second RemoveMedia = %v, %v; want Applied false", done2, err)
	}
	if kept, err := in.Media(ctx, fx.Placement, fx.Installer); err != nil || kept.Volume != fx.Installer {
		t.Errorf("after RemoveMedia the installer image is gone: %+v, %v", kept, err)
	}
	if _, err := w.Delete(ctx, target, provider.DeleteOptions{StopFirst: true}); err != nil {
		t.Errorf("Delete of the installed machine: %v", err)
	}

	// A create whose answer was lost: one machine, whether the call itself
	// or its retry finishes it.
	if fx.LoseNextCreateAnswer == nil {
		t.Fatal("InstallFixture.LoseNextCreateAnswer is required")
	}
	req2 := req
	req2.Name, req2.MintUUID, req2.OwnerMark = "ic-two", writeUUID2, "accrete:install-contract-2"
	fx.LoseNextCreateAnswer()
	m2, err := in.Create(ctx, req2)
	check("Create with a lost answer", err)
	if err != nil {
		var unknown *provider.OutcomeUnknownError
		if !errors.As(err, &unknown) {
			t.Fatalf("Create with a lost answer: want success or a *provider.OutcomeUnknownError, got %v", err)
		}
		if m2, err = in.Create(ctx, req2); err != nil {
			t.Fatalf("Create retried after a lost answer: %v", err)
		}
	}
	if got := count(t, s, "ic-two"); got != 1 {
		t.Errorf("after a lost create answer, %d machines are named ic-two, want 1", got)
	}
	if r := find(t, s, m2.Ref); r == nil || !sameUUID(r.Identity.SMBIOSUUID, writeUUID2) {
		t.Errorf("the machine whose create answer was lost is %+v, want it carrying %s", r, writeUUID2)
	}
	if _, err := w.Delete(ctx, provider.Target{Ref: m2.Ref, ExpectUUID: writeUUID2}, provider.DeleteOptions{StopFirst: true}); err != nil {
		t.Errorf("Delete of the machine whose create answer was lost: %v", err)
	}
	foreignUntouched("the suite")

	leak(t, secret, "results", fmt.Sprintf("%+v %+v %+v %+v", media, m, pl, m2))
}
