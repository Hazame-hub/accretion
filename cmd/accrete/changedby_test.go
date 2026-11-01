package main

import (
	"os"
	"strings"
	"testing"
	"time"

	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

// builtLab applies applyManifest (with extra after count:) and returns the
// lab with web/0 at VMID 100 and web/1 at 102.
func builtLab(t *testing.T, afterCount string) *applyLab {
	t.Helper()
	l := newApplyLab(t, "")
	if afterCount != "" {
		src, _ := os.ReadFile(l.manifest)
		if err := os.WriteFile(l.manifest, []byte(strings.Replace(string(src), "      count: 2\n", "      count: 2\n"+afterCount, 1)), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	return l
}

var t0 = time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC)

func entryAt(t *testing.T, p apiPlan, resource string, index int) plan.Entry {
	t.Helper()
	for _, e := range p.Entries {
		if e.Resource == resource && e.Index == index {
			return e
		}
	}
	t.Fatalf("no entry %s/%d in %s", resource, index, p.Summary)
	return plan.Entry{}
}

func TestDriftNamesWhoChangedIt(t *testing.T) {
	l := builtLab(t, "")
	l.pve.SetConfig(100, "memory", 2048)
	l.pve.AddTask(pvetest.Task{Type: "qmconfig", User: "accrete@pve!test", VMID: 100, Start: t0})
	l.pve.AddTask(pvetest.Task{Type: "qmconfig", User: "hamza@pve", VMID: 100, Start: t0.Add(time.Hour)})
	l.pve.AddTask(pvetest.Task{Type: "qmconfig", User: "someone@pve", VMID: 102, Start: t0.Add(2 * time.Hour)})

	out, _ := l.accrete("", "plan")
	if !strings.Contains(out, "changed by hamza@pve at 2026-11-01T13:00:00Z (qmconfig)") {
		t.Errorf("the author is not named:\n%s", out)
	}
	e := entryAt(t, l.plan(), "web", 0)
	if !e.Drift || e.ChangedBy == nil || e.ChangedBy.By != "hamza@pve" {
		t.Errorf("web/0: drift %v, changed_by %+v", e.Drift, e.ChangedBy)
	}
	if e1 := entryAt(t, l.plan(), "web", 1); e1.ChangedBy != nil {
		t.Errorf("web/1 did not drift but carries %+v", e1.ChangedBy)
	}
	for _, w := range l.pve.Writes() {
		if strings.Contains(w, "tasks") {
			t.Errorf("plan wrote %s", w)
		}
	}
}

func TestDriftWithoutARecordedChangeIsUnknown(t *testing.T) {
	l := builtLab(t, "")
	l.pve.SetConfig(100, "memory", 2048)
	out, _ := l.accrete("", "plan")
	if !strings.Contains(out, "changed by: unknown, no config change is recorded on this machine") {
		t.Errorf("a drift with no task is not unknown:\n%s", out)
	}
}

func TestDriftAfterAccretesOwnLastChangeIsUnknown(t *testing.T) {
	l := builtLab(t, "")
	l.pve.SetConfig(100, "memory", 2048)
	l.pve.AddTask(pvetest.Task{Type: "qmconfig", User: "hamza@pve", VMID: 100, Start: t0})
	l.pve.AddTask(pvetest.Task{Type: "qmconfig", User: "accrete@pve!test", VMID: 100, Start: t0.Add(time.Hour)})
	l.pve.AddTask(pvetest.Task{Type: "qmconfig", User: "eve@pve", VMID: 100, Start: t0.Add(2 * time.Hour), Status: "unable to parse value"})
	out, _ := l.accrete("", "plan")
	if !strings.Contains(out, "the last recorded config change is accrete's own") {
		t.Errorf("an older author, or a failed task, was blamed:\n%s", out)
	}
}

func TestPowerDifferenceNamesWhoStoppedIt(t *testing.T) {
	l := builtLab(t, "      power: \"on\"\n")
	l.pve.SetStatus(100, "stopped")
	l.pve.AddTask(pvetest.Task{Type: "qmstart", User: "accrete@pve!test", VMID: 100, Start: t0})
	l.pve.AddTask(pvetest.Task{Type: "qmshutdown", User: "hamza@pve", VMID: 100, Start: t0.Add(time.Hour)})
	out, _ := l.accrete("", "plan")
	if !strings.Contains(out, "changed by hamza@pve at 2026-11-01T13:00:00Z (qmshutdown)") {
		t.Errorf("the shutdown's author is not named:\n%s", out)
	}
}

func TestPowerDifferenceWithoutAnExplainingTaskIsUnknown(t *testing.T) {
	l := builtLab(t, "      power: \"on\"\n")
	l.pve.SetStatus(100, "stopped")
	l.pve.AddTask(pvetest.Task{Type: "qmstart", User: "hamza@pve", VMID: 100, Start: t0})
	out, _ := l.accrete("", "plan")
	if !strings.Contains(out, "does not explain it; a shutdown from inside the guest leaves no record") {
		t.Errorf("a start was blamed for a stop:\n%s", out)
	}
}
