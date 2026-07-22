//go:build windows

package pairing

// pidAlive has no cheap reliable stdlib probe on Windows (signal 0 is
// a Unix concept; FindProcess always succeeds). Answer "alive" and let
// the time-based staleness fallback decide.
func pidAlive(pid int) bool { return true }
