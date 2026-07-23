package tracker

import (
	"testing"
	"time"
)

var base = time.Date(2026, 7, 22, 10, 0, 0, 0, time.UTC)

func ev(kind EventKind, at time.Time) Event {
	return Event{Kind: kind, AgentKind: "claude", SessionID: "s1", ProjectPath: "/proj", At: at}
}

func TestWorkBeginOpensAndStopCloses(t *testing.T) {
	var s State
	s.Apply(ev(SessionBegin, base))
	if len(s.ActiveIntervals) != 0 {
		t.Fatalf("SessionBegin must not open an interval, got %d", len(s.ActiveIntervals))
	}
	s.Apply(ev(WorkBegin, base.Add(1*time.Minute)))
	if len(s.ActiveIntervals) != 1 || s.ActiveIntervals[0].End != nil {
		t.Fatalf("WorkBegin must open one interval: %+v", s.ActiveIntervals)
	}
	s.Apply(ev(WorkEnd, base.Add(5*time.Minute)))
	if s.ActiveIntervals[0].End == nil {
		t.Fatal("WorkEnd must close the open interval")
	}
	if got := s.ActiveIntervals[0].End.Time; !got.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("interval end = %v", got)
	}
	if !s.StartedAt.Time.Equal(base) {
		t.Fatalf("startedAt = %v, want %v", s.StartedAt.Time, base)
	}
	if !s.LastActivityAt.Time.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("lastActivityAt = %v", s.LastActivityAt.Time)
	}
}

func TestHeartbeatOpensIntervalIfNoneOpen(t *testing.T) {
	var s State
	s.Apply(ev(SessionBegin, base))
	s.Apply(ev(Heartbeat, base.Add(2*time.Minute)))
	if len(s.ActiveIntervals) != 1 || s.ActiveIntervals[0].End != nil {
		t.Fatalf("Heartbeat with no open interval must open one: %+v", s.ActiveIntervals)
	}
	s.Apply(ev(Heartbeat, base.Add(3*time.Minute)))
	if len(s.ActiveIntervals) != 1 {
		t.Fatalf("Heartbeat with open interval must not open another: %+v", s.ActiveIntervals)
	}
	if !s.LastActivityAt.Time.Equal(base.Add(3 * time.Minute)) {
		t.Fatalf("lastActivityAt = %v", s.LastActivityAt.Time)
	}
}

func TestWorkBeginKeepsExistingOpenInterval(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(ev(WorkBegin, base.Add(time.Minute)))
	if len(s.ActiveIntervals) != 1 {
		t.Fatalf("second WorkBegin must not open a second interval: %+v", s.ActiveIntervals)
	}
	if !s.ActiveIntervals[0].Start.Time.Equal(base) {
		t.Fatalf("open interval start moved: %v", s.ActiveIntervals[0].Start.Time)
	}
}

// Gaps shorter than the 30 s handoff-glue threshold carry no signal
// (agent-handoff noise, e.g. a background agent finishing and the main
// agent resuming) — the tracker glues them so the UI never sees them.
func TestMergeAdjacentIntervalsUnderHandoffGlueGap(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(ev(WorkEnd, base.Add(1*time.Minute)))
	// New interval 29 s after the previous closed → still glue noise.
	s.Apply(ev(WorkBegin, base.Add(1*time.Minute+29*time.Second)))
	s.Apply(ev(WorkEnd, base.Add(4*time.Minute)))
	if len(s.ActiveIntervals) != 1 {
		t.Fatalf("gap < 30 s must merge, got %d intervals", len(s.ActiveIntervals))
	}
	got := s.ActiveIntervals[0]
	if !got.Start.Time.Equal(base) || !got.End.Time.Equal(base.Add(4*time.Minute)) {
		t.Fatalf("merged interval = %+v", got)
	}
}

func TestNoMergeOverHandoffGlueGap(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(ev(WorkEnd, base.Add(1*time.Minute)))
	// 31 s gap — a real pause (e.g. the user answering), keep the split.
	s.Apply(ev(WorkBegin, base.Add(1*time.Minute+31*time.Second)))
	s.Apply(ev(WorkEnd, base.Add(5*time.Minute)))
	if len(s.ActiveIntervals) != 2 {
		t.Fatalf("gap > 30 s must stay separate, got %d intervals", len(s.ActiveIntervals))
	}
}

