package bootstrap

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestTheIgnitionConfigIsWhatTheFirstBootIsAcceptedOn(t *testing.T) {
	p, err := Parse(map[string]any{
		"users":    []any{map[string]any{"name": "core", "ssh_keys": []any{testKey}, "sudo": true}, map[string]any{"name": "ops", "shell": "/bin/bash"}},
		"timezone": "Europe/Rome",
		"files": []any{
			map[string]any{"path": "/etc/motd", "content": "managed by accrete\n", "permissions": "0644", "owner": "root:root"},
			map[string]any{"path": "/var/lib/app/env", "content": "A=1\n", "permissions": "0600", "owner": "ops"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	ign, err := RenderIgnition(Base{Hostname: "fcos-1"}, p)
	if err != nil {
		t.Fatal(err)
	}
	golden := "testdata/ignition.golden"
	if *update {
		_ = os.WriteFile(golden, []byte(ign), 0o644)
	}
	want, _ := os.ReadFile(golden)
	if ign != string(want) {
		t.Errorf("ignition config differs from %s:\n%s", golden, ign)
	}
	var doc struct {
		Ignition struct{ Version string }
		Passwd   struct {
			Users []struct {
				Name              string
				SSHAuthorizedKeys []string
			}
		}
		Storage struct {
			Files []struct {
				Path     string
				Mode     int
				Contents struct{ Source string }
			}
			Links []struct{ Path, Target string }
		}
		Systemd struct {
			Units []struct {
				Name     string
				Enabled  bool
				Contents string
			}
		}
	}
	if err := json.Unmarshal([]byte(ign), &doc); err != nil {
		t.Fatal(err)
	}
	if doc.Ignition.Version != IgnitionVersion || len(doc.Passwd.Users) != 2 || doc.Passwd.Users[0].SSHAuthorizedKeys[0] != testKey {
		t.Errorf("ignition %+v", doc)
	}
	paths := []string{}
	for _, f := range doc.Storage.Files {
		paths = append(paths, f.Path)
	}
	if strings.Join(paths, " ") != "/etc/hostname /etc/motd /etc/sudoers.d/90-accrete-core /var/lib/app/env" {
		t.Errorf("files %v", paths)
	}
	if doc.Storage.Files[0].Contents.Source != "data:;base64,ZmNvcy0xCg==" || doc.Storage.Files[2].Mode != 0o440 {
		t.Errorf("hostname or sudoers %+v", doc.Storage.Files)
	}
	if len(doc.Storage.Links) != 1 || doc.Storage.Links[0].Target != "../usr/share/zoneinfo/Europe/Rome" {
		t.Errorf("links %+v", doc.Storage.Links)
	}
	u := doc.Systemd.Units
	if len(u) != 1 || u[0].Name != FirstBootUnit || !u[0].Enabled || !strings.Contains(u[0].Contents, "ConditionPathExists=!"+FirstBootStamp) ||
		!strings.Contains(u[0].Contents, "systemctl --no-block poweroff") {
		t.Errorf("first boot unit %+v", u)
	}
	if strings.HasPrefix(ign, "#cloud-config") {
		t.Error("Ignition skips user-data that starts as cloud-config")
	}
}

func TestAnIgnitionConfigRefusesWhatItCannotHonour(t *testing.T) {
	for name, c := range map[string]struct {
		profile map[string]any
		want    string
	}{
		"packages": {map[string]any{"packages": []any{"vim"}}, "bootstrap packages are not supported"},
		"run":      {map[string]any{"run": []any{"echo hi"}}, "bootstrap run is not supported"},
		"usr":      {map[string]any{"files": []any{map[string]any{"path": "/usr/local/bin/x", "content": "x"}}}, "under /usr"},
		"hostname": {map[string]any{"files": []any{map[string]any{"path": "/etc/hostname", "content": "x"}}}, "written by accrete"},
		"sudoers":  {map[string]any{"users": []any{map[string]any{"name": "ops", "sudo": true}}, "files": []any{map[string]any{"path": "/etc/sudoers.d/90-accrete-ops", "content": "x"}}}, "written by accrete"},
	} {
		t.Run(name, func(t *testing.T) {
			p, err := Parse(c.profile)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := RenderIgnition(Base{Hostname: "h"}, p); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err %v, want %q", err, c.want)
			}
		})
	}
	if _, err := RenderIgnition(Base{Hostname: "h", Commands: [][]string{{"x"}}}, nil); err == nil {
		t.Error("enrolment commands were accepted")
	}
	if _, err := RenderIgnition(Base{Hostname: "not a label"}, nil); err == nil {
		t.Error("a bad hostname was accepted")
	}
}

func TestCloudConfigCanEndTheFirstBootWithAPowerOff(t *testing.T) {
	without, _ := Render(Base{Hostname: "h"}, nil)
	with, err := Render(Base{Hostname: "h", PowerOffWhenDone: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(without, "power_state") || !strings.Contains(with, "power_state:\n  condition: true\n  delay: now\n") || !strings.Contains(with, "mode: poweroff") {
		t.Errorf("without:\n%s\nwith:\n%s", without, with)
	}
}
