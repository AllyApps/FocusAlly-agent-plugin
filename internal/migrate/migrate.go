// Package migrate moves the pre-profile flat file layout into
// profiles/default, one-shot and idempotent. Every tracker entry point
// calls Run before resolving profile paths; after the first successful
// pass it is a fast no-op.
package migrate

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/paths"
	"github.com/withally/focusally-agent-plugin/internal/proc"
)

// flatConfigFiles are the per-profile files the flat layout kept at the
// config root.
var flatConfigFiles = []string{
	"config.json", "credentials.json", "pairing.json", "pairing-shown", "pairing.lock",
}

// removeLegacyRegistration deletes the pre-unify `claude mcp add
// --transport http --header "Authorization: Bearer …"` registration —
// the token-embedding flow this migration retires. Injectable for
// tests. Errors are ignored: the registration usually just isn't there.
var removeLegacyRegistration = func() {
	claudeBin, err := exec.LookPath("claude")
	if err != nil {
		return
	}
	cmd := exec.Command(claudeBin, "mcp", "remove", "-s", "user", "focusally")
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
}

// Run resolves the real roots and migrates. Silent on failure — the
// tracker's exit-0 discipline.
func Run() {
	configRoot, err := paths.RootConfigDir()
	if err != nil {
		return
	}
	stateRoot, err := paths.StateRootDir()
	if err != nil {
		return
	}
	run(configRoot, stateRoot)
}

func run(configRoot, stateRoot string) {
	if !needed(configRoot, stateRoot) {
		return
	}
	if !acquireLock(configRoot) {
		return // another process is migrating right now
	}
	defer os.Remove(lockPath(configRoot))
	if !needed(configRoot, stateRoot) {
		return // lost the race; the winner already migrated
	}

	// State first: `needed` treats the config-side profiles/default as
	// the "already migrated" sentinel, so the sessions move must be done
	// before that sentinel can exist — a crash in between must leave a
	// layout the next run still recognizes as unmigrated.
	flatSessions := filepath.Join(stateRoot, "sessions")
	if dirExists(flatSessions) {
		profileStateDir := filepath.Join(stateRoot, "profiles", "default")
		if os.MkdirAll(profileStateDir, 0o700) != nil {
			return
		}
		moveIfExists(flatSessions, filepath.Join(profileStateDir, "sessions"))
	}

	profileDir := filepath.Join(configRoot, "profiles", "default")
	if os.MkdirAll(profileDir, 0o700) != nil {
		return
	}
	for _, name := range flatConfigFiles {
		moveIfExists(filepath.Join(configRoot, name), filepath.Join(profileDir, name))
	}

	removeLegacyRegistration()
}

// needed reports whether any flat-layout remnant still awaits the move.
// profiles/default existing means a past pass already ran — flat files
// appearing afterwards would be stray writes, not user data.
func needed(configRoot, stateRoot string) bool {
	if dirExists(filepath.Join(configRoot, "profiles", "default")) {
		return false
	}
	for _, name := range flatConfigFiles {
		if fileExists(filepath.Join(configRoot, name)) {
			return true
		}
	}
	return dirExists(filepath.Join(stateRoot, "sessions"))
}

func lockPath(configRoot string) string { return filepath.Join(configRoot, "migrate.lock") }

// acquireLock is the same create-excl + stale-reclaim pattern as
// pairing.lock: the PID inside tells a dead holder from a live one,
// with an mtime fallback for Windows.
func acquireLock(configRoot string) bool {
	os.MkdirAll(configRoot, 0o700)
	path := lockPath(configRoot)
	if lockIsStale(path) {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return false
	}
	fmt.Fprintf(f, "%d", os.Getpid())
	f.Close()
	return true
}

func lockIsStale(path string) bool {
	info, err := os.Stat(path)
	if err != nil {
		return false
	}
	if data, err := os.ReadFile(path); err == nil {
		if pid, err := strconv.Atoi(strings.TrimSpace(string(data))); err == nil && pid > 0 {
			if !proc.PidAlive(pid) {
				return true
			}
		}
	}
	return time.Since(info.ModTime()) > time.Minute
}

func moveIfExists(from, to string) {
	err := os.Rename(from, to)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		// Cross-device or permission trouble: leave the flat file in
		// place; the next run retries.
		_ = err
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
