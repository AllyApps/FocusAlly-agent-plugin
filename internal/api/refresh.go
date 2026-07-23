package api

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/proc"
)

const (
	credLockRetry   = 25 * time.Millisecond
	credLockTimeout = 20 * time.Second
	// credLockStale must outlast one full token round-trip (the HTTP
	// client times out at 15 s) before a crashed holder's lock is
	// reclaimed by mtime.
	credLockStale = 30 * time.Second
)

func credLockPath(configDir string) string { return filepath.Join(configDir, "credentials.lock") }

// RefreshOutcome distinguishes why a refresh did not yield fresh
// credentials: a definitive grant rejection means the profile is
// genuinely unpaired (credentials dropped), while a transient failure
// (token-endpoint 5xx/429, network, lock timeout) leaves valid
// credentials on disk and must NOT present as unpaired.
type RefreshOutcome int

const (
	RefreshOK RefreshOutcome = iota
	RefreshRejected
	RefreshTransient
)

// RefreshUnderLock rotates the token pair while holding the profile's
// credentials.lock, so every consumer of a profile (flush, MCP proxy)
// shares ONE refresh path and can never double-rotate against another
// process. staleAccessToken is the access token that just failed: after
// acquiring the lock the credentials are re-read, and if they already
// differ (someone else refreshed first) the on-disk pair is returned
// without a network call.
//
// Credentials are dropped (forcing re-pairing) ONLY on a definitive
// grant rejection — the backend answering 400/401 to the refresh
// itself. Transport errors, 5xx, and 429 leave them in place for a
// later retry.
func RefreshUnderLock(configDir, base, clientID, staleAccessToken string) (Credentials, RefreshOutcome) {
	if !acquireCredLock(configDir) {
		return Credentials{}, RefreshTransient
	}
	defer os.Remove(credLockPath(configDir))

	creds, ok := LoadCredentialsBound(configDir, base)
	if !ok {
		return Credentials{}, RefreshRejected
	}
	if creds.AccessToken != staleAccessToken {
		return creds, RefreshOK // another process already rotated the pair
	}

	refreshed, err := Refresh(base, clientID, creds)
	if err != nil {
		if IsDefinitiveTokenRejection(err) {
			DeleteCredentials(configDir)
			return Credentials{}, RefreshRejected
		}
		return Credentials{}, RefreshTransient
	}
	if SaveCredentials(configDir, base, refreshed) != nil {
		return Credentials{}, RefreshTransient
	}
	return refreshed, RefreshOK
}

// acquireCredLock waits for the lock instead of failing fast: a caller
// arriving second must block until the winner finishes, then re-read
// the rotated pair.
func acquireCredLock(configDir string) bool {
	os.MkdirAll(configDir, 0o700)
	path := credLockPath(configDir)
	deadline := time.Now().Add(credLockTimeout)
	for {
		if credLockIsStale(path) {
			os.Remove(path)
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d", os.Getpid())
			f.Close()
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(credLockRetry)
	}
}

func credLockIsStale(path string) bool {
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
	return time.Since(info.ModTime()) > credLockStale
}
