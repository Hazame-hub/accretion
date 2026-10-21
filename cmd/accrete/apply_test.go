package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider/proxmox"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

func TestMain(m *testing.M) {
	proxmox.DefaultTaskPoll = 5 * time.Millisecond
	os.Exit(m.Run())
}

// These run accrete apply and destroy against the in-memory PVE, which
// clones, seeds, starts and deletes the way the Writer's own tests prove. No
// real Proxmox is called.

const applyTokenVar = "ACCRETE_PVETEST_TOKEN"

// applyManifest declares two web machines; extra is appended to the web
// resource (power:, more inputs).
const applyManifest = `apiVersion: accretion/v1alpha1
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
    - name: web
      count: 2
      blueprint: provision-proxmox-vm
      inputs:
        pve_node: pve1
        template_vmid: "9000"
        hostname: accrete-web-{{instance.number}}
        cores: 2
        memory_mb: 1024
EXTRA`

type applyLab struct {
	t        *testing.T
	pve      *pvetest.Server
	manifest string
	state    string
}

func newApplyLab(t *testing.T, extra string) *applyLab {
	t.Helper()
	pve := pvetest.New(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "env.yaml")
	src := strings.Replace(strings.Replace(applyManifest, "ENDPOINT", pve.URL, 1), "EXTRA", extra, 1)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	return &applyLab{t: t, pve: pve, manifest: path, state: filepath.Join(dir, "accrete.state.json")}
}

// accrete runs one command with stdin and returns its output and exit code.
// The token's secret must never be in what it printed.
func (l *applyLab) accrete(stdin string, args ...string) (string, int) {
	l.t.Helper()
	var out bytes.Buffer
	args = append(args, "-f", l.manifest)
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &out, func(k string) string {
		if k == applyTokenVar {
			return pvetest.Token
		}
		return ""
	})
	if strings.Contains(out.String(), pvetest.Secret) {
		l.t.Fatalf("the token's secret reached the output:\n%s", out.String())
	}
	if _, err := os.Stat(l.state + ".lock"); err == nil {
		l.t.Errorf("the lock was left behind after %v", args)
	}
	return out.String(), code
}

func (l *applyLab) loadState() State {
	l.t.Helper()
	s, err := loadState(l.state)
	if err != nil {
		l.t.Fatal(err)
	}
	return s
}

func (l *applyLab) plan() apiPlan {
	l.t.Helper()
	out, code := l.accrete("", "plan", "-o", "json")
	var p apiPlan
	if err := json.Unmarshal([]byte(out), &p); err != nil || code == 2 {
		l.t.Fatalf("plan: exit %d: %v\n%s", code, err, out)
	}
	return p
}

func current(s State) []plan.Realization {
	var out []plan.Realization
	for _, r := range s.Realizations {
		if r.Current() {
			out = append(out, r)
		}
	}
	return out
}

func TestApplyBuildsTheMachinesAndThePlanThenSaysUnchanged(t *testing.T) {
	l := newApplyLab(t, "")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	for _, want := range []string{"create web/0", "create web/1", "cloned as pve1/qemu/100", "seeded with local:iso/accrete-", "started", "confirmed and recorded", "2 done, 0 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("apply output lacks %q:\n%s", want, out)
		}
	}
	s := l.loadState()
	if len(s.Applying) != 0 || len(current(s)) != 2 {
		t.Fatalf("state: %d applying, %d realizations", len(s.Applying), len(current(s)))
	}
	for i, r := range current(s) {
		name := fmt.Sprintf("accrete-web-%d", i+1)
		ids := l.pve.Named(name)
		if len(ids) != 1 {
			t.Fatalf("guests named %s: %v", name, ids)
		}
		g := l.pve.Guest(ids[0])
		if g.Status != "running" || g.Config["smbios1"] != "uuid="+r.MintedUUID || fmt.Sprint(g.Config["memory"]) != "1024" {
			t.Errorf("%s: %s %v", name, g.Status, g.Config)
		}
		if s.Verdicts[*r.AssetID] != plan.AcceptanceAccepted || r.Provenance.RunbookName == "" || !r.Managed {
			t.Errorf("realization %+v, verdict %q", r, s.Verdicts[*r.AssetID])
		}
		if _, ok := l.pve.Files["local:iso/accrete-"+strings.ToLower(r.MintedUUID)+".iso"]; !ok {
			t.Errorf("no seed for %s", name)
		}
	}
	p := l.plan()
	if c := p.Plan.Counts(); c[plan.ActionNoOp] != 2 || len(p.Entries) != 2 {
		t.Errorf("plan after apply: %s", p.Summary)
	}
	// Applying again has nothing to do and sends nothing.
	before := len(l.pve.Writes())
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 || !strings.Contains(out, "Nothing to apply") {
		t.Errorf("second apply: exit %d\n%s", code, out)
	}
	if w := l.pve.Writes(); len(w) != before {
		t.Errorf("a second apply wrote %v", w[before:])
	}
}

