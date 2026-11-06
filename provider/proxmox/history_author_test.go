package proxmox

import (
	"testing"
	"time"

	"github.com/hazame-hub/accretion/provider"
)

// On PVE 9.1 (lab, 2026-11-02) a token's task lists "user" as the bare user
// and the token only in the UPID; reading "user" blamed accrete's own config
// task on accrete@pve.
func TestTaskAuthorNamesTheTokenFromTheUPID(t *testing.T) {
	const self = "accrete@pve!proof"
	cases := []struct {
		user, upid, tokenid, want string
		self                      bool
	}{
		{"accrete@pve", "UPID:flk-vega-01:0001:0002:69071F23:qmconfig:116:accrete@pve!proof:", "", "accrete@pve!proof", true},
		{"accrete@pve", "", "proof", "accrete@pve!proof", true},
		{"accrete@pve", "", "", "accrete@pve", true}, // cannot tell: never blamed
		{"root@pam", "UPID:flk-vega-01:0001:0002:69071F23:qmshutdown:117:root@pam:", "", "root@pam", false},
		{"accrete@pve", "UPID:flk-vega-01:0001:0002:69071F23:qmconfig:116:accrete@pve!other:", "", "accrete@pve!other", false},
	}
	for _, c := range cases {
		got := taskAuthor(c.user, c.upid, c.tokenid)
		if got != c.want || isSelf(got, self) != c.self {
			t.Errorf("taskAuthor(%q, %q, %q) = %q self %v, want %q self %v", c.user, c.upid, c.tokenid, got, isSelf(got, self), c.want, c.self)
		}
	}
}

// Lab, 2026-11-05: accrete's qmreboot is followed by a qmstart Proxmox logs
// under root@pam. That start is accrete's, not root@pam's.
func TestARebootsOwnStartIsGivenToWhoRebooted(t *testing.T) {
	t0 := time.Date(2026, 11, 5, 21, 17, 57, 0, time.UTC)
	changes := []provider.Change{
		{At: t0.Add(2 * time.Second), By: "root@pam", What: "qmstart", Kind: provider.ChangePower},
		{At: t0, By: "accrete@pve!proof", What: "qmreboot", Kind: provider.ChangePower, Self: true},
		{At: t0.Add(-time.Hour), By: "root@pam", What: "qmstart", Kind: provider.ChangePower},
	}
	markRebootStarts(changes)
	if !changes[0].Self || changes[0].By != "accrete@pve!proof" {
		t.Errorf("the reboot's start: %+v", changes[0])
	}
	if changes[2].Self || changes[2].By != "root@pam" {
		t.Errorf("an unrelated start was reattributed: %+v", changes[2])
	}
}
