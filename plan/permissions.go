package plan

import (
	"fmt"
	"sort"
	"strings"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// The token's network permissions in a plan.
//
// A connector's token that may clone machines may still be unable to create a
// network or attach a NIC to one: the provider checks those on their own ACL
// paths, which a token scoped to "the VMs" never mentions. Without this the
// first sign was an apply already under way — a network run failing at its
// subnet step, or a clone that had copied its disk failing at the NIC. The
// plan asks first, and a privilege it knows is missing is an error, so the
// apply that would have failed half way is refused whole.
//
// Which privileges a network or a bridge needs, and on which path, is the
// provider's knowledge and stays in its package: the plan hands it the
// networks the manifest declares and the bridges its NICs name, and gets back
// what is missing for each. A connector that cannot answer — one whose
// provider has no such check, or whose token cannot read its own ACL — leaves
// the plan saying it did not check, never claiming it did.

// NetworkAccess is what the connector reported the token lacks.
type NetworkAccess struct {
	// Networks are keyed by network name. Zone is what creating the
	// network's zone would need, Network what creating the network itself
	// would; either is empty when nothing is missing.
	Networks map[string]NetworkGrants
	// Bridges are keyed by the bridge a NIC names, which is a network's name
	// when the NIC joins one.
	Bridges map[string][]MissingPrivilege
}

// NetworkGrants is what one network's creation lacks.
type NetworkGrants struct {
	Zone    []MissingPrivilege
	Network []MissingPrivilege
}

// MissingPrivilege is one privilege the token lacks on one path.
type MissingPrivilege struct {
	Privilege string
	Path      string
}

// NetworkAccessQuery is what a plan of env asks the connector about: every
// network the manifest declares, and every bridge an instance's NIC names.
// Both are empty for a manifest with neither, and the caller then asks
// nothing.
//
// The bridges are read from the instances' expanded inputs, not from
// resource.network alone: a resource may set the NIC input itself, and a
// bridge named there is attached to all the same.
func NetworkAccessQuery(env envmanifest.Environment) (networks []envmanifest.EnvironmentNetwork, bridges []string) {
	instances, _ := Expand(env)
	seen := map[string]bool{}
	for _, in := range instances {
		for _, b := range nicBridges(in.Inputs) {
			if !seen[b] {
				seen[b] = true
				bridges = append(bridges, b)
			}
		}
	}
	sort.Strings(bridges)
	return env.Spec.Networks, bridges
}

// nicBridges is the bridge= of each of an instance's NIC inputs, net0 to
// the last card a manifest may declare, in card order.
func nicBridges(inputs map[string]any) []string {
	var out []string
	for n := 0; n < envmanifest.MaxNICs; n++ {
		if b := nicBridge(inputs, envmanifest.NICInputN(n)); b != "" {
			out = append(out, b)
		}
	}
	return out
}

// nicBridge is the bridge= of one NIC input, or "" when it names none or is
// not a plain string (a variable left unfilled reaches here as its
// placeholder, which names no bridge).
func nicBridge(inputs map[string]any, input string) string {
	s, ok := inputs[input].(string)
	if !ok {
		return ""
	}
	for _, part := range strings.Split(s, ",") {
		if v, found := strings.CutPrefix(strings.TrimSpace(part), "bridge="); found && !strings.Contains(v, "{{") {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

// NetworkAccessNotChecked opens the limit a plan states when it needed the
// token's network permissions and could not read them.
const NetworkAccessNotChecked = "the token's network permissions were not checked"

// checkNetworkAccess reports, as plan errors, the privileges the token lacks
// for what this apply would do: create the networks planned as create, and
// attach the NICs of the machines it would build or reconfigure.
//
// Only those. A network planned as unchanged needs nothing created, and one
// planned as change is shown, never acted on. A machine already built is not
// attached to anything again unless its NIC is what changed; refusing an
// apply of a settled Environment because the token lost SDN.Use since would
// block its deletes for a privilege none of them needs. A machine waiting on
// a network is counted: it is what the next apply creates, and the operator
// should hear now rather than then.
func checkNetworkAccess(env envmanifest.Environment, plan *Plan, world Snapshot) {
	creates := []NetworkEntry{}
	for _, n := range plan.Networks {
		if n.Action == ActionCreate {
			creates = append(creates, n)
		}
	}
	attaching := map[string][]string{} // bridge → resources, in manifest order
	for _, e := range plan.Entries {
		builds := e.Action == ActionCreate ||
			(e.Action == ActionUnknown && e.RealizationID == nil && e.RunID == nil && e.Blueprint != "") ||
			(e.Apply == ApplyReconfigure && diffsANIC(e.Diff))
		if !builds {
			continue
		}
		for _, b := range nicBridges(e.Inputs) {
			rs := attaching[b]
			if len(rs) == 0 || rs[len(rs)-1] != e.Resource {
				attaching[b] = append(rs, e.Resource)
			}
		}
	}
	if len(creates) == 0 && len(attaching) == 0 {
		return
	}

	if world.NetworkAccess == nil {
		reason := NetworkAccessNotChecked
		if world.NetworkAccessError != "" {
			reason += " (" + world.NetworkAccessError + ")"
		}
		plan.Limits = append(plan.Limits, reason+", so an apply may still stop half way on a privilege the token lacks")
		return
	}

	for _, n := range creates {
		g := world.NetworkAccess.Networks[n.Name]
		var missing []MissingPrivilege
		// The zone is created only when the provider does not have it; the
		// provider already left Zone empty when it does, and the plan's own
		// read of the SDN is a second word on it.
		if !zoneKnown(world, n.Zone) {
			missing = append(missing, g.Zone...)
		}
		missing = append(missing, g.Network...)
		if len(missing) > 0 {
			plan.Errors = append(plan.Errors, fmt.Sprintf("token cannot create network %q: it lacks %s; grant %s to the token's role",
				n.Name, lacks(missing), them(missing)))
		}
	}

	bridges := make([]string, 0, len(attaching))
	for b := range attaching {
		bridges = append(bridges, b)
	}
	sort.Strings(bridges)
	for _, b := range bridges {
		missing := world.NetworkAccess.Bridges[b]
		if len(missing) == 0 {
			continue
		}
		plan.Errors = append(plan.Errors, fmt.Sprintf("token cannot attach machines to network %q (resource %s): it lacks %s; grant %s to the token's role",
			b, quoteAll(attaching[b]), lacks(missing), them(missing)))
	}
}

func zoneKnown(world Snapshot, zone string) bool {
	if world.SDN == nil {
		return false
	}
	_, ok := world.SDN.ZoneDHCP[zone]
	return ok
}

// diffsANIC reports whether any of the machine's NIC inputs is what changed.
func diffsANIC(diff []InputDiff) bool {
	for _, d := range diff {
		for n := 0; n < envmanifest.MaxNICs; n++ {
			if d.Input == envmanifest.NICInputN(n) {
				return true
			}
		}
	}
	return false
}

// lacks renders "SDN.Use on /sdn/zones/sybil/back and SDN.Allocate on /sdn".
func lacks(missing []MissingPrivilege) string {
	parts := make([]string, 0, len(missing))
	for _, m := range missing {
		parts = append(parts, m.Privilege+" on "+m.Path)
	}
	if len(parts) == 1 {
		return parts[0]
	}
	return strings.Join(parts[:len(parts)-1], ", ") + " and " + parts[len(parts)-1]
}

func them(missing []MissingPrivilege) string {
	if len(missing) == 1 {
		return "it"
	}
	return "them"
}

func quoteAll(names []string) string {
	q := make([]string, 0, len(names))
	for _, n := range names {
		q = append(q, fmt.Sprintf("%q", n))
	}
	return strings.Join(q, ", ")
}
