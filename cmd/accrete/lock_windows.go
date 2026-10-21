//go:build windows

package main

import (
	"errors"
	"os"
	"syscall"
)

// processAlive reports whether a process with this pid is running here. On
// Windows, FindProcess opens the process: no such process is
// ERROR_INVALID_PARAMETER, and any other failure (access denied to another
// user's or an elevated process) means it exists.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return !errors.Is(err, syscall.Errno(87))
	}
	p.Release()
	return true
}

// syncDir does nothing: Windows cannot sync a directory, and its rename is
// already durable once it returns.
func syncDir(string) {}
