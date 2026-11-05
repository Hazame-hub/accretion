package redfish

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/hazame-hub/accretion/provider"
)

// fakeBMC is a small Redfish service in the older iDRAC/iLO layout:
// VirtualMedia under the Manager, not the System. It records every write.
type fakeBMC struct {
	mu       sync.Mutex
	serial   string
	uuid     string
	power    string
	boot     map[string]any
	media    map[string]map[string]any
	resets   []string
	writes   []string
	allowed  []string
	resetsOK []string
	user     string
	pass     string
	// dropWrites closes the connection on a write without answering.
	dropWrites bool
}

func newFakeBMC() *fakeBMC {
	return &fakeBMC{
		serial: "CZJ1234ABC", uuid: "4c4c4544-0042-3510-8051-b4c04f4e4d32", power: "Off",
		boot:     map[string]any{"BootSourceOverrideEnabled": "Disabled", "BootSourceOverrideTarget": "None"},
		allowed:  []string{"None", "Pxe", "Hdd", "UsbCd", "BiosSetup"},
		resetsOK: []string{"On", "ForceOff", "GracefulShutdown", "ForceRestart"},
		media: map[string]map[string]any{
			"CD":            {"MediaTypes": []string{"CD", "DVD"}, "Inserted": false, "Image": ""},
			"RemovableDisk": {"MediaTypes": []string{"USBStick", "Floppy"}, "Inserted": false, "Image": ""},
		},
		user: "root", pass: "calvin",
	}
}

func (f *fakeBMC) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if u, p, ok := r.BasicAuth(); !ok || u != f.user || p != f.pass {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad credential"}}`))
		return
	}
	if r.Method != http.MethodGet {
		f.writes = append(f.writes, r.Method+" "+r.URL.Path)
		if f.dropWrites {
			hj, _ := w.(http.Hijacker)
			conn, _, _ := hj.Hijack()
			_ = conn.Close()
			return
		}
	}
	sys := "/redfish/v1/Systems/System.Embedded.1"
	mgr := "/redfish/v1/Managers/iDRAC.Embedded.1"
	js := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch p := r.URL.Path; {
	case p == "/redfish/v1/Systems":
		js(map[string]any{"Members": []any{map[string]string{"@odata.id": sys}}})
	case p == sys && r.Method == http.MethodGet:
		boot := map[string]any{"BootSourceOverrideTarget@Redfish.AllowableValues": f.allowed}
		for k, v := range f.boot {
			boot[k] = v
		}
		js(map[string]any{
			"@odata.id": sys, "Id": "System.Embedded.1", "UUID": strings.ToUpper(f.uuid), "SerialNumber": f.serial + " ",
			"Manufacturer": "Dell Inc.", "Model": "PowerEdge R640", "PowerState": f.power, "Boot": boot,
			"Links":   map[string]any{"ManagedBy": []any{map[string]string{"@odata.id": mgr}}},
			"Actions": map[string]any{"#ComputerSystem.Reset": map[string]any{"target": sys + "/Actions/ComputerSystem.Reset", "ResetType@Redfish.AllowableValues": f.resetsOK}},
		})
	case p == sys && r.Method == http.MethodPatch:
		for k, v := range body["Boot"].(map[string]any) {
			f.boot[k] = v
		}
		w.WriteHeader(http.StatusNoContent)
	case p == sys+"/Actions/ComputerSystem.Reset":
		t := body["ResetType"].(string)
		f.resets = append(f.resets, t)
		if t == "On" || t == "ForceOn" {
			f.power = "On"
		} else {
			f.power = "Off"
		}
		w.WriteHeader(http.StatusNoContent)
	case p == mgr:
		js(map[string]any{"VirtualMedia": map[string]string{"@odata.id": mgr + "/VirtualMedia"}})
	case p == mgr+"/VirtualMedia":
		js(map[string]any{"Members": []any{
			map[string]string{"@odata.id": mgr + "/VirtualMedia/CD"},
			map[string]string{"@odata.id": mgr + "/VirtualMedia/RemovableDisk"},
		}})
	case strings.HasPrefix(p, mgr+"/VirtualMedia/"):
		rest := strings.TrimPrefix(p, mgr+"/VirtualMedia/")
		id, action, _ := strings.Cut(rest, "/Actions/")
		m, ok := f.media[id]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		switch action {
		case "":
			base := mgr + "/VirtualMedia/" + id
			js(map[string]any{"@odata.id": base, "Id": id, "MediaTypes": m["MediaTypes"], "Inserted": m["Inserted"], "Image": m["Image"],
				"Actions": map[string]any{
					"#VirtualMedia.InsertMedia": map[string]string{"target": base + "/Actions/VirtualMedia.InsertMedia"},
					"#VirtualMedia.EjectMedia":  map[string]string{"target": base + "/Actions/VirtualMedia.EjectMedia"},
				}})
		case "VirtualMedia.InsertMedia":
			m["Inserted"], m["Image"] = true, body["Image"]
			w.WriteHeader(http.StatusNoContent)
		case "VirtualMedia.EjectMedia":
			m["Inserted"], m["Image"] = false, ""
			w.WriteHeader(http.StatusNoContent)
		}
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *fakeBMC) writeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.writes)
}

