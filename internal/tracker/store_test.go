package tracker

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestSaveLoadRoundTrip(t *testing.T) {
	store := NewStore(t.TempDir())
	var s State
	s.Apply(Event{Kind: WorkBegin, AgentKind: "claude", SessionID: "abc", ProjectPath: "/p", At: base})
	s.Apply(Event{Kind: WorkEnd, AgentKind: "claude", SessionID: "abc", At: base.Add(time.Minute)})

	if err := store.Save("abc", s); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("abc")
	if err != nil {
		t.Fatal(err)
	}
	if got.ExternalSessionID != "abc" || len(got.ActiveIntervals) != 1 ||
		!got.ActiveIntervals[0].End.Time.Equal(base.Add(time.Minute)) {
		t.Fatalf("round trip mismatch: %+v", got)
	}
}

func TestLoadMissingYieldsZeroState(t *testing.T) {
	store := NewStore(t.TempDir())
	got, err := store.Load("nope")
	if err != nil || got.ExternalSessionID != "" {
		t.Fatalf("missing file must yield zero state, got %+v err %v", got, err)
	}
}

func TestLoadCorruptYieldsZeroState(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o700)
	os.WriteFile(store.Path("bad"), []byte("{not json"), 0o600)
	got, err := store.Load("bad")
	if err != nil || got.ExternalSessionID != "" {
		t.Fatalf("corrupt file must yield zero state, got %+v err %v", got, err)
	}
}

func TestSaveIsAtomicNoTempLeftovers(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	if err := store.Save("s", State{ExternalSessionID: "s", AgentKind: "claude"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(filepath.Join(dir, "sessions"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "s.json" {
		t.Fatalf("unexpected files after save: %v", entries)
	}
	data, _ := os.ReadFile(store.Path("s"))
	if !json.Valid(data) {
		t.Fatal("saved file is not valid JSON")
	}
}

func TestSanitizedSessionIDPaths(t *testing.T) {
	store := NewStore(t.TempDir())
	p := store.Path("../../evil/../id with spaces")
	if filepath.Base(p) != ".._.._evil_.._id_with_spaces.json" {
		t.Fatalf("sanitized path = %s", p)
	}
	if filepath.Dir(p) != store.sessionsDir() {
		t.Fatalf("path escaped sessions dir: %s", p)
	}
}

func TestLockIsExclusive(t *testing.T) {
	store := NewStore(t.TempDir())
	inFirst := make(chan struct{})
	releaseFirst := make(chan struct{})
	firstDone := make(chan struct{})

	go func() {
		defer close(firstDone)
		store.WithLock("s", func() error {
			close(inFirst)
			<-releaseFirst
			return nil
		})
	}()

	<-inFirst
	secondEntered := make(chan time.Time, 1)
	go func() {
		store.WithLock("s", func() error {
			secondEntered <- time.Now()
			return nil
		})
	}()

	select {
	case <-secondEntered:
		t.Fatal("second locker entered while first held the lock")
	case <-time.After(150 * time.Millisecond):
	}
	close(releaseFirst)
	<-firstDone
	select {
	case <-secondEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("second locker never entered after release")
	}
}

func TestStaleLockIsStolen(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	os.MkdirAll(filepath.Join(dir, "sessions"), 0o700)
	lockPath := store.Path("s") + ".lock"
	os.WriteFile(lockPath, []byte("999999"), 0o600)
	old := time.Now().Add(-time.Minute)
	os.Chtimes(lockPath, old, old)

	entered := false
	if err := store.WithLock("s", func() error { entered = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !entered {
		t.Fatal("stale lock must be stolen")
	}
}

// TestConcurrentSubagentEventsSameSession simulates parallel subagent
// tool calls racing on one session file: every event must land, exactly
// one session file must exist.
func TestConcurrentSubagentEventsSameSession(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	const workers = 8
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			at := base.Add(time.Duration(n) * time.Second)
			err := store.WithLock("shared", func() error {
				s, err := store.Load("shared")
				if err != nil {
					return err
				}
				s.Apply(Event{Kind: Heartbeat, AgentKind: "claude", SessionID: "shared", At: at})
				return store.Save("shared", s)
			})
			if err != nil {
				t.Error(err)
			}
		}(i)
	}
	wg.Wait()

	entries, _ := os.ReadDir(filepath.Join(dir, "sessions"))
	var files []string
	for _, e := range entries {
		files = append(files, e.Name())
	}
	if len(files) != 1 {
		t.Fatalf("parallel same-session events must share one file, got %v", files)
	}
	got, err := store.Load("shared")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.ActiveIntervals) != 1 || !got.Dirty {
		t.Fatalf("merged state after race: %+v", got)
	}
}
