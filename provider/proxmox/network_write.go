package proxmox

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/hazame-hub/accretion/provider"
)

// CreateNetwork: an SDN network on Proxmox, made the way Continuum's
// provision-proxmox-network blueprint makes it (simple zone with the PVE
// IPAM, vnet, subnet, cluster SDN apply, then every online node's network
// reloaded), with the lessons that blueprint learnt on flk-vega-01 kept:
//
//   - the cluster task finishing is not the bridge being up: each node
//     reloads on its own, so the call returns only once the vnet is
//     available on every online node;
//   - a node's reload applies whatever network edits that node has staged,
//     and the cluster apply publishes every pending SDN edit, so both are
//     refused, before anything is sent, when someone else's edits wait;
//   - DHCP is a zone setting, so a zone that exists with the other DHCP is
//     refused rather than changed under its other networks.
//
// Every step reads first and only defines what is missing, so a repeat (or a
// rerun after a lost answer) finishes the same network instead of failing on
// "already exists".

// sdnID is a zone or vnet id as Proxmox accepts it: a letter, then letters
// and digits, at most 8 in all.
var sdnID = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9]{0,7}$`)

// NetworkWait bounds the SDN apply task, each node's reload, and the wait
// for the vnet to be up everywhere. A variable so tests can shorten it.
var NetworkWait = 2 * time.Minute

// CreateNetwork implements provider.NetworkWriter.
func (s *session) CreateNetwork(ctx context.Context, n provider.NetworkSpec) (provider.NetworkCreated, error) {
	if err := checkNetworkSpec(n); err != nil {
		return provider.NetworkCreated{}, err
	}
	wantDHCP := ""
	if n.DHCPRange != "" {
		wantDHCP = "dnsmasq"
	}

	st, err := s.c.SDN(ctx)
	if err != nil {
		return provider.NetworkCreated{}, err
	}
	var zone *SDNZone
	for i := range st.Zones {
		if st.Zones[i].Zone == n.Zone {
			zone = &st.Zones[i]
		}
	}
	var vnet *SDNVnet
	for i := range st.Vnets {
		if st.Vnets[i].Vnet == n.Name {
			vnet = &st.Vnets[i]
		}
	}
	var subnet *SDNSubnet

	switch {
	case zone != nil && zone.Type != "" && zone.Type != "simple":
		return provider.NetworkCreated{}, provider.Refused("zone %s exists as a %s zone; accrete creates networks in simple zones only", n.Zone, zone.Type)
	case zone != nil && zone.DHCP != wantDHCP:
		return provider.NetworkCreated{}, provider.Refused("zone %s exists with DHCP %q, not %q; DHCP is a zone setting shared by every network in it, so it is not changed by a create", n.Zone, zone.DHCP, wantDHCP)
	case vnet != nil && vnet.Zone != n.Zone:
		return provider.NetworkCreated{}, provider.Refused("network %s exists in zone %s, not %s; moving it would re-plumb every machine on it", n.Name, vnet.Zone, n.Zone)
	}
	if vnet != nil {
		for i := range vnet.Subnets {
			sn := &vnet.Subnets[i]
			if sn.CIDR != n.Subnet {
				return provider.NetworkCreated{}, provider.Refused("network %s exists with subnet %s, not %s", n.Name, sn.CIDR, n.Subnet)
			}
			if sn.Gateway != n.Gateway || sn.SNAT != n.SNAT || sn.DHCPRange != n.DHCPRange {
				return provider.NetworkCreated{}, provider.Refused("network %s exists with subnet %s (gateway %s, outbound NAT %s, DHCP range %q), not as declared",
					n.Name, sn.CIDR, sn.Gateway, onOff(sn.SNAT), sn.DHCPRange)
			}
			subnet = sn
		}
	}

	// Edits that are not this network's would be published by the apply.
	ours := map[string]bool{"zone " + n.Zone: true, "vnet " + n.Name: true, "subnet " + n.Subnet + " on " + n.Name: true}
	var foreign []string
	for _, p := range st.Pending {
		obj, _, _ := strings.Cut(p, " (")
		if !ours[obj] {
			foreign = append(foreign, p)
		}
	}
	if len(foreign) > 0 {
		return provider.NetworkCreated{}, provider.Refused("the cluster has SDN edits that are not applied (%s); creating a network reloads the whole cluster's SDN and would publish them too, so apply or revert them first",
			strings.Join(foreign, ", "))
	}

	live := zone != nil && !zone.Pending && vnet != nil && !vnet.Pending && subnet != nil && !subnet.Pending && len(vnet.NotUp) == 0
	if live {
		return provider.NetworkCreated{}, nil
	}

	// A node's reload applies its own staged edits: refuse before sending
	// anything, not after the cluster SDN is already applied.
	nodes, err := s.onlineNodes(ctx)
	if err != nil {
		return provider.NetworkCreated{}, err
	}
	for _, node := range nodes {
		_, changes, err := s.c.CallEnvelope(ctx, http.MethodGet, fmt.Sprintf("/nodes/%s/network", url.PathEscape(node)), nil)
		if err != nil {
			return provider.NetworkCreated{}, err
		}
		if strings.TrimSpace(changes) != "" {
			return provider.NetworkCreated{}, provider.Refused("node %s has network changes of its own waiting to be applied; bringing network %s up there reloads the node's network and would apply them too, so apply or revert them on the node first", node, n.Name)
		}
	}

	out := provider.NetworkCreated{Applied: true}
	if zone == nil {
		form := url.Values{"zone": {n.Zone}, "type": {"simple"}, "ipam": {"pve"}}
		if wantDHCP != "" {
			form.Set("dhcp", wantDHCP)
		}
		if _, err := s.c.Call(ctx, http.MethodPost, "/cluster/sdn/zones", form); err != nil {
			return out, unknownAfter("create zone "+n.Zone, err)
		}
		out.Created = append(out.Created, "zone "+n.Zone)
	}
	if vnet == nil {
		form := url.Values{"vnet": {n.Name}, "zone": {n.Zone}, "alias": {"accrete"}}
		if _, err := s.c.Call(ctx, http.MethodPost, "/cluster/sdn/vnets", form); err != nil {
			return out, unknownAfter("create network "+n.Name, err)
		}
		out.Created = append(out.Created, "network "+n.Name)
	}
	if subnet == nil {
		form := url.Values{"type": {"subnet"}, "subnet": {n.Subnet}, "gateway": {n.Gateway}}
		if n.SNAT {
			form.Set("snat", "1")
		}
		if n.DHCPRange != "" {
			start, end, _ := strings.Cut(n.DHCPRange, "-")
			form.Set("dhcp-range", fmt.Sprintf("start-address=%s,end-address=%s", strings.TrimSpace(start), strings.TrimSpace(end)))
		}
		path := fmt.Sprintf("/cluster/sdn/vnets/%s/subnets", url.PathEscape(n.Name))
		if _, err := s.c.Call(ctx, http.MethodPost, path, form); err != nil {
			return out, unknownAfter("create subnet "+n.Subnet, err)
		}
		out.Created = append(out.Created, "subnet "+n.Subnet)
	}

	data, err := s.c.Call(ctx, http.MethodPut, "/cluster/sdn", nil)
	if err != nil {
		return out, unknownAfter("apply the cluster's SDN", err)
	}
	if err := s.c.WaitIfTask(ctx, "", data, NetworkWait); err != nil {
		return out, unknownAfter("apply the cluster's SDN", err)
	}
	// Found on flk-vega-01 (PVE 9.1.6): the cluster task committed the SDN
	// and every node reported the zone available, yet no node had a bridge
	// for the new vnet until its network was reloaded.
	for _, node := range nodes {
		data, err := s.c.Call(ctx, http.MethodPut, fmt.Sprintf("/nodes/%s/network", url.PathEscape(node)), nil)
		if err != nil {
			return out, unknownAfter("reload "+node+"'s network", err)
		}
		if err := s.c.WaitIfTask(ctx, node, data, NetworkWait); err != nil {
			return out, unknownAfter("reload "+node+"'s network", err)
		}
	}

	deadline := time.Now().Add(NetworkWait)
	for {
		status, err := s.c.VnetStatusOnNodes(ctx)
		if err != nil {
			return out, unknownAfter("check network "+n.Name+" is up", err)
		}
		var down []string
		for _, node := range status.Nodes {
			st, reported := status.Vnets[n.Name][node]
			if !reported {
				st = "not reported"
			}
			if st == "available" && status.BridgeMissing(node, n.Name) {
				st = "no bridge on the node"
			}
			if st != "available" {
				down = append(down, node+": "+st)
			}
		}
		if len(status.Nodes) > 0 && len(down) == 0 {
			return out, nil
		}
		if time.Now().After(deadline) {
			why := "no online node reports it"
			if len(down) > 0 {
				why = strings.Join(down, "; ")
			}
			msg := fmt.Sprintf("the SDN is applied, but network %s is not up: %s", n.Name, why)
			if wantDHCP != "" {
				msg += " (a zone with DHCP needs the dnsmasq package on every node)"
			}
			return out, fmt.Errorf("%s", msg)
		}
		if !SleepCtx(ctx, s.c.TaskPoll) {
			return out, &provider.OutcomeUnknownError{Op: "check network " + n.Name + " is up", Err: ctx.Err()}
		}
	}
}

// onlineNodes lists the nodes a network must be up on.
func (s *session) onlineNodes(ctx context.Context) ([]string, error) {
	var rows []nodeRow
	if err := s.c.Get(ctx, "/nodes", &rows); err != nil {
		return nil, err
	}
	var out []string
	for _, r := range rows {
		if r.Status == "" || r.Status == "online" {
			out = append(out, r.Node)
		}
	}
	return out, nil
}

// checkNetworkSpec refuses, before anything is read, a network Proxmox would
// refuse or half-make.
func checkNetworkSpec(n provider.NetworkSpec) error {
	if !sdnID.MatchString(n.Zone) {
		return provider.Refused("zone %q is not a zone id (a letter, then letters and digits, at most 8)", n.Zone)
	}
	if !sdnID.MatchString(n.Name) {
		return provider.Refused("network %q is not a network id (a letter, then letters and digits, at most 8)", n.Name)
	}
	_, cidr, err := net.ParseCIDR(n.Subnet)
	if err != nil || cidr.String() != n.Subnet {
		return provider.Refused("subnet %q is not a network address in CIDR form, e.g. 10.20.0.0/24", n.Subnet)
	}
	if gw := net.ParseIP(n.Gateway); gw == nil || !cidr.Contains(gw) {
		return provider.Refused("gateway %q is not an address in %s", n.Gateway, n.Subnet)
	}
	if n.DHCPRange != "" {
		start, end, ok := strings.Cut(n.DHCPRange, "-")
		a, b := net.ParseIP(strings.TrimSpace(start)), net.ParseIP(strings.TrimSpace(end))
		if !ok || a == nil || b == nil || !cidr.Contains(a) || !cidr.Contains(b) {
			return provider.Refused("DHCP range %q is not START-END inside %s", n.DHCPRange, n.Subnet)
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
