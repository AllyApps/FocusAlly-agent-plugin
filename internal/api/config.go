package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

const DefaultBaseURL = "https://focus.withally.app"

// Config lives at <config-dir>/focusally/config.json. BaseURL override
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

// Credentials live at <config-dir>/focusally/credentials.json (0600).
type Credentials struct {
	AccessToken  string `json:"accessToken"`
	RefreshToken string `json:"refreshToken"`
	// ExpiresAt is unix seconds when the access token expires.
	ExpiresAt int64 `json:"expiresAt"`
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

func SaveCredentials(configDir string, c Credentials) error {
	return saveJSON(CredentialsPath(configDir), c, 0o600)
}

func DeleteCredentials(configDir string) {
	os.Remove(CredentialsPath(configDir))
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
