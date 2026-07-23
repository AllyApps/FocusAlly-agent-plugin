package mcpproxy

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/api"
	"github.com/withally/focusally-agent-plugin/internal/pairing"
)

// The two auth tools are the ONLY tool definitions living in Go —
// every other tool keeps coming from the server via tools/list
// passthrough. They are appended to the server's list while paired and
// are the whole list while unpaired.
var localToolDefs = []json.RawMessage{
	json.RawMessage(`{
		"name": "auth.status",
		"description": "Report the FocusAlly connection status for this MCP profile: whether it is paired with the backend, when the access token expires, which profile Claude Code hook tracking writes to, and how many session snapshots are still waiting to upload. Use this to diagnose a silently expired login or a profile pointed at the wrong backend.",
		"inputSchema": {"type": "object", "properties": {}, "additionalProperties": false}
	}`),
	json.RawMessage(`{
		"name": "auth.login",
		"description": "Connect (or re-connect) FocusAlly for this MCP profile. Returns a pairing code that the user must approve in the FocusAlly app: Profile → MCP keys → enter code — on any device where they are signed in. Approval happens in the app, not here; once approved, the full FocusAlly tool list becomes available automatically. When already connected, pass force: true to drop the current login and re-pair (e.g. to renew scopes or switch accounts).",
		"inputSchema": {"type": "object", "properties": {"force": {"type": "boolean", "description": "Drop the current credentials and start a fresh pairing even when already connected."}}, "additionalProperties": false}
	}`),
}

var localToolNames = map[string]bool{"auth.status": true, "auth.login": true}

const defaultLoginWait = 2 * time.Second

// loginWait bounds how long pairing bootstrap waits for a freshly
// minted code.
func (s *server) loginWait() time.Duration {
	if s.deps.LoginWait > 0 {
		return s.deps.LoginWait
	}
	return defaultLoginWait
}

func toolTextResult(text string) map[string]any {
	return map[string]any{
		"content": []map[string]any{{"type": "text", "text": text}},
	}
}

func (s *server) handleLocalTool(id json.RawMessage, name string, raw []byte) {
	switch name {
	case "auth.status":
		s.writeResult(id, s.handleAuthStatus())
	case "auth.login":
		var call struct {
			Params struct {
				Arguments struct {
					Force bool `json:"force"`
				} `json:"arguments"`
			} `json:"params"`
		}
		_ = json.Unmarshal(raw, &call)
		s.writeResult(id, s.handleAuthLogin(call.Params.Arguments.Force))
	}
}

func (s *server) handleAuthStatus() map[string]any {
	status := map[string]any{
		"profile": s.deps.Profile,
		"apiBase": s.base,
	}
	creds, paired := api.LoadCredentialsBound(s.deps.ConfigDir, s.base)
	status["paired"] = paired
	if paired && creds.ExpiresAt > 0 {
		status["tokenExpiresAt"] = time.Unix(creds.ExpiresAt, 0).UTC().Format(time.RFC3339)
	}
	if !paired {
		if raw, ok := api.LoadCredentials(s.deps.ConfigDir); ok && raw.BaseURL != "" {
			status["credentialsBoundTo"] = raw.BaseURL
		}
	}
	tracking := "default"
	if s.deps.RootConfigDir != "" {
		if g, err := api.LoadGlobalConfig(s.deps.RootConfigDir); err == nil {
			tracking = g.ResolvedTrackingProfile()
		}
	}
	status["trackingProfile"] = tracking
	status["dirtySessions"] = s.countDirtySessions(tracking)

	text, err := json.Marshal(status)
	if err != nil {
		return toolTextResult("auth.status failed to serialize")
	}
	return toolTextResult(string(text))
}

// countDirtySessions counts snapshots still awaiting upload in the
// TRACKING profile's state dir — that is where hook data accumulates,
// regardless of which profile this proxy serves.
func (s *server) countDirtySessions(trackingProfile string) int {
	if trackingProfile == api.TrackingDisabled || s.deps.StateDirFor == nil {
		return 0
	}
	stateDir, err := s.deps.StateDirFor(trackingProfile)
	if err != nil {
		return 0
	}
	entries, err := os.ReadDir(filepath.Join(stateDir, "sessions"))
	if err != nil {
		return 0
	}
	dirty := 0
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(stateDir, "sessions", entry.Name()))
		if err != nil {
			continue
		}
		var snap struct {
			Dirty bool `json:"dirty"`
		}
		if json.Unmarshal(data, &snap) == nil && snap.Dirty {
			dirty++
		}
	}
	return dirty
}

func (s *server) handleAuthLogin(force bool) map[string]any {
	if _, paired := api.LoadCredentialsBound(s.deps.ConfigDir, s.base); paired {
		if !force {
			return toolTextResult(fmt.Sprintf(
				"FocusAlly is already connected for profile %s (backend %s). To re-login (renew scopes or switch accounts), call auth.login with force: true.",
				s.deps.Profile, s.base,
			))
		}
		// Forced re-login: drop the pair so the poller mints a fresh
		// code; the client is told the server tools are gone until the
		// new approval lands.
		api.DeleteCredentials(s.deps.ConfigDir)
		s.noteUnpaired()
	}
	pending, ok := pairing.Bootstrap(s.deps.ConfigDir, s.deps.Profile, s.deps.Spawn, s.loginWait())
	if !ok {
		return toolTextResult(
			"Pairing started, but the code is not ready yet. Call auth.login again in a few seconds.",
		)
	}
	return toolTextResult(fmt.Sprintf(
		"FocusAlly pairing code: %s (valid until %s). Ask the user to approve it in the FocusAlly app: Profile → MCP keys → enter code — on any device where they are signed in. The connection completes automatically once approved; the full tool list will appear then.",
		pairing.FormatCode(pending.Code),
		pending.ExpiresAt.UTC().Format(time.RFC3339),
	))
}
