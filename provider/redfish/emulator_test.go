package redfish

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

// TestAgainstEmulator drives a real Redfish service: the DMTF-modelled
// sushy-tools emulator with its fake driver. It runs only when
// ACCRETE_REDFISH_EMULATOR names one, for example:
//
//	pip install sushy-tools
//	sushy-emulator --fake -i 127.0.0.1 -p 8111 &
//	ACCRETE_REDFISH_EMULATOR=http://127.0.0.1:8111 go test ./provider/redfish -run Emulator -v
//
// The emulator fetches inserted images, as a BMC does, so the test serves
// two small files for it. The fake driver applies a power change 1 to 11 s
// after it is asked, and reports its boot override as Continuous whatever
// was set; the test allows for both.
func TestAgainstEmulator(t *testing.T) {
	endpoint := os.Getenv("ACCRETE_REDFISH_EMULATOR")
	if endpoint == "" {
		t.Skip("ACCRETE_REDFISH_EMULATOR is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	media := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("not really an ISO\n"))
	}))
	defer media.Close()
	installer, answers := media.URL+"/rhel-9.6-x86_64-dvd.iso", media.URL+"/accrete-bundle.img"

	c, err := New(Config{Endpoint: endpoint, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := c.Systems(ctx)
	if err != nil || len(paths) == 0 {
		t.Fatalf("systems: %v %v", paths, err)
	}
	s, err := c.System(ctx, paths[0])
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("system %s uuid=%s serial=%q power=%s boot targets=%v resets=%v", s.Path, s.UUID, s.Serial, s.Power, s.Boot.AllowedTargets, s.ResetTypes)
	// The fake driver reports no serial; guard on the UUID.
	g := Guard{UUID: s.UUID}

	if _, err := c.PowerOn(ctx, s.Path, Guard{UUID: "11111111-2222-3333-4444-555555555555"}); err == nil {
		t.Fatal("a wrong UUID was not refused")
	}

	var cd, second MediaSlot
	for _, m := range s.Media {
		switch {
		case cd.ID == "" && m.Accepts("CD", "DVD"):
			cd = m
		case second.ID == "" && m.Accepts("Floppy", "USBStick"):
			second = m
		}
	}
	if cd.ID == "" || second.ID == "" {
		t.Fatalf("want a CD slot and a second slot, got %+v", s.Media)
	}
	// Leave the emulator as found, whatever happens below.
	defer func() {
		_, _ = c.EjectMedia(context.Background(), s.Path, g, cd.ID, installer)
		_, _ = c.EjectMedia(context.Background(), s.Path, g, second.ID, answers)
	}()

	if r, err := c.PowerOff(ctx, s.Path, g, true); err != nil {
		t.Fatal(err)
	} else if r.Applied {
		waitPower(ctx, t, c, s.Path, "Off")
	}
	for _, step := range []struct {
		slot MediaSlot
		img  string
	}{{cd, installer}, {second, answers}} {
		if r, err := c.InsertMedia(ctx, s.Path, g, step.slot.ID, step.img); err != nil || !r.Applied {
			t.Fatalf("insert %s: %+v %v", step.slot.ID, r, err)
		}
		if r, err := c.InsertMedia(ctx, s.Path, g, step.slot.ID, step.img); err != nil || r.Applied {
			t.Fatalf("insert %s again: %+v %v", step.slot.ID, r, err)
		}
	}
	if _, err := c.BootOnce(ctx, s.Path, g, "Cd", "UsbCd"); err != nil {
		t.Fatal(err)
	}
	if r, err := c.PowerOn(ctx, s.Path, g); err != nil || !r.Applied {
		t.Fatalf("power on: %+v %v", r, err)
	}
	waitPower(ctx, t, c, s.Path, "On")

	after, err := c.System(ctx, s.Path)
	if err != nil {
		t.Fatal(err)
	}
	if after.Boot.Target != "Cd" {
		t.Fatalf("boot target %q, want Cd", after.Boot.Target)
	}
	for _, m := range after.Media {
		if (m.ID == cd.ID && m.Image != installer) || (m.ID == second.ID && m.Image != answers) || !m.Inserted {
			t.Fatalf("slot %s: inserted=%v image=%q", m.ID, m.Inserted, m.Image)
		}
	}
	// Eject is guarded by what was inserted: the installer is not the
	// answers slot's media.
	if _, err := c.EjectMedia(ctx, s.Path, g, second.ID, installer); err == nil {
		t.Fatal("ejected media accrete did not put in that slot")
	}
	for _, step := range []struct {
		slot MediaSlot
		img  string
	}{{cd, installer}, {second, answers}} {
		if r, err := c.EjectMedia(ctx, s.Path, g, step.slot.ID, step.img); err != nil || !r.Applied {
			t.Fatalf("eject %s: %+v %v", step.slot.ID, r, err)
		}
	}
	if r, err := c.PowerOff(ctx, s.Path, g, true); err != nil || !r.Applied {
		t.Fatalf("power off: %+v %v", r, err)
	}
	waitPower(ctx, t, c, s.Path, "Off")
}

func waitPower(ctx context.Context, t *testing.T, c *Client, path, want string) {
	t.Helper()
	for {
		s, err := c.System(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		if s.Power == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("power stayed %s, want %s", s.Power, want)
		case <-time.After(500 * time.Millisecond):
		}
	}
}
