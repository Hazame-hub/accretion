package provider

// Grant is what a credential needs on one of the provider's ACL paths to
// build, run and remove the machines a manifest declares, and why.
//
// A provider computes the grants from the recipes of the machines it would
// build (proxmox.Permissions), so a command can print them without knowing
// which privileges a provider has or how its paths are spelled.
type Grant struct {
	Path       string   `json:"path"`
	Privileges []string `json:"privileges"`
	// Why says which step needs them, in a few words.
	Why string `json:"why"`
	// Optional grants make plan show more; nothing fails without them.
	Optional bool `json:"optional,omitempty"`
	// Checked is true when the credential's own privileges on Path were
	// read. Missing is then what it lacks there, empty when nothing.
	Checked bool     `json:"checked"`
	Missing []string `json:"missing,omitempty"`
	// NotChecked says why they were not read, when Checked is false.
	NotChecked string `json:"not_checked,omitempty"`
}

// Permissions is what applying a manifest needs from the credential.
type Permissions struct {
	// Role is a suggested name for one role holding every privilege below.
	Role string `json:"role"`
	// Privileges is the union of the required grants' privileges, sorted:
	// what that role holds.
	Privileges []string `json:"privileges"`
	Grants     []Grant  `json:"grants"`
	// Setup is how to create the role and grant it, in the provider's own
	// words: UI steps and commands, with the credential's id as a
	// placeholder.
	Setup []string `json:"setup"`
	// Notes are what could not be worked out, said rather than guessed.
	Notes []string `json:"notes,omitempty"`
}

// Lacking reports whether some required grant was checked and found short.
func (p Permissions) Lacking() bool {
	for _, g := range p.Grants {
		if g.Checked && len(g.Missing) > 0 && !g.Optional {
			return true
		}
	}
	return false
}

// Unchecked reports whether some required grant could not be checked.
func (p Permissions) Unchecked() bool {
	for _, g := range p.Grants {
		if !g.Checked && !g.Optional {
			return true
		}
	}
	return false
}
