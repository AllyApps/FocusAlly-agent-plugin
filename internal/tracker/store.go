package tracker

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	lockRetryInterval = 20 * time.Millisecond
	lockTimeout       = 2 * time.Second
	lockStaleAfter    = 10 * time.Second
)

// Store persists one State per session as JSON under
// <dir>/sessions/<session_id>.json with atomic writes (temp + rename)
// and a portable lock file (O_CREATE|O_EXCL — flock is not portable
// to Windows).
type Store struct {
	dir string
}

func NewStore(dir string) *Store { return &Store{dir: dir} }

func (st *Store) sessionsDir() string { return filepath.Join(st.dir, "sessions") }

func (st *Store) Path(sessionID string) string {
	return filepath.Join(st.sessionsDir(), sanitizeID(sessionID)+".json")
}

func sanitizeID(id string) string {
	return strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
			return r
		default:
			return '_'
		}
	}, id)
}

// WithLock runs fn while holding the session's lock file. A lock older
// than lockStaleAfter is treated as leaked by a crashed process and is
// stolen.
func (st *Store) WithLock(sessionID string, fn func() error) error {
	if err := os.MkdirAll(st.sessionsDir(), 0o700); err != nil {
		return err
	}
	lockPath := st.Path(sessionID) + ".lock"
	deadline := time.Now().Add(lockTimeout)
	for {
		f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
		if err == nil {
			fmt.Fprintf(f, "%d", os.Getpid())
			f.Close()
			break
		}
		if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if info, statErr := os.Stat(lockPath); statErr == nil &&
			time.Since(info.ModTime()) > lockStaleAfter {
			os.Remove(lockPath)
			continue
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("tracker: lock timeout for session %s", sessionID)
		}
		time.Sleep(lockRetryInterval)
	}
	defer os.Remove(lockPath)
	return fn()
}

// Load reads the session state; a missing file yields a zero State.
func (st *Store) Load(sessionID string) (State, error) {
	var s State
	data, err := os.ReadFile(st.Path(sessionID))
	if errors.Is(err, fs.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if err := json.Unmarshal(data, &s); err != nil {
		// Corrupt state loses history but must never wedge tracking.
		return State{}, nil
	}
	return s, nil
}

// Save writes the state atomically: temp file in the same directory,
// then rename.
func (st *Store) Save(sessionID string, s State) error {
	if err := os.MkdirAll(st.sessionsDir(), 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	target := st.Path(sessionID)
	tmp, err := os.CreateTemp(st.sessionsDir(), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return err
	}
	if err := os.Rename(tmpName, target); err != nil {
		os.Remove(tmpName)
		return err
	}
	return nil
}
