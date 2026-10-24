package proxmox

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// diagServer answers each path with a fixed body (or status), and records
// every request it was asked.
func diagServer(t *testing.T, routes map[string]string) (*Client, *[]string) {
	t.Helper()
	var asked []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = append(asked, r.URL.Path+"?"+r.URL.RawQuery)
		p := strings.TrimPrefix(r.URL.Path, "/api2/json")
		body, ok := routes[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":` + body + `}`))
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "PVEAPIToken=t", HTTP: srv.Client()}, &asked
}

func TestGuestTasks(t *testing.T) {
	c, asked := diagServer(t, map[string]string{
		"/nodes/flk-vega-01/tasks": `[
			{"upid":"UPID:flk-vega-01:1:2:3:qmshutdown:114:root@pam:","type":"qmshutdown","status":"OK","user":"root@pam","starttime":1792800360,"endtime":1792800380,"id":"114"},
			{"upid":"UPID:flk-vega-01:1:2:3:vzdump:114:root@pam:","type":"vzdump","status":"OK","user":"root@pam","starttime":1792800000,"endtime":1792800100,"id":"114"},
			{"upid":"x","type":"qmstart","status":"","user":"sybil@pve!sybil","starttime":1792800500,"id":"114"},
			{"upid":"y","type":"qmstop","status":"OK","user":"root@pam","starttime":1792800400,"id":"115"}
		]`,
	})
	since := time.Unix(1792798560, 0)
	tasks, err := c.GuestTasks(context.Background(), "flk-vega-01", 114, since, since.Add(40*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	q := (*asked)[0]
	if !strings.Contains(q, "vmid=114") || !strings.Contains(q, "since=1792798560") || !strings.Contains(q, "source=all") {
		t.Errorf("query = %q", q)
	}
	if len(tasks) != 3 {
		t.Fatalf("want the three tasks of VM 114, got %+v", tasks)
	}
	if tasks[0].Action != TaskActionBackup || tasks[1].Action != TaskActionShutdown || tasks[2].Action != TaskActionStart {
		t.Errorf("not oldest first or wrong actions: %+v", tasks)
	}
	if !tasks[2].End.IsZero() || tasks[1].End != time.Unix(1792800380, 0).UTC() {
		t.Errorf("end times: %+v", tasks)
	}
}

// What a node's journal says when the host kills a guest for memory: the
// guest's kvm, in its own scope, is the victim.
var hostOOMJournal = []string{
	"2026-10-22T04:05:00+0200 flk-vega-01 pvedaemon[1200]: <root@pam> starting task UPID:flk-vega-01:qmstart:113",
	"2026-10-22T04:05:01+0200 flk-vega-01 kernel: kvm invoked oom-killer: gfp_mask=0x140dca(GFP_HIGHUSER_MOVABLE|__GFP_COMP|__GFP_ZERO), order=0, oom_score_adj=0",
	"2026-10-22T04:05:01+0200 flk-vega-01 kernel: oom-kill:constraint=CONSTRAINT_NONE,nodemask=(null),cpuset=qemu.slice,mems_allowed=0,global_oom,task_memcg=/qemu.slice/114.scope,task=kvm,pid=48211,uid=0",
	"2026-10-22T04:05:01+0200 flk-vega-01 kernel: Out of memory: Killed process 48211 (kvm) total-vm:9876543kB, anon-rss:8300000kB, file-rss:2048kB, shmem-rss:0kB, UID:0 pgtables:17000kB oom_score_adj:0",
	"2026-10-22T04:05:02+0200 flk-vega-01 systemd[1]: 114.scope: A process of this unit has been killed by the OOM killer.",
	"2026-10-22T04:05:03+0200 flk-vega-01 kernel: vmbr0: port 3(tap114i0) entered disabled state",
	"2026-10-22T04:06:00+0200 flk-vega-01 pveproxy[999]: worker exited",
}

func TestFilterGuestJournalMarksTheHostKillOfThisGuest(t *testing.T) {
	lines := FilterGuestJournal(hostOOMJournal, "qemu", 114)
	var kills []JournalLine
	for _, l := range lines {
		if l.GuestOOMKill {
			kills = append(kills, l)
		}
	}
	if len(kills) != 1 || !strings.Contains(kills[0].Text, "Killed process 48211 (kvm)") {
		t.Fatalf("want exactly the kvm kill marked, got %+v", kills)
	}
	if want := time.Date(2026, 10, 22, 2, 5, 1, 0, time.UTC); !kills[0].Time.Equal(want) {
		t.Errorf("time = %v, want %v", kills[0].Time, want)
	}
	for _, l := range lines {
		if strings.Contains(l.Text, "pveproxy") || strings.Contains(l.Text, "qmstart:113") {
			t.Errorf("unrelated line kept: %q", l.Text)
		}
	}
	if len(lines) != 5 {
		t.Errorf("want the 3 OOM lines, the 114.scope line and its tap line, got %d: %+v", len(lines), lines)
	}
}

func TestFilterGuestJournalDoesNotBlameAnotherGuest(t *testing.T) {
	other := []string{
		"kernel: oom-kill:constraint=CONSTRAINT_NONE,global_oom,task_memcg=/qemu.slice/115.scope,task=kvm,pid=5000,uid=0",
		"kernel: Out of memory: Killed process 5000 (kvm) anon-rss:100kB",
	}
	for _, l := range FilterGuestJournal(other, "qemu", 114) {
		if l.GuestOOMKill {
			t.Fatalf("VM 115's kill attributed to 114: %q", l.Text)
		}
	}
	ct := []string{
		"kernel: oom-kill:constraint=CONSTRAINT_MEMCG,oom_memcg=/lxc/120,task_memcg=/lxc/120/ns/system.slice/app.service,task=java,pid=777,uid=100000",
		"kernel: Memory cgroup out of memory: Killed process 777 (java) anon-rss:100kB",
	}
	kill := false
	for _, l := range FilterGuestJournal(ct, "lxc", 120) {
		kill = kill || l.GuestOOMKill
	}
	if !kill {
		t.Fatal("a kill inside container 120 is a kill of that guest")
	}
}

func TestGuestJournalMissingSyslog(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Proxmox's own refusal: the reason is in the status line.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(403)
	}))
	t.Cleanup(srv.Close)
	// httptest cannot set a custom reason phrase, so the parse of one is
	// asserted directly below; here the request shape and error type.
	c := &Client{BaseURL: srv.URL, Token: "PVEAPIToken=t", HTTP: srv.Client()}
	_, err := c.GuestJournal(context.Background(), "flk-vega-01", "qemu", 114, time.Unix(0, 0), time.Unix(60, 0))
	var ae *APIError
	if err == nil || !errors.As(err, &ae) || ae.Code != 403 {
		t.Fatalf("want a 403 APIError, got %v", err)
	}
	ae.Reason = "Permission check failed (/nodes/flk-vega-01, Sys.Syslog)"
	if path, priv, ok := MissingPrivilege(ae); !ok || path != "/nodes/flk-vega-01" || priv != "Sys.Syslog" {
		t.Fatalf("MissingPrivilege = %q %q %v", path, priv, ok)
	}
}

