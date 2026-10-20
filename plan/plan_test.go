package plan

import (
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

var (
	labID   = uuid.MustParse("00000000-0000-0000-0000-00000000aaaa")
	otherID = uuid.MustParse("00000000-0000-0000-0000-00000000bbbb")
)

// vmBlueprint is the shape that matters: a connector input sourced from
// integrations, a required hostname, and an optional input with a default.
var vmBlueprint = Blueprint{
	ID: "provision-vm",
	Inputs: []BlueprintInput{
		{Name: "integration", Type: "string", Required: true, Source: InputSourceIntegrations},
		{Name: "hostname", Type: "string", Required: true},
		{Name: "memory_mb", Type: "int", Default: 2048},
	},
}

func env(resources ...envmanifest.EnvironmentResource) envmanifest.Environment {
	return envmanifest.Environment{Name: "staging", Spec: envmanifest.EnvironmentSpec{Integration: "lab", Resources: resources}}
}

func web(count int) envmanifest.EnvironmentResource {
	return envmanifest.EnvironmentResource{Name: "web", Count: count, Blueprint: "provision-vm",
		Inputs: map[string]any{"hostname": "web-{{instance.number}}"}}
}

func world(rs ...Realization) Snapshot {
	return Snapshot{
		Connector:    &Connector{ID: labID, Name: "lab", Type: "proxmox"},
		Blueprints:   []Blueprint{vmBlueprint},
		Realizations: rs,
		Verdicts:     map[uuid.UUID]string{},
	}
}

func realized(resource string, index int, asset *uuid.UUID) Realization {
	return Realization{
		ID: uuid.New(), IntegrationID: labID, AssetID: asset, ResourceRefID: "pve/qemu/" + resource,
		ResourceKind: "vm", Managed: true,
		Intent: &RealizationIntent{Kind: IntentKind, Name: "staging", ResourceName: resource, InstanceIndex: index},
	}
}

func ptr(id uuid.UUID) *uuid.UUID { return &id }

func actions(p Plan) string {
	var parts []string
	for _, e := range p.Entries {
		parts = append(parts, e.Resource+"/"+string(rune('0'+e.Index))+"="+string(e.Action))
	}
	return strings.Join(parts, " ")
}

func TestNothingRealizedPlansACreatePerInstanceWithExpandedInputs(t *testing.T) {
	p := Build(env(web(3)), world())
	if !p.OK() {
		t.Fatalf("errors: %v", p.Errors)
	}
	if got := actions(p); got != "web/0=create web/1=create web/2=create" {
		t.Fatalf("actions = %s", got)
	}
	for i, want := range []string{"web-1", "web-2", "web-3"} {
		if h := p.Entries[i].Inputs["hostname"]; h != want {
			t.Errorf("instance %d hostname = %v, want %s", i, h, want)
		}
		if c := p.Entries[i].Inputs["integration"]; c != labID.String() {
			t.Errorf("instance %d connector = %v, want the resolved connector id", i, c)
		}
	}
	if p.Summary() != "3 to create, 0 unchanged, 0 to change, 0 to delete, 0 unknown" {
		t.Errorf("summary = %s", p.Summary())
	}
}

func TestIndexPlaceholderIsZeroBasedLikeTheRealization(t *testing.T) {
	r := web(2)
	r.Inputs = map[string]any{"hostname": "web-{{instance.index}}", "memory_mb": 4096}
	p := Build(env(r), world())
	if h := p.Entries[1].Inputs["hostname"]; h != "web-1" {
		t.Fatalf("hostname = %v, want web-1", h)
	}
	if m := p.Entries[1].Inputs["memory_mb"]; m != 4096 {
		t.Fatalf("a non-string input must pass through untouched, got %v", m)
	}
}

func TestEachEvidenceStatePlansTheHonestAction(t *testing.T) {
	accepted, rejected, incomplete, never := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	unmanaged := realized("web", 5, ptr(uuid.New()))
	unmanaged.Managed = false
	moved := realized("web", 6, ptr(accepted))
	moved.IntegrationID = otherID

	w := world(
		realized("web", 0, ptr(accepted)),
		realized("web", 1, ptr(rejected)),
		realized("web", 2, ptr(incomplete)),
		realized("web", 3, ptr(never)),
		realized("web", 4, nil),
		unmanaged,
		moved,
	)
	w.Verdicts[accepted] = AcceptanceAccepted
	w.Verdicts[rejected] = AcceptanceRejected
	w.Verdicts[incomplete] = AcceptanceIncomplete

	p := Build(env(web(8)), w)
	want := "web/0=no-op web/1=change web/2=unknown web/3=unknown web/4=unknown web/5=unknown web/6=change web/7=create"
	if got := actions(p); got != want {
		t.Fatalf("actions =\n %s\nwant\n %s", got, want)
	}
	for _, e := range p.Entries[:7] {
		if e.Reason == "" || e.RealizationID == nil {
			t.Errorf("%s/%d: an existing instance must say why and name its realization", e.Resource, e.Index)
		}
	}
	if p.Entries[0].Verdict != AcceptanceAccepted {
		t.Errorf("verdict not carried: %q", p.Entries[0].Verdict)
	}
}

func TestUndeclaredRealizationsPlanAsDeleteUnlessUnmanaged(t *testing.T) {
	a := uuid.New()
	gone := realized("db", 0, ptr(a))
	disowned := realized("cache", 0, nil)
	disowned.Managed = false
	w := world(realized("web", 0, ptr(a)), realized("web", 1, ptr(a)), gone, disowned)
	w.Verdicts[a] = AcceptanceAccepted

	p := Build(env(web(1)), w)
	want := "web/0=no-op cache/0=unknown db/0=delete web/1=delete"
	if got := actions(p); got != want {
		t.Fatalf("actions = %s, want %s", got, want)
	}
	if p.Entries[3].Blueprint != "" || p.Entries[3].Inputs != nil {
		t.Error("an undeclared instance has no blueprint or inputs to show")
	}
}

func TestOnlyThisEnvironmentsCurrentRealizationsCount(t *testing.T) {
	other := realized("web", 0, nil)
	other.Intent.Name = "production"
	noIntent := realized("web", 0, nil)
	noIntent.Intent = nil
	retired := realized("web", 0, nil)
	now := time.Now()
	retired.RetiredAt = &now

	p := Build(env(web(1)), world(other, noIntent, retired))
	if got := actions(p); got != "web/0=create" {
		t.Fatalf("actions = %s: another environment, an intent-less build and a retired row must not count", got)
	}
}

func TestPlanIsDeterministic(t *testing.T) {
	w := world(realized("b", 1, nil), realized("a", 2, nil), realized("a", 0, nil), realized("b", 0, nil))
	first := actions(Build(env(web(1)), w))
	for i := 0; i < 20; i++ {
		if got := actions(Build(env(web(1)), w)); got != first {
			t.Fatalf("run %d: %s != %s", i, got, first)
		}
	}
	if first != "web/0=create a/0=delete a/2=delete b/0=delete b/1=delete" {
		t.Fatalf("order = %s", first)
	}
}

func TestManifestProblemsAreAllReported(t *testing.T) {
	cases := []struct {
		name string
		env  envmanifest.Environment
		w    Snapshot
		want []string
	}{
		{"missing connector", env(web(1)), Snapshot{}, []string{`no connector named "lab"`}},
		{"missing group", func() envmanifest.Environment { e := env(web(1)); e.Spec.Group = "web-tier"; return e }(),
			world(), []string{`no asset group named "web-tier"`}},
		{"unknown blueprint", env(envmanifest.EnvironmentResource{Name: "x", Count: 1, Blueprint: "nope"}),
			world(), []string{`ships no blueprint "nope"`}},
		{"undeclared input", env(envmanifest.EnvironmentResource{Name: "x", Count: 1, Blueprint: "provision-vm",
			Inputs: map[string]any{"hostname": "x", "memroy_mb": 1}}), world(), []string{`has no input "memroy_mb"`}},
		{"connector as input", env(envmanifest.EnvironmentResource{Name: "x", Count: 1, Blueprint: "provision-vm",
			Inputs: map[string]any{"hostname": "x", "integration": "elsewhere"}}), world(), []string{`input "integration" is the connector`}},
		{"missing required", env(envmanifest.EnvironmentResource{Name: "x", Count: 2, Blueprint: "provision-vm",
			Inputs: map[string]any{"memory_mb": "{{instance.index}}"}}), world(), []string{`requires input "hostname"`}},
		{"identical instances", env(envmanifest.EnvironmentResource{Name: "x", Count: 2, Blueprint: "provision-vm",
			Inputs: map[string]any{"hostname": "same"}}), world(), []string{`every instance gets the same inputs`}},
		{"misspelt placeholder", env(envmanifest.EnvironmentResource{Name: "x", Count: 2, Blueprint: "provision-vm",
			Inputs: map[string]any{"hostname": "w-{{instance.nubmer}}"}}), world(), []string{`{{instance.nubmer}}`}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := Build(tc.env, tc.w)
			if p.OK() {
				t.Fatal("plan reported OK")
			}
			joined := strings.Join(p.Errors, "\n")
			for _, w := range tc.want {
				if !strings.Contains(joined, w) {
					t.Errorf("errors %q do not mention %q", joined, w)
				}
			}
		})
	}
}

