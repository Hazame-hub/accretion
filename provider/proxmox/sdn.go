package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/hazame-hub/accretion/plan"
)

// The SDN as a plan reads it: zones, vnets with their subnets, what is
// defined but not applied yet, and on which nodes each vnet is not up.

// SDNState is the cluster's SDN, applied and pending.
type SDNState struct {
	Zones []SDNZone
	Vnets []SDNVnet
	// Pending names every SDN object that is defined but not applied, sorted.
	Pending []string
}

// SDNZone is one zone. DHCP is "dnsmasq", or "" for none.
type SDNZone struct {
	Zone    string
	Type    string
	DHCP    string
	Pending bool
}

// SDNVnet is one vnet. NotUp names each online node where it is not up,
// as "node: status", sorted.
type SDNVnet struct {
	Vnet    string
	Zone    string
	Pending bool
	Subnets []SDNSubnet
	NotUp   []string
}

// SDNSubnet is one subnet of a vnet. DHCPRange is START-END, empty for none.
type SDNSubnet struct {
	CIDR      string
	Gateway   string
	SNAT      bool
	DHCPRange string
	Pending   bool
}

// SDN reads the cluster's SDN.
//
// Proxmox filters every SDN listing by SDN.Audit and answers a token without
// it with an empty list, not a refusal — so "no networks" and "not allowed to
// look" read the same. The second must never reach a plan as the first: that
// is the answer that would launch a create of a network that may already
// exist. So the privilege is checked first, and its absence is an error.
func (c *Client) SDN(ctx context.Context) (SDNState, error) {
	if err := c.RequireSDNAudit(ctx); err != nil {
		return SDNState{}, err
	}
	type pendingFields struct {
		Zone    string `json:"zone"`
		Type    string `json:"type"`
		Gateway string `json:"gateway"`
		SNAT    *int   `json:"snat"`
	}
	var zones []struct {
		Zone    string         `json:"zone"`
		Type    string         `json:"type"`
		DHCP    string         `json:"dhcp"`
		State   string         `json:"state"`
		Pending *pendingFields `json:"pending"`
	}
	if err := c.Get(ctx, "/cluster/sdn/zones?pending=1", &zones); err != nil {
		return SDNState{}, err
	}
	var vnets []struct {
		Vnet    string         `json:"vnet"`
		Zone    string         `json:"zone"`
		State   string         `json:"state"`
		Pending *pendingFields `json:"pending"`
	}
	if err := c.Get(ctx, "/cluster/sdn/vnets?pending=1", &vnets); err != nil {
		return SDNState{}, err
	}

	nodeStatus, err := c.VnetStatusOnNodes(ctx)
	if err != nil {
		return SDNState{}, err
	}

	out := SDNState{Zones: make([]SDNZone, 0, len(zones)), Vnets: make([]SDNVnet, 0, len(vnets)), Pending: []string{}}
	for _, z := range zones {
		kind := z.Type
		if kind == "" && z.Pending != nil {
			kind = z.Pending.Type
		}
		if z.State != "" {
			out.Pending = append(out.Pending, fmt.Sprintf("zone %s (%s)", z.Zone, z.State))
		}
		out.Zones = append(out.Zones, SDNZone{Zone: z.Zone, Type: kind, DHCP: z.DHCP, Pending: z.State != ""})
	}

	for _, v := range vnets {
		zone := v.Zone
		if zone == "" && v.Pending != nil {
			zone = v.Pending.Zone
		}
		if v.State != "" {
			out.Pending = append(out.Pending, fmt.Sprintf("vnet %s (%s)", v.Vnet, v.State))
		}
		var subnets []struct {
			CIDR      string          `json:"cidr"`
			Subnet    string          `json:"subnet"`
			Gateway   string          `json:"gateway"`
			SNAT      *int            `json:"snat"`
			DHCPRange json.RawMessage `json:"dhcp-range"`
			State     string          `json:"state"`
			Pending   *pendingFields  `json:"pending"`
		}
		path := fmt.Sprintf("/cluster/sdn/vnets/%s/subnets?pending=1", url.PathEscape(v.Vnet))
		if err := c.Get(ctx, path, &subnets); err != nil {
			return SDNState{}, err
		}
		ov := SDNVnet{Vnet: v.Vnet, Zone: zone, Pending: v.State != "", Subnets: make([]SDNSubnet, 0, len(subnets))}
		for _, s := range subnets {
			cidr := s.CIDR
			if cidr == "" {
				cidr = s.Subnet
			}
			gw, snat := s.Gateway, s.SNAT
			if s.Pending != nil {
				if gw == "" {
					gw = s.Pending.Gateway
				}
				if snat == nil {
					snat = s.Pending.SNAT
				}
			}
			if s.State != "" {
				out.Pending = append(out.Pending, fmt.Sprintf("subnet %s on %s (%s)", cidr, v.Vnet, s.State))
			}
			ov.Subnets = append(ov.Subnets, SDNSubnet{
				CIDR: cidr, Gateway: gw, SNAT: snat != nil && *snat != 0, Pending: s.State != "",
				DHCPRange: DHCPRange(s.DHCPRange),
			})
		}
		// Applied in the cluster configuration is not up on a node: a reload
		// can fail on one node and succeed on the others.
		ov.NotUp = []string{}
		for _, node := range nodeStatus.Nodes {
			st, reported := nodeStatus.Vnets[v.Vnet][node]
			if !reported {
				// Absent is not up: a node that has not generated the vnet
				// has no bridge for it.
				st = "not reported"
			}
			if st == "available" && nodeStatus.BridgeMissing(node, v.Vnet) {
				st = "no bridge on the node"
			}
			if st != "available" {
				ov.NotUp = append(ov.NotUp, node+": "+st)
			}
		}
		sort.Strings(ov.NotUp)
		out.Vnets = append(out.Vnets, ov)
	}
	sort.Strings(out.Pending)
	return out, nil
}

