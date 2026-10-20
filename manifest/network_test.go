package manifest

import (
	"net/netip"
	"reflect"
	"strings"
	"testing"
)

func decodeOne(t *testing.T, yaml string) (Environment, error) {
	t.Helper()
	docs, err := DecodeDocuments(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("bundle: %v", err)
	}
	return DecodeEnvironment(docs[0])
}

const netManifest = `apiVersion: sybil/v1alpha1
kind: Environment
metadata:
  name: okd
spec:
  integration: lab
  networks:
    - name: okd
      subnet: 10.20.0.0/24
  resources:
    - name: node
      count: 3
      blueprint: provision-proxmox-vm
      network: okd
      inputs:
        hostname: okd-{{instance.number}}
`

func TestANetworkTakesItsDefaults(t *testing.T) {
	env, err := decodeOne(t, netManifest)
	if err != nil {
		t.Fatal(err)
	}
	n := env.Spec.Networks[0]
	if n.Zone != DefaultNetworkZone || n.Gateway != "10.20.0.1" || !n.SNAT || n.DHCPRange != "" {
		t.Fatalf("network = %+v", n)
	}
	if env.Spec.Resources[0].Network != "okd" {
		t.Fatalf("resource network = %q", env.Spec.Resources[0].Network)
	}
}

func TestANetworkRoundTrips(t *testing.T) {
	env, err := decodeOne(t, netManifest)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := EncodeEnvironment(env)
	if err != nil {
		t.Fatal(err)
	}
	again, err := DecodeEnvironment(doc)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(again.Spec.Networks[0], env.Spec.Networks[0]) || again.Spec.Resources[0].Network != "okd" {
		t.Fatalf("round trip changed it: %+v", again.Spec)
	}
}

func TestBadNetworksAreRefused(t *testing.T) {
	cases := map[string]string{
		"host bits":       "subnet: 10.20.0.7/24",
		"not a cidr":      "subnet: 10.20.0.0",
		"gateway outside": "subnet: 10.20.0.0/24\n      gateway: 10.30.0.1",
		"gateway is net":  "subnet: 10.20.0.0/24\n      gateway: 10.20.0.0",
		"long name":       "subnet: 10.20.0.0/24\n      zone: muchtoolongzone",
	}
	for name, body := range cases {
		m := strings.Replace(netManifest, "subnet: 10.20.0.0/24", body, 1)
		if _, err := decodeOne(t, m); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	bad := strings.Replace(netManifest, "- name: okd\n      subnet", "- name: OKD-net\n      subnet", 1)
	if _, err := decodeOne(t, bad); err == nil {
		t.Error("accepted a name Proxmox cannot use as an interface")
	}
}

func TestNetworkAndNICInputTogetherAreRefused(t *testing.T) {
	m := strings.Replace(netManifest, "hostname: okd-{{instance.number}}", "hostname: okd-{{instance.number}}\n        net0: virtio,bridge=vmbr0", 1)
	if _, err := decodeOne(t, m); err == nil || !strings.Contains(err.Error(), "net0") {
		t.Fatalf("err = %v", err)
	}
}

func TestANetworkOnlyManifestDecodes(t *testing.T) {
	m := netManifest[:strings.Index(netManifest, "  resources:")]
	env, err := decodeOne(t, m)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Spec.Resources) != 0 || len(env.Spec.Networks) != 1 {
		t.Fatalf("spec = %+v", env.Spec)
	}
}

func TestDuplicateNetworksAreRefused(t *testing.T) {
	m := strings.Replace(netManifest, "  resources:", "    - name: okd\n      subnet: 10.30.0.0/24\n  resources:", 1)
	if _, err := decodeOne(t, m); err == nil {
		t.Fatal("accepted two networks with one name")
	}
}

// DHCP is off unless asked for: a zone with dnsmasq DHCP on a node without the
// package breaks every SDN reload on the cluster (found on flk-vega-01).
func TestDHCPIsOptIn(t *testing.T) {
	on, err := decodeOne(t, strings.Replace(netManifest, "subnet: 10.20.0.0/24", "subnet: 10.20.0.0/24\n      dhcp: true", 1))
	if err != nil || on.Spec.Networks[0].DHCPRange != "10.20.0.128-10.20.0.254" {
		t.Fatalf("dhcp on: %+v, %v", on.Spec.Networks, err)
	}
	narrow, err := decodeOne(t, strings.Replace(netManifest, "subnet: 10.20.0.0/24", "subnet: 10.20.0.0/24\n      dhcp: true\n      dhcp_range: 10.20.0.100-10.20.0.120", 1))
	if err != nil || narrow.Spec.Networks[0].DHCPRange != "10.20.0.100-10.20.0.120" {
		t.Fatalf("narrow: %+v, %v", narrow.Spec.Networks, err)
	}
	// A gateway in the upper half shortens the default range instead of failing it.
	high, err := decodeOne(t, strings.Replace(netManifest, "subnet: 10.20.0.0/24", "subnet: 10.20.0.0/24\n      gateway: 10.20.0.254\n      dhcp: true", 1))
	if err != nil || high.Spec.Networks[0].DHCPRange != "10.20.0.128-10.20.0.253" {
		t.Fatalf("high gateway: %+v, %v", high.Spec.Networks, err)
	}
	for name, body := range map[string]string{
		"range without dhcp": "dhcp_range: 10.20.0.100-10.20.0.120",
		"outside":            "dhcp: true\n      dhcp_range: 10.30.0.1-10.30.0.9",
		"backwards":          "dhcp: true\n      dhcp_range: 10.20.0.9-10.20.0.2",
		"gateway":            "dhcp: true\n      dhcp_range: 10.20.0.1-10.20.0.9",
		"broadcast gateway":  "gateway: 10.20.0.255",
	} {
		if _, err := decodeOne(t, strings.Replace(netManifest, "subnet: 10.20.0.0/24", "subnet: 10.20.0.0/24\n      "+body, 1)); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	tiny := strings.Replace(netManifest, "subnet: 10.20.0.0/24", "subnet: 10.20.0.0/30\n      dhcp: true", 1)
	if _, err := decodeOne(t, tiny); err == nil {
		t.Error("turned DHCP on for a /30 with no range it could serve")
	}
}

func TestNetworksInOneZoneMustAgreeOnDHCP(t *testing.T) {
	m := strings.Replace(netManifest, "  resources:", "    - name: other\n      subnet: 10.30.0.0/24\n      dhcp: true\n  resources:", 1)
	if _, err := decodeOne(t, m); err == nil || !strings.Contains(err.Error(), "zone") {
		t.Fatalf("err = %v", err)
	}
}

func TestDefaultDHCPRangeIsTheUpperHalf(t *testing.T) {
	cases := map[string]string{"10.20.0.0/24": "10.20.0.128-10.20.0.254", "10.0.0.0/16": "10.0.128.0-10.0.255.254",
		"192.168.5.0/29": "192.168.5.4-192.168.5.6", "192.168.5.0/30": ""}
	for cidr, want := range cases {
		if got := defaultDHCPRange(netip.MustParsePrefix(cidr), netip.Addr{}); got != want {
			t.Errorf("%s: %q, want %q", cidr, got, want)
		}
	}
}
