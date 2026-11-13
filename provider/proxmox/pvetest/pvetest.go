// Package pvetest is an in-memory Proxmox VE API for tests: one node, pve1,
// that answers the reads Accretion's discovery, capacity and SDN make and the
// writes its Writer sends, and remembers what those writes changed. A clone
// adds a guest with the template's config and, as Proxmox does, a fresh
// SMBIOS UUID of its own; for its first few reads the new guest is still
// being cloned (its config is only "lock: clone", and the listing does not
// name it yet). A config POST sets keys, a power call changes the status, a
// delete removes the guest, an upload puts a file on a storage. Every
// mutation answers with a task that has already stopped well.
//
// It is not a Proxmox: it answers what Accretion asks and 501s the rest, so
// a test that strays off the map fails loudly instead of passing against an
// invented answer. Every request is recorded, with its method, so a test can
// assert what was (and was not) sent.
package pvetest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Token is the token the server accepts, and Secret its secret half, which a
// test asserts never leaks.
const (
	Secret = "pvetest-s3cret"
	Token  = "accrete@pve!test=" + Secret
)

// TemplateUUID is the SMBIOS UUID the shipped template carries. A clone does
// not inherit it: it gets CloneUUID(vmid), as Proxmox gives every clone a
// random one.
const TemplateUUID = "0f0e0d0c-0b0a-4908-8706-050403020100"

// CloneUUID is the SMBIOS UUID a clone at vmid is born with.
func CloneUUID(vmid int) string { return fmt.Sprintf("c10e0000-0000-4000-8000-%012d", vmid) }

// cloningReads is how many reads of a new guest (its config, or a listing)
// see it still being cloned.
const cloningReads = 3

// Guest is one guest of the cluster.
type Guest struct {
	VMID     int
	Kind     string // qemu | lxc
	Status   string // running | stopped | paused
	Template bool
	Config   map[string]any
	// Pending holds config changes the running guest takes at its next
	// start: GET config shows them applied, status/current does not.
	Pending map[string]any
	// Pool is the resource pool the clone request put the guest in.
	Pool string
	// cloning counts the reads that still see the guest being cloned.
	cloning int
	// installing counts the status reads left before a simulated install
	// powers the guest off; -1 is an install that never ends.
	installing int
	// Installed is set once a simulated install finished: the disk boots,
	// and the guest agent answers with InstalledHostname while it runs.
	Installed         bool
	InstalledHostname string
	// AgentHostname, when set, is what the guest agent of a running guest
	// answers, installed or not.
	AgentHostname string
	// FromImage is the disk image the guest's disk was copied from, "" for
	// none. Such a guest has no guest agent (Fedora CoreOS ships none); its
	// first start with a seed whose config ends the first boot with a
	// power-off runs a simulated first boot, and Installed marks it done.
	FromImage string
}

// applyPending is a start: the guest takes its pending config.
func (g *Guest) applyPending() {
	for k, v := range g.Pending {
		g.Config[k] = v
	}
	g.Pending = nil
}

// seen is one read of the guest; it reports whether the clone is still
// running for this read.
func (g *Guest) seen() bool {
	if g.cloning > 0 {
		g.cloning--
		return true
	}
	return false
}

