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
		"/vms": {"VM.Allocate", "VM.Audit", "VM.Config.CDROM", "VM.Config.CPU", "VM.Config.Cloudinit", "VM.Config.Disk", "VM.Config.HWType",
			"VM.Config.Memory", "VM.Config.Network", "VM.Config.Options", "VM.PowerMgmt", "VM.GuestAgent.Audit"},
		"/storage/local":                {"Datastore.Allocate", "Datastore.AllocateTemplate", "Datastore.Audit"},
		"/storage/local-lvm":            {"Datastore.AllocateSpace", "Datastore.Audit"},
		"/nodes/pve1":                   {"Sys.Audit"},
		"/sdn/zones/localnetwork/vmbr0": {"SDN.Use"},
		"/sdn":                          {"SDN.Audit"},
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
		"Optional: grant SDN.Audit on /sdn (let plan list the SDN zones and networks",
		"Optional: grant VM.GuestAgent.Audit on /vms",
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

func TestPermissionsOptionalSDNListingNeverFails(t *testing.T) {
	l := newApplyLab(t, "")
	g := fullGrants()
	delete(g, "/sdn")
	l.pve.Privileges = g
	out, code := l.accrete("", "permissions")
	if code != 0 {
		t.Fatalf("exit %d, want 0 (the SDN listing grant is optional):\n%s", code, out)
	}
	if !strings.Contains(out, "the token lacks SDN.Audit (optional)") {
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
		if priv == "VM.GuestAgent.Audit" || priv == "SDN.Audit" {
			t.Error("an optional privilege is in the role's required list")
		}
	}
	for _, g := range p.Grants {
		if !g.Checked {
			t.Errorf("%s not checked: %s", g.Path, g.NotChecked)
		}
	}
}

// poolGrants is what a token scoped to pool sybil-lab needs on pvetest: the
// machines' role on the pool, a clone-only role on the template.
func poolGrants() map[string][]string {
	g := fullGrants()
	g["/pool/sybil-lab"] = g["/vms"]
	delete(g, "/vms")
	return g
}

func TestPermissionsWithAPoolGrantOnThePoolAndACloneOnlyRoleOnTheTemplate(t *testing.T) {
	l := newApplyLab(t, "        pool: sybil-lab\n")
	l.pve.Privileges = poolGrants()
	out, code := l.accrete("", "permissions")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	for _, want := range []string{
		"/pool/sybil-lab: create, configure, start, stop and delete the new machines, which are cloned into pool sybil-lab",
		"pveum acl modify /pool/sybil-lab --tokens '<user@realm!tokenid>' --roles accrete-lab",
		"pveum role add accrete-labClone --privs \"VM.Audit,VM.Clone\"",
		"pveum acl modify /vms/9000 --tokens '<user@realm!tokenid>' --roles accrete-labClone --propagate 0",
		"(role accrete-labClone)",
		"pveum role add accrete-labDisk --privs \"Datastore.AllocateSpace,Datastore.Audit\"",
		"pveum acl modify /storage/local-lvm --tokens '<user@realm!tokenid>' --roles accrete-labDisk",
		"pveum role add accrete-labSeed --privs \"Datastore.Allocate,Datastore.AllocateTemplate,Datastore.Audit\"",
		"pveum acl modify /nodes/pve1 --tokens '<user@realm!tokenid>' --roles accrete-labNode",
		"pveum acl modify /sdn/zones/localnetwork/vmbr0 --tokens '<user@realm!tokenid>' --roles accrete-labNet",
		"Optional: grant VM.GuestAgent.Audit on /pool/sybil-lab",
		"The token has every privilege an apply needs.",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, bad := range []string{"acl modify /vms --tokens", "/vms: create"} {
		if strings.Contains(out, bad) {
			t.Errorf("a pool-scoped manifest still asks for %q:\n%s", bad, out)
		}
	}
	// The main role holds the machines' privileges only: no storage
	// privilege, or the token could delete any volume on the disk storage.
	if !strings.Contains(out, "pveum role add accrete-lab --privs \"VM.Allocate,") || strings.Contains(out, "accrete-lab --privs \"Datastore") {
		t.Errorf("main role:\n%s", out)
	}
	for _, line := range strings.Split(out, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "pveum role add accrete-lab --privs") && strings.Contains(line, "Datastore") {
			t.Errorf("main role carries a storage privilege: %s", line)
		}
	}
	l.onlyGETs()
}

func TestPermissionsWithAPoolNamesWhatAVmsOnlyTokenLacks(t *testing.T) {
	l := newApplyLab(t, "        pool: sybil-lab\n")
	l.pve.Privileges = fullGrants() // granted on /vms, not on the pool
	out, code := l.accrete("", "permissions")
	if code != 1 || !strings.Contains(out, "/pool/sybil-lab") || !strings.Contains(out, "the token lacks VM.Allocate") {
		t.Fatalf("exit %d, want 1 naming the pool:\n%s", code, out)
	}
}

func TestApplyClonesIntoTheDeclaredPool(t *testing.T) {
	l := newApplyLab(t, "        pool: sybil-lab\n")
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	n := 0
	for _, g := range l.pve.Guests {
		if name, _ := g.Config["name"].(string); g.Template || !strings.HasPrefix(name, "accrete-web-") {
			continue
		}
		n++
		if g.Pool != "sybil-lab" {
			t.Errorf("guest %d was cloned into pool %q, want sybil-lab", g.VMID, g.Pool)
		}
	}
	if n != 2 {
		t.Errorf("%d machines built, want 2", n)
	}
}