func TestMergePreservesOpenInterval(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(ev(WorkEnd, base.Add(1*time.Minute)))
	s.Apply(ev(WorkBegin, base.Add(1*time.Minute+5*time.Second)))
	if len(s.ActiveIntervals) != 1 {
		t.Fatalf("merge with open successor: got %d intervals", len(s.ActiveIntervals))
	}
	if s.ActiveIntervals[0].End != nil {
		t.Fatal("merged interval must stay open")
	}
}

func TestIntervalCapAt500(t *testing.T) {
	var s State
	at := base
	for i := 0; i < 600; i++ {
		s.Apply(ev(WorkBegin, at))
		at = at.Add(time.Minute)
		s.Apply(ev(WorkEnd, at))
		at = at.Add(3 * time.Minute) // 3 min gap > 30 s glue: no merging
	}
	if len(s.ActiveIntervals) != 500 {
		t.Fatalf("cap = 500, got %d", len(s.ActiveIntervals))
	}
	// Newest are kept: the last interval must end at the final WorkEnd.
	last := s.ActiveIntervals[len(s.ActiveIntervals)-1]
	if !last.End.Time.Equal(at.Add(-3 * time.Minute)) {
		t.Fatalf("cap must drop oldest, last interval = %+v", last)
	}
}

func TestSessionFinishClosesAndStampsEndedAt(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(ev(SessionFinish, base.Add(4*time.Minute)))
	if s.EndedAt == nil || !s.EndedAt.Time.Equal(base.Add(4*time.Minute)) {
		t.Fatalf("endedAt = %+v", s.EndedAt)
	}
	if s.ActiveIntervals[0].End == nil {
		t.Fatal("SessionFinish must close the open interval")
	}
}

func subEv(kind EventKind, at time.Time) Event {
	e := ev(kind, at)
	e.FromSubagent = true
	return e
}

// The root cause of handoff gaps: the main agent's Stop used to close
// the interval while background subagents were still working, and
// their next tool call reopened it. With the refcount, Stop keeps the
// interval open while subagents are alive.
func TestStopWithLiveSubagentsKeepsIntervalOpen(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(subEv(SubagentBegin, base.Add(1*time.Minute)))
	closed := s.Apply(ev(WorkEnd, base.Add(2*time.Minute)))
	if closed {
		t.Fatal("Stop with a live subagent must not report an interval close")
	}
	if s.openInterval() == nil {
		t.Fatal("Stop with a live subagent must keep the interval open")
	}
	if !s.MainStopped {
		t.Fatal("Stop must set MainStopped")
	}
	if !s.LastActivityAt.Time.Equal(base.Add(2 * time.Minute)) {
		t.Fatal("Stop must still refresh lastActivityAt")
	}
}

func TestSubagentStopToZeroAfterMainStopClosesInterval(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(subEv(SubagentBegin, base.Add(1*time.Minute)))
	s.Apply(subEv(SubagentBegin, base.Add(2*time.Minute)))
	s.Apply(ev(WorkEnd, base.Add(3*time.Minute)))

	if closed := s.Apply(subEv(SubagentEnd, base.Add(4*time.Minute))); closed {
		t.Fatal("first SubagentStop (count 2→1) must not close")
	}
	closed := s.Apply(subEv(SubagentEnd, base.Add(5*time.Minute)))
	if !closed || s.openInterval() != nil {
		t.Fatal("SubagentStop bringing the count to 0 after main Stop must close the interval")
	}
	if end := s.ActiveIntervals[len(s.ActiveIntervals)-1].End; !end.Time.Equal(base.Add(5 * time.Minute)) {
		t.Fatalf("interval closed at %v", end.Time)
	}
}

func TestSubagentStopToZeroBeforeMainStopKeepsIntervalOpen(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(subEv(SubagentBegin, base.Add(1*time.Minute)))
	s.Apply(subEv(SubagentEnd, base.Add(2*time.Minute)))
	if s.openInterval() == nil {
		t.Fatal("subagents done but main still working — interval stays open")
	}
}

