// Package provider is the discovery contract every infrastructure provider
// implements, and the normalised model of what discovery returns. It imports
// nothing but the standard library, so that a provider package can be written
// against it without learning the database or the engine that embeds it
// (Sybil Continuum, for one).
//
// This file holds what identity means across a hypervisor and an agent; the
// contract itself (Provider, Session, Resource, Facts) follows in the same
// package.
package provider

import (
	"regexp"
	"strings"
)

var uuidPattern = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

// placeholderUUIDs are values boards and images ship instead of an identity.
// Treated as absent: two machines sharing one of these are not the same
// machine. Data, not a principle — extend it when a new one is met.
var placeholderUUIDs = map[string]bool{
	"00000000-0000-0000-0000-000000000000": true,
	"ffffffff-ffff-ffff-ffff-ffffffffffff": true,
	"03000200-0400-0500-0006-000700080009": true,
}

// UUIDForms returns the encodings under which one SMBIOS UUID may be seen:
// the canonical lower-case form and the SMBIOS-2.6 byte-swapped form (first
// three groups little-endian). A Linux guest on VMware reads the swapped form
// at /sys/class/dmi/id/product_uuid while vCenter reports the canonical one,
// so a matcher compares against both. nil for a blank, malformed or
// placeholder value, so a caller never matches on "no identity".
//
// The same function lives in the Sybil agent's (sybil-quasar) collector; it is repeated here
// rather than imported because quasar's collector is not a dependency
// continuum takes, only its generated protobuf types are.
func UUIDForms(u string) []string {
	u = strings.ToLower(strings.TrimSpace(u))
	if !uuidPattern.MatchString(u) || placeholderUUIDs[u] {
		return nil
	}
	swapped := swapHex(u[0:8]) + "-" + swapHex(u[9:13]) + "-" + swapHex(u[14:18]) + u[18:]
	if swapped == u {
		return []string{u}
	}
	return []string{u, swapped}
}

func swapHex(s string) string {
	b := []byte(s)
	for i, j := 0, len(b)-2; i < j; i, j = i+2, j-2 {
		b[i], b[j] = b[j], b[i]
		b[i+1], b[j+1] = b[j+1], b[i+1]
	}
	return string(b)
}

// NormalizeMACs lower-cases, de-duplicates and sorts hardware addresses, and
// drops the all-zero address some interfaces report. Both the agent path and a
// provider's discovery run their MACs through this so the two sides compare.
func NormalizeMACs(in []string) []string {
	out := make([]string, 0, len(in))
	seen := map[string]bool{}
	for _, m := range in {
		m = strings.ToLower(strings.TrimSpace(m))
		if m == "" || m == "00:00:00:00:00:00" || seen[m] {
			continue
		}
		seen[m] = true
		out = append(out, m)
	}
	sortStrings(out)
	return out
}

// sortStrings is insertion sort: MAC lists are a handful long and this keeps
// the package free of imports beyond the two above.
func sortStrings(s []string) {
	for i := 1; i < len(s); i++ {
		for j := i; j > 0 && s[j] < s[j-1]; j-- {
			s[j], s[j-1] = s[j-1], s[j]
		}
	}
}
