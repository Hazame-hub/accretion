package proxmox

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/hazame-hub/accretion/provider"
)

// refusingServer answers every request with one raw status line and body.
// Proxmox words its refusals in the reason phrase, which net/http's
// WriteHeader cannot set, so the response is written by hand.
func refusingServer(t *testing.T, statusLine, body string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, buf, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack: %v", err)
			return
		}
		defer conn.Close()
		fmt.Fprintf(buf, "HTTP/1.1 %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s", statusLine, len(body), body)
		buf.Flush()
	}))
	t.Cleanup(srv.Close)
	return &Client{BaseURL: srv.URL, Token: "PVEAPIToken=t", HTTP: srv.Client()}
}

func TestAPIErrorKeepsTheReasonProxmoxGives(t *testing.T) {
	c := refusingServer(t, "403 Permission check failed (/nodes/pve1, Sys.Syslog)", `{"data":null}`)
	err := c.Get(context.Background(), "/nodes/pve1/syslog", nil)

	var ae *APIError
	if !errors.As(err, &ae) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if ae.Reason != "Permission check failed (/nodes/pve1, Sys.Syslog)" {
		t.Errorf("reason = %q", ae.Reason)
	}
	want := `proxmox: API error 403 (Permission check failed (/nodes/pve1, Sys.Syslog)): {"data":null}`
	if err.Error() != want {
		t.Errorf("error = %q\nwant    %q", err.Error(), want)
	}
}

func TestAPIErrorMessageUnchangedForAStandardReason(t *testing.T) {
	// What every golden file recorded before Reason existed.
	c := refusingServer(t, "401 Unauthorized", `{"data":null,"message":"authentication failure"}`)
	err := c.Get(context.Background(), "/version", nil)
	want := `proxmox: API error 401: {"data":null,"message":"authentication failure"}`
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	if !errors.Is(err, provider.ErrUnauthorized) {
		t.Errorf("a 401 must still match provider.ErrUnauthorized")
	}
}

func TestMissingPrivilege(t *testing.T) {
	cases := []struct {
		name       string
		statusLine string
		wantOK     bool
		path, priv string
	}{
		{"syslog on a node", "403 Permission check failed (/nodes/flk-vega-01, Sys.Syslog)", true, "/nodes/flk-vega-01", "Sys.Syslog"},
		{"audit on a guest", "403 Permission check failed (/vms/114, VM.Audit)", true, "/vms/114", "VM.Audit"},
		{"bad token", "401 authentication failure", false, "", ""},
		{"plain forbidden", "403 Forbidden", false, "", ""},
		{"other 403 wording", "403 Permission check failed", false, "", ""},
		{"node unreachable", "595 No route to host", false, "", ""},
		{"node offline", "500 hostname lookup 'pve2' failed - failed to get address info", false, "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := refusingServer(t, tc.statusLine, `{"data":null}`)
			err := c.Get(context.Background(), "/anything", nil)
			path, priv, ok := MissingPrivilege(err)
			if ok != tc.wantOK || path != tc.path || priv != tc.priv {
				t.Errorf("MissingPrivilege = (%q, %q, %v), want (%q, %q, %v)", path, priv, ok, tc.path, tc.priv, tc.wantOK)
			}
		})
	}
}

func TestMissingPrivilegeIgnoresErrorsThatAreNotRefusals(t *testing.T) {
	if _, _, ok := MissingPrivilege(errors.New("proxmox: request /x: connection refused")); ok {
		t.Error("a transport error is not a missing privilege")
	}
	if _, _, ok := MissingPrivilege(nil); ok {
		t.Error("nil is not a missing privilege")
	}
}