// Server is the cluster.
type Server struct {
	*httptest.Server
	mu     sync.Mutex
	Guests map[int]*Guest
	// Files are the volumes on each storage, by volid ("local:iso/x.iso").
	Files map[string][]byte
	// Requests is every request, as "METHOD /path?query".
	Requests []string
	// cutNext, when it matches "METHOD /path" of a request, closes the
	// connection before the request is carried out: the caller cannot tell
	// whether it was.
	cutNext *regexp.Regexp
	// dropNext, when it matches "METHOD /path" of a request, closes the
	// connection after the request is carried out and before the answer is
	// written: the change happens, the caller never hears of it.
	dropNext *regexp.Regexp
	// delayNext, when it matches "METHOD /path" of a request, carries the
	// request out and holds its answer back for delay: a late answer.
	delayNext *regexp.Regexp
	delay     time.Duration
	// failing, while failTimes > 0, answers matching requests 500 without
	// carrying them out.
	failing   *regexp.Regexp
	failTimes int
	failCode  int
	// refuseToken answers every request 401.
	refuseToken bool
	// Privileges, when set, is what the token may do on each ACL path, as
	// GET /access/permissions?path= answers it. Nil keeps the default answer
	// (SDN.Audit on /sdn).
	Privileges map[string][]string
	// ImageGiB is the virtual size of a disk copied from an image, by
	// volid; 10 when unset.
	ImageGiB map[string]int
	// NoImportContent lists the storage local without the import content,
	// as a Proxmox VE whose operator never enabled it.
	NoImportContent bool
	// BrokenFirstBoot makes every simulated first boot hang instead of
	// powering off, as a machine whose Ignition config failed does.
	BrokenFirstBoot bool
	// InstallReads is how many status reads a simulated install runs for
	// before it powers the guest off: started with an empty disk, an ISO on
	// ide2 and an OEMDRV disc holding ks.cfg on ide3, a guest installs the
	// hostname its kickstart names. 0 is 2 reads; -1 never ends.
	InstallReads int
	// Tasks is the node's task log, oldest first, as GET /nodes/pve1/tasks
	// lists it. The server's own mutations add nothing to it: a test writes
	// the entries a change made by somebody would have left.
	Tasks []Task
	// Vnets are the cluster's applied SDN vnets, by name, each with its
	// zone, as GET /cluster/sdn/vnets lists them.
	Vnets map[string]string
	// Zones and Networks are the SDN the network writes made (sdn.go);
	// Vnets are listed beside them as applied and up.
	Zones    map[string]*Zone
	Networks map[string]*Vnet
	// NodeNetworkChanges is pve1's staged network diff, as GET
	// /nodes/pve1/network reports it beside the interfaces.
	NodeNetworkChanges string
	// BridgeNeverUp keeps every vnet's bridge down after a node reload, as
	// a zone with DHCP does on a node without dnsmasq.
	BridgeNeverUp bool
}

// Task is one entry of the task log.
type Task struct {
	Type  string // qmconfig, qmstop, ...
	User  string // user@realm or user@realm!tokenid
	VMID  int
	Start time.Time
	// Status is "OK" when empty.
	Status string
}

// AddTask appends an entry to the task log.
func (s *Server) AddTask(t Task) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Tasks = append(s.Tasks, t)
}

// New starts a cluster holding template 9000 (rocky-9-cloud, stopped, with a
// cloud-init drive and a 10G disk) and guest 101 (db-01, running, someone
// else's).
func New(t testing.TB) *Server {
	t.Helper()
	s := &Server{Files: map[string][]byte{}, Guests: map[int]*Guest{
		9000: {VMID: 9000, Kind: "qemu", Status: "stopped", Template: true, Config: map[string]any{
			"name": "rocky-9-cloud", "template": 1, "smbios1": "uuid=" + TemplateUUID,
			"net0": "virtio=BC:24:11:AA:BB:09,bridge=vmbr0", "cores": 2, "memory": 2048,
			"scsi0": "local-lvm:base-9000-disk-0,size=10G", "boot": "order=scsi0",
			"ide2": "local-lvm:vm-9000-cloudinit,media=cdrom", "agent": "1",
		}},
		101: {VMID: 101, Kind: "qemu", Status: "running", Config: map[string]any{
			"name": "db-01", "smbios1": "uuid=6f2c9a10-3b4d-4e5f-8a9b-0c1d2e3f4a5b",
			"net0": "virtio=BC:24:11:AA:BB:02,bridge=vmbr0", "cores": 1, "memory": 2048,
			"scsi0": "local-lvm:vm-101-disk-0,size=16G",
		}},
	}}
	s.Server = httptest.NewTLSServer(http.HandlerFunc(s.serve))
	t.Cleanup(s.Close)
	return s
}

