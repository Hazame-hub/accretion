package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/hazame-hub/accretion/provider"
)

// Proxmox's task log is its record of who changed a guest: every
// asynchronous API call (a start, a stop, a resize, a config change sent as
// POST) leaves a task naming the user or token that made it. A change made
// through a synchronous call (a config PUT) or from inside the guest (a
// shutdown typed in its console) leaves none, so a missing task is never
// read as "nobody changed it".
//
// Others' tasks are listed only to a token with Sys.Audit on the node.

// taskKinds maps the task types that change a qemu guest to a change kind
// and, for power, the state they leave the guest in.
var taskKinds = map[string]struct{ kind, power string }{
	"qmconfig":   {provider.ChangeConfig, ""},
	"qmresize":   {provider.ChangeDisk, ""},
	"qmmove":     {provider.ChangeDisk, ""},
	"qmstart":    {provider.ChangePower, "running"},
	"qmresume":   {provider.ChangePower, "running"},
	"qmreboot":   {provider.ChangePower, "running"},
	"qmreset":    {provider.ChangePower, "running"},
	"qmstop":     {provider.ChangePower, "stopped"},
	"qmshutdown": {provider.ChangePower, "stopped"},
	"qmsuspend":  {provider.ChangePower, "paused"},
	"qmpause":    {provider.ChangePower, "paused"},
}

// Changes implements provider.History from the node's task log.
func (s *session) Changes(ctx context.Context, ref provider.Ref) ([]provider.Change, error) {
	node, vmid, err := guestRef(ref)
	if err != nil {
		return nil, err
	}
	var rows []struct {
		Type      string          `json:"type"`
		User      string          `json:"user"`
		StartTime json.RawMessage `json:"starttime"`
		Status    string          `json:"status"`
		ID        string          `json:"id"`
		UPID      string          `json:"upid"`
		TokenID   string          `json:"tokenid"`
	}
	q := url.Values{"vmid": {vmid}, "limit": {"500"}, "source": {"all"}}
	if err := s.c.Get(ctx, fmt.Sprintf("/nodes/%s/tasks?%s", url.PathEscape(node), q.Encode()), &rows); err != nil {
		return nil, err
	}
	self := tokenID(s.c.Token)
	out := []provider.Change{}
	for _, r := range rows {
		k, ok := taskKinds[r.Type]
		if !ok || (r.ID != "" && r.ID != vmid) {
			continue
		}
		ts, ok := rrdNumber(r.StartTime)
		if !ok {
			continue
		}
		by := taskAuthor(r.User, r.UPID, r.TokenID)
		out = append(out, provider.Change{
			At: time.Unix(int64(ts), 0).UTC(), By: by, Kind: k.kind, What: r.Type, PowerState: k.power,
			// A running task has no status yet; any finished one other than
			// OK (or a warning) failed.
			Failed: r.Status != "" && r.Status != "OK" && !strings.HasPrefix(r.Status, "WARNINGS"),
			Self:   isSelf(by, self),
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	markRebootStarts(out)
	return out, nil
}

// rebootStartWindow is how soon after a reboot task Proxmox's own start of
// the guest follows it.
const rebootStartWindow = 2 * time.Minute

// markRebootStarts gives a reboot's own start to whoever asked for the
// reboot. Proxmox carries out a reboot as a stop and a qmstart task that it
// logs under root@pam (lab, PVE 9.1, 2026-11-05: accrete's qmreboot at
// 22:17:57, then "Start" by root@pam at 22:17:59), so the newest power task
// after accrete restarted a machine would otherwise blame root@pam. changes
// is newest first.
func markRebootStarts(changes []provider.Change) {
	for i := range changes {
		c := &changes[i]
		if c.What != "qmstart" || c.By != "root@pam" {
			continue
		}
		for j := i + 1; j < len(changes); j++ {
			r := changes[j]
			if c.At.Sub(r.At) > rebootStartWindow {
				break
			}
			if r.What == "qmreboot" && !r.Failed {
				c.By, c.Self = r.By, r.Self
				c.What = "qmstart, part of " + r.By + "'s qmreboot"
				break
			}
		}
	}
}

// taskAuthor is who made a task, naming the token when one did. The task
// list's "user" field names only the user (accrete@pve) even when a token of
// it ran the task; the token is in the UPID's user field
// (UPID:node:pid:pstart:starttime:type:id:user@realm!tokenid:), and newer
// releases also give it as "tokenid". Seen on PVE 9.1 on 2026-11-02, where
// comparing "user" with the token blamed accrete's own task on its user.
func taskAuthor(user, upid, tokenid string) string {
	if parts := strings.Split(upid, ":"); len(parts) >= 8 && parts[0] == "UPID" && parts[7] != "" {
		return parts[7]
	}
	if tokenid != "" && !strings.Contains(user, "!") {
		return user + "!" + tokenid
	}
	return user
}

// isSelf says whether a task's author is accrete's own token. When the
// record names only the token's user, it may be accrete or the person behind
// that user; it is taken as accrete's own, so that the change is reported
// unknown rather than blamed on somebody.
func isSelf(author, self string) bool {
	if self == "" || author == "" {
		return false
	}
	if author == self {
		return true
	}
	user, _, _ := strings.Cut(self, "!")
	return !strings.Contains(author, "!") && author == user
}

// tokenID is user@realm!tokenid out of the Authorization header value, as
// the task log names the token. "" when it is not a token.
func tokenID(header string) string {
	t := strings.TrimPrefix(header, "PVEAPIToken=")
	id, _, ok := strings.Cut(t, "=")
	if !ok || !strings.Contains(id, "!") {
		return ""
	}
	return id
}