func start(t *testing.T, f *fakeBMC) (*Client, string) {
	t.Helper()
	srv := httptest.NewTLSServer(f)
	t.Cleanup(srv.Close)
	c, err := New(Config{Endpoint: srv.URL, Username: "root", Password: "calvin", HTTPClient: srv.Client()})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := c.Systems(context.Background())
	if err != nil || len(paths) != 1 {
		t.Fatalf("systems: %v %v", paths, err)
	}
	return c, paths[0]
}

var good = Guard{Serial: "CZJ1234ABC"}

const iso = "https://media.lan/rhel-9.6-x86_64-dvd.iso"

func TestSystemReadsTheManagerLayout(t *testing.T) {
	c, path := start(t, newFakeBMC())
	s, err := c.System(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	if s.Serial != "CZJ1234ABC" || s.UUID != "4c4c4544-0042-3510-8051-b4c04f4e4d32" || s.Power != "Off" {
		t.Fatalf("identity: %+v", s)
	}
	if len(s.Media) != 2 || !s.Media[0].Accepts("dvd") || !s.Media[1].Accepts("Floppy") || s.Media[1].Accepts("CD") {
		t.Fatalf("media: %+v", s.Media)
	}
}

func TestGuardRefusesAnotherServer(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name string
		g    Guard
		says string
	}{
		{"nothing declared", Guard{}, "no serial number or UUID declared"},
		{"other serial", Guard{Serial: "XYZ789"}, `"CZJ1234ABC"`},
		{"placeholder serial", Guard{Serial: "To Be Filled By O.E.M."}, "placeholder"},
		{"other uuid", Guard{UUID: "11111111-2222-3333-4444-555555555555"}, "reports UUID"},
		{"serial right, uuid wrong", Guard{Serial: "CZJ1234ABC", UUID: "11111111-2222-3333-4444-555555555555"}, "reports UUID"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFakeBMC()
			c, path := start(t, f)
			verbs := map[string]func() error{
				"insert": func() error { _, err := c.InsertMedia(ctx, path, tc.g, "CD", iso); return err },
				"eject":  func() error { _, err := c.EjectMedia(ctx, path, tc.g, "CD", iso); return err },
				"boot":   func() error { _, err := c.BootOnce(ctx, path, tc.g, "Cd", "UsbCd"); return err },
				"on":     func() error { _, err := c.PowerOn(ctx, path, tc.g); return err },
				"off":    func() error { _, err := c.PowerOff(ctx, path, tc.g, true); return err },
			}
			for name, v := range verbs {
				err := v()
				var re *provider.RefusedError
				if !errors.As(err, &re) || !strings.Contains(err.Error(), tc.says) {
					t.Errorf("%s: %v; want a refusal saying %q", name, err, tc.says)
				}
			}
			if n := f.writeCount(); n != 0 {
				t.Fatalf("%d writes reached the BMC; want none", n)
			}
		})
	}
}

