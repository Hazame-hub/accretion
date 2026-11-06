package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/hazame-hub/accretion/bundle"
	"github.com/hazame-hub/accretion/pxe"
)

const bundleUsage = `accrete bundle writes one kickstart answer file (ks.cfg) for every machine a
NetworkInstall file declares. Each machine picks its own profile when it
boots, from its SMBIOS UUID, serial number or MAC; a machine that matches
none, or more than one, stops before any disk is partitioned. Put ks.cfg on
a disc or USB stick labelled OEMDRV, beside the installer.

Usage:
  accrete bundle -f install.yaml [-o DIR]
`

const pxeUsage = `accrete pxe writes what a network install needs, without changing the
network's DHCP server:

  gate.ipxe           boots the installer only for machines with armed: true;
                      any other machine goes back to its own disk
  ks.cfg              the answer file for the armed machines
  dnsmasq-proxy.conf  a proxy DHCP that assigns no address

Serve gate.ipxe, ks.cfg and the media under spec.server, and run
"dnsmasq -k -C dnsmasq-proxy.conf" on a machine on spec.subnet. Once a
machine is installed, set armed: false and run accrete pxe again, so its
next network boot falls through to its disk.

Usage:
  accrete pxe -f install.yaml [-o DIR]
`

// netInstall is a NetworkInstall file.
type netInstall struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		// Server is the http(s) base URL that serves gate.ipxe, ks.cfg and
		// the media.
		Server   string `yaml:"server"`
		Subnet   string `yaml:"subnet"`
		TFTPRoot string `yaml:"tftp_root"`
		Media    []struct {
			Name   string   `yaml:"name"`
			Kernel string   `yaml:"kernel"`
			Initrd string   `yaml:"initrd"`
			Repo   string   `yaml:"repo"`
			Args   []string `yaml:"args"`
		} `yaml:"media"`
		Machines []netMachine `yaml:"machines"`
	} `yaml:"spec"`
}

type netMachine struct {
	Name    string   `yaml:"name"`
	UUIDs   []string `yaml:"uuids"`
	Serials []string `yaml:"serials"`
	MACs    []string `yaml:"macs"`
	Media   string   `yaml:"media"`
	// Kickstart is the machine's profile, a file relative to the
	// NetworkInstall file.
	Kickstart string `yaml:"kickstart"`
	Armed     bool   `yaml:"armed"`
}

func runBundle(args []string, stdout, stderr io.Writer) int {
	return runNetInstall("bundle", bundleUsage, args, stdout, stderr)
}

func runPXE(args []string, stdout, stderr io.Writer) int {
	return runNetInstall("pxe", pxeUsage, args, stdout, stderr)
}

