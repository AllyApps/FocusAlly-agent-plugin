// Package claude maps Claude Code hook payloads onto the agent-agnostic
// tracker events. This is the only place that knows Claude's hook wire
// shape; a future Codex adapter is a sibling package.
package claude

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/tracker"
)

const AgentKind = "claude"

// Payload is the subset of the Claude Code hook stdin JSON the tracker
// consumes. Subagent events carry the parent session_id (plus agent_id)
// — they map onto the same session, never a new one.
type Payload struct {
	SessionID     string `json:"session_id"`
	CWD           string `json:"cwd"`
	HookEventName string `json:"hook_event_name"`
	// AgentID is present only when the hook fires inside a subagent
	// call — its presence distinguishes subagent tool calls from
	// main-thread ones (per the hooks doc).
	AgentID string `json:"agent_id"`
	// ToolName names the tool a PreToolUse/PostToolUse hook fired for.
	// A blocking question tool (AskUserQuestion/ExitPlanMode) marks the
	// start of a user-answer wait.
	ToolName string `json:"tool_name"`
}

// MapEvent translates a hook invocation into a tracker event.
// eventName is the dispatcher argument; the stdin payload's
// hook_event_name wins when both are present.
func MapEvent(eventName string, raw []byte, now time.Time) (tracker.Event, error) {
	var p Payload
	if err := json.Unmarshal(raw, &p); err != nil {
		return tracker.Event{}, fmt.Errorf("claude: bad hook payload: %w", err)
	}
	if p.HookEventName != "" {
		eventName = p.HookEventName
	}
	if p.SessionID == "" {
		return tracker.Event{}, fmt.Errorf("claude: payload has no session_id")
	}

	var kind tracker.EventKind
	switch eventName {
	case "SessionStart":
		kind = tracker.SessionBegin
	case "UserPromptSubmit":
		kind = tracker.WorkBegin
	case "PreToolUse":
		if p.ToolName == "AskUserQuestion" || p.ToolName == "ExitPlanMode" {
			kind = tracker.AwaitBegin
		} else {
			kind = tracker.Heartbeat
		}
	case "PostToolUse":
		kind = tracker.Heartbeat
	case "SubagentStart":
		kind = tracker.SubagentBegin
	case "SubagentStop":
		kind = tracker.SubagentEnd
	case "Stop":
		kind = tracker.WorkEnd
	case "SessionEnd":
		kind = tracker.SessionFinish
	default:
		return tracker.Event{}, fmt.Errorf("claude: unhandled hook event %q", eventName)
	}

	return tracker.Event{
		Kind:         kind,
		AgentKind:    AgentKind,
		SessionID:    p.SessionID,
		ProjectPath:  p.CWD,
		FromSubagent: p.AgentID != "",
		At:           now,
	}, nil
}
