package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/hazame-hub/accretion/provider"
)

// The write half's core: clone, power, delete and seed removal, moved from
// Sybil Continuum's typed actions (vm_clone, vm_start…, vm_delete,
// cidata_remove) with their bodies unchanged. They take the actions' loosely
// typed params and answer with a message and the result data instead of an
// integrations.ActionResult, so Continuum's actions and Accretion's own
// verbs run one implementation.

// answer is a successful outcome: the message and the result data.
func answer(msg string, data map[string]interface{}) (string, map[string]interface{}, error) {
	return msg, data, nil
}

// PlainFilename is the plain-name rule: no directory separators, so a filename
// cannot place the download anywhere but the storage's own directory.
var PlainFilename = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// PowerOutcome is what a power action answers. OK false with a nil error is
// a refusal or a failure Proxmox reported, with Message saying which: the
// power actions' contract, kept.
type PowerOutcome struct {
	OK      bool
	Message string
	Data    map[string]interface{}
	// Unknown is a failure whose task was not seen to end (its wait ran
	// out, or its status could not be read): the guest may yet get there.
	// Not in Data, so an action's answer does not change.
	Unknown bool
}

// BoolParam reads a boolean parameter, as a bool or a string strconv can
// parse; anything else is def.
func BoolParam(p map[string]interface{}, key string, def bool) bool {
	switch v := p[key].(type) {
	case bool:
		return v
	case string:
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}

// TimeoutParam reads "timeout_s", bounded so a typo cannot park a runbook
// step for a day.
func TimeoutParam(p map[string]interface{}, def time.Duration) time.Duration {
	if v, ok := p["timeout_s"].(float64); ok && v > 0 {
		d := time.Duration(v) * time.Second
		if d > 30*time.Minute {
			d = 30 * time.Minute
		}
		return d
	}
	return def
}

// GuestType reads the qemu/lxc parameter.
//
// Defaulted to qemu rather than required, because every existing runbook calls
// these actions without it and a required field would break all of them. The
// default is what they did unconditionally before: every config, resize and
// delete was hardcoded to /qemu/, so a container could be started and stopped
// and nothing else — including being attached to a network, which is
// vm_set_config net0 and therefore was impossible.
func GuestType(p map[string]interface{}) (string, error) {
	switch v := strParam(p, "type"); v {
	case "", "qemu":
		return "qemu", nil
	case "lxc":
		return "lxc", nil
	default:
		return "", fmt.Errorf("proxmox: type must be qemu or lxc, got %q", v)
	}
}

func mustInt(s string) int {
	n, _ := strconv.Atoi(s)
	return n
}

// CloneGuest is vm_clone: it clones a template and waits for the clone task. It is written to be
// retried, and a retry has to tell its own earlier attempt from somebody
// else's machine.
//
// A name and a VMID are not ownership. /cluster/nextid suggests an id, it does
// not reserve one: on PVE 9.1.6 two provisioning runs for web-01 were both
// given 104, the first cloned it, and the second found "vm 104 (web-01)
// already exists", took it for its own finished clone, and went on to
// configure and start the other run's machine.
//
// So the clone writes an ownership marker — its description — in the same
// request that creates the machine, and an existing VM is this attempt's only
// when it carries exactly that marker. Without a marker nothing can be proved
// and the existing VM is refused, whatever it is called. The SMBIOS uuid is
// not used here: it is written by the configure step after the clone, so a
// crash between the two would make a legitimate retry fail its own check.
func (c *Client) CloneGuest(ctx context.Context, p map[string]interface{}) (string, map[string]interface{}, error) {
	node := strParam(p, "node")
	template := strParam(p, "template_vmid")
	newID := strParam(p, "vmid")
	name := strParam(p, "name")
	if node == "" || template == "" || newID == "" || name == "" {
		return "", nil, fmt.Errorf("proxmox: vm_clone requires node, template_vmid, vmid, name")
	}
	placeNode := strParam(p, "target_node")
	if placeNode == "" {
		placeNode = node
	}

	done, err := c.CheckCloneTarget(ctx, placeNode, newID, name, strParam(p, "description"))
	if err != nil {
		return "", nil, err
	}
	if done {
		return answer(fmt.Sprintf("vm %s (%s) already exists on %s carrying this clone's marker — treating as done", newID, name, placeNode),
			map[string]interface{}{"vmid": mustInt(newID), "name": name, "node": placeNode})
	}

	form := url.Values{"newid": {newID}, "name": {name}}
	if BoolParam(p, "full", true) {
		form.Set("full", "1")
	}
	if s := strParam(p, "storage"); s != "" {
		form.Set("storage", s)
	}
	// Written by the clone itself, so the marker is on the machine from the
	// first moment it exists — before any later step can fail and leave it
	// behind unlabelled.
	if d := strParam(p, "description"); d != "" {
		form.Set("description", d)
	}
	if t := strParam(p, "target_node"); t != "" {
		form.Set("target", t)
	}
	// Placed in a resource pool by the clone itself. A token whose rights are
	// granted on a pool can only create a VM there: without `pool` Proxmox
	// checks VM.Allocate on /vms/<newid>, which such a token does not have.
	if pool := strings.TrimSpace(strParam(p, "pool")); pool != "" {
		form.Set("pool", pool)
	}
	data, err := c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%s/clone", node, template), form)
	if err != nil {
		return "", nil, err
	}
	if err := c.WaitIfTask(ctx, node, data, TimeoutParam(p, 5*time.Minute)); err != nil {
		return "", nil, err
	}
	return answer(fmt.Sprintf("cloned template %s to vm %s (%s) on %s", template, newID, name, placeNode),
		map[string]interface{}{"vmid": mustInt(newID), "name": name, "node": placeNode})
}

type powerOp struct {
	endpoint string
	// want is the state the guest must be seen in before the operation reports
	// success.
	want string
	// reboots marks the operations whose end state is also their start state,
	// so a guest already in it has not thereby been rebooted.
	reboots bool
	// refuse names the states in which the operation cannot do what it is
	// asked, and says why. These are refused before anything is sent.
	refuse map[string]string
	wait   time.Duration
}

var rebootRefusals = map[string]string{
	"stopped": "is stopped — a reboot does not start a stopped guest; start it instead",
	"paused":  "is suspended — a suspended guest cannot reboot; stop it first",
}

var powerOps = map[string]powerOp{
	"vm_start": {endpoint: "start", want: "running", wait: 2 * time.Minute, refuse: map[string]string{
		"paused": "is suspended — start does not resume a suspended guest; resume it in Proxmox, or stop it first",
	}},
	"vm_stop": {endpoint: "stop", want: "stopped", wait: 2 * time.Minute},
	"vm_shutdown": {endpoint: "shutdown", want: "stopped", wait: 5 * time.Minute, refuse: map[string]string{
		"paused": "is suspended — a suspended guest cannot answer a shutdown request; stop it instead",
	}},
	"vm_reboot":  {endpoint: "reboot", want: "running", reboots: true, wait: 5 * time.Minute, refuse: rebootRefusals},
	"vm_restart": {endpoint: "reboot", want: "running", reboots: true, wait: 5 * time.Minute, refuse: rebootRefusals},
	"vm_suspend": {endpoint: "suspend", want: "paused", wait: 2 * time.Minute, refuse: map[string]string{
		"stopped": "is stopped — there is nothing to suspend",
	}},
}

// PowerGuest is the power actions (vm_start, vm_stop, …). It keeps the original power-op contract — an API refusal is a
// Success:false result carrying the API's message, not an error — and adds
// what that contract never promised: the task is waited for, the resulting
// state is observed, and a guest already where it was sent is done.
func (c *Client) PowerGuest(ctx context.Context, actionType string, p map[string]interface{}) (PowerOutcome, error) {
	op, ok := powerOps[actionType]
	if !ok {
		return PowerOutcome{}, fmt.Errorf("proxmox: unsupported action: %s", actionType)
	}
	node := strParam(p, "node")
	vmType := strParam(p, "type") // "qemu" or "lxc"
	vmID := strParam(p, "vmid")
	if node == "" || vmType == "" || vmID == "" {
		return PowerOutcome{}, fmt.Errorf("proxmox: %s requires node, type, vmid", actionType)
	}
	if vmType != "qemu" && vmType != "lxc" {
		return PowerOutcome{}, fmt.Errorf("proxmox: type must be qemu or lxc, got %q", vmType)
	}
	target := fmt.Sprintf("%s/%s/%s", node, vmType, vmID)
	data := map[string]interface{}{"node": node, "type": vmType, "vmid": vmID}
	failed := func(msg string) (PowerOutcome, error) {
		return PowerOutcome{Message: msg, Data: data}, nil
	}

	want := op.want
	if op.endpoint == "suspend" && vmType == "lxc" {
		// A frozen container reports "running", so no state confirms it.
		want = ""
	}

	// Before sending anything: a guest already where it was sent needs
	// nothing, and a guest in a state the operation cannot act on is told so
	// rather than handed to a task that will fail. A guest whose state cannot
	// be read is still sent the request — the API's answer is a better error
	// than a guess made here.
	if before, err := c.ReadPower(ctx, node, vmType, vmID); err == nil {
		state := before.State()
		data["status"] = state
		if why, refused := op.refuse[state]; refused {
			return failed(fmt.Sprintf("%s %s", target, why))
		}
		if !op.reboots && want != "" && state == want {
			return PowerOutcome{OK: true, Message: fmt.Sprintf("%s is already %s", target, state), Data: data}, nil
		}
	}

	form := url.Values{}
	if actionType == "vm_shutdown" && BoolParam(p, "force_stop", false) {
		form.Set("forceStop", "1")
	}
	resp, err := c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/%s/%s/status/%s", node, vmType, vmID, op.endpoint), form)
	if err != nil {
		var ae *APIError
		if !errors.As(err, &ae) {
			return PowerOutcome{}, err
		}
		// A refusal can be a race lost to somebody who got there first.
		if c.isIn(ctx, node, vmType, vmID, op, want, data) {
			return PowerOutcome{OK: true, Message: fmt.Sprintf("%s is %s", target, want), Data: data}, nil
		}
		out, ferr := failed(ae.Body)
		out.Unknown = ae.Code > 500 // a gateway's answer: the node may carry on
		return out, ferr
	}
	if upid, isTask := AsUPID(resp); isTask {
		data["upid"] = upid
	}

	if err := c.WaitIfTask(ctx, node, resp, TimeoutParam(p, op.wait)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return PowerOutcome{}, ctxErr
		}
		var timeout *TaskTimeoutError
		if errors.As(err, &timeout) {
			data["timed_out"] = true
		}
		// Either way the guest may have got there — a task that timed out may
		// have finished since, and one that failed may have lost a race to
		// somebody doing the same thing.
		if c.isIn(ctx, node, vmType, vmID, op, want, data) {
			return PowerOutcome{OK: true, Message: fmt.Sprintf("%s is %s", target, want), Data: data}, nil
		}
		out, ferr := failed(err.Error())
		var unreadable *TaskUnreadableError
		out.Unknown = errors.As(err, &timeout) || errors.As(err, &unreadable)
		return out, ferr
	}

	if want == "" {
		return PowerOutcome{OK: true, Message: fmt.Sprintf("%s finished on %s — Proxmox reports no state that confirms it for a %s",
			op.endpoint, target, vmType), Data: data}, nil
	}
	state, observed := c.AwaitPower(ctx, node, vmType, vmID, want)
	data["status"] = state
	if !observed {
		return failed(fmt.Sprintf("the %s task on %s finished, but the guest is %s, not %s", op.endpoint, target, state, want))
	}
	return PowerOutcome{OK: true, Message: fmt.Sprintf("%s finished on %s; it is %s", op.endpoint, target, want), Data: data}, nil
}

