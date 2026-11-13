package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeTerraformState writes a raw state in which Terraform owns the given
// machines, by VMID and name.
func writeTerraformState(t *testing.T, dir string, vms map[int]string) string {
	t.Helper()
	var resources []any
	for vmid, name := range vms {
		resources = append(resources, map[string]any{
			"module": `module.vms["` + name + `"]`, "mode": "managed", "type": "proxmox_virtual_environment_vm", "name": "this",
			"provider":  `provider["registry.terraform.io/bpg/proxmox"]`,
			"instances": []any{map[string]any{"attributes": map[string]any{"vm_id": vmid, "name": name, "node_name": "pve1"}}},
		})
	}
	b, _ := json.Marshal(map[string]any{"version": 4, "terraform_version": "1.14.8", "serial": 1, "resources": resources})
	path := filepath.Join(dir, "terraform.tfstate")
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestApplyRefusesToCreateAMachineOfATerraformName(t *testing.T) {
	l := newApplyLab(t, "")
	tf := writeTerraformState(t, t.TempDir(), map[int]string{250: "accrete-web-2"})
	out, code := l.accrete("", "plan", "--terraform-state", tf)
	if code != 1 {
		t.Errorf("plan exit %d, want 1:\n%s", code, out)
	}
	for _, want := range []string{
		`error: web/1: a machine named "accrete-web-2" is Terraform's (module.vms["accrete-web-2"].proxmox_virtual_environment_vm.this in ` + tf + `)`,
		"1 machine is Terraform's (" + tf + "); accrete leaves it to Terraform",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("plan lacks %q:\n%s", want, out)
		}
	}
	out, code = l.accrete("", "apply", "--auto-approve", "--terraform-state", tf)
	if code != 1 {
		t.Errorf("apply exit %d, want 1:\n%s", code, out)
	}
	if w := l.pve.Writes(); len(w) != 0 {
		t.Errorf("apply wrote to the provider beside a Terraform machine: %v", w)
	}
	// Without the Terraform state, the same plan creates both: the flag is
	// what tells accrete whose the name is.
	if out, code := l.accrete("", "plan"); code != 0 || !strings.Contains(out, "2 to create") {
		t.Errorf("plan without --terraform-state: exit %d\n%s", code, out)
	}
}

func TestApplyAndDestroyRefuseAMachineTerraformOwns(t *testing.T) {
	// db-01 (101) was adopted by init before Terraform took it over: the
	// accrete record now points at a machine Terraform also owns.
	pve, dir := initLab(t)
	manifest := filepath.Join(dir, "env.yaml")
	if out, code := accreteInit(t, pve, "--name", "lab", "--template", "9000", "--select", "^db-01$", "-f", manifest); code != 0 {
		t.Fatalf("init exit %d:\n%s", code, out)
	}
	tf := writeTerraformState(t, t.TempDir(), map[int]string{101: "db-01"})
	l := &applyLab{t: t, pve: pve, manifest: manifest, state: filepath.Join(dir, "accrete.state.json")}
	out, code := l.accrete("", "destroy", "--auto-approve", "--terraform-state", tf)
	if code != 1 || !strings.Contains(out, "(pve1/qemu/101): Terraform owns this machine") {
		t.Errorf("destroy exit %d:\n%s", code, out)
	}
	if w := pve.Writes(); len(w) != 0 {
		t.Errorf("destroy touched a machine Terraform owns: %v", w)
	}
	if out, code := l.accrete("", "apply", "--terraform-state", "-"); code != 2 {
		t.Errorf("apply reads its confirmation from standard input, so --terraform-state - must be refused: exit %d\n%s", code, out)
	}
}

func TestInitLeavesOutWhatTerraformOwns(t *testing.T) {
	pve, dir := initLab(t)
	tf := writeTerraformState(t, t.TempDir(), map[int]string{201: "web-1", 101: "db-01"})
	manifest := filepath.Join(dir, "env.yaml")
	out, code := accreteInit(t, pve, "--name", "lab", "--template", "9000", "--terraform-state", tf, "-f", manifest)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		`left out web-1 (pve1/qemu/201): Terraform owns it (module.vms["web-1"].proxmox_virtual_environment_vm.this)`,
		`left out db-01 (pve1/qemu/101): Terraform owns it`,
		"wrote " + manifest + " (2 resources, 2 machines)",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	src, _ := os.ReadFile(manifest)
	if strings.Contains(string(src), "hostname: web-1") || strings.Contains(string(src), "db-01") {
		t.Errorf("the manifest declares a machine Terraform owns:\n%s", src)
	}
}