func TestMissingRequiredIsReportedOncePerResource(t *testing.T) {
	r := envmanifest.EnvironmentResource{Name: "x", Count: 5, Blueprint: "provision-vm",
		Inputs: map[string]any{"memory_mb": "{{instance.number}}"}}
	p := Build(env(r), world())
	if len(p.Errors) != 1 {
		t.Fatalf("errors = %v, want one", p.Errors)
	}
}

func TestAMisspeltPlaceholderNeverPlansTheResourcesDeletion(t *testing.T) {
	r := web(2)
	r.Inputs = map[string]any{"hostname": "web-{{instance.nubmer}}"}
	p := Build(env(r), world(realized("web", 0, nil), realized("web", 1, nil)))
	if p.OK() {
		t.Fatal("the typo must be an error")
	}
	for _, e := range p.Entries {
		if e.Action == ActionDelete {
			t.Fatalf("%s/%d planned as delete because of a typo: %s", e.Resource, e.Index, actions(p))
		}
	}
}

func building(resource string, index int, env string) Run {
	return Run{ID: uuid.New(), Status: "held",
		Intent: &RealizationIntent{Kind: IntentKind, Name: env, ResourceName: resource, InstanceIndex: index}}
}

// An instance a run is already building is never planned as create: a second
// launch would be a second machine.
func TestAnInstanceInFlightIsUnknownNotCreate(t *testing.T) {
	w := world()
	run := building("web", 1, "staging")
	w.InFlight = []Run{run, building("web", 0, "production"), building("web", 5, "staging")}

	p := Build(env(web(2)), w)
	if got := actions(p); got != "web/0=create web/1=unknown web/5=unknown" {
		t.Fatalf("actions = %s", got)
	}
	if p.Entries[1].RunID == nil || *p.Entries[1].RunID != run.ID {
		t.Errorf("the in-flight entry must name its run: %+v", p.Entries[1])
	}
	if !strings.Contains(p.Entries[2].Reason, "no longer declared") {
		t.Errorf("an undeclared build in flight must say so: %q", p.Entries[2].Reason)
	}
}

