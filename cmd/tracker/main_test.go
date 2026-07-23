package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/withally/focusally-agent-plugin/internal/api"
)

const sessionStartPayload = `{
	"session_id": "sess-none-test",
	"hook_event_name": "SessionStart",
	"cwd": "/tmp/project"
}`

// isolateDirs points the real path resolution at temp roots so the
// hook exercises the full production path without touching the user's
// config.
func isolateDirs(t *testing.T) (configRoot, stateRoot string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	stateRoot = filepath.Join(home, "state", "focusally")
	t.Setenv("XDG_STATE_HOME", filepath.Join(home, "state"))
	switch {
	case os.Getenv("XDG_CONFIG_HOME") != "":
		t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	}
	cfg, err := os.UserConfigDir()
	if err != nil {
		t.Skipf("no user config dir: %v", err)
	}
	if !strings.HasPrefix(cfg, home) {
		t.Skipf("config dir %q escaped the temp home", cfg)
	}
	return filepath.Join(cfg, "focusally"), stateRoot
}

func TestTrackingProfileNoneSilencesTheHookEntirely(t *testing.T) {
	configRoot, stateRoot := isolateDirs(t)
	if err := api.SaveGlobalConfig(configRoot, api.GlobalConfig{TrackingProfile: api.TrackingDisabled}); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	runHook("SessionStart", strings.NewReader(sessionStartPayload), &stdout)

	if stdout.Len() != 0 {
		t.Errorf("tracking none must write nothing to stdout, got %q", stdout.String())
	}
	if _, err := os.Stat(filepath.Join(configRoot, "profiles")); !os.IsNotExist(err) {
		t.Error("tracking none must not create profile config dirs (no pairing spawn)")
	}
	if _, err := os.Stat(stateRoot); !os.IsNotExist(err) {
		t.Error("tracking none must not write any state")
	}
}

func TestHookWritesStateForTrackingProfile(t *testing.T) {
	configRoot, stateRoot := isolateDirs(t)
	if err := api.SaveGlobalConfig(configRoot, api.GlobalConfig{TrackingProfile: "work"}); err != nil {
		t.Fatal(err)
	}
	// Pre-stamp the pairing throttle so the hook neither spawns a
	// detached pairing process nor waits for a code during the test.
	profileCfg := filepath.Join(configRoot, "profiles", "work")
	if err := os.MkdirAll(profileCfg, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(profileCfg, "pairing-shown"), []byte("now"), 0o600); err != nil {
		t.Fatal(err)
	}

	var stdout bytes.Buffer
	runHook("UserPromptSubmit", strings.NewReader(`{"session_id":"sess-1","hook_event_name":"UserPromptSubmit"}`), &stdout)

	stateFile := filepath.Join(stateRoot, "profiles", "work", "sessions", "sess-1.json")
	if _, err := os.Stat(stateFile); err != nil {
		t.Errorf("hook state must land in the tracking profile's dir: %v", err)
	}
}
