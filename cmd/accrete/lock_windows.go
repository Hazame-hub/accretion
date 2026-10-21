//go:build windows

package main

import "os"

// processAlive reports whether a process with this pid is running here. On
// Windows, FindProcess opens the process and fails when there is none.
func processAlive(pid int) bool {
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	p.Release()
	return true
}

// syncDir does nothing: Windows cannot sync a directory, and its rename is
// already durable once it returns.
func syncDir(string) {}
