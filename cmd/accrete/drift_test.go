package main

import "testing"

func TestPlanDetailedExitCodeSaysWhetherAnythingDiffers(t *testing.T) {
	l := newApplyLab(t, "")
	exit := func(args ...string) int {
		t.Helper()
		_, code := l.accrete("", append([]string{"plan"}, args...)...)
		return code
	}
	if c := exit(); c != 0 {
		t.Errorf("plan of machines to create: exit %d, want 0 without the flag", c)
	}
	if c := exit("--detailed-exitcode"); c != 3 {
		t.Errorf("plan of machines to create: exit %d, want 3", c)
	}
	l.built()
	if c := exit("--detailed-exitcode"); c != 0 {
		t.Errorf("plan of built machines: exit %d, want 0", c)
	}
	if c := exit("--detailed-exitcode", "-o", "json"); c != 0 {
		t.Errorf("json plan of built machines: exit %d, want 0", c)
	}
	delete(l.pve.Guests, 100)
	if c := exit("--detailed-exitcode"); c != 4 {
		t.Errorf("plan with a machine gone: exit %d, want 4", c)
	}
	if c := exit("--detailed-exitcode", "--offline"); c != 4 {
		t.Errorf("offline plan: exit %d, want 4", c)
	}
}
