// Package pxe writes what a machine with no BMC needs to install over the
// network without anyone choosing an entry in a menu, and without changing
// the network's DHCP server:
//
//   - a dnsmasq configuration in proxy-DHCP mode, which answers only the
//     PXE part of a boot request and leaves address assignment to the DHCP
//     server already on the network;
//   - an iPXE gate script. It boots the installer only for a machine accrete
//     has armed for an install, recognised by its SMBIOS UUID, serial number
//     or the MAC of the card it boots from, and sends every other machine
//     back to its firmware, which goes on to its own disk.
//
// The gate is a first check. The kickstart (package bundle) selects the
// profile again from the same values once Linux runs, and stops the install
// if the two disagree, so a machine is never installed on the gate's word
// alone.
//
// A machine that boots from the network first would meet the gate at every
// boot. That is why the gate lists only machines armed for an install:
// accrete removes a machine from it once its install is accepted, and a
// reboot then falls through to the disk.
//
// Design: sybil-dev openspec/changes/accrete-bare-metal-install, D11 to D13.
package pxe

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"

	"github.com/hazame-hub/accretion/bundle"
)

// Media is one installer the gate can boot: a kernel and initrd served over
// HTTP, and the arguments that point the installer at its repository.
type Media struct {
	// Name is how a machine refers to it: lower-case letters, digits, '.'
	// and '-'.
	Name string
	// Kernel and Initrd are http(s) URLs, for example the images/pxeboot
	// files of a RHEL DVD served by any web server.
	Kernel string
	Initrd string
	// Repo is the inst.repo URL, the DVD's tree on the same server.
	Repo string
	// Args are extra kernel arguments, each a key or key=value with no
	// space.
	Args []string
}

// Machine is one machine armed for an install.
type Machine struct {
	Name    string
	UUIDs   []string
	Serials []string
	MACs    []string
	// Media names the installer it boots.
	Media string
}

// Gate is the input of Script.
type Gate struct {
	// Kickstart is the http(s) URL of the shared answer file (package
	// bundle) that every armed machine installs from.
	Kickstart string
	Media     []Media
	Machines  []Machine
}

var (
	mediaName = regexp.MustCompile(`^[a-z0-9]([a-z0-9.-]{0,62}[a-z0-9])?$`)
	// argPattern keeps an argument one token in iPXE and on the kernel
	// command line.
	argPattern = regexp.MustCompile(`^[A-Za-z0-9._:/=,+@-]{1,200}$`)
	// gateSerial is stricter than the bundle's: iPXE splits words on
	// spaces and has no quoting.
	gateSerial = regexp.MustCompile(`^[A-Za-z0-9._/+:#-]{1,64}$`)
)

// Script writes the iPXE gate. Machines are written in name order, so the
// same input gives the same bytes. It refuses an unknown media name, a
// selector two machines share, a placeholder identity, and any value iPXE
// would split or interpret.
func Script(g Gate) ([]byte, error) {
	if err := httpURL("kickstart", g.Kickstart); err != nil {
		return nil, err
	}
	media := map[string]Media{}
	for _, m := range g.Media {
		if !mediaName.MatchString(m.Name) {
			return nil, fmt.Errorf("pxe: media name %q: use lower-case letters, digits, '.' and '-'", m.Name)
		}
		if _, dup := media[m.Name]; dup {
			return nil, fmt.Errorf("pxe: two media are named %q", m.Name)
		}
		for what, u := range map[string]string{"kernel": m.Kernel, "initrd": m.Initrd, "repo": m.Repo} {
			if err := httpURL("media "+m.Name+" "+what, u); err != nil {
				return nil, err
			}
		}
		for _, a := range m.Args {
			if !argPattern.MatchString(a) {
				return nil, fmt.Errorf("pxe: media %q: argument %q is not one token iPXE and the kernel read alike", m.Name, a)
			}
		}
		media[m.Name] = m
	}

	type armed struct {
		id    bundle.Identity
		media string
	}
	var ms []armed
	var claims bundle.Claims
	for _, m := range g.Machines {
		id, err := bundle.CheckIdentity(m.Name, m.UUIDs, m.Serials, m.MACs)
		if err != nil {
			return nil, fmt.Errorf("pxe: %w", err)
		}
		for _, s := range id.Serials {
			if !gateSerial.MatchString(s) {
				return nil, fmt.Errorf("pxe: machine %q: serial %q has a space or a character iPXE cannot compare; select it by UUID or MAC", m.Name, s)
			}
		}
		if _, ok := media[m.Media]; !ok {
			return nil, fmt.Errorf("pxe: machine %q boots media %q, which is not declared", m.Name, m.Media)
		}
		if err := claims.Add(id); err != nil {
			return nil, fmt.Errorf("pxe: %w", err)
		}
		ms = append(ms, armed{id, m.Media})
	}
	sort.Slice(ms, func(i, j int) bool { return ms[i].id.Name < ms[j].id.Name })

	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("#!ipxe\n")
	w("# Written by accrete. Armed for an install: %s.\n", namesOr(ms, func(a armed) string { return a.id.Name }))
	w("# Any other machine goes back to its firmware and boots its own disk.\n")
	w("set accrete-n:int32 0\n")
	w("set accrete-m\n")
	for _, m := range ms {
		w("# %s\n", m.id.Name)
		w("set accrete-hit 0\n")
		for _, forms := range m.id.UUIDs {
			for _, f := range forms {
				w("iseq ${uuid} %s && set accrete-hit 1 ||\n", f)
			}
		}
		for _, s := range m.id.Serials {
			w("iseq ${serial} %s && set accrete-hit 1 ||\n", s)
		}
		for _, mac := range m.id.MACs {
			w("iseq ${netX/mac} %s && set accrete-hit 1 ||\n", mac)
		}
		w("iseq ${accrete-hit} 1 && inc accrete-n && set accrete-m %s ||\n", m.id.Name)
	}
	w("iseq ${accrete-n} 0 && goto accrete-unknown ||\n")
	w("iseq ${accrete-n} 1 || goto accrete-ambiguous\n")
	w("echo accrete: installing ${accrete-m} (uuid ${uuid} serial ${serial} mac ${netX/mac})\n")
	for _, m := range ms {
		w("iseq ${accrete-m} %s && goto accrete-media-%s ||\n", m.id.Name, m.media)
	}
	w("goto accrete-unknown\n")
	used := map[string]bool{}
	for _, m := range ms {
		used[m.media] = true
	}
	names := make([]string, 0, len(used))
	for n := range used {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		md := media[n]
		args := append([]string{"initrd=initrd.img", "inst.repo=" + md.Repo, "inst.ks=" + g.Kickstart, "inst.text"}, md.Args...)
		w(":accrete-media-%s\n", n)
		w("kernel --name vmlinuz %s %s || goto accrete-failed\n", md.Kernel, strings.Join(args, " "))
		w("initrd --name initrd.img %s || goto accrete-failed\n", md.Initrd)
		w("boot || goto accrete-failed\n")
	}
	w(":accrete-unknown\n")
	w("echo accrete: no install is armed for this machine (uuid ${uuid} serial ${serial} mac ${netX/mac}); booting from its own disk\n")
	w("exit 1\n")
	w(":accrete-ambiguous\n")
	w("echo accrete: this machine matches more than one armed machine (uuid ${uuid} serial ${serial} mac ${netX/mac}); nothing is installed\n")
	w("sleep 10\n")
	w("exit 1\n")
	w(":accrete-failed\n")
	w("echo accrete: the installer for ${accrete-m} could not be loaded; nothing is installed\n")
	w("sleep 10\n")
	w("exit 1\n")
	return []byte(b.String()), nil
}

