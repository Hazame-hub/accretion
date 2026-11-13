package pvetest

import (
	"encoding/json"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
)

// The cluster's SDN, as Accretion's network writes leave it: a create
// defines an object as pending ("state": "new"), PUT /cluster/sdn applies
// every pending object, and a vnet's bridge is up on pve1 only once the
// node's network is reloaded (PUT /nodes/pve1/network), as found on
// flk-vega-01.

// Zone is one SDN zone.
type Zone struct {
	Type    string
	DHCP    string
	Pending bool
}

// Vnet is one SDN vnet and its subnets, by CIDR.
type Vnet struct {
	Zone    string
	Pending bool
	// Up is true once a node reload brought its bridge up.
	Up      bool
	Subnets map[string]*Subnet
}

// Subnet is one subnet of a vnet. DHCPRange is START-END.
type Subnet struct {
	Gateway   string
	SNAT      bool
	DHCPRange string
	Pending   bool
}

var (
	sdnSubnets  = regexp.MustCompile(`^/cluster/sdn/vnets/([^/]+)/subnets$`)
	nodeZoneSub = regexp.MustCompile(`^/nodes/pve1/sdn/zones/([^/]+)/(bridges|content)$`)
)

func state(pending bool) string {
	if pending {
		return "new"
	}
	return ""
}

// sdn answers the SDN calls; false when p is not one.
func (s *Server) sdn(w http.ResponseWriter, method, p string, form url.Values, data func(any), fail func(int, string), task func()) bool {
	if s.Zones == nil {
		s.Zones = map[string]*Zone{}
	}
	if s.Networks == nil {
		s.Networks = map[string]*Vnet{}
	}
	for name, zone := range s.Vnets {
		if s.Networks[name] == nil {
			s.Networks[name] = &Vnet{Zone: zone, Up: true, Subnets: map[string]*Subnet{}}
		}
	}
	switch {
	case method == http.MethodGet && p == "/cluster/sdn/zones":
		rows := []map[string]any{}
		for _, name := range keys(s.Zones) {
			z := s.Zones[name]
			rows = append(rows, map[string]any{"zone": name, "type": z.Type, "dhcp": z.DHCP, "state": state(z.Pending)})
		}
		data(rows)
	case method == http.MethodPost && p == "/cluster/sdn/zones":
		name := form.Get("zone")
		if s.Zones[name] != nil {
			fail(http.StatusBadRequest, "zone "+name+" already exists")
			break
		}
		s.Zones[name] = &Zone{Type: form.Get("type"), DHCP: form.Get("dhcp"), Pending: true}
		data(nil)
	case method == http.MethodGet && p == "/cluster/sdn/vnets":
		rows := []map[string]any{}
		for _, name := range keys(s.Networks) {
			v := s.Networks[name]
			rows = append(rows, map[string]any{"vnet": name, "zone": v.Zone, "state": state(v.Pending)})
		}
		data(rows)
	case method == http.MethodPost && p == "/cluster/sdn/vnets":
		name := form.Get("vnet")
		if s.Networks[name] != nil {
			fail(http.StatusBadRequest, "vnet "+name+" already exists")
			break
		}
		if s.Zones[form.Get("zone")] == nil {
			fail(http.StatusBadRequest, "zone "+form.Get("zone")+" does not exist")
			break
		}
		s.Networks[name] = &Vnet{Zone: form.Get("zone"), Pending: true, Subnets: map[string]*Subnet{}}
		data(nil)
	case sdnSubnets.MatchString(p):
		v := s.Networks[sdnSubnets.FindStringSubmatch(p)[1]]
		if v == nil {
			fail(http.StatusNotFound, "no such vnet")
			break
		}
		if method == http.MethodGet {
			rows := []map[string]any{}
			for _, cidr := range keys(v.Subnets) {
				sn := v.Subnets[cidr]
				row := map[string]any{"cidr": cidr, "gateway": sn.Gateway, "state": state(sn.Pending)}
				if sn.SNAT {
					row["snat"] = 1
				}
				if start, end, ok := strings.Cut(sn.DHCPRange, "-"); ok {
					row["dhcp-range"] = []map[string]string{{"start-address": start, "end-address": end}}
				}
				rows = append(rows, row)
			}
			data(rows)
			break
		}
		cidr := form.Get("subnet")
		if v.Subnets[cidr] != nil {
			fail(http.StatusBadRequest, "subnet "+cidr+" already exists")
			break
		}
		sn := &Subnet{Gateway: form.Get("gateway"), SNAT: form.Get("snat") == "1", Pending: true}
		if r := form.Get("dhcp-range"); r != "" {
			var start, end string
			for _, part := range strings.Split(r, ",") {
				if x, ok := strings.CutPrefix(part, "start-address="); ok {
					start = x
				}
				if x, ok := strings.CutPrefix(part, "end-address="); ok {
					end = x
				}
			}
			sn.DHCPRange = start + "-" + end
		}
		v.Subnets[cidr] = sn
		data(nil)
	case method == http.MethodPut && p == "/cluster/sdn":
		for _, z := range s.Zones {
			z.Pending = false
		}
		for _, v := range s.Networks {
			v.Pending = false
			for _, sn := range v.Subnets {
				sn.Pending = false
			}
		}
		task()
	case method == http.MethodGet && p == "/nodes/pve1/network":
		_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}, "changes": s.NodeNetworkChanges})
	case method == http.MethodPut && p == "/nodes/pve1/network":
		s.NodeNetworkChanges = ""
		for _, v := range s.Networks {
			if !v.Pending && !s.BridgeNeverUp {
				v.Up = true
			}
		}
		task()
	case method == http.MethodGet && p == "/nodes/pve1/sdn/zones":
		rows := []map[string]any{}
		for _, name := range keys(s.Zones) {
			if !s.Zones[name].Pending {
				rows = append(rows, map[string]any{"zone": name, "status": "available"})
			}
		}
		data(rows)
	case method == http.MethodGet && nodeZoneSub.MatchString(p):
		m := nodeZoneSub.FindStringSubmatch(p)
		rows := []map[string]any{}
		for _, name := range keys(s.Networks) {
			v := s.Networks[name]
			if v.Zone != m[1] || v.Pending {
				continue
			}
			switch {
			case m[2] == "bridges" && v.Up:
				rows = append(rows, map[string]any{"name": name})
			case m[2] == "content":
				st := "available"
				if !v.Up {
					st = "pending"
				}
				rows = append(rows, map[string]any{"vnet": name, "status": st})
			}
		}
		data(rows)
	default:
		return false
	}
	return true
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
