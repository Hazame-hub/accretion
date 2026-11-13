package main

import (
	"fmt"
	"strings"
	"testing"
)

// The flk shape: a fixed address and gateway on the first card, a second
// card on another bridge taking DHCP, a third with its own fixed address.
func TestApplyBuildsFixedAddressesAndMoreCards(t *testing.T) {
	l := newApplyLab(t, `        net0: virtio,bridge=vmgmt
        ip_address: 10.10.0.{{instance.number}}0/24
        gateway: 10.10.0.1
        nameservers: 10.10.1.11,10.10.1.12
        net1: virtio,bridge=vk8sx,tag=30
        net2: virtio,bridge=vobsv
        ip_address2: 10.10.7.{{instance.number}}0/24
`)
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	for i, vmid := range []int{100, 102} {
		g := l.pve.Guest(vmid)
		for card, bridge := range map[string]string{"net0": "bridge=vmgmt", "net1": "bridge=vk8sx,tag=30", "net2": "bridge=vobsv"} {
			if v := fmt.Sprint(g.Config[card]); !strings.HasSuffix(v, ","+bridge) {
				t.Errorf("guest %d %s = %q, want it on %s", vmid, card, v, bridge)
			}
		}
		mac := func(card string) string {
			v := fmt.Sprint(g.Config[card])
			_, rest, _ := strings.Cut(strings.Split(v, ",")[0], "=")
			return strings.ToLower(rest)
		}
		var seed string
		for volid, b := range l.pve.Files {
			if strings.Contains(string(b), "local-hostname: accrete-web-"+fmt.Sprint(i+1)) {
				seed = volid + "\n" + string(b)
			}
		}
		if seed == "" {
			t.Fatalf("no seed for guest %d among %d files", vmid, len(l.pve.Files))
		}
		for _, want := range []string{
			"  net0:\n    match:\n      macaddress: \"" + mac("net0") + "\"\n    dhcp4: false\n    addresses:\n      - 10.10.0." + fmt.Sprint(i+1) + "0/24",
			"via: 10.10.0.1",
			"  net1:\n    match:\n      macaddress: \"" + mac("net1") + "\"\n    dhcp4: true",
			"  net2:\n    match:\n      macaddress: \"" + mac("net2") + "\"\n    dhcp4: false\n    addresses:\n      - 10.10.7." + fmt.Sprint(i+1) + "0/24",
		} {
			if !strings.Contains(seed, want) {
				t.Errorf("guest %d seed lacks %q", vmid, want)
			}
		}
		if strings.Contains(seed, "{{accrete.mac") {
			t.Errorf("guest %d seed still holds a placeholder", vmid)
		}
	}
	if p := l.plan(); !strings.Contains(p.Summary, "2 unchanged") {
		t.Errorf("after apply: %s", p.Summary)
	}
}

func TestPlanRefusesOneFixedAddressForTwoMachines(t *testing.T) {
	l := newApplyLab(t, "        ip_address: 10.0.0.5/24\n")
	out, code := l.accrete("", "plan")
	if code != 1 || !strings.Contains(out, "error: 10.0.0.5 is the fixed address of web/0 and web/1") {
		t.Errorf("exit %d:\n%s", code, out)
	}
}
