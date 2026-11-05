package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

// An install from the RHEL DVD against the in-memory PVE, which installs a
// guest started with an empty disk, the DVD and an OEMDRV disc, using the
// hostname its kickstart names. openspec accrete-installer-iso, tasks 5.x.

const installManifest = `apiVersion: accretion/v1alpha1
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
    - name: ks
      count: 1
      blueprint: install-proxmox-vm
      inputs:
        pve_node: pve1
        hostname: accrete-ks-{{instance.number}}
        iso: local:iso/rhel-9.6-x86_64-dvd.iso
        memory_mb: 3072
        bootstrap:
          users:
            - name: ops
              ssh_keys: ["ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGbm9wZW5zc2gta2V5LXYxAAAAAA ops@laptop"]
              sudo: true
EXTRA`

const dvd = "local:iso/rhel-9.6-x86_64-dvd.iso"

func newInstallLab(t *testing.T, extra string) *applyLab {
	t.Helper()
	pve := pvetest.New(t)
	pve.Files[dvd] = []byte("RHEL-9.6 DVD")
	dir := t.TempDir()
	path := filepath.Join(dir, "env.yaml")
	src := strings.Replace(strings.Replace(installManifest, "ENDPOINT", pve.URL, 1), "EXTRA", extra, 1)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	shorten(t)
	return &applyLab{t: t, pve: pve, manifest: path, state: filepath.Join(dir, "accrete.state.json")}
}

// shorten makes the waits of an install test-sized.
func shorten(t *testing.T) {
	t.Helper()
	poll, wait, apoll, deadline := installPoll, agentWait, agentPoll, installDeadline
	installPoll, agentWait, agentPoll = time.Millisecond, 50*time.Millisecond, time.Millisecond
	installDeadline = func(time.Duration) time.Duration { return 200 * time.Millisecond }
	t.Cleanup(func() { installPoll, agentWait, agentPoll, installDeadline = poll, wait, apoll, deadline })
}

func mustContain(t *testing.T, out string, want ...string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(out, w) {
			t.Errorf("output lacks %q:\n%s", w, out)
		}
	}
}

func TestApplyInstallsAMachineAndAcceptsItOnItsAgentsHostname(t *testing.T) {
	l := newInstallLab(t, "")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "create ks/0", "created as pve1/qemu/100", "empty 32 GiB disk, booting "+dvd,
		"answer disc local:iso/accrete-", "on ide3 (label OEMDRV, ks.cfg SHA-256 ",
		"no console input is sent", "the installer powered it off", "discs ejected and its answer disc deleted; the installer ISO stays",
		"started from its own disk", "installed from "+dvd+", 12 bytes, dated ",
		"accepted and recorded: its guest agent reports accrete-ks-1 after a start from its own disk", "1 done, 0 failed")
	if strings.Contains(out, "as declared (not checked)") {
		t.Error("an undeclared hash was printed")
	}
	g := l.pve.Guest(100)
	if g.Config["ide2"] != "none,media=cdrom" || g.Config["ide3"] != nil || g.Status != "running" {
		t.Errorf("guest after: ide2 %v ide3 %v %s", g.Config["ide2"], g.Config["ide3"], g.Status)
	}
	if _, kept := l.pve.Files[dvd]; !kept || len(l.pve.Files) != 1 {
		t.Errorf("storage after: %v", keys(l.pve.Files))
	}
	s := l.loadState()
	if len(s.Applying) != 0 || len(current(s)) != 1 {
		t.Fatalf("state: %d applying, %d realizations", len(s.Applying), len(current(s)))
	}
	r := current(s)[0]
	if s.Verdicts[*r.AssetID] != plan.AcceptanceAccepted {
		t.Errorf("verdict %q", s.Verdicts[*r.AssetID])
	}
	rec, ok := s.Installs[r.ID]
	if !ok || rec.ISOVolume != dvd || rec.ISOSize != 12 || rec.ISOCreated == nil || len(rec.AnswerSHA256) != 64 ||
		rec.ISOSHA256Declared != "" || rec.Hostname != "accrete-ks-1" || rec.AnswerFormat != "kickstart" {
		t.Errorf("install record %+v", rec)
	}
	if p := l.plan(); !strings.Contains(p.Summary, "1 unchanged") {
		t.Errorf("after: %s", p.Summary)
	}
	// No keystroke, ever.
	for _, w := range l.pve.Writes() {
		if strings.Contains(w, "sendkey") || strings.Contains(w, "monitor") {
			t.Errorf("console input: %s", w)
		}
	}
}

