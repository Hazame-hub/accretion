package plan

import (
	"reflect"
	"strings"
	"testing"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// openspec/changes/image-approval, design D2: with approved images required,
// a machine still to be built needs its image's record to be approved, and
// its content known unless spec.images is known-or-warn.
func TestAPlanBuildsOnlyFromApprovedImagesWhenAsked(t *testing.T) {
	q := ImageQuery{Name: "rocky-9", Selector: "template", Params: "node=pve"}
	cases := []struct {
		name    string
		answer  ImageAnswer
		policy  string
		err     string
		warning string
	}{
		{"approved", ImageAnswer{Value: float64(103), SHA256: rockyDigest, Approval: ImageApproved}, "", "", ""},
		{"planned", ImageAnswer{Value: float64(103), SHA256: rockyDigest, Approval: "planned"}, "",
			`image "rocky-9" (sha256:6b1b2b6c5f0e…) is planned, not approved`, ""},
		{"rejected", ImageAnswer{Value: float64(103), SHA256: rockyDigest, Approval: "rejected"}, "", "is rejected, not approved", ""},
		{"no record", ImageAnswer{Value: float64(103), SHA256: rockyDigest}, "", "is not registered, not approved", ""},
		{"known, not approved, known-or-warn", ImageAnswer{Value: float64(103), SHA256: rockyDigest, Approval: "planned"},
			envmanifest.ImagesKnownOrWarn, "is planned, not approved", ""},
		{"unknown", ImageAnswer{Value: float64(103)}, "", "provenance UNKNOWN", ""},
		{"unknown, known-or-warn", ImageAnswer{Value: float64(103)}, envmanifest.ImagesKnownOrWarn, "", "provenance UNKNOWN"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			e := imageEnv("rocky-9", nil)
			e.Spec.Images = c.policy
			w := imageWorld(map[ImageQuery]ImageAnswer{q: c.answer})
			w.RequireApprovedImages = true
			p := Build(e, w)
			errs, warns := strings.Join(p.Errors, " | "), strings.Join(p.Warnings, " | ")
			if c.err == "" && !p.OK() {
				t.Fatalf("refused: %s", errs)
			}
			if c.err != "" && !strings.Contains(errs, c.err) {
				t.Fatalf("errors = %s, want %q", errs, c.err)
			}
			if c.warning != "" && !strings.Contains(warns, c.warning) {
				t.Fatalf("warnings = %s, want %q", warns, c.warning)
			}
		})
	}
}

// Off, which is accrete on its own, the same worlds plan exactly as they
// did before approvals existed: the answer's Approval and spec.images are
// not read.
func TestWithoutTheRequirementApprovalIsNotRead(t *testing.T) {
	q := ImageQuery{Name: "rocky-9", Selector: "template", Params: "node=pve"}
	for _, ans := range []ImageAnswer{{Value: float64(103)}, {Value: float64(103), SHA256: rockyDigest, Approval: "rejected"}} {
		e := imageEnv("rocky-9", nil)
		before := Build(e, imageWorld(map[ImageQuery]ImageAnswer{q: {Value: ans.Value, SHA256: ans.SHA256}}))
		e.Spec.Images = envmanifest.ImagesKnownOrWarn
		after := Build(e, imageWorld(map[ImageQuery]ImageAnswer{q: ans}))
		if !after.OK() || !reflect.DeepEqual(before, after) {
			t.Fatalf("policy off changed the plan:\nbefore %+v\nafter  %+v", before, after)
		}
	}
}

// A machine that already exists is not refused when its image is withdrawn:
// refusing its plan would also refuse changing or removing it. Only the
// instance still to be built is.
func TestAWithdrawnImageRefusesOnlyMachinesStillToBuild(t *testing.T) {
	q := ImageQuery{Name: "rocky-9", Selector: "template", Params: "node=pve"}
	w := imageWorld(map[ImageQuery]ImageAnswer{q: {Value: float64(103), SHA256: rockyDigest, Approval: "rejected"}})
	w.RequireApprovedImages = true
	w.Realizations = []Realization{realized("web", 0, nil)}
	e := imageEnv("rocky-9", nil)

	p := Build(e, w)
	if p.OK() || !strings.Contains(strings.Join(p.Errors, " "), "is rejected, not approved") {
		t.Fatalf("errors = %v, want web/1 refused", p.Errors)
	}
	e.Spec.Resources[0].Count = 1
	if p := Build(e, w); !p.OK() {
		t.Fatalf("the machine already built was refused: %v", p.Errors)
	}
}
