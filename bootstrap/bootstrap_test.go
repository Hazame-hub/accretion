package bootstrap

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

const key = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIK0wmN/Cr3JXqmLW7u+g9pTh+wyqDHpSQEIQczXkVx9q ops@example"

func profile() map[string]any {
	return map[string]any{
		"users":    []any{map[string]any{"name": "deploy", "ssh_keys": []any{key}, "sudo": true}},
		"packages": []any{"htop", "curl"},
		"timezone": "Europe/Paris",
		"files":    []any{map[string]any{"path": "/etc/motd", "content": "hello {{ name }}\n", "permissions": "644"}},
		"run":      []any{"systemctl enable --now nginx"},
	}
}

func TestAProfileRendersAfterSybilsOwnEnrollment(t *testing.T) {
	p, err := Parse(profile())
	if err != nil {
		t.Fatal(err)
	}
	out, err := Render(Base{Hostname: "web-1", Packages: []string{"curl", "qemu-guest-agent"},
		Commands: [][]string{{"sh", "-c", "enroll"}}}, p)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out, "#cloud-config\n") {
		t.Fatalf("no header:\n%s", out)
	}
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatalf("not YAML: %v\n%s", err, out)
	}
	run := doc["runcmd"].([]any)
	if first := run[0].([]any); first[2] != "enroll" {
		t.Fatalf("Sybil's enrollment is not first: %v", run)
	}
	if last := run[1].([]any); last[2] != "systemctl enable --now nginx" {
		t.Fatalf("the profile's command: %v", run)
	}
	if pk := doc["packages"].([]any); len(pk) != 3 {
		t.Fatalf("packages = %v, want curl once", pk)
	}
	users := doc["users"].([]any)
	if users[0] != "default" || users[1].(map[string]any)["sudo"] != "ALL=(ALL) NOPASSWD:ALL" {
		t.Fatalf("users = %v", users)
	}
	wf := doc["write_files"].([]any)[0].(map[string]any)
	if wf["permissions"] != "0644" || wf["content"] != "hello {{ name }}\n" {
		t.Fatalf("file = %v", wf)
	}
}

// A value that would break out of the document in a template is just a
// value when the document is marshalled.
func TestAValueCannotInjectIntoTheDocument(t *testing.T) {
	p, err := Parse(map[string]any{"files": []any{map[string]any{
		"path": "/etc/x", "content": "a\nruncmd:\n  - [ rm, -rf, / ]\n"}}})
	if err != nil {
		t.Fatal(err)
	}
	out, _ := Render(Base{Hostname: "h"}, p)
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(out), &doc); err != nil {
		t.Fatal(err)
	}
	if _, injected := doc["runcmd"]; injected {
		t.Fatalf("content became a key:\n%s", out)
	}
}

func TestAProfileIsCheckedFieldByField(t *testing.T) {
	bad := map[string]map[string]any{
		"unknown field":   {"userz": []any{}},
		"user name":       {"users": []any{map[string]any{"name": "Bad Name"}}},
		"root":            {"users": []any{map[string]any{"name": "root"}}},
		"private key":     {"users": []any{map[string]any{"name": "a", "ssh_keys": []any{"-----BEGIN OPENSSH PRIVATE KEY-----"}}}},
		"password field":  {"users": []any{map[string]any{"name": "a", "password": "x"}}},
		"package":         {"packages": []any{"htop; rm -rf /"}},
		"timezone":        {"timezone": "Europe/../../etc"},
		"relative path":   {"files": []any{map[string]any{"path": "etc/x", "content": ""}}},
		"dotdot path":     {"files": []any{map[string]any{"path": "/etc/../root/x", "content": ""}}},
		"mode":            {"files": []any{map[string]any{"path": "/x", "content": "", "permissions": "rwx"}}},
		"empty command":   {"run": []any{"  "}},
		"duplicate users": {"users": []any{map[string]any{"name": "a"}, map[string]any{"name": "a"}}},
	}
	for name, v := range bad {
		if _, err := Parse(v); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if p, err := Parse(nil); p != nil || err != nil {
		t.Errorf("nil: %v %v", p, err)
	}
	if p, err := Parse(map[string]any{}); p != nil || err != nil {
		t.Errorf("empty: %v %v", p, err)
	}
}

// A password is taken only as a crypt hash, and every renderer writes it as one.
func TestAUserPasswordIsAHashNeverClearText(t *testing.T) {
	const hash = "$6$rounds=5000$abcdefgh$" + "Zx2T4ftQjN0G9nqJ2s6c2rjU8q1C3rZ0v9iH1yq8kM4b7m3Xc1R2a5sQ0p9o8n7l6k5j4h3g2f1e0d9c8b7a6"
	for _, bad := range []string{"hunter2", "$1$abc$def", "$6$salt$short"} {
		if _, err := Parse(map[string]any{"users": []any{map[string]any{"name": "ops", "password_hash": bad}}}); err == nil {
			t.Errorf("%q was taken as a password hash", bad)
		}
	}
	for _, good := range []string{hash, "$y$j9T$F5Jx5fExrKuPp53xLKQ..1$X3DX6M94c7o.9agCG9G317fhZg9SqC.5i5rd.RhAtQ7"} {
		p, err := Parse(map[string]any{"users": []any{map[string]any{"name": "ops", "password_hash": good}}})
		if err != nil {
			t.Fatalf("%q: %v", good, err)
		}
		ci, err := Render(Base{Hostname: "h"}, p)
		if err != nil || !strings.Contains(ci, "passwd: "+good) || !strings.Contains(ci, "lock_passwd: false") {
			t.Errorf("cloud-init: %v\n%s", err, ci)
		}
		ign, err := RenderIgnition(Base{Hostname: "h"}, p)
		if err != nil || !strings.Contains(ign, `"passwordHash": "`+good+`"`) {
			t.Errorf("ignition: %v\n%s", err, ign)
		}
	}
}