func keys(m map[string][]byte) []string {
	var out []string
	for k := range m {
		out = append(out, k)
	}
	return out
}

func TestADeclaredISOHashIsRecordedAsDeclared(t *testing.T) {
	l := newInstallLab(t, "        iso_sha256: "+strings.Repeat("ab", 32)+"\n")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "SHA-256 "+strings.Repeat("ab", 32)+" as declared (not checked)")
	if strings.Contains(out, "verified") {
		t.Errorf("output calls the hash verified:\n%s", out)
	}
	s := l.loadState()
	if rec := s.Installs[current(s)[0].ID]; rec.ISOSHA256Declared != strings.Repeat("ab", 32) {
		t.Errorf("record %+v", rec)
	}
}

func TestAnInstallerThatIsNotThereIsRefusedBeforeAnythingIsMade(t *testing.T) {
	l := newInstallLab(t, "")
	delete(l.pve.Files, dvd)
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "refused: ks/0: proxmox: "+dvd+" is not on", "Nothing was sent.")
	if len(l.pve.Writes()) != 0 || len(l.loadState().Applying) != 0 {
		t.Errorf("writes %v", l.pve.Writes())
	}
}

func TestAnUnsupportedAnswerFormatOrBootstrapFieldIsRefused(t *testing.T) {
	for name, c := range map[string]struct{ extra, want string }{
		"format": {"        answer_format: autoinstall\n", `"autoinstall" is not a format`},
		"run":    {"          run: [\"echo hi\"]\n", "bootstrap run is not supported"},
		"secret": {"          files: [{path: /etc/x, content: \"{{secret.pw}}\"}]\n", "takes no secret"},
	} {
		t.Run(name, func(t *testing.T) {
			l := newInstallLab(t, c.extra)
			out, code := l.accrete("", "apply", "--auto-approve")
			if code == 0 || !strings.Contains(out, c.want) || len(l.pve.Writes()) != 0 {
				t.Fatalf("exit %d, writes %v\n%s", code, l.pve.Writes(), out)
			}
		})
	}
}

// The deadline passes: the machine is kept as it is, apply exits non-zero,
// plan says unknown; a later apply carries on with the same machine.
func TestAnInstallPastItsDeadlineIsKeptAndARerunFinishesIt(t *testing.T) {
	l := newInstallLab(t, "")
	l.pve.InstallReads = -1
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "pve1/qemu/100 is still running its installer after the 200ms this apply waits", "it is kept as it is, discs attached")
	g := l.pve.Guest(100)
	if g.Status != "running" || g.Config["ide3"] == nil || !strings.HasPrefix(g.Config["ide2"].(string), dvd) {
		t.Errorf("guest touched: %s ide2 %v ide3 %v", g.Status, g.Config["ide2"], g.Config["ide3"])
	}
	if s := l.loadState(); len(s.Applying) != 1 || s.Applying[0].Step != stepInstalling || len(current(s)) != 0 {
		t.Fatalf("state %+v", s.Applying)
	}
	if e := entryAt(t, l.plan(), "ks", 0); e.Action != plan.ActionUnknown {
		t.Errorf("plan during the install: %s", e.Action)
	}

	// Still installing: the rerun waits one more deadline on the same VM.
	out, code = l.accrete("", "apply", "--auto-approve")
	if code != 1 || !strings.Contains(out, "finish ks/0 (half-built by an earlier apply)") || !strings.Contains(out, "still running its installer") {
		t.Fatalf("second apply: exit %d\n%s", code, out)
	}
	l.pve.FinishInstall(100)
	out, code = l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("third apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "accepted and recorded")
	if ids := l.pve.Named("accrete-ks-1"); len(ids) != 1 || ids[0] != 100 {
		t.Errorf("machines: %v", ids)
	}
}

// Killed before the VM was recorded: the create's answer is lost, and the
// rerun finds the VM by its owner mark and identity.
func TestAnInstallWhoseCreateAnswerWasLostCarriesOnWithTheSameMachine(t *testing.T) {
	l := newInstallLab(t, "")
	l.pve.DropNext(`^POST /nodes/pve1/qemu$`)
	l.pve.FailNext(`^GET /nodes/pve1/qemu/100/config$`, 1)
	out, code := l.accrete("", "apply", "--auto-approve")
	if code == 0 {
		t.Fatalf("apply after a lost answer passed:\n%s", out)
	}
	out, code = l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("rerun: exit %d\n%s", code, out)
	}
	mustContain(t, out, "found its earlier machine pve1/qemu/100 by its owner mark")
	if ids := l.pve.Named("accrete-ks-1"); len(ids) != 1 {
		t.Errorf("machines: %v", ids)
	}
}

