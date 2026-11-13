package provider

import (
	"context"
	"time"
)

// Installer is a Writer that can build a machine from installer media: an
// empty machine booted from an installer image, with an answer file on a
// small disc of its own, so the install needs no console input. Optional,
// found by type assertion like Writer and Resizer, and it keeps their rules:
// identity is the guard, a repeat is a no-op, and failures are refused,
// failed or unknown. See openspec accrete-installer-iso, design D3.
//
// The installer image is never the machine's own: no verb removes it.
type Installer interface {
	// Media reads an installer image the provider keeps, refusing one it
	// does not list.
	Media(ctx context.Context, placement, volume string) (Media, error)
	// Create makes an empty machine, born with its identity, its owner mark,
	// its shape, an empty boot disk and the installer image in a CD drive,
	// booting from the disk first and the installer when the disk is empty.
	// Like Clone, an empty ID finds the machine an earlier call made.
	Create(ctx context.Context, req CreateRequest) (Machine, error)
	// PlaceAnswer puts the answer disc in the machine's second CD drive.
	PlaceAnswer(ctx context.Context, m Target, a Answer) (Placement, error)
	// RemoveMedia ejects the installer and the answer disc from a stopped
	// machine and removes the answer disc from storage; the installer image
	// stays where it is. Applied is false when nothing was left to remove.
	RemoveMedia(ctx context.Context, m Target, answer Placement) (bool, error)
}

// Media is an installer image as the provider lists it: what accrete can say
// it booted, short of hashing it.
type Media struct {
	Volume string
	Size   int64
	// Created is the image's date on the provider's storage, zero when the
	// provider does not say.
	Created time.Time
}

// CreateRequest makes an empty machine that boots an installer.
type CreateRequest struct {
	Name string
	// Placement is where the machine goes (a node). Required.
	Placement string
	// ID is the provider id to use. "" finds the machine an earlier call
	// made (by Name and OwnerMark), or else allocates one.
	ID string
	// Pool is the resource pool the machine is created in, "" for none.
	Pool string
	// MintUUID is the SMBIOS UUID the machine is born with. Required.
	MintUUID string
	// OwnerMark is written where a retry can read it back. Required.
	OwnerMark string
	Shape     Shape
	// DiskStorage is where the empty boot disk of Shape.DiskGB is made.
	DiskStorage string
	// Installer is the installer image's volume.
	Installer string
}

// Answer is an installer's answer disc: files at its root under a label the
// installer looks for. The provider places the bytes; what they say is the
// caller's.
type Answer struct {
	Label string
	Files map[string][]byte
	// Storage is where the disc is kept.
	Storage string
	// InstanceID names the disc. The caller mints it once per machine (the
	// minted SMBIOS UUID), so a retry places the same disc.
	InstanceID string
}
