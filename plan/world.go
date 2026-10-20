package plan

import (
	"time"

	"github.com/google/uuid"
)

// World is what the planner plans against: everything an embedder knows
// about the connector, its blueprints and what has been built, as of one
// moment.
//
// Build calls Read once, before it decides anything, and plans against the
// Snapshot it returns. One read rather than a method per fact, for two
// reasons. Every rule here tells "not read" (nil) from "read and empty", and a
// Snapshot carries that distinction field by field; and a plan that read some
// facts before a change and some after could disagree with itself. An
// embedder that fetches lazily does its fetching inside Read.
//
// Snapshot is itself a World, for tests and for embedders that fetch up
// front. Sybil Continuum's World reads its database and translates its rows
// into the shapes below.
type World interface {
	Read() Snapshot
}

// Read returns s: a Snapshot is a World that is already read.
func (s Snapshot) Read() Snapshot { return s }

// The planner's view of what an embedder stores. Each type is the subset of
// the embedder's own record the rules read, with field names an embedder
// built on Sybil's model will recognise.

// RealizationIntent is the declared instance a realization or a run is for:
// the manifest kind and name, the resource and the instance index.
type RealizationIntent struct {
	Kind          string `json:"kind"`
	Name          string `json:"name"`
	ResourceName  string `json:"resource_name"`
	InstanceIndex int    `json:"instance_index"`
}

// Provenance is what built a realization. RunbookName is the blueprint's
// name as the run that built the machine carried it.
type Provenance struct {
	RunbookName string `json:"runbook_name"`
}

// Realization is one provider object built (or adopted) for an intent.
type Realization struct {
	ID uuid.UUID `json:"id"`
	// IntegrationID is the connector the object was built through.
	IntegrationID uuid.UUID `json:"integration_id"`
	// AssetID is the inventory record the object was adopted as, nil when it
	// never was.
	AssetID *uuid.UUID `json:"asset_id,omitempty"`
	// ResourceRefID with IntegrationID is the provider object's identity.
	ResourceRefID string `json:"resource_ref_id"`
	ResourceKind  string `json:"resource_kind"`
	// Managed is whether the embedder claims the authority to act on it.
	Managed bool `json:"managed"`
	// RetiredAt nil is the current realization; set is history.
	RetiredAt  *time.Time         `json:"retired_at,omitempty"`
	Intent     *RealizationIntent `json:"intent,omitempty"`
	Provenance Provenance         `json:"provenance"`
	// IntentInputs are the inputs the instance was built from, nil when they
	// were not recorded.
	IntentInputs map[string]any `json:"intent_inputs,omitempty"`
	// MintedUUID is the identity the building run stamped on the object;
	// empty when it was built without one.
	MintedUUID string `json:"minted_uuid,omitempty"`
	// AdoptedAs names the blueprint an adopted machine stands in for.
	AdoptedAs string `json:"adopted_as,omitempty"`
}

// Current reports whether this is the live realization of its object.
func (r Realization) Current() bool { return r.RetiredAt == nil }

// Run is a run still running, held or queued, launched for an intent.
type Run struct {
	ID           uuid.UUID          `json:"id"`
	Status       string             `json:"status"`
	Intent       *RealizationIntent `json:"intent,omitempty"`
	IntentInputs map[string]any     `json:"-"`
}

// Blueprint is a provisioning blueprint as the planner reads it: its
// identity, its role, the blueprints that change, power, firewall and
// remove what it builds, and its inputs. How it runs is the embedder's.
type Blueprint struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	// Role is what the blueprint is for (BlueprintRole*), empty for none.
	Role           string           `json:"role,omitempty"`
	ReconfiguredBy string           `json:"reconfigured_by,omitempty"`
	DestroyedBy    string           `json:"destroyed_by,omitempty"`
	PoweredOnBy    string           `json:"powered_on_by,omitempty"`
	PoweredOffBy   string           `json:"powered_off_by,omitempty"`
	FirewalledBy   string           `json:"firewalled_by,omitempty"`
	AdoptedBy      string           `json:"adopted_by,omitempty"`
	Inputs         []BlueprintInput `json:"inputs"`
}

// BlueprintInput is one input a blueprint declares.
type BlueprintInput struct {
	Name     string            `json:"name"`
	Type     string            `json:"type"`
	Required bool              `json:"required,omitempty"`
	Default  interface{}       `json:"default,omitempty"`
	Label    string            `json:"label,omitempty"`
	Help     string            `json:"help,omitempty"`
	Source   string            `json:"source,omitempty"`
	Filter   map[string]string `json:"filter,omitempty"`
	// Selector names the picker that lists this input's values.
	Selector string `json:"selector,omitempty"`
	// InPlace: a change to this input can be applied to a built machine.
	InPlace bool `json:"in_place,omitempty"`
	// GrowOnly: an in-place change may only increase it.
	GrowOnly bool `json:"grow_only,omitempty"`
	// TakesImage: a resource's image resolves into this input.
	TakesImage bool `json:"takes_image,omitempty"`
	// Address is the address role this input fills (ip, gateway, …).
	Address string `json:"address,omitempty"`
	// Observes names the observed fact this input is checked against.
	Observes string `json:"observes,omitempty"`
}

// Selector is one of the connector's pickers.
type Selector struct {
	Name     string `json:"name"`
	Label    string `json:"label,omitempty"`
	Help     string `json:"help,omitempty"`
	Action   string `json:"action"`
	List     string `json:"list"`
	Value    string `json:"value"`
	LabelKey string `json:"label_key,omitempty"`
	// DependsOn are the selectors whose values this one's listing needs.
	DependsOn []string `json:"depends_on,omitempty"`
	RefKey    string   `json:"ref_key,omitempty"`
}

// Values the rules compare against. An embedder's records must use the same
// strings; Sybil Continuum pins its own constants to these in a test.
const (
	// InputSourceIntegrations marks a blueprint input that is the connector
	// itself, filled from spec.integration.
	InputSourceIntegrations = "integrations"

	// Acceptance verdicts.
	AcceptanceAccepted   = "accepted"
	AcceptanceRejected   = "rejected"
	AcceptanceIncomplete = "incomplete"

	// Blueprint roles.
	BlueprintRoleCreateMachine          = "create_machine"
	BlueprintRoleBuildImage             = "build_image"
	BlueprintRoleCreateNetwork          = "create_network"
	BlueprintRoleCreateInstallerMachine = "create_installer_machine"
	BlueprintRoleDeleteMachine          = "delete_machine"
)