// DropNext makes the next request whose "METHOD /path" matches pattern
// happen without its answer reaching the caller.
func (s *Server) DropNext(pattern string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.dropNext = regexp.MustCompile(pattern)
}

// DelayNext makes the next request whose "METHOD /path" matches pattern
// happen at once and answer only after d.
func (s *Server) DelayNext(pattern string, d time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.delayNext, s.delay = regexp.MustCompile(pattern), d
}

// FailNext makes the next n requests whose "METHOD /path" matches pattern
// answer 500 without being carried out, as an overloaded pveproxy does.
func (s *Server) FailNext(pattern string, n int) {
	s.FailNextWith(pattern, http.StatusInternalServerError, n)
}

// FailNextWith is FailNext answering code, such as the 596 pveproxy answers
// when the node it relays to does not.
func (s *Server) FailNextWith(pattern string, code, n int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.failing, s.failTimes, s.failCode = regexp.MustCompile(pattern), n, code
}

// CutNext makes the next request whose "METHOD /path" matches pattern fail
// on the wire, unanswered and not carried out.
func (s *Server) CutNext(pattern string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cutNext = regexp.MustCompile(pattern)
}

// RefuseToken makes every request answer 401, as a cluster does to a revoked
// token.
func (s *Server) RefuseToken() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refuseToken = true
}

// Guest returns a copy of a guest, nil when there is none.
func (s *Server) Guest(vmid int) *Guest {
	s.mu.Lock()
	defer s.mu.Unlock()
	g, ok := s.Guests[vmid]
	if !ok {
		return nil
	}
	cp := *g
	cp.Config = map[string]any{}
	for k, v := range g.Config {
		cp.Config[k] = v
	}
	return &cp
}

// Named returns the vmids of the guests with this name, sorted.
func (s *Server) Named(name string) []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for id, g := range s.Guests {
		if g.Config["name"] == name {
			out = append(out, id)
		}
	}
	sort.Ints(out)
	return out
}

// Writes is every request that was not a GET.
func (s *Server) Writes() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for _, r := range s.Requests {
		if !strings.HasPrefix(r, "GET ") {
			out = append(out, r)
		}
	}
	return out
}

// SetStatus sets a guest's power state, as somebody at the console would.
func (s *Server) SetStatus(vmid int, status string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Guests[vmid].Status = status
}

// SetConfig sets one config key of a guest, as somebody in the UI would.
func (s *Server) SetConfig(vmid int, key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.Guests[vmid].Config[key] = v
}

// SetPending holds a config change for the guest's next start, as Proxmox
// does for a memory change on a running VM without memory hotplug.
func (s *Server) SetPending(vmid int, key string, v any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := s.Guests[vmid]
	if g.Pending == nil {
		g.Pending = map[string]any{}
	}
	g.Pending[key] = v
}

const upid = "UPID:pve1:00001234:00005678:65000000:task:0:accrete@pve!test:"

var (
	guestPath = regexp.MustCompile(`^/nodes/pve1/(qemu|lxc)/(\d+)(/.*)?$`)
	storePath = regexp.MustCompile(`^/nodes/pve1/storage/([^/]+)/(content|upload|status)(/.*)?$`)
)

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	// The answer is written into the response's buffer, which goes out when
	// serve returns: a delay here holds a finished request's answer back.
	if d := s.serveLocked(w, r); d > 0 {
		time.Sleep(d)
	}
}

