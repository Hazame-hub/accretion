package proxmox

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// rrdServer serves testdata/rrddata-qemu-hour.json for one guest and records
// the query it was asked. The fixture follows the shape Proxmox VE returns
// (keys left out for steps with no value, PVE 9's extra keys present on one
// row), with a null, a NaN string and a row with no time added to pin how
// each is read.
func rrdServer(t *testing.T) (*Client, *string) {
	t.Helper()
	body, err := os.ReadFile("testdata/rrddata-qemu-hour.json")
	if err != nil {
		t.Fatal(err)
	}
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.URL.Path + "?" + r.URL.RawQuery
		if r.URL.Path != "/api2/json/nodes/flk-vega-01/qemu/114/rrddata" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write(body)
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "PVEAPIToken=t", HTTP: srv.Client()}, &got
}

func TestGuestRRDReadsPointsOldestFirst(t *testing.T) {
	c, asked := rrdServer(t)
	pts, err := c.GuestRRD(context.Background(), "flk-vega-01", "qemu", 114, TimeframeHour, ConsolidationMax)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(*asked, "timeframe=hour") || !strings.Contains(*asked, "cf=MAX") {
		t.Errorf("query = %q, want timeframe=hour and cf=MAX", *asked)
	}
	if len(pts) != 5 {
		t.Fatalf("got %d points, want 5 (the row with no time is dropped)", len(pts))
	}
	for i := 1; i < len(pts); i++ {
		if !pts[i-1].Time.Before(pts[i].Time) {
			t.Fatalf("points not oldest first at %d: %v then %v", i, pts[i-1].Time, pts[i].Time)
		}
	}
	if want := time.Unix(1792648860, 0).UTC(); !pts[0].Time.Equal(want) {
		t.Errorf("first time = %v, want %v", pts[0].Time, want)
	}

	peak := pts[1] // 1792648920: memory at its maximum
	if peak.Mem == nil || *peak.Mem != 8589934592 || peak.MaxMem == nil || *peak.MaxMem != 8589934592 {
		t.Errorf("peak memory = %v / %v, want 8 GiB / 8 GiB", peak.Mem, peak.MaxMem)
	}
	if peak.CPU == nil || *peak.CPU != 0.88 || peak.DiskWrite == nil || *peak.DiskWrite != 1048576 {
		t.Errorf("peak cpu/diskwrite = %v / %v", peak.CPU, peak.DiskWrite)
	}
}

func TestGuestRRDKeepsMissingValuesMissing(t *testing.T) {
	c, _ := rrdServer(t)
	pts, err := c.GuestRRD(context.Background(), "flk-vega-01", "qemu", 114, TimeframeHour, ConsolidationAverage)
	if err != nil {
		t.Fatal(err)
	}
	stopped := pts[3] // keys left out: the guest was not running
	if stopped.CPU != nil || stopped.Mem != nil || stopped.NetIn != nil {
		t.Errorf("a step with no value must read nil, got cpu=%v mem=%v netin=%v", stopped.CPU, stopped.Mem, stopped.NetIn)
	}
	if stopped.MaxMem == nil {
		t.Error("maxmem is present on that step and must be kept")
	}
	odd := pts[4] // null cpu, "NaN" mem
	if odd.CPU != nil || odd.Mem != nil {
		t.Errorf("null and NaN must read nil, got cpu=%v mem=%v", odd.CPU, odd.Mem)
	}
}

func TestGuestRRDRefusesBadArgumentsBeforeCalling(t *testing.T) {
	c, asked := rrdServer(t)
	cases := []struct {
		node, kind string
		vmid       int
		tf         Timeframe
		cf         Consolidation
	}{
		{"", "qemu", 114, TimeframeHour, ConsolidationMax},
		{"flk-vega-01", "vm", 114, TimeframeHour, ConsolidationMax},
		{"flk-vega-01", "qemu", 0, TimeframeHour, ConsolidationMax},
		{"flk-vega-01", "qemu", 114, "decade", ConsolidationMax},
		{"flk-vega-01", "qemu", 114, TimeframeHour, "MIN"},
	}
	for _, tc := range cases {
		if _, err := c.GuestRRD(context.Background(), tc.node, tc.kind, tc.vmid, tc.tf, tc.cf); err == nil {
			t.Errorf("GuestRRD(%q, %q, %d, %q, %q) = nil error", tc.node, tc.kind, tc.vmid, tc.tf, tc.cf)
		}
	}
	if *asked != "" {
		t.Errorf("a refused argument must not reach the API, but it was asked %q", *asked)
	}
}

func TestGuestRRDMissingAuditNamesThePrivilege(t *testing.T) {
	c := refusingServer(t, "403 Permission check failed (/vms/114, VM.Audit)", `{"data":null}`)
	_, err := c.GuestRRD(context.Background(), "flk-vega-01", "qemu", 114, TimeframeHour, ConsolidationMax)
	path, priv, ok := MissingPrivilege(err)
	if !ok || path != "/vms/114" || priv != "VM.Audit" {
		t.Fatalf("MissingPrivilege = (%q, %q, %v) for %v", path, priv, ok, err)
	}
}
