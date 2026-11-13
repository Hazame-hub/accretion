package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

// A build from a disk image against the in-memory PVE: Fedora CoreOS's
// proxmoxve image with an Ignition config, and a cloud image with
// cloud-config. The PVE copies the disk from the image, and a guest started
// with a seed whose config ends the first boot with a power-off powers
// itself off after a few reads, as the real first boot does.

const imageManifest = `apiVersion: accretion/v1alpha1
kind: Environment
metadata:
  name: lab
spec:
  provider:
    type: proxmox
    endpoint: ENDPOINT
    verify_tls: false
    token_env: ACCRETE_PVETEST_TOKEN
  resources:
    - name: fcos
      count: 1
      blueprint: image-proxmox-vm
      inputs:
        pve_node: pve1
        hostname: accrete-fcos-{{instance.number}}
        image: local:import/fedora-coreos-proxmoxve.x86_64.qcow2
        seed_format: ignition
        disk_gb: 20
        memory_mb: 4096
        bootstrap:
          users:
            - name: core
              ssh_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGbm9wZW5zc2gta2V5LXYxAAAAAA ops@laptop"]
              sudo: true
          timezone: Europe/Rome
EXTRA`

const fcosImage = "local:import/fedora-coreos-proxmoxve.x86_64.qcow2"

func newImageLab(t *testing.T, manifest, extra string) *applyLab {
	t.Helper()
	pve := pvetest.New(t)
	pve.Files[fcosImage] = []byte("FCOS qcow2")
	dir := t.TempDir()
	path := filepath.Join(dir, "env.yaml")
	src := strings.Replace(strings.Replace(manifest, "ENDPOINT", pve.URL, 1), "EXTRA", extra, 1)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	shorten(t)
	poll := firstBootPoll
	firstBootPoll = time.Millisecond
	t.Cleanup(func() { firstBootPoll = poll })
	return &applyLab{t: t, pve: pve, manifest: path, state: filepath.Join(dir, "accrete.state.json")}
}

func TestApplyBuildsFedoraCoreOSFromItsImageWithIgnition(t *testing.T) {
	l := newImageLab(t, imageManifest, "")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "create fcos/0", "created as pve1/qemu/100", "its disk a copy of "+fcosImage,
		"seeded with local:iso/accrete-", "(ignition, SHA-256 ", "started for its first boot; no console input is sent",
		"its first boot applied its ignition config and powered it off", "started again",
		"built from "+fcosImage+", 10 bytes, dated ",
		"confirmed and recorded: its first boot powered it off, and it runs again as declared", "1 done, 0 failed")

	g := l.pve.Guest(100)
	if g.Status != "running" || g.Config["scsi0"] != "local-lvm:vm-100-disk-0,size=20G" || g.Config["boot"] != "order=scsi0" ||
		g.Config["serial0"] != "socket" || g.Config["ide3"] != nil {
		t.Errorf("guest after: %s %v", g.Status, g.Config)
	}
	if !strings.HasPrefix(g.Config["ide2"].(string), "local:iso/accrete-") {
		t.Errorf("seed: %v", g.Config["ide2"])
	}
	// The seed's user-data is the Ignition config Fedora CoreOS reads from
	// the cidata disc.
	seed := string(l.pve.Files[strings.TrimSuffix(g.Config["ide2"].(string), ",media=cdrom")])
	for _, want := range []string{`"version": "3.4.0"`, `"name": "core"`, "accrete-first-boot.service", "cidata"} {
		if !strings.Contains(seed, want) {
			t.Errorf("seed lacks %q", want)
		}
	}
	if strings.Contains(seed, "#cloud-config") {
		t.Error("an Ignition seed must not start as cloud-config: Ignition skips it")
	}
	if _, kept := l.pve.Files[fcosImage]; !kept {
		t.Error("the image was removed")
	}

	s := l.loadState()
	if len(s.Applying) != 0 || len(current(s)) != 1 {
		t.Fatalf("state: %d applying, %d realizations", len(s.Applying), len(current(s)))
	}
	r := current(s)[0]
	if s.Verdicts[*r.AssetID] != plan.AcceptanceAccepted {
		t.Errorf("verdict %q", s.Verdicts[*r.AssetID])
	}
	rec, ok := s.Images[r.ID]
	if !ok || rec.ImageVolume != fcosImage || rec.ImageSize != 10 || rec.ImageCreated == nil || rec.SeedFormat != "ignition" ||
		len(rec.SeedSHA256) != 64 || rec.ImageSHA256Declared != "" {
		t.Errorf("image record %+v", rec)
	}
	if p := l.plan(); !strings.Contains(p.Summary, "1 unchanged") {
		t.Errorf("after: %s", p.Summary)
	}
	for _, w := range l.pve.Writes() {
		if strings.Contains(w, "sendkey") || strings.Contains(w, "monitor") {
			t.Errorf("console input: %s", w)
		}
	}
}

