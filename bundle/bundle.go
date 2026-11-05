// Package bundle writes one kickstart answer file for many machines. The
// machine that boots it picks its own profile from what its board reports
// (SMBIOS UUID, serial number, MAC addresses), so one installer medium serves
// many machines and nobody chooses an entry in a menu.
//
// A machine that matches no profile, or more than one, stops in %pre, before
// Anaconda partitions anything. The file never falls back to a default.
//
// Every machine that boots the medium can read every profile, so a profile
// holds hostnames, users and public keys only: a {{secret.*}} reference is
// refused.
//
// Design: sybil-dev openspec/changes/accrete-bare-metal-install, D1 to D3.
package bundle

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/hazame-hub/accretion/provider"
)

// IncludePath is where %pre writes the selected profile, and what the main
// section of the answer file includes.
const IncludePath = "/tmp/accrete-machine.ks"

// Profile is one machine's answer: the kickstart commands and sections that
// install it, and the values that name the machine it is for.
type Profile struct {
	// Name is the machine's name in the manifest: lower-case letters,
	// digits and hyphens.
	Name string
	// Selectors: the machine is this profile's when any one of them matches.
	UUIDs   []string
	Serials []string
	MACs    []string
	// Kickstart is the rendered profile. It is included as written, after
	// %pre has selected it.
	Kickstart []byte
}

// Bundle is the answer file and the digests that let a machine's record name
// the exact bytes it was installed from.
type Bundle struct {
	// KS is the content of ks.cfg.
	KS []byte
	// SHA256 is the digest of KS, lower-case hex.
	SHA256 string
	// ProfileSHA256 is the digest of each profile's Kickstart, by name.
	ProfileSHA256 map[string]string
}

var (
	namePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	macPattern  = regexp.MustCompile(`^[0-9a-f]{2}(:[0-9a-f]{2}){5}$`)
	// serialPattern keeps a serial safe inside a single-quoted shell word
	// and on one line. Boards print letters, digits and a little punctuation.
	serialPattern = regexp.MustCompile(`^[A-Za-z0-9 ._/+:#-]{1,64}$`)
	secretPattern = regexp.MustCompile(`\{\{\s*secret\.`)
)

// placeholderSerials are what boards ship instead of a serial number. Data,
// not a principle: extend it when a new one is met. Compared case-blind.
var placeholderSerials = map[string]bool{
	"to be filled by o.e.m.": true,
	"default string":         true,
	"system serial number":   true,
	"0123456789":             true,
	"not specified":          true,
	"none":                   true,
	"n/a":                    true,
	"0":                      true,
	"00000000":               true,
	"chassis serial number":  true,
	"not applicable":         true,
}

// placeholderMACs are addresses no card is identified by.
var placeholderMACs = map[string]bool{
	"00:00:00:00:00:00": true,
	"ff:ff:ff:ff:ff:ff": true,
}

// Build checks the profiles and writes the answer file. It refuses, naming
// the profile and the value, a profile with no selector, a selector two
// profiles share, a placeholder value, and a secret reference. Profiles are
// written in name order, so the same input gives the same bytes.
func Build(profiles []Profile) (Bundle, error) {
	if len(profiles) == 0 {
		return Bundle{}, fmt.Errorf("bundle: no profile to write")
	}
	ps := make([]profile, 0, len(profiles))
	owner := map[string]string{} // selector key -> profile name
	names := map[string]bool{}
	for _, p := range profiles {
		c, err := check(p)
		if err != nil {
			return Bundle{}, err
		}
		if names[c.name] {
			return Bundle{}, fmt.Errorf("bundle: two profiles are named %q", c.name)
		}
		names[c.name] = true
		for _, k := range c.keys() {
			if other, ok := owner[k.key]; ok {
				return Bundle{}, fmt.Errorf("bundle: profiles %q and %q share the %s %q; a machine would match both", other, c.name, k.kind, k.shown)
			}
			owner[k.key] = c.name
		}
		ps = append(ps, c)
	}
	sort.Slice(ps, func(i, j int) bool { return ps[i].name < ps[j].name })

	ks := render(ps)
	sum := sha256.Sum256(ks)
	b := Bundle{KS: ks, SHA256: hex.EncodeToString(sum[:]), ProfileSHA256: map[string]string{}}
	for _, p := range ps {
		s := sha256.Sum256(p.body)
		b.ProfileSHA256[p.name] = hex.EncodeToString(s[:])
	}
	return b, nil
}

// profile is a checked Profile in the form the script compares against.
type profile struct {
	name string
	// uuids holds every form a UUID may be read in (provider.UUIDForms).
	uuids   [][]string
	serials []string
	macs    []string
	body    []byte
}

type selectorKey struct{ kind, key, shown string }

func (p profile) keys() []selectorKey {
	var out []selectorKey
	for _, forms := range p.uuids {
		for _, f := range forms {
			out = append(out, selectorKey{"UUID", "uuid " + f, forms[0]})
		}
	}
	for _, s := range p.serials {
		out = append(out, selectorKey{"serial", "serial " + s, s})
	}
	for _, m := range p.macs {
		out = append(out, selectorKey{"MAC", "mac " + m, m})
	}
	return out
}

