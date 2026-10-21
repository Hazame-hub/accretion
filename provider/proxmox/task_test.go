package proxmox

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// A task is the unit of "done" on Proxmox, and waiting for one is where three
// different answers used to arrive as one error string: the task failed, the
// wait ran out while the task was still running, and the task could not be
// asked about at all. They call for different next steps — a failure is a
// failure, a timeout is an unknown outcome to check before retrying — so they
// are different errors.

type taskSim struct {
	mu         sync.Mutex
	polls      int    // reads the handler has served, which orders the answers below
	issued     int    // reads the client has sent, which is what the tests count
	failFirst  int    // the first N status reads answer 502
	runningFor int    // then this many reads report running
	exit       string // then stopped with this exit status; "" never stops
	srv        *httptest.Server
}

const testUPID = "UPID:pve1:0001:0002:0003:qmstart:105:root@pam:"

func newTaskSim(t *testing.T, failFirst, runningFor int, exit string) *taskSim {
	t.Helper()
	s := &taskSim{failFirst: failFirst, runningFor: runningFor, exit: exit}
	s.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api2/json/nodes/pve1/tasks/") {
			http.NotFound(w, r)
			return
		}
		s.mu.Lock()
		s.polls++
		n := s.polls
		s.mu.Unlock()
		if n <= s.failFirst {
			http.Error(w, "proxy error", http.StatusBadGateway)
			return
		}
		st := map[string]string{"status": "running"}
		if s.exit != "" && n > s.failFirst+s.runningFor {
			st = map[string]string{"status": "stopped", "exitstatus": s.exit}
		}
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"data": st})
	}))
	t.Cleanup(s.srv.Close)
	return s
}

// pollCount is how many reads the client has sent, counted as it sends them
// rather than as the handler serves them. The two differ in exactly the case
// TestCancellingTheContextEndsTheWait asks about: a read already in flight when
// the context is cancelled is abandoned by the client, but its handler still
// runs and would arrive after the wait returned. Counting where the request is
// made answers "did the wait send another one", which is the question.
func (s *taskSim) pollCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.issued
}

// countingTransport counts a read at the moment the client hands it over, so
// the count is settled by the time TaskWait returns: the poll loop is the only
// caller and it is synchronous.
type countingTransport struct {
	sim  *taskSim
	next http.RoundTripper
}

func (t countingTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	t.sim.mu.Lock()
	t.sim.issued++
	t.sim.mu.Unlock()
	return t.next.RoundTrip(r)
}

func (s *taskSim) wait(ctx context.Context, timeout time.Duration) error {
	client := s.srv.Client()
	client.Transport = countingTransport{sim: s, next: client.Transport}
	c := &Client{BaseURL: s.srv.URL, Token: "PVEAPIToken=test", HTTP: client, TaskPoll: time.Millisecond}
	return c.TaskWait(ctx, "pve1", testUPID, timeout)
}

func TestATaskThatExitsOKIsDone(t *testing.T) {
	s := newTaskSim(t, 0, 2, "OK")
	if err := s.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("err = %v", err)
	}
	if got := s.pollCount(); got != 3 {
		t.Errorf("polled %d times, want 3 — two running, one stopped", got)
	}
}

// Proxmox ends a task that succeeded with warnings as "WARNINGS: n". Reading
// that as a failure would roll back a clone that worked.
func TestATaskThatSucceededWithWarningsIsDone(t *testing.T) {
	s := newTaskSim(t, 0, 0, "WARNINGS: 2")
	if err := s.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("a task that ended with warnings was treated as failed: %v", err)
	}
}

func TestAFailedTaskIsATaskFailure(t *testing.T) {
	s := newTaskSim(t, 0, 1, "clone failed: no space left on device")
	err := s.wait(context.Background(), time.Second)
	var failed *TaskFailedError
	if !errors.As(err, &failed) {
		t.Fatalf("err = %v (%T), want *TaskFailedError", err, err)
	}
	if failed.ExitStatus != "clone failed: no space left on device" || failed.UPID != testUPID {
		t.Errorf("failure = %+v", failed)
	}
	var timeout *TaskTimeoutError
	if errors.As(err, &timeout) {
		t.Error("a task that failed was also reported as a timeout")
	}
}

// Running out of patience is not the task failing. The caller has to be able
// to tell, because the task may still succeed.
func TestAWaitThatRunsOutIsATimeoutNotAFailure(t *testing.T) {
	s := newTaskSim(t, 0, 0, "")
	err := s.wait(context.Background(), 20*time.Millisecond)
	var timeout *TaskTimeoutError
	if !errors.As(err, &timeout) {
		t.Fatalf("err = %v (%T), want *TaskTimeoutError", err, err)
	}
	var failed *TaskFailedError
	if errors.As(err, &failed) {
		t.Error("a timeout was also reported as a task failure")
	}
	if !strings.Contains(err.Error(), "may yet finish") {
		t.Errorf("message %q does not say the outcome is unknown", err)
	}
}

// One dropped status read is not evidence about the task. A provisioning run
// that rolled back a clone over a single proxy hiccup would delete a machine
// that was being built correctly.
func TestAFewFailedPollsDoNotEndTheWait(t *testing.T) {
	s := newTaskSim(t, TaskPollFailures-1, 1, "OK")
	if err := s.wait(context.Background(), time.Second); err != nil {
		t.Fatalf("err = %v after %d failed reads", err, TaskPollFailures-1)
	}
}

func TestRepeatedFailedPollsEndTheWait(t *testing.T) {
	s := newTaskSim(t, 100, 0, "OK")
	err := s.wait(context.Background(), time.Second)
	if err == nil || !strings.Contains(err.Error(), "could not read task") {
		t.Fatalf("err = %v, want a failure to read the task", err)
	}
	var timeout *TaskTimeoutError
	var failed *TaskFailedError
	if errors.As(err, &timeout) || errors.As(err, &failed) {
		t.Errorf("an unreadable task was reported as %T — nothing is known about it", err)
	}
	if got := s.pollCount(); got != TaskPollFailures {
		t.Errorf("read %d times, want %d", got, TaskPollFailures)
	}
}

// Cancellation ends the wait at once and nothing is asked of the cluster
// afterwards: no request outlives the call that made it.
func TestCancellingTheContextEndsTheWait(t *testing.T) {
	s := newTaskSim(t, 0, 0, "")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(15*time.Millisecond, cancel)

	start := time.Now()
	err := s.wait(ctx, time.Minute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Errorf("cancelled wait took %s to return", elapsed)
	}
	after := s.pollCount()
	time.Sleep(20 * time.Millisecond)
	if got := s.pollCount(); got != after {
		t.Errorf("the task was polled %d more time(s) after the wait returned", got-after)
	}
}
