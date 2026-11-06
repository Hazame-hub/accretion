package main

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func runCmd(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errb bytes.Buffer
	code := run(context.Background(), args, strings.NewReader(""), &out, &errb, func(string) string { return "" })
	return code, out.String(), errb.String()
}

// copyExample copies examples/network-install into a temp dir and returns
// the path of its install.yaml.
func copyExample(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join("..", "..", "examples", "network-install")
	err := filepath.Walk(src, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(src, p)
		if info.IsDir() {
			return os.MkdirAll(filepath.Join(dir, rel), 0o755)
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		return os.WriteFile(filepath.Join(dir, rel), b, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "install.yaml")
}

func TestPXEWritesGateKickstartAndProxy(t *testing.T) {
	f := copyExample(t)
	code, out, errs := runCmd(t, "pxe", "-f", f)
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errs)
	}
	dir := filepath.Join(filepath.Dir(f), "accrete-pxe")
	gate, _ := os.ReadFile(filepath.Join(dir, "gate.ipxe"))
	ks, _ := os.ReadFile(filepath.Join(dir, "ks.cfg"))
	conf, _ := os.ReadFile(filepath.Join(dir, "dnsmasq-proxy.conf"))
	if !strings.Contains(string(gate), "Armed for an install: accrete-pxe-a.") ||
		!strings.Contains(string(gate), "kernel --name vmlinuz http://192.168.1.212:8088/rhel96/vmlinuz initrd=initrd.img inst.repo=cdrom inst.ks=http://192.168.1.212:8088/ks.cfg") {
		t.Fatalf("gate:\n%s", gate)
	}
	// Only the armed machine is in the answer file the gate points at.
	if !strings.Contains(string(ks), "accrete-pxe-a") || strings.Contains(string(ks), "rack-01") {
		t.Fatalf("ks.cfg should hold accrete-pxe-a only:\n%s", ks)
	}
	if !strings.Contains(string(conf), "dhcp-range=192.168.1.0,proxy,255.255.255.0") || !strings.Contains(string(conf), "http://192.168.1.212:8088/gate.ipxe") {
		t.Fatalf("proxy conf:\n%s", conf)
	}
	if !strings.Contains(out, "armed for an install: accrete-pxe-a") || !strings.Contains(out, "profile sha256") {
		t.Fatalf("stdout: %s", out)
	}
}

func TestPXEDisarmRemovesTheAnswerFile(t *testing.T) {
	f := copyExample(t)
	if code, _, errs := runCmd(t, "pxe", "-f", f); code != 0 {
		t.Fatal(errs)
	}
	src, _ := os.ReadFile(f)
	if err := os.WriteFile(f, bytes.ReplaceAll(src, []byte("armed: true"), []byte("armed: false")), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, errs := runCmd(t, "pxe", "-f", f)
	if code != 0 {
		t.Fatal(errs)
	}
	dir := filepath.Join(filepath.Dir(f), "accrete-pxe")
	gate, _ := os.ReadFile(filepath.Join(dir, "gate.ipxe"))
	if strings.Contains(string(gate), "kernel ") || !strings.Contains(out, "no machine is armed") {
		t.Fatalf("a disarmed gate can still boot an installer:\n%s\n%s", gate, out)
	}
	if _, err := os.Stat(filepath.Join(dir, "ks.cfg")); !os.IsNotExist(err) {
		t.Fatalf("stale ks.cfg left behind: %v", err)
	}
}

func TestBundleServesEveryMachine(t *testing.T) {
	f := copyExample(t)
	out := filepath.Join(t.TempDir(), "usb")
	code, _, errs := runCmd(t, "bundle", "-f", f, "-o", out)
	if code != 0 {
		t.Fatal(errs)
	}
	ks, _ := os.ReadFile(filepath.Join(out, "ks.cfg"))
	if !strings.Contains(string(ks), "accrete-pxe-a, rack-01") {
		t.Fatalf("ks.cfg should hold both machines:\n%.400s", ks)
	}
	if _, err := os.Stat(filepath.Join(out, "gate.ipxe")); !os.IsNotExist(err) {
		t.Fatal("bundle wrote a gate")
	}
}

func TestNetInstallRefuses(t *testing.T) {
	cases := []struct {
		name, from, to, says string
	}{
		{"unknown field", "armed: true", "armed: true\n      armd: true", "armd"},
		{"wrong kind", "kind: NetworkInstall", "kind: Environment", "NetworkInstall"},
		{"missing kickstart", "profiles/accrete-pxe-a.ks", "profiles/nope.ks", "nope.ks"},
		{"unknown media", "      media: rhel-9.6\n      kickstart: profiles/accrete-pxe-a.ks", "      media: debian\n      kickstart: profiles/accrete-pxe-a.ks", `"debian"`},
		{"placeholder serial", `serials: ["CZJ1234ABC"]`, `serials: ["Default string"]`, "placeholder"},
		{"shared MAC", `serials: ["CZJ1234ABC"]`, `macs: ["bc:24:11:ac:ce:01"]`, "share"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := copyExample(t)
			src, _ := os.ReadFile(f)
			if !bytes.Contains(src, []byte(c.from)) {
				t.Fatalf("example lacks %q", c.from)
			}
			if err := os.WriteFile(f, bytes.Replace(src, []byte(c.from), []byte(c.to), 1), 0o644); err != nil {
				t.Fatal(err)
			}
			// bundle reads every machine, so it sees rack-01's edits too.
			code, _, errs := runCmd(t, "bundle", "-f", f)
			if c.name == "unknown media" {
				code, _, errs = runCmd(t, "pxe", "-f", f)
			}
			if code != 2 || !strings.Contains(errs, c.says) {
				t.Fatalf("exit %d, %q; want 2 mentioning %q", code, errs, c.says)
			}
		})
	}
}

func TestNetInstallRefusesASecretInAProfile(t *testing.T) {
	f := copyExample(t)
	p := filepath.Join(filepath.Dir(f), "profiles", "rack-01.ks")
	if err := os.WriteFile(p, []byte("rootpw {{secret.root}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, _, errs := runCmd(t, "bundle", "-f", f)
	if code != 2 || !strings.Contains(errs, "secret") {
		t.Fatalf("exit %d: %s", code, errs)
	}
}