func TestApplyAsksAndOnlyYesProceeds(t *testing.T) {
	for _, answer := range []string{"", "no\n", "y\n", "YES please\n"} {
		l := newApplyLab(t, "")
		out, code := l.accrete(answer, "apply")
		if code != 1 || !strings.Contains(out, "Apply these 2 actions?") || !strings.Contains(out, "Cancelled") {
			t.Errorf("answer %q: exit %d\n%s", answer, code, out)
		}
		if w := l.pve.Writes(); len(w) != 0 {
			t.Errorf("answer %q: sent %v", answer, w)
		}
		if _, err := os.Stat(l.state); err == nil {
			t.Errorf("answer %q: the state was written", answer)
		}
	}
	l := newApplyLab(t, "")
	if out, code := l.accrete("yes\n", "apply"); code != 0 {
		t.Fatalf("yes: exit %d\n%s", code, out)
	}
}

// An apply cut off between the clone and the identity write leaves a record;
// the plan shows the machine as unknown, and the next apply finishes the
// same guest rather than cloning another.
func TestAnInterruptedApplyResumesTheSameMachine(t *testing.T) {
	l := newApplyLab(t, "")
	l.pve.CutNext(`^POST /nodes/pve1/qemu/100/config$`)
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 || !strings.Contains(out, "run accrete apply again") || !strings.Contains(out, "1 done, 1 failed") {
		t.Fatalf("interrupted apply: exit %d\n%s", code, out)
	}
	s := l.loadState()
	if len(s.Applying) != 1 || s.Applying[0].Step != stepClone || len(current(s)) != 1 {
		t.Fatalf("state after the interruption: %+v", s.Applying)
	}
	p := l.plan()
	if c := p.Plan.Counts(); c[plan.ActionUnknown] != 1 || c[plan.ActionNoOp] != 1 {
		t.Errorf("plan while half-built: %s", p.Summary)
	}
	out, code = l.accrete("", "apply", "--auto-approve")
	if code != 0 || !strings.Contains(out, "finish web/0 (half-built by an earlier apply)") {
		t.Fatalf("resumed apply: exit %d\n%s", code, out)
	}
	if ids := l.pve.Named("accrete-web-1"); len(ids) != 1 || ids[0] != 100 {
		t.Errorf("guests named accrete-web-1: %v, want only 100", ids)
	}
	if s := l.loadState(); len(s.Applying) != 0 || len(current(s)) != 2 {
		t.Errorf("state after the resume: %d applying, %d built", len(s.Applying), len(current(s)))
	}
}

