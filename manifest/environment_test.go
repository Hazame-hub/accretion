package manifest

import (
	"reflect"
	"strings"
	"testing"
)

// A complete, valid manifest. Tests that need an invalid one edit a copy of
// this, so what each case is actually testing is the line it changed.
const envManifest = `apiVersion: sybil/v1alpha1
kind: Environment
metadata:
  name: pve-verification
spec:
  integration: pve-lab
  group: pve-verification
  resources:
    - name: verify
      count: 3
      blueprint: provision-from-schema
      inputs:
        cores: 2
        memoryMb: 2048
    - name: gateway
      blueprint: provision-from-schema
`

func decodeOneEnv(t *testing.T, src string) (Environment, error) {
	t.Helper()
	docs, err := DecodeDocuments(strings.NewReader(src))
	if err != nil {
		return Environment{}, err
	}
	if len(docs) != 1 {
		t.Fatalf("expected 1 document, got %d", len(docs))
	}
	return DecodeEnvironment(docs[0])
}

func mustDecodeEnv(t *testing.T, src string) Environment {
	t.Helper()
	env, err := decodeOneEnv(t, src)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	return env
}

func TestAnEnvironmentManifestDecodesToItsDeclaredIntent(t *testing.T) {
	env := mustDecodeEnv(t, envManifest)

	if env.Name != "pve-verification" {
		t.Errorf("name = %q", env.Name)
	}
	if env.Spec.Integration != "pve-lab" || env.Spec.Group != "pve-verification" {
		t.Errorf("integration/group = %q/%q", env.Spec.Integration, env.Spec.Group)
	}
	if len(env.Spec.Resources) != 2 {
		t.Fatalf("resources = %d, want 2", len(env.Spec.Resources))
	}
	// Declaration order is the operator's reading order and is preserved.
	if env.Spec.Resources[0].Name != "verify" || env.Spec.Resources[1].Name != "gateway" {
		t.Errorf("resource order = %v", env.ResourceNames())
	}
	if got := env.Spec.Resources[0].Count; got != 3 {
		t.Errorf("explicit count = %d, want 3", got)
	}
	// An omitted count means one, which is the only reading that makes a
	// countless resource mean anything at all.
	if got := env.Spec.Resources[1].Count; got != 1 {
		t.Errorf("omitted count = %d, want 1", got)
	}
	if got := env.Total(); got != 4 {
		t.Errorf("Total() = %d, want 4", got)
	}
	if got := env.Spec.Resources[0].Inputs["cores"]; got != 2 {
		t.Errorf("inputs.cores = %v (%T)", got, got)
	}
}

