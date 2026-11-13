package main

import (
	"os"
	"strings"
	"testing"
)

// built applies the two-machine lab and returns the minted UUID of web/0.
func (l *applyLab) built() string {
	l.t.Helper()
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		l.t.Fatalf("apply: exit %d\n%s", code, out)
	}
	for _, r := range current(l.loadState()) {
		if r.Intent.InstanceIndex == 0 {
			return r.MintedUUID
		}
	}
	l.t.Fatal("web/0 is not recorded")
	return ""
}

func TestReplaceDeletesAMachineAndBuildsItAgain(t *testing.T) {
	l := newApplyLab(t, "")
	was := l.built()
	out, code := l.accrete("", "apply", "--auto-approve", "--replace", "web/0")
	if code != 0 || !strings.Contains(out, "replace web/0 (delete pve1/qemu/100, then create it again)") || !strings.Contains(out, "deleted; record retired") {
		t.Fatalf("apply --replace: exit %d\n%s", code, out)
	}
	for id, g := range l.pve.Guests {
		if g.Config["smbios1"] == "uuid="+was {
			t.Errorf("the old machine is still there, as %d", id)
		}
	}
	ids := l.pve.Named("accrete-web-1")
	if len(ids) != 1 {
		t.Fatalf("guests named accrete-web-1: %v", ids)
	}
	var now string
	for _, r := range current(l.loadState()) {
		if r.Intent.InstanceIndex == 0 {
			now = r.MintedUUID
		}
	}
	if now == "" || now == was || l.pve.Guest(ids[0]).Config["smbios1"] != "uuid="+now {
		t.Errorf("web/0 is recorded as %q (was %q), guest %v", now, was, l.pve.Guest(ids[0]).Config)
	}
	if _, ok := l.pve.Files["local:iso/accrete-"+strings.ToLower(was)+".iso"]; ok {
		t.Error("the old machine's seed was left on storage")
	}
	if n := len(current(l.loadState())); n != 2 {
		t.Errorf("%d machines recorded, want 2", n)
	}
	if c := l.plan().Plan.Counts(); c["no-op"] != 2 {
		t.Errorf("plan after the replace: %v", c)
	}
}

func TestReplaceRebuildsAMachineThatWasRemovedByHand(t *testing.T) {
	l := newApplyLab(t, "")
	l.built()
	delete(l.pve.Guests, 100)
	out, code := l.accrete("", "apply", "--auto-approve", "--replace=web/0")
	if code != 0 || !strings.Contains(out, "pve1/qemu/100 is already gone: retire its record, then create it again") {
		t.Fatalf("apply --replace: exit %d\n%s", code, out)
	}
	for _, w := range l.pve.Writes() {
		if strings.HasPrefix(w, "DELETE /nodes/pve1/qemu/100") {
			t.Errorf("sent %s for a machine already gone", w)
		}
	}
	if ids := l.pve.Named("accrete-web-1"); len(ids) != 1 {
		t.Errorf("guests named accrete-web-1: %v", ids)
	}
	if c := l.plan().Plan.Counts(); c["no-op"] != 2 {
		t.Errorf("plan after the rebuild: %v", c)
	}
}

func TestReplaceRefusesBeforeSendingAnything(t *testing.T) {
	cases := map[string]struct {
		target, want string
		meddle       func(l *applyLab)
	}{
		"not declared": {"web/7", "--replace web/7: no declared machine is web/7", nil},
		"no index":     {"web", "--replace web: no declared machine is web", nil},
		"not its machine": {"web/0", "--replace web/0: its state cannot be confirmed", func(l *applyLab) {
			l.pve.Guests[100].Config["smbios1"] = "uuid=11111111-2222-3333-4444-555555555555"
		}},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			l := newApplyLab(t, "")
			l.built()
			if c.meddle != nil {
				c.meddle(l)
			}
			n := len(l.pve.Writes())
			out, code := l.accrete("", "apply", "--auto-approve", "--replace", c.target)
			if code == 0 || !strings.Contains(out, c.want) {
				t.Fatalf("exit %d, want %q in:\n%s", code, c.want, out)
			}
			if w := l.pve.Writes()[n:]; len(w) != 0 {
				t.Errorf("sent %v", w)
			}
		})
	}
}

func TestReplaceIsOnlyAnApplyFlag(t *testing.T) {
	l := newApplyLab(t, "")
	for _, cmd := range []string{"plan", "destroy"} {
		if _, code := l.accrete("", cmd, "--replace", "web/0"); code != 2 {
			t.Errorf("%s --replace: exit %d, want 2", cmd, code)
		}
	}
}

func TestReplaceIsHowAChangeThatCannotBeMadeInPlaceIsApplied(t *testing.T) {
	l := newApplyLab(t, "")
	l.built()
	tpl := *l.pve.Guests[9000]
	tpl.VMID, tpl.Config = 9001, map[string]any{}
	for k, v := range l.pve.Guests[9000].Config {
		tpl.Config[k] = v
	}
	tpl.Config["name"] = "rocky-10-cloud"
	l.pve.Guests[9001] = &tpl
	src, err := os.ReadFile(l.manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(l.manifest, []byte(strings.Replace(string(src), `template_vmid: "9000"`, `template_vmid: "9001"`, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 || !strings.Contains(out, "template_vmid cannot change in place") {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	out, code = l.accrete("", "apply", "--auto-approve", "--replace", "web/0,web/1")
	if code != 0 || strings.Count(out, "deleted; record retired") != 2 {
		t.Fatalf("apply --replace: exit %d\n%s", code, out)
	}
	if c := l.plan().Plan.Counts(); c["no-op"] != 2 {
		t.Errorf("plan after the replace: %v", c)
	}
}
