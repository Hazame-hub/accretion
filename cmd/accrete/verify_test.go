package main

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

// bpgVM is a proxmox_virtual_environment_vm instance as bpg/proxmox writes
// it in a raw state, with a cloud-init password Terraform marks sensitive.
func bpgVM(vmid int, name string, cores, memMiB int, disks map[string]int, nics ...[2]string) map[string]any {
	var d []any
	for slot, gib := range disks {
		d = append(d, map[string]any{"interface": slot, "size": gib, "datastore_id": "local-lvm"})
	}
	var n []any
	for _, nic := range nics {
		n = append(n, map[string]any{"bridge": nic[0], "mac_address": nic[1], "enabled": true, "disconnected": false, "vlan_id": 0, "model": "virtio"})
	}
	return map[string]any{
		"index_key": name, "schema_version": 0,
		"attributes": map[string]any{
			"vm_id": vmid, "name": name, "node_name": "pve1", "started": false, "template": false, "smbios": []any{},
			"cpu": []any{map[string]any{"cores": cores, "sockets": 1}}, "memory": []any{map[string]any{"dedicated": memMiB}},
			"disk": d, "network_device": n,
			"initialization": []any{map[string]any{"user_account": []any{map[string]any{"username": "ops", "password": "tf-sensitive-pw"}}}},
		},
		"sensitive_attributes": []any{[]any{
			map[string]any{"type": "get_attr", "value": "initialization"},
			map[string]any{"type": "index", "value": map[string]any{"value": 0, "type": "number"}},
			map[string]any{"type": "get_attr", "value": "user_account"},
			map[string]any{"type": "index", "value": map[string]any{"value": 0, "type": "number"}},
			map[string]any{"type": "get_attr", "value": "password"},
		}},
	}
}

