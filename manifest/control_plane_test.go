package manifest

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const controlPlaneBase = `apiVersion: sybil/v1alpha1
kind: Environment
metadata:
  name: rotproof
spec:
  integration: lab
SPEC  networks:
    - name: edge
      subnet: 10.20.0.0/24
NET  resources:
    - name: web
      blueprint: provision-vm
      network: edge
`

func controlPlaneManifest(spec, network string) []byte {
	src := strings.Replace(controlPlaneBase, "SPEC", spec, 1)
	return []byte(strings.Replace(src, "NET", network, 1))
}

const (
	specTailnet = "  control_plane:\n    url: http://100.80.85.32:8080\n    grpc: 100.80.85.32:50051\n"
	netLAN      = "      control_plane:\n        url: http://10.20.0.1:8080\n        grpc: 10.20.0.1:50051\n"
)

// Set at either level, it decodes, and the round trip is a fixed point.
func TestControlPlaneDecodesAndRoundTrips(t *testing.T) {
	env, err := Parse(controlPlaneManifest(specTailnet, netLAN))
	if err != nil {
		t.Fatal(err)
	}
	if want := (&ControlPlane{URL: "http://100.80.85.32:8080", GRPC: "100.80.85.32:50051"}); !reflect.DeepEqual(env.Spec.ControlPlane, want) {
		t.Fatalf("spec control plane = %+v, want %+v", env.Spec.ControlPlane, want)
	}
	if want := (&ControlPlane{URL: "http://10.20.0.1:8080", GRPC: "10.20.0.1:50051"}); !reflect.DeepEqual(env.Spec.Networks[0].ControlPlane, want) {
		t.Fatalf("network control plane = %+v, want %+v", env.Spec.Networks[0].ControlPlane, want)
	}
	doc, err := EncodeEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	back, err := DecodeEnvironment(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, env) {
		t.Fatalf("round trip changed the environment:\n got %+v\nwant %+v", back, env)
	}
}

// Unset, nothing changes: a manifest written before the field existed decodes
// to no route and re-encodes without one.
func TestControlPlaneUnsetStaysUnset(t *testing.T) {
	env, err := Parse(controlPlaneManifest("", ""))
	if err != nil {
		t.Fatal(err)
	}
	if env.Spec.ControlPlane != nil || env.Spec.Networks[0].ControlPlane != nil {
		t.Fatalf("control planes = %+v, %+v; want none", env.Spec.ControlPlane, env.Spec.Networks[0].ControlPlane)
	}
	doc, err := EncodeEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(nodeText(t, doc), "control_plane") {
		t.Fatal("an Environment without a route was encoded with one")
	}
}

func TestControlPlaneRefusesWhatIsNotARoute(t *testing.T) {
	for name, block := range map[string]string{
		"url only":      "  control_plane:\n    url: http://10.0.0.1:8080\n",
		"grpc only":     "  control_plane:\n    grpc: 10.0.0.1:50051\n",
		"no scheme":     "  control_plane:\n    url: 10.0.0.1:8080\n    grpc: 10.0.0.1:50051\n",
		"ftp":           "  control_plane:\n    url: ftp://10.0.0.1\n    grpc: 10.0.0.1:50051\n",
		"a path":        "  control_plane:\n    url: http://10.0.0.1:8080/sybil\n    grpc: 10.0.0.1:50051\n",
		"credentials":   "  control_plane:\n    url: http://u:p@10.0.0.1:8080\n    grpc: 10.0.0.1:50051\n",
		"grpc no port":  "  control_plane:\n    url: http://10.0.0.1:8080\n    grpc: 10.0.0.1\n",
		"grpc bad port": "  control_plane:\n    url: http://10.0.0.1:8080\n    grpc: 10.0.0.1:99999\n",
		"unknown field": "  control_plane:\n    url: http://10.0.0.1:8080\n    grpc: 10.0.0.1:50051\n    via: tailnet\n",
	} {
		if _, err := Parse(controlPlaneManifest(block, "")); err == nil {
			t.Errorf("%s: accepted", name)
		} else if !strings.Contains(err.Error(), "control_plane") && !strings.Contains(err.Error(), "via") {
			t.Errorf("%s: error does not name the field: %v", name, err)
		}
	}
	half := "      control_plane:\n        url: http://10.20.0.1:8080\n"
	if _, err := Parse(controlPlaneManifest("", half)); err == nil || !strings.Contains(err.Error(), `network "edge"`) {
		t.Fatalf("half a network route: err = %v", err)
	}
}

// accrete enrolls nothing, so its manifest names no route, at either level.
func TestAStandaloneManifestSetsNoControlPlane(t *testing.T) {
	spec := strings.Replace(standaloneBase, "  group: web\n", "  group: web\n"+specTailnet, 1)
	if _, err := ParseStandalone([]byte(spec)); err == nil || !strings.Contains(err.Error(), "spec.control_plane") {
		t.Fatalf("spec level: err = %v", err)
	}
	net := strings.Replace(standaloneBase, "      subnet: 10.20.0.0/24\n", "      subnet: 10.20.0.0/24\n"+netLAN, 1)
	if _, err := ParseStandalone([]byte(net)); err == nil || !strings.Contains(err.Error(), `network "appnet" control_plane`) {
		t.Fatalf("network level: err = %v", err)
	}
}

func TestOnlyTheSybilSchemaDescribesControlPlane(t *testing.T) {
	spec := func(s map[string]any) map[string]any {
		return s["properties"].(map[string]any)["spec"].(map[string]any)["properties"].(map[string]any)
	}
	netProps := func(s map[string]any) map[string]any {
		return spec(s)["networks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	}
	sybil, standalone := EnvironmentJSONSchema(), StandaloneJSONSchema()
	if spec(sybil)["control_plane"] == nil || netProps(sybil)["control_plane"] == nil {
		t.Fatal("the sybil/v1alpha1 schema does not describe control_plane")
	}
	if spec(standalone)["control_plane"] != nil || netProps(standalone)["control_plane"] != nil {
		t.Fatal("the accretion/v1alpha1 schema describes control_plane")
	}
}

func nodeText(t *testing.T, doc Document) string {
	t.Helper()
	out, err := yaml.Marshal(&doc.Spec)
	if err != nil {
		t.Fatal(err)
	}
	return string(out)
}
