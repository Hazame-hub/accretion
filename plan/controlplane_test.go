package plan

import (
	"reflect"
	"strings"
	"testing"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

var routedBlueprint = Blueprint{
	ID: "provision-vm",
	Inputs: []BlueprintInput{
		{Name: "integration", Type: "string", Required: true, Source: InputSourceIntegrations},
		{Name: "hostname", Type: "string", Required: true},
		{Name: "net0", Type: "string", Default: "virtio,bridge=vmbr0"},
		{Name: "cp_url", Type: "string", Default: "", Address: AddressControlPlaneURL},
		{Name: "cp_grpc", Type: "string", Default: "", Address: AddressControlPlaneGRPC},
		{Name: "cp_from", Type: "string", Default: "", Address: AddressControlPlaneFrom},
	},
}

var (
	tailnetRoute = &envmanifest.ControlPlane{URL: "http://100.80.85.32:8080", GRPC: "100.80.85.32:50051"}
	edgeRoute    = &envmanifest.ControlPlane{URL: "http://10.20.0.1:8080", GRPC: "10.20.0.1:50051"}
)

// routedEnv is one machine on network okd, with the routes given.
func routedEnv(count int, envRoute, netRoute *envmanifest.ControlPlane) envmanifest.Environment {
	env := staticEnv(count, "")
	env.Spec.Resources[0].IP = ""
	env.Spec.ControlPlane = envRoute
	env.Spec.Networks[0].ControlPlane = netRoute
	return env
}

func routedWorld(rs ...Realization) Snapshot {
	w := addressWorld(rs...)
	w.Blueprints = []Blueprint{routedBlueprint, netBlueprint}
	return w
}

func TestAnEnvironmentRouteReachesTheCreateButNotItsInputs(t *testing.T) {
	p := Build(routedEnv(1, tailnetRoute, nil), routedWorld())
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	e := p.Entries[0]
	if e.Action != ActionCreate || e.ControlPlane == nil {
		t.Fatalf("entry = %s, route %+v", e.Action, e.ControlPlane)
	}
	if e.ControlPlane.URL != tailnetRoute.URL || e.ControlPlane.GRPC != tailnetRoute.GRPC || e.ControlPlane.From != "environment" {
		t.Fatalf("route = %+v", e.ControlPlane)
	}
	for _, k := range []string{"cp_url", "cp_grpc", "cp_from"} {
		if _, ok := e.Inputs[k]; ok {
			t.Fatalf("the route leaked into the declared inputs as %s", k)
		}
	}
	launch := e.LaunchInputs()
	if launch["cp_url"] != tailnetRoute.URL || launch["cp_grpc"] != tailnetRoute.GRPC || launch["cp_from"] != "environment" {
		t.Fatalf("launch inputs = %v", launch)
	}
	if launch["hostname"] != e.Inputs["hostname"] {
		t.Fatalf("launch inputs lost the declared ones: %v", launch)
	}
}

func TestANetworkRouteBeatsTheEnvironments(t *testing.T) {
	p := Build(routedEnv(1, tailnetRoute, edgeRoute), routedWorld())
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	if cp := p.Entries[0].ControlPlane; cp == nil || cp.URL != edgeRoute.URL || cp.From != "network okd" {
		t.Fatalf("route = %+v", cp)
	}
}

func TestNoRouteLeavesTheDefault(t *testing.T) {
	p := Build(routedEnv(1, nil, nil), routedWorld())
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	e := p.Entries[0]
	if e.ControlPlane != nil {
		t.Fatalf("route = %+v, want none", e.ControlPlane)
	}
	if !reflect.DeepEqual(e.LaunchInputs(), e.Inputs) {
		t.Fatalf("launch inputs %v differ from inputs %v", e.LaunchInputs(), e.Inputs)
	}
}

func TestARouteNeedsABlueprintThatTakesOne(t *testing.T) {
	w := addressWorld()
	p := Build(routedEnv(1, tailnetRoute, nil), w)
	if p.OK() || !strings.Contains(strings.Join(p.Errors, "\n"), "cannot be given a control-plane route (control_plane from environment)") {
		t.Fatalf("errors: %v", p.Errors)
	}
}

func TestARouteAndTheSameInputArePickOne(t *testing.T) {
	env := routedEnv(1, tailnetRoute, nil)
	env.Spec.Resources[0].Inputs["cp_url"] = "http://192.168.1.45:8080"
	p := Build(env, routedWorld())
	if p.OK() || !strings.Contains(strings.Join(p.Errors, "\n"), "sets inputs.cp_url and gets control_plane from environment; pick one") {
		t.Fatalf("errors: %v", p.Errors)
	}
}

// A route only says where a machine phones home at first boot. Adding or
// changing one must not make a machine already built plan as change.
func TestARouteDoesNotChangeABuiltMachine(t *testing.T) {
	asset := uuid.New()
	built := realized("web", 0, &asset)
	built.IntentInputs = map[string]any{"hostname": "web-1", "integration": labID.String(), "net0": "virtio,bridge=okd"}
	w := routedWorld(built)
	w.Verdicts[asset] = AcceptanceAccepted

	before := Build(routedEnv(1, nil, nil), w)
	after := Build(routedEnv(1, tailnetRoute, edgeRoute), w)
	if !before.OK() || !after.OK() {
		t.Fatalf("errors: %v / %v", before.Errors, after.Errors)
	}
	if before.Entries[0].Action != ActionNoOp {
		t.Fatalf("fixture: the built machine plans %s: %+v", before.Entries[0].Action, before.Entries[0].Diff)
	}
	e := after.Entries[0]
	if e.Action != ActionNoOp || e.ControlPlane != nil || len(e.Diff) > 0 {
		t.Fatalf("with a route, the built machine plans %s, route %+v, diff %+v", e.Action, e.ControlPlane, e.Diff)
	}
}
