//go:build unix

package main

import "syscall"

// pidAlive reports whether a process exists, via kill(2) with signal 0 — no
// signal is sent, only the existence/permission check runs. EPERM still means
// the pid exists. Unlike /proc probing this also works on macOS and the BSDs.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}
