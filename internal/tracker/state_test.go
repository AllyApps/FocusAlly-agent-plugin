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

func TestMergeAdjacentIntervalsUnder30s(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(ev(WorkEnd, base.Add(1*time.Minute)))
	// New interval 10 s after the previous closed → merge.
	s.Apply(ev(WorkBegin, base.Add(1*time.Minute+10*time.Second)))
	s.Apply(ev(WorkEnd, base.Add(2*time.Minute)))
	if len(s.ActiveIntervals) != 1 {
		t.Fatalf("gap < 30s must merge, got %d intervals", len(s.ActiveIntervals))
	}
	got := s.ActiveIntervals[0]
	if !got.Start.Time.Equal(base) || !got.End.Time.Equal(base.Add(2*time.Minute)) {
		t.Fatalf("merged interval = %+v", got)
	}
}

func TestNoMergeOver30s(t *testing.T) {
	var s State
	s.Apply(ev(WorkBegin, base))
	s.Apply(ev(WorkEnd, base.Add(1*time.Minute)))
	s.Apply(ev(WorkBegin, base.Add(2*time.Minute)))
	s.Apply(ev(WorkEnd, base.Add(3*time.Minute)))
	if len(s.ActiveIntervals) != 2 {
		t.Fatalf("gap >= 30s must stay separate, got %d intervals", len(s.ActiveIntervals))
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
		at = at.Add(time.Minute) // 1 min gap > 30 s: no merging
	}
	if len(s.ActiveIntervals) != 500 {
		t.Fatalf("cap = 500, got %d", len(s.ActiveIntervals))
	}
	// Newest are kept: the last interval must end at the final WorkEnd.
	last := s.ActiveIntervals[len(s.ActiveIntervals)-1]
	if !last.End.Time.Equal(at.Add(-time.Minute)) {
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
