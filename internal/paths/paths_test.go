package paths

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestValidProfileName(t *testing.T) {
	valid := []string{"default", "work", "a", "dev-1", "0", strings.Repeat("a", 32)}
	for _, name := range valid {
		if !ValidProfileName(name) {
			t.Errorf("ValidProfileName(%q) = false, want true", name)
		}
	}
	invalid := []string{
		"", "Default", "with space", "under_score", "dot.name",
		"../x", "..", "a/b", "a\\b", strings.Repeat("a", 33), "юникод",
	}
	for _, name := range invalid {
		if ValidProfileName(name) {
			t.Errorf("ValidProfileName(%q) = true, want false", name)
		}
	}
}

func TestProfileDirShapes(t *testing.T) {
	configRoot, err := RootConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(configRoot) != "focusally" {
		t.Errorf("RootConfigDir = %q, want a focusally leaf", configRoot)
	}

	cfgDir, err := ProfileConfigDir("work")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(configRoot, "profiles", "work"); cfgDir != want {
		t.Errorf("ProfileConfigDir = %q, want %q", cfgDir, want)
	}

	stateRoot, err := StateRootDir()
	if err != nil {
		t.Fatal(err)
	}
	stateDir, err := ProfileStateDir("work")
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join(stateRoot, "profiles", "work"); stateDir != want {
		t.Errorf("ProfileStateDir = %q, want %q", stateDir, want)
	}
}

func TestStateRootHonorsXDG(t *testing.T) {
	t.Setenv("XDG_STATE_HOME", "/custom/state")
	got, err := StateRootDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/custom/state", "focusally"); got != want {
		t.Errorf("StateRootDir = %q, want %q", got, want)
	}
}
