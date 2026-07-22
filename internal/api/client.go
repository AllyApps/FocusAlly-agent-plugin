// Package api talks to the FocusAlly backend: the MCP JSON-RPC endpoint
// for session reports and the OAuth token endpoint for refresh.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/tracker"
)

var httpClient = &http.Client{Timeout: 15 * time.Second}

// agentSessionArgs is the `agentSession` object of the
// `agent_sessions.report` tool call — the shared-contract DTO fields
// minus server-managed ones. Dates go as ISO8601 UTC with exactly
// 3 fractional digits (backend SyncJSON convention).
type agentSessionArgs struct {
	AgentKind         string             `json:"agentKind"`
	ExternalSessionID string             `json:"externalSessionId"`
	MachineName       *string            `json:"machineName"`
	ProjectPath       *string            `json:"projectPath"`
	StartedAt         tracker.WireTime   `json:"startedAt"`
	LastActivityAt    tracker.WireTime   `json:"lastActivityAt"`
	EndedAt           *tracker.WireTime  `json:"endedAt"`
	ActiveIntervals   []tracker.Interval `json:"activeIntervals"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// ErrUnauthorized signals the access token was rejected (HTTP 401) and
// a refresh should be attempted.
type ErrUnauthorized struct{}

func (ErrUnauthorized) Error() string { return "api: unauthorized" }

// Report sends the full session snapshot via JSON-RPC 2.0 tools/call
// `agent_sessions.report` to POST <base>/mcp.
func Report(baseURL, accessToken string, s tracker.State) error {
	args := agentSessionArgs{
		AgentKind:         s.AgentKind,
		ExternalSessionID: s.ExternalSessionID,
		MachineName:       optional(s.MachineName),
		ProjectPath:       optional(s.ProjectPath),
		StartedAt:         s.StartedAt,
		LastActivityAt:    s.LastActivityAt,
		EndedAt:           s.EndedAt,
		ActiveIntervals:   nonNilIntervals(s.ActiveIntervals),
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      1,
		"method":  "tools/call",
		"params": map[string]any{
			"name":      "agent_sessions.report",
			"arguments": map[string]any{"agentSession": args},
		},
	})
	if err != nil {
		return err
	}
	req, err := http.NewRequest(http.MethodPost, baseURL+"/mcp", bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	// Only HTTP 401 means the access token was rejected. The backend
	// reuses JSON-RPC code -32001 for rate_limited (HTTP 429) and
	// insufficient_scope (HTTP 200) too — those must NOT trigger a
	// refresh, just a silent retry on the next flush.
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized{}
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("api: mcp endpoint returned %d", resp.StatusCode)
	}
	var envelope struct {
		Error *rpcError `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&envelope); err != nil {
		return fmt.Errorf("api: bad mcp response: %w", err)
	}
	if envelope.Error != nil {
		return fmt.Errorf("api: mcp error %d: %s", envelope.Error.Code, envelope.Error.Message)
	}
	return nil
}

func optional(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

func nonNilIntervals(in []tracker.Interval) []tracker.Interval {
	if in == nil {
		return []tracker.Interval{}
	}
	return in
}

// TokenResponse is the RFC 6749 token endpoint reply.
type TokenResponse struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	TokenType    string `json:"token_type"`
	ExpiresIn    int    `json:"expires_in"`
	Scope        string `json:"scope"`
}

// TokenError is a non-200 reply from the token endpoint, carrying the
// HTTP status and the RFC 6749 error code so callers can tell a
// definitive grant rejection from a transient failure.
type TokenError struct {
	Status int
	Code   string
}

func (e *TokenError) Error() string {
	return fmt.Sprintf("api: token endpoint returned %d (%s)", e.Status, e.Code)
}

// Definitive reports whether the grant itself was rejected (the
// backend answers 400/401 with an OAuth error envelope: invalid_grant,
// invalid_client, refresh_reuse_detected, …). Transport errors, 5xx,
// and 429 rate limits are NOT definitive — the credentials may still
// be good and the caller must retry later, not re-pair.
func (e *TokenError) Definitive() bool {
	return e.Status == http.StatusBadRequest || e.Status == http.StatusUnauthorized
}

// IsDefinitiveTokenRejection reports whether err proves the refresh
// chain is dead and credentials must be dropped.
func IsDefinitiveTokenRejection(err error) bool {
	var te *TokenError
	return errors.As(err, &te) && te.Definitive()
}

// PostTokenForm posts application/x-www-form-urlencoded to
// POST <base>/oauth/token and decodes the RFC 6749 reply.
func PostTokenForm(baseURL string, form url.Values) (TokenResponse, error) {
	var out TokenResponse
	resp, err := httpClient.Post(
		baseURL+"/oauth/token",
		"application/x-www-form-urlencoded",
		strings.NewReader(form.Encode()),
	)
	if err != nil {
		return out, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var oauthErr struct {
			Error string `json:"error"`
		}
		_ = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&oauthErr)
		return out, &TokenError{Status: resp.StatusCode, Code: oauthErr.Error}
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return out, err
	}
	if out.AccessToken == "" {
		return out, fmt.Errorf("api: token response missing access_token")
	}
	return out, nil
}

// Refresh rotates credentials via the refresh_token grant. The backend
// rotates the refresh token on every call, so both tokens are replaced.
func Refresh(baseURL, clientID string, creds Credentials) (Credentials, error) {
	form := url.Values{
		"grant_type":    {"refresh_token"},
		"refresh_token": {creds.RefreshToken},
		"client_id":     {clientID},
	}
	tok, err := PostTokenForm(baseURL, form)
	if err != nil {
		return Credentials{}, err
	}
	return Credentials{
		AccessToken:  tok.AccessToken,
		RefreshToken: tok.RefreshToken,
		ExpiresAt:    time.Now().Add(time.Duration(tok.ExpiresIn) * time.Second).Unix(),
	}, nil
}
