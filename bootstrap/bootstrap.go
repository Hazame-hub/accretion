// Package bootstrap is what a new machine is set up with on its first boot,
// beyond what Sybil itself needs: users and their SSH keys, packages, a
// timezone, files, commands.
//
// A manifest declares it as a structured profile, never as raw cloud-config:
// the profile is validated field by field, so a typo is a plan error rather
// than a machine that booted without its admin user, and it is rendered by
// marshalling, so no value can break out of the document it lands in. The
// enrollment Sybil needs (its agent, the guest agent) is always written by
// Sybil and comes first; a profile cannot remove or reorder it.
package bootstrap

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

// Profile is a validated bootstrap profile.
type Profile struct {
	Users    []User   `json:"users,omitempty" yaml:"users,omitempty"`
	Packages []string `json:"packages,omitempty" yaml:"packages,omitempty"`
	Timezone string   `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	Files    []File   `json:"files,omitempty" yaml:"files,omitempty"`
	// Run are shell commands run once, after Sybil's agent is installed: a
	// command that fails does not stop the machine from enrolling, so the
	// failure is visible in Sybil rather than a machine that never shows up.
	Run []string `json:"run,omitempty" yaml:"run,omitempty"`
}

// User is an account created on the machine.
type User struct {
	Name    string   `json:"name" yaml:"name"`
	SSHKeys []string `json:"ssh_keys,omitempty" yaml:"ssh_keys,omitempty"`
	// Sudo grants passwordless sudo. There is no password field on purpose:
	// a password in a manifest is a password in a Git history.
	Sudo  bool   `json:"sudo,omitempty" yaml:"sudo,omitempty"`
	Shell string `json:"shell,omitempty" yaml:"shell,omitempty"`
}

// File is a file written before the commands run.
type File struct {
	Path        string `json:"path" yaml:"path"`
	Content     string `json:"content" yaml:"content"`
	Permissions string `json:"permissions,omitempty" yaml:"permissions,omitempty"`
	Owner       string `json:"owner,omitempty" yaml:"owner,omitempty"`
}

var (
	userName    = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	sshKey      = regexp.MustCompile(`^(ssh-(ed25519|rsa|dss)|ecdsa-sha2-nistp(256|384|521)|sk-(ssh-ed25519|ecdsa-sha2-nistp256)@openssh\.com) [A-Za-z0-9+/=]+( [^\r\n]*)?$`)
	packageName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9.+_:-]{0,127}$`)
	timezone    = regexp.MustCompile(`^[A-Za-z_]+(/[A-Za-z0-9_+-]+){0,2}$`)
	shellPath   = regexp.MustCompile(`^/[A-Za-z0-9/._-]+$`)
	octalMode   = regexp.MustCompile(`^0?[0-7]{3}$`)
	ownerSpec   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}(:[a-z_][a-z0-9_-]{0,31})?$`)
)

// Reserved users are ones a profile must not redefine: the image's own
// admin and root.
var reserved = map[string]bool{"root": true}

// Parse reads a profile from a decoded input value (a map, as it comes out of
// a manifest or a run's input) and validates it. Nil is no profile.
func Parse(v any) (*Profile, error) {
	if v == nil {
		return nil, nil
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	if string(raw) == "{}" || string(raw) == `""` {
		return nil, nil
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	var p Profile
	if err := dec.Decode(&p); err != nil {
		return nil, fmt.Errorf("bootstrap: %w", err)
	}
	if err := p.Validate(); err != nil {
		return nil, err
	}
	return &p, nil
}

// Validate reports the first thing wrong with the profile.
func (p Profile) Validate() error {
	seen := map[string]bool{}
	for i, u := range p.Users {
		switch {
		case !userName.MatchString(u.Name):
			return fmt.Errorf("bootstrap: users[%d]: %q is not a user name", i, u.Name)
		case reserved[u.Name]:
			return fmt.Errorf("bootstrap: users[%d]: %s cannot be redefined", i, u.Name)
		case seen[u.Name]:
			return fmt.Errorf("bootstrap: users[%d]: %s is declared twice", i, u.Name)
		case u.Shell != "" && !shellPath.MatchString(u.Shell):
			return fmt.Errorf("bootstrap: users[%d]: %q is not a shell path", i, u.Shell)
		}
		seen[u.Name] = true
		for j, k := range u.SSHKeys {
			if !sshKey.MatchString(strings.TrimSpace(k)) {
				return fmt.Errorf("bootstrap: users[%d].ssh_keys[%d] is not a public SSH key", i, j)
			}
		}
	}
	for i, pkg := range p.Packages {
		if !packageName.MatchString(pkg) {
			return fmt.Errorf("bootstrap: packages[%d]: %q is not a package name", i, pkg)
		}
	}
	if p.Timezone != "" && !timezone.MatchString(p.Timezone) {
		return fmt.Errorf("bootstrap: %q is not a timezone", p.Timezone)
	}
	paths := map[string]bool{}
	for i, f := range p.Files {
		clean := path.Clean(f.Path)
		switch {
		case !strings.HasPrefix(f.Path, "/") || clean != f.Path:
			return fmt.Errorf("bootstrap: files[%d]: %q is not a clean absolute path", i, f.Path)
		case paths[clean]:
			return fmt.Errorf("bootstrap: files[%d]: %s is written twice", i, f.Path)
		case f.Permissions != "" && !octalMode.MatchString(f.Permissions):
			return fmt.Errorf("bootstrap: files[%d]: %q is not an octal mode", i, f.Permissions)
		case f.Owner != "" && !ownerSpec.MatchString(f.Owner):
			return fmt.Errorf("bootstrap: files[%d]: %q is not user[:group]", i, f.Owner)
		}
		paths[clean] = true
	}
	for i, c := range p.Run {
		if strings.TrimSpace(c) == "" {
			return fmt.Errorf("bootstrap: run[%d] is empty", i)
		}
	}
	return nil
}

// Strings visits every free-text value a profile carries — file contents,
// commands — so a caller can substitute references such as {{secret.name}}
// in them, and only in them.
func (p *Profile) Strings(fn func(string) (string, error)) error {
	for i := range p.Files {
		v, err := fn(p.Files[i].Content)
		if err != nil {
			return fmt.Errorf("bootstrap: files[%d]: %w", i, err)
		}
		p.Files[i].Content = v
	}
	for i := range p.Run {
		v, err := fn(p.Run[i])
		if err != nil {
			return fmt.Errorf("bootstrap: run[%d]: %w", i, err)
		}
		p.Run[i] = v
	}
	return nil
}

// Base is what Sybil itself needs on the machine, written whatever the
// profile says.
type Base struct {
	Hostname string
	Packages []string
	// Commands run first, in order: Sybil's enrollment.
	Commands [][]string
}

// Render writes the cloud-config for base plus the profile (nil: none).
func Render(base Base, p *Profile) (string, error) {
	if base.Hostname == "" {
		return "", errors.New("bootstrap: no hostname")
	}
	doc := map[string]any{
		"hostname":         base.Hostname,
		"manage_etc_hosts": true,
	}
	packages := append([]string{}, base.Packages...)
	runcmd := []any{}
	for _, c := range base.Commands {
		runcmd = append(runcmd, c)
	}
	if p != nil {
		have := map[string]bool{}
		for _, pkg := range packages {
			have[pkg] = true
		}
		for _, pkg := range p.Packages {
			if !have[pkg] {
				packages = append(packages, pkg)
				have[pkg] = true
			}
		}
		if len(p.Users) > 0 {
			// "default" keeps the image's own user; the profile's come after.
			users := []any{"default"}
			for _, u := range p.Users {
				cu := map[string]any{"name": u.Name, "lock_passwd": true}
				if len(u.SSHKeys) > 0 {
					keys := make([]string, 0, len(u.SSHKeys))
					for _, k := range u.SSHKeys {
						keys = append(keys, strings.TrimSpace(k))
					}
					cu["ssh_authorized_keys"] = keys
				}
				if u.Sudo {
					cu["sudo"] = "ALL=(ALL) NOPASSWD:ALL"
				}
				if u.Shell != "" {
					cu["shell"] = u.Shell
				}
				users = append(users, cu)
			}
			doc["users"] = users
		}
		if p.Timezone != "" {
			doc["timezone"] = p.Timezone
		}
		if len(p.Files) > 0 {
			files := []any{}
			for _, f := range p.Files {
				wf := map[string]any{"path": f.Path, "content": f.Content}
				if f.Permissions != "" {
					wf["permissions"] = "0" + strings.TrimPrefix(f.Permissions, "0")
				}
				if f.Owner != "" {
					wf["owner"] = f.Owner
				}
				files = append(files, wf)
			}
			doc["write_files"] = files
		}
		for _, c := range p.Run {
			runcmd = append(runcmd, []string{"sh", "-c", c})
		}
	}
	if len(packages) > 0 {
		doc["packages"] = packages
	}
	if len(runcmd) > 0 {
		doc["runcmd"] = runcmd
	}
	var b bytes.Buffer
	b.WriteString("#cloud-config\n")
	enc := yaml.NewEncoder(&b)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return "", fmt.Errorf("bootstrap: render: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("bootstrap: render: %w", err)
	}
	return b.String(), nil
}
