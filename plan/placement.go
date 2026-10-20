package plan

import (
	"fmt"
	"sort"
	"strings"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// Placement in a plan.
//
// A resource with a placement block has its node chosen here rather than typed
// into inputs. The choice is the node with the most memory left once running
// guests and the machines this plan already placed are counted — memory,
// because it is what a Proxmox host runs out of first (flk-vega-01 has 27 GiB
// and is promised 72) — among the nodes the rules leave: online, in
// placement.nodes when given, and not holding a machine the resource must stay
// apart from.
//
// The choice is written into the instance's node input, so it is what the plan
// shows, what the run is launched with and what the realization records. A
// machine that already exists keeps the node it was built on: placement
// chooses where new machines go and never moves one, so an existing pair that
// breaks a rule is a warning, not a change an apply could make.
//
// Instances are placed one at a time in manifest order, each against what the
// earlier ones took. That is greedy and not a solver: with narrow
// placement.nodes lists it can fail where another order would fit, and the
// error then names what each node was refused for.

// NodeCapacity is one node of the connector's cluster as the plan read it.
type NodeCapacity struct {
	Name                 string
	Online               bool
	MemoryBytes          int64
	AllocatedMemoryBytes int64
}

// PlacementNote says which node an instance is, or will be, on and why.
type PlacementNote struct {
	Node string `json:"node"`
	// Auto is true when the plan chose the node.
	Auto   bool   `json:"auto"`
	Reason string `json:"reason"`
	// FreeBytes is the node's memory left before this machine, when the plan
	// chose it.
	FreeBytes *int64 `json:"free_bytes,omitempty"`
}

// NodeSelector is the selector a blueprint's node input names: the input the
// planner fills when it chooses a node.
const NodeSelector = "node"

// PlacementByMemory is the limit stated whenever the plan chooses a node.
const PlacementByMemory = "nodes are chosen by free memory only; CPU and storage are not weighed"

type placed struct {
	key    instanceKey
	node   string
	create bool
	auto   bool
}

// place chooses nodes for the instances of resources that declare placement,
// writing each into the instance's node input, and checks every rule against
// where the machines are or will be.
func place(env envmanifest.Environment, instances []Instance, world Snapshot) (notes map[instanceKey]*PlacementNote, errs, warnings, limits []string) {
	rules := map[string]*envmanifest.Placement{}
	for _, r := range env.Spec.Resources {
		if r.Placement != nil {
			rules[r.Name] = r.Placement
		}
	}
	if len(rules) == 0 || world.Connector == nil {
		return nil, nil, nil, nil
	}

	// The node input of each resource's blueprint, and its default.
	nodeInput := map[string]string{}
	nodeDefault := map[string]string{}
	for _, r := range env.Spec.Resources {
		bp := blueprintByID(world.Blueprints, r.Blueprint)
		if bp == nil {
			continue // checkReferences says so
		}
		for _, in := range bp.Inputs {
			if in.Selector == NodeSelector {
				nodeInput[r.Name] = in.Name
				if s, ok := in.Default.(string); ok {
					nodeDefault[r.Name] = s
				}
				break
			}
		}
		if r.Placement == nil {
			continue
		}
		name := nodeInput[r.Name]
		switch {
		case name == "":
			errs = append(errs, fmt.Sprintf("resource %q has a placement, but blueprint %q has no node input to place it with",
				r.Name, r.Blueprint))
			delete(rules, r.Name)
		case r.Inputs[name] != nil:
			errs = append(errs, fmt.Sprintf("resource %q sets inputs.%s and a placement; placement chooses the node, "+
				"so remove the input (or write placement.node: %v)", r.Name, name, r.Inputs[name]))
		}
	}

	current := map[instanceKey]Realization{}
	for _, r := range world.Realizations {
		if r.Current() && r.Intent != nil && r.Intent.Kind == IntentKind && r.Intent.Name == env.Name {
			current[instanceKey{r.Intent.ResourceName, r.Intent.InstanceIndex}] = r
		}
	}
	building := map[instanceKey]map[string]any{}
	for _, run := range world.InFlight {
		if run.Intent != nil && run.Intent.Kind == IntentKind && run.Intent.Name == env.Name {
			building[instanceKey{run.Intent.ResourceName, run.Intent.InstanceIndex}] = run.IntentInputs
		}
	}

	notes = map[instanceKey]*PlacementNote{}
	var where []placed
	var queue []int
	for i := range instances {
		in := &instances[i]
		key := instanceKey{in.Resource, in.Index}
		input := nodeInput[in.Resource]
		if input == "" {
			continue
		}
		rule := rules[in.Resource]
		built, isBuilt := current[key]
		builtInputs := built.IntentInputs
		if !isBuilt {
			builtInputs, isBuilt = building[key]
		}
		switch {
		case isBuilt:
			node, recorded := builtInputs[input].(string)
			if !recorded {
				node = nodeDefault[in.Resource]
			}
			if rule != nil && rule.Auto() && recorded {
				// Where it was built, so choosing again cannot plan a move.
				setInput(in, input, node)
			}
			if rule != nil && !rule.Auto() {
				setInput(in, input, rule.Node)
			}
			if node != "" {
				where = append(where, placed{key: key, node: node})
				if rule != nil {
					notes[key] = &PlacementNote{Node: node, Reason: "already built on this node; placement never moves a machine"}
				}
			}
		case rule == nil:
			node, _ := in.Inputs[input].(string)
			if node == "" {
				node = nodeDefault[in.Resource]
			}
			if node != "" {
				where = append(where, placed{key: key, node: node, create: true})
			}
		case !rule.Auto():
			setInput(in, input, rule.Node)
			where = append(where, placed{key: key, node: rule.Node, create: true})
			notes[key] = &PlacementNote{Node: rule.Node, Reason: "the node placement.node names"}
		default:
			queue = append(queue, i)
		}
	}

	if len(queue) > 0 {
		limits = append(limits, PlacementByMemory)
		var nodes []NodeCapacity
		if world.Capacity != nil {
			nodes = world.Capacity.Nodes
		}
		if len(nodes) == 0 {
			reason := world.CapacityError
			if reason == "" {
				reason = "the connector did not list them"
			}
			reported := map[string]bool{}
			for _, i := range queue {
				instances[i].Unplaced = true
				if r := instances[i].Resource; !reported[r] {
					reported[r] = true
					errs = append(errs, fmt.Sprintf("resource %q: placement chooses a node by free memory, "+
						"and the cluster's nodes could not be read: %s", r, reason))
				}
			}
		} else {
			taken := map[string]int64{}
			for _, i := range queue {
				in := &instances[i]
				key := instanceKey{in.Resource, in.Index}
				rule := rules[in.Resource]
				mem, sized := instanceMemory(world, in.Blueprint, in.Inputs)
				best, free, refused := choose(nodes, rule, in.Resource, where, rules, taken)
				if best == "" {
					errs = append(errs, fmt.Sprintf("%s: no node can take it: %s", instanceName(key), strings.Join(refused, "; ")))
					in.Unplaced = true
					continue
				}
				setInput(in, nodeInput[in.Resource], best)
				where = append(where, placed{key: key, node: best, create: true, auto: true})
				taken[best] += mem
				note := &PlacementNote{Node: best, Auto: true, FreeBytes: &free}
				switch {
				case free < 0:
					note.Reason = fmt.Sprintf("the least overcommitted node allowed, though it has no free memory: "+
						"it is already promised %s more than it has", gib(-free))
				case !sized:
					note.Reason = fmt.Sprintf("the most free memory (%s); this machine's own memory is not known, so not counted", gib(free))
				default:
					note.Reason = fmt.Sprintf("the most free memory: %s before this machine, %s after", gib(free), gib(free-mem))
				}
				switch {
				case free < 0:
					w := fmt.Sprintf("%s goes to %s, the least overcommitted node it may use, "+
						"which is already promised %s more than it has", instanceName(key), best, gib(-free))
					if sized {
						w += "; this machine adds " + gib(mem)
					}
					warnings = append(warnings, w)
				case sized && free-mem < 0:
					warnings = append(warnings, fmt.Sprintf("%s goes to %s, the node with the most free memory, "+
						"and overcommits it by %s", instanceName(key), best, gib(mem-free)))
				}
				if len(refused) > 0 {
					note.Reason += "; ruled out: " + strings.Join(refused, "; ")
				}
				notes[key] = note
			}
		}
	}

	// Every pair the rules keep apart that shares a node. A choice above never
	// makes one; a fixed node or a machine built before the rule can.
	for i := 0; i < len(where); i++ {
		for j := i + 1; j < len(where); j++ {
			a, b := where[i], where[j]
			if a.node != b.node {
				continue
			}
			rule := conflict(a.key.resource, b.key.resource, rules)
			if rule == "" {
				continue
			}
			if a.create || b.create {
				errs = append(errs, fmt.Sprintf("%s and %s would both be on %s, which %s forbids",
					instanceName(a.key), instanceName(b.key), a.node, rule))
			} else {
				warnings = append(warnings, fmt.Sprintf("%s and %s are both on %s, which %s forbids; "+
					"an apply does not move machines, so they stay together until one is rebuilt",
					instanceName(a.key), instanceName(b.key), a.node, rule))
			}
		}
	}
	return notes, errs, warnings, limits
}

// choose is the node with the most free memory among those the rules allow,
// or "" and why each node was refused.
func choose(nodes []NodeCapacity, rule *envmanifest.Placement, resource string, where []placed,
	rules map[string]*envmanifest.Placement, taken map[string]int64) (string, int64, []string) {
	allowed := map[string]bool{}
	for _, n := range rule.Nodes {
		allowed[n] = true
	}
	sorted := append([]NodeCapacity(nil), nodes...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })

	best, bestFree := "", int64(0)
	var refused []string
	for _, n := range sorted {
		if len(allowed) > 0 && !allowed[n.Name] {
			continue // not a candidate at all, which is not worth a line
		}
		if !n.Online || n.MemoryBytes <= 0 {
			refused = append(refused, n.Name+" is offline or did not report its memory")
			continue
		}
		var blocker string
		for _, p := range where {
			if p.node != n.Name {
				continue
			}
			if why := conflict(resource, p.key.resource, rules); why != "" {
				blocker = fmt.Sprintf("%s holds %s (%s)", n.Name, instanceName(p.key), why)
				break
			}
		}
		if blocker != "" {
			refused = append(refused, blocker)
			continue
		}
		free := n.MemoryBytes - n.AllocatedMemoryBytes - taken[n.Name]
		if best == "" || free > bestFree {
			best, bestFree = n.Name, free
		}
	}
	if best == "" && len(refused) == 0 {
		refused = append(refused, fmt.Sprintf("none of placement.nodes (%s) is a node of this cluster", strings.Join(rule.Nodes, ", ")))
	}
	if best != "" {
		// What was refused only matters as context for the choice.
		return best, bestFree, refused
	}
	return "", 0, refused
}

