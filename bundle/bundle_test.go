package bundle

import (
	"bytes"
	"encoding/base64"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const webKS = `network --hostname=web-01
rootpw --lock
%packages
@^minimal-environment
%end
`

const dbKS = `network --hostname=db-01
rootpw --lock
`

func twoProfiles() []Profile {
	return []Profile{
		{Name: "web-01", Serials: []string{"CZJ1234ABC"}, MACs: []string{"52:54:00:AA:00:01"}, Kickstart: []byte(webKS)},
		{Name: "db-01", UUIDs: []string{"4c4c4544-0042-3510-8051-b4c04f4e4d32"}, Kickstart: []byte(dbKS)},
	}
}

func TestBuildGolden(t *testing.T) {
	b, err := Build(twoProfiles())
	if err != nil {
		t.Fatal(err)
	}
	golden := filepath.Join("testdata", "two-machines.ks.cfg")
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(golden, b.KS, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("%v (run with UPDATE_GOLDEN=1 to write it)", err)
	}
	if !bytes.Equal(b.KS, want) {
		t.Fatalf("ks.cfg differs from %s; run with UPDATE_GOLDEN=1 and review the diff", golden)
	}
	if len(b.SHA256) != 64 || len(b.ProfileSHA256) != 2 {
		t.Fatalf("digests: %q %v", b.SHA256, b.ProfileSHA256)
	}
}

func TestBuildIsDeterministic(t *testing.T) {
	a, _ := Build(twoProfiles())
	p := twoProfiles()
	p[0], p[1] = p[1], p[0]
	b, _ := Build(p)
	if a.SHA256 != b.SHA256 {
		t.Fatal("the same profiles in another order gave other bytes")
	}
}

// No profile body may leave %pre early: a profile's own %end must travel
// encoded, so the only section markers are the bundle's own.
func TestProfilesCannotEndPre(t *testing.T) {
	b, err := Build(twoProfiles())
	if err != nil {
		t.Fatal(err)
	}
	var markers []string
	for _, l := range strings.Split(string(b.KS), "\n") {
		if strings.HasPrefix(l, "%") {
			markers = append(markers, l)
		}
	}
	want := []string{"%pre --erroronfail --log=/tmp/accrete-pre.log", "%end", "%include " + IncludePath}
	if strings.Join(markers, "|") != strings.Join(want, "|") {
		t.Fatalf("section lines = %q, want %q", markers, want)
	}
}

func TestBuildRefuses(t *testing.T) {
	ok := func() Profile { return Profile{Name: "m-1", Serials: []string{"SER1"}, Kickstart: []byte("text\n")} }
	cases := []struct {
		name    string
		in      []Profile
		mention []string
	}{
		{"nothing", nil, []string{"no profile"}},
		{"no selector", []Profile{{Name: "m-1", Kickstart: []byte("x")}}, []string{`"m-1"`, "no UUID, serial or MAC"}},
		{"bad name", []Profile{{Name: "Web_01", Serials: []string{"S"}, Kickstart: []byte("x")}}, []string{`"Web_01"`}},
		{"placeholder serial", []Profile{{Name: "m-1", Serials: []string{"To Be Filled By O.E.M."}, Kickstart: []byte("x")}}, []string{`"m-1"`, "To Be Filled By O.E.M."}},
		{"quote in serial", []Profile{{Name: "m-1", Serials: []string{"A'B"}, Kickstart: []byte("x")}}, []string{`"m-1"`, "A'B"}},
		{"placeholder uuid", []Profile{{Name: "m-1", UUIDs: []string{"03000200-0400-0500-0006-000700080009"}, Kickstart: []byte("x")}}, []string{`"m-1"`, "placeholder"}},
		{"zero mac", []Profile{{Name: "m-1", MACs: []string{"00:00:00:00:00:00"}, Kickstart: []byte("x")}}, []string{`"m-1"`, "00:00:00:00:00:00"}},
		{"bad mac", []Profile{{Name: "m-1", MACs: []string{"52-54-00-aa-00-01"}, Kickstart: []byte("x")}}, []string{"52-54-00-aa-00-01"}},
		{"empty kickstart", []Profile{{Name: "m-1", Serials: []string{"S1"}, Kickstart: []byte(" \n")}}, []string{`"m-1"`, "empty"}},
		{"secret", []Profile{{Name: "m-1", Serials: []string{"S1"}, Kickstart: []byte("rootpw {{ secret.root }}\n")}}, []string{`"m-1"`, "secret"}},
		{"same name", []Profile{ok(), func() Profile { p := ok(); p.Serials = []string{"SER2"}; return p }()}, []string{`"m-1"`}},
		{"shared serial", []Profile{ok(), {Name: "m-2", Serials: []string{"SER1"}, Kickstart: []byte("x")}}, []string{`"m-1"`, `"m-2"`, "SER1"}},
		{"shared mac, other case", []Profile{
			{Name: "m-1", MACs: []string{"52:54:00:aa:00:01"}, Kickstart: []byte("x")},
			{Name: "m-2", MACs: []string{"52:54:00:AA:00:01"}, Kickstart: []byte("x")},
		}, []string{`"m-1"`, `"m-2"`}},
		// The same UUID written once canonical and once byte-swapped is
		// one machine.
		{"shared uuid, swapped form", []Profile{
			{Name: "m-1", UUIDs: []string{"4c4c4544-0042-3510-8051-b4c04f4e4d32"}, Kickstart: []byte("x")},
			{Name: "m-2", UUIDs: []string{"44454c4c-4200-1035-8051-b4c04f4e4d32"}, Kickstart: []byte("x")},
		}, []string{`"m-1"`, `"m-2"`, "UUID"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Build(c.in)
			if err == nil {
				t.Fatal("built; want a refusal")
			}
			for _, m := range c.mention {
				if !strings.Contains(err.Error(), m) {
					t.Errorf("error %q does not mention %q", err, m)
				}
			}
		})
	}
}

// machine is what a booting machine's board and cards report.
type machine struct {
	uuid, serial string
	macs         []string
}

// runPre runs the %pre section of ks against fake /sys trees with every
// shell found on this host, the way Anaconda would, and returns the exit
// code, the profile written and stderr per shell.
func runPre(t *testing.T, ks []byte, m machine) (code int, written, stderr string) {
	t.Helper()
	s := string(ks)
	start := strings.Index(s, "%pre")
	end := strings.Index(s, "\n%end\n")
	if start < 0 || end < 0 {
		t.Fatal("no pre section")
	}
	script := s[strings.Index(s[start:], "\n")+start+1 : end+1]

	dir := t.TempDir()
	dmi := filepath.Join(dir, "dmi")
	net := filepath.Join(dir, "net")
	out := filepath.Join(dir, "machine.ks")
	must(t, os.MkdirAll(dmi, 0o755))
	must(t, os.WriteFile(filepath.Join(dmi, "product_uuid"), []byte(m.uuid+"\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(dmi, "product_serial"), []byte(m.serial+"\n"), 0o644))
	for i, mac := range append([]string{"00:00:00:00:00:00"}, m.macs...) { // lo first
		d := filepath.Join(net, "if"+string(rune('a'+i)))
		must(t, os.MkdirAll(d, 0o755))
		must(t, os.WriteFile(filepath.Join(d, "address"), []byte(mac+"\n"), 0o644))
	}

	shells := []string{}
	for _, sh := range []string{"bash", "dash", "sh"} {
		if _, err := exec.LookPath(sh); err == nil {
			shells = append(shells, sh)
		}
	}
	if len(shells) == 0 {
		t.Skip("no POSIX shell on this host")
	}
	first := true
	for _, sh := range shells {
		_ = os.Remove(out)
		cmd := exec.Command(sh, "-c", script)
		cmd.Env = append(os.Environ(), "ACCRETE_DMI="+dmi, "ACCRETE_NET="+net, "ACCRETE_OUT="+out)
		if installerPath != "" {
			cmd.Env = append(cmd.Env, "PATH="+installerPath)
		}
		var e bytes.Buffer
		cmd.Stderr = &e
		err := cmd.Run()
		c := 0
		if x, ok := err.(*exec.ExitError); ok {
			c = x.ExitCode()
		} else if err != nil {
			t.Fatal(err)
		}
		w, _ := os.ReadFile(out)
		if first {
			code, written, stderr, first = c, string(w), e.String(), false
		} else if c != code || string(w) != written {
			t.Fatalf("%s and %s disagree: exit %d vs %d", shells[0], sh, code, c)
		}
	}
	return code, written, stderr
}

// installerPath, when set, is the only PATH %pre runs with: the tools the
// RHEL installer image is known to ship.
var installerPath string

// TestPreNeedsOnlyWhatTheInstallerShips runs %pre with a PATH holding only
// commands the RHEL 9.6 installer image has. base64 is not among them: the
// first lab run stopped on "base64: command not found".
func TestPreNeedsOnlyWhatTheInstallerShips(t *testing.T) {
	dir := t.TempDir()
	for _, tool := range []string{"sh", "tr", "sed", "sort", "cat", "python3"} {
		p, err := exec.LookPath(tool)
		if err != nil {
			t.Skipf("no %s on this host", tool)
		}
		must(t, os.Symlink(p, filepath.Join(dir, tool)))
	}
	installerPath = dir
	defer func() { installerPath = "" }()
	b, _ := Build(twoProfiles())
	code, got, stderr := runPre(t, b.KS, machine{uuid: "4c4c4544-0042-3510-8051-b4c04f4e4d32", serial: ""})
	if code != 0 || got != dbKS {
		t.Fatalf("exit %d, wrote %q: %s", code, got, stderr)
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func TestPreSelectsTheMachinesProfile(t *testing.T) {
	b, err := Build(twoProfiles())
	if err != nil {
		t.Fatal(err)
	}
	cases := []struct {
		name string
		m    machine
		want string
	}{
		{"by serial", machine{uuid: "11111111-2222-3333-4444-555555555555", serial: "CZJ1234ABC", macs: []string{"aa:bb:cc:00:00:01"}}, webKS},
		{"by serial with padding", machine{uuid: "11111111-2222-3333-4444-555555555555", serial: "  CZJ1234ABC  "}, webKS},
		{"by MAC on the second card, upper case", machine{serial: "OTHER", macs: []string{"aa:bb:cc:00:00:01", "52:54:00:AA:00:01"}}, webKS},
		{"by UUID", machine{uuid: "4C4C4544-0042-3510-8051-B4C04F4E4D32", serial: "X"}, dbKS},
		{"by UUID read byte-swapped", machine{uuid: "44454c4c-4200-1035-8051-b4c04f4e4d32", serial: "X"}, dbKS},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, got, stderr := runPre(t, b.KS, c.m)
			if code != 0 {
				t.Fatalf("exit %d: %s", code, stderr)
			}
			if got != c.want {
				t.Fatalf("wrote %q, want %q", got, c.want)
			}
		})
	}
}

func TestPreStopsAnUnknownMachine(t *testing.T) {
	b, _ := Build(twoProfiles())
	code, got, stderr := runPre(t, b.KS, machine{uuid: "11111111-2222-3333-4444-555555555555", serial: "NOPE", macs: []string{"aa:bb:cc:00:00:09"}})
	if code == 0 || got != "" {
		t.Fatalf("exit %d, wrote %q; want a stop with nothing written", code, got)
	}
	for _, m := range []string{"matches no profile", "serial=NOPE", "aa:bb:cc:00:00:09", "db-01, web-01"} {
		if !strings.Contains(stderr, m) {
			t.Errorf("stderr %q does not mention %q", stderr, m)
		}
	}
}

func TestPreStopsAnAmbiguousMachine(t *testing.T) {
	b, _ := Build(twoProfiles())
	// web-01's serial and db-01's UUID: two different profiles.
	code, got, stderr := runPre(t, b.KS, machine{uuid: "4c4c4544-0042-3510-8051-b4c04f4e4d32", serial: "CZJ1234ABC"})
	if code == 0 || got != "" {
		t.Fatalf("exit %d, wrote %q; want a stop", code, got)
	}
	if !strings.Contains(stderr, "more than one profile") || !strings.Contains(stderr, "web-01") || !strings.Contains(stderr, "db-01") {
		t.Fatalf("stderr %q", stderr)
	}
}

// A machine that matches one profile by two selectors is not ambiguous.
func TestPreTwoSelectorsOneProfile(t *testing.T) {
	b, _ := Build(twoProfiles())
	code, got, stderr := runPre(t, b.KS, machine{serial: "CZJ1234ABC", macs: []string{"52:54:00:aa:00:01"}})
	if code != 0 || got != webKS {
		t.Fatalf("exit %d, wrote %q: %s", code, got, stderr)
	}
}

func TestProfileDigestsNameTheBytes(t *testing.T) {
	b, _ := Build(twoProfiles())
	if !strings.Contains(string(b.KS), base64.StdEncoding.EncodeToString([]byte(dbKS))) {
		t.Fatal("db-01's profile is not in the file as written")
	}
	if b.ProfileSHA256["web-01"] == b.ProfileSHA256["db-01"] {
		t.Fatal("two different profiles share a digest")
	}
}
