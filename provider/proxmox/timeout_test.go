package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Three kinds of request, three deadlines. A plain call gets CallTimeout, an
// upload gets the budget its caller gave it, and a task wait ends when its
// own timeout says so even if a status read is hanging.

func hangingServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(2 * time.Second):
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestNewClientKeepsFifteenSecondsForPlainCalls(t *testing.T) {
	c, err := NewClient("https://pve.example:8006/", "root@pam!t=s", false)
	if err != nil {
		t.Fatal(err)
	}
	if c.callTimeout() != 15*time.Second {
		t.Errorf("call timeout = %s, want 15s", c.callTimeout())
	}
	if c.HTTP.Timeout != 0 {
		t.Errorf("HTTP.Timeout = %s; a client-wide timeout would also cap uploads", c.HTTP.Timeout)
	}
	if c.BaseURL != "https://pve.example:8006" || c.Token != "PVEAPIToken=root@pam!t=s" {
		t.Errorf("client = %q %q, want the slash trimmed and the token prefixed", c.BaseURL, c.Token)
	}
	if _, err := NewClient("", "t", false); err == nil {
		t.Error("a client without base_url was built")
	}
	if _, err := NewClient("https://pve", "", false); err == nil {
		t.Error("a client without api_token was built")
	}
}

func TestAPlainCallThatHangsIsCutOff(t *testing.T) {
	srv := hangingServer(t)
	c := &Client{BaseURL: srv.URL, Token: "PVEAPIToken=test", HTTP: srv.Client(), CallTimeout: 50 * time.Millisecond}

	start := time.Now()
	err := c.Get(context.Background(), "/version", nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want a deadline", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s, want about CallTimeout", took)
	}
}

func TestAnUploadIsNotHeldToThePlainCallDeadline(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/storage/local/upload") {
			http.NotFound(w, r)
			return
		}
		time.Sleep(200 * time.Millisecond) // slow storage
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": nil})
	}))
	t.Cleanup(srv.Close)
	c := &Client{BaseURL: srv.URL, Token: "PVEAPIToken=test", HTTP: srv.Client(), CallTimeout: 50 * time.Millisecond, TaskPoll: time.Millisecond}

	if err := c.StorageUpload(context.Background(), "pve1", "local", "iso", "seed.iso", []byte("seed"), 5*time.Second); err != nil {
		t.Fatalf("a slow but healthy upload failed: %v", err)
	}
}

func TestAnUploadIsHeldToItsOwnBudget(t *testing.T) {
	srv := hangingServer(t)
	c := &Client{BaseURL: srv.URL, Token: "PVEAPIToken=test", HTTP: srv.Client(), TaskPoll: time.Millisecond}

	start := time.Now()
	if err := c.StorageUpload(context.Background(), "pve1", "local", "iso", "seed.iso", []byte("seed"), 50*time.Millisecond); err == nil {
		t.Fatal("an upload that never answered succeeded")
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s, want about the 50ms budget", took)
	}
}

func TestATaskWaitEndsOnItsOwnDeadlineWhenAPollHangs(t *testing.T) {
	srv := hangingServer(t)
	c := &Client{BaseURL: srv.URL, Token: "PVEAPIToken=test", HTTP: srv.Client(), TaskPoll: time.Millisecond}

	start := time.Now()
	err := c.TaskWait(context.Background(), "pve1", testUPID, 100*time.Millisecond)
	var timeout *TaskTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v (%T), want *TaskTimeoutError", err, err)
	}
	if took := time.Since(start); took > time.Second {
		t.Errorf("took %s, want about the 100ms wait, not the call timeout", took)
	}
}
