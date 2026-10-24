package manifest

import (
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"

	"gopkg.in/yaml.v3"
)

// EnvironmentStandaloneV1Alpha1 is the apiVersion of a manifest that
// Accretion plans on its own, with no Sybil behind it. It is the
// sybil/v1alpha1 Environment with spec.provider in place of
// spec.integration: the provider's type, where it answers, and where the
// token is kept. Everything else in spec is decoded by the same code.
//
// The token itself is never in the manifest: only the name of a file or of
// an environment variable that holds it.
const EnvironmentStandaloneV1Alpha1 = "accretion/v1alpha1"

// ProviderRef is spec.provider: what a standalone manifest is built through.
type ProviderRef struct {
	// Type is the provider's registered name.
	Type string
	// Endpoint is the provider's API, an https URL.
	Endpoint string
	// VerifyTLS checks the endpoint's certificate. True unless the manifest
	// says verify_tls: false.
	VerifyTLS bool
	// TokenFile is the file that holds the token, relative to the manifest's
	// directory unless absolute. Exactly one of TokenFile and TokenEnv is set.
	TokenFile string
	// TokenEnv is the environment variable that holds the token.
	TokenEnv string
}

type providerWire struct {
	Type      string `yaml:"type"`
	Endpoint  string `yaml:"endpoint"`
	VerifyTLS *bool  `yaml:"verify_tls,omitempty"`
	TokenFile string `yaml:"token_file,omitempty"`
	TokenEnv  string `yaml:"token_env,omitempty"`
}

var (
	providerTypePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
	envVarPattern       = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// standalonePlaceholder stands in for spec.integration while the rest of a
// standalone spec goes through DecodeEnvironment, and is removed again
// before anything sees it.
const standalonePlaceholder = "accretion-standalone"

// Validate checks the provider block on its own. Its messages never repeat
// the endpoint, which could carry credentials someone pasted into it.
func (p ProviderRef) Validate() error {
	if !providerTypePattern.MatchString(p.Type) {
		return fmt.Errorf("type is required: the provider's name, lowercase letters, digits and -")
	}
	u, err := url.Parse(p.Endpoint)
	switch {
	case strings.TrimSpace(p.Endpoint) == "":
		return fmt.Errorf("endpoint is required")
	case err != nil || u.Scheme != "https" || u.Host == "":
		return fmt.Errorf("endpoint must be an https URL, such as https://host:port")
	case u.User != nil:
		return fmt.Errorf("endpoint must not carry a user or password; the token comes from token_file or token_env")
	}
	switch {
	case p.TokenFile != "" && p.TokenEnv != "":
		return fmt.Errorf("set exactly one of token_file and token_env, not both")
	case p.TokenFile == "" && p.TokenEnv == "":
		return fmt.Errorf("exactly one of token_file and token_env is required")
	case p.TokenEnv != "" && !envVarPattern.MatchString(p.TokenEnv):
		return fmt.Errorf("token_env %q is not an environment variable name", p.TokenEnv)
	}
	return nil
}

func decodeProvider(node *yaml.Node) (ProviderRef, error) {
	if node.Kind != yaml.MappingNode {
		return ProviderRef{}, fmt.Errorf("want a mapping with type, endpoint and token_file or token_env")
	}
	for i := 0; i+1 < len(node.Content); i += 2 {
		// Refused by name, and before decoding, so the message can say what
		// to do instead and the value never reaches an error string.
		if node.Content[i].Value == "token" {
			return ProviderRef{}, fmt.Errorf("an inline token is not accepted; name the file that holds it with token_file, " +
				"or the environment variable with token_env")
		}
	}
	raw, err := yaml.Marshal(node)
	if err != nil {
		return ProviderRef{}, fmt.Errorf("re-encode: %w", err)
	}
	dec := yaml.NewDecoder(strings.NewReader(string(raw)))
	dec.KnownFields(true)
	var w providerWire
	if err := dec.Decode(&w); err != nil && !errors.Is(err, io.EOF) {
		return ProviderRef{}, err
	}
	ref := ProviderRef{
		Type:      strings.TrimSpace(w.Type),
		Endpoint:  strings.TrimSpace(w.Endpoint),
		VerifyTLS: w.VerifyTLS == nil || *w.VerifyTLS,
		TokenFile: strings.TrimSpace(w.TokenFile),
		TokenEnv:  strings.TrimSpace(w.TokenEnv),
	}
	return ref, ref.Validate()
}

func encodeProvider(p ProviderRef) providerWire {
	verify := p.VerifyTLS
	return providerWire{Type: p.Type, Endpoint: p.Endpoint, VerifyTLS: &verify, TokenFile: p.TokenFile, TokenEnv: p.TokenEnv}
}

// DecodeStandalone parses and validates one accretion/v1alpha1 Environment
// document. Its spec.provider is decoded here; the rest of spec goes through
// DecodeEnvironment, so the two shapes cannot drift apart. The Environment it
// returns has Spec.Provider set and Spec.Integration empty.
func DecodeStandalone(doc Document) (Environment, error) {
	if doc.Kind != KindEnvironment {
		return Environment{}, fmt.Errorf("%q: expected kind %s, got %q", doc.Metadata.Name, KindEnvironment, doc.Kind)
	}
	switch strings.TrimSpace(doc.APIVersion) {
	case EnvironmentStandaloneV1Alpha1:
	case "":
		return Environment{}, fmt.Errorf("%s %q: apiVersion is required and must be %s",
			KindEnvironment, doc.Metadata.Name, EnvironmentStandaloneV1Alpha1)
	default:
		return Environment{}, fmt.Errorf("%s %q: unsupported apiVersion %q, want %s",
			KindEnvironment, doc.Metadata.Name, doc.APIVersion, EnvironmentStandaloneV1Alpha1)
	}
	name := strings.TrimSpace(doc.Metadata.Name)
	if name == "" {
		return Environment{}, fmt.Errorf("%s: %w", KindEnvironment, ErrMissingName)
	}

	spec := doc.Spec
	if spec.Kind == yaml.DocumentNode && len(spec.Content) == 1 {
		spec = *spec.Content[0]
	}
	if spec.Kind != yaml.MappingNode {
		return Environment{}, fmt.Errorf("%s %q: spec.provider is required", KindEnvironment, name)
	}
	rest := yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
	var provider *yaml.Node
	for i := 0; i+1 < len(spec.Content); i += 2 {
		switch spec.Content[i].Value {
		case "provider":
			provider = spec.Content[i+1]
		case "integration":
			return Environment{}, fmt.Errorf("%s %q: spec.integration names a Sybil connector; an %s manifest names spec.provider instead",
				KindEnvironment, name, EnvironmentStandaloneV1Alpha1)
		case "images":
			return Environment{}, fmt.Errorf("%s %q: spec.images is Sybil's image approval policy; accrete keeps no approvals, so an %s manifest does not set it",
				KindEnvironment, name, EnvironmentStandaloneV1Alpha1)
		default:
			rest.Content = append(rest.Content, spec.Content[i], spec.Content[i+1])
		}
	}
	if provider == nil {
		return Environment{}, fmt.Errorf("%s %q: spec.provider is required", KindEnvironment, name)
	}
	ref, err := decodeProvider(provider)
	if err != nil {
		return Environment{}, fmt.Errorf("%s %q: spec.provider: %w", KindEnvironment, name, err)
	}

	rest.Content = append([]*yaml.Node{
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: "integration"},
		{Kind: yaml.ScalarNode, Tag: "!!str", Value: standalonePlaceholder},
	}, rest.Content...)
	env, err := DecodeEnvironment(Document{APIVersion: EnvironmentV1Alpha1, Kind: doc.Kind, Metadata: Metadata{Name: name}, Spec: rest})
	if err != nil {
		return Environment{}, err
	}
	env.Spec.Integration = ""
	env.Spec.Provider = &ref
	if err := env.Validate(); err != nil {
		return Environment{}, err
	}
	return env, nil
}

