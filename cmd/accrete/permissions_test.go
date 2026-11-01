package main

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/hazame-hub/accretion/provider"
)

// fullGrants is what a token needs for applyManifest on pvetest: template
// 9000 on pve1, its disks on local-lvm, seeds on local, net0 on vmbr0.
func fullGrants() map[string][]string {
	return map[string][]string{
		"/vms/9000": {"VM.Audit", "VM.Clone"},
		"/vms": {"VM.Allocate", "VM.Audit", "VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Disk", "VM.Config.HWType",
			"VM.Config.Memory", "VM.Config.Network", "VM.Config.Options", "VM.PowerMgmt", "VM.GuestAgent.Audit"},
		"/storage/local":                {"Datastore.Allocate", "Datastore.AllocateTemplate", "Datastore.Audit"},
		"/storage/local-lvm":            {"Datastore.AllocateSpace", "Datastore.Audit"},
		"/nodes/pve1":                   {"Sys.Audit"},
		"/sdn/zones/localnetwork/vmbr0": {"SDN.Use"},
	}
}

func (l *applyLab) onlyGETs() {
	l.t.Helper()
	for _, r := range l.pve.Requests {
		if !strings.HasPrefix(r, "GET ") {
			l.t.Errorf("permissions sent %s; it may only read", r)
		}
	}
}

func TestPermissionsNamesEveryPathAndFindsNothingMissing(t *testing.T) {
	l := newApplyLab(t, "")
	l.pve.Privileges = fullGrants()
	out, code := l.accrete("", "permissions")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{
		"Role accrete-lab needs",
		"/vms/9000", "VM.Clone",
		"/storage/local-lvm", "Datastore.AllocateSpace",
		"/storage/local ", "Datastore.AllocateTemplate",
		"/nodes/pve1", "Sys.Audit",
		"/sdn/zones/localnetwork/vmbr0", "SDN.Use",
		"The token has every privilege an apply needs.",
		"pveum role add accrete-lab",
		"pveum acl modify /vms --tokens '<user@realm!tokenid>' --roles accrete-lab",
		"Datacenter → Permissions → Roles → Create",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "acl modify /vms/9000") {
		t.Errorf("/vms/9000 is granted again although the role on /vms reaches it:\n%s", out)
	}
	l.onlyGETs()
}

func TestPermissionsSaysWhatTheTokenLacksAndExitsOne(t *testing.T) {
	l := newApplyLab(t, "")
	g := fullGrants()
	g["/storage/local"] = []string{"Datastore.Audit"}
	delete(g, "/sdn/zones/localnetwork/vmbr0")
	l.pve.Privileges = g
	out, code := l.accrete("", "permissions")
	if code != 1 {
		t.Fatalf("exit %d, want 1:\n%s", code, out)
	}
	for _, want := range []string{"the token lacks Datastore.Allocate, Datastore.AllocateTemplate", "the token lacks SDN.Use", "apply would stop part way"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestPermissionsOptionalGuestAgentNeverFails(t *testing.T) {
	l := newApplyLab(t, "")
	g := fullGrants()
	g["/vms"] = g["/vms"][:len(g["/vms"])-1] // no VM.GuestAgent.Audit
	l.pve.Privileges = g
	out, code := l.accrete("", "permissions")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (the guest agent grant is optional):\n%s", code, out)
	}
	if !strings.Contains(out, "the token lacks VM.GuestAgent.Audit (optional)") {
		t.Errorf("the optional grant is not reported:\n%s", out)
	}
}

func TestPermissionsOfflineAsksNothing(t *testing.T) {
	l := newApplyLab(t, "")
	out, code := l.accrete("", "permissions", "--offline", "--role", "web-builders")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	if len(l.pve.Requests) != 0 {
		t.Errorf("--offline sent %v", l.pve.Requests)
	}
	for _, want := range []string{"Role web-builders needs", "not checked", "template 9000's disk storage was not read", "  /storage: "} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
}

func TestPermissionsJSON(t *testing.T) {
	l := newApplyLab(t, "")
	l.pve.Privileges = fullGrants()
	out, code := l.accrete("", "permissions", "-o", "json")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	var p provider.Permissions
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("%v\n%s", err, out)
	}
	if p.Role != "accrete-lab" || len(p.Grants) == 0 {
		t.Fatalf("role %q, %d grants", p.Role, len(p.Grants))
	}
	for _, priv := range p.Privileges {
		if priv == "VM.GuestAgent.Audit" {
			t.Error("an optional privilege is in the role's required list")
		}
	}
	for _, g := range p.Grants {
		if !g.Checked {
			t.Errorf("%s not checked: %s", g.Path, g.NotChecked)
		}
	}
}
