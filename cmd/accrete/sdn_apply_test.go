package main

import (
	"os"
	"strings"
	"testing"
)

// A machine on a network the manifest declares and the cluster lacks.
const onNewNetwork = `      network: web
  networks:
    - name: web
      zone: accrete
      subnet: 10.30.0.0/24
`

func TestApplyCreatesTheNetworkThenTheMachinesOnIt(t *testing.T) {
	l := newApplyLab(t, onNewNetwork)
	p := l.plan()
	if len(p.Networks) != 1 || p.Networks[0].Action != "create" {
		t.Fatalf("plan networks = %+v", p.Networks)
	}
	for _, e := range p.Entries {
		if e.Action != "unknown" || strings.Join(e.WaitsFor, ",") != "web" {
			t.Fatalf("a machine on a network not live yet must wait for it: %+v", e)
		}
	}
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 0 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	net, web0 := strings.Index(out, "create network web (10.30.0.0/24 in zone accrete)"), strings.Index(out, "create web/0 (on web, once created)")
	if net < 0 || web0 < net || !strings.Contains(out, "3 done, 0 failed") {
		t.Fatalf("the network must be created first, then both machines:\n%s", out)
	}
	if v := l.pve.Networks["web"]; v == nil || !v.Up {
		t.Fatalf("vnet = %+v", v)
	}
	if got := l.pve.Guest(l.pve.Named("accrete-web-1")[0]).Config["net0"]; !strings.Contains(got.(string), "bridge=web") {
		t.Errorf("net0 = %v", got)
	}

	// Applied again, the network plans as no-op and nothing is sent.
	before := len(l.pve.Writes())
	out, code = l.accrete("", "apply", "--auto-approve")
	if code != 0 || !strings.Contains(out, "Nothing to apply.") || len(l.pve.Writes()) != before {
		t.Errorf("second apply: exit %d, writes %v\n%s", code, l.pve.Writes()[before:], out)
	}
}

func TestApplyStartsNoMachineOnANetworkItFailedToCreate(t *testing.T) {
	l := newApplyLab(t, onNewNetwork)
	l.pve.NodeNetworkChanges = "--- /etc/network/interfaces\n+++ /etc/network/interfaces.new\n"
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 {
		t.Fatalf("apply: exit %d\n%s", code, out)
	}
	for _, want := range []string{"node pve1 has network changes", "not started: network web was not created", "0 done, 3 failed"} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if w := l.pve.Writes(); len(w) != 0 {
		t.Errorf("nothing may be sent: %v", w)
	}
}

func TestApplyNeverChangesANetwork(t *testing.T) {
	l := newApplyLab(t, onNewNetwork)
	if out, code := l.accrete("", "apply", "--auto-approve"); code != 0 {
		t.Fatalf("first apply: %d\n%s", code, out)
	}
	src := strings.Replace(readFile(t, l.manifest), "subnet: 10.30.0.0/24", "subnet: 10.31.0.0/24", 1)
	writeFile(t, l.manifest, src)
	before := len(l.pve.Writes())
	out, code := l.accrete("", "apply", "--auto-approve")
	if code != 1 || !strings.Contains(out, "never changes one") || len(l.pve.Writes()) != before {
		t.Errorf("exit %d, writes %v\n%s", code, l.pve.Writes()[before:], out)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func writeFile(t *testing.T, path, src string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionsAddWhatCreatingTheNetworkNeeds(t *testing.T) {
	l := newApplyLab(t, onNewNetwork)
	out, _ := l.accrete("", "permissions", "--offline")
	for _, want := range []string{
		"SDN.Allocate", "Sys.Modify",
		"/sdn/zones/accrete/web", // a card on the declared network is granted on its zone's path
		"only used while a declared network has to be created",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "/sdn/zones/localnetwork/web") {
		t.Errorf("a card on a declared network is not on a Linux bridge:\n%s", out)
	}
}