// Ignition failed: Fedora CoreOS stops in its emergency shell and never
// powers off. The machine is kept, its record is not accepted, and a rerun
// waits again on the same VM.
func TestAFirstBootThatNeverPowersOffIsNotAccepted(t *testing.T) {
	l := newImageLab(t, imageManifest, "")
	l.pve.BrokenFirstBoot = true
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "pve1/qemu/100 has not powered itself off after the 200ms this apply waits",
		"its ignition config is not known to have been applied; it is kept as it is")
	if s := l.loadState(); len(s.Applying) != 1 || s.Applying[0].Step != stepFirstBoot || len(current(s)) != 0 {
		t.Fatalf("state %+v", s.Applying)
	}
	if g := l.pve.Guest(100); g == nil || g.Status != "running" {
		t.Fatalf("guest %+v", g)
	}
	if e := entryAt(t, l.plan(), "fcos", 0); e.Action != plan.ActionUnknown {
		t.Errorf("plan during the first boot: %s", e.Action)
	}

	// It powers off at last (an operator fixed it from the console): the
	// rerun carries on with the same VM.
	l.pve.FinishInstall(100)
	out, code = l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("rerun: exit %d\n%s", code, out)
	}
	mustContain(t, out, "finish fcos/0 (half-built by an earlier apply)", "confirmed and recorded")
	if ids := l.pve.Named("accrete-fcos-1"); len(ids) != 1 {
		t.Errorf("machines: %v", ids)
	}
}

// The create's answer is lost: the provider tells its own VM by its mark,
// so the build carries on with it and the image is not copied twice.
func TestABuildFromAnImageWhoseCreateAnswerWasLostCarriesOn(t *testing.T) {
	l := newImageLab(t, imageManifest, "")
	l.pve.DropNext(`^POST /nodes/pve1/qemu$`)
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "confirmed and recorded")
	if ids := l.pve.Named("accrete-fcos-1"); len(ids) != 1 {
		t.Errorf("machines: %v", ids)
	}
}

func TestAnImageThatIsNotThereOrABadSeedIsRefusedBeforeAnythingIsMade(t *testing.T) {
	for name, c := range map[string]struct{ extra, want string }{
		"no image":  {"", "is not on pve1's storage local; upload it first (Datacenter → pve1 → local → Import)"},
		"packages":  {"          packages: [vim]\n", "bootstrap packages are not supported"},
		"run":       {"          run: [\"echo hi\"]\n", "bootstrap run is not supported by an Ignition config"},
		"bad fmt":   {"        seed_format: kickstart\n", "duplicate"},
		"usr file":  {"          files: [{path: /usr/local/bin/x, content: x}]\n", "under /usr"},
		"iso image": {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			manifest := imageManifest
			switch name {
			case "bad fmt":
				manifest = strings.Replace(manifest, "seed_format: ignition", "seed_format: kickstart", 1)
				c.extra, c.want = "", `"kickstart" is not a format accrete writes`
			case "iso image":
				manifest = strings.Replace(manifest, fcosImage, "local:iso/fcos-live.iso", 1)
				c.want = "is not a disk image in a storage's import content"
			}
			l := newImageLab(t, manifest, c.extra)
			if name == "no image" {
				delete(l.pve.Files, fcosImage)
			}
			out, code := l.accrete("", "apply", "--auto-approve")
			if code == 0 || !strings.Contains(out, c.want) || len(l.pve.Writes()) != 0 {
				t.Fatalf("exit %d, writes %v\n%s", code, l.pve.Writes(), out)
			}
		})
	}
}

