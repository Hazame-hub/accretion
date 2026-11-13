package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
	"github.com/hazame-hub/accretion/plan"
)

// State is what accrete keeps between runs: what has been built, and whether
// it was accepted, in the plan package's JSON shapes, which are those of
// Sybil Continuum's own rows. Importing a manifest and its state into Sybil
// is the intended way up from accrete, so version 1 keeps every field a
// realization row has.
//
// It holds no endpoint and no credential: those are the manifest's, and the
// token is only ever named there.
type State struct {
	Version int `json:"version"`
	// Realizations are the machines built, each with the provider ref it was
	// built at, the minted SMBIOS UUID and the intent it was built for. A
	// realization's integration_id is the provider's ProviderID.
	Realizations []plan.Realization `json:"realizations"`
	// Verdicts is the latest acceptance verdict per asset.
	Verdicts map[uuid.UUID]string `json:"verdicts,omitempty"`
	InFlight []plan.Run           `json:"in_flight,omitempty"`
	// Groups are asset groups by name, with their members.
	Groups map[string][]uuid.UUID `json:"groups,omitempty"`
	// Applying are machines accrete started building and has not recorded
	// yet. accrete's own: InFlight is what an embedder recorded, and
	// accrete never writes it. An older accrete refuses a state that has
	// these, which is wanted: it would plan a half-built machine as create.
	Applying []Applying `json:"applying,omitempty"`
	// Installs is what produced each machine accrete installed from
	// installer media, by realization id. accrete's own, like Applying.
	Installs map[uuid.UUID]InstallRecord `json:"installs,omitempty"`
	// Images is what produced each machine accrete built from a disk
	// image, by realization id. accrete's own, like Applying.
	Images map[uuid.UUID]ImageRecord `json:"images,omitempty"`
}

// InstallRecord is the provenance of an install (openspec
// accrete-installer-iso, D8): the installer as the provider listed it, the
// answer file's hash, and an ISO hash only as declared. accrete cannot hash
// a file on the provider's storage, so nothing here says the ISO was checked.
type InstallRecord struct {
	ISOVolume  string     `json:"iso_volume"`
	ISOSize    int64      `json:"iso_size"`
	ISOCreated *time.Time `json:"iso_created,omitempty"`
	// ISOSHA256Declared is the manifest's iso_sha256, never verified.
	ISOSHA256Declared string `json:"iso_sha256_declared,omitempty"`
	AnswerFormat      string `json:"answer_format"`
	AnswerSHA256      string `json:"answer_sha256,omitempty"`
	// Hostname is what the guest agent reported when the machine was
	// accepted.
	Hostname string `json:"hostname,omitempty"`
}

// Applying is one machine apply is building, rewritten after every step so
// a resumed apply carries on with the same identity and owner mark and never
// clones a second machine. See openspec accretion-apply, D7.
type Applying struct {
	RunID        uuid.UUID              `json:"run_id"`
	Intent       plan.RealizationIntent `json:"intent"`
	IntentInputs map[string]any         `json:"intent_inputs"`
	// Blueprint is the blueprint id the instance declared when the build
	// started; a resume builds with it.
	Blueprint  string `json:"blueprint"`
	MintedUUID string `json:"minted_uuid"`
	OwnerMark  string `json:"owner_mark"`
	Name       string `json:"name"`
	// ResourceRefID is set once the clone answered.
	ResourceRefID string `json:"resource_ref_id,omitempty"`
	// Step is the next step to run: clone, guest-init, power or confirm; or
	// for an install, create, answer, start-installer, installing,
	// remove-media, boot or accept; for a build from a disk image, create,
	// guest-init, start-first-boot, first-boot, boot or confirm.
	Step string `json:"step"`
	// Install is set for a machine installed from installer media.
	Install *InstallRecord `json:"install,omitempty"`
	// Image is set for a machine built from a disk image.
	Image *ImageRecord `json:"image,omitempty"`
}

// The steps of a build, in order.
const (
	stepClone     = "clone"
	stepGuestInit = "guest-init"
	stepPower     = "power"
	stepConfirm   = "confirm"
)

// The steps of an install, in order (openspec accrete-installer-iso, D6).
const (
	stepCreate         = "create"
	stepAnswer         = "answer"
	stepStartInstaller = "start-installer"
	stepInstalling     = "installing"
	stepRemoveMedia    = "remove-media"
	stepBoot           = "boot"
	stepAccept         = "accept"
)

// stateVersion is the only State.Version this accrete reads.
const stateVersion = 1

// loadState reads a state file. A file that does not exist is an empty
// state: nothing is built yet, so every declared machine plans as create.
func loadState(path string) (State, error) {
	raw, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return State{Version: stateVersion}, nil
	}
	if err != nil {
		return State{}, err
	}
	var s State
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&s); err != nil {
		return State{}, fmt.Errorf("%s: %w", path, err)
	}
	if s.Version != stateVersion {
		return State{}, fmt.Errorf("%s: state version %d, this accrete reads version %d", path, s.Version, stateVersion)
	}
	return s, nil
}

// saveState writes the state so that a crash leaves either the old file or
// the new one: a temporary file beside it, synced, renamed over it, and the
// directory synced so the rename itself survives.
func saveState(path string, s State) error {
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(raw); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// ProviderID is the identity a state records its machines against: a UUIDv5
// of the provider's type and endpoint, so a machine recorded against one
// cluster is never matched on another. It is what Sybil calls the
// connector's id.
func ProviderID(p envmanifest.ProviderRef) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("accretion:"+p.Type+":"+strings.TrimRight(p.Endpoint, "/")))
}
