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
		out = append(out, provider.Change{
			At: time.Unix(int64(ts), 0).UTC(), By: r.User, Kind: k.kind, What: r.Type, PowerState: k.power,
			// A running task has no status yet; any finished one other than
			// OK (or a warning) failed.
			Failed: r.Status != "" && r.Status != "OK" && !strings.HasPrefix(r.Status, "WARNINGS"),
			Self:   self != "" && r.User == self,
		})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].At.After(out[j].At) })
	return out, nil
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
