package bootstrap

import (
	"encoding/base64"
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// KickstartLabel is the label of the disc Anaconda reads a kickstart from
// with no boot option, and KickstartFile the file it reads at its root.
const (
	KickstartLabel = "OEMDRV"
	KickstartFile  = "ks.cfg"
)

// Install is what an unattended install needs besides the profile: the
// machine's hostname and the one disk the kickstart may touch.
type Install struct {
	Hostname string
	// Disk is the guest's name for the disk the build created (sda).
	Disk string
}

var (
	hostLabel = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
	diskName  = regexp.MustCompile(`^[a-z]+[a-z0-9]*$`)
)

// RenderKickstart writes the kickstart for an unattended RHEL-family install
// of in plus the profile (nil: none). See openspec accrete-installer-iso, D5.
//
// It is fixed on purpose, so the evidence an install is accepted on means
// what it says: text mode from the disc, DHCP, root locked, only in.Disk
// partitioned, the minimal environment with qemu-guest-agent enabled (the
// agent's hostname is the acceptance), and poweroff at the end, never
// reboot (the power-off is how the install's end is seen).
//
// A profile field it cannot honour is refused by name rather than dropped:
// run, whose commands cloud-init runs on a booted machine and a kickstart
// would run in the installer's chroot instead, and a key whose comment would
// need quoting the kickstart parser does not offer.
func RenderKickstart(in Install, p *Profile) (string, error) {
	if !hostLabel.MatchString(in.Hostname) {
		return "", fmt.Errorf("kickstart: hostname %q is not a DNS label", in.Hostname)
	}
	if !diskName.MatchString(in.Disk) {
		return "", fmt.Errorf("kickstart: disk %q is not a device name", in.Disk)
	}
	if p == nil {
		p = &Profile{}
	}
	if len(p.Run) > 0 {
		return "", errors.New("kickstart: bootstrap run is not supported by an install: its commands would run in the installer, not on the booted machine; use files, or run them once the machine is up")
	}
	tz := p.Timezone
	if tz == "" {
		tz = "Etc/UTC"
	}
	var b strings.Builder
	line := func(format string, args ...any) { fmt.Fprintf(&b, format+"\n", args...) }

	line("# Written by accrete for %s. It is rendered from the manifest: edit that, not this.", in.Hostname)
	line("text")
	line("cdrom")
	line("lang en_US.UTF-8")
	line("keyboard us")
	line("timezone %s --utc", tz)
	line("network --bootproto=dhcp --device=link --activate --hostname=%s", in.Hostname)
	line("rootpw --lock")
	line("selinux --enforcing")
	line("firewall --enabled --service=ssh")
	line("skipx")
	line("firstboot --disable")
	line("ignoredisk --only-use=%s", in.Disk)
	line("zerombr")
	line("clearpart --all --initlabel --drives=%s", in.Disk)
	line("autopart")
	line("bootloader --boot-drive=%s", in.Disk)
	line("services --enabled=qemu-guest-agent,sshd")
	for i, u := range p.Users {
		opts := ""
		if u.Shell != "" {
			opts = " --shell=" + u.Shell
		}
		// No password: the account is reached by its keys only.
		line("user --name=%s%s", u.Name, opts)
		for j, k := range u.SSHKeys {
			k = strings.TrimSpace(k)
			if strings.ContainsAny(k, "\"\\") {
				return "", fmt.Errorf("kickstart: users[%d].ssh_keys[%d]: its comment holds a quote or a backslash, which a kickstart cannot carry; drop them from the comment", i, j)
			}
			line("sshkey --username=%s \"%s\"", u.Name, k)
		}
	}
	line("")
	line("%%packages")
	line("@^minimal-environment")
	line("qemu-guest-agent")
	have := map[string]bool{"qemu-guest-agent": true}
	for _, pkg := range p.Packages {
		if !have[pkg] {
			line("%s", pkg)
			have[pkg] = true
		}
	}
	line("%%end")

	var post []string
	for _, u := range p.Users {
		if u.Sudo {
			file := "/etc/sudoers.d/90-accrete-" + u.Name
			post = append(post,
				fmt.Sprintf("echo '%s ALL=(ALL) NOPASSWD: ALL' > %s", u.Name, file),
				"chmod 0440 "+file)
		}
	}
	for i, f := range p.Files {
		if strings.ContainsAny(f.Path, "\x00\r\n") {
			return "", fmt.Errorf("kickstart: files[%d]: the path holds a control character", i)
		}
		q := shellQuote(f.Path)
		mode := f.Permissions
		if mode == "" {
			mode = "0644"
		}
		post = append(post,
			fmt.Sprintf("mkdir -p \"$(dirname %s)\"", q),
			fmt.Sprintf("base64 -d > %s <<'ACCRETE_FILE'", q),
			wrap(base64.StdEncoding.EncodeToString([]byte(f.Content)), 76),
			"ACCRETE_FILE",
			fmt.Sprintf("chmod %s %s", mode, q))
		if f.Owner != "" {
			post = append(post, fmt.Sprintf("chown %s %s", f.Owner, q))
		}
		post = append(post, fmt.Sprintf("restorecon %s 2>/dev/null || true", q))
	}
	if len(post) > 0 {
		line("")
		line("%%post --erroronfail")
		line("set -eu")
		for _, l := range post {
			line("%s", l)
		}
		line("restorecon -R /etc/sudoers.d 2>/dev/null || true")
		line("%%end")
	}
	line("")
	line("poweroff")
	return b.String(), nil
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

func wrap(s string, n int) string {
	if s == "" {
		return ""
	}
	var out []string
	for len(s) > n {
		out = append(out, s[:n])
		s = s[n:]
	}
	return strings.Join(append(out, s), "\n")
}
