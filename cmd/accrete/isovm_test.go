package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

// A machine that boots an installer needing no answer file from accrete (an
// OKD agent ISO carries its own): Sybil's provision-proxmox-iso-vm, built
// against the in-memory PVE.

const isoVMManifest = `apiVersion: accretion/v1alpha1
kind: Environment
metadata:
  name: okd
spec:
  provider:
    type: proxmox
    endpoint: ENDPOINT
    verify_tls: false
    token_env: ACCRETE_PVETEST_TOKEN
  resources:
    - name: master
      count: 3
      blueprint: provision-proxmox-iso-vm
      inputs:
        pve_node: pve1
        hostname: okd-master-{{instance.number}}
        iso: local:iso/agent.x86_64.iso
        disk_gb: 120
        cores: 4
        memory_mb: 16384
        net0: virtio=52:54:00:aa:bb:0{{instance.number}},bridge=vmbr0
EXTRA`

const agentISO = "local:iso/agent.x86_64.iso"

func newISOVMLab(t *testing.T, extra string) *applyLab {
	t.Helper()
	pve := pvetest.New(t)
	pve.Files[agentISO] = []byte("OKD agent ISO")
	dir := t.TempDir()
	path := filepath.Join(dir, "env.yaml")
	src := strings.Replace(strings.Replace(isoVMManifest, "ENDPOINT", pve.URL, 1), "EXTRA", extra, 1)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	shorten(t)
	return &applyLab{t: t, pve: pve, manifest: path, state: filepath.Join(dir, "accrete.state.json")}
}

func TestApplyBootsAnInstallerThatNeedsNoAnswerFile(t *testing.T) {
	l := newISOVMLab(t, "")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	mustContain(t, out, "create master/0", "empty 120 GiB disk, booting "+agentISO,
		"what the installer does inside the machine is not checked",
		"accepted and recorded on the hypervisor's evidence only", "3 done, 0 failed")
	if strings.Contains(out, "answer disc") {
		t.Errorf("an answer disc was made:\n%s", out)
	}
	s := l.loadState()
	if n := len(current(s)); n != 3 {
		t.Fatalf("%d machines recorded, want 3", n)
	}
	for _, r := range current(s) {
		if s.Verdicts[*r.AssetID] != plan.AcceptanceAccepted {
			t.Errorf("%s: verdict %q", r.ResourceRefID, s.Verdicts[*r.AssetID])
		}
		ids := l.pve.Named(r.IntentInputs["hostname"].(string))
		if len(ids) != 1 {
			t.Fatalf("guests named %v: %v", r.IntentInputs["hostname"], ids)
		}
		g := l.pve.Guest(ids[0])
		mac := "52:54:00:aa:bb:0" + strings.TrimPrefix(r.IntentInputs["hostname"].(string), "okd-master-")
		if g.Status != "running" || g.Config["smbios1"] != "uuid="+r.MintedUUID || g.Config["ide2"] != agentISO+",media=cdrom" ||
			g.Config["boot"] != "order=scsi0;ide2" || !strings.Contains(strings.ToLower(g.Config["net0"].(string)), mac) {
			t.Errorf("%s: %s %v", r.ResourceRefID, g.Status, g.Config)
		}
		if _, ok := g.Config["ide3"]; ok {
			t.Errorf("%s has an ide3 disc: %v", r.ResourceRefID, g.Config["ide3"])
		}
	}
	for volid := range l.pve.Files {
		if volid != agentISO && strings.HasPrefix(volid, "local:iso/") {
			t.Errorf("accrete left %s on storage", volid)
		}
	}
	if c := l.plan().Plan.Counts(); c[plan.ActionNoOp] != 3 {
		t.Errorf("plan after apply: %v", c)
	}
	if out, code := l.accrete("", "destroy", "--auto-approve"); code != 0 || len(current(l.loadState())) != 0 {
		t.Fatalf("destroy: exit %d\n%s", code, out)
	}
	if _, ok := l.pve.Files[agentISO]; !ok {
		t.Error("destroy removed the installer ISO")
	}
}

func TestAnISOVMIsRefusedBeforeAnythingIsSent(t *testing.T) {
	for name, c := range map[string]struct{ extra, want string }{
		"missing ISO":       {"", "agent.x86_64.iso"},
		"unknown input":     {"        bootstrap: {users: []}\n", "input bootstrap: provision-proxmox-iso-vm takes no such input"},
		"enrolment failure": {"        on_enrolment_failure: keep\n", "only the default, destroy, is taken"},
	} {
		t.Run(name, func(t *testing.T) {
			l := newISOVMLab(t, c.extra)
			if name == "missing ISO" {
				delete(l.pve.Files, agentISO)
			}
			out, code := l.accrete("", "apply", "--auto-approve")
			if code == 0 || !strings.Contains(out, c.want) {
				t.Fatalf("exit %d, want %q in:\n%s", code, c.want, out)
			}
			if w := l.pve.Writes(); len(w) != 0 {
				t.Errorf("sent %v", w)
			}
		})
	}
}

func TestAnISOVMNeedsNoGuestAgentNorAnswerStorage(t *testing.T) {
	l := newISOVMLab(t, "")
	out, code := l.accrete("", "permissions", "--offline")
	if code == 2 {
		t.Fatalf("permissions: exit %d\n%s", code, out)
	}
	mustContain(t, out, "/storage/local-lvm", "/vms")
	for _, not := range []string{"guest agent", "answer disc"} {
		if strings.Contains(out, not) {
			t.Errorf("permissions mention %q:\n%s", not, out)
		}
	}
}
