package api

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestSaveCredentialsStampsBaseURL(t *testing.T) {
	dir := t.TempDir()
	creds := Credentials{AccessToken: "at", RefreshToken: "rt", ExpiresAt: 100}
	if err := SaveCredentials(dir, "https://a.example", creds); err != nil {
		t.Fatal(err)
	}
	loaded, ok := LoadCredentials(dir)
	if !ok {
		t.Fatal("LoadCredentials failed")
	}
	if loaded.BaseURL != "https://a.example" {
		t.Errorf("BaseURL = %q, want stamped base", loaded.BaseURL)
	}
}

func TestLoadCredentialsBound(t *testing.T) {
	dir := t.TempDir()
	if err := SaveCredentials(dir, "https://a.example", Credentials{AccessToken: "at"}); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadCredentialsBound(dir, "https://a.example"); !ok {
		t.Error("matching base rejected")
	}
	if _, ok := LoadCredentialsBound(dir, "https://b.example"); ok {
		t.Error("mismatched base accepted")
	}
}

func TestLoadCredentialsBoundLegacyEmptyBase(t *testing.T) {
	dir := t.TempDir()
	legacy, _ := json.Marshal(Credentials{AccessToken: "at", RefreshToken: "rt"})
	if err := os.WriteFile(filepath.Join(dir, "credentials.json"), legacy, 0o600); err != nil {
		t.Fatal(err)
	}
	creds, ok := LoadCredentialsBound(dir, "https://any.example")
	if !ok {
		t.Fatal("legacy credentials with empty baseUrl must load as bound")
	}
	// The binding is stamped on the next save.
	if err := SaveCredentials(dir, "https://any.example", creds); err != nil {
		t.Fatal(err)
	}
	if _, ok := LoadCredentialsBound(dir, "https://other.example"); ok {
		t.Error("post-stamp mismatched base accepted")
	}
}

func TestGlobalConfigResolution(t *testing.T) {
	if got := (GlobalConfig{}).ResolvedTrackingProfile(); got != "default" {
		t.Errorf("empty → %q, want default", got)
	}
	if got := (GlobalConfig{TrackingProfile: "work"}).ResolvedTrackingProfile(); got != "work" {
		t.Errorf("work → %q", got)
	}
	if got := (GlobalConfig{TrackingProfile: TrackingDisabled}).ResolvedTrackingProfile(); got != TrackingDisabled {
		t.Errorf("none → %q, want none", got)
	}
}

func TestGlobalConfigRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := SaveGlobalConfig(dir, GlobalConfig{TrackingProfile: "work"}); err != nil {
		t.Fatal(err)
	}
	g, err := LoadGlobalConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if g.TrackingProfile != "work" {
		t.Errorf("TrackingProfile = %q, want work", g.TrackingProfile)
	}
	missing, err := LoadGlobalConfig(t.TempDir())
	if err != nil || missing.TrackingProfile != "" {
		t.Errorf("missing tracker.json should load zero config, got %+v err %v", missing, err)
	}
}
