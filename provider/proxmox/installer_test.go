package proxmox

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

const dvd = "local:iso/rhel-9.6-x86_64-dvd.iso"

func installerOn(t *testing.T, pve *pvetest.Server) provider.Installer {
	t.Helper()
	return writerOn(t, pve).(provider.Installer)
}

func createReq() provider.CreateRequest {
	return provider.CreateRequest{Name: "ks-1", Placement: "pve1", MintUUID: mintA, OwnerMark: "accrete:run-a",
		DiskStorage: "local-lvm", Installer: dvd,
		Shape: provider.Shape{Cores: 2, MemoryMB: 3072, CPU: "x86-64-v2-AES", DiskGB: 32, NICs: []string{"virtio,bridge=vmbr0"}}}
}

func TestMediaListsTheInstallerAndRefusesOneThatIsNotThere(t *testing.T) {
	pve := pvetest.New(t)
	pve.Files[dvd] = make([]byte, 4096)
	in := installerOn(t, pve)
	m, err := in.Media(context.Background(), "pve1", dvd)
	if err != nil || m.Size != 4096 || m.Created.Unix() != pvetest.FileCTime {
		t.Fatalf("media = %+v, %v", m, err)
	}
	_, err = in.Media(context.Background(), "pve1", "local:iso/rhel-10.iso")
	var refused *provider.RefusedError
	if !errors.As(err, &refused) || !strings.Contains(err.Error(), "local:iso/rhel-10.iso is not on") {
		t.Fatalf("missing ISO: %v", err)
	}
}

// Born with its identity, mark, shape, an empty disk and the installer,
// booting the disk first; a repeat finds it and creates nothing.
func TestCreateMakesAnEmptyMachineThatBootsTheInstaller(t *testing.T) {
	pve := pvetest.New(t)
	in := installerOn(t, pve)
	m, err := in.Create(context.Background(), createReq())
	if err != nil || !m.Applied || m.Found || m.Ref.ID != "pve1/qemu/100" {
		t.Fatalf("create = %+v, %v", m, err)
	}
	cfg := pve.Guest(100).Config
	for k, want := range map[string]string{
		"name": "ks-1", "description": "accrete:run-a", "smbios1": "uuid=" + mintA, "cores": "2", "memory": "3072",
		"scsi0": "local-lvm:vm-100-disk-0,size=32G", "ide2": dvd + ",media=cdrom", "boot": "order=scsi0;ide2", "agent": "1",
	} {
		if got := fmt.Sprint(cfg[k]); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	before := len(pve.Writes())
	again, err := in.Create(context.Background(), createReq())
	if err != nil || again.Applied || !again.Found || again.Ref != m.Ref || len(pve.Writes()) != before {
		t.Fatalf("repeat = %+v, %v; writes %v", again, err, pve.Writes()[before:])
	}
}

// The answer of the create was lost: the guest is there, and the retry
// carries on with it.
func TestCreateWhoseAnswerWasLostIsFoundByItsMark(t *testing.T) {
	pve := pvetest.New(t)
	in := installerOn(t, pve)
	pve.DropNext(`^POST /nodes/pve1/qemu$`)
	m, err := in.Create(context.Background(), createReq())
	if err != nil || m.Ref.ID != "pve1/qemu/100" {
		t.Fatalf("create after a lost answer = %+v, %v", m, err)
	}
	if len(pve.Named("ks-1")) != 1 {
		t.Fatalf("machines named ks-1: %v", pve.Named("ks-1"))
	}
}

func TestTheAnswerDiscIsPlacedThenRemovedWithTheInstallerKept(t *testing.T) {
	pve := pvetest.New(t)
	pve.Files[dvd] = []byte("DVD")
	in := installerOn(t, pve)
	m, err := in.Create(context.Background(), createReq())
	if err != nil {
		t.Fatal(err)
	}
	target := provider.Target{Ref: m.Ref, ExpectUUID: mintA, Name: "ks-1", OwnerMark: "accrete:run-a"}
	pl, err := in.PlaceAnswer(context.Background(), target, provider.Answer{Label: OEMDRVLabel, Files: map[string][]byte{"ks.cfg": []byte("text\n")}, Storage: "local", InstanceID: mintA})
	if err != nil || !pl.Applied || pl.Drive != "ide3" {
		t.Fatalf("place = %+v, %v", pl, err)
	}
	if got := pve.Guest(100).Config["ide3"]; got != pl.Volume+",media=cdrom" {
		t.Fatalf("ide3 = %v", got)
	}
	if again, err := in.PlaceAnswer(context.Background(), target, provider.Answer{Label: OEMDRVLabel, Files: map[string][]byte{"ks.cfg": []byte("text\n")}, Storage: "local", InstanceID: mintA}); err != nil || again.Applied {
		t.Fatalf("repeat place = %+v, %v", again, err)
	}

	// Another machine's disc is refused, and so is a running machine.
	other := pl
	other.Volume = "local:iso/accrete-99999999-2222-4333-8444-555555555555.iso"
	var refused *provider.RefusedError
	if _, err := in.RemoveMedia(context.Background(), target, other); !errors.As(err, &refused) {
		t.Fatalf("another machine's disc: %v", err)
	}
	pve.SetStatus(100, "running")
	if _, err := in.RemoveMedia(context.Background(), target, pl); !errors.As(err, &refused) || !strings.Contains(err.Error(), "powered it off") {
		t.Fatalf("running: %v", err)
	}
	pve.SetStatus(100, "stopped")

	done, err := in.RemoveMedia(context.Background(), target, pl)
	if err != nil || !done {
		t.Fatalf("remove = %v, %v", done, err)
	}
	cfg := pve.Guest(100).Config
	if cfg["ide2"] != "none,media=cdrom" || cfg["ide3"] != nil {
		t.Errorf("drives after: ide2 %v, ide3 %v", cfg["ide2"], cfg["ide3"])
	}
	if _, kept := pve.Files[dvd]; !kept {
		t.Error("the installer ISO was removed")
	}
	if _, left := pve.Files[pl.Volume]; left {
		t.Error("the answer disc is still on storage")
	}
	if again, err := in.RemoveMedia(context.Background(), target, pl); err != nil || again {
		t.Fatalf("repeat remove = %v, %v", again, err)
	}
}

// Identity is the guard: a target whose UUID the guest does not carry is
// refused with nothing sent.
func TestInstallerVerbsRefuseAnotherGuest(t *testing.T) {
	pve := pvetest.New(t)
	in := installerOn(t, pve)
	before := len(pve.Writes())
	target := provider.Target{Ref: provider.Ref{ID: "pve1/qemu/101"}, ExpectUUID: mintA}
	var refused *provider.RefusedError
	if _, err := in.PlaceAnswer(context.Background(), target, provider.Answer{Label: OEMDRVLabel, Files: map[string][]byte{"ks.cfg": nil}, Storage: "local", InstanceID: mintA}); !errors.As(err, &refused) {
		t.Fatalf("place on another guest: %v", err)
	}
	if len(pve.Writes()) != before {
		t.Fatalf("writes: %v", pve.Writes()[before:])
	}
}