// Observed is the SDN as the planner reads it.
func (s SDNState) Observed() plan.ObservedSDN {
	out := plan.ObservedSDN{Vnets: map[string]plan.ObservedVnet{}, ZoneDHCP: map[string]string{}, Pending: s.Pending}
	for _, z := range s.Zones {
		out.ZoneDHCP[z.Zone] = z.DHCP
	}
	for _, v := range s.Vnets {
		ov := plan.ObservedVnet{Zone: v.Zone, Pending: v.Pending, NotUp: v.NotUp}
		for _, sub := range v.Subnets {
			ov.Subnets = append(ov.Subnets, plan.ObservedSubnet{
				CIDR: sub.CIDR, Gateway: sub.Gateway, SNAT: sub.SNAT, DHCPRange: sub.DHCPRange, Pending: sub.Pending,
			})
		}
		out.Vnets[v.Vnet] = ov
	}
	return out
}

// RequireSDNAudit fails unless the token may read the SDN.
func (c *Client) RequireSDNAudit(ctx context.Context) error {
	var perms map[string]map[string]interface{}
	if err := c.Get(ctx, "/access/permissions?path=/sdn", &perms); err != nil {
		return fmt.Errorf("proxmox: could not check whether this token may read the SDN: %w", err)
	}
	for _, privs := range perms {
		if v, ok := privs["SDN.Audit"]; ok && v != nil && fmt.Sprint(v) != "0" {
			return nil
		}
	}
	return fmt.Errorf("proxmox: this token cannot see the SDN — Proxmox answers it with an empty list rather than a " +
		"refusal, so Sybil cannot tell \"no networks\" from \"not allowed to look\". Grant SDN.Audit on /sdn " +
		"(and SDN.Allocate to create networks, SDN.Use to attach machines to them)")
}