func TestNodeMemoryHistory(t *testing.T) {
	c, asked := diagServer(t, map[string]string{
		"/nodes/flk-vega-01/rrddata": `[{"time":1792800060,"memused":28000000000,"memtotal":29000000000},{"time":1792800000,"memused":20000000000,"memtotal":29000000000},{"memused":1}]`,
	})
	pts, err := c.NodeMemoryHistory(context.Background(), "flk-vega-01", TimeframeHour)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains((*asked)[0], "cf=MAX") || len(pts) != 2 || *pts[1].UsedBytes != 28000000000 {
		t.Fatalf("got %+v (asked %v)", pts, *asked)
	}
}

func TestEffectivePrivilegesReadsOnlyThePathItself(t *testing.T) {
	c, asked := diagServer(t, map[string]string{
		"/access/permissions": `{"/nodes/flk-vega-01":{"Sys.Audit":1,"VM.Audit":1},"/":{"Sys.Syslog":1}}`,
	})
	privs, err := c.EffectivePrivileges(context.Background(), "/nodes/flk-vega-01")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains((*asked)[0], "userid") {
		t.Error("must ask for the caller, never a userid")
	}
	if !privs["Sys.Audit"] || privs["Sys.Syslog"] {
		t.Fatalf("got %v", privs)
	}
}

func TestTokenAndRole(t *testing.T) {
	c, _ := diagServer(t, map[string]string{
		"/access/users/sybil@pve/token/sybil": `{"privsep":1,"comment":""}`,
		"/access/roles/SybilDiagnostics":      `{"Sys.Audit":1,"Sys.Syslog":1}`,
	})
	if sep, ok := c.TokenPrivilegeSeparated(context.Background(), "sybil@pve", "sybil"); !ok || !sep {
		t.Fatalf("privsep = %v %v", sep, ok)
	}
	if _, ok := c.TokenPrivilegeSeparated(context.Background(), "sybil@pve", "other"); ok {
		t.Fatal("an unreadable token must say it does not know")
	}
	privs, exists, err := c.RolePrivileges(context.Background(), "SybilDiagnostics")
	if err != nil || !exists || !privs["Sys.Syslog"] {
		t.Fatalf("role = %v %v %v", privs, exists, err)
	}
	if _, exists, err := c.RolePrivileges(context.Background(), "Nope"); exists || err != nil {
		t.Fatalf("a missing role is not an error: %v %v", exists, err)
	}
}

func TestTaskAction(t *testing.T) {
	for typ, want := range map[string]string{"qmstop": "stop", "vzshutdown": "shutdown", "qmreboot": "reboot", "vzdump": "backup", "qmigrate": "migrate", "qmstart": "start", "qmclone": "other"} {
		if got := TaskAction(typ); got != want {
			t.Errorf("%s -> %s, want %s", typ, got, want)
		}
	}
}
