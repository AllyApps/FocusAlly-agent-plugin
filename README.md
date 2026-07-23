# focusally-tracker

Claude Code plugin that connects Claude Code to
[FocusAlly](https://focus.withally.app): it reports coding-agent session
activity (the "AI rail" on the Day timeline) **and** provides the
`focusally` MCP server for interactive tools (tasks, sessions,
priorities). No slash commands, no runtime dependencies — a static Go
binary per platform, committed to this repo, invoked by Claude Code
hooks and as an MCP stdio server.

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

Nothing else — the plugin installs both halves with zero manual steps:

- **Hooks** fold session activity into local state and flush it to the
  backend.
- **The `focusally` MCP server** (declared by the plugin's `.mcp.json`,
  stdio transport) proxies every tool from the backend and adds two
  local tools: `auth.status` and `auth.login`.

The access token lives ONLY in
`<os-config-dir>/focusally/profiles/<name>/credentials.json` (`0600`) —
it never appears in any Claude config file.

## Login and re-login

On the first `SessionStart` without credentials the tracker starts a
detached pairing flow and shows a message (at most once per day):

> FocusAlly tracking is not connected — approve code `ABCD-2345` in the
> FocusAlly app (Profile → MCP keys → enter code), or ask the agent to
> call the focusally auth.login tool.

Approve the code in the FocusAlly app on **any** of your devices — the
app is not required on the machine running Claude Code. On macOS, if
the app is installed locally, a `focusally://mcp-authorize` deeplink
pops the approval window automatically.

Re-login is one chat message away: ask the agent to call the
`auth.login` tool — it replies with a fresh code to approve in the app.
While already connected, `auth.login` with `force: true` drops the
current login and re-pairs in place (renew scopes, switch accounts) —
no shell commands involved.

When the login has lapsed and the agent calls any FocusAlly tool, the
proxy also pops a **native dialog** (MCP elicitation, Claude Code
v2.1.76+): it shows the pairing code; approve it in the app, press
Accept, and the original tool call is retried transparently — as if
the login had never expired. Declining the dialog mutes it until the
connection state changes; clients without elicitation support just get
the plain "call auth.login" reply.
`auth.status` reports the connection state (paired, token expiry, which
profile tracking writes to, pending un-flushed sessions) — use it when
tracking seems silent.

One approval covers everything: pairing requests the full scope set
(sessions/tasks/priorities read+write, devices:read, sync:read,
agent:write), so the same token powers both activity reporting and all
interactive tools. Scopes can be narrowed per-key in the app.

## Profiles

All per-account data lives under a named profile
(`<os-config-dir>/focusally/profiles/<name>/`), so multiple
accounts/backends never conflict:

- The plugin's default MCP server and hook tracking use the `default`
  profile.
- `tracker.json` at `<os-config-dir>/focusally/` selects which ONE
  profile hook tracking writes to:
  `{"trackingProfile": "work"}`. The literal `"none"` disables tracking
  entirely (MCP-only mode); empty/missing means `default`.
- Extra profiles are registered manually as extra MCP servers:

  ```bash
  claude mcp add -s user focusally-work -- \
    <abs-path-to-plugin>/bin/tracker-<os>-<arch> mcp --profile work
  ```

  Each profile pairs separately (ask the agent on that server to call
  `auth.login`). Credentials are bound to the profile's `baseUrl` — if
  the backend URL changes, the profile reads as unpaired instead of
  sending data to the wrong place.

**Migration:** the first run after this update automatically moves the
old flat file layout into `profiles/default/` and removes the legacy
header-based `focusally` HTTP registration from the Claude config (the
one that embedded the Bearer token). Nothing to do manually.

## Platform support

| Platform | Status |
|---|---|
| macOS arm64 / amd64 | supported |
| Linux amd64 / arm64 | supported |
| Windows via WSL | supported (runs the Linux binary) |
| Windows via Git Bash / MSYS | supported (`tracker-windows-amd64.exe`) |
| **Bare Windows without Git for Windows** | **not supported yet** — Claude Code runs hooks under PowerShell there, and the `sh` dispatchers (`scripts/run.sh`, `scripts/run-mcp.sh`) cannot run. Deferred until validated on a real Windows machine (see also claude-code issue #18610 on `${CLAUDE_PLUGIN_ROOT}` path handling on native Windows). |

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

The MCP server (`scripts/run-mcp.sh` → `tracker mcp`) is a transparent
stdio⇄HTTP proxy to `POST <apiBase>/mcp`: every request passes through
byte-identical with the profile's Bearer token injected, and token
refresh on 401 is serialized against the flush path via
`credentials.lock`. Tools are never hardcoded in the binary — the list
always comes from the server. While unpaired, the proxy serves only the
two local auth tools and notifies the client (`tools/list_changed`)
the moment pairing completes. The backend's raw HTTP MCP stays fully
usable standalone; the proxy is an overlay, not a replacement.

## What data is sent

Per session, the full current snapshot (nothing else — no prompts, no
code, no tool payloads):

- agent kind (`claude`) and Claude Code session id
- machine hostname and project directory path (`cwd`)
- session start / last-activity / end timestamps
- active-work intervals (when the agent was actually working)

## Where things live

- Global settings: `<os-config-dir>/focusally/tracker.json`
  (`trackingProfile`). `<os-config-dir>` is
  `~/Library/Application Support` on macOS, `~/.config` on Linux,
  `%AppData%` on Windows.
- Per profile (`<os-config-dir>/focusally/profiles/<name>/`):
  - `credentials.json` (`0600`) — access + refresh token from OAuth
    PKCE pairing, bound to the backend `baseUrl` that issued them.
  - `config.json` — `baseUrl` override for dev, OAuth `clientId`.
  - `pairing.json` / `pairing-shown` / locks — transient pairing state.
- Session state: `$XDG_STATE_HOME/focusally/profiles/<name>/sessions/`
  or `~/.local/state/focusally/profiles/<name>/sessions/`.

## Disconnect

1. Revoke the authorization in the FocusAlly app: Profile → MCP keys.
2. Delete the config dir: `rm -rf "<os-config-dir>/focusally"`.
3. Uninstall the plugin (its MCP server and hooks go with it). Remove
   any manually added extra-profile servers with
   `claude mcp remove -s user focusally-<name>`.

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