func TestDestroyRemovesWhatApplyBuilt(t *testing.T) {
	l := newApplyLab(t, "")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	out, code := l.accrete("yes\n", "destroy")
	if code != 0 || !strings.Contains(out, "Destroy these 2 actions?") || strings.Count(out, "deleted; record retired") != 2 {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	if l.pve.Guest(100) != nil || l.pve.Guest(102) != nil || l.pve.Guest(101) == nil {
		t.Errorf("after destroy: 100 %v, 102 %v, foreign 101 %v", l.pve.Guest(100), l.pve.Guest(102), l.pve.Guest(101))
	}
	for v := range l.pve.Files {
		if strings.Contains(v, "accrete-") {
			t.Errorf("seed %s left behind", v)
		}
	}
	if s := l.loadState(); len(current(s)) != 0 || len(s.Realizations) != 2 {
		t.Errorf("state after destroy: %d current of %d", len(current(s)), len(s.Realizations))
	}
	if out, code := l.accrete("", "destroy"); code != 0 || !strings.Contains(out, "Nothing to destroy") {
		t.Errorf("second destroy: exit %d\n%s", code, out)
	}
}

// A delete whose answer was lost leaves the guest gone and the record
// current. The next destroy sees nothing at the ref under a complete listing
// and, told to forget gone machines, retires the record.
func TestALostDeleteAnswerIsRetiredByTheNextRun(t *testing.T) {
	l := newApplyLab(t, "")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	l.pve.DropNext(`^DELETE /nodes/pve1/qemu/100$`)
	l.accrete("", "destroy", "--auto-approve")
	if l.pve.Guest(100) != nil {
		t.Fatal("the dropped DELETE was not carried out; the test proves nothing")
	}
	out, code := l.accrete("", "destroy", "--auto-approve", "--forget-gone")
	if code != 0 || len(current(l.loadState())) != 0 {
		t.Fatalf("second destroy: exit %d, %d still recorded\n%s", code, len(current(l.loadState())), out)
	}
}

// A record whose clone never answered is not dropped while the provider
// lists a guest that may be that clone, still copying under no name.
func TestDestroyKeepsARecordWhileItsCloneMayStillBeCopying(t *testing.T) {
	l := newApplyLab(t, "")
	l.pve.CutNext(`^POST /nodes/pve1/qemu/9000/clone$`)
	l.accrete("", "apply", "--auto-approve")
	if s := l.loadState(); len(s.Applying) != 1 || s.Applying[0].ResourceRefID != "" {
		t.Fatalf("applying: %+v", s.Applying)
	}
	l.pve.Guests[150] = &pvetest.Guest{VMID: 150, Kind: "qemu", Status: "stopped", Config: map[string]any{"lock": "clone"}}
	out, code := l.accrete("", "destroy", "--auto-approve")
	if code != 1 || !strings.Contains(out, "may be accrete-web-1's clone still copying") {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	if s := l.loadState(); len(s.Applying) != 1 {
		t.Errorf("the record was dropped while a clone may be copying")
	}
}

// A guest of somebody else's that has no SMBIOS UUID (an old or imported
// one, with a name of its own) is not taken for a clone still copying.
func TestDestroyDropsANeverAnsweredCloneDespiteAnUnrelatedGuestWithoutIdentity(t *testing.T) {
	l := newApplyLab(t, "")
	l.pve.CutNext(`^POST /nodes/pve1/qemu/9000/clone$`)
	l.accrete("", "apply", "--auto-approve")
	if s := l.loadState(); len(s.Applying) != 1 || s.Applying[0].ResourceRefID != "" {
		t.Fatalf("applying: %+v", s.Applying)
	}
	l.pve.Guests[150] = &pvetest.Guest{VMID: 150, Kind: "qemu", Status: "running", Config: map[string]any{"name": "legacy-db"}}
	out, code := l.accrete("", "destroy", "--auto-approve")
	if code != 0 || len(l.loadState().Applying) != 0 {
		t.Fatalf("destroy: exit %d, applying %+v\n%s", code, l.loadState().Applying, out)
	}
	if l.pve.Guest(150) == nil {
		t.Error("the unrelated guest was removed")
	}
}

// A machine whose guest no longer carries its identity is unknown in the
// plan, and destroy leaves it alone while it removes the rest.
func TestDestroyLeavesAMachineTheProviderDoesNotConfirm(t *testing.T) {
	l := newApplyLab(t, "")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	l.pve.SetConfig(100, "smbios1", "uuid=99999999-8888-4777-8666-555555555555")
	out, code := l.accrete("", "destroy", "--auto-approve")
	// Exit 1: a machine is still recorded, and a script must not read the
	// environment as gone.
	if code != 1 || !strings.Contains(out, "not done: unknown web/0: identity mismatch") || strings.Count(out, "deleted; record retired") != 1 {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	if l.pve.Guest(100) == nil || l.pve.Guest(102) != nil {
		t.Errorf("after destroy: 100 %v, 102 %v", l.pve.Guest(100), l.pve.Guest(102))
	}
}

func TestDestroyHandlesHalfBuiltMachines(t *testing.T) {
	// The clone request never arrived: nothing was built, the record goes.
	l := newApplyLab(t, "")
	l.pve.CutNext(`^POST /nodes/pve1/qemu/9000/clone$`)
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	out, code := l.accrete("", "destroy", "--auto-approve")
	if code != 0 || !strings.Contains(out, "nothing was built; record dropped") {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	if s := l.loadState(); len(s.Applying) != 0 || len(current(s)) != 0 {
		t.Errorf("state after destroy: %+v", s)
	}

	// Cloned but never stamped: the guest may be the machine, but nothing
	// proves it, so destroy leaves it and says how to finish it.
	l = newApplyLab(t, "")
	l.pve.CutNext(`^POST /nodes/pve1/qemu/100/config$`)
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	out, code = l.accrete("", "destroy", "--auto-approve")
	if code != 1 || !strings.Contains(out, "run accrete apply to finish it") || l.pve.Guest(100) == nil {
		t.Fatalf("destroy of an unstamped clone: exit %d\n%s", code, out)
	}
	// Finished by apply, it is then destroyed like any other.
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	if out, code := l.accrete("", "destroy", "--auto-approve"); code != 0 || l.pve.Guest(100) != nil {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
}

// A half-built machine whose guest is no longer listed keeps its record:
// the token may just not see it. --forget-gone drops it.
func TestDestroyKeepsAHalfBuiltRecordWhoseGuestIsNotListed(t *testing.T) {
	l := newApplyLab(t, "")
	l.pve.CutNext(`^POST /nodes/pve1/qemu/100/status/start$`)
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	if s := l.loadState(); len(s.Applying) != 1 || s.Applying[0].ResourceRefID == "" {
		t.Fatalf("applying: %+v", s.Applying)
	}
	delete(l.pve.Guests, 100)
	out, code := l.accrete("", "destroy", "--auto-approve")
	if code != 1 || !strings.Contains(out, "run again with --forget-gone") || len(l.loadState().Applying) != 1 {
		t.Fatalf("destroy: exit %d, applying %d\n%s", code, len(l.loadState().Applying), out)
	}
	out, code = l.accrete("", "destroy", "--auto-approve", "--forget-gone")
	if code != 0 || !strings.Contains(out, "record dropped") || len(l.loadState().Applying) != 0 {
		t.Fatalf("destroy --forget-gone: exit %d\n%s", code, out)
	}
}

// A declared machine the provider does not confirm is not what the
// manifest asks for: apply leaves it alone, says so, and exits 1.
func TestApplyReportsADeclaredMachineItCannotConfirm(t *testing.T) {
	l := newApplyLab(t, "")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	delete(l.pve.Guests, 100)
	n := len(l.pve.Writes())
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 || !strings.Contains(out, "not done: unknown web/") {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	if w := l.pve.Writes()[n:]; len(w) != 0 {
		t.Errorf("apply sent %v", w)
	}
}

func TestApplyPowersAMachineAsDeclared(t *testing.T) {
	l := newApplyLab(t, "")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	src, _ := os.ReadFile(l.manifest)
	if err := os.WriteFile(l.manifest, []byte(strings.Replace(string(src), "      count: 2\n", "      count: 2\n      power: \"off\"\n", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 || !strings.Contains(out, "power off web/0") {
		t.Fatalf("apply power off: exit %d\n%s", code, out)
	}
	if l.pve.Guest(100).Status != "stopped" || l.pve.Guest(102).Status != "stopped" {
		t.Errorf("after power off: %s, %s", l.pve.Guest(100).Status, l.pve.Guest(102).Status)
	}
}

func TestApplyRefusesWhatV01CannotBuildBeforeSendingAnything(t *testing.T) {
	cases := map[string]string{
		"fixed address":    "        ip_address: 10.0.0.5/24\n",
		"second card":      "        net1: virtio,bridge=vmbr1\n",
		"secret reference": "        bootstrap:\n          run:\n            - echo {{secret.db_password}}\n",
		"firewall":         "      firewall:\n        rules:\n          - {direction: in, action: ACCEPT, proto: tcp, dport: \"22\"}\n",
	}
	for name, extra := range cases {
		l := newApplyLab(t, extra)
		out, code := l.accrete("yes\n", "apply")
		if code != 1 || !strings.Contains(out, "refused:") || !strings.Contains(out, "Nothing was sent") {
			t.Errorf("%s: exit %d\n%s", name, code, out)
		}
		if w := l.pve.Writes(); len(w) != 0 {
			t.Errorf("%s: sent %v", name, w)
		}
	}
}

func TestALockIsHonouredAndAStaleOneTakenOver(t *testing.T) {
	l := newApplyLab(t, "")
	host, _ := os.Hostname()
	held, _ := json.Marshal(stateLock{Host: host, PID: os.Getpid(), Started: time.Now()})
	if err := os.WriteFile(l.state+".lock", held, 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := run(context.Background(), []string{"apply", "--auto-approve", "-f", l.manifest}, strings.NewReader(""), &out, &out,
		func(string) string { return pvetest.Token })
	if code != 2 || !strings.Contains(out.String(), "another accrete is running") || len(l.pve.Writes()) != 0 {
		t.Fatalf("a held lock: exit %d\n%s", code, out.String())
	}
	if _, err := os.Stat(l.state + ".lock"); err != nil {
		t.Fatal("a held lock was removed")
	}
	stale, _ := json.Marshal(stateLock{Host: host, PID: 1 << 30, Started: time.Now()})
	if err := os.WriteFile(l.state+".lock", stale, 0o600); err != nil {
		t.Fatal(err)
	}
	got, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 || !strings.Contains(got, "took over") {
		t.Fatalf("a stale lock: exit %d\n%s", code, got)
	}
}

// A state with a half-built machine is one an older accrete refuses rather
// than planning a second machine; this one reads it back as it wrote it.
func TestTheStateRoundTripsWithAnApplyingRecord(t *testing.T) {
	l := newApplyLab(t, "")
	l.pve.CutNext(`^POST /nodes/pve1/qemu/100/config$`)
	l.accrete("", "apply", "--auto-approve")
	raw, err := os.ReadFile(l.state)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"applying"`) || !strings.Contains(string(raw), `"owner_mark": "accrete:`) {
		t.Fatalf("state:\n%s", raw)
	}
	if info, _ := os.Stat(l.state); info.Mode().Perm() != 0o600 {
		t.Errorf("state mode %v, want 0600", info.Mode().Perm())
	}
}

// A machine removed by hand: the provider listing nothing at its ref is no
// proof (it leaves out guests the token cannot see), so destroy keeps the
// record and exits 1. With --forget-gone it retires the record, and sends no
// DELETE for it.
func TestDestroyRetiresAMachineAlreadyGone(t *testing.T) {
	l := newApplyLab(t, "")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	delete(l.pve.Guests, 100)
	out, code := l.accrete("", "destroy", "--auto-approve")
	if code != 1 || !strings.Contains(out, "run again with --forget-gone") || len(current(l.loadState())) != 1 {
		t.Fatalf("destroy without --forget-gone: exit %d, %d recorded\n%s", code, len(current(l.loadState())), out)
	}
	out, code = l.accrete("", "destroy", "--auto-approve", "--forget-gone")
	if code != 0 || !strings.Contains(out, "retire web/0 (pve1/qemu/100 is already gone)") || !strings.Contains(out, "already gone; record retired") {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	for _, w := range l.pve.Writes() {
		if strings.HasPrefix(w, "DELETE /nodes/pve1/qemu/100") {
			t.Errorf("sent %s for a guest already gone", w)
		}
	}
	if n := len(current(l.loadState())); n != 0 {
		t.Errorf("%d still recorded", n)
	}
}
