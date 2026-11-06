package main

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"testing"
)

// declare rewrites the manifest's memory_mb.
func (l *applyLab) declareMemory(mb int) {
	l.t.Helper()
	src, _ := os.ReadFile(l.manifest)
	out := strings.Replace(string(src), "memory_mb: 1024", fmt.Sprintf("memory_mb: %d", mb), 1)
	if err := os.WriteFile(l.manifest, []byte(out), 0o600); err != nil {
		l.t.Fatal(err)
	}
}

func memoryOf(l *applyLab, vmid int) string {
	g := l.pve.Guest(vmid)
	return fmt.Sprint(g.Config["memory"])
}

// Gate 1: a manifest change is applied in place. Proxmox keeps memory pending
// on a running guest without hotplug, so the machine is restarted, then
// confirmed, then recorded; the next plan is a no-op.
func TestApplyResizesInPlaceAndRestartsWhenNeeded(t *testing.T) {
	l := builtLab(t, "")
	l.declareMemory(2048)
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	for _, want := range []string{"change web/0 (pve1/qemu/100)", "set, and restarted to apply it", "confirmed and recorded: as declared", "2 done, 0 failed, 0 not done"} {
		if !strings.Contains(out, want) {
			t.Errorf("apply lacks %q:\n%s", want, out)
		}
	}
	for _, vmid := range []int{100, 102} {
		if got := memoryOf(l, vmid); got != "2048" || l.pve.Guest(vmid).Pending != nil {
			t.Errorf("vm %d: memory %s, pending %v", vmid, got, l.pve.Guest(vmid).Pending)
		}
	}
	if p := l.plan(); !strings.Contains(p.Summary, "2 unchanged") {
		t.Errorf("after: %s", p.Summary)
	}
	// Again: nothing to do, nothing sent.
	before := len(l.pve.Writes())
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 || !strings.Contains(out, "Nothing to apply.") {
		t.Fatalf("second apply: exit %d\n%s", code, out)
	}
	if after := l.pve.Writes(); len(after) != before {
		t.Errorf("a second apply wrote %v", after[before:])
	}
}

// Drift is put back: memory set outside accrete returns to the declared size.
func TestApplyPutsBackDrift(t *testing.T) {
	l := builtLab(t, "")
	l.pve.SetConfig(100, "memory", 2048)
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 || !strings.Contains(out, "confirmed and recorded: as declared") {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	if got := memoryOf(l, 100); got != "1024" {
		t.Errorf("memory %s, want 1024", got)
	}
	if p := l.plan(); !strings.Contains(p.Summary, "2 unchanged") {
		t.Errorf("after: %s", p.Summary)
	}
}

// Lab case: a memory edit pending the next restart. Setting the declared
// value back cancels it without a restart.
func TestApplyCancelsAPendingEditWithoutARestart(t *testing.T) {
	l := builtLab(t, "")
	l.pve.SetPending(100, "memory", "2048")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 || !strings.Contains(out, "  set\n") || strings.Contains(out, "restarted") {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	if g := l.pve.Guest(100); memoryOf(l, 100) != "1024" || len(g.Pending) != 0 {
		t.Errorf("memory %s, pending %v", memoryOf(l, 100), g.Pending)
	}
	for _, r := range l.pve.Writes() {
		if strings.Contains(r, "/status/reboot") {
			t.Errorf("restarted: %s", r)
		}
	}
}

// A machine that is not the one recorded is never resized.
func TestResizeRefusesAMachineThatLostItsIdentity(t *testing.T) {
	l := builtLab(t, "")
	l.declareMemory(2048)
	l.pve.SetConfig(100, "smbios1", "uuid=00000000-0000-4000-8000-000000000001")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code == 0 {
		t.Fatalf("apply: exit 0\n%s", out)
	}
	if memoryOf(l, 100) == "2048" {
		t.Error("a machine with another identity was resized")
	}
}

// Lab case (R8): a machine built without disk_gb grows its disk. What the
// provider reports of the disk stands in for the size never recorded, and a
// size below it is still refused.
func TestApplyGrowsADiskThatWasNotDeclaredAtBuild(t *testing.T) {
	l := builtLab(t, "")
	setDisk := func(gb int) {
		src, _ := os.ReadFile(l.manifest)
		out := regexp.MustCompile(`(?m)^(\s*)memory_mb: 1024\n(\s*disk_gb: \d+\n)?`).
			ReplaceAllString(string(src), fmt.Sprintf("${1}memory_mb: 1024\n${1}disk_gb: %d\n", gb))
		if err := os.WriteFile(l.manifest, []byte(out), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	setDisk(8)
	out, code := l.accrete("", "apply", "--auto-approve")
	if code == 0 || !strings.Contains(out, "never shrunk: disk_gb (10 to 8)") {
		t.Fatalf("a shrink: exit %d\n%s", code, out)
	}

	setDisk(12)
	out, code = l.accrete("", "apply", "--auto-approve")
	if code != 0 || !strings.Contains(out, "confirmed and recorded: as declared") {
		t.Fatalf("grow: exit %d\n%s", code, out)
	}
	if disk := fmt.Sprint(l.pve.Guest(100).Config["scsi0"]); !strings.Contains(disk, "size=12G") {
		t.Errorf("scsi0 = %s, want size=12G", disk)
	}
	if p := l.plan(); !strings.Contains(p.Summary, "2 unchanged") {
		t.Errorf("after: %s", p.Summary)
	}
}
