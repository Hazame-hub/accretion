package plan

import (
	"fmt"
	"sort"
	"strings"

	"github.com/google/uuid"
	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// Networks in a plan.
//
// A declared network is planned against what the provider says exists, read
// at plan time — not against a record of what Sybil built. That is deliberate
// and it is the opposite of how machines are planned. A machine needs a
// realization because its identity is something Sybil stamped on it and only
// Sybil can recognise; a vnet's identity is its name, which the provider
// itself enforces as unique. Asking the provider is therefore the whole truth
// for "does it exist and is it what the manifest says", and a second record
// would only be a copy that can go stale.
//
// What that cannot answer is ownership: a vnet with the declared name and
// subnet plans as no-op whoever made it. That is safe only because nothing
// here ever deletes or rewrites a network — CHANGE is shown, never acted on —
// and it is stated in the plan's limits whenever networks are declared.

// NetworkResourcePrefix marks the runs an apply launches for networks, in the
// intent's resource name. The planner reads such runs back only as networks,
// never as machine instances.
const NetworkResourcePrefix = "network/"

// NetworksNotOwned is the limit every plan that declares a network carries.
const NetworksNotOwned = "networks are compared with what the provider has, by name: an existing network with the " +
	"declared name and subnet plans as unchanged whoever created it, and Sybil never deletes or rewrites one"

// ObservedSDN is the provider's networks as the plan read them.
type ObservedSDN struct {
	// Vnets by name.
	Vnets map[string]ObservedVnet
	// ZoneDHCP is each zone's DHCP setting ("dnsmasq", or "" for none).
	ZoneDHCP map[string]string
	// Pending names every SDN edit that is defined but not applied. Applying
	// a network reloads the whole cluster's SDN, which would publish these
	// too.
	Pending []string
}

// ObservedVnet is one network on the provider.
type ObservedVnet struct {
	Zone    string
	Pending bool
	Subnets []ObservedSubnet
	// NotUp names the nodes where the vnet's bridge is not up, with the
	// status each reports. Configured is not live: a machine on a vnet that
	// is down on its node cannot start.
	NotUp []string
}

// ObservedSubnet is one subnet of an ObservedVnet.
type ObservedSubnet struct {
	CIDR      string
	Gateway   string
	SNAT      bool
	DHCPRange string
	Pending   bool
}

// NetworkEntry is one declared network's line in the plan.
type NetworkEntry struct {
	Name    string `json:"name"`
	Zone    string `json:"zone"`
	Subnet  string `json:"subnet"`
	Gateway string `json:"gateway"`
	SNAT    bool   `json:"snat"`
	// DHCPRange is what the network's DHCP server hands out, empty for none.
	DHCPRange string `json:"dhcp_range,omitempty"`
	Action    Action `json:"action"`
	Reason    string `json:"reason,omitempty"`
	// Blueprint and Inputs are what a create would run.
	Blueprint string         `json:"blueprint,omitempty"`
	Inputs    map[string]any `json:"inputs,omitempty"`
	// RunID is the run already creating this network, when one is in flight.
	RunID *uuid.UUID `json:"run_id,omitempty"`
}

// planNetworks plans every declared network.
func planNetworks(env envmanifest.Environment, world Snapshot, building map[instanceKey]uuid.UUID) ([]NetworkEntry, []string) {
	if len(env.Spec.Networks) == 0 {
		return nil, nil
	}
	var errs []string
	bp := blueprintByRole(world.Blueprints, BlueprintRoleCreateNetwork)
	if world.Connector != nil && bp == nil {
		errs = append(errs, fmt.Sprintf("connector %q (%s) ships no blueprint that creates networks, so spec.networks cannot be realized through it",
			world.Connector.Name, world.Connector.Type))
	}

	out := make([]NetworkEntry, 0, len(env.Spec.Networks))
	for _, n := range env.Spec.Networks {
		e := NetworkEntry{Name: n.Name, Zone: n.Zone, Subnet: n.Subnet, Gateway: n.Gateway, SNAT: n.SNAT, DHCPRange: n.DHCPRange}
		if bp != nil {
			e.Blueprint = bp.ID
			e.Inputs = networkInputs(n, *bp, world)
		}
		if runID, inFlight := building[instanceKey{NetworkResourcePrefix + n.Name, 0}]; inFlight {
			e.RunID = &runID
			e.Action, e.Reason = ActionUnknown, "a run is already creating this network; its outcome is not known yet"
			out = append(out, e)
			continue
		}
		e.Action, e.Reason = classifyNetwork(n, world)
		out = append(out, e)
	}
	return out, errs
}

// classifyNetwork compares one declared network with the provider.
func classifyNetwork(n envmanifest.EnvironmentNetwork, world Snapshot) (Action, string) {
	if world.SDN == nil {
		reason := "the provider's networks could not be read"
		if world.SDNError != "" {
			reason += ": " + world.SDNError
		}
		return ActionUnknown, reason
	}
	v, exists := world.SDN.Vnets[n.Name]
	if exists {
		switch {
		case v.Pending:
			return ActionUnknown, "a network with this name is defined on the provider but not applied yet"
		case v.Zone != n.Zone:
			return ActionChange, fmt.Sprintf("exists in zone %q, not %q; moving it would re-plumb every machine on it", v.Zone, n.Zone)
		case n.DHCPRange != "" && world.SDN.ZoneDHCP[v.Zone] == "":
			return ActionChange, fmt.Sprintf("exists in zone %q, which serves no DHCP; the declared range would never be handed out", v.Zone)
		case len(v.NotUp) > 0:
			return ActionUnknown, fmt.Sprintf("configured on the provider but not up on %s; a zone with DHCP needs dnsmasq on every node",
				strings.Join(v.NotUp, ", "))
		}
		for _, s := range v.Subnets {
			if s.CIDR != n.Subnet {
				continue
			}
			switch {
			case s.Pending:
				return ActionUnknown, "the subnet is defined on the provider but not applied yet"
			case s.Gateway != n.Gateway:
				return ActionChange, fmt.Sprintf("exists with gateway %s, not %s", s.Gateway, n.Gateway)
			case s.SNAT != n.SNAT:
				return ActionChange, fmt.Sprintf("exists with outbound NAT %s", onOff(s.SNAT))
			case s.DHCPRange != n.DHCPRange:
				return ActionChange, fmt.Sprintf("exists with DHCP range %q, not %q", s.DHCPRange, n.DHCPRange)
			}
			return ActionNoOp, "exists on the provider as declared"
		}
		if len(v.Subnets) > 0 {
			cidrs := make([]string, 0, len(v.Subnets))
			for _, s := range v.Subnets {
				cidrs = append(cidrs, s.CIDR)
			}
			sort.Strings(cidrs)
			return ActionChange, fmt.Sprintf("exists with subnet %s, not %s", strings.Join(cidrs, ", "), n.Subnet)
		}
		// The vnet is there without a subnet: a previous run stopped half
		// way. Creating finishes it — every step is idempotent.
	}
	if dhcp, zoneExists := world.SDN.ZoneDHCP[n.Zone]; zoneExists && dhcp != wantDHCP(n) {
		return ActionChange, fmt.Sprintf("zone %q exists with DHCP %q, and DHCP is a zone setting: the network cannot be created "+
			"there as declared", n.Zone, dhcp)
	}
	if len(world.SDN.Pending) > 0 {
		return ActionUnknown, fmt.Sprintf("the provider has SDN edits that are not applied (%s); creating a network reloads "+
			"the whole cluster's SDN and would publish them too, so apply or revert them on the provider first",
			strings.Join(world.SDN.Pending, ", "))
	}
	return ActionCreate, ""
}

// networkInputs is what the network blueprint is run with.
func networkInputs(n envmanifest.EnvironmentNetwork, bp Blueprint, world Snapshot) map[string]any {
	zoneDHCP := ""
	if n.DHCPRange != "" {
		zoneDHCP = wantDHCP(n)
	}
	in := map[string]any{"zone": n.Zone, "vnet": n.Name, "subnet": n.Subnet, "gateway": n.Gateway, "snat": n.SNAT,
		"dhcp_range": n.DHCPRange, "zone_dhcp": zoneDHCP}
	return withConnector(Instance{Blueprint: bp.ID, Inputs: in},
		Snapshot{Connector: world.Connector, Blueprints: []Blueprint{bp}})
}

// gateOnNetworks keeps a machine from being created onto a network the same
// manifest declares but the provider does not have live yet.
//
// The alternative — launch the machine and the network together — leaves the
// order to whoever approves the runs, and a machine approved first fails on a
// NIC naming a bridge that does not exist, after its VM was cloned. Waiting
// costs one more apply; it never costs a rollback.
func gateOnNetworks(env envmanifest.Environment, entries []Entry, networks []NetworkEntry) {
	state := map[string]NetworkEntry{}
	for _, n := range networks {
		state[n.Name] = n
	}
	byResource := map[string][]envmanifest.EnvironmentNIC{}
	for _, r := range env.Spec.Resources {
		byResource[r.Name] = r.NICs()
	}
	for i := range entries {
		e := &entries[i]
		if e.Action != ActionCreate {
			continue
		}
		// Every card's network, not only the first: a VM whose second NIC
		// names a bridge that does not exist yet fails just the same.
		for _, nic := range byResource[e.Resource] {
			n, declared := state[nic.Network]
			if !declared || n.Action == ActionNoOp {
				continue
			}
			e.Action = ActionUnknown
			e.Reason = fmt.Sprintf("waits for network %q, which is not live yet (%s); apply again once it is", n.Name, n.Action)
			break
		}
	}
}

func blueprintByRole(bps []Blueprint, role string) *Blueprint {
	for i := range bps {
		if bps[i].Role == role {
			return &bps[i]
		}
	}
	return nil
}

func onOff(b bool) string {
	if b {
		return "on"
	}
	return "off"
}

func wantDHCP(n envmanifest.EnvironmentNetwork) string {
	if n.DHCPRange != "" {
		return "dnsmasq"
	}
	return ""
}

// DHCPNeedsDnsmasq is the limit stated whenever a network to create serves
// DHCP. Proxmox cannot be asked whether the package is installed, and a zone
// with dnsmasq DHCP on a node without it makes every SDN reload on the cluster
// fail — found on flk-vega-01.
const DHCPNeedsDnsmasq = "DHCP on a network needs the dnsmasq package on every node (apt install dnsmasq && " +
	"systemctl disable --now dnsmasq); Sybil cannot check this, and without it every SDN reload on the cluster fails"