const cloudImageManifest = `apiVersion: accretion/v1alpha1
kind: Environment
metadata:
  name: lab
spec:
  provider:
    type: proxmox
    endpoint: ENDPOINT
    verify_tls: false
    token_env: ACCRETE_PVETEST_TOKEN
  resources:
    - name: deb
      count: 1
      blueprint: image-proxmox-vm
      inputs:
        pve_node: pve1
        hostname: accrete-deb-{{instance.number}}
        image: local:import/debian-13-genericcloud-amd64.qcow2
        image_sha256: ` + "ABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABABAB" + `
EXTRA`

// The same blueprint builds any cloud-init image: the seed is cloud-config
// that powers the machine off when cloud-init is done, and the disk keeps
// the image's size when no disk_gb is declared.
func TestApplyBuildsACloudInitImage(t *testing.T) {
	l := newImageLab(t, cloudImageManifest, "")
	l.pve.Files["local:import/debian-13-genericcloud-amd64.qcow2"] = []byte("Debian qcow2")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "(cloud-init, SHA-256 ", "its first boot applied its cloud-init config",
		"SHA-256 "+strings.Repeat("ab", 32)+" as declared (not checked)")
	g := l.pve.Guest(100)
	if g.Config["scsi0"] != "local-lvm:vm-100-disk-0,size=10G" {
		t.Errorf("disk %v", g.Config["scsi0"])
	}
	seed := string(l.pve.Files[strings.TrimSuffix(g.Config["ide2"].(string), ",media=cdrom")])
	mustContain(t, seed, "#cloud-config", "power_state:", "mode: poweroff", "hostname: accrete-deb-1")
	for _, w := range l.pve.Writes() {
		if strings.Contains(w, "/resize") {
			t.Errorf("a disk with no declared size was resized: %s", w)
		}
	}
}

// Destroy removes the VM and its seed, and keeps the image.
func TestDestroyAfterABuildFromAnImageKeepsTheImage(t *testing.T) {
	l := newImageLab(t, imageManifest, "")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	out, code := l.accrete("", "destroy", "--auto-approve")
	if code != 0 {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	mustContain(t, out, "deleted; record retired")
	if _, kept := l.pve.Files[fcosImage]; !kept || l.pve.Guest(100) != nil || len(l.pve.Files) != 1 {
		t.Errorf("after destroy: guest %v, files %v", l.pve.Guest(100), keys(l.pve.Files))
	}
}

// No template and no guest agent: the image's storage is read, the disk
// storage gets the copy.
func TestPermissionsForABuildFromAnImage(t *testing.T) {
	l := newImageLab(t, imageManifest, "")
	g := fullGrants()
	delete(g, "/vms/9000")
	l.pve.Privileges = g
	out, code := l.accrete("", "permissions")
	if code != 0 {
		t.Fatalf("exit %d with every grant:\n%s", code, out)
	}
	if strings.Contains(out, "/vms/9000") || !strings.Contains(out, "find the disk image and copy it") ||
		!strings.Contains(out, "copy each machine's disk from the image") {
		t.Errorf("grants:\n%s", out)
	}
	l.onlyGETs()
}

// The state's image record survives a round trip under its JSON names.
func TestTheImageRecordIsWrittenUnderItsNames(t *testing.T) {
	raw, _ := json.Marshal(ImageRecord{ImageVolume: "v", SeedFormat: "ignition"})
	if string(raw) != `{"image_volume":"v","image_size":0,"seed_format":"ignition"}` {
		t.Errorf("%s", raw)
	}
}

// The shipped Fedora CoreOS example decodes and plans one create.
func TestTheFedoraCoreOSExamplePlansOffline(t *testing.T) {
	p, out, code := accrete(t, "", "-f", "../../examples/fedora-coreos.yaml", "--offline")
	if code != 0 || p.Summary != "1 to create, 0 unchanged, 0 to change, 0 to delete, 0 unknown" {
		t.Fatalf("exit %d, summary %q\n%s", code, p.Summary, out)
	}
}
