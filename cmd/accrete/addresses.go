package main

import (
	"fmt"
	"net/netip"
	"sort"
	"strings"

	"github.com/hazame-hub/accretion/plan"
)

// addressKeys are the inputs that give a card a fixed address.
var addressKeys = []string{"ip_address", "ip_address1", "ip_address2", "ip_address3"}

// checkAddresses makes a fixed address that two declared machines would both
// take a plan error: the second to boot would knock the first off the
// network. A resource with a count gives each instance its own with
// {{instance.number}} (10.0.0.{{instance.number}}/24), or lets a network
// with ip: static allocate them.
func checkAddresses(p *plan.Plan) {
	users := map[netip.Addr][]string{}
	for _, e := range p.Entries {
		if e.Action == plan.ActionDelete {
			continue
		}
		for _, k := range addressKeys {
			s, _ := e.Inputs[k].(string)
			pfx, err := netip.ParsePrefix(strings.TrimSpace(s))
			if err != nil {
				continue
			}
			users[pfx.Addr()] = append(users[pfx.Addr()], fmt.Sprintf("%s/%d", e.Resource, e.Index))
		}
	}
	addrs := make([]netip.Addr, 0, len(users))
	for a, who := range users {
		if len(who) > 1 {
			addrs = append(addrs, a)
		}
	}
	sort.Slice(addrs, func(i, j int) bool { return addrs[i].Less(addrs[j]) })
	for _, a := range addrs {
		p.Errors = append(p.Errors, fmt.Sprintf("%s is the fixed address of %s; each machine needs its own (use {{instance.number}} in it, or a network with ip: static)",
			a, strings.Join(users[a], " and ")))
	}
}
