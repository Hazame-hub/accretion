package terraform

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func addresses(s State) []string {
	out := make([]string, 0, len(s.Instances))
	for _, in := range s.Instances {
		out = append(out, in.Address)
	}
	return out
}

// noSecret fails when a value Terraform marks sensitive is still anywhere in
// what was read.
func noSecret(t *testing.T, s State, secrets ...string) {
	t.Helper()
	for _, in := range s.Instances {
		b, err := json.Marshal(in.Attributes)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range secrets {
			if strings.Contains(string(b), secret) {
				t.Errorf("%s still holds a sensitive value: %s", in.Address, b)
			}
		}
	}
}

func TestReadsARawStateWithoutItsSecrets(t *testing.T) {
	s, err := ReadFile("testdata/raw.tfstate", nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Format != FormatRawState || s.Serial != 12 || s.TerraformVersion != "1.14.8" || !strings.HasPrefix(s.Lineage, "ef40adc2") {
		t.Errorf("header: %+v", s)
	}
	want := []string{
		`module.sdn.proxmox_sdn_vnet.vnets["mgmt"]`,
		`module.sdn.proxmox_sdn_vnet.vnets[0]`,
		`module.vms["web-01"].proxmox_virtual_environment_vm.this`,
	}
	if got := addresses(s); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("addresses (data sources must be left out):\n got %q\nwant %q", got, want)
	}
	vm := s.Instances[2]
	if vm.Type != "proxmox_virtual_environment_vm" || vm.Provider != "registry.terraform.io/bpg/proxmox" {
		t.Errorf("instance: %+v", vm)
	}
	if n, ok := vm.Attributes["vm_id"].(json.Number); !ok || n.String() != "201" {
		t.Errorf("vm_id = %#v, want json.Number 201", vm.Attributes["vm_id"])
	}
	// The username next to the password stays: only the marked value goes.
	b, _ := json.Marshal(vm.Attributes["initialization"])
	if !strings.Contains(string(b), `"username":"ops"`) {
		t.Errorf("initialization lost more than the password: %s", b)
	}
	noSecret(t, s, "hunter2-secret", `"api_key"`, "c2VjcmV0LXByaXZhdGU=")
}

func TestReadsShowJSONWithoutItsSecrets(t *testing.T) {
	src, err := os.ReadFile("testdata/show.json")
	if err != nil {
		t.Fatal(err)
	}
	s, err := ReadFile("-", strings.NewReader(string(src)))
	if err != nil {
		t.Fatal(err)
	}
	if s.Format != FormatShowJSON || s.Source != "-" || s.Serial != 0 {
		t.Errorf("header: %+v", s)
	}
	want := []string{`module.vms["web-01"].proxmox_virtual_environment_vm.this`, "proxmox_network_linux_bridge.vmbr0"}
	if got := addresses(s); strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("addresses:\n got %q\nwant %q", got, want)
	}
	noSecret(t, s, "hunter2-secret")
}

func TestADirectoryIsReadThroughItsStateFile(t *testing.T) {
	dir := t.TempDir()
	if _, err := ReadFile(dir, nil); err == nil || !strings.Contains(err.Error(), "terraform show -json") {
		t.Errorf("an empty directory must say how to get a state: %v", err)
	}
	src, _ := os.ReadFile("testdata/raw.tfstate")
	if err := os.WriteFile(filepath.Join(dir, "terraform.tfstate"), src, 0o600); err != nil {
		t.Fatal(err)
	}
	s, err := ReadFile(dir, nil)
	if err != nil || len(s.Instances) != 3 {
		t.Fatalf("got %d instances, %v", len(s.Instances), err)
	}
}

func TestRefusesWhatItCannotRead(t *testing.T) {
	for name, src := range map[string]string{
		"old state":          `{"version": 3, "resources": []}`,
		"future show format": `{"format_version": "2.0"}`,
		"not a state":        `{"hello": "world"}`,
		"not json":           `terraform`,
		"bad sensitive path": `{"version":4,"resources":[{"mode":"managed","type":"t","name":"n","instances":[{"attributes":{"a":1},"sensitive_attributes":[[{"type":"index","value":0}]]}]}]}`,
	} {
		if _, err := Parse([]byte(src)); err == nil {
			t.Errorf("%s: read without an error", name)
		}
	}
}

func TestAnEmptyShowJSONHasNoInstances(t *testing.T) {
	// terraform show -json on an empty state prints no values at all.
	s, err := Parse([]byte(`{"format_version":"1.0"}`))
	if err != nil || len(s.Instances) != 0 {
		t.Fatalf("%+v %v", s, err)
	}
}
