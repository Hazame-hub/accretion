package plan

import (
	"strings"
	"testing"
)

// A guest that nothing accounts for already carries web-2's name: most often
// what a create that failed after its clone left behind. web-2 is unknown and
// names the guest; web-1, whose name is free, is created.
func TestACreateBesideAnUnaccountedGuestOfItsNameIsUnknown(t *testing.T) {
	w := world()
	w.Unmanaged = map[string]string{"web-2": "pve/qemu/104"}
	p := Build(env(web(2)), w)
	if len(p.Entries) != 2 {
		t.Fatalf("entries = %+v", p.Entries)
	}
	if p.Entries[0].Action != ActionCreate {
		t.Fatalf("web-1 = %s (%s), want create", p.Entries[0].Action, p.Entries[0].Reason)
	}
	if e := p.Entries[1]; e.Action != ActionUnknown || !strings.Contains(e.Reason, `"web-2"`) || !strings.Contains(e.Reason, "pve/qemu/104") {
		t.Fatalf("web-2 = %s (%s), want unknown naming pve/qemu/104", e.Action, e.Reason)
	}
}

// Guests that were not listed are a limit on what the plan checked, said
// only when it creates something.
func TestUnlistedGuestsAreALimit(t *testing.T) {
	w := world()
	w.UnmanagedError = "403 Forbidden"
	p := Build(env(web(1)), w)
	found := false
	for _, l := range p.Limits {
		found = found || strings.Contains(l, NamesNotChecked) && strings.Contains(l, "403")
	}
	if !found {
		t.Fatalf("limits = %v", p.Limits)
	}

	p = Build(env(web(0)), w)
	for _, l := range p.Limits {
		if strings.Contains(l, NamesNotChecked) {
			t.Fatalf("nothing to create, and limits = %v", p.Limits)
		}
	}
}

// Not read at all (an embedder that does not list guests, such as accrete
// before it did): plans exactly as before, and says nothing.
func TestUnreadGuestsChangeNothing(t *testing.T) {
	p := Build(env(web(1)), world())
	if p.Entries[0].Action != ActionCreate {
		t.Fatalf("web-1 = %s", p.Entries[0].Action)
	}
	for _, l := range p.Limits {
		if strings.Contains(l, NamesNotChecked) {
			t.Fatalf("limits = %v", p.Limits)
		}
	}
}
