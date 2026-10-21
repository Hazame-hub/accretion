package proxmox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

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
	// Found by mark, but carrying another identity than its template's:
	// somebody changed it, and it is not re-stamped.
	if _, err := w.Clone(context.Background(), provider.CloneRequest{
		Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-2", MintUUID: mintA, OwnerMark: "accrete:run-c"}); err != nil {
		t.Fatal(err)
	}
	pve.SetConfig(100, "smbios1", "uuid=99999999-8888-4777-8666-555555555555")
	_, err = w.Clone(context.Background(), provider.CloneRequest{
		Template: provider.Ref{ID: "pve1/qemu/9000"}, Name: "web-2", MintUUID: mintA, OwnerMark: "accrete:run-c"})
	if !errors.As(err, &refused) {
		t.Fatalf("Clone over a changed identity: want a RefusedError, got %v", err)
	}
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
