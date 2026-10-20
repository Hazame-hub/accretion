package proxmox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

const guestUUID = "421b7f1e-7b7a-3d6e-2c0e-9c4b5a6f7d8e"

// guestStub is one VM, pve1/qemu/105, named web-01 with guestUUID and a
// clone marker, plus a cluster listing that drops it after `goneAfter` reads
// (0 keeps it forever). It answers GETs only: a guard that sent anything else
// would be acting, and is failed.
type guestStub struct {
	mu        sync.Mutex
	listReads int
	goneAfter int
	power     []string // status/current answers in order; the last repeats
	powerRead int
	srv       *httptest.Server
}

func newGuestStub(t *testing.T) *guestStub {
	t.Helper()
	g := &guestStub{power: []string{"running"}}
	g.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("a guard sent %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusMethodNotAllowed)
			return
		}
		data := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
		g.mu.Lock()
		defer g.mu.Unlock()
		switch strings.TrimPrefix(r.URL.Path, "/api2/json") {
		case "/nodes/pve1/qemu/105/config":
			data(map[string]any{"name": "web-01", "description": "made by run 7", "smbios1": "uuid=" + guestUUID})
		case "/nodes/pve1/qemu/105/status/current":
			st := g.power[min(g.powerRead, len(g.power)-1)]
			g.powerRead++
			data(map[string]any{"status": st})
		case "/cluster/resources":
			g.listReads++
			if g.goneAfter > 0 && g.listReads > g.goneAfter {
				data([]any{})
				return
			}
			data([]map[string]any{{"vmid": 105, "name": "web-01", "node": "pve1", "type": "qemu", "status": "running"}})
		default:
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	t.Cleanup(g.srv.Close)
	return g
}

func (g *guestStub) client() *Client {
	return &Client{BaseURL: g.srv.URL, Token: "PVEAPIToken=test", HTTP: g.srv.Client(), TaskPoll: time.Millisecond}
}

func TestWithRefFillsNodeTypeAndVMID(t *testing.T) {
	p, err := WithRef("vm_stop", map[string]interface{}{"ref": "pve1/qemu/105", "timeout_s": 5})
	if err != nil {
		t.Fatal(err)
	}
	if p["node"] != "pve1" || p["type"] != "qemu" || p["vmid"] != "105" || p["timeout_s"] != 5 {
		t.Fatalf("params = %v", p)
	}
	if _, err := WithRef("vm_stop", map[string]interface{}{"ref": "pve1/qemu/105", "vmid": "106"}); err == nil {
		t.Fatal("a vmid that disagrees with the ref was accepted")
	}
	for _, bad := range []string{"105", "pve1/vm/105", "pve1/qemu/", "/qemu/105", "../qemu/105", "pve1/qemu/105?x=1"} {
		if _, err := WithRef("vm_stop", map[string]interface{}{"ref": bad}); err == nil {
			t.Errorf("ref %q was accepted", bad)
		}
	}
}

func TestGuardIdentity(t *testing.T) {
	g := newGuestStub(t)
	c := g.client()
	ctx := context.Background()
	at := func(extra map[string]interface{}) map[string]interface{} {
		p := map[string]interface{}{"node": "pve1", "type": "qemu", "vmid": "105"}
		for k, v := range extra {
			p[k] = v
		}
		return p
	}

	if err := c.GuardIdentity(ctx, "vm_stop", at(map[string]interface{}{"expected_uuid": guestUUID})); err != nil {
		t.Errorf("the right guest was refused: %v", err)
	}
	// The same UUID in the other byte order is the same machine.
	if err := c.GuardIdentity(ctx, "vm_stop", at(map[string]interface{}{"expected_uuid": "1e7f1b42-7a7b-6e3d-2c0e-9c4b5a6f7d8e"})); err != nil {
		t.Errorf("the swapped-endian form was refused: %v", err)
	}
	if err := c.GuardIdentity(ctx, "vm_stop", at(map[string]interface{}{"expected_uuid": guestUUID, "name": "web-01"})); err != nil {
		t.Errorf("the right name was refused: %v", err)
	}

	for name, p := range map[string]map[string]interface{}{
		"another identity": at(map[string]interface{}{"expected_uuid": "11111111-2222-3333-4444-555555555555"}),
		"another name":     at(map[string]interface{}{"expected_uuid": guestUUID, "name": "web-02"}),
		"a name alone":     at(map[string]interface{}{"name": "web-01"}),
		"an unusable uuid": at(map[string]interface{}{"expected_uuid": "not-a-uuid"}),
		"a container":      {"node": "pve1", "type": "lxc", "vmid": "105", "expected_uuid": guestUUID},
		"no node or vmid":  {"expected_uuid": guestUUID},
	} {
		if err := c.GuardIdentity(ctx, "vm_stop", p); err == nil {
			t.Errorf("%s: not refused", name)
		}
	}
	// No identity asked for: nothing to check.
	if err := c.GuardIdentity(ctx, "vm_stop", at(nil)); err != nil {
		t.Errorf("an unguarded action was refused: %v", err)
	}
}

func TestCheckCloneTarget(t *testing.T) {
	g := newGuestStub(t)
	c := g.client()
	ctx := context.Background()

	if done, err := c.CheckCloneTarget(ctx, "pve1", "106", "web-02", "made by run 8"); done || err != nil {
		t.Errorf("a free vmid: done=%v err=%v, want go ahead", done, err)
	}
	if done, err := c.CheckCloneTarget(ctx, "pve1", "105", "web-01", " made by run 7 "); !done || err != nil {
		t.Errorf("this clone's own guest: done=%v err=%v, want already done", done, err)
	}
	if _, err := c.CheckCloneTarget(ctx, "pve1", "105", "web-01", "made by run 8"); err == nil || !strings.Contains(err.Error(), "without this clone's ownership marker") {
		t.Errorf("another run's guest: err=%v, want a refusal", err)
	}
	if _, err := c.CheckCloneTarget(ctx, "pve1", "105", "web-01", ""); err == nil || !strings.Contains(err.Error(), "no ownership marker") {
		t.Errorf("no marker: err=%v, want a refusal", err)
	}
}

func TestAwaitPowerWaitsForTheState(t *testing.T) {
	g := newGuestStub(t)
	g.power = []string{"running", "running", "stopped"}
	if st, ok := g.client().AwaitPower(context.Background(), "pve1", "qemu", "105", "stopped"); !ok || st != "stopped" {
		t.Errorf("AwaitPower = %q %v, want stopped", st, ok)
	}
	g2 := newGuestStub(t)
	if st, ok := g2.client().AwaitPower(context.Background(), "pve1", "qemu", "105", "stopped"); ok || st != "running" {
		t.Errorf("AwaitPower = %q %v, want it to give up still running", st, ok)
	}
	if g2.powerRead != SettlePolls {
		t.Errorf("read %d times, want SettlePolls = %d", g2.powerRead, SettlePolls)
	}
}

func TestAwaitAbsent(t *testing.T) {
	g := newGuestStub(t)
	g.goneAfter = 2
	c := g.client()
	if found, err := c.FindGuest(context.Background(), "105"); err != nil || found == nil || found.Node != "pve1" {
		t.Fatalf("FindGuest = %+v, %v", found, err)
	}
	if gone, err := c.AwaitAbsent(context.Background(), "105"); !gone || err != nil {
		t.Errorf("AwaitAbsent = %v, %v, want gone", gone, err)
	}
	g2 := newGuestStub(t)
	if gone, _ := g2.client().AwaitAbsent(context.Background(), "105"); gone {
		t.Error("a guest still listed was reported absent")
	}
}

func TestGuestPowerFoldsAPausedVM(t *testing.T) {
	if s := (GuestPower{Status: "running", QMPStatus: "paused"}).State(); s != "paused" {
		t.Errorf("State = %q, want paused", s)
	}
	if s := (GuestPower{Status: "running"}).State(); s != "running" {
		t.Errorf("State = %q, want running", s)
	}
}
