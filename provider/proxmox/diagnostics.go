package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reads for "why did this guest stop": what the node did to it (its task log),
// what the node's kernel said about it (the journal), how full the node was,
// and whether this token may read the journal at all. All reads; nothing here
// changes the cluster.

// GuestTask is one entry of a node's task log about one guest.
type GuestTask struct {
	UPID string
	// Type is Proxmox's own task type: qmstop, qmshutdown, qmreboot,
	// vzdump, qmigrate, qmstart, vzstop, ... Action says what it amounts to.
	Type   string
	Action string
	// Status is "OK", an error message, or "" while the task still runs.
	Status string
	User   string
	Start  time.Time
	// End is zero while the task runs.
	End time.Time
}

// Task actions, independent of the hypervisor's own names.
const (
	TaskActionStop     = "stop"
	TaskActionShutdown = "shutdown"
	TaskActionReboot   = "reboot"
	TaskActionStart    = "start"
	TaskActionBackup   = "backup"
	TaskActionMigrate  = "migrate"
	TaskActionOther    = "other"
)

// TaskAction maps a task type to what it does to the guest.
func TaskAction(taskType string) string {
	switch taskType {
	case "qmstop", "vzstop":
		return TaskActionStop
	case "qmshutdown", "vzshutdown":
		return TaskActionShutdown
	case "qmreboot", "vzreboot", "qmreset":
		return TaskActionReboot
	case "qmstart", "vzstart", "qmresume", "vzresume":
		return TaskActionStart
	case "vzdump":
		return TaskActionBackup
	case "qmigrate", "vzmigrate", "qmremote_migrate":
		return TaskActionMigrate
	}
	return TaskActionOther
}

// GuestTasks reads the node's task log entries for one guest that started in
// [since, until], oldest first. The token needs Sys.Audit on the node to see
// tasks other users started; without it Proxmox returns only the token's own.
func (c *Client) GuestTasks(ctx context.Context, node string, vmid int, since, until time.Time) ([]GuestTask, error) {
	if node == "" || vmid <= 0 {
		return nil, fmt.Errorf("proxmox: tasks: node and vmid are required")
	}
	q := url.Values{
		"vmid":   {strconv.Itoa(vmid)},
		"since":  {strconv.FormatInt(since.Unix(), 10)},
		"until":  {strconv.FormatInt(until.Unix(), 10)},
		"source": {"all"},
		"limit":  {"200"},
	}
	var rows []struct {
		UPID      string          `json:"upid"`
		Type      string          `json:"type"`
		Status    string          `json:"status"`
		User      string          `json:"user"`
		StartTime json.RawMessage `json:"starttime"`
		EndTime   json.RawMessage `json:"endtime"`
		ID        string          `json:"id"`
	}
	if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/tasks?%s", url.PathEscape(node), q.Encode()), &rows); err != nil {
		return nil, err
	}
	out := make([]GuestTask, 0, len(rows))
	for _, r := range rows {
		if r.ID != "" && r.ID != strconv.Itoa(vmid) {
			continue // filtered by vmid already; a release that ignores the filter is held to it here
		}
		start, ok := rrdNumber(r.StartTime)
		if !ok {
			continue
		}
		t := GuestTask{
			UPID: r.UPID, Type: r.Type, Action: TaskAction(r.Type),
			Status: r.Status, User: r.User, Start: time.Unix(int64(start), 0).UTC(),
		}
		if end, ok := rrdNumber(r.EndTime); ok {
			t.End = time.Unix(int64(end), 0).UTC()
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start.Before(out[j].Start) })
	return out, nil
}

// JournalLine is one line of a node's journal.
type JournalLine struct {
	// Time is parsed from the line when it carries a full timestamp; zero
	// otherwise. The read is bounded by time on the server, so a line's place
	// in the window is known even when its own stamp is not.
	Time time.Time
	Text string
	// GuestOOMKill is set on the kernel's record of killing this guest's own
	// process (its kvm, or a process in its container) for lack of memory.
	GuestOOMKill bool
}

// maxJournalLines bounds one journal read. A node's journal over forty
// minutes is usually a few hundred lines.
const maxJournalLines = 5000