// verifyLab is a Terraform-managed fleet as homelab-gen keeps it: VMs in a
// for_each module, an SDN module, and a resource type accrete does not map.
//   - web-01 (201) is as the state records it;
//   - mgmt-01 (202) got a second card and two more cores outside Terraform,
//     the cores by hamza@pve (a task), the card through the API (no task);
//   - gone-01 (203) is in the state but not listed by the provider;
//   - db-01 (101, pvetest's own) and the vnet vextra are not in the state.
func verifyLab(t *testing.T) (*pvetest.Server, string) {
	t.Helper()
	pve := pvetest.New(t)
	pve.Guests[201] = &pvetest.Guest{VMID: 201, Kind: "qemu", Status: "running", Config: map[string]any{
		"name": "web-01", "smbios1": "uuid=22222222-0000-4000-8000-000000000201", "cores": 2, "memory": 4096,
		"net0": "virtio=BC:24:11:00:02:01,bridge=vweb", "scsi0": "local-lvm:vm-201-disk-0,size=20G",
		"scsi1": "local-lvm:vm-201-disk-1,size=50G", "ide0": "local:iso/rhel.iso,media=cdrom", "efidisk0": "local-lvm:vm-201-disk-2,size=4M",
	}}
	pve.Guests[202] = &pvetest.Guest{VMID: 202, Kind: "qemu", Status: "running", Config: map[string]any{
		"name": "mgmt-01", "smbios1": "uuid=22222222-0000-4000-8000-000000000202", "cores": 4, "memory": 4096,
		"net0": "virtio=BC:24:11:00:02:02,bridge=vmgmt", "net1": "virtio=BC:24:11:00:02:12,bridge=vweb,tag=30",
		"scsi0": "local-lvm:vm-202-disk-0,size=20G",
	}}
	pve.Vnets = map[string]string{"vmgmt": "mgmt", "vweb": "web", "vextra": "extra"}
	pve.AddTask(pvetest.Task{Type: "qmconfig", User: "hamza@pve", VMID: 202, Start: time.Date(2026, 11, 8, 9, 30, 0, 0, time.UTC)})

	module := func(name string, insts ...map[string]any) map[string]any {
		return map[string]any{"module": `module.vms["` + name + `"]`, "mode": "managed", "type": "proxmox_virtual_environment_vm", "name": "this",
			"provider": `provider["registry.terraform.io/bpg/proxmox"]`, "instances": insts}
	}
	strip := func(m map[string]any) map[string]any { delete(m, "index_key"); return m }
	state := map[string]any{
		"version": 4, "terraform_version": "1.14.8", "serial": 1488, "lineage": "x",
		"resources": []any{
			module("web-01", strip(bpgVM(201, "web-01", 2, 4096, map[string]int{"scsi0": 20, "scsi1": 50}, [2]string{"vweb", "BC:24:11:00:02:01"}))),
			module("mgmt-01", strip(bpgVM(202, "mgmt-01", 2, 4096, map[string]int{"scsi0": 20}, [2]string{"vmgmt", "BC:24:11:00:02:02"}))),
			module("gone-01", strip(bpgVM(203, "gone-01", 1, 1024, map[string]int{"scsi0": 10}, [2]string{"vweb", "BC:24:11:00:02:03"}))),
			map[string]any{"module": "module.sdn", "mode": "managed", "type": "proxmox_sdn_vnet", "name": "vnets",
				"provider": `provider["registry.terraform.io/bpg/proxmox"]`, "instances": []any{
					map[string]any{"index_key": "mgmt", "attributes": map[string]any{"id": "vmgmt", "zone": "mgmt"}},
					map[string]any{"index_key": "web", "attributes": map[string]any{"id": "vweb", "zone": "web"}},
				}},
			map[string]any{"module": "module.sdn", "mode": "managed", "type": "proxmox_sdn_zone_simple", "name": "zones",
				"provider": `provider["registry.terraform.io/bpg/proxmox"]`, "instances": []any{
					map[string]any{"index_key": "mgmt", "attributes": map[string]any{"id": "mgmt"}},
				}},
		},
	}
	dir := t.TempDir()
	b, _ := json.Marshal(state)
	if err := os.WriteFile(filepath.Join(dir, "terraform.tfstate"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	return pve, dir
}

func accreteVerify(t *testing.T, pve *pvetest.Server, stdin string, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	args = append([]string{"verify", "--provider", "proxmox", "--endpoint", pve.URL, "--token-env", applyTokenVar, "--verify-tls=false"}, args...)
	code := run(context.Background(), args, strings.NewReader(stdin), &out, &out, func(k string) string {
		if k == applyTokenVar {
			return pvetest.Token
		}
		return ""
	})
	for _, secret := range []string{pvetest.Secret, "tf-sensitive-pw"} {
		if strings.Contains(out.String(), secret) {
			t.Fatalf("a secret reached the output:\n%s", out.String())
		}
	}
	if w := pve.Writes(); len(w) != 0 {
		t.Fatalf("verify wrote to the provider: %v", w)
	}
	return out.String(), code
}

func TestVerifySaysWhatTheTerraformStateGetsWrong(t *testing.T) {
	pve, dir := verifyLab(t)
	before, _ := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
	out, code := accreteVerify(t, pve, "", "--terraform-state", dir)
	if code != 1 {
		t.Errorf("exit %d, want 1 (something differs)", code)
	}
	for _, want := range []string{
		"(raw-state, serial 1488, written by Terraform 1.14.8)",
		`  matches  module.vms["web-01"].proxmox_virtual_environment_vm.this  (pve1/qemu/201)`,
		`  differs  module.vms["mgmt-01"].proxmox_virtual_environment_vm.this  (pve1/qemu/202)`,
		"cores: the state records 2, the provider reports 4",
		"net1 is on the machine, not in the state (bridge=vweb, link=up, mac=bc:24:11:00:02:12, vlan=30)",
		"changed by hamza@pve at 2026-11-08T09:30:00Z (qmconfig)",
		`  unknown  module.vms["gone-01"].proxmox_virtual_environment_vm.this  the provider lists no vm qemu/203; it may be gone, or the token may not see it`,
		`  matches  module.sdn.proxmox_sdn_vnet.vnets["mgmt"]  (vmgmt)`,
		"not in the Terraform state:",
		"  vm       db-01  (pve1/qemu/101)",
		"  network  vextra  (vextra)",
		"not verified (accrete does not map these types): proxmox_sdn_zone_simple (1)",
		"note: power is not compared",
		"3 match, 1 differ, 1 unknown, 2 not in the state, 1 not verified",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// The template is no stranger: Terraform not recording it says nothing.
	if strings.Contains(out, "rocky-9-cloud") {
		t.Errorf("the template was listed as not in the state:\n%s", out)
	}
	after, _ := os.ReadFile(filepath.Join(dir, "terraform.tfstate"))
	if !bytes.Equal(before, after) {
		t.Error("verify changed the Terraform state")
	}
	if entries, _ := os.ReadDir(dir); len(entries) != 1 {
		t.Errorf("verify wrote files beside the state: %v", entries)
	}
}

func TestVerifyBlamesNobodyForAChangeProxmoxDidNotLog(t *testing.T) {
	pve, dir := verifyLab(t)
	// Only the card differs now; the card went on through the API, which
	// leaves no task, and the only config task is older and accrete's own.
	pve.Guest(202).Config["cores"] = 2
	pve.Tasks = nil
	pve.AddTask(pvetest.Task{Type: "qmconfig", User: "accrete@pve!test", VMID: 202, Start: time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)})
	out, _ := accreteVerify(t, pve, "", "--terraform-state", dir, "--select", "mgmt-01")
	if !strings.Contains(out, "changed by: unknown, the last recorded config change is accrete's own") {
		t.Errorf("an unlogged change must stay unknown:\n%s", out)
	}
	if strings.Contains(out, "not in the Terraform state") {
		t.Errorf("--select narrows the state, so what it leaves out is not news:\n%s", out)
	}
}

func TestVerifyReadsShowJSONFromStandardInput(t *testing.T) {
	pve, _ := verifyLab(t)
	show := map[string]any{"format_version": "1.0", "terraform_version": "1.14.8", "values": map[string]any{"root_module": map[string]any{
		"child_modules": []any{map[string]any{"address": `module.vms["web-01"]`, "resources": []any{map[string]any{
			"address": `module.vms["web-01"].proxmox_virtual_environment_vm.this`, "mode": "managed", "type": "proxmox_virtual_environment_vm",
			"name": "this", "provider_name": "registry.terraform.io/bpg/proxmox",
			"values":           bpgVM(201, "web-01", 2, 4096, map[string]int{"scsi0": 20, "scsi1": 50}, [2]string{"vweb", "BC:24:11:00:02:01"})["attributes"],
			"sensitive_values": map[string]any{"initialization": []any{map[string]any{"user_account": []any{map[string]any{"password": true}}}}},
		}}}},
	}}}
	b, _ := json.Marshal(show)
	out, code := accreteVerify(t, pve, string(b), "--terraform-state", "-", "--select", "web-01", "-o", "json")
	if code != 0 {
		t.Fatalf("exit %d, want 0:\n%s", code, out)
	}
	var rep verifyReport
	if err := json.Unmarshal([]byte(out), &rep); err != nil {
		t.Fatalf("%v:\n%s", err, out)
	}
	if rep.Format != "show-json" || !rep.OK || len(rep.Lines) != 1 || rep.Lines[0].Verdict != "matches" {
		t.Errorf("report: %+v", rep)
	}
}

func TestVerifyComparesPowerOnlyWhenAsked(t *testing.T) {
	pve, dir := verifyLab(t)
	// The state says started=false; web-01 runs.
	out, _ := accreteVerify(t, pve, "", "--terraform-state", dir, "--select", "web-01", "--terraform-power")
	if !strings.Contains(out, "power: the state records stopped, the provider reports running") {
		t.Errorf("--terraform-power must compare the power state:\n%s", out)
	}
}

func TestVerifyTellsADifferentMachineAtTheSameVMID(t *testing.T) {
	pve, dir := verifyLab(t)
	path := filepath.Join(dir, "terraform.tfstate")
	b, _ := os.ReadFile(path)
	// The state recorded an SMBIOS UUID; the guest at 201 carries another.
	b = bytes.Replace(b, []byte(`"smbios":[]`), []byte(`"smbios":[{"uuid":"33333333-0000-4000-8000-000000000201"}]`), 1)
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	out, _ := accreteVerify(t, pve, "", "--terraform-state", path, "--select", "web-01")
	if !strings.Contains(out, "smbios_uuid: the state records 33333333-0000-4000-8000-000000000201, the provider reports 22222222-0000-4000-8000-000000000201; the machine there is not the one the state records") {
		t.Errorf("a different machine must be named as such:\n%s", out)
	}
	if strings.Contains(out, "changed by") {
		t.Errorf("a different machine is not a change to blame anyone for:\n%s", out)
	}
}

func TestVerifyStopsOnARefusedToken(t *testing.T) {
	pve, dir := verifyLab(t)
	pve.RefuseToken()
	if out, code := accreteVerify(t, pve, "", "--terraform-state", dir); code != 2 {
		t.Errorf("exit %d, want 2:\n%s", code, out)
	}
}
