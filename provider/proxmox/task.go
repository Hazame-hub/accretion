package proxmox

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Tasks. Every mutation on Proxmox answers with a UPID and finishes later on
// the cluster, so "done" means the task stopped and stopped well.

// TaskNode extracts the node a task runs on from its UPID
// ("UPID:nodename:..."), the only reliable source, since cluster-wide calls
// like the SDN apply return a task without saying where.
func TaskNode(upid string) string {
	parts := strings.Split(upid, ":")
	if len(parts) > 1 {
		return parts[1]
	}
	return ""
}

// AsUPID reports whether a response's data is a task reference.
func AsUPID(data json.RawMessage) (string, bool) {
	var s string
	if err := json.Unmarshal(data, &s); err != nil {
		return "", false
	}
	if strings.HasPrefix(s, "UPID:") {
		return s, true
	}
	return "", false
}

// TaskFailedError is a task the cluster ran to its end, and that ended badly.
type TaskFailedError struct {
	UPID       string
	ExitStatus string
}

func (e *TaskFailedError) Error() string {
	return fmt.Sprintf("proxmox: task failed: %s", e.ExitStatus)
}

// TaskTimeoutError is a task this client stopped waiting for. Nothing is
// known about its outcome: it may still be running, and it may yet succeed.
// So it is not a failure of the operation, and a caller has to look at the
// resource before doing anything that would be unsafe to do twice.
type TaskTimeoutError struct {
	UPID  string
	After time.Duration
}

func (e *TaskTimeoutError) Error() string {
	return fmt.Sprintf("proxmox: task %s still running after %s — stopped waiting; it may yet finish", e.UPID, e.After)
}

// TaskUnreadableError is a task whose status could not be read, a run of
// polls in a row. Like a timeout, nothing is known about its outcome.
type TaskUnreadableError struct {
	UPID     string
	Attempts int
	Err      error
}

func (e *TaskUnreadableError) Error() string {
	return fmt.Sprintf("proxmox: could not read task %s (%d attempts): %v", e.UPID, e.Attempts, e.Err)
}

func (e *TaskUnreadableError) Unwrap() error { return e.Err }

// TaskPollFailures is how many consecutive status reads may fail before a wait
// gives up. One dropped request says nothing about the task; a run of them says
// the cluster cannot be asked.
const TaskPollFailures = 3

// TaskWait polls a task until it stops, then demands it stopped well. This is
// what makes an action's success mean "the cluster finished" rather than "the
// cluster accepted".
//
// Its three unhappy answers are different errors because they call for
// different next steps: *TaskFailedError (the task ran and failed),
// *TaskTimeoutError (the wait ran out; the outcome is unknown), and
// *TaskUnreadableError when the task's status could not be read at all. Cancelling ctx ends
// the wait at once with ctx.Err().
func (c *Client) TaskWait(ctx context.Context, node, upid string, timeout time.Duration) error {
	if node == "" {
		node = TaskNode(upid)
	}
	if node == "" {
		return fmt.Errorf("proxmox: cannot poll task %q: no node", upid)
	}
	deadline := time.Now().Add(timeout)
	// A poll is a plain call, bounded by the call timeout, and also by the
	// wait's own deadline: a status read that hangs must not stretch a
	// 20-second wait to 35.
	pollCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	path := fmt.Sprintf("/nodes/%s/tasks/%s/status", node, url.PathEscape(upid))
	failures := 0
	for {
		var st struct {
			Status     string `json:"status"`
			ExitStatus string `json:"exitstatus"`
		}
		if err := c.Get(pollCtx, path, &st); err != nil {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			if time.Until(deadline) <= 0 {
				// The poll was cut off by the wait's own deadline: the
				// task's outcome is unknown, not unreadable.
				return &TaskTimeoutError{UPID: upid, After: timeout}
			}
			failures++
			if failures >= TaskPollFailures {
				return &TaskUnreadableError{UPID: upid, Attempts: failures, Err: err}
			}
		} else {
			failures = 0
			if st.Status == "stopped" {
				// "WARNINGS: n" is how Proxmox ends a task that succeeded with
				// warnings; anything else but OK is the task's own failure.
				if st.ExitStatus != "OK" && !strings.HasPrefix(st.ExitStatus, "WARNINGS") {
					return &TaskFailedError{UPID: upid, ExitStatus: st.ExitStatus}
				}
				return nil
			}
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return &TaskTimeoutError{UPID: upid, After: timeout}
		}
		wait := c.TaskPoll
		if wait > remaining {
			wait = remaining
		}
		if !SleepCtx(ctx, wait) {
			return ctx.Err()
		}
	}
}

// WaitIfTask waits when a mutation came back asynchronous; some config calls
// return null instead of a UPID and are already done.
func (c *Client) WaitIfTask(ctx context.Context, node string, data json.RawMessage, timeout time.Duration) error {
	if upid, ok := AsUPID(data); ok {
		return c.TaskWait(ctx, node, upid, timeout)
	}
	return nil
}

// SleepCtx waits d, or less if the context ends first, and says which.
func SleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
