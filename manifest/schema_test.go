package manifest

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Every key the decoder accepts is in the schema, so an editor never flags a
// key Sybil would take, and a key added to a wire struct cannot be forgotten
// here.
func TestTheSchemaCoversEveryManifestKey(t *testing.T) {
	schema := EnvironmentJSONSchema()
	if _, err := json.Marshal(schema); err != nil {
		t.Fatalf("not JSON: %v", err)
	}
	props := func(path ...string) map[string]any {
		node := schema
		for _, p := range path {
			node = node[p].(map[string]any)
		}
		return node["properties"].(map[string]any)
	}
	spec := props("properties", "spec")
	networks := spec["networks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	resources := spec["resources"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)

	for _, c := range []struct {
		wire  any
		props map[string]any
	}{
		{environmentSpecWire{}, spec},
		{environmentNetworkWire{}, networks},
		{environmentResourceWire{}, resources},
		{placementWire{}, resources["placement"].(map[string]any)["properties"].(map[string]any)},
	} {
		typ := reflect.TypeOf(c.wire)
		for i := 0; i < typ.NumField(); i++ {
			key := strings.Split(typ.Field(i).Tag.Get("yaml"), ",")[0]
			if _, ok := c.props[key]; !ok {
				t.Errorf("%s.%s (yaml %q) is not in the schema", typ.Name(), typ.Field(i).Name, key)
			}
		}
	}
}

// The schema itself says what the decoder says about a manifest that names
// the wrong apiVersion: a const, not a suggestion.
func TestTheSchemaPinsTheEnvelope(t *testing.T) {
	props := EnvironmentJSONSchema()["properties"].(map[string]any)
	if props["apiVersion"].(map[string]any)["const"] != EnvironmentV1Alpha1 || props["kind"].(map[string]any)["const"] != KindEnvironment {
		t.Fatalf("envelope = %v %v", props["apiVersion"], props["kind"])
	}
}