func TestGuardAcceptsTheSwappedUUID(t *testing.T) {
	c, path := start(t, newFakeBMC())
	s, _ := c.System(context.Background(), path)
	if err := (Guard{UUID: "44454C4C-4200-1035-8051-B4C04F4E4D32"}).Check(s); err != nil {
		t.Fatal(err)
	}
}

func TestInstallSequenceIsIdempotent(t *testing.T) {
	ctx := context.Background()
	f := newFakeBMC()
	c, path := start(t, f)
	answers := "https://media.lan/accrete/bundle-3f2a.img"
	steps := []struct {
		name string
		do   func() (Result, error)
	}{
		{"insert installer", func() (Result, error) { return c.InsertMedia(ctx, path, good, "CD", iso) }},
		{"insert answers", func() (Result, error) { return c.InsertMedia(ctx, path, good, "RemovableDisk", answers) }},
		{"boot once", func() (Result, error) { return c.BootOnce(ctx, path, good, "Cd", "UsbCd") }},
		{"power on", func() (Result, error) { return c.PowerOn(ctx, path, good) }},
	}
	for _, s := range steps {
		r, err := s.do()
		if err != nil || !r.Applied {
			t.Fatalf("%s: %+v %v", s.name, r, err)
		}
		r, err = s.do()
		if err != nil || r.Applied {
			t.Fatalf("%s again: %+v %v; want success with nothing done", s.name, r, err)
		}
	}
	if f.boot["BootSourceOverrideTarget"] != "UsbCd" || f.boot["BootSourceOverrideEnabled"] != "Once" {
		t.Fatalf("boot override %v; want Once on the CD target the BMC lists (UsbCd)", f.boot)
	}
	if strings.Join(f.resets, ",") != "On" {
		t.Fatalf("resets %v", f.resets)
	}
	for _, slot := range []struct{ id, img string }{{"CD", iso}, {"RemovableDisk", answers}} {
		if r, err := c.EjectMedia(ctx, path, good, slot.id, slot.img); err != nil || !r.Applied {
			t.Fatalf("eject %s: %+v %v", slot.id, r, err)
		}
		if r, err := c.EjectMedia(ctx, path, good, slot.id, slot.img); err != nil || r.Applied {
			t.Fatalf("eject %s again: %+v %v", slot.id, r, err)
		}
	}
}

func TestMediaThatIsNotOursIsLeftAlone(t *testing.T) {
	ctx := context.Background()
	f := newFakeBMC()
	f.media["CD"]["Inserted"], f.media["CD"]["Image"] = true, "https://other/firmware.iso"
	c, path := start(t, f)
	for name, err := range map[string]error{
		"insert": func() error { _, err := c.InsertMedia(ctx, path, good, "CD", iso); return err }(),
		"eject":  func() error { _, err := c.EjectMedia(ctx, path, good, "CD", iso); return err }(),
	} {
		var re *provider.RefusedError
		if !errors.As(err, &re) || !strings.Contains(err.Error(), "firmware.iso") {
			t.Errorf("%s: %v", name, err)
		}
	}
	if f.writeCount() != 0 {
		t.Fatal("a write reached the BMC")
	}
}

func TestOnlyAllowedValuesAreSent(t *testing.T) {
	ctx := context.Background()
	f := newFakeBMC()
	f.allowed = []string{"Pxe", "Hdd"}
	f.resetsOK = []string{"ForceRestart"}
	c, path := start(t, f)
	_, err := c.BootOnce(ctx, path, good, "Cd", "UsbCd")
	if !strings.Contains(fmt.Sprint(err), "Pxe, Hdd") {
		t.Errorf("boot: %v", err)
	}
	_, err = c.PowerOn(ctx, path, good)
	if !strings.Contains(fmt.Sprint(err), "ForceRestart") {
		t.Errorf("power on: %v", err)
	}
	if _, err := c.InsertMedia(ctx, path, good, "Floppy", iso); !strings.Contains(fmt.Sprint(err), "CD, RemovableDisk") {
		t.Errorf("unknown slot: %v", err)
	}
	if f.writeCount() != 0 {
		t.Fatal("a write reached the BMC")
	}
}

