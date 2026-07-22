package tracker

import "time"

const (
	// mergeGap: adjacent intervals separated by less than this are one
	// continuous stretch of work.
	mergeGap = 30 * time.Second
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

	Dirty       bool      `json:"dirty"`
	LastFlushAt *WireTime `json:"lastFlushAt,omitempty"`
}

// Apply folds one event into the state. A zero-value State is a valid
// target for the first event of a session.
func (s *State) Apply(ev Event) {
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
		// Session boundaries only; no interval work.
	case WorkBegin, Heartbeat:
		if s.openInterval() == nil {
			s.ActiveIntervals = append(s.ActiveIntervals, Interval{Start: at})
		}
	case WorkEnd:
		s.closeOpenInterval(at)
	case SessionFinish:
		s.closeOpenInterval(at)
		s.EndedAt = &at
	}
	s.normalizeIntervals()
}

func (s *State) openInterval() *Interval {
	if n := len(s.ActiveIntervals); n > 0 && s.ActiveIntervals[n-1].End == nil {
		return &s.ActiveIntervals[n-1]
	}
	return nil
}

func (s *State) closeOpenInterval(at WireTime) {
	if open := s.openInterval(); open != nil {
		if at.Before(open.Start.Time) {
			at = open.Start
		}
		open.End = &at
	}
}

// normalizeIntervals merges adjacent intervals separated by less than
// mergeGap and caps the array at intervalCap (keeping the newest).
func (s *State) normalizeIntervals() {
	if len(s.ActiveIntervals) < 2 {
		return
	}
	merged := s.ActiveIntervals[:1]
	for _, next := range s.ActiveIntervals[1:] {
		last := &merged[len(merged)-1]
		if last.End != nil && next.Start.Sub(last.End.Time) < mergeGap {
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
// flush regardless of the debounce window.
func (k EventKind) ForcesFlush() bool {
	return k == WorkEnd || k == SessionFinish
}