func (s *Server) serveLocked(w http.ResponseWriter, r *http.Request) (delay time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := strings.TrimPrefix(r.URL.Path, "/api2/json")
	if s.delayNext != nil && s.delayNext.MatchString(r.Method+" "+p) {
		s.delayNext, delay = nil, s.delay
	}
	rec := r.Method + " " + p
	if r.URL.RawQuery != "" {
		rec += "?" + r.URL.RawQuery
	}
	s.Requests = append(s.Requests, rec)

	data := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	fail := func(code int, msg string) {
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{"data": nil, "message": msg})
	}
	if s.refuseToken || r.Header.Get("Authorization") != "PVEAPIToken="+Token {
		fail(http.StatusUnauthorized, "authentication failure")
		return
	}
	var form url.Values
	if strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/") {
		_ = r.ParseMultipartForm(64 << 20)
		form = url.Values{}
		for k, v := range r.MultipartForm.Value {
			form[k] = v
		}
		if fh := r.MultipartForm.File["filename"]; len(fh) == 1 {
			if f, err := fh[0].Open(); err == nil {
				body, _ := io.ReadAll(f)
				_ = f.Close()
				form.Set("filename", fh[0].Filename)
				form.Set("(body)", string(body))
			}
		}
	} else {
		body, _ := io.ReadAll(r.Body)
		form, _ = url.ParseQuery(string(body))
	}
	if s.cutNext != nil && s.cutNext.MatchString(r.Method+" "+p) {
		s.cutNext = nil
		if hj, ok := w.(http.Hijacker); ok {
			if conn, _, err := hj.Hijack(); err == nil {
				_ = conn.Close()
			}
		}
		return
	}
	if s.failTimes > 0 && s.failing.MatchString(r.Method+" "+p) {
		s.failTimes--
		fail(s.failCode, "pvetest: failing on purpose")
		return
	}
	// A dropped answer: the work below is done, then the connection is cut.
	drop := s.dropNext != nil && s.dropNext.MatchString(r.Method+" "+p)
	if drop {
		s.dropNext = nil
		raw := w
		defer func() {
			if hj, ok := raw.(http.Hijacker); ok {
				if conn, _, err := hj.Hijack(); err == nil {
					_ = conn.Close()
				}
			}
		}()
		w = discard{}
		data = func(any) {}
		fail = func(int, string) {}
	}
	task := func() { data(upid) }

	switch {
	case r.Method == http.MethodGet && p == "/nodes":
		data([]map[string]any{{"node": "pve1", "status": "online", "maxmem": 34359738368, "maxcpu": 16, "mem": 8589934592}})
	case r.Method == http.MethodPost && p == "/nodes/pve1/qemu":
		id, err := strconv.Atoi(form.Get("vmid"))
		if err != nil {
			fail(http.StatusBadRequest, "pvetest: vmid")
			return
		}
		if _, taken := s.Guests[id]; taken {
			fail(http.StatusInternalServerError, fmt.Sprintf("unable to create VM %d - VM %d already exists on node 'pve1'", id, id))
			return
		}
		cfg := map[string]any{}
		for k := range form {
			if k == "vmid" || k == "pool" {
				continue
			}
			cfg[k] = form.Get(k)
			if n, err := strconv.Atoi(form.Get(k)); err == nil && (k == "cores" || k == "memory") {
				cfg[k] = n
			}
		}
		// A disk "storage:0,import-from=volid" is a copy of an image.
		fromImage := ""
		if line, ok := cfg["scsi0"].(string); ok && strings.Contains(line, ",import-from=") {
			st, rest, _ := strings.Cut(line, ":")
			fromImage = strings.TrimPrefix(rest[strings.Index(rest, ",import-from="):], ",import-from=")
			if _, ok := s.Files[fromImage]; !ok || !strings.HasPrefix(rest, "0,") {
				fail(http.StatusInternalServerError, fmt.Sprintf("unable to create VM %d - import-from: volume '%s' does not exist", id, fromImage))
				return
			}
			gb := s.ImageGiB[fromImage]
			if gb == 0 {
				gb = 10
			}
			cfg["scsi0"] = fmt.Sprintf("%s:vm-%d-disk-0,size=%dG", st, id, gb)
		}
		// A new disk "storage:size" is allocated as Proxmox names it.
		if line, ok := cfg["scsi0"].(string); ok {
			if st, size, ok := strings.Cut(line, ":"); ok && !strings.Contains(size, "/") && !strings.Contains(size, "disk") {
				cfg["scsi0"] = fmt.Sprintf("%s:vm-%d-disk-0,size=%sG", st, id, size)
			}
		}
		if line, ok := cfg["net0"].(string); ok && !strings.Contains(strings.SplitN(line, ",", 2)[0], "=") {
			model, rest, _ := strings.Cut(line, ",")
			cfg["net0"] = fmt.Sprintf("%s=BC:24:11:01:%02X:%02X,%s", model, id/256, id%256, rest)
		}
		s.Guests[id] = &Guest{VMID: id, Kind: "qemu", Status: "stopped", Config: cfg, FromImage: fromImage}
		task()
	case r.Method == http.MethodGet && (p == "/nodes/pve1/qemu" || p == "/nodes/pve1/lxc"):
		kind := strings.TrimPrefix(p, "/nodes/pve1/")
		rows := []map[string]any{}
		for _, g := range s.sorted() {
			if g.Kind != kind {
				continue
			}
			name := g.Config["name"]
			if g.seen() {
				name = nil
			}
			row := map[string]any{"vmid": g.VMID, "name": name, "status": g.Status,
				"maxmem": memBytes(g), "cpus": g.Config["cores"]}
			if g.Template {
				row["template"] = 1
			}
			rows = append(rows, row)
		}
		data(rows)
	case r.Method == http.MethodGet && p == "/cluster/resources":
		rows := []map[string]any{}
		for _, g := range s.sorted() {
			name := g.Config["name"]
			if g.seen() {
				name = nil
			}
			rows = append(rows, map[string]any{"vmid": g.VMID, "name": name, "node": "pve1", "type": g.Kind,
				"status": g.Status, "template": boolInt(g.Template), "id": fmt.Sprintf("%s/%d", g.Kind, g.VMID)})
		}
		data(rows)
	case r.Method == http.MethodGet && p == "/cluster/nextid":
		// The lowest free id from 100, as Proxmox answers.
		next := 100
		for s.Guests[next] != nil {
			next++
		}
		data(strconv.Itoa(next))
	case r.Method == http.MethodGet && p == "/nodes/pve1/tasks":
		// The task log, newest first, for the vmid asked.
		vmid := r.URL.Query().Get("vmid")
		rows := []map[string]any{}
		for i := len(s.Tasks) - 1; i >= 0; i-- {
			t := s.Tasks[i]
			if vmid != "" && strconv.Itoa(t.VMID) != vmid {
				continue
			}
			status := t.Status
			if status == "" {
				status = "OK"
			}
			// As Proxmox lists it: "user" names the user only, the token
			// is in the UPID.
			user, _, _ := strings.Cut(t.User, "!")
			upid := fmt.Sprintf("UPID:pve1:00001234:00005678:%08X:%s:%d:%s:", t.Start.Unix(), t.Type, t.VMID, t.User)
			rows = append(rows, map[string]any{"type": t.Type, "user": user, "upid": upid, "starttime": t.Start.Unix(),
				"status": status, "id": strconv.Itoa(t.VMID), "node": "pve1"})
		}
		data(rows)
	case strings.HasPrefix(p, "/nodes/pve1/tasks/"):
		data(map[string]any{"status": "stopped", "exitstatus": "OK"})
	case r.Method == http.MethodGet && p == "/access/permissions":
		path := r.URL.Query().Get("path")
		if s.Privileges == nil || path == "" {
			data(map[string]any{"/sdn": map[string]any{"SDN.Audit": 1}})
			break
		}
		// As Proxmox answers for one path: what the token may do there,
		// propagation from parent paths applied.
		have := map[string]any{}
		for _, priv := range s.Privileges[path] {
			have[priv] = 1
		}
		data(map[string]any{path: have})
	case s.sdn(w, r.Method, p, form, data, fail, task):
	case r.Method == http.MethodGet && p == "/nodes/pve1/storage":
		localContent := "iso,vztmpl,import"
		if s.NoImportContent {
			localContent = "iso,vztmpl"
		}
		rows := []map[string]any{}
		for _, row := range []map[string]any{
			{"storage": "local", "type": "dir", "content": localContent, "active": 1, "enabled": 1, "total": 53687091200, "used": 10737418240, "avail": 42949672960},
			{"storage": "local-lvm", "type": "lvmthin", "content": "images,rootdir", "active": 1, "enabled": 1, "total": 107374182400, "used": 32212254720, "avail": 75161927680},
		} {
			if want := r.URL.Query().Get("content"); want == "" || strings.Contains(","+row["content"].(string)+",", ","+want+",") {
				rows = append(rows, row)
			}
		}
		data(rows)
	case storePath.MatchString(p):
		s.storage(r.Method, storePath.FindStringSubmatch(p), form, data, fail, task)
	case guestPath.MatchString(p):
		s.guest(r, guestPath.FindStringSubmatch(p), form, data, fail, task)
	default:
		fail(http.StatusNotImplemented, "pvetest: "+rec+" is not simulated")
	}
	return delay
}

