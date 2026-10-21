package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
)

const (
	testToken  = "root@pam!accrete=0d1e2f3a-s3cret"
	secretPart = "0d1e2f3a-s3cret"
	webUUID    = "6f2c9a10-3b4d-4e5f-8a9b-0c1d2e3f4a5b"
	webAsset   = "22222222-2222-4222-8222-222222222222"
	webRealize = "33333333-3333-4333-8333-333333333333"
	tokenVar   = "ACCRETE_TEST_TOKEN"
)

// manifest declares web (built, per the state) and db (not built yet).
const manifest = `apiVersion: accretion/v1alpha1
kind: Environment
metadata:
  name: accrete-lab
spec:
  provider:
    type: proxmox
    endpoint: ENDPOINT
    verify_tls: false
    token_env: ACCRETE_TEST_TOKEN
  resources:
    - name: web
      blueprint: provision-proxmox-vm
      inputs:
        pve_node: pve1
        template_vmid: "9000"
        hostname: accrete-web-{{instance.number}}
        cores: 2
        memory_mb: 4096
    - name: db
      blueprint: provision-proxmox-vm
      inputs:
        pve_node: pve1
        template_vmid: "9000"
        hostname: accrete-db-{{instance.number}}
        cores: 2
        memory_mb: 2048
`

// pve is one node holding web's guest at VMID 105 with the given SMBIOS UUID
// and memory. It answers GETs only and records anything else, which accrete
// must never send.
type pve struct {
	*httptest.Server
	mu       sync.Mutex
	nonGET   []string
	smbios   string
	memoryMB int
}

func newPVE(t *testing.T, smbios string, memoryMB int) *pve {
	t.Helper()
	p := &pve{smbios: smbios, memoryMB: memoryMB}
	p.Server = httptest.NewTLSServer(http.HandlerFunc(p.serve))
	t.Cleanup(p.Close)
	return p
}

func (p *pve) serve(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		p.mu.Lock()
		p.nonGET = append(p.nonGET, r.Method+" "+r.URL.Path)
		p.mu.Unlock()
		w.WriteHeader(http.StatusMethodNotAllowed)
		return
	}
	if r.Header.Get("Authorization") != "PVEAPIToken="+testToken {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	data := func(v any) { _ = json.NewEncoder(w).Encode(map[string]any{"data": v}) }
	mem := int64(p.memoryMB) << 20
	switch strings.TrimPrefix(r.URL.Path, "/api2/json") {
	case "/nodes":
		data([]map[string]any{{"node": "pve1", "status": "online", "maxmem": int64(32) << 30, "maxcpu": 8}})
	case "/nodes/pve1/qemu":
		data([]map[string]any{{"vmid": 105, "name": "accrete-web-1", "status": "running", "maxmem": mem, "cpus": 2}})
	case "/nodes/pve1/lxc", "/nodes/pve1/storage", "/cluster/sdn/vnets", "/cluster/sdn/zones":
		data([]any{})
	case "/access/permissions":
		data(map[string]any{"/sdn": map[string]any{"SDN.Audit": 1}})
	case "/nodes/pve1/qemu/105/config":
		data(map[string]any{"name": "accrete-web-1", "smbios1": "uuid=" + p.smbios, "cores": 2, "memory": p.memoryMB,
			"net0": "virtio=BC:24:11:00:00:05,bridge=vmbr0", "scsi0": "local-lvm:vm-105-disk-0,size=20G"})
	case "/nodes/pve1/qemu/105/status/current":
		data(map[string]any{"status": "running", "maxmem": mem})
	default:
		w.WriteHeader(http.StatusNotImplemented)
	}
}

// lab writes the manifest for endpoint, and, when built is true, a state
// recording web/0 as built at pve1/qemu/105, accepted, at 4096 MiB. It
// returns the manifest's path.
func lab(t *testing.T, endpoint string, built bool) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "env.yaml")
	if err := os.WriteFile(path, []byte(strings.Replace(manifest, "ENDPOINT", endpoint, 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if !built {
		return path
	}
	env, err := envmanifest.ParseStandalone([]byte(strings.Replace(manifest, "ENDPOINT", endpoint, 1)))
	if err != nil {
		t.Fatal(err)
	}
	provider := ProviderID(*env.Spec.Provider)
	asset := uuid.MustParse(webAsset)
	state := State{
		Version: stateVersion,
		Realizations: []plan.Realization{{
			ID: uuid.MustParse(webRealize), IntegrationID: provider, AssetID: &asset,
			ResourceRefID: "pve1/qemu/105", ResourceKind: "vm", Managed: true,
			Intent:     &plan.RealizationIntent{Kind: plan.IntentKind, Name: "accrete-lab", ResourceName: "web", InstanceIndex: 0},
			Provenance: plan.Provenance{RunbookName: "Provision VM on Proxmox & enroll"},
			IntentInputs: map[string]any{
				"integration": provider.String(), "pve_node": "pve1", "template_vmid": "9000",
				"hostname": "accrete-web-1", "cores": 2, "memory_mb": 4096,
			},
			MintedUUID: webUUID,
		}},
		Verdicts: map[uuid.UUID]string{asset: plan.AcceptanceAccepted},
	}
	raw, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "accrete.state.json"), raw, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func accrete(t *testing.T, token string, args ...string) (apiPlan, string, int) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := run(context.Background(), append([]string{"plan", "-o", "json"}, args...), &out, &errOut,
		func(k string) string {
			if k == tokenVar {
				return token
			}
			return ""
		})
	var p apiPlan
	if code != 2 {
		if err := json.Unmarshal(out.Bytes(), &p); err != nil {
			t.Fatalf("output is not a plan: %v\n%s\n%s", err, out.String(), errOut.String())
		}
	}
	all := out.String() + errOut.String()
	if strings.Contains(all, secretPart) {
		t.Fatalf("the token's secret reached the output:\n%s", all)
	}
	return p, all, code
}

func entry(t *testing.T, p apiPlan, resource string) plan.Entry {
	t.Helper()
	for _, e := range p.Entries {
		if e.Resource == resource {
			return e
		}
	}
	t.Fatalf("no entry for %s in %+v", resource, p.Entries)
	return plan.Entry{}
}

func noWrites(t *testing.T, srv *pve) {
	t.Helper()
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.nonGET) > 0 {
		t.Fatalf("accrete sent %v; it must send GETs only", srv.nonGET)
	}
}