// GuestJournal reads the node's journal between since and until and keeps
// the lines about memory and about this guest. The token needs Sys.Syslog on
// /nodes/<node>; without it the error is an APIError MissingPrivilege names.
func (c *Client) GuestJournal(ctx context.Context, node, kind string, vmid int, since, until time.Time) ([]JournalLine, error) {
	if node == "" || vmid <= 0 {
		return nil, fmt.Errorf("proxmox: journal: node and vmid are required")
	}
	q := url.Values{
		"since": {strconv.FormatInt(since.Unix(), 10)},
		"until": {strconv.FormatInt(until.Unix(), 10)},
	}
	var raw []string
	if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/journal?%s", url.PathEscape(node), q.Encode()), &raw); err != nil {
		return nil, err
	}
	if len(raw) > maxJournalLines {
		raw = raw[len(raw)-maxJournalLines:]
	}
	return FilterGuestJournal(raw, kind, vmid), nil
}

var (
	journalOOMRe        = regexp.MustCompile(`(?i)out of memory|oom-kill|oom_reaper|invoked oom-killer`)
	journalKilledRe     = regexp.MustCompile(`Killed process (\d+) \(([^)]*)\)`)
	journalTaskMemcgRe  = regexp.MustCompile(`task_memcg=([^,\s]+)`)
	journalPidRe        = regexp.MustCompile(`[,:]pid=(\d+)`)
	journalISOStampRe   = regexp.MustCompile(`^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d+)?(?:[+-]\d{2}:?\d{2}|Z))\s`)
	journalMaxLineBytes = 1024
)

// FilterGuestJournal keeps the lines worth showing about one guest: every
// OOM-related kernel line, and any line naming the guest's scope or VMID.
// A kill whose cgroup is the guest's own (/qemu.slice/<vmid>.scope for a VM,
// /lxc/<vmid>/ for a container) is marked GuestOOMKill.
func FilterGuestJournal(lines []string, kind string, vmid int) []JournalLine {
	id := strconv.Itoa(vmid)
	scope := "/qemu.slice/" + id + ".scope"
	if kind == "lxc" {
		scope = "/lxc/" + id + "/"
	}
	mentions := regexp.MustCompile(`\b(?:VM|CT) ` + id + `\b|\bvmid[= ]` + id + `\b|\b` + id + `\.scope\b|/lxc/` + id + `/|\b(?:tap|veth)` + id + `i\d+\b`)

	out := []JournalLine{}
	pendingPID := ""
	for _, l := range lines {
		text := strings.TrimSpace(l)
		if text == "" {
			continue
		}
		oom := journalOOMRe.MatchString(text) || journalKilledRe.MatchString(text)
		if !oom && !mentions.MatchString(text) {
			continue
		}
		jl := JournalLine{Text: truncateLine(text)}
		if m := journalISOStampRe.FindStringSubmatch(text); m != nil {
			if t, err := parseJournalStamp(m[1]); err == nil {
				jl.Time = t
			}
		}
		if m := journalTaskMemcgRe.FindStringSubmatch(text); m != nil {
			pendingPID = ""
			if strings.HasPrefix(m[1]+"/", scope) || strings.HasPrefix(m[1], scope) {
				if p := journalPidRe.FindStringSubmatch(text); p != nil {
					pendingPID = p[1]
				}
			}
		}
		if m := journalKilledRe.FindStringSubmatch(text); m != nil && pendingPID != "" && m[1] == pendingPID {
			jl.GuestOOMKill = true
			pendingPID = ""
		}
		out = append(out, jl)
	}
	return out
}

func truncateLine(s string) string {
	if len(s) <= journalMaxLineBytes {
		return s
	}
	return s[:journalMaxLineBytes]
}