func check(p Profile) (profile, error) {
	c := profile{name: p.Name, body: p.Kickstart}
	if !namePattern.MatchString(p.Name) {
		return c, fmt.Errorf("bundle: profile name %q: use lower-case letters, digits and hyphens", p.Name)
	}
	for _, u := range p.UUIDs {
		forms := provider.UUIDForms(u)
		if forms == nil {
			return c, fmt.Errorf("bundle: profile %q: UUID %q is malformed or a placeholder boards ship instead of an identity", p.Name, u)
		}
		c.uuids = append(c.uuids, forms)
	}
	for _, s := range p.Serials {
		s = strings.TrimSpace(s)
		if placeholderSerials[strings.ToLower(s)] {
			return c, fmt.Errorf("bundle: profile %q: serial %q is a placeholder boards ship instead of a serial number", p.Name, s)
		}
		if !serialPattern.MatchString(s) {
			return c, fmt.Errorf("bundle: profile %q: serial %q has characters this file does not accept (letters, digits, space and ._/+:#- only)", p.Name, s)
		}
		c.serials = append(c.serials, s)
	}
	for _, m := range p.MACs {
		m = strings.ToLower(strings.TrimSpace(m))
		if !macPattern.MatchString(m) {
			return c, fmt.Errorf("bundle: profile %q: MAC %q is not of the form aa:bb:cc:dd:ee:ff", p.Name, m)
		}
		if placeholderMACs[m] {
			return c, fmt.Errorf("bundle: profile %q: MAC %q names no card", p.Name, m)
		}
		c.macs = append(c.macs, m)
	}
	if len(c.uuids)+len(c.serials)+len(c.macs) == 0 {
		return c, fmt.Errorf("bundle: profile %q has no UUID, serial or MAC; no machine could select it", p.Name)
	}
	if len(bytes.TrimSpace(p.Kickstart)) == 0 {
		return c, fmt.Errorf("bundle: profile %q has an empty kickstart", p.Name)
	}
	if secretPattern.Match(p.Kickstart) {
		return c, fmt.Errorf("bundle: profile %q references a secret; every machine that boots the medium can read every profile, so a bundle holds no secret", p.Name)
	}
	return c, nil
}

// render writes the answer file. The profiles travel base64-encoded inside
// %pre: a profile's own %packages ... %end would otherwise end %pre early,
// and Anaconda copies only ks.cfg off an OEMDRV device, so they cannot be
// separate files on the medium.
//
// ACCRETE_DMI, ACCRETE_NET and ACCRETE_OUT exist for tests; the installer
// never sets them.
func render(ps []profile) []byte {
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("# Written by accrete. One answer file for %d machine(s): %s.\n", len(ps), joinNames(ps))
	w("# The machine selects its profile in %%pre from its SMBIOS UUID, serial\n")
	w("# number or MAC addresses. An unknown or ambiguous machine stops here,\n")
	w("# before any disk is partitioned.\n")
	w("%%pre --erroronfail --log=/tmp/accrete-pre.log\n")
	w("dmi=\"${ACCRETE_DMI:-/sys/class/dmi/id}\"\n")
	w("net=\"${ACCRETE_NET:-/sys/class/net}\"\n")
	w("out=\"${ACCRETE_OUT:-%s}\"\n", IncludePath)
	w("uuid=$(tr 'A-F' 'a-f' < \"$dmi/product_uuid\" 2>/dev/null | tr -d '[:space:]')\n")
	w("serial=$(sed -e 's/^[[:space:]]*//' -e 's/[[:space:]]*$//' \"$dmi/product_serial\" 2>/dev/null)\n")
	w("macs=$(cat \"$net\"/*/address 2>/dev/null | tr 'A-F' 'a-f' | sort -u | tr '\\n' ' ')\n")
	w("match=\"\"\n")
	w("hit() { case \" $match \" in *\" $1 \"*) ;; *) match=\"${match:+$match }$1\" ;; esac; }\n")
	for _, p := range ps {
		w("# %s\n", p.name)
		for _, forms := range p.uuids {
			for _, f := range forms {
				w("[ \"$uuid\" = '%s' ] && hit %s\n", f, p.name)
			}
		}
		for _, s := range p.serials {
			w("[ \"$serial\" = '%s' ] && hit %s\n", s, p.name)
		}
		for _, m := range p.macs {
			w("case \" $macs \" in *' %s '*) hit %s ;; esac\n", m, p.name)
		}
	}
	w("if [ -z \"$match\" ]; then\n")
	w("  echo \"accrete: this machine matches no profile (uuid=$uuid serial=$serial macs=$macs); expected one of: %s. Nothing was installed.\" >&2\n", joinNames(ps))
	w("  exit 1\n")
	w("fi\n")
	w("case \"$match\" in *' '*)\n")
	w("  echo \"accrete: this machine matches more than one profile ($match; uuid=$uuid serial=$serial macs=$macs). Nothing was installed.\" >&2\n")
	w("  exit 1 ;;\n")
	w("esac\n")
	w("echo \"accrete: installing profile $match (uuid=$uuid serial=$serial)\" >&2\n")
	w("case \"$match\" in\n")
	for _, p := range ps {
		w("%s)\n  base64 -d > \"$out\" <<'ACCRETE_PROFILE'\n", p.name)
		enc := base64.StdEncoding.EncodeToString(p.body)
		for len(enc) > 76 {
			w("%s\n", enc[:76])
			enc = enc[76:]
		}
		if enc != "" {
			w("%s\n", enc)
		}
		w("ACCRETE_PROFILE\n  ;;\n")
	}
	w("esac\n")
	w("%%end\n")
	w("%%include %s\n", IncludePath)
	return []byte(b.String())
}

func joinNames(ps []profile) string {
	n := make([]string, len(ps))
	for i, p := range ps {
		n[i] = p.name
	}
	return strings.Join(n, ", ")
}
