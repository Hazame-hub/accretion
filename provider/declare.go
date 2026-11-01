package provider

import "context"

// Declared is an existing machine described as the inputs of one of the
// provider's blueprints, so that a manifest can declare it and its record can
// adopt it without building anything.
type Declared struct {
	Ref   Ref    `json:"ref"`
	Name  string `json:"name"`
	State string `json:"state"`
	// SMBIOSUUID is the identity the machine carries now, which its record
	// is tied to from then on.
	SMBIOSUUID string `json:"smbios_uuid,omitempty"`
	// Blueprint is the blueprint whose inputs Inputs are.
	Blueprint string `json:"blueprint,omitempty"`
	// Inputs are read off the machine, in the blueprint's units.
	Inputs map[string]any `json:"inputs,omitempty"`
	// Unread are inputs the blueprint requires that the machine does not
	// show (on Proxmox, the template a full clone came from), with why. The
	// operator supplies them; they are never guessed.
	Unread map[string]string `json:"unread,omitempty"`
	// Notes are what an operator should know about the declaration.
	Notes []string `json:"notes,omitempty"`
	// Skip, when set, says why the machine cannot be declared.
	Skip string `json:"skip,omitempty"`
}

// Declarer is a Session that can describe its existing machines as
// blueprint inputs (accrete init --from-cluster). It sends reads only.
type Declarer interface {
	Declare(ctx context.Context) ([]Declared, error)
}
