package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const DefaultBaseURL = "https://focus.withally.app"

// Config lives at <profile-config-dir>/config.json. BaseURL override
// exists for testing against a dev stack.
type Config struct {
	BaseURL  string `json:"baseUrl,omitempty"`
	ClientID string `json:"clientId,omitempty"`
}

func (c Config) ResolvedBaseURL() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return DefaultBaseURL
}

// Credentials live at <profile-config-dir>/credentials.json (0600).
type Credentials struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	// ExpiresAt is unix seconds when the access token expires.
	ExpiresAt int64 `json:"expiresAt"`
	// BaseURL records which backend issued this token pair. A profile
	// whose resolved base URL no longer matches reads as unpaired, so
	// data can never flow to the wrong backend. Empty = legacy
	// credentials migrated from the flat layout (bound to the profile's
	// current base; stamped on the next save).
	BaseURL string `json:"baseUrl,omitempty"`
}

// NearExpiry reports whether the access token is expired or about to
// expire, so a flush can refresh proactively instead of burning a
// round-trip on a guaranteed 401.
func (c Credentials) NearExpiry(now int64) bool {
	return c.ExpiresAt > 0 && now >= c.ExpiresAt-60
}

func configPath(configDir string) string      { return filepath.Join(configDir, "config.json") }
func CredentialsPath(configDir string) string { return filepath.Join(configDir, "credentials.json") }

func LoadConfig(configDir string) (Config, error) {
	var c Config
	err := loadJSON(configPath(configDir), &c)
	return c, err
}

func SaveConfig(configDir string, c Config) error {
	return saveJSON(configPath(configDir), c, 0o600)
}

func LoadCredentials(configDir string) (Credentials, bool) {
	var c Credentials
	if err := loadJSON(CredentialsPath(configDir), &c); err != nil || c.AccessToken == "" {
		return Credentials{}, false
	}
	return c, true
}

// LoadCredentialsBound loads credentials only if they were issued by
// wantBaseURL. A stored binding to a different backend reads as
// unpaired — better no data than data sent to the wrong place.
func LoadCredentialsBound(configDir, wantBaseURL string) (Credentials, bool) {
	c, ok := LoadCredentials(configDir)
	if !ok {
		return Credentials{}, false
	}
	if c.BaseURL != "" && c.BaseURL != wantBaseURL {
		return Credentials{}, false
	}
	return c, true
}

// SaveCredentials persists the pair, always stamping the issuing
// backend's base URL.
func SaveCredentials(configDir, baseURL string, c Credentials) error {
	c.BaseURL = baseURL
	return saveJSON(CredentialsPath(configDir), c, 0o600)
}

func DeleteCredentials(configDir string) {
	os.Remove(CredentialsPath(configDir))
}

// GlobalConfig lives at <root-config-dir>/tracker.json and holds
// profile-independent settings.
type GlobalConfig struct {
	// TrackingProfile names the ONE profile Claude Code hook tracking
	// writes to. Empty means "default"; the literal "none" disables
	// tracking entirely (MCP-only mode).
	TrackingProfile string `json:"trackingProfile,omitempty"`
}

// TrackingDisabled is the TrackingProfile value that switches hook
// tracking off entirely.
const TrackingDisabled = "none"

func (g GlobalConfig) ResolvedTrackingProfile() string {
	if g.TrackingProfile == "" {
		return "default"
	}
	return g.TrackingProfile
}

func globalConfigPath(rootDir string) string { return filepath.Join(rootDir, "tracker.json") }

func LoadGlobalConfig(rootDir string) (GlobalConfig, error) {
	var g GlobalConfig
	err := loadJSON(globalConfigPath(rootDir), &g)
	return g, err
}

func SaveGlobalConfig(rootDir string, g GlobalConfig) error {
	return saveJSON(globalConfigPath(rootDir), g, 0o600)
}

func loadJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal(data, out)
}

func saveJSON(path string, v any, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
