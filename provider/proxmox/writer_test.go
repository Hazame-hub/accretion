package proxmox

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/providertest"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

// The Proxmox Writer passes the write contract against the in-memory PVE.
// The interrupted clone is the hard case: the clone task ran and its guest
// carries the owner mark, then the config write that stamps the identity
// never arrived.
func TestProxmoxWriteContract(t *testing.T) {
	pve := pvetest.New(t)
	cfg := provider.Config{Endpoint: pve.URL, Params: map[string]any{"api_token": pvetest.Token}}
	providertest.WriteContract(t, New(), cfg, pvetest.Secret, providertest.WriteFixture{
		Template:    provider.Ref{Kind: provider.KindTemplate, ID: "pve1/qemu/9000"},
		SeedStorage: "local",
		Foreign:     provider.Ref{Kind: provider.KindVM, ID: "pve1/qemu/101"},
		ForeignUUID: "6f2c9a10-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
		InterruptNextClone: func() {
			pve.CutNext(`^POST /nodes/pve1/qemu/\d+/config$`)
		},
		LoseNextCloneAnswer: func() {
			pve.DropNext(`^POST /nodes/pve1/qemu/9000/clone$`)
		},
	})
	for _, w := range pve.Writes() {
		if w == "DELETE /nodes/pve1/qemu/101?purge=1&destroy-unreferenced-disks=1" {
			t.Errorf("the guest that was not the suite's was deleted")
		}
	}
}

func writerOn(t *testing.T, pve *pvetest.Server) provider.Writer {
	t.Helper()
	s, err := New().Connect(context.Background(), provider.Config{Endpoint: pve.URL, Params: map[string]any{"api_token": pvetest.Token}})
	if err != nil {
		t.Fatal(err)
	}
	return s.(provider.Writer)
}

const mintA = "11111111-2222-4333-8444-555555555555"

