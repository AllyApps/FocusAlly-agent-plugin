//go:build windows

package proc

// PidAlive has no cheap reliable stdlib probe on Windows (signal 0 is
// a Unix concept; FindProcess always succeeds). Answer "alive" and let
// the time-based staleness fallback decide.
func PidAlive(pid int) bool { return true }
