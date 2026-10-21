package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"strings"

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
}

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

// ProviderID is the identity a state records its machines against: a UUIDv5
// of the provider's type and endpoint, so a machine recorded against one
// cluster is never matched on another. It is what Sybil calls the
// connector's id.
func ProviderID(p envmanifest.ProviderRef) uuid.UUID {
	return uuid.NewSHA1(uuid.NameSpaceURL, []byte("accretion:"+p.Type+":"+strings.TrimRight(p.Endpoint, "/")))
}
