package main

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hazame-hub/accretion/bootstrap"
	"github.com/hazame-hub/accretion/provider"
)

// Secrets in a bootstrap profile.
//
// A profile names a secret as {{secret.name}}; accrete has no secret store,
// so apply reads each one from the environment of whoever runs it, as
// ACCRETE_SECRET_<NAME> (the name upper-cased, every character that is not a
// letter or digit an underscore). A value is never written to the state, the
// plan or the output: the state keeps the reference.
//
// A seed or an answer disc is a file on a Proxmox storage, readable by anyone
// who may read that storage. So a disc that carries a secret is there only
// for the first boot or the install: an answer disc is removed once the
// installer powers the machine off, as always; a seed with a secret ends the
// first boot with a power-off, and is then replaced, before the machine
// starts again, by the same seed with every secret taken out. cloud-init
// keeps the instance id, so nothing runs a second time.

// secretRef is one reference to a secret.
var secretRef = regexp.MustCompile(`\{\{\s*secret\.([A-Za-z0-9_.-]+)\s*\}\}`)

// redacted is what a secret becomes on a seed left on storage.
const redacted = "(removed by accrete after the first boot)"

// secretEnv is the environment variable a secret is read from.
func secretEnv(name string) string {
	var b strings.Builder
	for _, r := range strings.ToUpper(name) {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return "ACCRETE_SECRET_" + b.String()
}

// filler rewrites one string of a profile.
type filler func(string) (string, error)

// withSecrets puts each secret's value in, or fails naming the variables
// that are not set.
func withSecrets(getenv func(string) string) filler {
	return func(s string) (string, error) {
		var missing []string
		out := secretRef.ReplaceAllStringFunc(s, func(ref string) string {
			name := secretRef.FindStringSubmatch(ref)[1]
			v := ""
			if getenv != nil {
				v = getenv(secretEnv(name))
			}
			if v == "" {
				missing = append(missing, secretEnv(name))
			}
			return v
		})
		if len(missing) > 0 {
			sort.Strings(missing)
			return "", fmt.Errorf("references a secret that is not set: set %s in the environment accrete runs in", strings.Join(missing, ", "))
		}
		return out, nil
	}
}

// withoutSecrets takes every secret out.
func withoutSecrets(s string) (string, error) {
	return secretRef.ReplaceAllString(s, redacted), nil
}

// profileOf parses a recipe's bootstrap profile and rewrites its strings
// with fill. Nil when the recipe has none.
func profileOf(rec provider.Recipe, fill filler) (*bootstrap.Profile, error) {
	if rec.Bootstrap == nil {
		return nil, nil
	}
	p, err := bootstrap.Parse(rec.Bootstrap)
	if err != nil {
		return nil, err
	}
	if err := p.Strings(fill); err != nil {
		return nil, err
	}
	return p, nil
}

// hasSecrets says whether a recipe's bootstrap references a secret.
func hasSecrets(rec provider.Recipe) bool {
	found := false
	_, _ = profileOf(rec, func(s string) (string, error) {
		found = found || secretRef.MatchString(s)
		return s, nil
	})
	return found
}

// fillFor is how a seed or answer file is written: with its secrets in when
// secret is true, else with them taken out. A recipe with no secret is the
// same either way.
func (a *applier) fillFor(secret bool) filler {
	if secret {
		return withSecrets(a.getenv)
	}
	return withoutSecrets
}
