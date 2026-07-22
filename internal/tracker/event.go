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
	// WorkEnd closes the open interval (agent finished its turn).
	WorkEnd
	// SessionFinish closes the open interval and stamps endedAt.
	SessionFinish
)

// Event is one agent activity observation.
type Event struct {
	Kind        EventKind
	AgentKind   string
	SessionID   string
	ProjectPath string
	At          time.Time
}
