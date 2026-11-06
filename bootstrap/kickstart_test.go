package bootstrap

import (
	"flag"
	"os"
	"strings"
	"testing"
)

var update = flag.Bool("update", false, "rewrite the golden files")

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIGbm9wZW5zc2gta2V5LXYxAAAAAA ops@laptop"

func TestTheKickstartIsWhatTheInstallIsAcceptedOn(t *testing.T) {
	p, err := Parse(map[string]any{
		"users":    []any{map[string]any{"name": "ops", "ssh_keys": []any{testKey}, "sudo": true}},
		"packages": []any{"vim-enhanced", "qemu-guest-agent"},
		"timezone": "Europe/Rome",
		"files":    []any{map[string]any{"path": "/etc/motd", "content": "managed by accrete\n", "permissions": "0644", "owner": "root:root"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	ks, err := RenderKickstart(Install{Hostname: "ks-1", Disk: "sda"}, p)
	if err != nil {
		t.Fatal(err)
	}
	golden := "testdata/kickstart.golden"
	if *update {
		_ = os.WriteFile(golden, []byte(ks), 0o644)
	}
	want, _ := os.ReadFile(golden)
	if ks != string(want) {
		t.Errorf("kickstart differs from %s:\n%s", golden, ks)
	}
	for _, must := range []string{
		"rootpw --lock\n", "network --bootproto=dhcp --device=link --activate --hostname=ks-1\n",
		"ignoredisk --only-use=sda\n", "clearpart --all --initlabel --drives=sda\n",
		"user --name=ops\n", `sshkey --username=ops "` + testKey + `"`,
		"echo 'ops ALL=(ALL) NOPASSWD: ALL' > /etc/sudoers.d/90-accrete-ops\n",
		"\nqemu-guest-agent\n", "services --enabled=qemu-guest-agent,sshd\n", "\npoweroff\n",
	} {
		if !strings.Contains(ks, must) {
			t.Errorf("kickstart lacks %q", must)
		}
	}
	if strings.Contains(ks, "reboot") || strings.Count(ks, "qemu-guest-agent\n") != 1 {
		t.Error("the install must power off, and name the agent package once")
	}
}

func TestAKickstartRefusesWhatItCannotHonour(t *testing.T) {
	for name, c := range map[string]struct {
		profile map[string]any
		want    string
	}{
		"run":           {map[string]any{"run": []any{"echo hi"}}, "bootstrap run is not supported"},
		"quoted key":    {map[string]any{"users": []any{map[string]any{"name": "ops", "ssh_keys": []any{testKey + ` "x"`}}}}, "quote or a backslash"},
		"path new line": {map[string]any{"files": []any{map[string]any{"path": "/etc/a\nb", "content": "x"}}}, "control character"},
	} {
		p, err := Parse(c.profile)
		if err == nil {
			_, err = RenderKickstart(Install{Hostname: "ks-1", Disk: "sda"}, p)
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: %v, want %q", name, err, c.want)
		}
	}
	if _, err := RenderKickstart(Install{Hostname: "ks_1", Disk: "sda"}, nil); err == nil {
		t.Error("a bad hostname was accepted")
	}
	if ks, err := RenderKickstart(Install{Hostname: "ks-1", Disk: "sda"}, nil); err != nil || strings.Contains(ks, "%post") {
		t.Errorf("no profile: %v\n%s", err, ks)
	}
}
