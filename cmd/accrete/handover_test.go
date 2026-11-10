package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestImportFromTerraformAdoptsAndWritesTheHandover(t *testing.T) {
	pve, dir := initLab(t)
	inst := func(vmid int, name string, key any) map[string]any {
		m := map[string]any{"attributes": map[string]any{"vm_id": vmid, "name": name, "node_name": "pve1"}}
		if key != nil {
			m["index_key"] = key
		}
		return m
	}
	state := map[string]any{"version": 4, "terraform_version": "1.14.8", "serial": 3, "resources": []any{
		map[string]any{"module": `module.vms["web-1"]`, "mode": "managed", "type": "proxmox_virtual_environment_vm", "name": "this", "instances": []any{inst(201, "web-1", nil)}},
		map[string]any{"module": `module.vms["web-2"]`, "mode": "managed", "type": "proxmox_virtual_environment_vm", "name": "this", "instances": []any{inst(202, "web-2", nil)}},
		map[string]any{"module": `module.vms["db-01"]`, "mode": "managed", "type": "proxmox_virtual_environment_vm", "name": "this", "instances": []any{inst(101, "db-01", nil)}},
		map[string]any{"mode": "managed", "type": "proxmox_virtual_environment_vm", "name": "batch", "instances": []any{inst(203, "batch", nil)}},
	}}
	b, _ := json.Marshal(state)
	tf := filepath.Join(t.TempDir(), "terraform.tfstate")
	if err := os.WriteFile(tf, b, 0o600); err != nil {
		t.Fatal(err)
	}
	before := string(b)
	manifest := filepath.Join(dir, "env.yaml")
	// accrete import is init --from-terraform.
	out, code := accreteInit(t, pve, "--from-cluster=false", "--from-terraform", tf, "--name", "lab", "--template", "9000",
		"--select", `^(web-|batch)`, "-f", manifest)
	_ = out
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"wrote " + manifest + " (2 resources, 3 machines)",
		"terraform state rm 'module.vms[\"web-1\"].proxmox_virtual_environment_vm.this'",
		"terraform state rm 'module.vms[\"web-2\"].proxmox_virtual_environment_vm.this'",
		"Until Terraform forgets them, both tools record these machines",
		"0 to create, 3 unchanged",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	// db-01 is Terraform's but not selected; no-identity is in no state.
	src, _ := os.ReadFile(manifest)
	if strings.Contains(string(src), "db-01") || strings.Contains(string(src), "no-identity") {
		t.Errorf("the manifest declares a machine not handed over:\n%s", src)
	}
	handover, err := os.ReadFile(filepath.Join(dir, "terraform-handover.tf"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(handover), "removed {\n  from = proxmox_virtual_environment_vm.batch\n\n  lifecycle {\n    destroy = false\n  }\n}") ||
		strings.Contains(string(handover), "module.vms") {
		t.Errorf("handover: only batch has every instance adopted, so only it gets a removed block:\n%s", handover)
	}
	if after, _ := os.ReadFile(tf); string(after) != before {
		t.Error("import changed the Terraform state")
	}
	if w := pve.Writes(); len(w) != 0 {
		t.Errorf("import wrote to the provider: %v", w)
	}
}

func TestExportWritesImportBlocksThenRetiresWhatTerraformTookOver(t *testing.T) {
	l := newApplyLab(t, "")
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	out, code := l.accrete("", "export", "--terraform")
	if code != 0 {
		t.Fatalf("export: exit %d\n%s", code, out)
	}
	blocks, _ := os.ReadFile(filepath.Join(filepath.Dir(l.manifest), "terraform-import.tf"))
	for _, want := range []string{
		"import {\n  to = proxmox_virtual_environment_vm.web_1\n  id = \"pve1/100\"\n}",
		"import {\n  to = proxmox_virtual_environment_vm.web_2\n  id = \"pve1/102\"\n}",
		"terraform plan -generate-config-out=generated.tf",
	} {
		if !strings.Contains(string(blocks), want) {
			t.Errorf("import blocks lack %q:\n%s", want, blocks)
		}
	}
	if _, code := l.accrete("", "export", "--terraform"); code != 2 {
		t.Error("a second export must not overwrite the file")
	}

	// Terraform imported web-1 only.
	tf := writeTerraformState(t, t.TempDir(), map[int]string{100: "accrete-web-1"})
	out, code = l.accrete("", "export", "--retire", "--terraform-state", tf)
	if code != 1 || !strings.Contains(out, "kept web/0 (pve1/qemu/100): Terraform owns it") || !strings.Contains(out, "remove it there first") {
		t.Errorf("retire while still declared: exit %d\n%s", code, out)
	}
	src, _ := os.ReadFile(l.manifest)
	cut := strings.Index(string(src), "  resources:")
	if err := os.WriteFile(l.manifest, []byte(string(src)[:cut]+"  resources: []\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code = l.accrete("", "export", "--retire", "--terraform-state", tf)
	if code != 0 || !strings.Contains(out, "retired web/0 (pve1/qemu/100): handed to Terraform") || !strings.Contains(out, "1 retired, 0 kept") {
		t.Errorf("retire: exit %d\n%s", code, out)
	}
	// web/1 is still accrete's: the plan deletes it, web/0 is gone from it.
	p := l.plan()
	if !strings.Contains(p.Summary, "1 to delete") {
		t.Errorf("after retire: %s", p.Summary)
	}
	if w := l.pve.Writes(); len(w) == 0 {
		t.Fatal("apply wrote nothing")
	}
}
