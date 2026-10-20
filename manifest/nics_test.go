package manifest

import (
	"reflect"
	"strings"
	"testing"
)

const nicsManifest = `apiVersion: sybil/v1alpha1
kind: Environment
metadata:
  name: okd
spec:
  integration: lab
  networks:
    - name: front
      subnet: 10.20.0.0/24
      dns: [10.20.0.1]
    - name: storage
      subnet: 10.30.0.0/24
  resources:
    - name: node
      count: 2
      blueprint: provision-proxmox-vm
      nics:
        - network: front
          ip: static
        - network: storage
          ip: static
        - network: vmbr0
      inputs:
        hostname: okd-{{instance.number}}
`

// nics' first entry is the card network and ip always described, so a
// machine with one card means the same in either form.
func TestNICsDecodeIntoTheFirstCardAndTheRest(t *testing.T) {
	env, err := decodeOne(t, nicsManifest)
	if err != nil {
		t.Fatal(err)
	}
	r := env.Spec.Resources[0]
	if r.Network != "front" || r.IP != "static" {
		t.Fatalf("first card = %q %q", r.Network, r.IP)
	}
	want := []EnvironmentNIC{{Network: "storage", IP: "static"}, {Network: "vmbr0"}}
	if !reflect.DeepEqual(r.ExtraNICs, want) {
		t.Fatalf("extra cards = %+v", r.ExtraNICs)
	}
	if got := len(r.NICs()); got != 3 {
		t.Fatalf("NICs() has %d cards", got)
	}
}

func TestNICsRoundTripIsAFixedPoint(t *testing.T) {
	env, err := decodeOne(t, nicsManifest)
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
	if !reflect.DeepEqual(env, again) {
		t.Fatalf("round trip changed the value:\n%+v\n%+v", env, again)
	}
	// One card through nics: comes back the short way, and means the same.
	one := strings.Replace(nicsManifest, "        - network: storage\n          ip: static\n        - network: vmbr0\n", "", 1)
	env, err = decodeOne(t, one)
	if err != nil {
		t.Fatal(err)
	}
	if r := env.Spec.Resources[0]; r.Network != "front" || r.IP != "static" || r.ExtraNICs != nil {
		t.Fatalf("one card = %+v", r)
	}
}

func TestNICsAreRefusedWhenTheyCannotMeanAnything(t *testing.T) {
	cases := map[string]struct{ from, to, want string }{
		"both forms":               {"      nics:\n", "      network: front\n      nics:\n", "sets both nics and network/ip"},
		"two cards on one network": {"        - network: vmbr0\n", "        - network: front\n", `two network cards on "front"`},
		"too many":                 {"        - network: vmbr0\n", "        - network: vmbr0\n        - network: vmbr1\n        - network: vmbr2\n", "at most 4"},
		"static off spec.networks": {"        - network: vmbr0\n", "        - network: vmbr0\n          ip: static\n", "nics[2].ip: static"},
		"bad ip":                   {"        - network: vmbr0\n", "        - network: vmbr0\n          ip: fixed\n", `nics[2].ip "fixed"`},
		"no network":               {"        - network: vmbr0\n", "        - ip: dhcp\n", "nics[2] names no network"},
		"and the input":            {"      inputs:\n", "      inputs:\n        net1: virtio,bridge=x\n", "inputs.net1"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			if !strings.Contains(nicsManifest, c.from) {
				t.Fatalf("fixture has no %q", c.from)
			}
			_, err := decodeOne(t, strings.Replace(nicsManifest, c.from, c.to, 1))
			if err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("err = %v, want %q", err, c.want)
			}
		})
	}
}

// A later card gets an address and no name servers, so its network needs
// none declared — only the first card's does.
func TestALaterStaticCardNeedsNoDNS(t *testing.T) {
	if _, err := decodeOne(t, nicsManifest); err != nil {
		t.Fatalf("storage has no dns and is the second card: %v", err)
	}
}
