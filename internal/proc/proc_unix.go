//go:build !windows

package proc

import (
	"errors"
	"os"
	"syscall"
)

// PidAlive probes process existence with signal 0 (kill -0 style).
// EPERM means the process exists but belongs to someone else — alive.
func PidAlive(pid int) bool {
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
