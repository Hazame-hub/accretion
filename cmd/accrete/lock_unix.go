//go:build !windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether a process with this pid is running here. A
// process another user owns answers EPERM, and is running.
func processAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// syncDir makes a rename in dir durable. Best effort: the rename is done
// either way.
func syncDir(dir string) {
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
}
