package manifest

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

const standaloneBase = `apiVersion: accretion/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  provider:
    type: example
    endpoint: https://pve.example:8006
    token_env: ACCRETE_TOKEN
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

func standalone(t *testing.T, old, new string) (Environment, error) {
	t.Helper()
	src := standaloneBase
	if old != "" {
		if !strings.Contains(src, old) {
			t.Fatalf("%q not in the base manifest", old)
		}
		src = strings.Replace(src, old, new, 1)
	}
	return ParseStandalone([]byte(src))
}

// The same resources under each apiVersion decode to the same Environment,
// except for what builds them.
func TestBothShapesDecodeTheSameResources(t *testing.T) {
	a, err := Parse([]byte(goldenBase))
	if err != nil {
		t.Fatal(err)
	}
	b, err := standalone(t, "", "")
	if err != nil {
		t.Fatal(err)
	}
	want := &ProviderRef{Type: "example", Endpoint: "https://pve.example:8006", VerifyTLS: true, TokenEnv: "ACCRETE_TOKEN"}
	if !reflect.DeepEqual(b.Spec.Provider, want) || b.Spec.Integration != "" {
		t.Fatalf("provider = %+v, integration %q", b.Spec.Provider, b.Spec.Integration)
	}
	b.Spec.Provider, b.Spec.Integration = nil, a.Spec.Integration
	if !reflect.DeepEqual(a, b) {
		t.Fatalf("the two shapes differ beyond the provider:\n%+v\n%+v", a, b)
	}
}

func TestTheProviderBlockIsChecked(t *testing.T) {
	const secret = "PVEAPIToken=root@pam!x=0b1c2d3e"
	cases := []struct{ name, old, new, want string }{
		{"inline token", "    token_env: ACCRETE_TOKEN\n", "    token: " + secret + "\n", "name the file that holds it with token_file, or the environment variable with token_env"},
		{"inline token beside a source", "    token_env: ACCRETE_TOKEN\n", "    token_env: ACCRETE_TOKEN\n    token: " + secret + "\n", "token_file"},
		{"both sources", "    token_env: ACCRETE_TOKEN\n", "    token_env: ACCRETE_TOKEN\n    token_file: ./t\n", "exactly one of token_file and token_env, not both"},
		{"no source", "    token_env: ACCRETE_TOKEN\n", "", "exactly one of token_file and token_env is required"},
		{"unknown key", "    token_env: ACCRETE_TOKEN\n", "    token_env: ACCRETE_TOKEN\n    password: x\n", "field password not found"},
		{"no type", "    type: example\n", "", "type is required"},
		{"no endpoint", "    endpoint: https://pve.example:8006\n", "", "endpoint is required"},
		{"plain http", "https://pve.example", "http://pve.example", "endpoint must be an https URL"},
		{"credentials in the endpoint", "https://pve.example", "https://root:" + secret + "@pve.example", "must not carry a user or password"},
		{"bad variable name", "token_env: ACCRETE_TOKEN", "token_env: 1TOKEN", "not an environment variable name"},
		{"no provider", "  provider:\n    type: example\n    endpoint: https://pve.example:8006\n    token_env: ACCRETE_TOKEN\n", "", "spec.provider is required"},
		{"a connector instead", "  provider:\n", "  integration: lab\n  provider:\n", "spec.integration names a Sybil connector"},
		{"the Sybil apiVersion", "accretion/v1alpha1", "sybil/v1alpha1", `unsupported apiVersion "sybil/v1alpha1", want accretion/v1alpha1`},
		{"no apiVersion", "apiVersion: accretion/v1alpha1\n", "", "apiVersion is required and must be accretion/v1alpha1"},
		{"a resource error still reads the same", "      blueprint: provision-vm\n", "", `resource "web" has no blueprint`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := standalone(t, c.old, c.new)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v; want %q", err, c.want)
			}
			if strings.Contains(err.Error(), secret) || strings.Contains(err.Error(), "0b1c2d3e") {
				t.Fatalf("the error repeats the secret: %v", err)
			}
			if strings.Contains(err.Error(), standalonePlaceholder) {
				t.Fatalf("the error leaks the placeholder: %v", err)
			}
		})
	}
}

func TestVerifyTLSDefaultsToOn(t *testing.T) {
	env, err := standalone(t, "", "")
	if err != nil || !env.Spec.Provider.VerifyTLS {
		t.Fatalf("env = %+v, %v", env.Spec.Provider, err)
	}
	env, err = standalone(t, "    token_env:", "    verify_tls: false\n    token_env:")
	if err != nil || env.Spec.Provider.VerifyTLS {
		t.Fatalf("env = %+v, %v", env.Spec.Provider, err)
	}
}

func TestAStandaloneManifestRoundTrips(t *testing.T) {
	env, err := standalone(t, "token_env: ACCRETE_TOKEN", "token_file: ./pve.token")
	if err != nil {
		t.Fatal(err)
	}
	src, err := Marshal(env)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(src, []byte("apiVersion: accretion/v1alpha1\n")) || !bytes.Contains(src, []byte("  provider:\n    type: example\n")) ||
		bytes.Contains(src, []byte("integration")) {
		t.Fatalf("encoded:\n%s", src)
	}
	back, err := ParseStandalone(src)
	if err != nil {
		t.Fatalf("%v\n%s", err, src)
	}
	if !reflect.DeepEqual(env, back) {
		t.Fatalf("round trip changed it:\n%+v\n%+v", env, back)
	}
	if _, err := Parse(src); err == nil {
		t.Fatal("DecodeEnvironment took a standalone manifest")
	}
}

func TestAStandaloneManifestNamesNoConnector(t *testing.T) {
	env, err := standalone(t, "", "")
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range env.References() {
		if r.Kind == RefIntegration {
			t.Fatalf("references = %+v", env.References())
		}
	}
	e := env.Emptied()
	if e.Spec.Provider == nil || *e.Spec.Provider != *env.Spec.Provider || e.Spec.Provider == env.Spec.Provider || e.Validate() != nil {
		t.Fatalf("emptied = %+v", e.Spec)
	}
}

func TestValidateWantsExactlyOneOfConnectorAndProvider(t *testing.T) {
	env, err := standalone(t, "", "")
	if err != nil {
		t.Fatal(err)
	}
	env.Spec.Integration = "lab"
	if err := env.Validate(); err == nil || !strings.Contains(err.Error(), "both set") {
		t.Fatalf("err = %v", err)
	}
	env.Spec.Integration, env.Spec.Provider = "", nil
	if err := env.Validate(); err == nil || !strings.Contains(err.Error(), "spec.integration is required") {
		t.Fatalf("err = %v", err)
	}
}

func TestATokenFileIsReadFromBesideTheManifest(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "pve.token"), []byte("  s3cret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	p := ProviderRef{TokenFile: "./pve.token"}
	var warn bytes.Buffer
	tok, err := p.Token(dir, nil, &warn)
	if err != nil || tok != "s3cret-token" || warn.Len() != 0 {
		t.Fatalf("token %q, err %v, warning %q", tok, err, warn.String())
	}
	abs := ProviderRef{TokenFile: filepath.Join(dir, "pve.token")}
	if got := abs.TokenPath("/elsewhere"); got != abs.TokenFile {
		t.Fatalf("absolute path resolved to %q", got)
	}
}

func TestATokenFileOthersCanReadIsWarnedAboutAndStillUsed(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no mode bits")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "pve.token")
	if err := os.WriteFile(path, []byte("s3cret-token"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil { // past the umask
		t.Fatal(err)
	}
	var warn bytes.Buffer
	tok, err := ProviderRef{TokenFile: "pve.token"}.Token(dir, nil, &warn)
	if err != nil || tok != "s3cret-token" {
		t.Fatalf("token %q, err %v", tok, err)
	}
	if w := warn.String(); !strings.Contains(w, path) || !strings.Contains(w, "0644") || strings.Contains(w, "s3cret") {
		t.Fatalf("warning = %q", w)
	}
}

func TestTokenErrorsNeverHoldTheToken(t *testing.T) {
	env := map[string]string{"SET": "s3cret-token"}
	getenv := func(k string) string { return env[k] }
	if tok, err := (ProviderRef{TokenEnv: "SET"}).Token("", getenv, nil); err != nil || tok != "s3cret-token" {
		t.Fatalf("token %q, err %v", tok, err)
	}
	if _, err := (ProviderRef{TokenEnv: "UNSET"}).Token("", getenv, nil); err == nil || !strings.Contains(err.Error(), "UNSET is not set") {
		t.Fatalf("err = %v", err)
	}
	dir := t.TempDir()
	if _, err := (ProviderRef{TokenFile: "missing"}).Token(dir, getenv, nil); err == nil || !strings.Contains(err.Error(), "token_file") {
		t.Fatalf("err = %v", err)
	}
	empty := filepath.Join(dir, "empty")
	if err := os.WriteFile(empty, []byte("\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (ProviderRef{TokenFile: empty}).Token(dir, getenv, nil); err == nil || !strings.Contains(err.Error(), "is empty") {
		t.Fatalf("err = %v", err)
	}
}

func TestTheStandaloneSchema(t *testing.T) {
	s := StandaloneJSONSchema()
	got, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	checkGolden(t, "schema.accretion-v1alpha1.json", append(got, '\n'))

	spec := s["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	if _, ok := spec["integration"]; ok {
		t.Fatal("the standalone schema still has spec.integration")
	}
	provider := spec["provider"].(map[string]any)["properties"].(map[string]any)
	typ := reflect.TypeOf(providerWire{})
	for i := 0; i < typ.NumField(); i++ {
		key := strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]
		if _, ok := provider[key]; !ok {
			t.Errorf("providerWire.%s (yaml %q) is not in the schema", typ.Field(i).Name, key)
		}
	}
	if _, ok := provider["token"]; ok {
		t.Error("the schema offers an inline token")
	}
	// Building it must not touch the Sybil schema.
	if _, ok := EnvironmentJSONSchema()["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)["integration"]; !ok {
		t.Fatal("the Sybil schema lost spec.integration")
	}
}
