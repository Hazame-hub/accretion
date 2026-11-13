package proxmox

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/hazame-hub/accretion/provider"
	"github.com/hazame-hub/accretion/provider/proxmox/pvetest"
)

func networkWriterOn(t *testing.T, pve *pvetest.Server) provider.NetworkWriter {
	t.Helper()
	w, ok := writerOn(t, pve).(provider.NetworkWriter)
	if !ok {
		t.Fatal("a Proxmox session must create networks")
	}
	return w
}

var labNet = provider.NetworkSpec{Zone: "accrete", Name: "web", Subnet: "10.30.0.0/24", Gateway: "10.30.0.1", SNAT: true, DHCPRange: "10.30.0.100-10.30.0.199"}

func TestCreateNetworkDefinesAppliesAndBringsItUp(t *testing.T) {
	pve := pvetest.New(t)
	w := networkWriterOn(t, pve)
	res, err := w.CreateNetwork(context.Background(), labNet)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Applied || strings.Join(res.Created, ", ") != "zone accrete, network web, subnet 10.30.0.0/24" {
		t.Errorf("result = %+v", res)
	}
	z, v := pve.Zones["accrete"], pve.Networks["web"]
	if z == nil || z.Type != "simple" || z.DHCP != "dnsmasq" || z.Pending {
		t.Errorf("zone = %+v", z)
	}
	if v == nil || v.Zone != "accrete" || v.Pending || !v.Up {
		t.Fatalf("vnet = %+v", v)
	}
	if sn := v.Subnets["10.30.0.0/24"]; sn == nil || sn.Gateway != "10.30.0.1" || !sn.SNAT || sn.DHCPRange != "10.30.0.100-10.30.0.199" {
		t.Errorf("subnet = %+v", sn)
	}
	// The node is reloaded after the cluster apply, never before.
	writes := strings.Join(pve.Writes(), "\n")
	if i, j := strings.Index(writes, "PUT /cluster/sdn"), strings.Index(writes, "PUT /nodes/pve1/network"); i < 0 || j < i {
		t.Errorf("writes:\n%s", writes)
	}

	// A repeat finds it live and sends nothing.
	before := len(pve.Writes())
	res, err = w.CreateNetwork(context.Background(), labNet)
	if err != nil || res.Applied || len(pve.Writes()) != before {
		t.Errorf("repeat: %+v %v, writes %v", res, err, pve.Writes()[before:])
	}
}

func TestCreateNetworkFinishesAHalfDoneOne(t *testing.T) {
	pve := pvetest.New(t)
	pve.Zones = map[string]*pvetest.Zone{"accrete": {Type: "simple", DHCP: "dnsmasq"}}
	pve.Networks = map[string]*pvetest.Vnet{"web": {Zone: "accrete", Pending: true, Subnets: map[string]*pvetest.Subnet{}}}
	res, err := networkWriterOn(t, pve).CreateNetwork(context.Background(), labNet)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(res.Created, ", ") != "subnet 10.30.0.0/24" || !pve.Networks["web"].Up {
		t.Errorf("result = %+v, vnet = %+v", res, pve.Networks["web"])
	}
}