func TestNothingBuiltYetPlansEveryMachineAsCreate(t *testing.T) {
	srv := newPVE(t, webUUID, 4096)
	p, out, code := accrete(t, testToken, "-f", lab(t, srv.URL, false))
	if code != 0 || p.Summary != "2 to create, 0 unchanged, 0 to change, 0 to delete, 0 unknown" {
		t.Fatalf("exit %d, summary %q\n%s", code, p.Summary, out)
	}
	noWrites(t, srv)
}

func TestABuiltMachineAsDeclaredIsANoOp(t *testing.T) {
	srv := newPVE(t, webUUID, 4096)
	p, out, code := accrete(t, testToken, "-f", lab(t, srv.URL, true))
	if web := entry(t, p, "web"); code != 0 || web.Action != plan.ActionNoOp {
		t.Fatalf("exit %d, web = %+v\n%s", code, web, out)
	}
	if db := entry(t, p, "db"); db.Action != plan.ActionCreate {
		t.Fatalf("db = %+v", db)
	}
	if len(p.Notes) != 0 {
		t.Errorf("notes = %v", p.Notes)
	}
	noWrites(t, srv)
}

func TestAMachineResizedOutsideIsAChangeNamingBothValues(t *testing.T) {
	srv := newPVE(t, webUUID, 2048)
	p, out, _ := accrete(t, testToken, "-f", lab(t, srv.URL, true))
	web := entry(t, p, "web")
	if web.Action != plan.ActionChange || !web.Drift || len(web.Diff) != 1 || web.Diff[0].Input != "memory_mb" {
		t.Fatalf("web = %+v\n%s", web, out)
	}
	if !strings.Contains(web.Reason, "2048") || !strings.Contains(web.Reason, "4096") {
		t.Errorf("reason = %q", web.Reason)
	}
	noWrites(t, srv)
}

// Another guest at the recorded VMID is not this machine: unknown, saying
// why, never a no-op from the record alone.
func TestAnIdentityMismatchIsUnknown(t *testing.T) {
	srv := newPVE(t, "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee", 4096)
	p, out, _ := accrete(t, testToken, "-f", lab(t, srv.URL, true))
	web := entry(t, p, "web")
	if web.Action != plan.ActionUnknown || !strings.Contains(web.Reason, "identity mismatch") || !strings.Contains(web.Reason, webUUID) {
		t.Fatalf("web = %+v\n%s", web, out)
	}
	noWrites(t, srv)
}

func TestAnUnreachableProviderLeavesRecordedMachinesUnknown(t *testing.T) {
	srv := newPVE(t, webUUID, 4096)
	url := srv.URL
	srv.Close()
	p, out, code := accrete(t, testToken, "-f", lab(t, url, true), "--timeout", "10s")
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	if web := entry(t, p, "web"); web.Action != plan.ActionUnknown || !strings.Contains(web.Reason, "could not be read") {
		t.Fatalf("web = %+v", web)
	}
	if db := entry(t, p, "db"); db.Action != plan.ActionCreate {
		t.Fatalf("db = %+v", db)
	}
	if !contains(p.Notes, "could not be read") {
		t.Errorf("notes = %v", p.Notes)
	}
}

func TestARefusedTokenLeavesRecordedMachinesUnknown(t *testing.T) {
	srv := newPVE(t, webUUID, 4096)
	p, out, _ := accrete(t, "root@pam!accrete=wrong-"+secretPart+"x", "-f", lab(t, srv.URL, true))
	web := entry(t, p, "web")
	if web.Action != plan.ActionUnknown || !strings.Contains(web.Reason, "401") || !strings.Contains(web.Reason, "refused the token") {
		t.Fatalf("web = %+v\n%s", web, out)
	}
	if !contains(p.Notes, "refused the token") {
		t.Errorf("notes = %v", p.Notes)
	}
	noWrites(t, srv)
}