func runNetInstall(cmd, usageText string, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("accrete "+cmd, flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.Usage = func() { _, _ = fmt.Fprint(stderr, usageText) }
	file := fs.String("f", "", "the NetworkInstall file (YAML)")
	out := fs.String("o", "", "the directory to write into (default: accrete-"+cmd+" beside the file)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *file == "" || fs.NArg() > 0 {
		_, _ = fmt.Fprint(stderr, usageText)
		return 2
	}
	fail := func(format string, a ...any) int {
		_, _ = fmt.Fprintf(stderr, "accrete %s: "+format+"\n", append([]any{cmd}, a...)...)
		return 2
	}
	ni, err := loadNetInstall(*file)
	if err != nil {
		return fail("%s: %v", *file, err)
	}
	dir := filepath.Dir(*file)
	if *out == "" {
		*out = filepath.Join(dir, "accrete-"+cmd)
	}

	// bundle serves every declared machine (a USB stick has no arming);
	// pxe serves only the armed ones, so the gate and the answer file it
	// points at always agree.
	var profiles []bundle.Profile
	var armed []netMachine
	for _, m := range ni.Spec.Machines {
		if cmd == "pxe" && !m.Armed {
			continue
		}
		if m.Kickstart == "" {
			return fail("%s: machine %q has no kickstart file", *file, m.Name)
		}
		ks, err := os.ReadFile(filepath.Join(dir, m.Kickstart))
		if err != nil {
			return fail("%s: machine %q: %v", *file, m.Name, err)
		}
		profiles = append(profiles, bundle.Profile{Name: m.Name, UUIDs: m.UUIDs, Serials: m.Serials, MACs: m.MACs, Kickstart: ks})
		armed = append(armed, m)
	}

	files := map[string][]byte{}
	var ksSum string
	var profileSums map[string]string
	if len(profiles) > 0 {
		b, err := bundle.Build(profiles)
		if err != nil {
			return fail("%s: %v", *file, err)
		}
		files["ks.cfg"], ksSum, profileSums = b.KS, b.SHA256, b.ProfileSHA256
	} else if cmd == "bundle" {
		return fail("%s declares no machine", *file)
	}

	if cmd == "pxe" {
		server := strings.TrimRight(ni.Spec.Server, "/")
		g := pxe.Gate{Kickstart: server + "/ks.cfg"}
		for _, md := range ni.Spec.Media {
			repo := md.Repo
			if repo != "cdrom" {
				repo = under(server, repo)
			}
			g.Media = append(g.Media, pxe.Media{Name: md.Name, Kernel: under(server, md.Kernel), Initrd: under(server, md.Initrd), Repo: repo, Args: md.Args})
		}
		for _, m := range armed {
			g.Machines = append(g.Machines, pxe.Machine{Name: m.Name, UUIDs: m.UUIDs, Serials: m.Serials, MACs: m.MACs, Media: m.Media})
		}
		gate, err := pxe.Script(g)
		if err != nil {
			return fail("%s: %v", *file, err)
		}
		tftp := ni.Spec.TFTPRoot
		if tftp == "" {
			tftp = "/srv/tftp"
		}
		conf, err := pxe.DnsmasqProxy(pxe.Proxy{Subnet: ni.Spec.Subnet, TFTPRoot: tftp, Script: server + "/gate.ipxe"})
		if err != nil {
			return fail("%s: %v", *file, err)
		}
		files["gate.ipxe"], files["dnsmasq-proxy.conf"] = gate, conf
		if len(profiles) == 0 {
			// Nothing armed: an answer file nothing will fetch would only
			// mislead; remove a stale one.
			files["ks.cfg"] = nil
		}
	}

	if err := os.MkdirAll(*out, 0o755); err != nil {
		return fail("%v", err)
	}
	names := make([]string, 0, len(files))
	for n := range files {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		p := filepath.Join(*out, n)
		if files[n] == nil {
			if err := os.Remove(p); err != nil && !os.IsNotExist(err) {
				return fail("%v", err)
			}
			continue
		}
		if err := writeFileAtomic(p, files[n]); err != nil {
			return fail("%v", err)
		}
	}

	say := func(format string, a ...any) { _, _ = fmt.Fprintf(stdout, format, a...) }
	if cmd == "pxe" {
		say("armed for an install: %s\n", machineNames(armed))
		say("wrote %s/gate.ipxe and %s/dnsmasq-proxy.conf\n", *out, *out)
	}
	if ksSum != "" {
		say("wrote %s/ks.cfg  sha256 %s\n", *out, ksSum)
		for _, m := range armed {
			say("  %-24s profile sha256 %s\n", m.Name, profileSums[m.Name])
		}
	} else if cmd == "pxe" {
		say("no machine is armed: every machine that boots from the network goes back to its own disk\n")
	}
	return 0
}

func loadNetInstall(path string) (netInstall, error) {
	var ni netInstall
	src, err := os.ReadFile(path)
	if err != nil {
		return ni, err
	}
	dec := yaml.NewDecoder(bytes.NewReader(src))
	dec.KnownFields(true)
	if err := dec.Decode(&ni); err != nil {
		return ni, err
	}
	if ni.APIVersion != "accretion/v1alpha1" || ni.Kind != "NetworkInstall" {
		return ni, fmt.Errorf("want apiVersion accretion/v1alpha1 and kind NetworkInstall, got %q %q", ni.APIVersion, ni.Kind)
	}
	if len(ni.Spec.Machines) == 0 {
		return ni, fmt.Errorf("spec.machines is empty")
	}
	return ni, nil
}

// under resolves a path relative to the server; a full URL is kept.
func under(server, p string) string {
	if u, err := url.Parse(p); err == nil && u.Scheme != "" {
		return p
	}
	return server + "/" + strings.TrimLeft(p, "/")
}

func machineNames(ms []netMachine) string {
	if len(ms) == 0 {
		return "none"
	}
	n := make([]string, len(ms))
	for i, m := range ms {
		n[i] = m.Name
	}
	return strings.Join(n, ", ")
}

func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}