// The agent reports another hostname: not accepted, both names given, the
// record unknown and the machine kept.
func TestAnInstallWhoseAgentReportsAnotherHostnameIsNotAccepted(t *testing.T) {
	l := newInstallLab(t, "")
	l.pve.InstallReads = -1
	if _, code := l.accrete("", "apply", "--auto-approve"); code != 1 {
		t.Fatal("the first apply should stop at the deadline")
	}
	l.pve.FinishInstall(100)
	l.pve.SetAgentHostname(100, "localhost")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, `reports hostname "localhost", not the declared "accrete-ks-1"`, "its record stays unknown and the machine is kept")
	s := l.loadState()
	if len(current(s)) != 0 || len(s.Applying) != 1 || s.Applying[0].Step != stepAccept || l.pve.Guest(100) == nil {
		t.Errorf("state %+v realizations %d", s.Applying, len(current(s)))
	}
}

// Powered off without a working install (a Stop mid-install): the disk does
// not boot, the agent never answers, and nothing is accepted.
func TestAPowerOffWithoutAWorkingInstallIsNotAccepted(t *testing.T) {
	l := newInstallLab(t, "")
	l.pve.InstallReads = -1
	if _, code := l.accrete("", "apply", "--auto-approve"); code != 1 {
		t.Fatal("the first apply should stop at the deadline")
	}
	l.pve.CutInstall(100)
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "the installer powered it off", "started from its own disk", "its guest agent did not report a hostname", "the machine is kept")
	if s := l.loadState(); len(current(s)) != 0 || len(s.Applying) != 1 {
		t.Errorf("recorded %d, applying %d", len(current(s)), len(s.Applying))
	}
}

// Giving up on a stuck install: destroy removes the VM and its answer disc,
// guarded by the minted identity, and keeps the installer.
func TestDestroyRemovesAStuckInstallAndKeepsTheInstaller(t *testing.T) {
	l := newInstallLab(t, "")
	l.pve.InstallReads = -1
	if _, code := l.accrete("", "apply", "--auto-approve"); code != 1 {
		t.Fatal("the first apply should stop at the deadline")
	}
	out, code := l.accrete("", "destroy", "--auto-approve")
	if code != 0 {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	mustContain(t, out, "remove ks/0 (half-built by an earlier apply)", "deleted pve1/qemu/100")
	if l.pve.Guest(100) != nil {
		t.Error("the VM is still there")
	}
	if _, kept := l.pve.Files[dvd]; !kept || len(l.pve.Files) != 1 {
		t.Errorf("storage after: %v", keys(l.pve.Files))
	}
	if s := l.loadState(); len(s.Applying) != 0 {
		t.Errorf("applying %+v", s.Applying)
	}
}

// Destroy after an accepted install: the VM goes, the ISO stays.
func TestDestroyAfterAnInstallKeepsTheInstaller(t *testing.T) {
	l := newInstallLab(t, "")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	out, code := l.accrete("", "destroy", "--auto-approve")
	if code != 0 {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	mustContain(t, out, "deleted; record retired")
	if _, kept := l.pve.Files[dvd]; !kept || l.pve.Guest(100) != nil {
		t.Errorf("after destroy: guest %v, files %v", l.pve.Guest(100), keys(l.pve.Files))
	}
}

// An install needs no template, and the guest agent grant is required: the
// hostname it reports is what the install is accepted on (task 6.1).
func TestPermissionsForAnInstallRequireTheGuestAgent(t *testing.T) {
	l := newInstallLab(t, "")
	g := fullGrants()
	delete(g, "/vms/9000")
	l.pve.Privileges = g
	out, code := l.accrete("", "permissions")
	if code != 0 {
		t.Fatalf("exit %d with every grant:\n%s", code, out)
	}
	if strings.Contains(out, "/vms/9000") || !strings.Contains(out, "which its install is accepted on") {
		t.Errorf("grants:\n%s", out)
	}
	g["/vms"] = g["/vms"][:len(g["/vms"])-1] // no VM.GuestAgent.Audit
	l.pve.Privileges = g
	out, code = l.accrete("", "permissions")
	if code != 1 || !strings.Contains(out, "the token lacks VM.GuestAgent.Audit") || strings.Contains(out, "VM.GuestAgent.Audit (optional)") {
		t.Fatalf("exit %d, want 1 naming the agent privilege:\n%s", code, out)
	}
	l.onlyGETs()
}
