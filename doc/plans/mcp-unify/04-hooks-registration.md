# Step 4 — Hook routing, daily unpaired message, plugin MCP declaration, cleanup

Create a task list from this plan before starting implementation.

Repo: `~/Documents/Code/Projects/Back/FocusAlly-agent-plugin`. Requires steps 1
and 3 complete. Read first: `cmd/tracker/main.go`, `internal/pairing/pairing.go`,
`.claude-plugin/plugin.json`, `hooks/hooks.json`, `scripts/run.sh`, `README.md`,
master plan `00-master.md`. Precision matters; hook latency discipline (fast,
silent, exit 0) must not regress.

## Steps

1. **Unpaired SessionStart message** (`internal/pairing/pairing.go`,
   `cmd/tracker/main.go`):
   - `codeShowThrottle`: `time.Hour` → `24 * time.Hour`.
   - Message text gains the MCP path:
     `FocusAlly tracking is not connected — approve code %s in the FocusAlly
     app (Profile → MCP keys → enter code), or ask the agent to call the
     focusally auth.login tool.` (and the code-less variant analogously).
   - The `trackingProfile: "none"` short-circuit from step 1 already suppresses
     everything — verify with a test: no state writes, no pairing spawn, no
     stdout.

2. **Delete `RegisterMCPServer`** (`internal/pairing/pairing.go`) and its last
   call site in the pairing success path. Pairing now ends at "credentials
   saved". The only `claude` shell-out left in the codebase is the migration's
   one-time `claude mcp remove -s user focusally` (step 1).

3. **Plugin-declared MCP server** — new `.mcp.json` at the plugin root:
   ```json
   {
     "mcpServers": {
       "focusally": {
         "command": "${CLAUDE_PLUGIN_ROOT}/scripts/run-mcp.sh"
       }
     }
   }
   ```
   Serves the `default` profile (no `--profile` argument). Verify the plugin
   manifest layout matches what Claude Code expects for plugin MCP servers
   (`.mcp.json` beside `.claude-plugin/plugin.json`); if the local Claude Code
   version resolves plugin MCP config from a different location, follow the
   documented location — do not guess silently, check the docs
   (`claude-code-guide` agent) and note the source in the commit message.
   Windows: `run-mcp.sh` matches `run.sh`'s existing sh-dispatcher approach —
   platform parity is inherited, not expanded.

4. **README.md** — rewrite the relevant sections:
   - What gets installed: hooks + the `focusally` MCP server, both from the
     plugin, zero manual steps; token lives only in
     `profiles/<name>/credentials.json` (0600).
   - Re-login: ask the agent to call `auth.login`, or approve the SessionStart
     code; `auth.status` for diagnostics.
   - Profiles: layout, `tracker mcp --profile <name>`, registering an extra
     profile: `claude mcp add -s user focusally-<name> --
     <abs-path-to>/bin/tracker-<platform> mcp --profile <name>`; per-profile
     pairing; `tracker.json` / `trackingProfile` semantics including `"none"`
     (MCP-only mode).
   - Migration note: the legacy header-based `focusally` http registration is
     removed automatically on first run after update.

5. **Binaries** — `make build-all` regenerates the committed per-platform
   binaries; `make test` green first. Do not hand-edit anything under `bin/`.

6. **End-to-end smoke** (manual, document results in the final report):
   - `printf '…initialize…\n…tools/list…\n' | ./tracker mcp --profile default`
     against the local backend: unpaired → auth tools only; after approving a
     login → server tools + auth tools.
   - A real Claude Code session: hooks write state for the tracking profile;
     `claude mcp list` shows `focusally` (plugin, stdio) connected.

7. `go test ./...`, `go vet ./...` green.
