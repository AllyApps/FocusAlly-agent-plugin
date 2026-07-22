// Package paths centralizes where the tracker keeps its files.
//
//   - State (per-session snapshots): $XDG_STATE_HOME/focusally, else
//     ~/.local/state/focusally, else <os config dir>/focusally/state.
//   - Config (config.json, credentials.json, pairing files):
//     <os config dir>/focusally — ~/Library/Application Support on
//     macOS, ~/.config on Linux, %AppData% on Windows.
package paths

import (
	"os"
	"path/filepath"
)

func StateDir() (string, error) {
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

func ConfigDir() (string, error) {
	cfg, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(cfg, "focusally"), nil
}
