package manifest

import (
	"fmt"
	"strings"
)

// Placement is where a resource's machines may run, declared beside them so
// "keep the two databases apart" is part of the intent and not something an
// operator remembers when picking a node by hand.
//
// A resource without a placement block is placed as it always was: by its
// blueprint's node input, set in inputs or left to its default. With one, the
// planner chooses the node, and the blueprint's node input is the planner's to
// fill, so setting it in inputs as well is refused rather than one silently
// winning.
type Placement struct {
	// Node is PlacementAuto (the default) for the planner to choose, or the
	// name of one node, which the rules are then checked against.
	Node string
	// Nodes, when not empty, are the only nodes an automatic choice may pick.
	Nodes []string
	// Spread puts every instance of the resource on its own node.
	Spread bool
	// ApartFrom names other resources of the environment whose machines must
	// never share a node with this one's. The rule is symmetric: declaring it
	// on one side keeps both apart.
	ApartFrom []string
}

// PlacementAuto is a placement's node when the planner chooses it.
const PlacementAuto = "auto"

type placementWire struct {
	Node      string   `yaml:"node,omitempty"`
	Nodes     []string `yaml:"nodes,omitempty"`
	Spread    bool     `yaml:"spread,omitempty"`
	ApartFrom []string `yaml:"apart_from,omitempty"`
}

// Auto reports whether the planner chooses the node.
func (p Placement) Auto() bool { return p.Node == PlacementAuto }

func decodePlacement(w *placementWire) *Placement {
	if w == nil {
		return nil
	}
	p := &Placement{Node: strings.TrimSpace(w.Node), Spread: w.Spread}
	if p.Node == "" {
		p.Node = PlacementAuto
	}
	for _, n := range w.Nodes {
		p.Nodes = append(p.Nodes, strings.TrimSpace(n))
	}
	for _, r := range w.ApartFrom {
		p.ApartFrom = append(p.ApartFrom, strings.TrimSpace(r))
	}
	return p
}

func encodePlacement(p *Placement) *placementWire {
	if p == nil {
		return nil
	}
	return &placementWire{Node: p.Node, Nodes: p.Nodes, Spread: p.Spread, ApartFrom: p.ApartFrom}
}

// validatePlacement checks what can be checked without the cluster: names that
// are there, rules that name resources the manifest declares. Whether the
// cluster has enough nodes to honour them is the plan's to say.
func (e Environment) validatePlacement(r EnvironmentResource) error {
	p := r.Placement
	if p == nil {
		return nil
	}
	if p.Node == "" {
		return fmt.Errorf("%s %q: resource %q has an empty placement.node; leave it out or write auto", KindEnvironment, e.Name, r.Name)
	}
	if !p.Auto() && len(p.Nodes) > 0 {
		return fmt.Errorf("%s %q: resource %q names placement.node %q and placement.nodes; nodes limits an automatic choice, "+
			"so it goes with node: auto", KindEnvironment, e.Name, r.Name, p.Node)
	}
	seen := map[string]bool{}
	for _, n := range p.Nodes {
		if n == "" || n == PlacementAuto {
			return fmt.Errorf("%s %q: resource %q has %q in placement.nodes; want node names", KindEnvironment, e.Name, r.Name, n)
		}
		if seen[n] {
			return fmt.Errorf("%s %q: resource %q repeats node %q in placement.nodes", KindEnvironment, e.Name, r.Name, n)
		}
		seen[n] = true
	}
	apart := map[string]bool{}
	for _, other := range p.ApartFrom {
		switch {
		case other == r.Name:
			return fmt.Errorf("%s %q: resource %q is apart_from itself; spread: true keeps its own instances apart",
				KindEnvironment, e.Name, r.Name)
		case apart[other]:
			return fmt.Errorf("%s %q: resource %q repeats %q in placement.apart_from", KindEnvironment, e.Name, r.Name, other)
		case !e.declaresResource(other):
			return fmt.Errorf("%s %q: resource %q is apart_from %q, which this environment does not declare",
				KindEnvironment, e.Name, r.Name, other)
		}
		apart[other] = true
	}
	return nil
}

func (e Environment) declaresResource(name string) bool {
	for _, r := range e.Spec.Resources {
		if r.Name == name {
			return true
		}
	}
	return false
}
