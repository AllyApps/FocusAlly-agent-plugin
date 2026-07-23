package mcpproxy

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/pairing"
)

// Vars so tests can shrink the waits.
var (
	// elicitResponseTimeout bounds how long the dialog may stay open;
	// clamped further by the pairing code's own expiry.
	elicitResponseTimeout = 15 * time.Minute
	// elicitCredsWait is how long to wait, after the user pressed
	// Accept, for the detached poller to finish the code exchange.
	elicitCredsWait = 45 * time.Second
	elicitCredsPoll = 500 * time.Millisecond
)

// elicitLoginAndRetry handles an unpaired tools/call by asking the USER
// directly through MCP elicitation (a native Claude Code dialog): it
// surfaces the pairing code, waits for the in-app approval, then
// transparently retries the original call — the tool just works, as if
// the login had never expired. Returns true when it wrote the reply.
//
// Falls back (returns false → the usual unpaired reply) when the
// client never declared the elicitation capability, when the user
// declined a previous dialog (muted until the pairing state changes),
// or on any failure along the way.
func (s *server) elicitLoginAndRetry(id json.RawMessage, raw []byte) bool {
	if !s.elicitationOK.Load() || s.elicitMuted.Load() {
		return false
	}
	pending, ok := pairing.Bootstrap(s.deps.ConfigDir, s.deps.Profile, s.deps.Spawn, s.loginWait())
	if !ok {
		return false
	}

	timeout := time.Until(pending.ExpiresAt)
	if timeout <= 0 || timeout > elicitResponseTimeout {
		timeout = elicitResponseTimeout
	}
	respRaw, err := s.request("elicitation/create", map[string]any{
		"message": fmt.Sprintf(
			"FocusAlly is not connected (profile %s). Approve code %s in the FocusAlly app: Profile → MCP keys → enter code — on any device where you are signed in. Press Accept after approving to retry the call.",
			s.deps.Profile, pairing.FormatCode(pending.Code),
		),
		"requestedSchema": map[string]any{"type": "object", "properties": map[string]any{}},
	}, timeout)
	if err != nil {
		return false
	}
	var resp struct {
		Result struct {
			Action string `json:"action"`
		} `json:"result"`
		Error *struct{} `json:"error"`
	}
	if json.Unmarshal(respRaw, &resp) != nil || resp.Error != nil {
		return false
	}
	if resp.Result.Action != "accept" {
		// Declined/cancelled: stop popping dialogs until the pairing
		// state actually changes.
		s.elicitMuted.Store(true)
		return false
	}

	// The user says the code is approved; the detached poller still
	// needs a poll cycle to exchange it for tokens.
	deadline := time.Now().Add(elicitCredsWait)
	for time.Now().Before(deadline) {
		if s.credentialsPresent() {
			body, outcome, cause := s.forward(raw)
			switch outcome {
			case fwdOK:
				s.out.writeMessage(body)
			case fwdError:
				s.writeError(id, -32000, "focusally proxy: "+cause)
			case fwdUnpaired:
				return false
			}
			return true
		}
		time.Sleep(elicitCredsPoll)
	}
	return false
}