func TestSubagentStartAfterStopContinuesWork(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(ev(WorkEnd, base.Add(1*time.Minute)))
	// A subagent spawning right after Stop is activity again; the
	// sub-glue-gap reopen merges into the same interval.
	s.Apply(subEv(SubagentBegin, base.Add(1*time.Minute+10*time.Second)))
	if s.openInterval() == nil {
		t.Fatal("SubagentStart after Stop must reopen an interval")
	}
	if len(s.ActiveIntervals) != 1 {
		t.Fatalf("sub-glue-gap reopen must merge, got %d intervals", len(s.ActiveIntervals))
	}
	// Main is still stopped: the subagent finishing brings rest.
	if closed := s.Apply(subEv(SubagentEnd, base.Add(2*time.Minute))); !closed {
		t.Fatal("last subagent finishing with main stopped must close")
	}
}

func TestSubagentHeartbeatDoesNotClearMainStopped(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(subEv(SubagentBegin, base.Add(1*time.Minute)))
	s.Apply(ev(WorkEnd, base.Add(2*time.Minute)))
	s.Apply(subEv(Heartbeat, base.Add(3*time.Minute)))
	if !s.MainStopped {
		t.Fatal("subagent heartbeat must not clear MainStopped")
	}
	s.Apply(ev(Heartbeat, base.Add(4*time.Minute)))
	if s.MainStopped {
		t.Fatal("main-thread heartbeat must clear MainStopped")
	}
}

func TestSessionStartResetsSubagentCount(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(subEv(SubagentBegin, base.Add(1*time.Minute)))
	s.Apply(subEv(SubagentBegin, base.Add(2*time.Minute)))
	s.Apply(ev(WorkEnd, base.Add(3*time.Minute)))

	// Resume (e.g. after crash — SubagentStop events were lost).
	s.Apply(ev(SessionBegin, base.Add(10*time.Minute)))
	if s.SubagentCount != 0 || s.MainStopped {
		t.Fatalf("SessionStart must reset refcount and MainStopped: %+v", s)
	}
	s.Apply(ev(WorkBegin, base.Add(11*time.Minute)))
	if closed := s.Apply(ev(WorkEnd, base.Add(12*time.Minute))); !closed {
		t.Fatal("after the reset, Stop with no subagents must close normally")
	}
}

// Drift scenario: a SubagentStop was lost (killed process), the
// refcount is stuck > 0, and Stop therefore leaves the interval open
// in STATE. This is accepted: the client clips an open interval to
// lastActivityAt + 2 min and treats the session as ended after 30 min
// of silence, so rendering damage is bounded; the next SessionStart
// resets the count.
func TestDriftStuckRefcountLeavesIntervalOpenButClientClipBounds(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(subEv(SubagentBegin, base.Add(1*time.Minute)))
	// SubagentStop never arrives.
	s.Apply(ev(WorkEnd, base.Add(2*time.Minute)))
	if s.openInterval() == nil {
		t.Fatal("stuck refcount keeps the interval open in state (by design)")
	}
	// Nothing in the tracker mutates state without an event — the open
	// interval's rendered extent is bounded client-side by
	// lastActivityAt, which stays frozen at the last real event.
	if !s.LastActivityAt.Time.Equal(base.Add(2 * time.Minute)) {
		t.Fatal("lastActivityAt must stay at the last real event")
	}
}

func TestSubagentEndFloorsAtZero(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(subEv(SubagentEnd, base.Add(time.Minute))) // drift: never started
	if s.SubagentCount != 0 {
		t.Fatalf("refcount must floor at 0, got %d", s.SubagentCount)
	}
	if s.openInterval() == nil {
		t.Fatal("MainStopped is false — interval must stay open")
	}
}

// A user-answer wait closes the work interval at once (live pause). A
// wait longer than handoffGlueGap stays a visible gap: the interval
// ends at the question time and the resume (the question tool's
// PostToolUse → Heartbeat) opens a fresh one at the answer time.
func TestAwaitClosesAtOnceAndLongWaitStaysSplit(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	closed := s.Apply(ev(AwaitBegin, base.Add(1*time.Minute)))
	if !closed {
		t.Fatal("AwaitBegin must close the interval at once (forces a flush → live pause)")
	}
	if end := s.ActiveIntervals[0].End; end == nil || !end.Time.Equal(base.Add(1*time.Minute)) {
		t.Fatalf("AwaitBegin must close at the question time: %+v", s.ActiveIntervals[0])
	}
	s.Apply(ev(Heartbeat, base.Add(21*time.Minute)))
	if len(s.ActiveIntervals) != 2 {
		t.Fatalf("wait > 30 s must stay two intervals, got %d: %+v", len(s.ActiveIntervals), s.ActiveIntervals)
	}
	if second := s.ActiveIntervals[1]; second.End != nil || !second.Start.Time.Equal(base.Add(21*time.Minute)) {
		t.Fatalf("second interval must open at the answer time: %+v", second)
	}
}

