package pxe

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestGateInRealIPXE boots QEMU machines from the network into a real iPXE
// and records which installer each one asks for. It runs only when
// ACCRETE_QEMU_IPXE=1, with qemu-system-x86_64, SeaBIOS and the Debian/Ubuntu
// ipxe package installed:
//
//	apt-get install qemu-system-x86 ipxe
//	ACCRETE_QEMU_IPXE=1 go test ./pxe -run RealIPXE -v
//
// The machines get their serial and UUID from -smbios, their MAC from
// -device. QEMU's user network hands out boot.ipxe over TFTP, which chains
// the gate over HTTP, as dnsmasq's proxy does on a real network.
func TestGateInRealIPXE(t *testing.T) {
	if os.Getenv("ACCRETE_QEMU_IPXE") != "1" {
		t.Skip("ACCRETE_QEMU_IPXE is not 1")
	}
	if _, err := exec.LookPath("qemu-system-x86_64"); err != nil {
		t.Skip("no qemu-system-x86_64")
	}

	var mu sync.Mutex
	var requests []string
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	base := fmt.Sprintf("http://10.0.2.2:%d", port)

	g := Gate{
		Kickstart: base + "/ks.cfg",
		Media: []Media{
			{Name: "rhel-9.6", Kernel: base + "/rhel96/vmlinuz", Initrd: base + "/rhel96/initrd.img", Repo: base + "/rhel96"},
			{Name: "rhel-10.0", Kernel: base + "/rhel10/vmlinuz", Initrd: base + "/rhel10/initrd.img", Repo: base + "/rhel10"},
		},
		Machines: []Machine{
			{Name: "web-01", Serials: []string{"CZJ1234ABC"}, Media: "rhel-9.6"},
			{Name: "db-01", UUIDs: []string{"4c4c4544-0042-3510-8051-b4c04f4e4d32"}, Media: "rhel-10.0"},
			{Name: "pc-01", MACs: []string{"52:54:00:aa:00:09"}, Media: "rhel-9.6"},
		},
	}
	script, err := Script(g)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests = append(requests, r.URL.Path)
		mu.Unlock()
		if r.URL.Path == "/gate.ipxe" {
			_, _ = w.Write(script)
			return
		}
		http.NotFound(w, r) // no real kernel: the request is the evidence
	})}
	go func() { _ = srv.Serve(ln) }()
	defer func() { _ = srv.Close() }()

	tftp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tftp, "boot.ipxe"), []byte("#!ipxe\nchain "+base+"/gate.ipxe\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name, serial, uuid, mac string
		wantKernel              string
		wantConsole             string
	}{
		{"by serial", "CZJ1234ABC", "11111111-2222-3333-4444-555555555555", "52:54:00:aa:00:01", "/rhel96/vmlinuz", "installing web-01"},
		{"by UUID", "OTHER1", "4c4c4544-0042-3510-8051-b4c04f4e4d32", "52:54:00:aa:00:02", "/rhel10/vmlinuz", "installing db-01"},
		{"by MAC", "OTHER2", "11111111-2222-3333-4444-555555555556", "52:54:00:aa:00:09", "/rhel96/vmlinuz", "installing pc-01"},
		{"unknown", "OTHER3", "11111111-2222-3333-4444-555555555557", "52:54:00:aa:00:03", "", "no install is armed"},
		{"ambiguous", "CZJ1234ABC", "4c4c4544-0042-3510-8051-b4c04f4e4d32", "52:54:00:aa:00:04", "", "more than one armed machine"},
	}
	ansi := regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			mu.Lock()
			requests = nil
			mu.Unlock()
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			defer cancel()
			var console bytes.Buffer
			cmd := exec.CommandContext(ctx, "qemu-system-x86_64",
				// iPXE finds only the 32-bit SMBIOS entry point.
				"-machine", "pc,smbios-entry-point-type=32",
				"-L", "/usr/share/seabios", "-L", "/usr/lib/ipxe/qemu",
				"-nographic", "-m", "256", "-boot", "n",
				"-smbios", "type=1,serial="+c.serial+",uuid="+c.uuid,
				"-netdev", "user,id=n0,tftp="+tftp+",bootfile=boot.ipxe",
				"-device", "e1000,netdev=n0,mac="+c.mac)
			cmd.Stdout, cmd.Stderr = &syncWriter{w: &console}, &syncWriter{w: &console}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
			for {
				out := ansi.ReplaceAllString(console.String(), "")
				if strings.Contains(out, c.wantConsole) && (c.wantKernel == "" || seen(&mu, &requests, c.wantKernel)) {
					// Give a wrong kernel request time to show up.
					time.Sleep(2 * time.Second)
					break
				}
				select {
				case <-ctx.Done():
					t.Fatalf("console never said %q; requests %v; console tail:\n%s", c.wantConsole, snapshot(&mu, &requests), tail(out))
				case <-time.After(300 * time.Millisecond):
				}
			}
			got := snapshot(&mu, &requests)
			for _, r := range got {
				if strings.HasSuffix(r, "/vmlinuz") && r != c.wantKernel {
					t.Fatalf("asked for %s; want %q (requests %v)", r, c.wantKernel, got)
				}
			}
			t.Logf("requests %v", got)
		})
	}
}

type syncWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (s *syncWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func seen(mu *sync.Mutex, rs *[]string, path string) bool {
	for _, r := range snapshot(mu, rs) {
		if r == path {
			return true
		}
	}
	return false
}

func snapshot(mu *sync.Mutex, rs *[]string) []string {
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), (*rs)...)
}

func tail(s string) string {
	if len(s) > 1500 {
		return s[len(s)-1500:]
	}
	return s
}
