package bootstrap

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// renderedCommand is the AllowGuestFileReads entry as cloud-init reads it
// back from rendered user-data, so the quoting is what is under test too.
func renderedCommand(t *testing.T) []string {
	t.Helper()
	out, err := Render(Base{Hostname: "web-1", Commands: [][]string{
		{"systemctl", "enable", "--now", "qemu-guest-agent"},
		AllowGuestFileReads(),
	}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		Runcmd [][]string `yaml:"runcmd"`
	}
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("user-data is not YAML: %v\n%s", err, out)
	}
	if len(doc.Runcmd) != 2 || !reflect.DeepEqual(doc.Runcmd[1], AllowGuestFileReads()) {
		t.Fatalf("runcmd = %q", doc.Runcmd)
	}
	return doc.Runcmd[1]
}

// runAllowGuestFileReads runs the command against conf (absent when empty)
// with a fake systemctl on PATH that records its arguments. It returns the
// file afterwards and what systemctl was asked to do.
func runAllowGuestFileReads(t *testing.T, conf string) (string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		// MSYS sh mangles the Windows paths in the fake systemctl.
		t.Skip("needs a POSIX system")
	}
	for _, tool := range []string{"sh", "awk", "cmp"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("needs %s: %v", tool, err)
		}
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "qemu-ga")
	if conf != "" {
		if err := os.WriteFile(path, []byte(conf), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "bin")
	log := filepath.Join(dir, "systemctl.log")
	if err := os.Mkdir(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	fake := "#!/bin/sh\necho \"$@\" >> " + log + "\n"
	if err := os.WriteFile(filepath.Join(bin, "systemctl"), []byte(fake), 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := renderedCommand(t)
	c := exec.Command(cmd[0], cmd[1:]...)
	c.Env = append(os.Environ(), "ACCRETION_QEMU_GA_CONFIG="+path, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("the command failed (it must always exit 0): %v\n%s", err, b)
	}
	got, _ := os.ReadFile(path)
	calls, _ := os.ReadFile(log)
	if _, err := os.Stat(path + ".accretion"); err == nil {
		t.Error("the temporary file was left behind")
	}
	return string(got), strings.TrimSpace(string(calls))
}

// Rocky / RHEL 9: an allow-list that leaves the file commands out.
func TestTheAgentAllowListGainsTheReadCommands(t *testing.T) {
	conf := "# This is a systemd environment file, not a shell script.\n" +
		"FILTER_RPC_ARGS=\"--allow-rpcs=guest-sync-delimited,guest-sync,guest-ping,guest-get-osinfo\"\n" +
		"FSFREEZE_HOOK_PATHNAME=/etc/qemu-ga/fsfreeze-hook\n"
	got, calls := runAllowGuestFileReads(t, conf)
	want := "# This is a systemd environment file, not a shell script.\n" +
		"FILTER_RPC_ARGS=\"--allow-rpcs=guest-sync-delimited,guest-sync,guest-ping,guest-get-osinfo,guest-file-open,guest-file-read,guest-file-close\"\n" +
		"FSFREEZE_HOOK_PATHNAME=/etc/qemu-ga/fsfreeze-hook\n"
	if got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
	if calls != "restart qemu-guest-agent" {
		t.Errorf("systemctl calls = %q", calls)
	}
}

// RHEL 8 style: a block-list naming them. Write, seek and exec stay blocked.
func TestTheAgentBlockListLosesOnlyTheReadCommands(t *testing.T) {
	conf := "BLACKLIST_RPC=guest-file-open,guest-file-close,guest-file-read,guest-file-write,guest-file-seek,guest-exec\n" +
		"FILTER_RPC_ARGS=\"--blacklist=${BLACKLIST_RPC}\"\n"
	got, calls := runAllowGuestFileReads(t, conf)
	want := "BLACKLIST_RPC=guest-file-write,guest-file-seek,guest-exec\n" +
		"FILTER_RPC_ARGS=\"--blacklist=${BLACKLIST_RPC}\"\n"
	if got != want {
		t.Fatalf("config =\n%s\nwant\n%s", got, want)
	}
	if calls != "restart qemu-guest-agent" {
		t.Errorf("systemctl calls = %q", calls)
	}
}

// A literal list given straight to the flag is handled the same way.
func TestTheAgentInlineBlockListLosesOnlyTheReadCommands(t *testing.T) {
	got, _ := runAllowGuestFileReads(t, "FILTER_RPC_ARGS=\"--block-rpcs=guest-exec,guest-file-open,guest-file-read\"\n")
	if want := "FILTER_RPC_ARGS=\"--block-rpcs=guest-exec\"\n"; got != want {
		t.Fatalf("config = %q, want %q", got, want)
	}
}

// Already allowed, commented out, or no file at all: nothing changes and the
// agent is not restarted.
func TestTheAgentIsLeftAloneWhenNothingBlocksTheReads(t *testing.T) {
	for name, conf := range map[string]string{
		"already allowed": "FILTER_RPC_ARGS=\"--allow-rpcs=guest-ping,guest-file-open,guest-file-read,guest-file-close\"\n",
		"commented out":   "#BLACKLIST_RPC=guest-file-open,guest-file-read\nFSFREEZE_HOOK_PATHNAME=/x\n",
	} {
		got, calls := runAllowGuestFileReads(t, conf)
		if got != conf || calls != "" {
			t.Errorf("%s: config = %q, systemctl = %q", name, got, calls)
		}
	}
	if got, calls := runAllowGuestFileReads(t, ""); got != "" || calls != "" {
		t.Errorf("no file: config = %q, systemctl = %q", got, calls)
	}
}