// isIn reads the guest once and reports whether it is already in the state an
// operation was for. Never true for a reboot: running proves nothing about
// whether it rebooted.
func (c *Client) isIn(ctx context.Context, node, kind, vmID string, op powerOp, want string, data map[string]interface{}) bool {
	if op.reboots || want == "" {
		return false
	}
	st, err := c.ReadPower(ctx, node, kind, vmID)
	if err != nil {
		return false
	}
	data["status"] = st.State()
	return st.State() == want
}

// DeleteGuest is vm_delete: it destroys a guest. It demands confirm=true in its own params —
// defense in depth under whatever confirmation the caller's UI already did,
// because this is the one action here that cannot be retried out of.
//
// What it will delete is the guest the caller described, and only that guest.
// The VMID says where to look; node and type must match; name, when given,
// must match; and expected_uuid, when given, must be the SMBIOS UUID the VM
// carries. That last one is the guard that means something: a VMID is reused
// the moment its guest is gone, and a name is changed by anyone with a
// keyboard, but the UUID was chosen when the machine was made.
//
// It is written to be retried, and to be right about what happened when an
// answer is lost. A guest already gone has been deleted. A delete whose
// request errored or whose task outlived the wait is judged by the cluster
// listing afterwards, not by the error. A destroy task that finished is not
// success until the listing agrees.
//
// What no guard here can close is the moment between reading the guest and
// sending the DELETE: Proxmox takes no precondition on a destroy. The guest
// must be stopped and match every guard milliseconds before, and a VMID is not
// handed out again while its guest exists, so replacing it in that window
// means deleting the original first.
func (c *Client) DeleteGuest(ctx context.Context, p map[string]interface{}) (string, map[string]interface{}, error) {
	if !BoolParam(p, "confirm", false) {
		return "", nil, fmt.Errorf("proxmox: vm_delete requires confirm=true — it destroys the VM and its disks")
	}
	node := strParam(p, "node")
	vmID := strParam(p, "vmid")
	if node == "" || vmID == "" {
		return "", nil, fmt.Errorf("proxmox: vm_delete requires node, vmid")
	}
	kind, err := GuestType(p)
	if err != nil {
		return "", nil, err
	}
	target := fmt.Sprintf("%s/%s/%s", node, kind, vmID)
	data := map[string]interface{}{"node": node, "type": kind, "vmid": vmID}
	done := func(outcome, msg string) (string, map[string]interface{}, error) {
		data["deleted"] = true
		data["outcome"] = outcome
		// The provider ref that no longer exists, in the same form the
		// realization recorded it, so the engine can retire that record
		// without knowing how Proxmox spells a guest.
		data["removed_ref"] = target
		return msg, data, nil
	}

	var wantUUID []string
	if raw := strings.TrimSpace(strParam(p, "expected_uuid")); raw != "" {
		if kind != "qemu" {
			return "", nil, fmt.Errorf(
				"proxmox: vm_delete: expected_uuid needs a qemu guest — a container has no SMBIOS identity to check")
		}
		if wantUUID = provider.UUIDForms(raw); wantUUID == nil {
			return "", nil, fmt.Errorf(
				"proxmox: vm_delete: expected_uuid %q is not a usable UUID — refusing rather than deleting unguarded", raw)
		}
	}

	guest, err := c.FindGuest(ctx, vmID)
	if err != nil {
		return "", nil, fmt.Errorf("proxmox: vm_delete: look up %s: %w", target, err)
	}
	if guest == nil {
		return done("already_absent", fmt.Sprintf("%s does not exist — treating as deleted", target))
	}
	if guest.Node != node || guest.Type != kind {
		return "", nil, fmt.Errorf(
			"proxmox: vmid %s is a %s on %s, not a %s on %s — refusing to delete it", vmID, guest.Type, guest.Node, kind, node)
	}
	// The cluster listing names a guest only once pvestatd has reported it,
	// several seconds after it was created. Found on flk-vega-01: a rollback
	// right after a start failed read the name as "" and refused to delete
	// the guest it had just made. The config is authoritative and immediate.
	if want := strParam(p, "name"); want != "" && guest.Name == "" {
		var cfg struct {
			Name string `json:"name"`
		}
		if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/%s/%s/config", node, kind, vmID), &cfg); err == nil {
			guest.Name = cfg.Name
		}
	}
	if want := strParam(p, "name"); want != "" && guest.Name != want {
		return "", nil, fmt.Errorf(
			"proxmox: vmid %s is %q, not %q — refusing to delete a guest this action was not asked about", vmID, guest.Name, want)
	}
	if wantUUID != nil {
		got, err := c.GuestSMBIOSUUID(ctx, node, vmID)
		if err != nil {
			return "", nil, fmt.Errorf(
				"proxmox: cannot read the SMBIOS UUID of %s to confirm it is the guest meant — refusing: %w", target, err)
		}
		if !SameUUID(got, wantUUID) {
			if got == "" {
				got = "none"
			}
			return "", nil, fmt.Errorf(
				"proxmox: vmid %s carries SMBIOS UUID %s, not %s — refusing to delete a guest this action was not asked about",
				vmID, got, wantUUID[0])
		}
	}
	// The cluster listing is a cache that trails the node by several seconds:
	// on PVE 9.1.6 a rollback stopped a guest, saw the stop task finish, and
	// was then refused here because /cluster/resources still said running. So
	// the listing only raises the question; the node's own status answers it.
	if guest.Status == "running" {
		st, err := c.ReadPower(ctx, node, kind, vmID)
		if err != nil {
			return "", nil, fmt.Errorf(
				"proxmox: %s is listed as running and its status could not be read — refusing: %w", target, err)
		}
		if st.Status != "stopped" {
			return "", nil, fmt.Errorf(
				"proxmox: %s is running — stop or shut it down first; a running guest is not deleted", target)
		}
	}

	// purge takes the guest out of backup jobs, replication and HA with it;
	// destroy-unreferenced-disks removes disks carrying its VMID that its
	// config no longer names. Without them a deleted machine leaves entries
	// that name a VMID the next clone may be given.
	resp, err := c.Call(ctx, http.MethodDelete,
		fmt.Sprintf("/nodes/%s/%s/%s?purge=1&destroy-unreferenced-disks=1", node, kind, vmID), nil)
	if err != nil {
		var ae *APIError
		if errors.As(err, &ae) {
			// A refusal may be the guest having gone in the moment since it
			// was read. One look says whether it is gone; nothing was started.
			if g, lookErr := c.FindGuest(ctx, vmID); lookErr == nil && g == nil {
				return done("already_absent", fmt.Sprintf("%s was gone by the time it was deleted", target))
			}
			return "", nil, fmt.Errorf("proxmox: delete %s: %w", target, err)
		}
		// No answer at all: the request may have been carried out.
		if gone, _ := c.AwaitAbsent(ctx, vmID); gone {
			return done("deleted", fmt.Sprintf("deleted %s — its answer was lost, and the cluster no longer lists it", target))
		}
		return "", nil, fmt.Errorf("proxmox: delete %s: %w", target, err)
	}
	if upid, isTask := AsUPID(resp); isTask {
		data["upid"] = upid
	}
	if err := c.WaitIfTask(ctx, node, resp, TimeoutParam(p, 5*time.Minute)); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return "", nil, ctxErr
		}
		if gone, _ := c.AwaitAbsent(ctx, vmID); gone {
			return done("deleted", fmt.Sprintf("deleted %s — the cluster no longer lists it", target))
		}
		return "", nil, fmt.Errorf("proxmox: delete %s: %w", target, err)
	}
	gone, lookErr := c.AwaitAbsent(ctx, vmID)
	if !gone {
		return "", nil, fmt.Errorf(
			"proxmox: the destroy task for %s finished, but the cluster still lists vmid %s (last look: %v)", target, vmID, lookErr)
	}
	return done("deleted", fmt.Sprintf("deleted %s", target))
}

