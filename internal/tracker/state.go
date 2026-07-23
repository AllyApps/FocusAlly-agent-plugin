package tracker

import "time"

const (
	// handoffGlueGap: adjacent intervals separated by less than this
	// are one continuous stretch of work. Sub-30 s pauses are event
	// jitter, not a real break; genuine waits (e.g. the user answering
	// a question) stay visible. Main↔subagent handoffs need no glue —
	// the subagent refcount keeps the interval open across them. It is
	// also the user-answer threshold: AwaitBegin closes the interval at
	// the question, and if the answer lands within this gap the resuming
	// event re-glues it into one continuous stretch.
	handoffGlueGap = 30 * time.Second
	// intervalCap bounds the array defensively; oldest intervals drop.
	intervalCap = 500
	// flushDebounce: non-forced flushes happen at most this often.
	flushDebounce = 20 * time.Second
)

// Interval is one closed (or currently open, End == nil) stretch of
// active agent work.
type Interval struct {
	Start WireTime  `json:"start"`
	End   *WireTime `json:"end"`
}

// State is the full snapshot of one agent session, mirroring the
// AgentSession contract fields plus local flush bookkeeping.
type State struct {
	AgentKind         string     `json:"agentKind"`
	ExternalSessionID string     `json:"externalSessionId"`
	MachineName       string     `json:"machineName,omitempty"`
	ProjectPath       string     `json:"projectPath,omitempty"`
	StartedAt         WireTime   `json:"startedAt"`
	LastActivityAt    WireTime   `json:"lastActivityAt"`
	EndedAt           *WireTime  `json:"endedAt,omitempty"`
	ActiveIntervals   []Interval `json:"activeIntervals"`

	// SubagentCount is the live-subagent refcount: the main agent's
	// Stop must not close the interval while subagents still work.
	SubagentCount int `json:"subagentCount,omitempty"`
	// MainStopped remembers that the main agent finished its turn, so
	// the SubagentEnd that brings the refcount to zero knows to close
	// the interval. Cleared by main-thread activity only.
	MainStopped bool `json:"mainStopped,omitempty"`

	Dirty       bool      `json:"dirty"`
	LastFlushAt *WireTime `json:"lastFlushAt,omitempty"`
}

// Apply folds one event into the state and reports whether it closed
// the open interval (a natural force-flush moment). A zero-value State
// is a valid target for the first event of a session.
func (s *State) Apply(ev Event) (closedInterval bool) {
	at := WireTimeOf(ev.At)
	if s.ExternalSessionID == "" {
		s.ExternalSessionID = ev.SessionID
		s.AgentKind = ev.AgentKind
		s.StartedAt = at
	}
	if ev.ProjectPath != "" {
		s.ProjectPath = ev.ProjectPath
	}
	s.LastActivityAt = at
	s.Dirty = true

	switch ev.Kind {
	case SessionBegin:
		// Session boundaries only; no interval work. Reset the
		// refcount — missed SubagentStop events (crash, kill) must not
		// leak a stuck count into the resumed session.
		s.SubagentCount = 0
		s.MainStopped = false
	case AwaitBegin:
		// The agent blocked on a user answer: close the work interval
		// at the question so the pause shows live. If the answer lands
		// within handoffGlueGap, the resuming event re-glues it into one
		// continuous stretch; a longer wait stays a visible gap. A live
		// subagent keeps working, so leave the interval open then.
		if s.SubagentCount == 0 {
			closedInterval = s.closeOpenInterval(at)
		}
	case WorkBegin, Heartbeat:
		if !ev.FromSubagent {
			s.MainStopped = false
		}
		if s.openInterval() == nil {
			s.ActiveIntervals = append(s.ActiveIntervals, Interval{Start: at})
		}
	case SubagentBegin:
		s.SubagentCount++
		if s.openInterval() == nil {
			s.ActiveIntervals = append(s.ActiveIntervals, Interval{Start: at})
		}
	case SubagentEnd:
		if s.SubagentCount > 0 {
			s.SubagentCount--
		}
		if s.SubagentCount == 0 && s.MainStopped {
			closedInterval = s.closeOpenInterval(at)
		}
	case WorkEnd:
		s.MainStopped = true
		if s.SubagentCount == 0 {
			closedInterval = s.closeOpenInterval(at)
		}
		// Otherwise the session is still working through its
		// subagents — keep the interval open, refresh activity only.
	case SessionFinish:
		closedInterval = s.closeOpenInterval(at)
		s.EndedAt = &at
	}
	s.normalizeIntervals()
	return closedInterval
}

func (s *State) openInterval() *Interval {
	if n := len(s.ActiveIntervals); n > 0 && s.ActiveIntervals[n-1].End == nil {
		return &s.ActiveIntervals[n-1]
	}
	return nil
}

func (s *State) closeOpenInterval(at WireTime) bool {
	open := s.openInterval()
	if open == nil {
		return false
	}
	if at.Before(open.Start.Time) {
		at = open.Start
	}
	open.End = &at
	return true
}

// normalizeIntervals merges adjacent intervals separated by less than
// handoffGlueGap and caps the array at intervalCap (keeping the
// newest).
func (s *State) normalizeIntervals() {
	if len(s.ActiveIntervals) < 2 {
		return
	}
	merged := s.ActiveIntervals[:1]
	for _, next := range s.ActiveIntervals[1:] {
		last := &merged[len(merged)-1]
		if last.End != nil && next.Start.Sub(last.End.Time) < handoffGlueGap {
			last.End = next.End
			continue
		}
		merged = append(merged, next)
	}
	if len(merged) > intervalCap {
		merged = merged[len(merged)-intervalCap:]
	}
	s.ActiveIntervals = merged
}

// FlushDecision says whether the caller should spawn a flush now.
type FlushDecision int

const (
	FlushSkip FlushDecision = iota
	FlushNow
)

// DecideFlush implements the flush policy: force events flush
// immediately; otherwise flush at most once per flushDebounce window.
// The caller stamps LastFlushAt via MarkFlushSpawned when it acts.
func (s *State) DecideFlush(force bool, now time.Time) FlushDecision {
	if !s.Dirty {
		return FlushSkip
	}
	if force {
		return FlushNow
	}
	if s.LastFlushAt == nil || now.Sub(s.LastFlushAt.Time) >= flushDebounce {
		return FlushNow
	}
	return FlushSkip
}

// MarkFlushSpawned stamps the debounce clock at spawn time so parallel
// hooks do not stampede the backend while the detached flush runs.
func (s *State) MarkFlushSpawned(now time.Time) {
	at := WireTimeOf(now)
	s.LastFlushAt = &at
}

// MarkFlushed clears Dirty after a successful report, but only if no
// newer activity landed while the flush was in flight.
func (s *State) MarkFlushed(snapshotLastActivity WireTime) {
	if !s.LastActivityAt.After(snapshotLastActivity.Time) {
		s.Dirty = false
	}
}

// ForcesFlush reports whether the event kind requires an immediate
// flush regardless of the debounce window. Interval closes signalled
// by Apply's return value additionally force a flush (e.g. the
// SubagentEnd that brings the session to rest).
func (k EventKind) ForcesFlush() bool {
	return k == WorkEnd || k == SessionFinish
}
