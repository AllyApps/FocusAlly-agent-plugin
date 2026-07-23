package tracker

import "time"

// EventKind is the agent-agnostic activity vocabulary. Concrete agents
// (Claude today, Codex later) map their hook payloads onto these in
// their own adapter package.
type EventKind int

const (
	// SessionBegin creates or refreshes the session record.
	SessionBegin EventKind = iota
	// WorkBegin opens a work interval (user handed the agent a task).
	WorkBegin
	// Heartbeat refreshes lastActivityAt and opens an interval if none
	// is open (covers resumed sessions and long turns).
	Heartbeat
	// WorkEnd marks the main agent's turn as finished. The open
	// interval closes only once no subagents are alive either.
	WorkEnd
	// SubagentBegin marks a spawned subagent (live-subagent refcount
	// +1). A working subagent IS activity: opens an interval if none.
	SubagentBegin
	// SubagentEnd marks a finished subagent (refcount -1, floor 0).
	// Brings the session to rest — and closes the interval — only if
	// the main agent has also stopped.
	SubagentEnd
	// SessionFinish closes the open interval and stamps endedAt.
	SessionFinish
	// AwaitBegin marks the agent blocking on a user answer (a question
	// via AskUserQuestion/ExitPlanMode). It closes the work interval at
	// the question so the pause shows live; if the answer returns within
	// handoffGlueGap the resuming event re-glues the interval, so a short
	// wait stays continuous and only a longer one leaves a visible gap.
	AwaitBegin
)

// Event is one agent activity observation.
type Event struct {
	Kind        EventKind
	AgentKind   string
	SessionID   string
	ProjectPath string
	// FromSubagent marks events fired inside a subagent call (the
	// Claude payload carries agent_id there). Subagent heartbeats must
	// not clear the main agent's stopped flag.
	FromSubagent bool
	At           time.Time
}