// The round trip has to be a fixed point and not merely reversible, so that a
// plan computed from a manifest read twice is the same plan both times.
//
// "Fixed point" is a claim about the decoded value, not about the file: the
// normalized form states a count the manifest left implicit, so the emitted
// YAML deliberately differs from the input — this test asserts that difference
// at the end. TestAnEnvironmentsNormalizedFormIsWhatDecodeAlreadyProduced
// covers the inputs that used to break the value-level claim.
func TestDecodingAnEncodedEnvironmentIsAFixedPoint(t *testing.T) {
	first := mustDecodeEnv(t, envManifest)

	doc, err := EncodeEnvironment(first)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if doc.APIVersion != EnvironmentV1Alpha1 || doc.Kind != KindEnvironment {
		t.Fatalf("envelope = %s/%s", doc.APIVersion, doc.Kind)
	}

	second, err := DecodeEnvironment(doc)
	if err != nil {
		t.Fatalf("decode round two: %v", err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Errorf("round trip drifted:\n first = %+v\nsecond = %+v", first, second)
	}

	// And again through the YAML text, which is what actually gets committed.
	var buf strings.Builder
	if err := EncodeDocuments(&buf, []Document{doc}); err != nil {
		t.Fatalf("encode bundle: %v", err)
	}
	third := mustDecodeEnv(t, buf.String())
	if !reflect.DeepEqual(first, third) {
		t.Errorf("round trip through YAML drifted:\n first = %+v\nthird = %+v", first, third)
	}
	// The normalized form states the count that was left implicit, so the
	// second read does not have to know the default to agree with the first.
	if !strings.Contains(buf.String(), "count: 1") {
		t.Errorf("normalized YAML does not state the defaulted count:\n%s", buf.String())
	}
}

func TestAnEnvironmentManifestIsRejectedWhenItIsNotWhatItClaims(t *testing.T) {
	cases := []struct {
		name string
		edit func(string) string
		want string
	}{
		{
			// An unversioned manifest cannot be migrated later, because
			// nothing records which shape it was written against.
			name: "no apiVersion",
			edit: func(s string) string { return strings.Replace(s, "apiVersion: sybil/v1alpha1\n", "", 1) },
			want: "apiVersion is required",
		},
		{
			name: "future apiVersion",
			edit: func(s string) string { return strings.Replace(s, "sybil/v1alpha1", "sybil/v1", 1) },
			want: "unsupported apiVersion",
		},
		{
			name: "no integration",
			edit: func(s string) string { return strings.Replace(s, "  integration: pve-lab\n", "", 1) },
			want: "spec.integration is required",
		},
		{
			name: "resource with no blueprint",
			edit: func(s string) string { return strings.Replace(s, "      blueprint: provision-from-schema\n", "", 1) },
			want: "has no blueprint",
		},
		{
			name: "negative count",
			edit: func(s string) string { return strings.Replace(s, "count: 3", "count: -2", 1) },
			want: "want 0 or more",
		},
		{
			// Resource names are intent identity, so a duplicate makes two
			// different intents indistinguishable.
			name: "duplicate resource name",
			edit: func(s string) string { return strings.Replace(s, "name: gateway", "name: verify", 1) },
			want: "repeats the name",
		},
		{
			name: "resource with no name",
			edit: func(s string) string { return strings.Replace(s, "    - name: gateway\n", "    - \n", 1) },
			want: "has no name",
		},
		{
			// A typo in the spec must be an error, not a dropped field.
			name: "misspelled spec field",
			edit: func(s string) string { return strings.Replace(s, "  integration:", "  integraton:", 1) },
			want: "field integraton not found",
		},
		{
			// And a typo in the envelope likewise, which is what the strict
			// DecodeDocuments buys: without it this reads as "no apiVersion",
			// reported far from the line that is actually wrong.
			name: "misspelled envelope field",
			edit: func(s string) string { return strings.Replace(s, "apiVersion:", "apiVersoin:", 1) },
			want: "field apiVersoin not found",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeOneEnv(t, tc.edit(envManifest))
			if err == nil {
				t.Fatalf("expected rejection, got none")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestADocumentOfAnotherKindIsNotAnEnvironment(t *testing.T) {
	_, err := DecodeEnvironment(Document{
		APIVersion: EnvironmentV1Alpha1,
		Kind:       "Script",
		Metadata:   Metadata{Name: "n"},
	})
	if err == nil || !strings.Contains(err.Error(), "expected kind Environment") {
		t.Fatalf("err = %v", err)
	}
}

func TestAnEnvironmentNamesWhatItDependsOn(t *testing.T) {
	env := mustDecodeEnv(t, envManifest)

	want := []Reference{
		{Kind: RefIntegration, Name: "pve-lab"},
		{Kind: RefGroup, Name: "pve-verification"},
		// Two resources, one blueprint: one dependency, not two.
		{Kind: RefProvisionBlueprint, Name: "provision-from-schema"},
	}
	if got := env.References(); !reflect.DeepEqual(got, want) {
		t.Errorf("References() = %v, want %v", got, want)
	}
	for _, r := range env.References() {
		if !r.Valid() {
			t.Errorf("%v is not a valid reference", r)
		}
		// Every reference must survive the text form it is written in.
		parsed, err := ParseReference(r.String())
		if err != nil || parsed != r {
			t.Errorf("ParseReference(%q) = %v, %v", r.String(), parsed, err)
		}
	}
}

func TestAnEnvironmentWithNoGroupNamesOnlyWhatItHas(t *testing.T) {
	env := mustDecodeEnv(t, strings.Replace(envManifest, "  group: pve-verification\n", "", 1))
	if env.Spec.Group != "" {
		t.Fatalf("group = %q, want empty", env.Spec.Group)
	}
	for _, r := range env.References() {
		if r.Kind == RefGroup {
			t.Errorf("References() invented a group reference: %v", r)
		}
	}
}

// The fixed-point claim used to be false, and both counterexamples came out of
// DecodeEnvironment itself rather than from anything exotic. They are pinned
// here by value and by emitted text, because a comment asserting a property is
// worth nothing next to the input that breaks it.
func TestAnEnvironmentsNormalizedFormIsWhatDecodeAlreadyProduced(t *testing.T) {
	cases := []struct {
		name string
		// inputs is spliced in under the single resource, already indented.
		inputs string
		want   map[string]any
	}{
		{
			// An empty map decoded to a non-nil empty map while `omitempty`
			// dropped the key on the way out, so the second read saw nil and
			// the two values disagreed. Absent and empty are now the same
			// thing, which is the only reading that survives a round trip.
			name:   "an empty inputs map is the same as no inputs at all",
			inputs: "      inputs: {}\n",
			want:   nil,
		},
		{
			name:   "no inputs at all",
			inputs: "",
			want:   nil,
		},
		{
			// 1.0 decoded to float64(1), re-emitted as `1`, and read back as
			// int(1) — so the type and the file both changed. Decode now
			// applies that conversion once, up front.
			name:   "an integral float is already an int",
			inputs: "      inputs:\n        v: 1.0\n",
			want:   map[string]any{"v": 1},
		},
		{
			// A non-integral float has nothing to collapse and stays a float.
			name:   "a fractional float stays a float",
			inputs: "      inputs:\n        v: 2.5\n",
			want:   map[string]any{"v": 2.5},
		},
		{
			// Normalization has to reach inside nested values, or a float one
			// level down reintroduces the drift.
			name:   "nesting is normalized too",
			inputs: "      inputs:\n        outer:\n          v: 3.0\n        list:\n          - 4.0\n",
			want: map[string]any{
				"outer": map[string]any{"v": 3},
				"list":  []any{4},
			},
		},
		{
			// A quoted numeric-looking value is a string and must stay one,
			// or a version input silently becomes arithmetic.
			name:   "a quoted number stays a string",
			inputs: "      inputs:\n        v: \"1.0\"\n",
			want:   map[string]any{"v": "1.0"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			src := "apiVersion: sybil/v1alpha1\nkind: Environment\nmetadata:\n  name: e\n" +
				"spec:\n  integration: i\n  resources:\n    - name: r\n      blueprint: b\n" + tc.inputs
			first := mustDecodeEnv(t, src)
			if got := first.Spec.Resources[0].Inputs; !reflect.DeepEqual(got, tc.want) {
				t.Errorf("decoded inputs = %#v, want %#v", got, tc.want)
			}

			doc, err := EncodeEnvironment(first)
			if err != nil {
				t.Fatalf("encode: %v", err)
			}
			second, err := DecodeEnvironment(doc)
			if err != nil {
				t.Fatalf("decode round two: %v", err)
			}
			if !reflect.DeepEqual(first, second) {
				t.Errorf("not a fixed point:\n first = %#v\nsecond = %#v", first, second)
			}

			// And through the text, twice, because it is the second emission
			// that would reveal a value that only looks settled.
			var once strings.Builder
			if err := EncodeDocuments(&once, []Document{doc}); err != nil {
				t.Fatalf("encode bundle: %v", err)
			}
			redoc, err := EncodeEnvironment(mustDecodeEnv(t, once.String()))
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			var twice strings.Builder
			if err := EncodeDocuments(&twice, []Document{redoc}); err != nil {
				t.Fatalf("re-encode bundle: %v", err)
			}
			if once.String() != twice.String() {
				t.Errorf("emitted YAML is not stable:\n once:\n%s\n twice:\n%s", once.String(), twice.String())
			}
		})
	}
}

// A count large enough to overflow Total() must be refused, or a manifest
// asking for an impossible number of machines reports asking for fewer than
// none. MaxResourceCount is a typo guard, not a quota — the real ceiling is the
// provider's and is enforced at provision time.
func TestAnEnvironmentRefusesACountNothingCouldSatisfy(t *testing.T) {
	for _, count := range []string{"9223372036854775807", "1001"} {
		t.Run(count, func(t *testing.T) {
			src := strings.Replace(envManifest, "count: 3", "count: "+count, 1)
			_, err := decodeOneEnv(t, src)
			if err == nil {
				t.Fatalf("count %s was accepted", count)
			}
			if !strings.Contains(err.Error(), "want at most") {
				t.Errorf("error = %q, want it to mention the bound", err)
			}
		})
	}

	// The bound itself is allowed: an off-by-one here would silently cap real
	// environments one machine short.
	env := mustDecodeEnv(t, strings.Replace(envManifest, "count: 3", "count: 1000", 1))
	if got := env.Spec.Resources[0].Count; got != MaxResourceCount {
		t.Errorf("count at the bound = %d, want %d", got, MaxResourceCount)
	}
	if got, want := env.Total(), MaxResourceCount+1; got != want {
		t.Errorf("Total() = %d, want %d", got, want)
	}
}

// EncodeEnvironment used to trust its argument, so a hand-built Environment
// emitted a document its own decoder rejects. The planner builds these, so the
// invariant has to be checked where it is claimed rather than assumed.
func TestEncodingRefusesAnEnvironmentThatWasNeverValid(t *testing.T) {
	valid := mustDecodeEnv(t, envManifest)

	cases := []struct {
		name string
		env  Environment
		want string
	}{
		{"no name", Environment{Spec: valid.Spec}, "metadata.name"},
		{"no integration", Environment{Name: "e"}, "spec.integration is required"},
		{
			"a resource with no blueprint",
			Environment{Name: "e", Spec: EnvironmentSpec{
				Integration: "i",
				Resources:   []EnvironmentResource{{Name: "r", Count: 1}},
			}},
			"has no blueprint",
		},
		{
			"a resource with a negative count",
			Environment{Name: "e", Spec: EnvironmentSpec{
				Integration: "i",
				Resources:   []EnvironmentResource{{Name: "r", Blueprint: "b", Count: -1}},
			}},
			"want 0..",
		},
		{
			"two resources with one name",
			Environment{Name: "e", Spec: EnvironmentSpec{
				Integration: "i",
				Resources: []EnvironmentResource{
					{Name: "r", Count: 1, Blueprint: "b"},
					{Name: "r", Count: 1, Blueprint: "b"},
				},
			}},
			"repeats the name",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := EncodeEnvironment(tc.env); err == nil {
				t.Fatalf("encoded an invalid Environment without complaint")
			} else if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error = %q, want it to mention %q", err, tc.want)
			}
			if err := tc.env.Validate(); err == nil {
				t.Errorf("Validate() accepted it")
			}
		})
	}

	// And the decoded one passes, so the check is not merely refusing everything.
	if err := valid.Validate(); err != nil {
		t.Errorf("a decoded Environment failed Validate(): %v", err)
	}
}

// An Environment that declares nothing, or a resource with count 0, is how one
// is emptied: it must decode, encode and decode again to the same value, so a
// stored emptied manifest reads back for intent_check and the next plan.
func TestAnEmptyEnvironmentIsValid(t *testing.T) {
	empty := mustDecodeEnv(t, envManifest[:strings.Index(envManifest, "  resources:")])
	if empty.Declares() || empty.Total() != 0 {
		t.Errorf("an Environment with no resources declares something: total %d", empty.Total())
	}
	zero := mustDecodeEnv(t, strings.Replace(envManifest, "count: 3", "count: 0", 1))
	if got := zero.Spec.Resources[0].Count; got != 0 {
		t.Errorf("explicit count 0 decoded as %d", got)
	}
	if zero.Total() != 1 {
		t.Errorf("total = %d, want 1 (the gateway)", zero.Total())
	}

	emptied := mustDecodeEnv(t, envManifest).Emptied()
	if emptied.Name != "pve-verification" || emptied.Spec.Integration != "pve-lab" || emptied.Declares() || emptied.Spec.Group != "" {
		t.Errorf("Emptied() = %+v, want the name and connector only", emptied)
	}
	for _, env := range []Environment{emptied, zero} {
		doc, err := EncodeEnvironment(env)
		if err != nil {
			t.Fatalf("encode %+v: %v", env, err)
		}
		back, err := DecodeEnvironment(doc)
		if err != nil {
			t.Fatalf("decode the encoded form: %v", err)
		}
		if back.Total() != env.Total() || len(back.Spec.Resources) != len(env.Spec.Resources) {
			t.Errorf("round trip changed it: %+v, want %+v", back, env)
		}
	}
}