func (s *Server) storage(method string, m []string, form url.Values, data func(any), fail func(int, string), task func()) {
	store, what, rest := m[1], m[2], strings.TrimPrefix(m[3], "/")
	switch {
	case method == http.MethodGet && what == "content":
		rows := []map[string]any{}
		names := make([]string, 0, len(s.Files))
		for v := range s.Files {
			names = append(names, v)
		}
		sort.Strings(names)
		for _, v := range names {
			content := "iso"
			if strings.HasPrefix(v, store+":import/") {
				content = "import"
			}
			if strings.HasPrefix(v, store+":") {
				rows = append(rows, map[string]any{"volid": v, "content": content, "size": len(s.Files[v]), "ctime": FileCTime})
			}
		}
		data(rows)
	case method == http.MethodPost && what == "upload":
		name, content := form.Get("filename"), form.Get("content")
		if content != "iso" && content != "import" {
			fail(http.StatusBadRequest, "pvetest: only iso and import uploads are simulated")
			return
		}
		s.Files[store+":"+content+"/"+name] = []byte(form.Get("(body)"))
		task()
	case method == http.MethodDelete && what == "content":
		vol, _ := url.PathUnescape(rest)
		if _, ok := s.Files[vol]; !ok {
			fail(http.StatusNotFound, "no such volume")
			return
		}
		delete(s.Files, vol)
		task()
	default:
		fail(http.StatusNotImplemented, "pvetest: storage "+what+" is not simulated")
	}
}

