package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/hazame-hub/accretion/provider"
)

// Guards: the reads that stand between a request and the guest it is about.
//
// Each one either says the guest is the one meant, in the state meant, or
// refuses. None of them changes anything; they are what a write must pass
// first, and what it reads afterwards to know it is done.

// Acting on a machine by its ref, under the identity it was built with.
//
// An embedder that changes or removes a machine knows two things about it:
// the provider ref its record stored ("pve/qemu/107") and the SMBIOS UUID the
// building run minted. It hands the ref over whole, and WithRef splits it.
// The UUID becomes a guard (GuardIdentity): the guest at that VMID must still
// carry it, or nothing is sent.

// WithRef returns the params with node, type and vmid filled from `ref`, when
// one is given. A ref that disagrees with an explicit node, type or vmid is
// refused rather than one of them silently winning.
func WithRef(actionType string, p map[string]interface{}) (map[string]interface{}, error) {
	ref := strings.TrimSpace(strParam(p, "ref"))
	if ref == "" {
		return p, nil
	}
	parts := strings.Split(ref, "/")
	if len(parts) != 3 || !nodeName.MatchString(parts[0]) || !vmidValue.MatchString(parts[2]) ||
		(parts[1] != "qemu" && parts[1] != "lxc") {
		return nil, fmt.Errorf("proxmox: %s: ref %q is not node/qemu|lxc/vmid", actionType, ref)
	}
	out := make(map[string]interface{}, len(p)+3)
	for k, v := range p {
		out[k] = v
	}
	for key, val := range map[string]string{"node": parts[0], "type": parts[1], "vmid": parts[2]} {
		if have := strings.TrimSpace(strParam(p, key)); have != "" && have != val {
			return nil, fmt.Errorf("proxmox: %s: ref %q names %s %q, but %s %q was also given", actionType, ref, key, val, key, have)
		}
		out[key] = val
	}
	return out, nil
}