// The secret typed alone, without user@realm!tokenid=, is refused before
// any call, without being quoted.
func TestAMalformedTokenCannotPlan(t *testing.T) {
	srv := newPVE(t, webUUID, 4096)
	_, out, code := accrete(t, secretPart, "-f", lab(t, srv.URL, false))
	if code != 2 || !strings.Contains(out, "user@realm!tokenid=secret") {
		t.Fatalf("exit %d\n%s", code, out)
	}
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if len(srv.nonGET) > 0 {
		t.Fatalf("sent %v", srv.nonGET)
	}
}

func TestNoTokenIsAnError(t *testing.T) {
	srv := newPVE(t, webUUID, 4096)
	_, out, code := accrete(t, "", "-f", lab(t, srv.URL, false))
	if code != 2 || !strings.Contains(out, tokenVar+" is not set") {
		t.Fatalf("exit %d\n%s", code, out)
	}
}

func TestOfflineAsksNothingAndConfirmsNothing(t *testing.T) {
	p, _, code := accrete(t, "", "-f", lab(t, "https://192.0.2.1:8006", true), "--offline")
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if web := entry(t, p, "web"); web.Action != plan.ActionUnknown || !strings.Contains(web.Reason, "--offline") {
		t.Fatalf("web = %+v", web)
	}
}

// A state recorded against one endpoint is not matched against another.
func TestAStateFromAnotherEndpointIsNotThisOne(t *testing.T) {
	srv := newPVE(t, webUUID, 4096)
	path := lab(t, srv.URL, true)
	src, _ := os.ReadFile(path)
	moved := strings.Replace(string(src), srv.URL, srv.URL+"/other", 1)
	if err := os.WriteFile(path, []byte(moved), 0o600); err != nil {
		t.Fatal(err)
	}
	p, _, _ := accrete(t, testToken, "-f", path)
	if web := entry(t, p, "web"); web.Action == plan.ActionNoOp {
		t.Fatalf("web = %+v", web)
	}
}

func TestBadInputCannotPlan(t *testing.T) {
	dir := t.TempDir()
	sybil := filepath.Join(dir, "sybil.yaml")
	_ = os.WriteFile(sybil, []byte(strings.Replace(strings.Replace(manifest, "accretion/v1alpha1", "sybil/v1alpha1", 1), "ENDPOINT", "https://x", 1)), 0o600)
	inline := filepath.Join(dir, "inline.yaml")
	_ = os.WriteFile(inline, []byte(strings.Replace(strings.Replace(manifest, "token_env: ACCRETE_TEST_TOKEN", "token: "+testToken, 1), "ENDPOINT", "https://x", 1)), 0o600)
	unknown := filepath.Join(dir, "unknown.yaml")
	_ = os.WriteFile(unknown, []byte(strings.Replace(strings.Replace(manifest, "type: proxmox", "type: nimbus", 1), "ENDPOINT", "https://x", 1)), 0o600)
	badState := lab(t, "https://x", false)
	_ = os.WriteFile(filepath.Join(filepath.Dir(badState), "accrete.state.json"), []byte(`{"version": 2}`), 0o600)
	for name, args := range map[string][]string{
		"no -f":            {},
		"bad output":       {"-f", sybil, "-o", "yaml"},
		"sybil manifest":   {"-f", sybil},
		"inline token":     {"-f", inline},
		"unknown provider": {"-f", unknown},
		"newer state":      {"-f", badState},
		"missing manifest": {"-f", filepath.Join(dir, "nope.yaml")},
	} {
		t.Run(name, func(t *testing.T) {
			if _, out, code := accrete(t, testToken, args...); code != 2 {
				t.Fatalf("exit %d\n%s", code, out)
			}
		})
	}
}

func TestTextOutput(t *testing.T) {
	srv := newPVE(t, webUUID, 2048)
	var out bytes.Buffer
	code := run(context.Background(), []string{"plan", "-f", lab(t, srv.URL, true)}, &out, &out,
		func(k string) string {
			if k == tokenVar {
				return testToken
			}
			return ""
		})
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out.String())
	}
	for _, want := range []string{
		"Environment/accrete-lab",
		"change   web/0  (pve1/qemu/105)  changed outside Sybil",
		"memory_mb: 2048 -> 4096",
		"create   db/0",
		"1 to create, 0 unchanged, 1 to change, 0 to delete, 0 unknown",
	} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("text output lacks %q:\n%s", want, out.String())
		}
	}
	if strings.Contains(out.String(), secretPart) {
		t.Fatal("the token reached the output")
	}
}

// The shipped example decodes and plans with two creates.
func TestTheExamplePlansOffline(t *testing.T) {
	p, out, code := accrete(t, "", "-f", "../../examples/two-vms.yaml", "--offline")
	if code != 0 || p.Summary != "2 to create, 0 unchanged, 0 to change, 0 to delete, 0 unknown" {
		t.Fatalf("exit %d, summary %q\n%s", code, p.Summary, out)
	}
}

func contains(lines []string, sub string) bool {
	for _, l := range lines {
		if strings.Contains(l, sub) {
			return true
		}
	}
	return false
}