// DHCPRange reads a subnet's dhcp-range back as START-END, the shape a subnet
// is created with. Proxmox returns it as a list, of objects or of
// "start-address=…,end-address=…" strings depending on version; only the
// first range is reported. Empty when the subnet has none.
func DHCPRange(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var objs []map[string]string
	if json.Unmarshal(raw, &objs) == nil && len(objs) > 0 {
		return objs[0]["start-address"] + "-" + objs[0]["end-address"]
	}
	var strs []string
	if json.Unmarshal(raw, &strs) == nil && len(strs) > 0 {
		var start, end string
		for _, part := range strings.Split(strs[0], ",") {
			if v, ok := strings.CutPrefix(part, "start-address="); ok {
				start = v
			}
			if v, ok := strings.CutPrefix(part, "end-address="); ok {
				end = v
			}
		}
		if start != "" && end != "" {
			return start + "-" + end
		}
	}
	return ""
}

// VnetStatus is vnet → node → status, plus the online nodes asked.
type VnetStatus struct {
	Nodes []string
	Vnets map[string]map[string]string
	// bridges is node → the bridges its zones report, for the nodes whose
	// Proxmox serves /nodes/<n>/sdn/zones/<z>/bridges (PVE 8.4 and later).
	// A node missing here could not be asked, which is not the same as a
	// node with no bridges.
	bridges map[string]map[string]bool
}

// BridgeMissing reports whether a node that could be asked has no bridge for
// vnet. False when the node could not be asked.
func (v VnetStatus) BridgeMissing(node, vnet string) bool {
	b, asked := v.bridges[node]
	return asked && !b[vnet]
}

// VnetStatusOnNodes reads, for every online node, the status each vnet has
// there: "available" when its bridge is up, otherwise what the node reports
// ("error", "pending", ...) with its message. It is the difference between
// the SDN configuration saying a network exists and a machine being able to
// plug into it — found on flk-vega-01, where an applied vnet had no bridge on
// the node and vm_start refused the NIC.
func (c *Client) VnetStatusOnNodes(ctx context.Context) (VnetStatus, error) {
	var nodes []nodeRow
	if err := c.Get(ctx, "/nodes", &nodes); err != nil {
		return VnetStatus{}, err
	}
	res := VnetStatus{Vnets: map[string]map[string]string{}, bridges: map[string]map[string]bool{}}
	out := res.Vnets
	for _, n := range nodes {
		if n.Status != "" && n.Status != "online" {
			continue
		}
		res.Nodes = append(res.Nodes, n.Node)
		var zones []struct {
			Zone   string `json:"zone"`
			Status string `json:"status"`
		}
		if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/sdn/zones", url.PathEscape(n.Node)), &zones); err != nil {
			return VnetStatus{}, err
		}
		bridgesKnown := true
		nodeBridges := map[string]bool{}
		for _, z := range zones {
			var bridges []struct {
				Name string `json:"name"`
			}
			if err := c.Get(ctx, fmt.Sprintf("/nodes/%s/sdn/zones/%s/bridges", url.PathEscape(n.Node), url.PathEscape(z.Zone)), &bridges); err != nil {
				bridgesKnown = false
			}
			for _, b := range bridges {
				nodeBridges[b.Name] = true
			}
			var content []struct {
				Vnet      string `json:"vnet"`
				Status    string `json:"status"`
				StatusMsg string `json:"statusmsg"`
			}
			path := fmt.Sprintf("/nodes/%s/sdn/zones/%s/content", url.PathEscape(n.Node), url.PathEscape(z.Zone))
			if err := c.Get(ctx, path, &content); err != nil {
				return VnetStatus{}, err
			}
			for _, item := range content {
				st := item.Status
				if st == "" {
					st = z.Status
				}
				if item.StatusMsg != "" && st != "available" {
					st += " (" + item.StatusMsg + ")"
				}
				if out[item.Vnet] == nil {
					out[item.Vnet] = map[string]string{}
				}
				out[item.Vnet][n.Node] = st
			}
		}
		if bridgesKnown && len(zones) > 0 {
			res.bridges[n.Node] = nodeBridges
		}
	}
	sort.Strings(res.Nodes)
	return res, nil
}