// nodeName and vmidValue are what a ref's parts may be: they become API
// path segments.
var (
	nodeName  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.-]{0,62}$`)
	vmidValue = regexp.MustCompile(`^[0-9]{1,9}$`)
)

// GuardIdentity refuses an action on a guest that does not carry the SMBIOS
// UUID it was told to expect, or the name, when one is given. vm_delete checks
// its own, so it is not guarded twice here.
func (c *Client) GuardIdentity(ctx context.Context, actionType string, p map[string]interface{}) error {
	raw := strings.TrimSpace(strParam(p, "expected_uuid"))
	if actionType == "vm_delete" {
		return nil
	}
	if raw == "" {
		if strings.TrimSpace(strParam(p, "name")) != "" {
			return fmt.Errorf("proxmox: %s: a name guard needs expected_uuid beside it", actionType)
		}
		return nil
	}
	want := provider.UUIDForms(raw)
	if want == nil {
		return fmt.Errorf("proxmox: %s: expected_uuid %q is not a usable UUID — refusing rather than acting unguarded", actionType, raw)
	}
	kind := strParam(p, "type")
	if kind == "" {
		kind = "qemu"
	}
	if kind != "qemu" {
		return fmt.Errorf("proxmox: %s: expected_uuid needs a qemu guest — a container has no SMBIOS identity to check", actionType)
	}
	node, vmID := strParam(p, "node"), strParam(p, "vmid")
	if node == "" || vmID == "" {
		return fmt.Errorf("proxmox: %s requires node and vmid, or ref", actionType)
	}
	if want := strings.TrimSpace(strParam(p, "name")); want != "" {
		var cfg struct {
			Name string `json:"name"`
		}
		if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, vmID), &cfg); err != nil {
			return fmt.Errorf("proxmox: %s: cannot read the name of %s/qemu/%s — refusing: %w", actionType, node, vmID, err)
		}
		if cfg.Name != want {
			return fmt.Errorf("proxmox: %s: vmid %s is %q, not %q — refusing to act on a guest this action was not asked about",
				actionType, vmID, cfg.Name, want)
		}
	}
	got, err := c.GuestSMBIOSUUID(ctx, node, vmID)
	if err != nil {
		return fmt.Errorf("proxmox: %s: cannot read the SMBIOS UUID of %s/qemu/%s to confirm it is the guest meant — refusing: %w",
			actionType, node, vmID, err)
	}
	if !SameUUID(got, want) {
		if got == "" {
			got = "none"
		}
		return fmt.Errorf("proxmox: %s: vmid %s carries SMBIOS UUID %s, not %s — refusing to act on a guest this action was not asked about",
			actionType, vmID, got, want[0])
	}
	return nil
}

// CheckCloneTarget looks at the VMID a clone is about to create, before it is
// sent. A clone writes an ownership marker (its description) in the same call
// that creates the guest, so a retry can recognise its own work:
//
//   - nothing answers at that VMID: (false, nil), the clone may go ahead;
//   - a guest is there with this clone's name and marker: (true, nil), the
//     clone already happened;
//   - anything else is there: an error. Without a marker nothing can be
//     proved, and a guest without this one is somebody else's.
func (c *Client) CheckCloneTarget(ctx context.Context, node, vmID, name, marker string) (bool, error) {
	marker = strings.TrimSpace(marker)
	var existing struct {
		Name        string `json:"name"`
		Description string `json:"description"`
	}
	if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, vmID), &existing); err != nil {
		return false, nil
	}
	if marker != "" && existing.Name == name && strings.TrimSpace(existing.Description) == marker {
		return true, nil
	}
	if marker == "" {
		return false, fmt.Errorf(
			"proxmox: vmid %s already exists on %s as %q, and this clone carries no ownership marker to prove it is its own — refusing to touch it",
			vmID, node, existing.Name)
	}
	return false, fmt.Errorf(
		"proxmox: vmid %s already exists on %s as %q without this clone's ownership marker — another run or person made it; refusing to touch it",
		vmID, node, existing.Name)
}

// GuestPower is status/current, reduced to what a power operation waits for.
type GuestPower struct {
	Status    string `json:"status"`    // "running" or "stopped"
	QMPStatus string `json:"qmpstatus"` // qemu only: "running", "paused", …
}

// State folds the two fields into the one word a power operation waits for.
//
// A suspended VM is still "running" to Proxmox (its process is alive) and
// only QEMU's own status says it is paused. A container has no QMP status, so
// from this endpoint a suspended container cannot be told from a running one.
func (g GuestPower) State() string {
	if g.Status == "running" && g.QMPStatus == "paused" {
		return "paused"
	}
	return g.Status
}

// SettlePolls bounds how many times a guest's state is read after its task
// has finished, waiting for the state the task was for. The task is the long
// wait; this bound is for a cluster that is slow to report its outcome, and
// at the production poll interval it is thirty seconds.
const SettlePolls = 15

// ReadPower is what Proxmox says a guest is doing right now.
func (c *Client) ReadPower(ctx context.Context, node, kind, vmID string) (GuestPower, error) {
	var st GuestPower
	err := c.Get(ctx, fmt.Sprintf("/nodes/%s/%s/%s/status/current", node, kind, vmID), &st)
	return st, err
}

// AwaitPower reads the guest until it is in want, at most SettlePolls times.
// A read that fails is not a verdict; only running out of reads is.
func (c *Client) AwaitPower(ctx context.Context, node, kind, vmID, want string) (string, bool) {
	last := "unknown"
	for i := 0; i < SettlePolls; i++ {
		if st, err := c.ReadPower(ctx, node, kind, vmID); err == nil {
			last = st.State()
			if last == want {
				return last, true
			}
		}
		if !SleepCtx(ctx, c.TaskPoll) {
			return last, false
		}
	}
	return last, false
}

// ClusterGuest is one row of /cluster/resources?type=vm: every guest the
// cluster has, on every node, with the fields a guard compares.
type ClusterGuest struct {
	VMID     int    `json:"vmid"`
	Name     string `json:"name"`
	Node     string `json:"node"`
	Type     string `json:"type"`
	Status   string `json:"status"`
	Template int    `json:"template"`
}

// FindGuest looks a VMID up across the whole cluster, or returns nil when no
// guest has it.
//
// The cluster listing rather than the guest's own status endpoint, because
// that endpoint answers a missing VMID with a 500 whose only distinguishing
// feature is its wording, and "the machine is gone" is not something to decide
// by matching an error string.
func (c *Client) FindGuest(ctx context.Context, vmID string) (*ClusterGuest, error) {
	var guests []ClusterGuest
	if err := c.Get(ctx, "/cluster/resources?type=vm", &guests); err != nil {
		return nil, err
	}
	for i := range guests {
		if strconv.Itoa(guests[i].VMID) == vmID {
			return &guests[i], nil
		}
	}
	return nil, nil
}

// AwaitAbsent reads the cluster listing until the VMID is gone, at most
// SettlePolls times. A VMID that another guest has taken in the meantime is
// not absent, which is the answer a delete must give.
func (c *Client) AwaitAbsent(ctx context.Context, vmID string) (bool, error) {
	var lastErr error
	for i := 0; i < SettlePolls; i++ {
		g, err := c.FindGuest(ctx, vmID)
		if err == nil && g == nil {
			return true, nil
		}
		lastErr = err
		if !SleepCtx(ctx, c.TaskPoll) {
			break
		}
	}
	return false, lastErr
}

// GuestSMBIOSUUID is the UUID a VM's firmware presents to its guest: the one
// value here chosen before the machine existed and never changed by renaming
// it, moving it, or giving its VMID to somebody else.
func (c *Client) GuestSMBIOSUUID(ctx context.Context, node, vmID string) (string, error) {
	var config map[string]json.RawMessage
	if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/config", node, vmID), &config); err != nil {
		return "", err
	}
	return KVField(ConfigString(config, "smbios1"), "uuid"), nil
}

// SameUUID compares in every byte order a UUID is reported in.
func SameUUID(got string, wantForms []string) bool {
	for _, g := range provider.UUIDForms(got) {
		for _, w := range wantForms {
			if g == w {
				return true
			}
		}
	}
	return false
}

// ConfigString reads one key of a guest config as a string, whether the API
// sent it as a string or a number.
func ConfigString(config map[string]json.RawMessage, key string) string {
	raw, ok := config[key]
	if !ok {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var n json.Number
	if json.Unmarshal(raw, &n) == nil {
		return n.String()
	}
	return ""
}

// KVField reads one key out of PVE's "k=v,k=v" option strings.
func KVField(s, key string) string {
	for _, part := range strings.Split(s, ",") {
		if kv := strings.SplitN(part, "=", 2); len(kv) == 2 && kv[0] == key {
			return kv[1]
		}
	}
	return ""
}

// ParseSize reads PVE's "32G" / "512M" / "1T" disk sizes into bytes.
func ParseSize(s string) uint64 {
	if s == "" {
		return 0
	}
	mult := uint64(1)
	switch s[len(s)-1] {
	case 'K':
		mult = 1 << 10
	case 'M':
		mult = 1 << 20
	case 'G':
		mult = 1 << 30
	case 'T':
		mult = 1 << 40
	}
	if mult > 1 {
		s = s[:len(s)-1]
	}
	n, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0
	}
	return n * mult
}

// strParam reads one loosely typed parameter as a string: a JSON number
// arrives as float64 and is written without a decimal point.
func strParam(p map[string]interface{}, key string) string {
	switch v := p[key].(type) {
	case nil:
		return ""
	case string:
		return v
	case float64:
		return strconv.FormatInt(int64(v), 10)
	default:
		return fmt.Sprintf("%v", v)
	}
}
