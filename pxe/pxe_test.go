package pxe

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func gate() Gate {
	return Gate{
		Kickstart: "http://media.lan/accrete/ks.cfg",
		Media: []Media{
			{Name: "rhel-9.6", Kernel: "http://media.lan/rhel96/images/pxeboot/vmlinuz", Initrd: "http://media.lan/rhel96/images/pxeboot/initrd.img", Repo: "http://media.lan/rhel96"},
			{Name: "rhel-10.0", Kernel: "http://media.lan/rhel10/images/pxeboot/vmlinuz", Initrd: "http://media.lan/rhel10/images/pxeboot/initrd.img", Repo: "http://media.lan/rhel10", Args: []string{"console=ttyS0,115200"}},
		},
		Machines: []Machine{
			{Name: "web-01", Serials: []string{"CZJ1234ABC"}, Media: "rhel-9.6"},
			{Name: "db-01", UUIDs: []string{"4c4c4544-0042-3510-8051-b4c04f4e4d32"}, Media: "rhel-10.0"},
			{Name: "pc-01", MACs: []string{"52:54:00:AA:00:09"}, Media: "rhel-9.6"},
		},
	}
}

func golden(t *testing.T, name string, got []byte) {
	t.Helper()
	path := filepath.Join("testdata", name)
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%v (run with UPDATE_GOLDEN=1 to write it)", err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("%s differs; run with UPDATE_GOLDEN=1 and review the diff", path)
	}
}

func TestScriptGolden(t *testing.T) {
	s, err := Script(gate())
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "gate.ipxe", s)
	// Only media an armed machine uses is written.
	g := gate()
	g.Machines = g.Machines[:1]
	s, _ = Script(g)
	if strings.Contains(string(s), "rhel10") {
		t.Fatal("unused media written into the gate")
	}
}

func TestNothingArmedBootsTheDisk(t *testing.T) {
	g := gate()
	g.Machines = nil
	s, err := Script(g)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(s), "kernel ") || !strings.Contains(string(s), "Armed for an install: none") {
		t.Fatalf("an empty gate can boot an installer:\n%s", s)
	}
}

func TestScriptRefuses(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(*Gate)
		mention string
	}{
		{"kickstart not http", func(g *Gate) { g.Kickstart = "tftp://x/ks.cfg" }, "kickstart"},
		{"credential in URL", func(g *Gate) { g.Media[0].Repo = "http://u:p@media.lan/r" }, "credential"},
		{"iPXE expansion in URL", func(g *Gate) { g.Media[0].Kernel = "http://media.lan/${x}/vmlinuz" }, "interpret"},
		{"argument with a space", func(g *Gate) { g.Media[0].Args = []string{"a b"} }, `"a b"`},
		{"unknown media", func(g *Gate) { g.Machines[0].Media = "debian" }, `"debian"`},
		{"serial with a space", func(g *Gate) { g.Machines[0].Serials = []string{"CZJ 1234"} }, "CZJ 1234"},
		{"placeholder serial", func(g *Gate) { g.Machines[0].Serials = []string{"Default string"} }, "placeholder"},
		{"no selector", func(g *Gate) { g.Machines[0].Serials = nil }, "no UUID, serial or MAC"},
		{"shared MAC", func(g *Gate) { g.Machines[0].MACs = []string{"52:54:00:aa:00:09"} }, `"pc-01"`},
		{"duplicate media", func(g *Gate) { g.Media = append(g.Media, g.Media[0]) }, "two media"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			g := gate()
			c.edit(&g)
			_, err := Script(g)
			if err == nil || !strings.Contains(err.Error(), c.mention) {
				t.Fatalf("%v; want a refusal mentioning %q", err, c.mention)
			}
		})
	}
}

func TestDnsmasqProxy(t *testing.T) {
	conf, err := DnsmasqProxy(Proxy{Subnet: "192.168.1.0/24", TFTPRoot: "/srv/tftp", Script: "http://192.168.1.45:8088/gate.ipxe"})
	if err != nil {
		t.Fatal(err)
	}
	golden(t, "dnsmasq-proxy.conf", conf)
	s := string(conf)
	// The whole point: the network's DHCP server keeps assigning addresses.
	if !strings.Contains(s, "dhcp-range=192.168.1.0,proxy,255.255.255.0\n") || !strings.Contains(s, "port=0\n") {
		t.Fatalf("not a proxy-only configuration:\n%s", s)
	}
	for _, bad := range []Proxy{
		{Subnet: "192.168.1.0", TFTPRoot: "/srv/tftp", Script: "http://h/g"},
		{Subnet: "fd00::/64", TFTPRoot: "/srv/tftp", Script: "http://h/g"},
		{Subnet: "192.168.1.0/24", TFTPRoot: "srv", Script: "http://h/g"},
		{Subnet: "192.168.1.0/24", TFTPRoot: "/srv/tftp", Script: "/gate.ipxe"},
	} {
		if _, err := DnsmasqProxy(bad); err == nil {
			t.Errorf("%+v accepted", bad)
		}
	}
}
