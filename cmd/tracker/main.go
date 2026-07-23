// tracker is the FocusAlly agent-tracking binary invoked by the Claude
// Code plugin's hook dispatcher (scripts/run.sh).
//
//	tracker hook <event>              fold one hook event (stdin JSON)
//	                                  into the tracking profile's state;
//	                                  maybe spawn a detached flush/pairing
//	tracker flush <id> [--profile X]  report one session snapshot
//	tracker pair [--profile X]        run the OAuth PKCE pairing flow
//	tracker mcp [--profile X]         serve the MCP stdio proxy
//
// Every failure is silent (exit 0): the tracker must never disturb the
// Claude session.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/withally/focusally-agent-plugin/internal/api"
	"github.com/withally/focusally-agent-plugin/internal/claude"
	"github.com/withally/focusally-agent-plugin/internal/mcpproxy"
	"github.com/withally/focusally-agent-plugin/internal/migrate"
	"github.com/withally/focusally-agent-plugin/internal/pairing"
	"github.com/withally/focusally-agent-plugin/internal/paths"
	"github.com/withally/focusally-agent-plugin/internal/tracker"
)

const maxHookPayload = 4 << 20

func main() {
	if len(os.Args) < 2 {
		return
	}
	migrate.Run()
	switch os.Args[1] {
	case "hook":
		if len(os.Args) >= 3 {
			runHook(os.Args[2], os.Stdin, os.Stdout)
		}
	case "flush":
		if len(os.Args) >= 3 {
			if profile, ok := parseProfile(os.Args[3:]); ok {
				runFlush(os.Args[2], profile)
			}
		}
	case "pair":
		if profile, ok := parseProfile(os.Args[2:]); ok {
			if configDir, err := paths.ProfileConfigDir(profile); err == nil {
				pairing.Run(configDir)
			}
		}
	case "mcp":
		runMCP(os.Args[2:])
	}
}

// runMCP serves the stdio MCP proxy until stdin EOF. Unlike hook mode
// it may report problems on stderr (Claude Code surfaces MCP server
// stderr in logs) — but stdout carries only JSON-RPC.
func runMCP(args []string) {
	profile, ok := parseProfile(args)
	if !ok {
		fmt.Fprintln(os.Stderr, "tracker mcp: invalid --profile (want [a-z0-9-]{1,32})")
		os.Exit(1)
	}
	configDir, err := paths.ProfileConfigDir(profile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tracker mcp:", err)
		os.Exit(1)
	}
	rootDir, err := paths.RootConfigDir()
	if err != nil {
		fmt.Fprintln(os.Stderr, "tracker mcp:", err)
		os.Exit(1)
	}
	err = mcpproxy.Serve(os.Stdin, os.Stdout, mcpproxy.Deps{
		Profile:       profile,
		ConfigDir:     configDir,
		Client:        &http.Client{Timeout: 60 * time.Second},
		RootConfigDir: rootDir,
		StateDirFor:   paths.ProfileStateDir,
		Spawn:         spawnSelf,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "tracker mcp:", err)
	}
}

// parseProfile reads an optional --profile flag from a subcommand's
// argument tail. Invalid names (path-traversal safety) fail closed.
func parseProfile(args []string) (string, bool) {
	fs := flag.NewFlagSet("tracker", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	profile := fs.String("profile", "default", "profile name")
	if fs.Parse(args) != nil {
		return "", false
	}
	if !paths.ValidProfileName(*profile) {
		return "", false
	}
	return *profile, true
}

// runHook is the latency-critical path: parse stdin, update state under
// the session lock, maybe spawn detached helpers, exit. Hooks have no
// --profile flag — they route to the ONE tracking profile named by
// tracker.json ("none" = tracking off, return before any state write).
func runHook(eventName string, stdin io.Reader, stdout io.Writer) {
	raw, err := io.ReadAll(io.LimitReader(stdin, maxHookPayload))
	if err != nil {
		return
	}
	ev, err := claude.MapEvent(eventName, raw, time.Now())
	if err != nil {
		return
	}

	rootDir, err := paths.RootConfigDir()
	if err != nil {
		return
	}
	gcfg, err := api.LoadGlobalConfig(rootDir)
	if err != nil {
		return
	}
	profile := gcfg.ResolvedTrackingProfile()
	if profile == api.TrackingDisabled || !paths.ValidProfileName(profile) {
		return
	}

	stateDir, err := paths.ProfileStateDir(profile)
	if err != nil {
		return
	}
	store := tracker.NewStore(stateDir)

	paired := false
	configDir, cfgErr := paths.ProfileConfigDir(profile)
	if cfgErr == nil {
		if cfg, err := api.LoadConfig(configDir); err == nil {
			_, paired = api.LoadCredentialsBound(configDir, cfg.ResolvedBaseURL())
		}
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
		spawnSelf("flush", ev.SessionID, "--profile", profile)
	}
	if !paired && ev.Kind == tracker.SessionBegin && cfgErr == nil {
		surfacePairing(configDir, profile, stdout)
	}
}

// surfacePairing makes sure a pairing process is running and, at most
// once per codeShowThrottle, emits the SessionStart hook JSON with a
// user-visible systemMessage carrying the code (systemMessage is shown
// to the user and never enters model context).
func surfacePairing(configDir, profile string, stdout io.Writer) {
	if !pairing.ShouldShowCode(configDir) {
		pairing.Bootstrap(configDir, profile, spawnSelf, 0)
		return
	}
	// Minting the code is two fast HTTPS round-trips; 1 s covers the
	// common case without stretching SessionStart. If the code is not
	// there yet, show a code-less notice now and the real code on the
	// next SessionStart (the throttle is stamped only when the code
	// itself is shown).
	pending, ok := pairing.Bootstrap(configDir, profile, spawnSelf, time.Second)
	var msg string
	if ok {
		pairing.MarkCodeShown(configDir)
		msg = fmt.Sprintf(
			"FocusAlly tracking is not connected — approve code %s in the FocusAlly app (Profile → MCP keys → enter code), or ask the agent to call the focusally auth.login tool.",
			pairing.FormatCode(pending.Code),
		)
	} else {
		msg = "FocusAlly tracking is not connected — pairing started; the approval code will appear at the start of your next session, or ask the agent to call the focusally auth.login tool."
	}
	out, err := json.Marshal(map[string]string{"systemMessage": msg})
	if err == nil {
		stdout.Write(out)
	}
}

// runFlush reports one session's snapshot from the given profile.
// Network errors are silent — the state stays dirty and the next flush
// retries. Credentials bound to a different backend also stay silent:
// the profile reads as unpaired rather than reporting to the wrong
// place.
func runFlush(sessionID, profile string) {
	configDir, err := paths.ProfileConfigDir(profile)
	if err != nil {
		return
	}
	cfg, err := api.LoadConfig(configDir)
	if err != nil {
		return
	}
	base := cfg.ResolvedBaseURL()
	creds, ok := api.LoadCredentialsBound(configDir, base)
	if !ok {
		return
	}

	stateDir, err := paths.ProfileStateDir(profile)
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
		refreshed, outcome := api.RefreshUnderLock(configDir, base, cfg.ClientID, creds.AccessToken)
		if outcome != api.RefreshOK {
			return // state stays dirty; next flush retries (or re-pairs)
		}
		creds = refreshed
	}

	err = api.Report(base, creds.AccessToken, snapshot)
	if _, unauthorized := err.(api.ErrUnauthorized); unauthorized {
		refreshed, outcome := api.RefreshUnderLock(configDir, base, cfg.ClientID, creds.AccessToken)
		if outcome != api.RefreshOK {
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
