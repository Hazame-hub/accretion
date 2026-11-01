package provider

import (
	"context"
	"time"
)

// The kinds of change a provider's record of a machine can name.
const (
	// ChangeConfig is a change to the machine's settings (CPU, memory, ...).
	ChangeConfig = "config"
	// ChangeDisk is a disk grown or moved.
	ChangeDisk = "disk"
	// ChangePower is the machine started, stopped, rebooted or paused.
	ChangePower = "power"
)

// Change is one change the provider recorded on a machine.
type Change struct {
	At time.Time `json:"at"`
	// By is who made it, as the provider names them (a user or a token).
	By string `json:"by"`
	// Kind is ChangeConfig, ChangeDisk or ChangePower.
	Kind string `json:"kind"`
	// What is the provider's own name for it (on Proxmox, the task type).
	What string `json:"what"`
	// PowerState is what a power change left the machine in ("running",
	// "stopped", "paused"), "" for other kinds or when it cannot be told.
	PowerState string `json:"power_state,omitempty"`
	// Failed is set when the provider recorded the change as having failed.
	Failed bool `json:"failed,omitempty"`
	// Self is set when the session's own credential made it.
	Self bool `json:"self,omitempty"`
}

// History is a Session whose provider keeps a record of who changed what on
// a machine. A provider without one does not implement it, and "who changed
// it" is then unknown.
type History interface {
	// Changes lists what the provider recorded on the machine at ref,
	// newest first, as far back as it keeps them. A record the provider
	// does not keep (a change it applies without logging) is simply not in
	// it: an empty answer is no evidence that nothing changed.
	Changes(ctx context.Context, ref Ref) ([]Change, error)
}
