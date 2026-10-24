package plan

import (
	"fmt"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// Control-plane routes.
//
// The address a new machine is told to reach its control plane at is chosen
// per machine, most specific first: the network its first card joins, then
// the Environment. Neither set leaves the note empty, and the embedder's own
// default applies when the machine is built.
//
// The route is a first-boot fact, not part of what the machine is: once it
// has enrolled, the address it was told at boot describes nothing about it.
// So it is carried beside the inputs (Entry.ControlPlane), never in them —
// a route changed later must not make every machine already built plan as
// change, and a run's recorded declaration must stay what the manifest says.

// Address roles of the blueprint inputs a route fills.
const (
	AddressControlPlaneURL  = "control_plane_url"
	AddressControlPlaneGRPC = "control_plane_grpc"
	// AddressControlPlaneFrom takes where the route came from, so the run can
	// say it. Optional.
	AddressControlPlaneFrom = "control_plane_from"
)

// ControlPlaneNote is the route a create runs with, and where it came from:
// "network <name>" or "environment".
type ControlPlaneNote struct {
	URL  string `json:"url"`
	GRPC string `json:"grpc"`
	From string `json:"from"`
	// The blueprint's inputs it fills, by role.
	urlInput, grpcInput, fromInput string
}

// LaunchInputs is what a create's run is started with: the entry's inputs,
// plus its route when it has one. The run's recorded declaration stays
// e.Inputs.
func (e Entry) LaunchInputs() map[string]any {
	cp := e.ControlPlane
	if cp == nil {
		return e.Inputs
	}
	out := make(map[string]any, len(e.Inputs)+3)
	for k, v := range e.Inputs {
		out[k] = v
	}
	out[cp.urlInput] = cp.URL
	out[cp.grpcInput] = cp.GRPC
	if cp.fromInput != "" {
		out[cp.fromInput] = cp.From
	}
	return out
}

// controlPlaneInputs names the blueprint's route inputs.
func controlPlaneInputs(bp *Blueprint) (url, grpc, from string) {
	for _, in := range bp.Inputs {
		switch in.Address {
		case AddressControlPlaneURL:
			url = in.Name
		case AddressControlPlaneGRPC:
			grpc = in.Name
		case AddressControlPlaneFrom:
			from = in.Name
		}
	}
	return url, grpc, from
}

// resolveControlPlanes picks each instance's route, and returns what is wrong,
// one message per resource and problem.
func resolveControlPlanes(env envmanifest.Environment, instances []Instance, world Snapshot) (map[instanceKey]*ControlPlaneNote, []string) {
	resources := make(map[string]envmanifest.EnvironmentResource, len(env.Spec.Resources))
	for _, r := range env.Spec.Resources {
		resources[r.Name] = r
	}
	out := map[instanceKey]*ControlPlaneNote{}
	var errs []string
	reported := map[string]bool{}
	for _, in := range instances {
		cp, from := env.Spec.ControlPlane, "environment"
		if nics := resources[in.Resource].NICs(); len(nics) > 0 {
			if n, ok := env.NetworkByName(nics[0].Network); ok && n.ControlPlane != nil {
				cp, from = n.ControlPlane, "network "+n.Name
			}
		}
		if cp == nil {
			continue
		}
		bp := blueprintByID(world.Blueprints, in.Blueprint)
		if bp == nil {
			continue
		}
		urlIn, grpcIn, fromIn := controlPlaneInputs(bp)
		if urlIn == "" || grpcIn == "" {
			report(&errs, reported, fmt.Sprintf("resource %q: blueprint %q cannot be given a control-plane route (control_plane from %s)",
				in.Resource, in.Blueprint, from))
			continue
		}
		set := false
		for _, k := range []string{urlIn, grpcIn, fromIn} {
			if _, ok := in.Inputs[k]; ok && k != "" {
				report(&errs, reported, fmt.Sprintf("resource %q sets inputs.%s and gets control_plane from %s; pick one",
					in.Resource, k, from))
				set = true
			}
		}
		if set {
			continue
		}
		out[instanceKey{in.Resource, in.Index}] = &ControlPlaneNote{
			URL: cp.URL, GRPC: cp.GRPC, From: from,
			urlInput: urlIn, grpcInput: grpcIn, fromInput: fromIn,
		}
	}
	return out, errs
}