func (s *Server) guest(r *http.Request, m []string, form url.Values, data func(any), fail func(int, string), task func()) {
	kind, vmid, rest := m[1], m[2], m[3]
	id, _ := strconv.Atoi(vmid)
	g, ok := s.Guests[id]
	if !ok || g.Kind != kind {
		fail(http.StatusInternalServerError, fmt.Sprintf("Configuration file 'nodes/pve1/%s/%s.conf' does not exist", kind, vmid))
		return
	}
	switch {
	case r.Method == http.MethodGet && rest == "/config":
		if g.seen() {
			data(map[string]any{"lock": "clone", "digest": "0"})
			return
		}
		cfg := map[string]any{}
		for k, v := range g.Config {
			cfg[k] = v
		}
		if r.URL.Query().Get("current") != "1" {
			for k, v := range g.Pending {
				cfg[k] = v
			}
		}
		data(cfg)
	case r.Method == http.MethodGet && rest == "/status/current":
		if g.installing > 0 {
			g.installing--
			if g.installing == 0 {
				g.Status, g.Installed = "stopped", true
			}
		}
		data(map[string]any{"status": statusOf(g), "qmpstatus": g.Status, "maxmem": memBytes(g)})
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/agent/"):
		host := g.AgentHostname
		if host == "" && g.Installed {
			host = g.InstalledHostname
		}
		if g.Status != "running" || host == "" || g.installing != 0 {
			fail(http.StatusInternalServerError, "QEMU guest agent is not running")
			return
		}
		switch strings.TrimPrefix(rest, "/agent/") {
		case "get-host-name":
			data(map[string]any{"result": map[string]any{"host-name": host}})
		case "network-get-interfaces":
			data(map[string]any{"result": []any{}})
		default:
			fail(http.StatusNotImplemented, "pvetest: agent "+rest+" is not simulated")
		}
	case r.Method == http.MethodPost && rest == "/clone":
		if !g.Template {
			fail(http.StatusBadRequest, "pvetest: clone of a non-template")
			return
		}
		newid, _ := strconv.Atoi(form.Get("newid"))
		if _, taken := s.Guests[newid]; taken {
			fail(http.StatusInternalServerError, fmt.Sprintf("unable to create VM %d: config file already exists", newid))
			return
		}
		cfg := map[string]any{}
		for k, v := range g.Config {
			if k != "template" {
				cfg[k] = v
			}
		}
		cfg["name"] = form.Get("name")
		cfg["description"] = form.Get("description")
		cfg["scsi0"] = strings.Replace(fmt.Sprint(g.Config["scsi0"]), fmt.Sprintf("base-%d-disk-0", g.VMID), fmt.Sprintf("vm-%d-disk-0", newid), 1)
		cfg["net0"] = fmt.Sprintf("virtio=BC:24:11:00:%02X:%02X,bridge=vmbr0", newid/256, newid%256)
		cfg["smbios1"] = "uuid=" + CloneUUID(newid)
		s.Guests[newid] = &Guest{VMID: newid, Kind: "qemu", Status: "stopped", Config: cfg, Pool: form.Get("pool"), cloning: cloningReads}
		task()
	case r.Method == http.MethodPost && rest == "/config":
		for k := range form {
			if k == "delete" {
				for _, d := range strings.Split(form.Get(k), ",") {
					delete(g.Config, d)
				}
				continue
			}
			// As Proxmox does without cpu or memory hotplug (its default):
			// a running guest takes the new value at its next start.
			if g.Status != "stopped" && (k == "cores" || k == "memory") && !strings.Contains(fmt.Sprint(g.Config["hotplug"]), map[string]string{"cores": "cpu", "memory": "memory"}[k]) {
				if g.Pending == nil {
					g.Pending = map[string]any{}
				}
				g.Pending[k] = form.Get(k)
				if fmt.Sprint(g.Config[k]) == form.Get(k) {
					delete(g.Pending, k)
				}
				continue
			}
			delete(g.Pending, k)
			v := form.Get(k)
			// As Proxmox does: a card set without a MAC is given one.
			if strings.HasPrefix(k, "net") {
				if model, rest, _ := strings.Cut(v, ","); !strings.Contains(model, "=") {
					n, _ := strconv.Atoi(k[3:])
					v = fmt.Sprintf("%s=BC:24:11:%02X:%02X:%02X,%s", model, n+1, g.VMID/256, g.VMID%256, rest)
				}
			}
			g.Config[k] = v
		}
		data(nil)
	case r.Method == http.MethodPut && rest == "/resize":
		disk, size := form.Get("disk"), form.Get("size")
		line := fmt.Sprint(g.Config[disk])
		if strings.HasPrefix(size, "+") {
			fail(http.StatusBadRequest, "pvetest: relative resize is not simulated")
			return
		}
		g.Config[disk] = regexp.MustCompile(`size=[0-9]+[KMGT]?`).ReplaceAllString(line, "size="+size)
		task()
	case r.Method == http.MethodPost && strings.HasPrefix(rest, "/status/"):
		switch op := strings.TrimPrefix(rest, "/status/"); op {
		case "start":
			if g.Status == "paused" {
				fail(http.StatusInternalServerError, "VM is paused")
				return
			}
			g.applyPending()
			g.Status = "running"
			s.startInstall(g)
		case "reboot":
			if g.Status != "running" {
				fail(http.StatusInternalServerError, "VM is not running")
				return
			}
			g.applyPending()
		case "stop", "shutdown":
			// A stop during an install cuts it short: the disk does not boot.
			g.Status, g.installing = "stopped", 0
		default:
			fail(http.StatusNotImplemented, "pvetest: status/"+op+" is not simulated")
			return
		}
		task()
	case r.Method == http.MethodDelete && rest == "":
		if g.Status != "stopped" {
			fail(http.StatusInternalServerError, "VM is running - destroy failed")
			return
		}
		delete(s.Guests, id)
		task()
	default:
		fail(http.StatusNotImplemented, "pvetest: "+r.Method+" "+rest+" is not simulated")
	}
}

