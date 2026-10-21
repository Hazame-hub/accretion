package provider

import (
	"context"
	"errors"
	"fmt"
)

// Writer is a session that can change machines. It is optional: a session
// implements it beside Session, and a caller finds it by type assertion, so a
// read-only provider stays a four-method Session.
//
// Four verbs, the ones a create-and-destroy lifecycle cannot do without.
// Resize, AttachNIC and Adopt are designed (openspec
// extract-accretion-module/proxmox-write-contract.md) and not here yet.
//
// The rules every verb keeps:
//
//   - Identity is the guard. Every verb but Clone takes a Target and refuses,
//     sending no change, when ExpectUUID is empty or unusable or the guest at
//     the ref no longer carries it.
//   - Idempotent. The same call twice leaves the same end state and succeeds
//     the second time saying it had nothing to do (Applied false, or
//     AlreadyAbsent).
//   - Three kinds of failure: *RefusedError (nothing was sent), a plain error
//     (sent, failed, and the outcome is known) and *OutcomeUnknownError
//     (sent, and the answer was lost). Retrying the same call is always safe.
//   - The far side's tasks are the provider's business: a verb returns when
//     the work is done, bounded by ctx.
//   - Secrets arrive resolved in Config; a verb never echoes them.
type Writer interface {
	Clone(ctx context.Context, req CloneRequest) (Machine, error)
	GuestInit(ctx context.Context, m Target, seed Seed) (Placement, error)
	Power(ctx context.Context, m Target, want PowerState) (PowerResult, error)
	Delete(ctx context.Context, m Target, opts DeleteOptions) (Deleted, error)
}

// Target is a machine a verb may touch: where it is, and the identity it must
// still have.
type Target struct {
	Ref Ref
	// ExpectUUID is the SMBIOS UUID the machine was built with. Required.
	ExpectUUID string
	// Name is checked when set.
	Name string
	// OwnerMark is checked when set, where the provider keeps one.
	OwnerMark string
}

// CloneRequest makes a machine from a template, born with its identity, its
// owner mark and its shape.
type CloneRequest struct {
	Template Ref
	Name     string
	// Placement is where the machine goes (a node); "" is the template's.
	Placement string
	// ID is the provider id to use. "" finds the machine an earlier call
	// made (by Name and OwnerMark), or else allocates one.
	ID      string
	Full    bool
	Storage string
	// Pool is the resource pool the machine is created in, "" for none.
	Pool string
	// MintUUID is the SMBIOS UUID the machine is born with. Required.
	MintUUID string
	// OwnerMark is written where a retry can read it back. Required when
	// Capabilities.Write.OwnerMark.
	OwnerMark string
	Shape     Shape
}

// Shape is the size a machine is born with. Zero values keep the template's.
type Shape struct {
	Cores    int
	MemoryMB int
	// CPU is a model name only.
	CPU string
	// DiskGB grows the boot disk to at least this.
	DiskGB int
	// NICs are the cards, in the provider's own notation, first card first.
	NICs []string
}

// Machine is what Clone made, or found already made.
type Machine struct {
	Ref      Ref
	Identity Identity
	// Applied is false when the clone was found already done.
	Applied bool
}

// Seed is a cloud-init NoCloud seed. The provider places the bytes; what
// they say is the caller's.
type Seed struct {
	UserData []byte
	MetaData []byte
	// NetworkConfig nil means DHCP on the first card.
	NetworkConfig []byte
	// Storage is where the seed is kept.
	Storage string
	// InstanceID names the seed. The caller mints it once per machine (the
	// minted SMBIOS UUID), so a retry places the same seed.
	InstanceID string
}

// Placement is where a seed was put, and what Delete removes.
type Placement struct {
	Drive  string
	Volume string
	// Storage and Node say where Volume lives, for its removal.
	Storage string
	Node    string
	// Applied is false when the seed was already in place.
	Applied bool
}

// PowerState is a machine's power end state.
type PowerState string

const (
	PowerRunning PowerState = "running"
	PowerStopped PowerState = "stopped"
)

// PowerResult is the state the machine was seen in afterwards.
type PowerResult struct {
	State PowerState
	// Applied is false when the machine was already in that state.
	Applied bool
}

// DeleteOptions are what Delete may do beyond deleting.
type DeleteOptions struct {
	// StopFirst stops a running machine, after its identity is checked;
	// without it a running machine is refused.
	StopFirst bool
	// RemovePlacements are seeds removed once the machine is gone.
	RemovePlacements []Placement
}

// Deleted is what Delete did.
type Deleted struct {
	AlreadyAbsent bool
	RemovedRef    Ref
}

// RefusedError is a verb whose precondition did not hold. Nothing was sent.
type RefusedError struct {
	Reason string
}

func (e *RefusedError) Error() string { return e.Reason }

// Refused returns a *RefusedError with a formatted reason.
func Refused(format string, args ...any) error {
	return &RefusedError{Reason: fmt.Sprintf(format, args...)}
}

// OutcomeUnknownError is a request that went out and whose answer was lost: a
// task that outlived its wait, a connection cut after sending. The machine may
// or may not have changed. Retrying the same verb is safe; anything else
// needs a look first.
type OutcomeUnknownError struct {
	Op  string
	Err error
}

func (e *OutcomeUnknownError) Error() string {
	return fmt.Sprintf("%s: outcome unknown: %v", e.Op, e.Err)
}

func (e *OutcomeUnknownError) Unwrap() error { return e.Err }

// ErrUnauthorized is what a provider's error matches (errors.Is) when the far
// side refused the credential itself, as opposed to one privilege or one
// object. A command stops on it: nothing it reads after is evidence.
var ErrUnauthorized = errors.New("the provider refused the credential")

// WriteCapabilities is what a session's Writer can do. A session that does
// not implement Writer reports all false. Like Capabilities, it is branched
// on by field, never by asking which provider it is.
type WriteCapabilities struct {
	Clone     bool `json:"clone"`
	GuestInit bool `json:"guest_init"`
	Power     bool `json:"power"`
	Delete    bool `json:"delete"`
	// MintUUID: Clone writes CloneRequest.MintUUID as the guest's identity.
	MintUUID bool `json:"mint_uuid"`
	// OwnerMark: Clone writes the owner mark where a retry reads it back.
	OwnerMark bool `json:"owner_mark"`
	// GracefulShutdown: Power(stopped) asks the guest before forcing it.
	GracefulShutdown bool `json:"graceful_shutdown"`
	// GrowDisk: Clone grows the boot disk to Shape.DiskGB.
	GrowDisk bool `json:"grow_disk"`
}