func TestCreateNetworkRefusesBeforeSendingAnything(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*pvetest.Server)
		spec  provider.NetworkSpec
		want  string
	}{
		{"zone serves no DHCP", func(s *pvetest.Server) { s.Zones = map[string]*pvetest.Zone{"accrete": {Type: "simple"}} }, labNet, "DHCP is a zone setting"},
		{"zone of another type", func(s *pvetest.Server) {
			s.Zones = map[string]*pvetest.Zone{"accrete": {Type: "vlan", DHCP: "dnsmasq"}}
		}, labNet, "simple zones only"},
		{"network in another zone", func(s *pvetest.Server) {
			s.Zones = map[string]*pvetest.Zone{"other": {Type: "simple"}}
			s.Networks = map[string]*pvetest.Vnet{"web": {Zone: "other", Up: true, Subnets: map[string]*pvetest.Subnet{}}}
		}, labNet, "re-plumb"},
		{"another subnet", func(s *pvetest.Server) {
			s.Zones = map[string]*pvetest.Zone{"accrete": {Type: "simple", DHCP: "dnsmasq"}}
			s.Networks = map[string]*pvetest.Vnet{"web": {Zone: "accrete", Up: true, Subnets: map[string]*pvetest.Subnet{"10.99.0.0/24": {Gateway: "10.99.0.1"}}}}
		}, labNet, "not 10.30.0.0/24"},
		{"somebody's pending SDN edit", func(s *pvetest.Server) { s.Zones = map[string]*pvetest.Zone{"theirs": {Type: "simple", Pending: true}} }, labNet, "would publish them too"},
		{"node network edits staged", func(s *pvetest.Server) { s.NodeNetworkChanges = "--- a\n+++ b\n" }, labNet, "node pve1 has network changes"},
		{"bad id", nil, provider.NetworkSpec{Zone: "accrete", Name: "web-net-1", Subnet: "10.30.0.0/24", Gateway: "10.30.0.1"}, "not a network id"},
		{"gateway outside", nil, provider.NetworkSpec{Zone: "accrete", Name: "web", Subnet: "10.30.0.0/24", Gateway: "10.31.0.1"}, "not an address in"},
		{"range outside", nil, provider.NetworkSpec{Zone: "accrete", Name: "web", Subnet: "10.30.0.0/24", Gateway: "10.30.0.1", DHCPRange: "10.30.0.10-10.31.0.20"}, "not START-END inside"},
		{"subnet not a network address", nil, provider.NetworkSpec{Zone: "accrete", Name: "web", Subnet: "10.30.0.5/24", Gateway: "10.30.0.1"}, "CIDR form"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pve := pvetest.New(t)
			if c.setup != nil {
				c.setup(pve)
			}
			_, err := networkWriterOn(t, pve).CreateNetwork(context.Background(), c.spec)
			var refused *provider.RefusedError
			if !errors.As(err, &refused) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("want a refusal saying %q, got %v", c.want, err)
			}
			if w := pve.Writes(); len(w) > 0 {
				t.Errorf("a refusal sent %v", w)
			}
		})
	}
}

func TestCreateNetworkSaysWhenTheBridgeNeverComesUp(t *testing.T) {
	old := NetworkWait
	NetworkWait = 200 * time.Millisecond
	defer func() { NetworkWait = old }()
	pve := pvetest.New(t)
	pve.BridgeNeverUp = true
	_, err := networkWriterOn(t, pve).CreateNetwork(context.Background(), labNet)
	if err == nil || !strings.Contains(err.Error(), "not up: pve1") || !strings.Contains(err.Error(), "dnsmasq") {
		t.Fatalf("got %v", err)
	}
	// A rerun once the cause is fixed only reloads and checks again.
	pve.BridgeNeverUp = false
	res, err := networkWriterOn(t, pve).CreateNetwork(context.Background(), labNet)
	if err != nil || !res.Applied || len(res.Created) != 0 || !pve.Networks["web"].Up {
		t.Errorf("rerun: %+v %v", res, err)
	}
}

func TestCreateNetworkLostAnswerIsUnknown(t *testing.T) {
	pve := pvetest.New(t)
	pve.DropNext(`^POST /cluster/sdn/vnets$`)
	_, err := networkWriterOn(t, pve).CreateNetwork(context.Background(), labNet)
	var unknown *provider.OutcomeUnknownError
	if !errors.As(err, &unknown) {
		t.Fatalf("want an unknown outcome, got %v", err)
	}
	// Retrying the same call finishes the network.
	res, err := networkWriterOn(t, pve).CreateNetwork(context.Background(), labNet)
	if err != nil || strings.Join(res.Created, ", ") != "subnet 10.30.0.0/24" || !pve.Networks["web"].Up {
		t.Errorf("retry: %+v %v", res, err)
	}
}