// conflict names the rule that keeps a machine of resource a off a node with
// one of resource b, or "" when none does.
func conflict(a, b string, rules map[string]*envmanifest.Placement) string {
	if a == b {
		if r := rules[a]; r != nil && r.Spread {
			return fmt.Sprintf("%q's spread", a)
		}
		return ""
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		if r := rules[pair[0]]; r != nil {
			for _, other := range r.ApartFrom {
				if other == pair[1] {
					return fmt.Sprintf("%q's apart_from", pair[0])
				}
			}
		}
	}
	return ""
}

func setInput(in *Instance, name string, v any) {
	if in.Inputs == nil {
		in.Inputs = map[string]any{}
	}
	in.Inputs[name] = v
}

func instanceName(k instanceKey) string { return fmt.Sprintf("%s[%d]", k.resource, k.index) }

// instanceMemory is the memory one instance asks for, in bytes: its declared
// memory input, else the blueprint's default.
func instanceMemory(world Snapshot, blueprint string, inputs map[string]any) (int64, bool) {
	v, declared := inputs[MemoryInput]
	if !declared {
		v = blueprintDefault(world, blueprint, MemoryInput)
	}
	mib, ok := number(v)
	if !ok || mib <= 0 {
		return 0, false
	}
	return int64(mib) << 20, true
}
