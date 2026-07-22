# focusally-tracker

Claude Code plugin that reports coding-agent session activity to
[FocusAlly](https://focus.withally.app), so agent work shows up as an
"AI rail" on the Day timeline. No slash commands, no runtime
dependencies — a static Go binary per platform, committed to this repo,
invoked by Claude Code hooks.

## Install

Prototype (local checkout):

```bash
claude --plugin-dir /path/to/FocusAlly-agent-plugin
```

Marketplace (once published):

```
/plugin marketplace add <marketplace>
/plugin install focusally-tracker
```

Nothing else. On the first `SessionStart` without credentials the
tracker starts a detached pairing flow and shows a message like:

> FocusAlly tracking is not connected — approve code `ABCD-2345` in the
> FocusAlly app (Profile → MCP keys → enter code).

Approve the code in the FocusAlly app on **any** of your devices —
the app is not required on the machine running Claude Code. On macOS,
if the app is installed locally, a `focusally://mcp-authorize` deeplink
pops the approval window automatically. One approval covers both the
activity reporting and (if `claude` is on PATH) registers the
`focusally` MCP server for interactive tools.

## Platform support

| Platform | Status |
|---|---|
| macOS arm64 / amd64 | supported |
| Linux amd64 / arm64 | supported |
| Windows via WSL | supported (runs the Linux binary) |
| Windows via Git Bash / MSYS | supported (`tracker-windows-amd64.exe`) |
| **Bare Windows without Git for Windows** | **not supported yet** — Claude Code runs hooks under PowerShell there, and the `sh` dispatcher (`scripts/run.sh`) cannot run. Deferred until validated on a real Windows machine (see also claude-code issue #18610 on `${CLAUDE_PLUGIN_ROOT}` path handling on native Windows). |

## How it works

Hooks (`SessionStart`, `UserPromptSubmit`, `PostToolUse`, `Stop`,
`SessionEnd`) invoke `scripts/run.sh`, which picks
`bin/tracker-<os>-<arch>` by `uname` and execs it. The binary folds the
event into a per-session snapshot on disk (<100 ms, no network), then —
at most once per 20 s, or immediately on `Stop`/`SessionEnd` — re-execs
itself detached to flush the snapshot to the backend via the
`agent_sessions.report` MCP tool. Subagent activity carries the parent
session id and never creates a second session. Network failures are
silent; the snapshot stays dirty and the next flush retries. The
tracker never blocks or disturbs the Claude session.

## What data is sent

Per session, the full current snapshot (nothing else — no prompts, no
code, no tool payloads):

- agent kind (`claude`) and Claude Code session id
- machine hostname and project directory path (`cwd`)
- session start / last-activity / end timestamps
- active-work intervals (when the agent was actually working)

## Where things live

- Credentials: `<os-config-dir>/focusally/credentials.json` (`0600`)
  — access + refresh token obtained via OAuth PKCE pairing.
  `<os-config-dir>` is `~/Library/Application Support` on macOS,
  `~/.config` on Linux, `%AppData%` on Windows.
- Config: `<os-config-dir>/focusally/config.json` (`baseUrl` override
  for dev, OAuth `clientId`).
- Session state: `$XDG_STATE_HOME/focusally/sessions/` or
  `~/.local/state/focusally/sessions/`.

## Disconnect

1. Revoke the authorization in the FocusAlly app: Profile → MCP keys.
2. Delete the config dir: `rm -rf "<os-config-dir>/focusally"`.
3. Optionally `claude mcp remove -s user focusally` and uninstall the
   plugin.

## Development

Requires Go (stdlib only, `CGO_ENABLED=0`).

```bash
make test        # unit tests
make build       # host binary ./tracker
make build-all   # cross-compile the committed bin/ matrix
```

`bin/` binaries are committed on purpose: a marketplace install is a
git clone and must work without a bootstrap download. Rebuild them with
`make build-all` whenever the Go source changes.

The event→state core (`internal/tracker`) is agent-agnostic; Claude
specifics live in `internal/claude`. Adding another agent (e.g. Codex)
means a new adapter package plus its hook wiring — the state, flush,
and pairing machinery is shared.