// spec.group is applied now, so an accepted machine outside it is a change —
// and one inside it is not.
func TestAnAcceptedMachineOutsideTheGroupIsAChange(t *testing.T) {
	e := env(memWeb(1024))
	e.Spec.Group = "web-tier"
	w := world()
	w.GroupExists = true
	accepted(&w, 0, builtWith("web-1", 1024))
	asset := *w.Realizations[0].AssetID

	p := Build(e, w)
	if p.Entries[0].Action != ActionChange || !strings.Contains(p.Entries[0].Reason, `not a member of group "web-tier"`) {
		t.Fatalf("entry = %+v, want a change naming the group", p.Entries[0])
	}
	w.GroupMembers = map[uuid.UUID]bool{asset: true}
	if q := Build(e, w); q.Entries[0].Action != ActionNoOp {
		t.Fatalf("a member plans as %s (%s)", q.Entries[0].Action, q.Entries[0].Reason)
	}
	if len(p.Limits) != 0 {
		t.Errorf("the group is applied, yet limits = %v", p.Limits)
	}
}

func TestAPlanWithNoEntriesEncodesAnEmptyList(t *testing.T) {
	if p := Empty("x"); p.Entries == nil {
		t.Fatal("Entries is nil, which encodes as JSON null")
	}
}

// accepted is a realization of web/index built from inputs, accepted.
func accepted(w *Snapshot, index int, inputs map[string]any) {
	a := uuid.New()
	r := realized("web", index, &a)
	r.IntentInputs = inputs
	w.Realizations = append(w.Realizations, r)
	w.Verdicts[a] = AcceptanceAccepted
}

