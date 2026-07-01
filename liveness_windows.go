//go:build windows

package main

import "os"

// pidAlive reports whether a process exists. On Windows os.FindProcess opens a
// handle, so it fails for a pid that no longer exists.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	_ = p.Release()
	return true
}