// FileCTime is the date every volume on a storage is listed with.
const FileCTime = 1762300000

// startInstall starts a simulated install on a guest started with an empty
// disk, the installer on ide2 and its answer disc on ide3. The hostname is
// the one the kickstart on the disc names.
func (s *Server) startInstall(g *Guest) {
	if g.Installed || g.installing != 0 {
		return
	}
	vol := func(key string) string {
		line, _ := g.Config[key].(string)
		v, _, _ := strings.Cut(line, ",")
		return v
	}
	if g.FromImage != "" {
		// A first boot: its seed on ide2 asks for a power-off at its end,
		// in cloud-config or in an Ignition config.
		seed := s.Files[vol("ide2")]
		if !bytes.Contains(seed, []byte("accrete-first-boot.service")) && !bytes.Contains(seed, []byte("power_state:")) {
			return
		}
		switch {
		case s.BrokenFirstBoot || s.InstallReads < 0:
			g.installing = -1
		case s.InstallReads == 0:
			g.installing = 2
		default:
			g.installing = s.InstallReads
		}
		return
	}
	iso, answer := vol("ide2"), s.Files[vol("ide3")]
	if _, ok := s.Files[iso]; !ok || !bytes.Contains(answer, []byte("OEMDRV")) {
		return
	}
	m := regexp.MustCompile(`--hostname=([A-Za-z0-9-]+)`).FindSubmatch(answer)
	if m == nil {
		return
	}
	g.InstalledHostname = string(m[1])
	switch {
	case s.InstallReads < 0:
		g.installing = -1
	case s.InstallReads == 0:
		g.installing = 2
	default:
		g.installing = s.InstallReads
	}
}