// The manifest's inputs for web/0 as the plan expands them, the way the
// realization would have recorded them — numbers as float64, because they came
// back from a JSONB column.
func builtWith(hostname string, memory float64) map[string]any {
	return map[string]any{"hostname": hostname, "integration": labID.String(), "memory_mb": memory}
}

func memWeb(memory int) envmanifest.EnvironmentResource {
	r := web(1)
	r.Inputs = map[string]any{"hostname": "web-{{instance.number}}", "memory_mb": memory}
	return r
}

// The case the limit used to apologise for: an edited input on an accepted
// machine is a change, and the plan says which input and from what to what.
func TestAnEditedInputOnAnAcceptedMachineIsAChange(t *testing.T) {
	w := world()
	accepted(&w, 0, builtWith("web-1", 1024))

	p := Build(env(memWeb(2048)), w)
	e := p.Entries[0]
	if e.Action != ActionChange {
		t.Fatalf("action = %s (%s), want change", e.Action, e.Reason)
	}
	if len(e.Diff) != 1 || e.Diff[0].Input != "memory_mb" || e.Diff[0].From != 1024.0 || e.Diff[0].To != 2048 {
		t.Fatalf("diff = %+v, want memory_mb 1024 → 2048", e.Diff)
	}
	if len(p.Limits) != 0 {
		t.Errorf("every instance was compared, yet limits = %v", p.Limits)
	}
}

// The same inputs — 1024 recorded as float64, declared as int — is no change.
func TestUnchangedInputsStayNoOp(t *testing.T) {
	w := world()
	accepted(&w, 0, builtWith("web-1", 1024))
	p := Build(env(memWeb(1024)), w)
	if p.Entries[0].Action != ActionNoOp || len(p.Entries[0].Diff) != 0 {
		t.Fatalf("entry = %+v, want a no-op with no diff", p.Entries[0])
	}
	if len(p.Limits) != 0 {
		t.Errorf("limits = %v", p.Limits)
	}
}

// An input added to or removed from the manifest is a difference too.
func TestAnAddedOrRemovedInputIsAChange(t *testing.T) {
	w := world()
	built := builtWith("web-1", 1024)
	built["cores"] = 2.0
	accepted(&w, 0, built)

	p := Build(env(memWeb(1024)), w)
	e := p.Entries[0]
	if e.Action != ActionChange || len(e.Diff) != 1 || e.Diff[0].Input != "cores" || e.Diff[0].To != nil {
		t.Fatalf("entry = %+v, want a change naming the removed cores input", e)
	}
}

// A machine built before inputs were recorded cannot be compared. It stays a
// no-op, and the plan says it did not compare — only then.
func TestAMachineWithNoRecordedInputsIsNoOpAndTheLimitSaysSo(t *testing.T) {
	w := world()
	accepted(&w, 0, nil)
	p := Build(env(memWeb(2048)), w)
	if p.Entries[0].Action != ActionNoOp {
		t.Fatalf("action = %s", p.Entries[0].Action)
	}
	if len(p.Limits) != 1 || p.Limits[0] != InputsNotCompared {
		t.Fatalf("limits = %v, want the not-compared limit", p.Limits)
	}
	if q := Build(env(web(1)), world()); len(q.Limits) != 0 {
		t.Fatalf("a plan with nothing realized compared nothing it could have, yet limits = %v", q.Limits)
	}
}

// Input drift never outranks a stronger reason: a rejected machine is a
// change because acceptance rejected it, and an unevaluated one stays unknown.
func TestInputDriftDoesNotMaskTheAcceptanceVerdict(t *testing.T) {
	w := world()
	a := uuid.New()
	r := realized("web", 0, &a)
	r.IntentInputs = builtWith("web-1", 1024)
	w.Realizations = []Realization{r}

	if e := Build(env(memWeb(2048)), w).Entries[0]; e.Action != ActionUnknown || len(e.Diff) != 0 {
		t.Fatalf("unevaluated: entry = %+v, want unknown with no diff", e)
	}
	w.Verdicts[a] = AcceptanceRejected
	if e := Build(env(memWeb(2048)), w).Entries[0]; e.Action != ActionChange || !strings.Contains(e.Reason, "rejected") {
		t.Fatalf("rejected: entry = %+v", e)
	}
}