func TestFailureKinds(t *testing.T) {
	ctx := context.Background()
	t.Run("bad credential", func(t *testing.T) {
		f := newFakeBMC()
		srv := httptest.NewTLSServer(f)
		defer srv.Close()
		c, _ := New(Config{Endpoint: srv.URL, Username: "root", Password: "wrong-secret", HTTPClient: srv.Client()})
		_, err := c.Systems(ctx)
		if !errors.Is(err, provider.ErrUnauthorized) {
			t.Fatalf("%v; want ErrUnauthorized", err)
		}
		if strings.Contains(err.Error(), "wrong-secret") {
			t.Fatal("the password is in the error")
		}
	})
	t.Run("answer lost", func(t *testing.T) {
		f := newFakeBMC()
		c, path := start(t, f)
		f.mu.Lock()
		f.dropWrites = true
		f.mu.Unlock()
		_, err := c.PowerOn(ctx, path, good)
		var ou *provider.OutcomeUnknownError
		if !errors.As(err, &ou) {
			t.Fatalf("%v; want OutcomeUnknownError", err)
		}
	})
	t.Run("refused by the BMC", func(t *testing.T) {
		f := newFakeBMC()
		c, path := start(t, f)
		// Insert into a slot the BMC no longer has: it answers 404.
		s, _ := c.System(ctx, path)
		delete(f.media, "CD")
		err := c.post(ctx, "insert media", s.Media[0].insertPath, map[string]any{"Image": iso})
		var he *HTTPError
		if !errors.As(err, &he) || he.Status != 404 {
			t.Fatalf("%v", err)
		}
	})
}

func TestNewRefusesUnsafeEndpoints(t *testing.T) {
	for _, tc := range []struct {
		cfg  Config
		says string
	}{
		{Config{Endpoint: "http://bmc.lan"}, "plain http"},
		{Config{Endpoint: "ftp://bmc.lan"}, "https"},
		{Config{Endpoint: "https://root:calvin@bmc.lan"}, "not in the endpoint"},
		{Config{Endpoint: "bmc.lan"}, "not a URL"},
	} {
		_, err := New(tc.cfg)
		if err == nil || !strings.Contains(err.Error(), tc.says) {
			t.Errorf("%s: %v; want %q", tc.cfg.Endpoint, err, tc.says)
		}
		if err != nil && strings.Contains(err.Error(), "calvin") {
			t.Errorf("%s: the password is in the error", tc.cfg.Endpoint)
		}
	}
	c, err := New(Config{Endpoint: "http://127.0.0.1:8000", AllowHTTP: true, Password: "calvin"})
	if err != nil || !c.Insecure() {
		t.Fatalf("emulator endpoint: %v insecure=%v", err, c != nil && c.Insecure())
	}
	if strings.Contains(c.String(), "calvin") || strings.Contains(Config{Password: "calvin"}.String(), "calvin") {
		t.Fatal("String shows the password")
	}
	if c, _ := New(Config{Endpoint: "https://bmc.lan"}); c.Insecure() {
		t.Fatal("verified https reported insecure")
	}
}

func TestBMCCannotRedirectTheCredential(t *testing.T) {
	c, _ := New(Config{Endpoint: "https://bmc.lan", Password: "calvin"})
	if _, err := c.resolve("https://evil.example/redfish/v1"); err == nil {
		t.Fatal("followed a path to another host")
	}
	if _, err := c.resolve("//evil.example/x"); err == nil {
		t.Fatal("followed a scheme-relative path to another host")
	}
}