// RemoveSeed is cidata_remove: it ejects a cloud-init seed from the machine it was built for and
// deletes it from the storage.
//
// The seed carries an enrollment token in clear. Once the machine has enrolled,
// or its build was rolled back, nothing will read it again, and leaving it
// would leave a credential on the hypervisor for as long as the storage lives.
// It is ejected first because Proxmox will not start a guest whose CD-ROM
// names a volume that no longer exists.
//
// Idempotent: a guest that is gone is not ejected, and a seed that is not there
// is already deleted.
func (c *Client) RemoveSeed(ctx context.Context, p map[string]interface{}) (string, map[string]interface{}, error) {
	node := strParam(p, "node")
	storage := strParam(p, "storage")
	filename := strParam(p, "filename")
	vmID := strParam(p, "vmid")
	if node == "" || storage == "" || filename == "" {
		return "", nil, fmt.Errorf("proxmox: cidata_remove requires node, storage, filename")
	}
	if !PlainFilename.MatchString(filename) || !strings.HasSuffix(filename, ".iso") {
		return "", nil, fmt.Errorf("proxmox: cidata_remove: %q is not a seed filename", filename)
	}
	volume := fmt.Sprintf("%s:iso/%s", storage, filename)
	data := map[string]interface{}{"volume": volume, "ejected": false, "deleted": false}
	timeout := TimeoutParam(p, 2*time.Minute)

	if vmID != "" {
		guest, err := c.FindGuest(ctx, vmID)
		if err != nil {
			return "", nil, fmt.Errorf("proxmox: cidata_remove: look up vm %s: %w", vmID, err)
		}
		if guest != nil && guest.Type == "qemu" {
			var cfg map[string]interface{}
			if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/qemu/%s/config", guest.Node, vmID), &cfg); err != nil {
				return "", nil, err
			}
			for key, v := range cfg {
				line, _ := v.(string)
				if !isDriveKey(key) || !strings.HasPrefix(line, volume+",") && line != volume {
					continue
				}
				resp, err := c.Call(ctx, http.MethodPost, fmt.Sprintf("/nodes/%s/qemu/%s/config", guest.Node, vmID),
					url.Values{key: {"none,media=cdrom"}})
				if err != nil {
					return "", nil, err
				}
				if err := c.WaitIfTask(ctx, guest.Node, resp, timeout); err != nil {
					return "", nil, err
				}
				data["ejected"] = true
			}
		}
	}

	var content []struct {
		VolID string `json:"volid"`
	}
	if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/storage/%s/content?content=iso", node, storage), &content); err != nil {
		return "", nil, err
	}
	present := false
	for _, c := range content {
		if c.VolID == volume {
			present = true
			break
		}
	}
	if !present {
		return fmt.Sprintf("%s is not on %s — treating as deleted", volume, node), data, nil
	}
	resp, err := c.Call(ctx, http.MethodDelete,
		fmt.Sprintf("/nodes/%s/storage/%s/content/%s", node, storage, url.PathEscape(volume)), nil)
	if err != nil {
		return "", nil, err
	}
	if err := c.WaitIfTask(ctx, node, resp, timeout); err != nil {
		return "", nil, err
	}
	data["deleted"] = true
	return fmt.Sprintf("removed cloud-init seed %s from %s on %s", filename, storage, node), data, nil
}

// isDriveKey reports whether a config key names a drive slot a seed could be
// attached to.
func isDriveKey(key string) bool {
	for _, bus := range []string{"ide", "sata", "scsi"} {
		if strings.HasPrefix(key, bus) && len(key) > len(bus) && key[len(bus)] >= '0' && key[len(bus)] <= '9' {
			return true
		}
	}
	return false
}
