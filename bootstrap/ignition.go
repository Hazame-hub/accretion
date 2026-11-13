package bootstrap

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// IgnitionVersion is the Ignition spec the configs accrete writes declare.
// Fedora CoreOS has read 3.4.0 since 2022.
const IgnitionVersion = "3.4.0"

// FirstBootUnit is the systemd unit an Ignition config from accrete adds: on
// the machine's first full boot it marks itself done and powers the machine
// off. A machine Ignition failed on stops in its emergency shell instead and
// never powers off, so the power-off is the guest's own word, readable from
// the hypervisor with no agent, that the config was applied and the boot
// reached multi-user.target. FirstBootStamp is the file it leaves.
const (
	FirstBootUnit  = "accrete-first-boot.service"
	FirstBootStamp = "/var/lib/accrete/first-boot-done"
)

// RenderIgnition writes the Ignition config of a Fedora CoreOS (or other
// Ignition-booted) machine for base plus the profile (nil: none).
//
// Like Render, it marshals, so no value can break out of the document, and
// it refuses what it cannot honour by name rather than dropping it:
//   - packages: an image-based OS has no package step at first boot; run the
//     software as a container, or layer it later with rpm-ostree;
//   - run: commands at first boot would need a unit per command; write a
//     unit with files instead;
//   - a file under /usr, which is read-only on CoreOS, or one accrete writes
//     itself (/etc/hostname, /etc/localtime, the sudoers drop-ins).
//
// Users get their SSH keys and shell; sudo is a sudoers drop-in, like the
// kickstart's. Base.Packages and Base.Commands are refused: nothing accrete
// enrols is installed this way.
func RenderIgnition(base Base, p *Profile) (string, error) {
	if !hostLabel.MatchString(base.Hostname) {
		return "", fmt.Errorf("ignition: hostname %q is not a DNS label", base.Hostname)
	}
	if len(base.Packages) > 0 || len(base.Commands) > 0 {
		return "", errors.New("ignition: an Ignition config installs no package and runs no command at first boot")
	}
	if p == nil {
		p = &Profile{}
	}
	switch {
	case len(p.Packages) > 0:
		return "", errors.New("ignition: bootstrap packages are not supported: an image-based OS such as Fedora CoreOS installs no package at first boot; run the software as a container, or layer it later with rpm-ostree")
	case len(p.Run) > 0:
		return "", errors.New("ignition: bootstrap run is not supported by an Ignition config; write a systemd unit with files instead")
	}

	type node struct {
		Name string `json:"name"`
	}
	type contents struct {
		Source string `json:"source"`
	}
	type file struct {
		Path      string   `json:"path"`
		Mode      int      `json:"mode"`
		Overwrite bool     `json:"overwrite"`
		Contents  contents `json:"contents"`
		User      *node    `json:"user,omitempty"`
		Group     *node    `json:"group,omitempty"`
	}
	type link struct {
		Path      string `json:"path"`
		Target    string `json:"target"`
		Overwrite bool   `json:"overwrite"`
		Hard      bool   `json:"hard"`
	}
	type user struct {
		Name              string   `json:"name"`
		SSHAuthorizedKeys []string `json:"sshAuthorizedKeys,omitempty"`
		Shell             string   `json:"shell,omitempty"`
	}
	type unit struct {
		Name     string `json:"name"`
		Enabled  bool   `json:"enabled"`
		Contents string `json:"contents"`
	}
	data := func(s string) contents {
		return contents{Source: "data:;base64," + base64.StdEncoding.EncodeToString([]byte(s))}
	}

	ours := map[string]bool{"/etc/hostname": true, "/etc/localtime": true}
	files := []file{{Path: "/etc/hostname", Mode: 0o644, Overwrite: true, Contents: data(base.Hostname + "\n")}}
	var users []user
	for _, u := range p.Users {
		cu := user{Name: u.Name, Shell: u.Shell}
		for _, k := range u.SSHKeys {
			cu.SSHAuthorizedKeys = append(cu.SSHAuthorizedKeys, strings.TrimSpace(k))
		}
		users = append(users, cu)
		if u.Sudo {
			path := "/etc/sudoers.d/90-accrete-" + u.Name
			ours[path] = true
			files = append(files, file{Path: path, Mode: 0o440, Overwrite: true, Contents: data(u.Name + " ALL=(ALL) NOPASSWD: ALL\n")})
		}
	}
	for i, f := range p.Files {
		switch {
		case ours[f.Path]:
			return "", fmt.Errorf("ignition: files[%d]: %s is written by accrete from the manifest's other fields", i, f.Path)
		case f.Path == "/usr" || strings.HasPrefix(f.Path, "/usr/"):
			return "", fmt.Errorf("ignition: files[%d]: %s is under /usr, which is read-only on an image-based OS; use /etc or /var", i, f.Path)
		}
		mode := 0o644
		if f.Permissions != "" {
			m, err := strconv.ParseUint(f.Permissions, 8, 32)
			if err != nil {
				return "", fmt.Errorf("ignition: files[%d]: %q is not an octal mode", i, f.Permissions)
			}
			mode = int(m)
		}
		wf := file{Path: f.Path, Mode: mode, Overwrite: true, Contents: data(f.Content)}
		if f.Owner != "" {
			u, g, _ := strings.Cut(f.Owner, ":")
			wf.User = &node{Name: u}
			if g != "" {
				wf.Group = &node{Name: g}
			}
		}
		files = append(files, wf)
	}
	sort.SliceStable(files[1:], func(i, j int) bool { return files[1+i].Path < files[1+j].Path })

	storage := map[string]any{"files": files}
	if p.Timezone != "" {
		storage["links"] = []link{{Path: "/etc/localtime", Target: "../usr/share/zoneinfo/" + p.Timezone, Overwrite: true}}
	}
	doc := map[string]any{
		"ignition": map[string]any{"version": IgnitionVersion},
		"storage":  storage,
		"systemd": map[string]any{"units": []unit{{
			Name: FirstBootUnit, Enabled: true, Contents: firstBootUnit,
		}}},
	}
	if len(users) > 0 {
		doc["passwd"] = map[string]any{"users": users}
	}
	out, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", fmt.Errorf("ignition: render: %w", err)
	}
	return string(out) + "\n", nil
}

// firstBootUnit powers the machine off once, at the end of its first boot.
// The stamp is on /var, which survives the power-off; without it every boot
// would power off.
var firstBootUnit = strings.Join([]string{
	"[Unit]",
	"Description=accrete: end the first boot with a power-off, the sign that Ignition's config was applied",
	"ConditionPathExists=!" + FirstBootStamp,
	"After=multi-user.target",
	"",
	"[Service]",
	"Type=oneshot",
	"ExecStart=/usr/bin/mkdir -p /var/lib/accrete",
	"ExecStart=/usr/bin/touch " + FirstBootStamp,
	"ExecStart=/usr/bin/sync",
	"ExecStart=/usr/bin/systemctl --no-block poweroff",
	"",
	"[Install]",
	"WantedBy=multi-user.target",
	"",
}, "\n")
