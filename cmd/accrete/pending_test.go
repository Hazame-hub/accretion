package main

import (
	"strings"
	"testing"
)

// Lab, 2026-11-02: memory set to 2048 in the PVE UI on a running VM without
// memory hotplug stays pending; plan said "no-op" until the VM restarted.
func TestPlanReportsAChangePendingTheNextRestart(t *testing.T) {
	l := builtLab(t, "")
	l.pve.SetPending(100, "memory", 2048)

	out, _ := l.accrete("", "plan")
	for _, want := range []string{
		"memory_mb was set to 2048 and takes effect at the machine's next restart, the manifest declares 1024",
		"memory_mb: 2048 (pending, at next restart) -> 1024",
		"1 to change",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	e := entryAt(t, l.plan(), "web", 0)
	if !e.Drift || len(e.Diff) != 1 || !e.Diff[0].Pending {
		t.Errorf("web/0: drift %v, diff %+v", e.Drift, e.Diff)
	}
	if e1 := entryAt(t, l.plan(), "web", 1); e1.Drift {
		t.Errorf("web/1 has nothing pending but drifted: %+v", e1.Diff)
	}
}

// A pending value equal to the declared one is not drift.
func TestAPendingValueAsDeclaredIsNotDrift(t *testing.T) {
	l := builtLab(t, "")
	l.pve.SetPending(100, "memory", 1024)
	if p := l.plan(); !strings.Contains(p.Summary, "0 to change") {
		t.Errorf("summary %q", p.Summary)
	}
}
