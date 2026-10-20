package plan

import (
	"fmt"
	"net/netip"
	"strings"

	envmanifest "github.com/hazame-hub/accretion/manifest"
)

// Fixed addresses.
//
// A network card with `ip: static` gets one address per instance from its
// network's static pool. The allocation is the plan's, and it is stable:
//
//   - a machine that exists keeps the address it was built with, read back
//     from what its realization recorded — renumbering a running machine
//     because a resource above it changed count is exactly what IPAM is for
//     not doing;
//   - a new instance takes the lowest address no machine Sybil knows of holds:
//     any current realization in the org, and any run still building one.
//
// What a blueprint calls the inputs is its own business: the ones it marks
// Address "ip", "gateway" and "nameservers" are filled for the first card,
// and "ip1", "ip2", "ip3" for the later ones, which get an address and
// nothing else — one default route and one resolver list per machine.

// Address roles a blueprint input can declare (BlueprintInput.Address).
const (
	AddressIP          = "ip"
	AddressGateway     = "gateway"
	AddressNameservers = "nameservers"
)

// AddressIPN is the address role of network card n (0-based).
func AddressIPN(n int) string {
	if n == 0 {
		return AddressIP
	}
	return fmt.Sprintf("%s%d", AddressIP, n)
}

// addressInputs names the blueprint's inputs for card n's address, and for
// the first card its gateway and name servers.
func addressInputs(bp *Blueprint, n int) (ip, gw, ns string) {
	for _, in := range bp.Inputs {
		switch {
		case in.Address == AddressIPN(n):
			ip = in.Name
		case n == 0 && in.Address == AddressGateway:
			gw = in.Name
		case n == 0 && in.Address == AddressNameservers:
			ns = in.Name
		}
	}
	return ip, gw, ns
}

// cardKey is one network card of one instance.
type cardKey struct {
	instanceKey
	card int
}

// assignAddresses fills the address inputs of every `ip: static` card, and
// returns what is wrong, one message per resource and problem.
func assignAddresses(env envmanifest.Environment, instances []Instance, world Snapshot) []string {
	static := map[string]map[int]envmanifest.EnvironmentNetwork{}
	for _, r := range env.Spec.Resources {
		for j, nic := range r.NICs() {
			if nic.IP != "static" {
				continue
			}
			if n, ok := env.NetworkByName(nic.Network); ok {
				if static[r.Name] == nil {
					static[r.Name] = map[int]envmanifest.EnvironmentNetwork{}
				}
				static[r.Name][j] = n
			}
		}
	}
	if len(static) == 0 {
		return nil
	}

	// Every address-holding input name any shipped blueprint declares, by
	// card: what to read a realization's recorded inputs by.
	ipKeys := map[string]int{}
	for i := range world.Blueprints {
		for _, in := range world.Blueprints[i].Inputs {
			for n := 0; n < envmanifest.MaxNICs; n++ {
				if in.Address == AddressIPN(n) {
					ipKeys[in.Name] = n
				}
			}
		}
	}
	used := map[netip.Addr]bool{}
	note := func(inputs map[string]any) {
		for k := range ipKeys {
			if s, ok := inputs[k].(string); ok {
				if p, err := netip.ParsePrefix(s); err == nil {
					used[p.Addr()] = true
				}
			}
		}
	}
	kept := map[cardKey]string{}
	for _, r := range world.Realizations {
		if !r.Current() {
			continue
		}
		note(r.IntentInputs)
		if r.Intent != nil && r.Intent.Kind == IntentKind && r.Intent.Name == env.Name {
			for k, n := range ipKeys {
				if s, ok := r.IntentInputs[k].(string); ok && s != "" {
					kept[cardKey{instanceKey{r.Intent.ResourceName, r.Intent.InstanceIndex}, n}] = s
				}
			}
		}
	}
	for _, run := range world.InFlight {
		note(run.IntentInputs)
	}

	var errs []string
	said := map[string]bool{}
	report := func(msg string) {
		if !said[msg] {
			said[msg] = true
			errs = append(errs, msg)
		}
	}
	for i := range instances {
		in := &instances[i]
		cards, ok := static[in.Resource]
		if !ok {
			continue
		}
		bp := blueprintByID(world.Blueprints, in.Blueprint)
		if bp == nil {
			continue
		}
		for card := 0; card < envmanifest.MaxNICs; card++ {
			n, ok := cards[card]
			if !ok {
				continue
			}
			ipIn, gwIn, nsIn := addressInputs(bp, card)
			if ipIn == "" {
				if card == 0 {
					report(fmt.Sprintf("resource %q: blueprint %q cannot be given a fixed address", in.Resource, in.Blueprint))
				} else {
					report(fmt.Sprintf("resource %q: blueprint %q cannot give network card %d a fixed address", in.Resource, in.Blueprint, card))
				}
				continue
			}
			if _, both := in.Inputs[ipIn]; both {
				report(fmt.Sprintf("resource %q sets both ip: static and inputs.%s; pick one", in.Resource, ipIn))
				continue
			}
			prefix, err := netip.ParsePrefix(n.Subnet)
			if err != nil {
				continue
			}
			addr, ok := kept[cardKey{instanceKey{in.Resource, in.Index}, card}]
			if !ok {
				start, end, poolOK := n.StaticPool()
				if !poolOK {
					report(fmt.Sprintf("network %q has no addresses for ip: static", n.Name))
					continue
				}
				var free netip.Addr
				for a := start; !end.Less(a); a = a.Next() {
					if !used[a] {
						free = a
						break
					}
				}
				if !free.IsValid() {
					report(fmt.Sprintf("network %q: no free address left in %s-%s for resource %q", n.Name, start, end, in.Resource))
					continue
				}
				used[free] = true
				addr = netip.PrefixFrom(free, prefix.Bits()).String()
			}
			inputs := make(map[string]any, len(in.Inputs)+3)
			for k, v := range in.Inputs {
				inputs[k] = v
			}
			inputs[ipIn] = addr
			if gwIn != "" {
				inputs[gwIn] = n.Gateway
			}
			if nsIn != "" {
				inputs[nsIn] = strings.Join(n.DNS, ",")
			}
			in.Inputs = inputs
		}
	}
	return errs
}
