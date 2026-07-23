package migrate

import (
	"os"
	"path/filepath"
	"testing"
)

func stubRegistrationRemoval(t *testing.T) *int {
	t.Helper()
	calls := 0
	orig := removeLegacyRegistration
	removeLegacyRegistration = func() { calls++ }
	t.Cleanup(func() { removeLegacyRegistration = orig })
	return &calls
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFullFlatLayoutMigrates(t *testing.T) {
	calls := stubRegistrationRemoval(t)
	configRoot, stateRoot := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(configRoot, "config.json"), `{"clientId":"c1"}`)
	writeFile(t, filepath.Join(configRoot, "credentials.json"), `{"accessToken":"at"}`)
	writeFile(t, filepath.Join(configRoot, "pairing.json"), `{"code":"AAAA2222"}`)
	writeFile(t, filepath.Join(configRoot, "pairing-shown"), "ts")
	writeFile(t, filepath.Join(stateRoot, "sessions", "s1.json"), `{"dirty":true}`)

	run(configRoot, stateRoot)

	profileDir := filepath.Join(configRoot, "profiles", "default")
	for _, name := range []string{"config.json", "credentials.json", "pairing.json", "pairing-shown"} {
		if _, err := os.Stat(filepath.Join(profileDir, name)); err != nil {
			t.Errorf("%s not migrated: %v", name, err)
		}
		if _, err := os.Stat(filepath.Join(configRoot, name)); !os.IsNotExist(err) {
			t.Errorf("flat %s still present", name)
		}
	}
	migrated := filepath.Join(stateRoot, "profiles", "default", "sessions", "s1.json")
	data, err := os.ReadFile(migrated)
	if err != nil {
		t.Fatalf("session file not migrated: %v", err)
	}
	if string(data) != `{"dirty":true}` {
		t.Errorf("session contents changed: %s", data)
	}
	if _, err := os.Stat(filepath.Join(stateRoot, "sessions")); !os.IsNotExist(err) {
		t.Error("flat sessions dir still present")
	}
	if *calls != 1 {
		t.Errorf("legacy registration removal ran %d times, want 1", *calls)
	}
	if _, err := os.Stat(filepath.Join(configRoot, "migrate.lock")); !os.IsNotExist(err) {
		t.Error("migrate.lock left behind")
	}
}

func TestSecondRunIsNoOp(t *testing.T) {
	calls := stubRegistrationRemoval(t)
	configRoot, stateRoot := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(configRoot, "credentials.json"), `{"accessToken":"at"}`)

	run(configRoot, stateRoot)
	run(configRoot, stateRoot)

	if *calls != 1 {
		t.Errorf("legacy registration removal ran %d times, want 1", *calls)
	}
}

func TestPartialLayouts(t *testing.T) {
	t.Run("credentials only", func(t *testing.T) {
		stubRegistrationRemoval(t)
		configRoot, stateRoot := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(configRoot, "credentials.json"), `{"accessToken":"at"}`)
		run(configRoot, stateRoot)
		if _, err := os.Stat(filepath.Join(configRoot, "profiles", "default", "credentials.json")); err != nil {
			t.Errorf("credentials not migrated: %v", err)
		}
	})
	t.Run("sessions only", func(t *testing.T) {
		stubRegistrationRemoval(t)
		configRoot, stateRoot := t.TempDir(), t.TempDir()
		writeFile(t, filepath.Join(stateRoot, "sessions", "s1.json"), `{}`)
		run(configRoot, stateRoot)
		if _, err := os.Stat(filepath.Join(stateRoot, "profiles", "default", "sessions", "s1.json")); err != nil {
			t.Errorf("sessions not migrated: %v", err)
		}
		if _, err := os.Stat(filepath.Join(configRoot, "profiles", "default")); err != nil {
			t.Error("profiles/default marker dir missing")
		}
	})
}

func TestFreshInstallSkipsEntirely(t *testing.T) {
	calls := stubRegistrationRemoval(t)
	configRoot, stateRoot := t.TempDir(), t.TempDir()
	run(configRoot, stateRoot)
	if *calls != 0 {
		t.Error("fresh install must not shell out to claude")
	}
	if _, err := os.Stat(filepath.Join(configRoot, "profiles")); !os.IsNotExist(err) {
		t.Error("fresh install must not create profile dirs")
	}
}

func TestHeldLockSkips(t *testing.T) {
	calls := stubRegistrationRemoval(t)
	configRoot, stateRoot := t.TempDir(), t.TempDir()
	writeFile(t, filepath.Join(configRoot, "credentials.json"), `{"accessToken":"at"}`)
	writeFile(t, filepath.Join(configRoot, "migrate.lock"), "999999999")

	run(configRoot, stateRoot)

	// PID 999999999 is dead ⇒ the stale lock is reclaimed and migration
	// proceeds; a live holder's lock would have blocked it.
	if *calls != 1 {
		t.Errorf("stale lock should be reclaimed; removal ran %d times", *calls)
	}
	writeFile(t, filepath.Join(configRoot, "credentials.json"), `{"accessToken":"at2"}`)
	writeFile(t, filepath.Join(configRoot, "migrate.lock"), "-")
	os.RemoveAll(filepath.Join(configRoot, "profiles"))

	run(configRoot, stateRoot)

	if *calls != 1 {
		t.Error("fresh unparseable lock must block migration until stale")
	}
}