// ParseStandalone decodes exactly one accretion/v1alpha1 Environment
// document from src.
func ParseStandalone(src []byte) (Environment, error) {
	docs, err := DecodeDocuments(strings.NewReader(string(src)))
	if err != nil {
		return Environment{}, err
	}
	if len(docs) != 1 {
		return Environment{}, fmt.Errorf("want one %s document, found %d", KindEnvironment, len(docs))
	}
	return DecodeStandalone(docs[0])
}

// TokenPath is where TokenFile points, resolved from dir (the manifest's
// directory) when relative. Empty when the token comes from the environment.
func (p ProviderRef) TokenPath(dir string) string {
	if p.TokenFile == "" || filepath.IsAbs(p.TokenFile) {
		return p.TokenFile
	}
	return filepath.Join(dir, p.TokenFile)
}

// Token reads the token p names: TokenFile, resolved from dir, or TokenEnv
// through getenv. Surrounding whitespace is dropped.
//
// A token file that its group or others can read or write gets a warning on
// warn, naming the file and its mode, as ssh does for a key; the token is
// still returned. Windows has no such mode bits, so it is not checked there.
// Neither the warning nor any error contains the token.
func (p ProviderRef) Token(dir string, getenv func(string) string, warn io.Writer) (string, error) {
	if p.TokenEnv != "" {
		tok := strings.TrimSpace(getenv(p.TokenEnv))
		if tok == "" {
			return "", fmt.Errorf("spec.provider.token_env: environment variable %s is not set or empty", p.TokenEnv)
		}
		return tok, nil
	}
	path := p.TokenPath(dir)
	if path == "" {
		return "", fmt.Errorf("spec.provider: exactly one of token_file and token_env is required")
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", fmt.Errorf("spec.provider.token_file: %w", err)
	}
	if perm := info.Mode().Perm(); runtime.GOOS != "windows" && perm&0o077 != 0 && warn != nil {
		fmt.Fprintf(warn, "warning: token_file %s has mode %04o, so others than its owner can read or change it; chmod 600 %s\n",
			path, perm, path)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("spec.provider.token_file: %w", err)
	}
	tok := strings.TrimSpace(string(raw))
	if tok == "" {
		return "", fmt.Errorf("spec.provider.token_file: %s is empty", path)
	}
	return tok, nil
}

// encodeStandalone turns the spec EncodeEnvironment built into the
// accretion/v1alpha1 shape: spec.provider where spec.integration was.
func encodeStandalone(env Environment, spec yaml.Node) (Document, error) {
	provider, err := EncodeSpec(encodeProvider(*env.Spec.Provider))
	if err != nil {
		return Document{}, fmt.Errorf("%s %q: spec.provider: %w", KindEnvironment, env.Name, err)
	}
	for i := 0; i+1 < len(spec.Content); i += 2 {
		if spec.Content[i].Value == "integration" {
			spec.Content[i] = &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: "provider"}
			spec.Content[i+1] = &provider
			break
		}
	}
	return Document{
		APIVersion: EnvironmentStandaloneV1Alpha1,
		Kind:       KindEnvironment,
		Metadata:   Metadata{Name: env.Name},
		Spec:       spec,
	}, nil
}
