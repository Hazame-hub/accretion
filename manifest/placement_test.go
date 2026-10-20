package manifest

import (
	"reflect"
	"strings"
	"testing"
)

const placementManifest = `apiVersion: sybil/v1alpha1
kind: Environment
metadata:
  name: okd
spec:
  integration: pve-lab
  resources:
    - name: db
      count: 2
      blueprint: provision-proxmox-vm
      placement:
        spread: true
        nodes: [pve-01, pve-02]
    - name: web
      blueprint: provision-proxmox-vm
      placement:
        node: pve-03
        apart_from: [db]
    - name: tools
      blueprint: provision-proxmox-vm
`

func TestAPlacementDecodesWithAutoAsItsDefault(t *testing.T) {
	env := mustDecodeEnv(t, placementManifest)
	db, web, tools := env.Spec.Resources[0], env.Spec.Resources[1], env.Spec.Resources[2]
	if want := (&Placement{Node: PlacementAuto, Nodes: []string{"pve-01", "pve-02"}, Spread: true}); !reflect.DeepEqual(db.Placement, want) {
		t.Errorf("db placement = %+v, want %+v", db.Placement, want)
	}
	if web.Placement == nil || web.Placement.Auto() || web.Placement.Node != "pve-03" ||
		!reflect.DeepEqual(web.Placement.ApartFrom, []string{"db"}) {
		t.Errorf("web placement = %+v", web.Placement)
	}
	if tools.Placement != nil {
		t.Errorf("a resource without placement has none, got %+v", tools.Placement)
	}
}

func TestAPlacementRoundTrips(t *testing.T) {
	env := mustDecodeEnv(t, placementManifest)
	doc, err := EncodeEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecodeEnvironment(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(env, again) {
		t.Errorf("round trip changed the placement:\n%+v\n%+v", env.Spec.Resources, again.Spec.Resources)
	}
}

func TestAPlacementThatCannotMeanAnythingIsRefused(t *testing.T) {
	for _, c := range []struct{ name, from, to, want string }{
		{"apart from an undeclared resource", "apart_from: [db]", "apart_from: [cache]", `apart_from "cache", which this environment does not declare`},
		{"apart from itself", "apart_from: [db]", "apart_from: [web]", "apart_from itself"},
		{"apart from twice", "apart_from: [db]", "apart_from: [db, db]", `repeats "db"`},
		{"a fixed node limited to nodes", "node: pve-03", "node: pve-03\n        nodes: [pve-01]", "goes with node: auto"},
		{"a node listed twice", "nodes: [pve-01, pve-02]", "nodes: [pve-01, pve-01]", `repeats node "pve-01"`},
		{"auto as a node name", "nodes: [pve-01, pve-02]", "nodes: [auto]", "want node names"},
		{"an unknown key", "spread: true", "spread: true\n        anti_affinity: true", "anti_affinity"},
	} {
		t.Run(c.name, func(t *testing.T) {
			src := strings.Replace(placementManifest, c.from, c.to, 1)
			if src == placementManifest {
				t.Fatalf("the edit %q did not apply", c.from)
			}
			_, err := decodeOneEnv(t, src)
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want it to mention %q", err, c.want)
			}
		})
	}
}
