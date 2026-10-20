package proxmox

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/providertest"
)

// clusterStub is a two-node PVE: pve1 answers everything, pve2 is down when
// downNode is set. One VM has a running guest agent, one is a template, one
// container has MACs and no DMI.
func clusterStub(t *testing.T, downNode bool) *httptest.Server {
	return clusterStubWith(t, downNode, false, nil)
}

// clusterStubWith is clusterStub with a token that cannot see the SDN when
// sdnHidden, and every path asked recorded in seen when it is not nil.
func clusterStubWith(t *testing.T, downNode, sdnHidden bool, seen *[]string) *httptest.Server {
	t.Helper()
	data := func(w http.ResponseWriter, v any) {
		_ = json.NewEncoder(w).Encode(map[string]any{"data": v})
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.Header.Get("Authorization"), "PVEAPIToken=") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api2/json")
		if seen != nil {
			*seen = append(*seen, p)
		}
		if strings.HasPrefix(p, "/nodes/pve2/") && downNode {
			w.WriteHeader(595)
			_ = json.NewEncoder(w).Encode(map[string]any{"errors": "node offline"})
			return
		}
		switch p {
		case "/nodes":
			data(w, []map[string]any{{"node": "pve1", "status": "online"}, {"node": "pve2", "status": "online"}})
		case "/nodes/pve1/qemu":
			data(w, []map[string]any{
				{"vmid": 100, "name": "web-01", "status": "running", "maxmem": 8589934592, "cpus": 4, "tags": "web;prod"},
				{"vmid": 9000, "name": "debian-12-cloud", "status": "stopped", "template": 1, "maxmem": 2147483648, "cpus": 2},
			})
		case "/nodes/pve1/lxc":
			data(w, []map[string]any{{"vmid": 200, "name": "cache-01", "status": "running", "maxmem": 1073741824, "cpus": 1}})
		case "/nodes/pve2/qemu":
			data(w, []map[string]any{{"vmid": 101, "name": "db-01", "status": "stopped", "maxmem": 4294967296, "cpus": 2}})
		case "/nodes/pve2/lxc":
			data(w, []map[string]any{})
		case "/nodes/pve1/qemu/100/config":
			data(w, map[string]any{
				"smbios1": "uuid=421b7f1e-7b7a-3d6e-2c0e-9c4b5a6f7d8e,base64=1",
				"net0":    "virtio=BC:24:11:AA:BB:01,bridge=vmbr0",
				"cores":   2, "sockets": 2, "memory": 8192, "balloon": 4096,
				"ostype": "l26",
				"scsi0":  "local-lvm:vm-100-disk-0,size=32G",
				"scsi1":  "local-lvm:vm-100-disk-1,size=512M",
				// An installer ISO: not a disk the guest has.
				"ide2": "local:iso/agent.x86_64.iso,media=cdrom,size=1203M",
			})
		case "/nodes/pve1/qemu/100/status/current":
			data(w, map[string]any{"status": "running", "maxmem": 8589934592, "balloon": 6442450944})
		case "/nodes/pve1/qemu/100/agent/get-host-name":
			data(w, map[string]any{"result": map[string]any{"host-name": "web-01.lab"}})
		case "/nodes/pve1/qemu/100/agent/network-get-interfaces":
			data(w, map[string]any{"result": []map[string]any{
				{"name": "lo", "hardware-address": "00:00:00:00:00:00", "ip-addresses": []map[string]any{{"ip-address": "127.0.0.1", "ip-address-type": "ipv4"}}},
				{"name": "ens18", "hardware-address": "bc:24:11:aa:bb:01", "ip-addresses": []map[string]any{
					{"ip-address": "10.0.0.5", "ip-address-type": "ipv4"},
					{"ip-address": "fe80::1", "ip-address-type": "ipv6"},
				}},
			}})
		case "/nodes/pve1/qemu/9000/config":
			data(w, map[string]any{"smbios1": "uuid=9a1d3c2b-4e5f-6a7b-8c9d-0e1f2a3b4c5d", "net0": "virtio=BC:24:11:AA:BB:09,bridge=vmbr0", "cores": 2, "memory": 2048, "template": 1})
		case "/nodes/pve1/qemu/9000/status/current":
			data(w, map[string]any{"status": "stopped", "maxmem": 2147483648})
		case "/nodes/pve1/lxc/200/config":
			data(w, map[string]any{"hostname": "cache-01", "net0": "name=eth0,bridge=vmbr0,hwaddr=BC:24:11:AA:BB:03,ip=dhcp", "cores": 1, "memory": 1024, "rootfs": "local-lvm:vm-200-disk-0,size=8G", "ostype": "debian"})
		case "/nodes/pve1/lxc/200/status/current":
			data(w, map[string]any{"status": "running", "maxmem": 1073741824})
		case "/nodes/pve2/qemu/101/config":
			data(w, map[string]any{"smbios1": "uuid=03000200-0400-0500-0006-000700080009", "net0": "e1000=BC:24:11:AA:BB:02,bridge=vmbr0", "cores": 2, "memory": 4096, "balloon": 0})
		case "/nodes/pve2/qemu/101/status/current":
			data(w, map[string]any{"status": "stopped", "maxmem": 4294967296})
		case "/nodes/pve1/storage":
			data(w, []map[string]any{
				{"storage": "local-lvm", "type": "lvmthin", "content": "images,rootdir", "active": 1, "enabled": 1, "total": 107374182400, "used": 32212254720, "avail": 75161927680},
				{"storage": "local", "type": "dir", "content": "iso,vztmpl,snippets", "active": 1, "enabled": 1, "total": 53687091200, "used": 10737418240, "avail": 42949672960},
			})
		case "/nodes/pve2/storage":
			data(w, []map[string]any{{"storage": "local-lvm", "type": "lvmthin", "active": 0, "enabled": 1}})
		case "/access/permissions":
			if sdnHidden {
				data(w, map[string]any{"/sdn": map[string]any{"VM.Audit": 1}})
				return
			}
			data(w, map[string]any{"/sdn": map[string]any{"SDN.Audit": 1}})
		case "/cluster/sdn/vnets":
			data(w, []map[string]any{
				{"vnet": "vk8s", "zone": "lab"},
				{"vnet": "vnew", "state": "new", "pending": map[string]any{"zone": "lab"}},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
}

func config(srv *httptest.Server) provider.Config {
	return provider.Config{Endpoint: srv.URL, Params: map[string]any{"api_token": "root@pam!tok=s3cret-token"}, VerifyTLS: false}
}

func TestProxmoxPassesTheContract(t *testing.T) {
	srv := clusterStub(t, false)
	defer srv.Close()
	providertest.Contract(t, New(), config(srv), "s3cret-token")
}

func discover(t *testing.T, srv *httptest.Server) ([]provider.Resource, error) {
	t.Helper()
	s, err := New().Connect(context.Background(), config(srv))
	if err != nil {
		t.Fatal(err)
	}
	return s.Discover(context.Background())
}

func byID(rs []provider.Resource, id string) *provider.Resource {
	for i := range rs {
		if rs[i].Ref.ID == id {
			return &rs[i]
		}
	}
	return nil
}

func TestDiscoverReadsIdentitySizingAndMemoryFromTheGuestConfig(t *testing.T) {
	srv := clusterStub(t, false)
	defer srv.Close()
	rs, err := discover(t, srv)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	if len(rs) != 11 { // 2 hosts, 2 VMs, 1 template, 1 container, 3 storages, 2 vnets
		t.Fatalf("got %d resources: %v", len(rs), refs(rs))
	}

	web := byID(rs, "pve1/qemu/100")
	if web == nil || web.Ref.Kind != provider.KindVM {
		t.Fatalf("web-01 missing or wrong kind: %+v", web)
	}
	if web.Identity.SMBIOSUUID != "421b7f1e-7b7a-3d6e-2c0e-9c4b5a6f7d8e" {
		t.Errorf("SMBIOSUUID = %q", web.Identity.SMBIOSUUID)
	}
	if !reflect.DeepEqual(web.Identity.MACs, []string{"bc:24:11:aa:bb:01"}) {
		t.Errorf("MACs = %v", web.Identity.MACs)
	}
	if web.Identity.Hostname != "web-01.lab" || !reflect.DeepEqual(web.Identity.IPs, []string{"10.0.0.5"}) {
		t.Errorf("guest agent not applied: hostname=%q ips=%v", web.Identity.Hostname, web.Identity.IPs)
	}
	if web.Facts.CPUCores != 4 {
		t.Errorf("CPUCores = %d, want sockets×cores = 4", web.Facts.CPUCores)
	}
	if web.Facts.RAMBytes != 8<<30 || web.Memory.MaxBytes != 8<<30 {
		t.Errorf("RAM = %d / max %d, want 8 GiB", web.Facts.RAMBytes, web.Memory.MaxBytes)
	}
	if !web.Memory.Balloon || web.Memory.CurrentBytes != 6<<30 {
		t.Errorf("Memory = %+v, want balloon on with current 6 GiB from status", web.Memory)
	}
	if len(web.Facts.Disks) != 2 || web.Facts.Disks[0].Total != 32<<30 || web.Facts.Disks[1].Total != 512<<20 {
		t.Errorf("Disks = %+v (a CD-ROM is not a disk)", web.Facts.Disks)
	}
	if len(web.Facts.NetworkInterfaces) != 1 || web.Facts.NetworkInterfaces[0].HardwareAddr != "bc:24:11:aa:bb:01" ||
		!reflect.DeepEqual(web.Facts.NetworkInterfaces[0].Addresses, []string{"10.0.0.5"}) {
		t.Errorf("NetworkInterfaces = %+v", web.Facts.NetworkInterfaces)
	}
	if !reflect.DeepEqual(web.Tags, []string{"web", "prod"}) || web.Parent == nil || web.Parent.ID != "pve1" {
		t.Errorf("tags/parent = %v / %v", web.Tags, web.Parent)
	}
	if !strings.Contains(string(web.Raw), `"smbios1"`) {
		t.Error("Raw does not carry the config verbatim")
	}

	if tpl := byID(rs, "pve1/qemu/9000"); tpl == nil || tpl.Ref.Kind != provider.KindTemplate {
		t.Errorf("template not classified as template: %+v", tpl)
	}

	ct := byID(rs, "pve1/lxc/200")
	if ct == nil || ct.Ref.Kind != provider.KindContainer {
		t.Fatalf("container missing: %+v", ct)
	}
	if ct.Identity.SMBIOSUUID != "" || !reflect.DeepEqual(ct.Identity.MACs, []string{"bc:24:11:aa:bb:03"}) || ct.Identity.Hostname != "cache-01" {
		t.Errorf("container identity = %+v (no DMI, MAC from hwaddr, hostname from config)", ct.Identity)
	}
	if ct.Memory.Balloon || ct.Memory.CurrentBytes != 1<<30 {
		t.Errorf("container memory = %+v, want no balloon, current = max", ct.Memory)
	}
	if len(ct.Facts.Disks) != 1 || ct.Facts.Disks[0].Total != 8<<30 {
		t.Errorf("container disks = %+v", ct.Facts.Disks)
	}

	db := byID(rs, "pve2/qemu/101")
	if db == nil || db.Identity.SMBIOSUUID != "" {
		t.Errorf("a placeholder SMBIOS UUID must be reported as none: %+v", db)
	}
	if db != nil && (db.Memory.Balloon || db.Memory.CurrentBytes != 4<<30) {
		t.Errorf("balloon=0 must read as disabled with current = max: %+v", db.Memory)
	}
}

// One node down: the other node's guests still come back, and the error
// names the node. The bridge reads slice-plus-error as an incomplete walk.
func TestDiscoverHalfSucceedsWhenANodeIsDown(t *testing.T) {
	srv := clusterStub(t, true)
	defer srv.Close()
	rs, err := discover(t, srv)
	if err == nil {
		t.Fatal("want an error naming the node that did not answer")
	}
	if !strings.Contains(err.Error(), "pve2") {
		t.Errorf("error does not name the node: %v", err)
	}
	if byID(rs, "pve1/qemu/100") == nil || byID(rs, "pve1/lxc/200") == nil {
		t.Errorf("the healthy node's guests must still be returned: %v", refs(rs))
	}
	if byID(rs, "pve2/qemu/101") != nil {
		t.Error("the down node's guests cannot have been listed")
	}
}

func TestDescribeReadsOneGuestAndRefusesABadRef(t *testing.T) {
	srv := clusterStub(t, false)
	defer srv.Close()
	s, _ := New().Connect(context.Background(), config(srv))
	facts, err := s.Describe(context.Background(), provider.Ref{Kind: provider.KindVM, ID: "pve1/qemu/100"})
	if err != nil || facts.CPUCores != 4 {
		t.Fatalf("Describe = %+v, %v", facts, err)
	}
	if _, err := s.Describe(context.Background(), provider.Ref{Kind: provider.KindVM, ID: "not-a-ref"}); err == nil {
		t.Error("a malformed ref must be refused")
	}
	if _, err := s.Describe(context.Background(), provider.Ref{Kind: provider.KindVM, ID: "pve1/qemu/999"}); err == nil {
		t.Error("an unknown vmid must be refused, not invented")
	}
}

func TestParseSizeAndKV(t *testing.T) {
	for in, want := range map[string]uint64{"32G": 32 << 30, "512M": 512 << 20, "1T": 1 << 40, "4096": 4096, "": 0, "x": 0} {
		if got := ParseSize(in); got != want {
			t.Errorf("ParseSize(%q) = %d, want %d", in, got, want)
		}
	}
	if KVField("virtio=BC:24:11:AA:BB:01,bridge=vmbr0", "bridge") != "vmbr0" {
		t.Error("KVField bridge")
	}
}

func refs(rs []provider.Resource) []string {
	out := make([]string, len(rs))
	for i, r := range rs {
		out[i] = string(r.Ref.Kind) + ":" + r.Ref.ID
	}
	return out
}

func TestDiscoverListsStoragesAndVnets(t *testing.T) {
	srv := clusterStub(t, false)
	defer srv.Close()
	rs, err := discover(t, srv)
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}
	st := byID(rs, "pve1/local-lvm")
	if st == nil || st.Ref.Kind != provider.KindStorage || st.Parent == nil || *st.Parent != (provider.Ref{Kind: provider.KindHost, ID: "pve1"}) {
		t.Fatalf("pve1/local-lvm = %+v; listing %v", st, refs(rs))
	}
	if d := st.Facts.Disks; len(d) != 1 || d[0].Total != 107374182400 || d[0].Used != 32212254720 || d[0].FsType != "lvmthin" || st.State != "running" {
		t.Errorf("pve1/local-lvm facts = %+v, state %q", d, st.State)
	}
	if byID(rs, "pve1/local") == nil {
		t.Errorf("the second storage on pve1 is missing: %v", refs(rs))
	}
	if inactive := byID(rs, "pve2/local-lvm"); inactive == nil || inactive.State != "stopped" {
		t.Errorf("pve2/local-lvm = %+v, want listed and stopped", inactive)
	}
	vk8s, vnew := byID(rs, "vk8s"), byID(rs, "vnew")
	if vk8s == nil || vk8s.Ref.Kind != provider.KindNetwork || vk8s.State != "running" {
		t.Fatalf("vk8s = %+v", vk8s)
	}
	if !strings.Contains(string(vk8s.Raw), `"zone":"lab"`) {
		t.Errorf("vk8s does not carry its zone: %s", vk8s.Raw)
	}
	if vnew == nil || vnew.State != "unknown" || !strings.Contains(string(vnew.Raw), `"zone":"lab"`) {
		t.Errorf("a pending vnet = %+v, want state unknown and its pending zone", vnew)
	}
	for _, r := range rs {
		if r.Ref.Kind == provider.KindStorage || r.Ref.Kind == provider.KindNetwork {
			if r.Ref.Kind.Importable() {
				t.Errorf("%s must not be importable", r.Ref.Kind)
			}
		}
	}

	s, _ := New().Connect(context.Background(), config(srv))
	if f, err := s.Describe(context.Background(), provider.Ref{Kind: provider.KindStorage, ID: "pve1/local"}); err != nil || len(f.Disks) != 1 || f.Disks[0].Total != 53687091200 {
		t.Errorf("Describe storage = %+v, %v", f, err)
	}
	if _, err := s.Describe(context.Background(), provider.Ref{Kind: provider.KindNetwork, ID: "vk8s"}); err != nil {
		t.Errorf("Describe vnet: %v", err)
	}
	for _, bad := range []provider.Ref{{Kind: provider.KindStorage, ID: "pve1/nope"}, {Kind: provider.KindStorage, ID: "nostorage"}, {Kind: provider.KindNetwork, ID: "vnope"}} {
		if _, err := s.Describe(context.Background(), bad); err == nil {
			t.Errorf("Describe %v: want an error", bad)
		}
	}
}

// A token without SDN.Audit is shown an empty SDN. The listing must say it
// could not look, and keep everything else it found.
func TestDiscoverReportsAnSDNItCannotSee(t *testing.T) {
	var seen []string
	srv := clusterStubWith(t, false, true, &seen)
	defer srv.Close()
	rs, err := discover(t, srv)
	if err == nil || !strings.Contains(err.Error(), "SDN.Audit") {
		t.Fatalf("err = %v, want one naming SDN.Audit", err)
	}
	if byID(rs, "pve1/qemu/100") == nil || byID(rs, "pve1/local-lvm") == nil {
		t.Errorf("guests and storages must still be listed: %v", refs(rs))
	}
	for _, p := range seen {
		if strings.HasPrefix(p, "/cluster/sdn") {
			t.Errorf("listed %s, which the token may not see", p)
		}
	}
}

// The guest session is the listing an embedder had before storages and vnets
// were discovered: it neither returns them nor asks for them.
func TestGuestSessionNeverAsksForStoragesOrTheSDN(t *testing.T) {
	var seen []string
	srv := clusterStubWith(t, false, false, &seen)
	defer srv.Close()
	c, err := NewClient(srv.URL, "root@pam!tok=s3cret-token", false)
	if err != nil {
		t.Fatal(err)
	}
	s := NewGuestSession(c)
	rs, err := s.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range rs {
		if r.Ref.Kind == provider.KindStorage || r.Ref.Kind == provider.KindNetwork {
			t.Errorf("guest session listed %s %s", r.Ref.Kind, r.Ref.ID)
		}
	}
	for _, p := range seen {
		if strings.HasSuffix(p, "/storage") || strings.HasPrefix(p, "/cluster/sdn") || strings.HasPrefix(p, "/access/") {
			t.Errorf("guest session asked for %s", p)
		}
	}
	caps, _ := s.Capabilities(context.Background())
	if len(caps.Kinds) != 4 {
		t.Errorf("guest session kinds = %v", caps.Kinds)
	}
	if _, err := s.Describe(context.Background(), provider.Ref{Kind: provider.KindStorage, ID: "pve1/local"}); err == nil {
		t.Error("guest session described a storage")
	}
	full, _ := NewSession(c).Capabilities(context.Background())
	if len(full.Kinds) != 6 {
		t.Errorf("full session kinds = %v", full.Kinds)
	}
}
