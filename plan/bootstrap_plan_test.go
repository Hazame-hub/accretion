package plan

import (
	"strings"
	"testing"
)

func TestABootstrapProfileIsCheckedAtPlanTime(t *testing.T) {
	bp := vmBlueprint
	bp.Inputs = append(append([]BlueprintInput{}, vmBlueprint.Inputs...),
		BlueprintInput{Name: "bootstrap", Type: BootstrapInputType})
	w := world()
	w.Blueprints = []Blueprint{bp}

	r := web(1)
	r.Inputs = map[string]any{"hostname": "web-1", "bootstrap": map[string]any{"users": []any{map[string]any{"name": "root"}}}}
	if p := Build(env(r), w); !strings.Contains(strings.Join(p.Errors, " "), "root cannot be redefined") {
		t.Fatalf("errors = %v", p.Errors)
	}
	r.Inputs["bootstrap"] = map[string]any{"users": []any{map[string]any{"name": "deploy"}}, "packages": []any{"htop"}}
	if p := Build(env(r), w); !p.OK() {
		t.Fatalf("a good profile: %v", p.Errors)
	}
}
