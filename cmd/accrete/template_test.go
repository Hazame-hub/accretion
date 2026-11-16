package main

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

// build-proxmox-template, Sybil's blueprint for the template a provisioning
// run clones, built by accrete with the same inputs: the image fetched and
// checked, a VM whose disk is a copy of it, never started, given its
// cloud-init drive and converted.

const templateManifest = `apiVersion: accretion/v1alpha1
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
    - name: debian
      count: 1
      blueprint: build-proxmox-template
      inputs:
        pve_node: pve1
        template_name: debian-12-cloud
        image_url: IMAGEURL
        image_filename: debian-12-genericcloud-amd64.qcow2
        download_storage: local
        disk_storage: local-lvm
        disk_size: 20G
        on_enrolment_failure: destroy
EXTRA`

const debianImage = "local:import/debian-12-genericcloud-amd64.qcow2"

func newTemplateLab(t *testing.T, imageURL, extra string) *applyLab {
	t.Helper()
	pve := pvetest.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "env.yaml")
	src := strings.NewReplacer("ENDPOINT", pve.URL, "IMAGEURL", imageURL, "EXTRA", extra).Replace(templateManifest)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	shorten(t)
	return &applyLab{t: t, pve: pve, manifest: path, state: filepath.Join(dir, "accrete.state.json")}
}

func debianServer(t *testing.T, body []byte) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return srv.URL + "/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2"
}

func TestApplyBuildsATemplateFromACloudImage(t *testing.T) {
	qcow := []byte("QFI\xfb debian genericcloud")
	url := debianServer(t, qcow)
	sum := fmt.Sprintf("%x", sha256.Sum256(qcow))
	l := newTemplateLab(t, url, "        image_sha256: "+sum+"\n")

	if p := l.plan(); !strings.Contains(p.Summary, "1 to create") {
		t.Fatalf("plan before: %s", p.Summary)
	}
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "create debian/0", debianImage+" is not there yet; fetching "+url,
		"created as pve1/qemu/100", "its disk a copy of "+debianImage,
		"given a cloud-init drive on local-lvm and converted into a template; it never ran",
		"fetched from "+url+" with SHA-256 "+sum+" (checked on download)",
		"confirmed and recorded: a template, as declared, that clones can start from", "1 done, 0 failed")

	g := l.pve.Guest(100)
	if !g.Template || g.Status != "stopped" {
		t.Fatalf("guest: template %v, %s", g.Template, g.Status)
	}
	for k, want := range map[string]string{
		"name": "debian-12-cloud", "ide2": "local-lvm:vm-100-cloudinit,media=cdrom", "boot": "order=scsi0",
		"serial0": "socket", "vga": "serial0", "scsi0": "local-lvm:base-100-disk-0,size=20G", "agent": "1", "cpu": "x86-64-v2-AES",
	} {
		if got := fmt.Sprint(g.Config[k]); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	for _, w := range l.pve.Writes() {
		if strings.Contains(w, "/status/start") {
			t.Errorf("a template is never started: %s", w)
		}
	}

	s := l.loadState()
	if len(s.Applying) != 0 || len(current(s)) != 1 {
		t.Fatalf("state: %d applying, %d realizations", len(s.Applying), len(current(s)))
	}
	r := current(s)[0]
	if r.ResourceKind != "template" || s.Verdicts[*r.AssetID] != plan.AcceptanceAccepted {
		t.Errorf("realization kind %q, verdict %q", r.ResourceKind, s.Verdicts[*r.AssetID])
	}
	if rec := s.Images[r.ID]; !rec.Template || rec.ImageSHA256Fetched != sum || rec.ImageURL != url || rec.SeedSHA256 != "" {
		t.Errorf("image record %+v", rec)
	}

	// Applied again: nothing to do, and nothing sent.
	before := len(l.pve.Writes())
	if p := l.plan(); !strings.Contains(p.Summary, "1 unchanged") {
		t.Errorf("plan after: %s", p.Summary)
	}
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("second apply: exit %d\n%s", code, out)
	}
	if after := len(l.pve.Writes()); after != before {
		t.Errorf("a second apply wrote %d times", after-before)
	}
}

func TestApplyRefusesToFetchATemplateImageWithoutItsHash(t *testing.T) {
	url := debianServer(t, []byte("anything"))
	l := newTemplateLab(t, url, "")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code == 0 {
		t.Fatalf("apply passed without image_sha256:\n%s", out)
	}
	mustContain(t, out, "accrete fetches "+url+" itself only with image_sha256")
	if len(l.pve.Writes()) != 0 {
		t.Errorf("writes: %v", l.pve.Writes())
	}
}

