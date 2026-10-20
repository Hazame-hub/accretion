package plan

import (
	"strings"
	"testing"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

func TestVariablesFillInputsKeepingTheirType(t *testing.T) {
	r := web(2)
	r.Inputs = map[string]any{
		"hostname":  "{{var.prefix}}-{{instance.number}}",
		"memory_mb": "{{var.memory}}",
		"bootstrap": map[string]any{"packages": []any{"{{var.tool}}"}},
	}
	e := env(r)
	e.Spec.Variables = map[string]any{"prefix": "stg", "memory": 4096, "tool": "htop"}
	instances, errs := Expand(e)
	if len(errs) > 0 {
		t.Fatalf("errors: %v", errs)
	}
	in := instances[1].Inputs
	if in["hostname"] != "stg-2" || in["memory_mb"] != 4096 {
		t.Fatalf("inputs = %v", in)
	}
	if pk := in["bootstrap"].(map[string]any)["packages"].([]any)[0]; pk != "htop" {
		t.Fatalf("nested = %v", pk)
	}
}

func TestAnUnknownVariableIsAnErrorNotABlank(t *testing.T) {
	r := web(1)
	r.Inputs = map[string]any{"hostname": "{{var.prefx}}-1"}
	_, errs := Expand(env(r))
	if len(errs) != 1 || !strings.Contains(errs[0], "{{var.prefx}}") {
		t.Fatalf("errors = %v", errs)
	}
}

func TestAnImageFromAVariable(t *testing.T) {
	e := imageEnv("{{var.os}}", nil)
	e.Spec.Variables = map[string]any{"os": "rocky-9"}
	w := imageWorld(nil)
	qs := ImageQueries(e, w)
	if len(qs) != 1 || qs[0].Name != "rocky-9" {
		t.Fatalf("queries = %+v", qs)
	}
	w.Images = map[ImageQuery]ImageAnswer{qs[0]: {Value: float64(103)}}
	if p := Build(e, w); !p.OK() {
		t.Fatalf("errors = %v", p.Errors)
	}
	e.Spec.Variables = map[string]any{"os": "not an image!"}
	if p := Build(e, imageWorld(nil)); !strings.Contains(strings.Join(p.Errors, " "), `has image "not an image!"`) {
		t.Fatalf("errors = %v", p.Errors)
	}
	_ = Blueprint{}
}

func TestVariableNamesAndValuesAreChecked(t *testing.T) {
	e := env(web(1))
	e.Spec.Variables = map[string]any{"bad-name": "x"}
	if err := e.Validate(); err == nil {
		t.Fatal("a variable name with a dash was accepted")
	}
	e.Spec.Variables = map[string]any{"list": []any{"a"}}
	if err := e.Validate(); err == nil {
		t.Fatal("a list variable was accepted")
	}
	_ = envmanifest.VariableName
}
