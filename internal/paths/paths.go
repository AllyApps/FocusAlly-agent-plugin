// Package paths centralizes where the tracker keeps its files. All
// per-account data lives under a named profile:
//
//   - Config (config.json, credentials.json, pairing files):
//     <os config dir>/focusally/profiles/<name> — ~/Library/Application
//     Support on macOS, ~/.config on Linux, %AppData% on Windows.
//   - State (per-session snapshots):
//     <state root>/focusally/profiles/<name>, where the state root is
//     $XDG_STATE_HOME, else ~/.local/state, else <os config dir> with a
//     "state" suffix.
//   - Global, profile-independent config (tracker.json) sits at the
//     config root <os config dir>/focusally.
package paths

import (
	"os"
	"path/filepath"
	"regexp"
)

var profileNameRe = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)

// ValidProfileName reports whether name is a safe profile directory
// name (lowercase alphanumerics and dashes only — no path traversal).
func ValidProfileName(name string) bool {
	return profileNameRe.MatchString(name)
}

// RootConfigDir is <os config dir>/focusally — home of tracker.json,
// the profiles/ tree, and the pre-profile flat layout migration reads.
func RootConfigDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "focusally"), nil
}

// StateRootDir is <state root>/focusally — parent of the profiles/
// state tree and of the pre-profile flat sessions/ dir migration moves.
func StateRootDir() (string, error) {
	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "focusally"), nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".local", "state", "focusally"), nil
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "focusally", "state"), nil
}

func ProfileConfigDir(profile string) (string, error) {
	root, err := RootConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "profiles", profile), nil
}

func ProfileStateDir(profile string) (string, error) {
	root, err := StateRootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "profiles", profile), nil
}
