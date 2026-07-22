// tracker is the FocusAlly agent-tracking binary invoked by the Claude
// Code plugin's hook dispatcher (scripts/run.sh).
//
//	tracker hook <event>    fold one hook event (stdin JSON) into local
//	                        state; maybe spawn a detached flush/pairing
//	tracker flush <id>      report one session snapshot to the backend
//	tracker pair            run the OAuth PKCE pairing flow (detached)
//
// Every failure is silent (exit 0): the tracker must never disturb the
// Claude session.
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/api"
	"github.com/withally/focusally-agent-plugin/internal/claude"
	"github.com/withally/focusally-agent-plugin/internal/pairing"
	"github.com/withally/focusally-agent-plugin/internal/paths"
	"github.com/withally/focusally-agent-plugin/internal/tracker"
)

const maxHookPayload = 4 << 20

func main() {
	if len(os.Args) < 2 {
		return
	}
	switch os.Args[1] {
	case "hook":
		if len(os.Args) >= 3 {
			runHook(os.Args[2])
		}
	case "flush":
		if len(os.Args) >= 3 {
			runFlush(os.Args[2])
		}
	case "pair":
		configDir, err := paths.ConfigDir()
		if err == nil {
			pairing.Run(configDir)
		}
	}
}

// runHook is the latency-critical path: parse stdin, update state under
// the session lock, maybe spawn detached helpers, exit.
func runHook(eventName string) {
	raw, err := io.ReadAll(io.LimitReader(os.Stdin, maxHookPayload))
	if err != nil {
		return
	}
	ev, err := claude.MapEvent(eventName, raw, time.Now())
	if err != nil {
		return
	}

	stateDir, err := paths.StateDir()
	if err != nil {
		return
	}
	store := tracker.NewStore(stateDir)

	configDir, cfgErr := paths.ConfigDir()
	paired := false
	if cfgErr == nil {
		_, paired = api.LoadCredentials(configDir)
	}

	var flush bool
	err = store.WithLock(ev.SessionID, func() error {
		state, err := store.Load(ev.SessionID)
		if err != nil {
			return err
		}
		if state.MachineName == "" {
			if host, err := os.Hostname(); err == nil {
				state.MachineName = host
			}
		}
		closedInterval := state.Apply(ev)
		force := ev.Kind.ForcesFlush() || closedInterval
		if paired && state.DecideFlush(force, time.Now()) == tracker.FlushNow {
			flush = true
			state.MarkFlushSpawned(time.Now())
		}
		return store.Save(ev.SessionID, state)
	})
	if err != nil {
		return
	}

	if flush {
		spawnSelf("flush", ev.SessionID)
	}
	if !paired && ev.Kind == tracker.SessionBegin && cfgErr == nil {
		surfacePairing(configDir)
	}
}

// surfacePairing makes sure a pairing process is running and, at most
// once per hour, emits the SessionStart hook JSON with a user-visible
// systemMessage carrying the code (systemMessage is shown to the user
// and never enters model context).
func surfacePairing(configDir string) {
	// Always (re)spawn the pairing process: it resumes a persisted
	// pending pairing after process death/reboot, exits immediately if
	// a live poller holds the lock, and mints a fresh code only when
	// there is nothing to resume.
	spawnSelf("pair")
	if !pairing.ShouldShowCode(configDir) {
		return
	}
	pending, ok := pairing.LoadPending(configDir)
	if !ok {
		// Minting the code is two fast HTTPS round-trips; 1 s covers
		// the common case without stretching SessionStart. If the code
		// is not there yet, show a code-less notice now and the real
		// code on the next SessionStart (the throttle is stamped only
		// when the code itself is shown).
		pending, ok = waitForPending(configDir, time.Second)
	}
	var msg string
	if ok {
		pairing.MarkCodeShown(configDir)
		msg = fmt.Sprintf(
			"FocusAlly tracking is not connected — approve code %s in the FocusAlly app (Profile → MCP keys → enter code).",
			pairing.FormatCode(pending.Code),
		)
	} else {
		msg = "FocusAlly tracking is not connected — pairing started; the approval code will appear at the start of your next session."
	}
	out, err := json.Marshal(map[string]string{"systemMessage": msg})
	if err == nil {
		os.Stdout.Write(out)
	}
}

func waitForPending(configDir string, timeout time.Duration) (pairing.PendingFile, bool) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if p, ok := pairing.LoadPending(configDir); ok {
			return p, true
		}
		time.Sleep(100 * time.Millisecond)
	}
	return pairing.PendingFile{}, false
}

// runFlush reports one session's snapshot. Network errors are silent —
// the state stays dirty and the next flush retries.
func runFlush(sessionID string) {
	configDir, err := paths.ConfigDir()
	if err != nil {
		return
	}
	creds, ok := api.LoadCredentials(configDir)
	if !ok {
		return
	}
	cfg, err := api.LoadConfig(configDir)
	if err != nil {
		return
	}
	base := cfg.ResolvedBaseURL()

	stateDir, err := paths.StateDir()
	if err != nil {
		return
	}
	store := tracker.NewStore(stateDir)

	var snapshot tracker.State
	if err := store.WithLock(sessionID, func() error {
		s, err := store.Load(sessionID)
		snapshot = s
		return err
	}); err != nil || snapshot.ExternalSessionID == "" {
		return
	}

	// Refresh proactively when the access token is (nearly) expired —
	// the report would only bounce with a 401 anyway.
	if creds.NearExpiry(time.Now().Unix()) {
		refreshed, ok := refreshCredentials(configDir, base, cfg.ClientID, creds)
		if !ok {
			return // state stays dirty; next flush retries (or re-pairs)
		}
		creds = refreshed
	}

	err = api.Report(base, creds.AccessToken, snapshot)
	if _, unauthorized := err.(api.ErrUnauthorized); unauthorized {
		refreshed, ok := refreshCredentials(configDir, base, cfg.ClientID, creds)
		if !ok {
			return
		}
		creds = refreshed
		err = api.Report(base, creds.AccessToken, snapshot)
	}
	if err != nil {
		return
	}

	_ = store.WithLock(sessionID, func() error {
		s, err := store.Load(sessionID)
		if err != nil {
			return err
		}
		s.MarkFlushed(snapshot.LastActivityAt)
		return store.Save(sessionID, s)
	})
}

// refreshCredentials rotates the token pair. Credentials are dropped
// (forcing re-pairing) ONLY on a definitive grant rejection — the
// backend answering 400/401 to the refresh itself. Transport errors,
// 5xx, and 429 leave the credentials in place: the snapshot stays
// dirty and the next flush retries.
func refreshCredentials(configDir, base, clientID string, creds api.Credentials) (api.Credentials, bool) {
	refreshed, err := api.Refresh(base, clientID, creds)
	if err != nil {
		if api.IsDefinitiveTokenRejection(err) {
			api.DeleteCredentials(configDir)
		}
		return api.Credentials{}, false
	}
	if api.SaveCredentials(configDir, refreshed) != nil {
		return api.Credentials{}, false
	}
	// The MCP registration header embeds the access token — rewrite it.
	pairing.RegisterMCPServer(base, refreshed.AccessToken)
	return refreshed, true
}

// spawnSelf re-execs this binary with the given subcommand, fully
// detached, so the hook returns instantly.
func spawnSelf(args ...string) {
	self, err := os.Executable()
	if err != nil {
		return
	}
	cmd := exec.Command(self, args...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil
	detach(cmd)
	if cmd.Start() == nil {
		_ = cmd.Process.Release()
	}
}
