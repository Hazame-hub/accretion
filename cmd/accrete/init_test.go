package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

// initLab is pvetest with db-01 (its own guest 101) plus web-1 and web-2,
// alike, and a guest with no identity.
func initLab(t *testing.T) (*pvetest.Server, string) {
	t.Helper()
	pve := pvetest.New(t)
	web := func(vmid int, name, uuid string) *pvetest.Guest {
		return &pvetest.Guest{VMID: vmid, Kind: "qemu", Status: "running", Config: map[string]any{
			"name": name, "smbios1": "uuid=" + uuid, "cores": 2, "memory": 4096, "cpu": "x86-64-v2-AES",
			"net0": "virtio=BC:24:11:00:00:01,bridge=vmbr0,tag=30", "scsi0": "local-lvm:vm-" + name + "-disk-0,size=20G",
		}}
	}
	pve.Guests[201] = web(201, "web-1", "11111111-0000-4000-8000-000000000201")
	pve.Guests[202] = web(202, "web-2", "11111111-0000-4000-8000-000000000202")
	// A linked clone of template 9000.
	pve.Guests[203] = &pvetest.Guest{VMID: 203, Kind: "qemu", Status: "stopped", Config: map[string]any{
		"name": "batch", "smbios1": "uuid=11111111-0000-4000-8000-000000000203", "cores": 1, "memory": 1024,
		"net0": "virtio=BC:24:11:00:00:03,bridge=vmbr0", "scsi0": "local-lvm:base-9000-disk-0/vm-203-disk-0,size=10G",
	}}
	pve.Guests[204] = &pvetest.Guest{VMID: 204, Kind: "qemu", Status: "running", Config: map[string]any{
		"name": "no-identity", "cores": 1, "memory": 512, "scsi0": "local-lvm:vm-204-disk-0,size=4G",
	}}
	return pve, t.TempDir()
}

func accreteInit(t *testing.T, pve *pvetest.Server, args ...string) (string, int) {
	t.Helper()
	var out bytes.Buffer
	args = append([]string{"init", "--from-cluster", "--provider", "proxmox", "--endpoint", pve.URL,
		"--token-env", applyTokenVar, "--verify-tls=false"}, args...)
	code := run(context.Background(), args, strings.NewReader(""), &out, &out, func(k string) string {
		if k == applyTokenVar {
			return pvetest.Token
		}
		return ""
	})
	if strings.Contains(out.String(), pvetest.Secret) {
		t.Fatalf("the token's secret reached the output:\n%s", out.String())
	}
	return out.String(), code
}

func TestInitDeclaresTheClusterAndThePlanIsUnchanged(t *testing.T) {
	pve, dir := initLab(t)
	manifest := filepath.Join(dir, "env.yaml")
	out, code := accreteInit(t, pve, "--name", "lab", "--template", "9000", "-f", manifest)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	for _, want := range []string{
		"left out no-identity (pve1/qemu/204): it carries no SMBIOS UUID",
		"wrote " + manifest + " (3 resources, 4 machines)",
		"0 to create, 4 unchanged, 0 to change, 0 to delete, 0 unknown",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if w := pve.Writes(); len(w) != 0 {
		t.Errorf("init wrote to the provider: %v", w)
	}

	src, err := os.ReadFile(manifest)
	if err != nil {
		t.Fatal(err)
	}
	env, err := envmanifest.ParseStandalone(src)
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	byName := map[string]envmanifest.EnvironmentResource{}
	for _, r := range env.Spec.Resources {
		byName[r.Name] = r
	}
	web, ok := byName["web"]
	if !ok || web.Count != 2 || web.Inputs["hostname"] != "web-{{instance.number}}" || web.Inputs["net0"] != "virtio,bridge=vmbr0,tag=30" {
		t.Errorf("web/1 and web/2 are not one resource of 2:\n%s", src)
	}
	if b := byName["batch"]; b.Power != "off" || b.Inputs["template_vmid"] != "9000" {
		t.Errorf("batch (stopped, linked clone of 9000): %+v", b)
	}
	if _, ok := byName["db-01"]; !ok {
		t.Errorf("db-01 is not declared on its own:\n%s", src)
	}

	s, err := loadState(filepath.Join(dir, "accrete.state.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range s.Realizations {
		if r.AdoptedAs != "provision-proxmox-vm" || r.MintedUUID == "" || s.Verdicts[*r.AssetID] != plan.AcceptanceAccepted {
			t.Errorf("realization %+v, verdict %q", r, s.Verdicts[*r.AssetID])
		}
	}

	// A machine resized afterwards is drift, like one accrete built.
	pve.SetConfig(201, "memory", 8192)
	var buf bytes.Buffer
	run(context.Background(), []string{"plan", "-f", manifest}, strings.NewReader(""), &buf, &buf, func(k string) string {
		if k == applyTokenVar {
			return pvetest.Token
		}
		return ""
	})
	if !strings.Contains(buf.String(), "changed outside Sybil: memory_mb is 8192") {
		t.Errorf("drift on an adopted machine is not seen:\n%s", buf.String())
	}
}

func TestInitNeverGuessesAFullClonesTemplate(t *testing.T) {
	pve, dir := initLab(t)
	manifest := filepath.Join(dir, "env.yaml")
	out, code := accreteInit(t, pve, "--select", "^web-", "-f", manifest)
	if code == 0 {
		t.Errorf("the plan passed with the template unset:\n%s", out)
	}
	if !strings.Contains(out, "set variables.template_web to the template_vmid resource web's machines came from (Proxmox does not record which template a full clone was made from), or run init with --template") {
		t.Errorf("the unread template is not asked for:\n%s", out)
	}
	src, _ := os.ReadFile(manifest)
	if !strings.Contains(string(src), "template_vmid: '{{var.template_web}}'") {
		t.Errorf("the manifest does not leave the template as a variable:\n%s", src)
	}
	if strings.Contains(string(src), "db-01") || strings.Contains(string(src), "batch") {
		t.Errorf("--select let other machines in:\n%s", src)
	}
}

func TestInitNeverOverwrites(t *testing.T) {
	pve, dir := initLab(t)
	manifest := filepath.Join(dir, "env.yaml")
	if err := os.WriteFile(manifest, []byte("keep me"), 0o600); err != nil {
		t.Fatal(err)
	}
	out, code := accreteInit(t, pve, "-f", manifest)
	if code != 2 || !strings.Contains(out, "init never overwrites a file") {
		t.Errorf("exit %d:\n%s", code, out)
	}
	if b, _ := os.ReadFile(manifest); string(b) != "keep me" {
		t.Error("the manifest was overwritten")
	}
}