// CutInstall stops a guest mid-install, as an operator's Stop would: the
// install never finished, so its disk does not boot.
func (s *Server) CutInstall(vmid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.Guests[vmid]; g != nil {
		g.installing, g.Status = 0, "stopped"
	}
}

// SetAgentHostname makes the guest agent of a running guest answer host.
func (s *Server) SetAgentHostname(vmid int, host string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.Guests[vmid]; g != nil {
		g.AgentHostname = host
	}
}

// FinishInstall ends a guest's simulated install now, as the installer's
// poweroff would.
func (s *Server) FinishInstall(vmid int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if g := s.Guests[vmid]; g != nil && g.installing != 0 {
		g.installing, g.Status, g.Installed = 0, "stopped", true
	}
}

func (s *Server) sorted() []*Guest {
	out := make([]*Guest, 0, len(s.Guests))
	for _, g := range s.Guests {
		out = append(out, g)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].VMID < out[j].VMID })
	return out
}

// statusOf is what status/current's "status" says: a paused VM is running
// to Proxmox, and only qmpstatus says it is paused.
func statusOf(g *Guest) string {
	if g.Status == "paused" {
		return "running"
	}
	return g.Status
}

func memBytes(g *Guest) int64 {
	mb, _ := strconv.ParseInt(fmt.Sprint(g.Config["memory"]), 10, 64)
	return mb << 20
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// discard is where a dropped answer is written.
type discard struct{}

func (discard) Header() http.Header         { return http.Header{} }
func (discard) Write(b []byte) (int, error) { return len(b), nil }
func (discard) WriteHeader(int)             {}