func parseJournalStamp(s string) (time.Time, error) {
	for _, layout := range []string{"2006-01-02T15:04:05Z0700", "2006-01-02T15:04:05Z07:00", "2006-01-02T15:04:05.999999Z0700", "2006-01-02T15:04:05.999999Z07:00"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("not a journal timestamp: %q", s)
}

// NodeMemoryPoint is one step of a node's memory history.
type NodeMemoryPoint struct {
	Time       time.Time
	UsedBytes  *float64
	TotalBytes *float64
}

// NodeMemoryHistory reads a node's memory over one timeframe, MAX
// consolidated, oldest first. Needs Sys.Audit on the node.
func (c *Client) NodeMemoryHistory(ctx context.Context, node string, tf Timeframe) ([]NodeMemoryPoint, error) {
	if node == "" {
		return nil, fmt.Errorf("proxmox: node rrd: node is required")
	}
	if !tf.Valid() {
		return nil, fmt.Errorf("proxmox: node rrd: unknown timeframe %q", tf)
	}
	q := url.Values{"timeframe": {string(tf)}, "cf": {string(ConsolidationMax)}}
	var rows []map[string]json.RawMessage
	if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/rrddata?%s", url.PathEscape(node), q.Encode()), &rows); err != nil {
		return nil, err
	}
	out := make([]NodeMemoryPoint, 0, len(rows))
	for _, r := range rows {
		ts, ok := rrdNumber(r["time"])
		if !ok {
			continue
		}
		used, _ := rrdField(r, "memused")
		total, _ := rrdField(r, "memtotal")
		out = append(out, NodeMemoryPoint{Time: time.Unix(int64(ts), 0).UTC(), UsedBytes: used, TotalBytes: total})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Time.Before(out[j].Time) })
	return out, nil
}

// Privileges is what the calling token may do on one ACL path, as Proxmox
// computes it: for a privilege-separated token, the intersection of the
// user's and the token's grants, propagated from parent paths.
type Privileges map[string]bool

// EffectivePrivileges asks Proxmox what the caller itself may do on path. No
// userid is sent, so the answer is for this token, never for its user.
func (c *Client) EffectivePrivileges(ctx context.Context, path string) (Privileges, error) {
	var byPath map[string]map[string]json.RawMessage
	if err := c.Get(ctx, "/access/permissions?"+url.Values{"path": {path}}.Encode(), &byPath); err != nil {
		return nil, err
	}
	out := Privileges{}
	// Only the entry for the path itself: Proxmox has applied propagation to
	// it. A parent's entry may hold a grant made with propagate=0, which does
	// not reach this path, so reading parents could claim a privilege the
	// token does not have.
	for name, v := range byPath[path] {
		if n, ok := rrdNumber(v); ok && n != 0 {
			out[name] = true
		}
	}
	return out, nil
}

// TokenPrivilegeSeparated reports whether the API token user!tokenID is
// privilege-separated. ok is false when Proxmox would not say (the caller
// may not read its own token's settings without User.Modify or the like).
func (c *Client) TokenPrivilegeSeparated(ctx context.Context, user, tokenID string) (separated, ok bool) {
	var t struct {
		Privsep json.RawMessage `json:"privsep"`
	}
	path := fmt.Sprintf("/access/users/%s/token/%s", url.PathEscape(user), url.PathEscape(tokenID))
	if err := c.Get(ctx, path, &t); err != nil {
		return false, false
	}
	n, known := rrdNumber(t.Privsep)
	if !known {
		return false, false
	}
	return n != 0, true
}

// RolePrivileges lists a role's privileges. exists is false when Proxmox says
// there is no such role; err is any other failure (including not being
// allowed to read roles).
func (c *Client) RolePrivileges(ctx context.Context, role string) (privs Privileges, exists bool, err error) {
	var raw map[string]json.RawMessage
	err = c.Get(ctx, "/access/roles/"+url.PathEscape(role), &raw)
	if err != nil {
		var ae *APIError
		// A missing role is a 500 "role '...' does not exist" on PVE 7-9.
		if errors.As(err, &ae) && (ae.Code == http.StatusNotFound ||
			(ae.Code == http.StatusInternalServerError && strings.Contains(strings.ToLower(ae.Reason+" "+ae.Body), "does not exist"))) {
			return nil, false, nil
		}
		return nil, false, err
	}
	privs = Privileges{}
	for name, v := range raw {
		if n, ok := rrdNumber(v); ok && n != 0 {
			privs[name] = true
		}
	}
	return privs, true, nil
}