// Proxy is the input of DnsmasqProxy.
type Proxy struct {
	// Subnet is the network the proxy answers on, in CIDR form, for example
	// 192.168.1.0/24. The proxy assigns no address in it.
	Subnet string
	// TFTPRoot holds the iPXE loaders: undionly.kpxe (BIOS) and ipxe.efi
	// (UEFI). UEFI Secure Boot must be off for ipxe.efi, which is unsigned.
	TFTPRoot string
	// Script is the http(s) URL of the gate script.
	Script string
}

// DnsmasqProxy writes a dnsmasq configuration in proxy-DHCP mode. It
// answers the PXE options of a boot request (first the iPXE loader over
// TFTP, then, once iPXE asks, the gate script) and assigns no address, so
// the DHCP server already on the network is not changed. Run it on any
// machine on that network: dnsmasq -k -C <file>.
func DnsmasqProxy(p Proxy) ([]byte, error) {
	ip, n, err := net.ParseCIDR(p.Subnet)
	if err != nil || ip.To4() == nil {
		return nil, fmt.Errorf("pxe: subnet %q is not an IPv4 CIDR such as 192.168.1.0/24", p.Subnet)
	}
	if err := httpURL("script", p.Script); err != nil {
		return nil, err
	}
	if !strings.HasPrefix(p.TFTPRoot, "/") || strings.ContainsAny(p.TFTPRoot, " \n,\"") {
		return nil, fmt.Errorf("pxe: TFTP root %q must be an absolute path with no space, comma or quote", p.TFTPRoot)
	}
	mask := net.IP(n.Mask).String()
	var b strings.Builder
	w := func(format string, args ...any) { fmt.Fprintf(&b, format, args...) }
	w("# Written by accrete. Proxy DHCP for network installs on %s.\n", n.String())
	w("# It assigns no address: the network's own DHCP server keeps doing that.\n")
	w("port=0\n")
	w("dhcp-range=%s,proxy,%s\n", n.IP.String(), mask)
	w("log-dhcp\n")
	w("enable-tftp\n")
	w("tftp-root=%s\n", p.TFTPRoot)
	w("# iPXE says so in option 175; until then, hand out the iPXE loader.\n")
	w("dhcp-match=set:ipxe,175\n")
	w("pxe-prompt=\"accrete\",0\n")
	w("pxe-service=tag:!ipxe,x86PC,\"accrete: load iPXE\",undionly.kpxe\n")
	w("pxe-service=tag:!ipxe,X86-64_EFI,\"accrete: load iPXE\",ipxe.efi\n")
	w("pxe-service=tag:ipxe,x86PC,\"accrete: gate\",%s\n", p.Script)
	w("pxe-service=tag:ipxe,X86-64_EFI,\"accrete: gate\",%s\n", p.Script)
	return []byte(b.String()), nil
}

func httpURL(what, s string) error {
	u, err := url.Parse(s)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return fmt.Errorf("pxe: %s %q must be an http(s) URL", what, s)
	}
	if u.User != nil {
		return fmt.Errorf("pxe: %s URL must not carry a credential: every machine on the network can read the gate", what)
	}
	if strings.ContainsAny(s, " \n\"'${}") {
		return fmt.Errorf("pxe: %s URL %q has a character iPXE would interpret", what, s)
	}
	return nil
}

func namesOr[T any](xs []T, name func(T) string) string {
	if len(xs) == 0 {
		return "none"
	}
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = name(x)
	}
	return strings.Join(out, ", ")
}
