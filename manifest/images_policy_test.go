package manifest

import (
	"strings"
	"testing"
)

const imagesPolicyBase = `apiVersion: sybil/v1alpha1
kind: Environment
metadata:
  name: staging
spec:
  integration: lab
POLICY  resources:
    - name: web
      blueprint: provision-vm
      image: rocky-9
`

// spec.images: the default is written as nothing, known-or-warn survives a
// round trip, and anything else is refused.
func TestSpecImages(t *testing.T) {
	for policy, want := range map[string]string{"": "", "  images: approved-only\n": "", "  images: known-or-warn\n": ImagesKnownOrWarn} {
		env, err := Parse([]byte(strings.Replace(imagesPolicyBase, "POLICY", policy, 1)))
		if err != nil {
			t.Fatalf("%q: %v", policy, err)
		}
		if env.Spec.Images != want {
			t.Fatalf("%q: Images = %q, want %q", policy, env.Spec.Images, want)
		}
		doc, err := EncodeEnvironment(env)
		if err != nil {
			t.Fatal(err)
		}
		back, err := DecodeEnvironment(doc)
		if err != nil || back.Spec.Images != want {
			t.Fatalf("%q: round trip = %q, %v", policy, back.Spec.Images, err)
		}
	}
	if _, err := Parse([]byte(strings.Replace(imagesPolicyBase, "POLICY", "  images: anything\n", 1))); err == nil {
		t.Fatal("an unknown spec.images was accepted")
	}
}

// accrete keeps no approvals, so its manifest does not set the policy.
func TestAStandaloneManifestDoesNotSetSpecImages(t *testing.T) {
	src := strings.Replace(standaloneBase, "  group: web\n", "  group: web\n  images: known-or-warn\n", 1)
	if _, err := ParseStandalone([]byte(src)); err == nil || !strings.Contains(err.Error(), "spec.images") {
		t.Fatalf("err = %v", err)
	}
}
