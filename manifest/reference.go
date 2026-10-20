package manifest

import (
	"errors"
	"fmt"
	"strings"
)

// A reference is what a manifest names without resolving it, written
// "<kind>/<name>". An embedder resolves them against its own store; this
// package only says what they are.
type RefKind string

const (
	// RefIntegration names the connector the machines are provisioned
	// through (spec.integration).
	RefIntegration RefKind = "integration"
	// RefGroup names the asset group the machines join (spec.group).
	RefGroup RefKind = "group"
	// RefProvisionBlueprint names a blueprint that builds a resource's
	// machines. It is only meaningful relative to RefIntegration: the same id
	// may exist for more than one provider.
	RefProvisionBlueprint RefKind = "provisionBlueprint"
)

var refKinds = map[RefKind]bool{RefIntegration: true, RefGroup: true, RefProvisionBlueprint: true}

// ErrNotAReference is returned for text that is not "<kind>/<name>".
var ErrNotAReference = errors.New("not a reference")

// Reference is one thing a manifest points at.
type Reference struct {
	Kind RefKind
	Name string
}

func (r Reference) String() string { return string(r.Kind) + "/" + r.Name }

// Valid reports whether the kind is one a manifest uses and the name is set.
func (r Reference) Valid() bool { return refKinds[r.Kind] && r.Name != "" }

// ParseReference reads "<kind>/<name>".
func ParseReference(s string) (Reference, error) {
	kind, name, ok := strings.Cut(strings.TrimSpace(s), "/")
	r := Reference{Kind: RefKind(kind), Name: name}
	if !ok || !r.Valid() {
		return Reference{}, fmt.Errorf("%w: %q", ErrNotAReference, s)
	}
	return r, nil
}