// A wait under handoffGlueGap re-glues on resume: the momentary close
// is retracted and the line stays one continuous stretch.
func TestAwaitShortWaitRegluesContinuous(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(ev(AwaitBegin, base.Add(1*time.Minute)))
	s.Apply(ev(Heartbeat, base.Add(1*time.Minute+15*time.Second)))
	if len(s.ActiveIntervals) != 1 || s.ActiveIntervals[0].End != nil {
		t.Fatalf("wait < 30 s must re-glue into one open interval: %+v", s.ActiveIntervals)
	}
	if !s.ActiveIntervals[0].Start.Time.Equal(base) {
		t.Fatalf("re-glued interval must keep the original start: %v", s.ActiveIntervals[0].Start.Time)
	}
}

// A long gap between two Heartbeats with no AwaitBegin (e.g. a slow
// non-blocking Bash build) must never split — only a genuine
// user-answer wait closes the interval.
func TestNoAwaitNoSplitOnLongHeartbeatGap(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	closed := s.Apply(ev(Heartbeat, base.Add(45*time.Second)))
	if closed {
		t.Fatal("a Heartbeat gap without AwaitBegin must not close the interval")
	}
	if len(s.ActiveIntervals) != 1 || s.ActiveIntervals[0].End != nil {
		t.Fatalf("slow tool (no AwaitBegin) must stay one open interval: %+v", s.ActiveIntervals)
	}
}

// A subagent working through the wait keeps the interval open — the
// refcount, not the question, governs rest, so no pause is shown.
func TestAwaitNoCloseWhileSubagentActive(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(subEv(SubagentBegin, base.Add(30*time.Second)))
	closed := s.Apply(ev(AwaitBegin, base.Add(1*time.Minute)))
	if closed {
		t.Fatal("a live subagent must suppress the await close")
	}
	if len(s.ActiveIntervals) != 1 || s.ActiveIntervals[0].End != nil {
		t.Fatalf("close must not happen while a subagent is active: %+v", s.ActiveIntervals)
	}
}

func TestFlushDebounce(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))

	if got := s.DecideFlush(false, base); got != FlushNow {
		t.Fatal("first flush (no lastFlushAt) must fire")
	}
	s.MarkFlushSpawned(base)
	if got := s.DecideFlush(false, base.Add(5*time.Second)); got != FlushSkip {
		t.Fatal("flush within 20 s of the last one must be debounced")
	}
	if got := s.DecideFlush(true, base.Add(5*time.Second)); got != FlushNow {
		t.Fatal("forced flush must bypass the debounce")
	}
	if got := s.DecideFlush(false, base.Add(25*time.Second)); got != FlushNow {
		t.Fatal("flush after the 20 s window must fire")
	}

	s.Dirty = false
	if got := s.DecideFlush(true, base.Add(time.Minute)); got != FlushSkip {
		t.Fatal("clean state must never flush")
	}
}

func TestMarkFlushedRespectsNewerActivity(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	snapshotActivity := s.LastActivityAt

	s.MarkFlushed(snapshotActivity)
	if s.Dirty {
		t.Fatal("MarkFlushed with unchanged activity must clear Dirty")
	}

	s.Apply(ev(Heartbeat, base.Add(time.Minute)))
	s.MarkFlushed(snapshotActivity)
	if !s.Dirty {
		t.Fatal("MarkFlushed must keep Dirty when newer activity landed mid-flight")
	}
}

func TestForcesFlush(t *testing.T) {
	forcing := map[EventKind]bool{
		SessionBegin: false, WorkBegin: false, Heartbeat: false,
		WorkEnd: true, SessionFinish: true,
	}
	for kind, want := range forcing {
		if kind.ForcesFlush() != want {
			t.Fatalf("ForcesFlush(%d) = %v, want %v", kind, !want, want)
		}
	}
}
