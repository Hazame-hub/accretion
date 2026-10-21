package proxmox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/hazame-hub/accretion/plan"
)

// sdnStub is a cluster with one applied vnet with a DHCP subnet, one vnet
// only pending, and one node where the pending vnet is not up. Without
// audit, the token cannot see the SDN.
func sdnStub(t *testing.T, audit bool) (*Client, *bool) {
	t.Helper()
	listed := false
	mux := http.NewServeMux()
	data := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux.HandleFunc("/api2/json/access/permissions", func(w http.ResponseWriter, r *http.Request) {
		if audit {
			data(w, map[string]any{"/sdn": map[string]any{"SDN.Audit": 1}})
			return
		}
		data(w, map[string]any{"/sdn": map[string]any{"VM.Audit": 1}})
	})
	mux.HandleFunc("/api2/json/cluster/sdn/zones", func(w http.ResponseWriter, r *http.Request) {
		listed = true
		if r.URL.Query().Get("pending") != "1" {
			t.Errorf("zones read without pending=1")
		}
		data(w, []map[string]any{{"zone": "lab", "type": "simple", "dhcp": "dnsmasq"}})
	})
	mux.HandleFunc("/api2/json/cluster/sdn/vnets", func(w http.ResponseWriter, r *http.Request) {
		data(w, []map[string]any{
			{"vnet": "vk8s", "zone": "lab"},
			{"vnet": "vnew", "state": "new", "pending": map[string]any{"zone": "lab"}},
		})
	})
	mux.HandleFunc("/api2/json/cluster/sdn/vnets/vk8s/subnets", func(w http.ResponseWriter, r *http.Request) {
		data(w, []map[string]any{{"cidr": "10.10.1.0/24", "gateway": "10.10.1.1", "snat": 1,
			"dhcp-range": []map[string]any{{"start-address": "10.10.1.100", "end-address": "10.10.1.200"}}}})
	})
	mux.HandleFunc("/api2/json/cluster/sdn/vnets/vnew/subnets", func(w http.ResponseWriter, r *http.Request) {
		data(w, []map[string]any{})
	})
	mux.HandleFunc("/api2/json/nodes", func(w http.ResponseWriter, r *http.Request) {
		data(w, []map[string]any{{"node": "pve", "status": "online"}})
	})
	mux.HandleFunc("/api2/json/nodes/pve/sdn/zones", func(w http.ResponseWriter, r *http.Request) {
		data(w, []map[string]any{{"zone": "lab", "status": "available"}})
	})
	mux.HandleFunc("/api2/json/nodes/pve/sdn/zones/lab/content", func(w http.ResponseWriter, r *http.Request) {
		data(w, []map[string]any{
			{"vnet": "vk8s", "status": "available"},
			{"vnet": "vnew", "status": "error", "statusmsg": "missing bridge"},
		})
	})
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "PVEAPIToken=test", HTTP: srv.Client(), TaskPoll: time.Millisecond}, &listed
}

func TestSDNReadsAppliedAndPendingObjects(t *testing.T) {
	c, _ := sdnStub(t, true)
	st, err := c.SDN(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := SDNState{
		Zones: []SDNZone{{Zone: "lab", Type: "simple", DHCP: "dnsmasq"}},
		Vnets: []SDNVnet{
			{Vnet: "vk8s", Zone: "lab", NotUp: []string{}, Subnets: []SDNSubnet{
				{CIDR: "10.10.1.0/24", Gateway: "10.10.1.1", SNAT: true, DHCPRange: "10.10.1.100-10.10.1.200"},
			}},
			{Vnet: "vnew", Zone: "lab", Pending: true, NotUp: []string{"pve: error (missing bridge)"}, Subnets: []SDNSubnet{}},
		},
		Pending: []string{"vnet vnew (new)"},
	}
	if !reflect.DeepEqual(st, want) {
		t.Fatalf("SDN =\n%+v\nwant\n%+v", st, want)
	}

	obs := st.Observed()
	if obs.ZoneDHCP["lab"] != "dnsmasq" || len(obs.Pending) != 1 {
		t.Errorf("observed = %+v", obs)
	}
	if v := obs.Vnets["vk8s"]; v.Zone != "lab" || len(v.Subnets) != 1 || v.Subnets[0] != (plan.ObservedSubnet{
		CIDR: "10.10.1.0/24", Gateway: "10.10.1.1", SNAT: true, DHCPRange: "10.10.1.100-10.10.1.200",
	}) {
		t.Errorf("observed vk8s = %+v", v)
	}
	if v := obs.Vnets["vnew"]; !v.Pending || len(v.NotUp) != 1 {
		t.Errorf("observed vnew = %+v", v)
	}
}

// A token without SDN.Audit gets empty lists from Proxmox. SDN must say it
// could not look, never report an empty SDN.
func TestSDNRefusesWhenItCannotSee(t *testing.T) {
	c, listed := sdnStub(t, false)
	if _, err := c.SDN(context.Background()); err == nil || !strings.Contains(err.Error(), "SDN.Audit") {
		t.Fatalf("err = %v", err)
	}
	if *listed {
		t.Fatal("listed zones it was not allowed to see")
	}
}

func TestCapacityCountsOnlyRunningGuests(t *testing.T) {
	mux := http.NewServeMux()
	data := func(w http.ResponseWriter, v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mux.HandleFunc("/api2/json/nodes", func(w http.ResponseWriter, r *http.Request) {
		data(w, []map[string]any{
			{"node": "pve2", "status": "offline"},
			{"node": "pve1", "status": "online", "maxmem": 32 << 30, "maxcpu": 16},
		})
	})
	mux.HandleFunc("/api2/json/nodes/pve1/qemu", func(w http.ResponseWriter, r *http.Request) {
		data(w, []map[string]any{
			{"status": "running", "maxmem": 16 << 30, "cpus": 4},
			{"status": "stopped", "maxmem": 64 << 30, "cpus": 8},
			{"status": "running", "maxmem": 8 << 30, "cpus": 2, "template": 1},
		})
	})
	mux.HandleFunc("/api2/json/nodes/pve1/lxc", func(w http.ResponseWriter, r *http.Request) {
		data(w, []map[string]any{{"status": "running", "maxmem": 1 << 30, "cpus": 1}})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()
	c := &Client{BaseURL: srv.URL, Token: "PVEAPIToken=test", HTTP: srv.Client()}

	got, err := c.Capacity(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := Capacity{
		Nodes: []NodeCapacity{
			{Node: "pve1", Status: "online", Known: true, MemoryBytes: 32 << 30, AllocatedMemoryBytes: 17 << 30, CPUs: 16, AllocatedVCPUs: 5},
			{Node: "pve2", Status: "offline", Offline: true},
		},
		MemoryBytes: 32 << 30, AllocatedMemoryBytes: 17 << 30, CPUs: 16, AllocatedVCPUs: 5, Known: true,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Capacity =\n%+v\nwant\n%+v", got, want)
	}
	obs := got.Observed()
	if obs.MemoryBytes != 32<<30 || !obs.Known || len(obs.Nodes) != 2 || !obs.Nodes[0].Online || obs.Nodes[1].Online {
		t.Errorf("observed = %+v", obs)
	}
}
