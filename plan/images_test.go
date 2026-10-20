package plan

import (
	"strings"
	"testing"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

var cloneBlueprint = Blueprint{
	ID: "provision-vm",
	Inputs: []BlueprintInput{
		{Name: "integration", Type: "string", Required: true, Source: InputSourceIntegrations},
		{Name: "hostname", Type: "string", Required: true},
		{Name: "pve_node", Type: "string", Default: "pve", Selector: "node"},
		{Name: "template_vmid", Type: "string", Required: true, Selector: "template", TakesImage: true},
	},
}

var templateSelector = Selector{
	Name: "template", Action: "vm_list_templates", List: "templates", Value: "vmid", LabelKey: "name",
	RefKey: "ref", DependsOn: []string{"node"},
}

const rockyDigest = "6b1b2b6c5f0e4c0a9d6a5f1e2d3c4b5a69788766554433221100ffeeddccbbaa"

func imageWorld(answers map[ImageQuery]ImageAnswer) Snapshot {
	w := world()
	w.Blueprints = []Blueprint{cloneBlueprint}
	w.Selectors = []Selector{templateSelector}
	w.Images = answers
	return w
}

func imageEnv(image string, inputs map[string]any) envmanifest.Environment {
	r := web(2)
	r.Image = image
	r.Inputs = map[string]any{"hostname": "web-{{instance.number}}"}
	for k, v := range inputs {
		r.Inputs[k] = v
	}
	return env(r)
}

func TestAnImageByNameFillsTheTemplateInput(t *testing.T) {
	e := imageEnv("rocky-9", nil)
	w := imageWorld(nil)
	qs := ImageQueries(e, w)
	if len(qs) != 1 || qs[0] != (ImageQuery{Name: "rocky-9", Selector: "template", Params: "node=pve"}) {
		t.Fatalf("queries = %+v, want one lookup on the default node", qs)
	}
	w.Images = map[ImageQuery]ImageAnswer{qs[0]: {Value: float64(103)}}
	p := Build(e, w)
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	for _, en := range p.Entries {
		if en.Inputs["template_vmid"] != "103" {
			t.Fatalf("template_vmid = %#v", en.Inputs["template_vmid"])
		}
	}
	if !strings.Contains(strings.Join(p.Limits, " "), "named without a digest") {
		t.Errorf("an unpinned image must be said: %v", p.Limits)
	}
}

func TestAPinnedImageMustBeTheOneSybilBuilt(t *testing.T) {
	q := ImageQuery{Name: "rocky-9", Selector: "template", Params: "node=pve"}
	pinned := "rocky-9@sha256:" + rockyDigest
	cases := map[string]struct {
		answer ImageAnswer
		err    string
	}{
		"matches":       {ImageAnswer{Value: float64(103), SHA256: rockyDigest}, ""},
		"other content": {ImageAnswer{Value: float64(103), SHA256: strings.Repeat("a", 64)}, "was built from sha256:aaaaaaaaaaaa"},
		"not ours":      {ImageAnswer{Value: float64(103)}, "content is not known"},
		"missing":       {ImageAnswer{Err: "there is no template of that name (node=pve)"}, "there is no template of that name"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			p := Build(imageEnv(pinned, nil), imageWorld(map[ImageQuery]ImageAnswer{q: c.answer}))
			got := strings.Join(p.Errors, " | ")
			if c.err == "" {
				if !p.OK() || strings.Contains(strings.Join(p.Limits, " "), "named without a digest") {
					t.Fatalf("errors %v limits %v", p.Errors, p.Limits)
				}
				return
			}
			if !strings.Contains(got, c.err) {
				t.Fatalf("errors = %s, want %q", got, c.err)
			}
		})
	}
}

func TestAnImageAndTheInputItFillsCannotBothBeGiven(t *testing.T) {
	p := Build(imageEnv("rocky-9", map[string]any{"template_vmid": "103"}), imageWorld(nil))
	if !strings.Contains(strings.Join(p.Errors, " "), "sets both image and inputs.template_vmid") {
		t.Fatalf("errors = %v", p.Errors)
	}
	// One instance per node: two nodes are two lookups.
	r := web(2)
	r.Image = "rocky-9"
	r.Inputs = map[string]any{"hostname": "web-{{instance.number}}", "pve_node": "pve-{{instance.number}}"}
	if qs := ImageQueries(env(r), imageWorld(nil)); len(qs) != 2 {
		t.Fatalf("queries = %+v", qs)
	}
}

func TestImageSyntax(t *testing.T) {
	for _, ok := range []string{"rocky-9", "debian_12.5", "rocky-9@sha256:" + rockyDigest} {
		if !envmanifest.ImagePattern.MatchString(ok) {
			t.Errorf("%q refused", ok)
		}
	}
	for _, bad := range []string{"", "-x", "rocky 9", "rocky@sha256:abc", "rocky@sha512:" + rockyDigest} {
		if envmanifest.ImagePattern.MatchString(bad) {
			t.Errorf("%q accepted", bad)
		}
	}
	if n, d := envmanifest.SplitImage("rocky-9@sha256:" + rockyDigest); n != "rocky-9" || d != rockyDigest {
		t.Errorf("split = %q %q", n, d)
	}
}