func TestApplyBuildsATemplateFromAnImageAlreadyOnStorage(t *testing.T) {
	l := newTemplateLab(t, "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2", "")
	l.pve.Files[debianImage] = []byte("QFI\xfb already here")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "converted into a template", "confirmed and recorded")
	if strings.Contains(out, "fetching") {
		t.Errorf("fetched an image the storage has:\n%s", out)
	}
}

func TestDestroyRemovesATemplateItBuilt(t *testing.T) {
	l := newTemplateLab(t, "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2", "")
	l.pve.Files[debianImage] = []byte("QFI\xfb already here")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	out, code := l.accrete("", "destroy", "--auto-approve")
	if code != 0 {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	if g := l.pve.Guest(100); g != nil {
		t.Errorf("template still there: %+v", g.Config)
	}
	if _, kept := l.pve.Files[debianImage]; !kept {
		t.Error("the image was removed with the template")
	}
}

func TestApplyWritesTheTemplatesHardwareAndCloudInitDefaults(t *testing.T) {
	l := newTemplateLab(t, "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2",
		"        bios: ovmf\n        machine: q35\n        tags: sybil-template;debian\n        ciuser: ops\n"+
			"        sshkeys: ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGbm9w ops@laptop\n        ipconfig0: ip=dhcp\n        searchdomain: lab.example\n")
	l.pve.Files[debianImage] = []byte("QFI\xfb already here")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	g := l.pve.Guest(100)
	for k, want := range map[string]string{
		"bios": "ovmf", "machine": "q35", "tags": "sybil-template;debian", "ciuser": "ops", "ipconfig0": "ip=dhcp",
		"searchdomain": "lab.example", "efidisk0": "local-lvm:1,efitype=4m,pre-enrolled-keys=0",
		"sshkeys": "ssh-ed25519%20AAAAC3NzaC1lZDI1NTE5AAAAIGbm9w%20ops%40laptop",
	} {
		if got := fmt.Sprint(g.Config[k]); got != want {
			t.Errorf("%s = %q, want %q", k, got, want)
		}
	}
	if !g.Template {
		t.Error("not a template")
	}
}

func TestApplyBakesATemplateOnceThenSealsAndConvertsIt(t *testing.T) {
	l := newTemplateLab(t, "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2",
		"        bake_guest_agent: true\n        bake_upgrade: true\n        bake:\n          packages: [htop]\n"+
			"          users:\n            - name: ops\n              ssh_keys: [\"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGbm9wZW5zc2gta2V5LXYxAAAAAA ops@laptop\"]\n              sudo: true\n")
	l.pve.Files[debianImage] = []byte("QFI\xfb already here")
	poll := firstBootPoll
	firstBootPoll = time.Millisecond
	t.Cleanup(func() { firstBootPoll = poll })
	var seed string
	l.pve.OnUpload = func(vol string, body []byte) {
		if strings.Contains(vol, "accrete-") {
			seed = string(body)
		}
	}

	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "seeded with local:iso/accrete-", "started for its first boot",
		"its first boot applied its cloud-init config and powered it off", "baked and sealed; its seed local:iso/accrete-",
		"converted into a template", "confirmed and recorded: a template")
	for _, want := range []string{"qemu-guest-agent", "htop", "package_upgrade: true", "name: ops",
		"cloud-init clean --logs --seed", "/etc/ssh/ssh_host_*", "power_state:"} {
		if !strings.Contains(seed, want) {
			t.Errorf("bake seed lacks %q", want)
		}
	}
	if strings.Index(seed, "systemctl") > strings.Index(seed, "cloud-init clean") {
		t.Error("the seal must run after everything else")
	}
	g := l.pve.Guest(100)
	if !g.Template || g.Config["ide2"] != "local-lvm:vm-100-cloudinit,media=cdrom" {
		t.Errorf("template %v, ide2 %v", g.Template, g.Config["ide2"])
	}
	for vol := range l.pve.Files {
		if strings.Contains(vol, "accrete-") {
			t.Errorf("bake seed %s left on storage", vol)
		}
	}
	s := l.loadState()
	if rec := s.Images[current(s)[0].ID]; !rec.Template || len(rec.SeedSHA256) != 64 {
		t.Errorf("image record %+v", rec)
	}
}

func TestApplyRefusesToBakeASecretIntoATemplate(t *testing.T) {
	l := newTemplateLab(t, "https://cloud.debian.org/images/cloud/bookworm/latest/debian-12-genericcloud-amd64.qcow2",
		"        bake:\n          run: [\"echo {{secret.token}} > /etc/token\"]\n")
	l.pve.Files[debianImage] = []byte("QFI\xfb already here")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code == 0 || !strings.Contains(out, "is in every clone") {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	if len(l.pve.Writes()) != 0 {
		t.Errorf("writes: %v", l.pve.Writes())
	}
}
