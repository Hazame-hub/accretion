package proxmox

import "testing"

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
