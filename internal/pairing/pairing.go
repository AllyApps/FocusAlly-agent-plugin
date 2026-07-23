// Package pairing implements the zero-touch OAuth PKCE pairing flow
// against the FocusAlly backend:
//
//	POST /oauth/register                    (once; client_id persisted)
//	GET  /oauth/authorize?...               (mints the pairing code,
//	                                         parsed from the consent
//	                                         page HTML)
//	GET  /oauth/authorize/{code}/status     (poll until approved; the
//	                                         auth code arrives inside
//	                                         redirectTo's ?code= query)
//	POST /oauth/token                       (authorization_code + PKCE)
//
// It runs fully detached from the Claude session; every failure is
// silent.
package pairing

import (
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/api"
	"github.com/withally/focusally-agent-plugin/internal/proc"
)

const (
	// RedirectURI must be registered with the client and repeated
	// verbatim at /oauth/authorize AND /oauth/token — the backend
	// requires an exact match at both. http://localhost* is in the
	// backend's allowed set; nothing ever listens on it (the auth code
	// arrives via the status poll, not a browser redirect).
	RedirectURI = "http://localhost/focusally-tracker/callback"

	// Scope requests the full tool surface in one approval: the same
	// token powers hook tracking AND every interactive MCP tool, so a
	// re-login via auth.login loses nothing. The user can narrow
	// scopes per-key in the app afterwards.
	Scope      = "sessions:read sessions:write tasks:read tasks:write priorities:read priorities:write devices:read sync:read agent:write"
	ClientName = "FocusAlly Agent Tracker"
	SoftwareID = "focusally-agent-plugin"

	pollInterval = 5 * time.Second
	pollTimeout  = 15 * time.Minute
	// codeShowThrottle limits how often the SessionStart hook re-shows
	// the pairing message while unpaired.
	codeShowThrottle = 24 * time.Hour
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// PendingFile is written to <config-dir>/pairing.json the moment a
// pairing code is minted. It carries everything needed to RESUME the
// pairing after process death (reboot, crash): the SessionStart hook
// surfaces the same code, and a restarted `pair` process polls it with
// the stored PKCE verifier instead of minting a new one — one
// connection means one code and one approval. Same sensitivity class
// and mode (0600) as credentials.json.
type PendingFile struct {
	Code      string    `json:"code"`
	ExpiresAt time.Time `json:"expiresAt"`
	Verifier  string    `json:"verifier"`
}

func pendingPath(configDir string) string { return filepath.Join(configDir, "pairing.json") }
func shownPath(configDir string) string   { return filepath.Join(configDir, "pairing-shown") }
func lockPath(configDir string) string    { return filepath.Join(configDir, "pairing.lock") }

// LoadPending returns the current unexpired pending pairing code.
func LoadPending(configDir string) (PendingFile, bool) {
	var p PendingFile
	data, err := os.ReadFile(pendingPath(configDir))
	if err != nil || json.Unmarshal(data, &p) != nil {
		return PendingFile{}, false
	}
	if p.Code == "" || time.Now().After(p.ExpiresAt) {
		return PendingFile{}, false
	}
	return p, true
}

// ShouldShowCode throttles the user-visible SessionStart message to at
// most once per day while unpaired.
func ShouldShowCode(configDir string) bool {
	info, err := os.Stat(shownPath(configDir))
	if err != nil {
		return true
	}
	return time.Since(info.ModTime()) >= codeShowThrottle
}

// MarkCodeShown stamps the throttle file.
func MarkCodeShown(configDir string) {
	os.MkdirAll(configDir, 0o700)
	os.WriteFile(shownPath(configDir), []byte(time.Now().Format(time.RFC3339)), 0o600)
}

// FormatCode renders "ABCD2345" as "ABCD-2345" (display form the app
// and consent page use).
func FormatCode(code string) string {
	if len(code) == 8 {
		return code[:4] + "-" + code[4:]
	}
	return code
}

// Bootstrap makes sure a detached pairing poller is running for the
// profile and returns the current pending code, waiting up to wait for
// a fresh mint to land. One code path serves both the SessionStart
// hook message and the auth.login MCP tool. spawn re-execs the tracker
// binary (injectable for tests).
func Bootstrap(configDir, profile string, spawn func(args ...string), wait time.Duration) (PendingFile, bool) {
	// Always (re)spawn: the poller resumes a persisted pending pairing
	// after process death/reboot, exits immediately if a live poller
	// holds the lock, and mints a fresh code only when there is nothing
	// to resume.
	if spawn != nil {
		spawn("pair", "--profile", profile)
	}
	deadline := time.Now().Add(wait)
	for {
		if p, ok := LoadPending(configDir); ok {
			return p, true
		}
		if !time.Now().Before(deadline) {
			return PendingFile{}, false
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// Run executes the pairing flow. A valid pending pairing (unexpired
// code + stored verifier) is RESUMED — no new code, no new approval
// window, no deeplink; a fresh code is minted only when there is
// nothing to resume (or the resumed one ended in a terminal failure).
// Returns silently on any failure; on success credentials are saved —
// the plugin-declared MCP server picks them up from disk, so pairing
// ends here.
func Run(configDir string) {
	cfg, err := api.LoadConfig(configDir)
	if err != nil {
		return
	}
	base := cfg.ResolvedBaseURL()

	if _, ok := api.LoadCredentialsBound(configDir, base); ok {
		os.Remove(pendingPath(configDir))
		return
	}
	if !acquireLock(configDir) {
		return // another poller is live
	}
	defer os.Remove(lockPath(configDir))

	if pending, ok := LoadPending(configDir); ok && pending.Verifier != "" && cfg.ClientID != "" {
		if finishPairing(configDir, cfg, base, pending) {
			return
		}
		// Terminal failure (denied/expired/consumed/timeout): the
		// pending file is gone; fall through to mint a fresh code.
		if _, ok := api.LoadCredentialsBound(configDir, base); ok {
			return
		}
	}

	if cfg.ClientID == "" {
		id, err := registerClient(base)
		if err != nil {
			return
		}
		cfg.ClientID = id
		if api.SaveConfig(configDir, cfg) != nil {
			return
		}
	}

	verifier, challenge, err := pkcePair()
	if err != nil {
		return
	}
	pairingCode, err := startAuthorize(base, cfg.ClientID, challenge)
	if err != nil {
		// Stale client_id (e.g. wiped backend): re-register once.
		id, regErr := registerClient(base)
		if regErr != nil {
			return
		}
		cfg.ClientID = id
		if api.SaveConfig(configDir, cfg) != nil {
			return
		}
		pairingCode, err = startAuthorize(base, cfg.ClientID, challenge)
		if err != nil {
			return
		}
	}

	pending := PendingFile{
		Code:      pairingCode,
		ExpiresAt: time.Now().Add(pollTimeout),
		Verifier:  verifier,
	}
	if data, err := json.Marshal(pending); err == nil {
		if os.WriteFile(pendingPath(configDir), data, 0o600) != nil {
			return // resume impossible without the file; don't orphan an approval
		}
	}
	// The deeplink fires only for a freshly minted code — resuming must
	// never pop a second approval window.
	openInApp(pairingCode)

	finishPairing(configDir, cfg, base, pending)
}

// finishPairing polls the pending code until approval, then exchanges
// the auth code using the pending file's stored verifier. Reports
// success; on any terminal outcome the pending file is removed.
func finishPairing(configDir string, cfg api.Config, base string, pending PendingFile) bool {
	authCode, err := pollForApproval(base, pending.Code, pending.ExpiresAt)
	if err != nil {
		os.Remove(pendingPath(configDir))
		return false
	}

	creds, err := exchangeCode(base, cfg.ClientID, authCode, pending.Verifier)
	if err != nil {
		os.Remove(pendingPath(configDir))
		return false
	}
	if api.SaveCredentials(configDir, base, creds) != nil {
		return false
	}
	os.Remove(pendingPath(configDir))
	os.Remove(shownPath(configDir))
	return true
}

func acquireLock(configDir string) bool {
	os.MkdirAll(configDir, 0o700)
	path := lockPath(configDir)
	if lockIsStale(path) {
		os.Remove(path)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		if errors.Is(err, fs.ErrExist) {
			return false // another pairing process is running
		}
		return false
	}
	fmt.Fprintf(f, "%d", os.Getpid())
	f.Close()
	return true
}

// lockIsStale detects a poller that died without cleanup. The lock
// stores the holder's PID: if that process is gone (Unix liveness
// probe; on Windows PidAlive conservatively says "alive"), the lock is
// reclaimed immediately — otherwise a crashed poller would block the
// resume for the whole time-based window. The mtime fallback covers
// Windows and unparseable locks.
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
	return time.Since(info.ModTime()) > pollTimeout+time.Minute
}

// registerClient performs RFC 7591 dynamic client registration.
func registerClient(base string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"client_name":                ClientName,
		"redirect_uris":              []string{RedirectURI},
		"token_endpoint_auth_method": "none",
		"software_id":                SoftwareID,
	})
	resp, err := httpClient.Post(base+"/oauth/register", "application/json", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pairing: register returned %d", resp.StatusCode)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if out.ClientID == "" {
		return "", fmt.Errorf("pairing: register response missing client_id")
	}
	return out.ClientID, nil
}

func pkcePair() (verifier, challenge string, err error) {
	raw := make([]byte, 32)
	if _, err = rand.Read(raw); err != nil {
		return
	}
	verifier = base64.RawURLEncoding.EncodeToString(raw)
	sum := sha256.Sum256([]byte(verifier))
	challenge = base64.RawURLEncoding.EncodeToString(sum[:])
	return
}

// consent page embeds the raw pairing code as `var code = "XXXXXXXX";`.
var consentCodeRe = regexp.MustCompile(`var code = "([2-9A-HJKMNP-TV-Z]{8})"`)

// startAuthorize hits GET /oauth/authorize and parses the pairing code
// out of the HTML consent page (the backend has no JSON variant of
// this endpoint; the code also renders as XXXX-XXXX in the page body).
func startAuthorize(base, clientID, challenge string) (string, error) {
	q := url.Values{
		"response_type":         {"code"},
		"client_id":             {clientID},
		"redirect_uri":          {RedirectURI},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
		"scope":                 {Scope},
	}
	resp, err := httpClient.Get(base + "/oauth/authorize?" + q.Encode())
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("pairing: authorize returned %d", resp.StatusCode)
	}
	html, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	m := consentCodeRe.FindSubmatch(html)
	if m == nil {
		return "", fmt.Errorf("pairing: pairing code not found in consent page")
	}
	return string(m[1]), nil
}