func TestCloneIsBornWithItsIdentityShapeAndDisk(t *testing.T) {
	pve := pvetest.New(t)
	w := writerOn(t, pve)
	m, err := w.Clone(context.Background(), provider.CloneRequest{
		Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-1", Full: true, Pool: "accrete",
		MintUUID: mintA, OwnerMark: "accrete:run-a",
		Shape: provider.Shape{Cores: 4, MemoryMB: 4096, CPU: "x86-64-v2-AES", DiskGB: 20, NICs: []string{"virtio,bridge=vmbr1"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if m.Ref.ID != "pve1/qemu/100" || !m.Applied {
		t.Fatalf("Clone = %+v", m)
	}
	g := pve.Guest(100)
	for key, want := range map[string]string{
		"smbios1": "uuid=" + mintA, "cores": "4", "memory": "4096", "cpu": "x86-64-v2-AES",
		"net0": "virtio,bridge=vmbr1", "description": "accrete:run-a",
		"scsi0": "local-lvm:vm-100-disk-0,size=20G",
	} {
		if got := fmt.Sprint(g.Config[key]); got != want {
			t.Errorf("config %s = %q, want %q", key, got, want)
		}
	}
	var sawClone bool
	for _, r := range pve.Writes() {
		sawClone = sawClone || strings.HasPrefix(r, "POST /nodes/pve1/qemu/9000/clone")
	}
	if !sawClone {
		t.Error("no clone request was sent")
	}

	// The same request again sends nothing.
	before := len(pve.Writes())
	again, err := w.Clone(context.Background(), provider.CloneRequest{
		Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-1", Full: true, Pool: "accrete",
		MintUUID: mintA, OwnerMark: "accrete:run-a",
		Shape: provider.Shape{Cores: 4, MemoryMB: 4096, CPU: "x86-64-v2-AES", DiskGB: 20, NICs: []string{"virtio,bridge=vmbr1"}},
	})
	if err != nil || again.Applied || again.Ref != m.Ref {
		t.Fatalf("second Clone = %+v, %v", again, err)
	}
	if after := pve.Writes(); len(after) != before {
		t.Errorf("a repeated Clone wrote %v", after[before:])
	}
}

func TestCloneRefusesAGuestItDidNotMake(t *testing.T) {
	pve := pvetest.New(t)
	w := writerOn(t, pve)
	_, err := w.Clone(context.Background(), provider.CloneRequest{
		Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "db-01", ID: "101", MintUUID: mintA, OwnerMark: "accrete:run-b",
	})
	var refused *provider.RefusedError
	if !errors.As(err, &refused) {
		t.Fatalf("Clone onto somebody's vmid: want a RefusedError, got %v", err)
	}
}

// Proxmox gives every clone a random SMBIOS UUID. A clone whose identity
// write was lost is found by its name and mark and stamped, keeping the
// other smbios1 fields.
func TestAnInterruptedCloneIsStampedOverItsOwnUUID(t *testing.T) {
	pve := pvetest.New(t)
	w := writerOn(t, pve)
	req := provider.CloneRequest{Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-2", MintUUID: mintA, OwnerMark: "accrete:run-c"}
	pve.CutNext(`^POST /nodes/pve1/qemu/\d+/config$`)
	var unknown *provider.OutcomeUnknownError
	if _, err := w.Clone(context.Background(), req); !errors.As(err, &unknown) {
		t.Fatalf("interrupted Clone: want an OutcomeUnknownError, got %v", err)
	}
	if got := pve.Guest(100).Config["smbios1"]; got != "uuid="+pvetest.CloneUUID(100) {
		t.Fatalf("after the interruption the clone carries %v, want its own random UUID", got)
	}
	pve.SetConfig(100, "smbios1", "uuid="+pvetest.CloneUUID(100)+",manufacturer=QWNjcmV0ZQ==,base64=1")
	m, err := w.Clone(context.Background(), req)
	if err != nil || !m.Applied || m.Ref.ID != "pve1/qemu/100" {
		t.Fatalf("resumed Clone = %+v, %v", m, err)
	}
	if got := pve.Guest(100).Config["smbios1"]; got != "uuid="+mintA+",manufacturer=QWNjcmV0ZQ==,base64=1" {
		t.Errorf("stamped smbios1 = %v, want the minted UUID and the other fields kept", got)
	}
	if n := pve.Named("web-2"); len(n) != 1 {
		t.Errorf("guests named web-2: %v, want one", n)
	}
}

// A clone whose answer is lost made its guest. The verb carries on with
// that guest, recognised by its name and mark once its clone lock goes,
// rather than trying another id.
func TestALostCloneAnswerNeverMakesASecondGuest(t *testing.T) {
	pve := pvetest.New(t)
	w := writerOn(t, pve)
	req := provider.CloneRequest{Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-4", MintUUID: mintA, OwnerMark: "accrete:run-e",
		Shape: provider.Shape{Cores: 4}}
	pve.DropNext(`^POST /nodes/pve1/qemu/9000/clone$`)
	m, err := w.Clone(context.Background(), req)
	if err != nil || m.Ref.ID != "pve1/qemu/100" || !m.Applied {
		t.Fatalf("Clone with a lost answer = %+v, %v", m, err)
	}
	if n := pve.Named("web-4"); len(n) != 1 {
		t.Fatalf("guests named web-4: %v, want the one clone", n)
	}
	if g := pve.Guest(100); g.Config["smbios1"] != "uuid="+mintA || g.Config["cores"] != "4" {
		t.Errorf("the clone was not finished: %v", g.Config)
	}
}

// A clone task whose status cannot be read is the same: the guest is there,
// and it is this clone's.
func TestAnUnreadableCloneTaskNeverMakesASecondGuest(t *testing.T) {
	pve := pvetest.New(t)
	w := writerOn(t, pve)
	pve.FailNext(`^GET /nodes/pve1/tasks/`, 3)
	m, err := w.Clone(context.Background(), provider.CloneRequest{Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-7",
		MintUUID: mintA, OwnerMark: "accrete:run-h"})
	if err != nil || m.Ref.ID != "pve1/qemu/100" {
		t.Fatalf("Clone with an unreadable task = %+v, %v", m, err)
	}
	if n := pve.Named("web-7"); len(n) != 1 {
		t.Errorf("guests named web-7: %v, want one", n)
	}
	// A resize task whose status cannot be read: the outcome is unknown.
	pve.FailNext(`^GET /nodes/pve1/tasks/`, 3)
	var unknown *provider.OutcomeUnknownError
	if _, err := w.Clone(context.Background(), provider.CloneRequest{Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-7",
		MintUUID: mintA, OwnerMark: "accrete:run-h", Shape: provider.Shape{DiskGB: 40}}); !errors.As(err, &unknown) {
		t.Fatalf("resize with an unreadable task: want an OutcomeUnknownError, got %v", err)
	}
}

// A guest carrying the clone's name and mark that runs under another
// identity is in use: it is not re-stamped.
func TestARunningGuestIsNotReStamped(t *testing.T) {
	pve := pvetest.New(t)
	w := writerOn(t, pve)
	req := provider.CloneRequest{Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-8", MintUUID: mintA, OwnerMark: "accrete:run-i"}
	if _, err := w.Clone(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	pve.SetStatus(100, "running")
	req.MintUUID = "22222222-3333-4444-8555-666666666666"
	if _, err := w.Clone(context.Background(), req); err == nil {
		t.Fatal("Clone re-stamped a running guest")
	}
	if got := pve.Guest(100).Config["smbios1"]; got != "uuid="+mintA {
		t.Errorf("the running guest now carries %v", got)
	}
}

// An answer that comes after the call gave up is a lost answer: the start
// was carried out, and the caller is told the outcome is unknown.
func TestALateAnswerIsAnUnknownOutcome(t *testing.T) {
	pve := pvetest.New(t)
	c, err := NewClient(pve.URL, pvetest.Token, false)
	if err != nil {
		t.Fatal(err)
	}
	c.CallTimeout = 200 * time.Millisecond
	w := NewSession(c).(provider.Writer)
	m, err := w.Clone(context.Background(), provider.CloneRequest{
		Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-5", MintUUID: mintA, OwnerMark: "accrete:run-f"})
	if err != nil {
		t.Fatal(err)
	}
	pve.DelayNext(`^POST /nodes/pve1/qemu/100/status/start$`, time.Second)
	var unknown *provider.OutcomeUnknownError
	if _, err := w.Power(context.Background(), provider.Target{Ref: m.Ref, ExpectUUID: mintA}, provider.PowerRunning); !errors.As(err, &unknown) {
		t.Fatalf("Power with a late answer: want an OutcomeUnknownError, got %v", err)
	}
	if pve.Guest(100).Status != "running" {
		t.Fatal("the start was not carried out; the test proves nothing")
	}
}

// A seed that is not the target's own is never removed with it.
func TestDeleteRemovesOnlyTheTargetsOwnSeed(t *testing.T) {
	pve := pvetest.New(t)
	w := writerOn(t, pve)
	m, err := w.Clone(context.Background(), provider.CloneRequest{
		Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-6", MintUUID: mintA, OwnerMark: "accrete:run-g"})
	if err != nil {
		t.Fatal(err)
	}
	before := len(pve.Writes())
	var refused *provider.RefusedError
	_, err = w.Delete(context.Background(), provider.Target{Ref: m.Ref, ExpectUUID: mintA}, provider.DeleteOptions{
		RemovePlacements: []provider.Placement{{Volume: "local:iso/rocky-9.iso", Storage: "local", Node: "pve1"}}})
	if !errors.As(err, &refused) || len(pve.Writes()) != before {
		t.Errorf("Delete removing another file: %v, sent %v", err, pve.Writes()[before:])
	}
}

func TestMain(m *testing.M) {
	defaultTaskPoll = 5 * time.Millisecond
	os.Exit(m.Run())
}

func TestPowerRefusesASuspendedGuestAndDeleteIsGuardedBeforeStopping(t *testing.T) {
	pve := pvetest.New(t)
	w := writerOn(t, pve)
	m, err := w.Clone(context.Background(), provider.CloneRequest{
		Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-3", MintUUID: mintA, OwnerMark: "accrete:run-d"})
	if err != nil {
		t.Fatal(err)
	}
	pve.SetStatus(100, "paused")
	var refused *provider.RefusedError
	if _, err := w.Power(context.Background(), provider.Target{Ref: m.Ref, ExpectUUID: mintA}, provider.PowerRunning); !errors.As(err, &refused) {
		t.Fatalf("Power on a suspended guest: want a RefusedError, got %v", err)
	}
	pve.SetStatus(100, "running")
	before := len(pve.Writes())
	other := "99999999-8888-4777-8666-555555555555"
	if _, err := w.Delete(context.Background(), provider.Target{Ref: m.Ref, ExpectUUID: other}, provider.DeleteOptions{StopFirst: true}); !errors.As(err, &refused) {
		t.Fatalf("Delete under another identity: want a RefusedError, got %v", err)
	}
	if after := pve.Writes(); len(after) != before || pve.Guest(100).Status != "running" {
		t.Errorf("a refused Delete sent %v and left the guest %s", after[before:], pve.Guest(100).Status)
	}
}
