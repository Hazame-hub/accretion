// Package manifest is the Environment manifest codec: decode, validate,
// normalize and encode a statement of which machines should exist, and say
// what it points at. Planning against what was built is package plan's.
//
// The envelope is the same k8s-shaped document Sybil Continuum uses for its
// configuration bundles (apiVersion, kind, metadata.name, spec), so a
// manifest can travel in one of those, but nothing here knows about that
// bundle format's other kinds or its registry.
//
// An Environment comes in two shapes, which differ only in what builds the
// machines. Sybil's names one of its connectors and is decoded by
// DecodeEnvironment:
//
//	apiVersion: sybil/v1alpha1
//	kind: Environment
//	metadata:
//	  name: two-vms
//	spec:
//	  integration: lab
//	  resources:
//	    - name: web
//	      count: 2
//	      blueprint: provision-vm
//
// Accretion's own, planned with no Sybil behind it, names the provider and
// where its token is kept, never the token, and is decoded by
// DecodeStandalone:
//
//	apiVersion: accretion/v1alpha1
//	kind: Environment
//	metadata:
//	  name: two-vms
//	spec:
//	  provider:
//	    type: <provider>
//	    endpoint: https://pve.example:8006
//	    verify_tls: true
//	    token_env: ACCRETE_TOKEN    # or token_file: ./provider.token
//	  resources:
//	    - name: web
//	      count: 2
//	      blueprint: provision-vm
//
// Each decoder refuses the other's apiVersion. EnvironmentJSONSchema and
// StandaloneJSONSchema describe the two shapes.
package manifest

import (
	"errors"
	"fmt"
	"io"
	"strings"

	"gopkg.in/yaml.v3"
)

// Document is one manifest document. The envelope is deliberately
// k8s-shaped for familiarity, but it is not a reconciled resource model:
// there is no status, no generation, and no controller.
type Document struct {
	// APIVersion is required by the Environment kind; see DecodeEnvironment.
	APIVersion string   `yaml:"apiVersion,omitempty"`
	Kind       string   `yaml:"kind"`
	Metadata   Metadata `yaml:"metadata"`
	// Spec is held as a yaml.Node rather than a map so field order survives
	// a round trip.
	Spec yaml.Node `yaml:"spec"`
}

// Metadata carries the document's identity. Name is a stable natural key.
type Metadata struct {
	Name string `yaml:"name"`
}

var (
	// ErrMissingKind / ErrMissingName mark a structurally invalid envelope.
	ErrMissingKind = errors.New("document has no kind")
	ErrMissingName = errors.New("document has no metadata.name")
)

// DocumentError attaches a document's position and identity to a failure, so
// a stream of several documents reports which one is wrong.
type DocumentError struct {
	// Index is the 0-based position of the document within the stream.
	Index int
	Kind  string
	Name  string
	Err   error
}

func (e *DocumentError) Error() string {
	switch {
	case e.Kind != "" && e.Name != "":
		return fmt.Sprintf("document %d (%s/%s): %v", e.Index, e.Kind, e.Name, e.Err)
	case e.Kind != "":
		return fmt.Sprintf("document %d (%s): %v", e.Index, e.Kind, e.Err)
	default:
		return fmt.Sprintf("document %d: %v", e.Index, e.Err)
	}
}

func (e *DocumentError) Unwrap() error { return e.Err }

// Validate checks the envelope itself, not the spec.
func (d Document) Validate() error {
	if strings.TrimSpace(d.Kind) == "" {
		return ErrMissingKind
	}
	if strings.TrimSpace(d.Metadata.Name) == "" {
		return ErrMissingName
	}
	return nil
}

// DecodeDocuments reads a multi-document YAML stream, in order. Unknown
// envelope keys are an error, so `apiVersoin:` is reported where it is and
// not as a missing version. Empty documents (a stray `---`) are skipped.
func DecodeDocuments(r io.Reader) ([]Document, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var docs []Document
	for i := 0; ; i++ {
		var doc Document
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs, nil
		}
		if err != nil {
			return nil, &DocumentError{Index: i, Err: err}
		}
		if doc.Kind == "" && doc.Metadata.Name == "" && doc.Spec.IsZero() {
			continue
		}
		if err := doc.Validate(); err != nil {
			return nil, &DocumentError{Index: i, Kind: doc.Kind, Name: doc.Metadata.Name, Err: err}
		}
		docs = append(docs, doc)
	}
}

// EncodeDocuments writes documents as a multi-document YAML stream with a
// two-space indent.
func EncodeDocuments(w io.Writer, docs []Document) error {
	enc := yaml.NewEncoder(w)
	enc.SetIndent(2)
	for i, doc := range docs {
		if err := enc.Encode(doc); err != nil {
			_ = enc.Close()
			return &DocumentError{Index: i, Kind: doc.Kind, Name: doc.Metadata.Name, Err: err}
		}
	}
	return enc.Close()
}

// DecodeSpec unmarshals a document's spec into a typed struct, refusing
// unknown fields: what you read in a reviewed manifest is what you get.
func DecodeSpec(doc Document, out any) error {
	raw, err := yaml.Marshal(&doc.Spec)
	if err != nil {
		return fmt.Errorf("re-encode spec: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	if err := dec.Decode(out); err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	return nil
}

// EncodeSpec renders a typed spec into the node the envelope carries.
func EncodeSpec(in any) (yaml.Node, error) {
	var out yaml.Node
	if err := out.Encode(in); err != nil {
		return yaml.Node{}, err
	}
	return out, nil
}

// Parse decodes exactly one Environment document from src.
func Parse(src []byte) (Environment, error) {
	docs, err := DecodeDocuments(strings.NewReader(string(src)))
	if err != nil {
		return Environment{}, err
	}
	if len(docs) != 1 {
		return Environment{}, fmt.Errorf("want one %s document, found %d", KindEnvironment, len(docs))
	}
	return DecodeEnvironment(docs[0])
}

// Marshal encodes env as one YAML document.
func Marshal(env Environment) ([]byte, error) {
	doc, err := EncodeEnvironment(env)
	if err != nil {
		return nil, err
	}
	var b strings.Builder
	if err := EncodeDocuments(&b, []Document{doc}); err != nil {
		return nil, err
	}
	return []byte(b.String()), nil
}