// pollForApproval polls GET /oauth/authorize/{code}/status until the
// user approves in the FocusAlly app (or the code's own expiry
// passes). On approval the raw auth code is extracted from the
// redirectTo URL's `code` query parameter.
func pollForApproval(base, pairingCode string, deadline time.Time) (string, error) {
	if cap := time.Now().Add(pollTimeout); deadline.After(cap) {
		deadline = cap
	}
	for time.Now().Before(deadline) {
		status, redirectTo, err := pollOnce(base, pairingCode)
		if err == nil {
			switch status {
			case "approved":
				if code := extractAuthCode(redirectTo); code != "" {
					return code, nil
				}
				return "", fmt.Errorf("pairing: approved but no code in redirectTo")
			case "denied", "expired", "consumed":
				return "", fmt.Errorf("pairing: terminal status %s", status)
			}
		}
		time.Sleep(pollInterval)
	}
	return "", fmt.Errorf("pairing: approval timed out")
}

func pollOnce(base, pairingCode string) (status, redirectTo string, err error) {
	resp, err := httpClient.Get(base + "/oauth/authorize/" + url.PathEscape(pairingCode) + "/status")
	if err != nil {
		return "", "", err
	}
	defer resp.Body.Close()
	var out struct {
		Status     string  `json:"status"`
		RedirectTo *string `json:"redirectTo"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", "", err
	}
	if out.RedirectTo != nil {
		redirectTo = *out.RedirectTo
	}
	return out.Status, redirectTo, nil
}

func extractAuthCode(redirectTo string) string {
	u, err := url.Parse(redirectTo)
	if err != nil {
		return ""
	}
	return u.Query().Get("code")
}

func exchangeCode(base, clientID, authCode, verifier string) (api.Credentials, error) {
	form := url.Values{
		"grant_type":    {"authorization_code"},
		"code":          {authCode},
		"redirect_uri":  {RedirectURI},
		"client_id":     {clientID},
		"code_verifier": {verifier},
	}
	tok, err := api.PostTokenForm(base, form)
	if err != nil {
		return api.Credentials{}, err
	}
	return api.Credentials{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
	}, nil
}

// openInApp fires the optional local accelerator: if the FocusAlly app
// is on this machine, the deeplink pops the approval window directly.
// macOS only; failure is silent.
func openInApp(pairingCode string) {
	if runtime.GOOS != "darwin" {
		return
	}
	if _, err := exec.LookPath("open"); err != nil {
		return
	}
	link := "focusally://mcp-authorize?code=" + url.QueryEscape(FormatCode(pairingCode))
	cmd := exec.Command("open", link)
	cmd.Stdout, cmd.Stderr = nil, nil
	_ = cmd.Run()
}
