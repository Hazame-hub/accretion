package proxmox

import (
	"fmt"
	"strings"
	"testing"
)

func TestCheckToken(t *testing.T) {
	for _, tok := range []string{
		"sybil@pve!sybil=0d1e2f3a-4b5c-6d7e-8f90-a1b2c3d4e5f6",
		"PVEAPIToken=root@pam!accrete=s3cret",
	} {
		if err := CheckToken(tok); err != nil {
			t.Errorf("CheckToken(%q) = %v", tok, err)
		}
	}
	for _, tok := range []string{
		"0d1e2f3a-4b5c-6d7e-8f90-a1b2c3d4e5f6", // the secret alone
		"sybil@pve!sybil",                      // the id alone
		"sybil@pve!sybil=",
		"sybil@pve=s3cret",
		"@pve!sybil=s3cret",
		"sybil!sybil=s3cret",
		"PVEAPIToken=",
		"",
	} {
		err := CheckToken(tok)
		if err == nil {
			t.Errorf("CheckToken(%q) = nil", tok)
			continue
		}
		if tok != "" && strings.Contains(err.Error(), tok) {
			t.Errorf("CheckToken(%q) quotes the token: %v", tok, err)
		}
	}
}

func TestExplain(t *testing.T) {
	if got := Explain(fmt.Errorf("listing: %w", &APIError{Code: 401})); !strings.Contains(got, "refused the token") {
		t.Errorf("401: %q", got)
	}
	if got := Explain(&APIError{Code: 403}); !strings.Contains(got, "privilege") {
		t.Errorf("403: %q", got)
	}
	if got := Explain(&APIError{Code: 500}); got != "" {
		t.Errorf("500: %q", got)
	}
	if got := Explain(fmt.Errorf("dial: refused")); got != "" {
		t.Errorf("network: %q", got)
	}
}
