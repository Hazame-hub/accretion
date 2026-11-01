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
	// cloning counts the reads that still see the guest being cloned.
	cloning int
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
		_ = r.ParseMultipartForm(32 << 20)
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
	case r.Method == http.MethodGet && strings.HasPrefix(p, "/cluster/sdn/"):
		data([]any{})
	case r.Method == http.MethodGet && p == "/nodes/pve1/storage":
		data([]map[string]any{
			{"storage": "local", "type": "dir", "content": "iso,vztmpl,import", "active": 1, "enabled": 1, "total": 53687091200, "used": 10737418240, "avail": 42949672960},
			{"storage": "local-lvm", "type": "lvmthin", "content": "images,rootdir", "active": 1, "enabled": 1, "total": 107374182400, "used": 32212254720, "avail": 75161927680},
		})
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
			if strings.HasPrefix(v, store+":") {
				rows = append(rows, map[string]any{"volid": v, "content": "iso", "size": len(s.Files[v])})
			}
		}
		data(rows)
	case method == http.MethodPost && what == "upload":
		name := form.Get("filename")
		if form.Get("content") != "iso" {
			fail(http.StatusBadRequest, "pvetest: only iso uploads are simulated")
			return
		}
		s.Files[store+":iso/"+name] = []byte(form.Get("(body)"))
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
		data(g.Config)
	case r.Method == http.MethodGet && rest == "/status/current":
		data(map[string]any{"status": statusOf(g), "qmpstatus": g.Status, "maxmem": memBytes(g)})
	case r.Method == http.MethodGet && strings.HasPrefix(rest, "/agent/"):
		fail(http.StatusInternalServerError, "QEMU guest agent is not running")
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
		s.Guests[newid] = &Guest{VMID: newid, Kind: "qemu", Status: "stopped", Config: cfg, cloning: cloningReads}
		task()
	case r.Method == http.MethodPost && rest == "/config":
		for k := range form {
			if k == "delete" {
				for _, d := range strings.Split(form.Get(k), ",") {
					delete(g.Config, d)
				}
				continue
			}
			g.Config[k] = form.Get(k)
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
			g.Status = "running"
		case "stop", "shutdown":
			g.Status = "stopped"
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
