package manifest

import (
	"bytes"
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
)

var updateGolden = flag.Bool("update-golden", false, "rewrite testdata/golden from what the code does now")

// checkGolden compares got with testdata/golden/<name>, or rewrites it under
// -update-golden.
func checkGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", "golden", name)
	if *updateGolden {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v; run with -update-golden", err)
	}
	if !bytes.Equal(got, want) {
		t.Errorf("%s differs from %s:\n%s", name, path, got)
	}
}

// The schema Continuum serves for sybil/v1alpha1, byte for byte, so adding
// the standalone shape can be shown to leave it alone.
func TestGoldenSybilSchema(t *testing.T) {
	got, err := json.MarshalIndent(EnvironmentJSONSchema(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "schema.sybil-v1alpha1.json", append(got, '\n'))
}

const goldenBase = `apiVersion: sybil/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  integration: lab
  group: web
  variables:
    size: 2048
  networks:
    - name: appnet
      zone: lab
      subnet: 10.20.0.0/24
  resources:
    - name: web
      count: 2
      blueprint: provision-vm
      network: appnet
      inputs:
        hostname: "web-{{instance.number}}"
        memory_mb: "{{var.size}}"
`

// The error text every Continuum path (config import, Git sync, the API)
// returns for a bad sybil/v1alpha1 manifest, and the re-encoded form of a
// good one, so a change to Validate or EncodeEnvironment can be shown to
// leave them alone.
func TestGoldenSybilDecode(t *testing.T) {
	cases := []struct{ name, old, new string }{
		{"valid", "", ""},
		{"no_api_version", "apiVersion: sybil/v1alpha1\n", ""},
		{"future_api_version", "sybil/v1alpha1", "sybil/v1"},
		{"standalone_api_version", "sybil/v1alpha1", "accretion/v1alpha1"},
		{"wrong_kind", "kind: Environment", "kind: Runbook"},
		{"no_integration", "  integration: lab\n", ""},
		{"blank_integration", "integration: lab", "integration: \"  \""},
		{"provider_key", "  integration: lab\n", "  provider:\n    type: x\n"},
		{"misspelled_key", "  group:", "  groop:"},
		{"no_blueprint", "      blueprint: provision-vm\n", ""},
		{"negative_count", "count: 2", "count: -1"},
		{"huge_count", "count: 2", "count: 5000"},
		{"bad_variable", "    size: 2048", "    9size: 2048"},
		{"bad_network_name", "name: appnet", "name: App-Net"},
		{"bad_subnet", "10.20.0.0/24", "10.20.0.1/24"},
		{"unknown_network", "network: appnet", "network: other-net"},
		{"bad_image", "      network: appnet\n", "      network: appnet\n      image: \"bad image\"\n"},
		{"bad_power", "      network: appnet\n", "      network: appnet\n      power: sometimes\n"},
	}
	type outcome struct {
		Name  string `json:"name"`
		Error string `json:"error,omitempty"`
		YAML  string `json:"yaml,omitempty"`
	}
	var out []outcome
	for _, c := range cases {
		src := goldenBase
		if c.old != "" {
			if !bytes.Contains([]byte(src), []byte(c.old)) {
				t.Fatalf("%s: %q not in the base manifest", c.name, c.old)
			}
			src = string(bytes.Replace([]byte(src), []byte(c.old), []byte(c.new), 1))
		}
		o := outcome{Name: c.name}
		env, err := Parse([]byte(src))
		if err != nil {
			o.Error = err.Error()
		} else {
			b, err := Marshal(env)
			if err != nil {
				o.Error = "marshal: " + err.Error()
			}
			o.YAML = string(b)
		}
		out = append(out, o)
	}
	got, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "decode.sybil-v1alpha1.json", append(got, '\n'))
}
