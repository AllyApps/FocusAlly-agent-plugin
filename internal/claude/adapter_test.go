package claude

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/tracker"
)

var now = time.Date(2026, 7, 22, 12, 0, 0, 0, time.UTC)

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestMapEventFixtures(t *testing.T) {
	cases := []struct {
		file  string
		event string
		kind  tracker.EventKind
	}{
		{"session_start.json", "SessionStart", tracker.SessionBegin},
		{"user_prompt_submit.json", "UserPromptSubmit", tracker.WorkBegin},
		{"post_tool_use.json", "PostToolUse", tracker.Heartbeat},
		{"stop.json", "Stop", tracker.WorkEnd},
		{"session_end.json", "SessionEnd", tracker.SessionFinish},
	}
	for _, tc := range cases {
		ev, err := MapEvent(tc.event, fixture(t, tc.file), now)
		if err != nil {
			t.Fatalf("%s: %v", tc.file, err)
		}
		if ev.Kind != tc.kind {
			t.Fatalf("%s: kind = %d, want %d", tc.file, ev.Kind, tc.kind)
		}
		if ev.AgentKind != "claude" {
			t.Fatalf("%s: agentKind = %q", tc.file, ev.AgentKind)
		}
		if ev.SessionID != "3f8f2b1c-9a7d-4e21-b3aa-6c1a0f9d2e55" {
			t.Fatalf("%s: sessionID = %q", tc.file, ev.SessionID)
		}
		if ev.ProjectPath != "/Users/dev/Projects/FocusAlly" {
			t.Fatalf("%s: projectPath = %q", tc.file, ev.ProjectPath)
		}
		if !ev.At.Equal(now) {
			t.Fatalf("%s: at = %v", tc.file, ev.At)
		}
	}
}

// Subagent events carry the parent session_id — they must map onto the
// SAME session (same SessionID) so the tracker never opens a second
// lane for subagent activity.
func TestSubagentEventSharesParentSession(t *testing.T) {
	parent, err := MapEvent("PostToolUse", fixture(t, "post_tool_use.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	sub, err := MapEvent("PostToolUse", fixture(t, "post_tool_use_subagent.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	if sub.SessionID != parent.SessionID {
		t.Fatalf("subagent sessionID %q != parent %q", sub.SessionID, parent.SessionID)
	}
	if sub.Kind != tracker.Heartbeat {
		t.Fatalf("subagent event kind = %d, want Heartbeat", sub.Kind)
	}
}

func TestPayloadHookEventNameWinsOverArgument(t *testing.T) {
	ev, err := MapEvent("Stop", fixture(t, "session_end.json"), now)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Kind != tracker.SessionFinish {
		t.Fatalf("payload hook_event_name must win, got kind %d", ev.Kind)
	}
}

func TestMapEventRejectsBadInput(t *testing.T) {
	if _, err := MapEvent("Stop", []byte("{not json"), now); err == nil {
		t.Fatal("malformed JSON must error")
	}
	if _, err := MapEvent("Stop", []byte(`{"hook_event_name":"Stop"}`), now); err == nil {
		t.Fatal("missing session_id must error")
	}
	if _, err := MapEvent("Notification", []byte(`{"session_id":"x","hook_event_name":"Notification"}`), now); err == nil {
		t.Fatal("unhandled event must error")
	}
}
