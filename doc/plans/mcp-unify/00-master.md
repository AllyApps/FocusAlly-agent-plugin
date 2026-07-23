# MCP unify — master plan

Create a task list from this plan before starting implementation.

Repo: `~/Documents/Code/Projects/Back/FocusAlly-agent-plugin`. Goal: one Go binary
owns credentials for BOTH the hook-based session tracking and the interactive
FocusAlly MCP. The binary gains an MCP stdio proxy mode that transparently
forwards JSON-RPC to the backend `POST <apiBase>/mcp` (tools are NEVER hardcoded
in Go — they come from the server via `tools/list` passthrough), injects the
Bearer token, refreshes it, and exposes local `auth.login` / `auth.status` tools
for in-chat re-login. Multiple named profiles isolate accounts/backends. The
backend repo (`FocusAlly-back`) is NOT touched: the raw HTTP MCP stays fully
usable standalone (that is a hard requirement).

Fixed user decisions (do not re-litigate):
1. Full named profiles in this task (own apiBase, credentials, pairing, state).
2. Hook tracking goes to exactly ONE profile (`trackingProfile` global setting);
   `"none"` disables tracking entirely — the MCP-only mode.
3. Registration of the default MCP server is declared by the plugin itself
   (`.mcp.json` with `${CLAUDE_PLUGIN_ROOT}`); the legacy
   `claude mcp add --transport http --header "Authorization: Bearer …"` flow is
   removed and the stale registration deleted during migration. Extra profiles
   are registered manually via `claude mcp add … tracker mcp --profile <name>`.
4. While unpaired, the SessionStart pairing message is shown at most once per
   DAY, and tracking can be switched off entirely (`trackingProfile: "none"`).
5. Pairing requests the FULL scope set (user decision after plan review):
   `sessions:read sessions:write tasks:read tasks:write priorities:read
   priorities:write devices:read sync:read agent:write` — one approval powers
   both tracking and every interactive tool, and re-login via `auth.login`
   loses nothing. (The backend's `tools/list` filters by token scopes; with
   only the legacy `agent:write` the unified MCP would expose a single tool.)
   The user can still narrow scopes per-key in the app afterwards.

Steps (separate atomic plan files, each executable independently with a clean
context):

| # | File | Depends on | Parallel? |
|---|------|-----------|-----------|
| 1 | `01-profiles.md` — profile storage layout, credentials↔apiBase binding, migration from the flat layout (incl. deleting the stale `focusally` http registration) | — | first |
| 2 | `02-mcp-proxy.md` — `tracker mcp` stdio JSON-RPC passthrough proxy with auth injection and serialized refresh | 1 | after 1 |
| 3 | `03-auth-tools.md` — local `auth.login` / `auth.status` tools, unpaired-mode responses, `tools/list_changed` after login | 2 | after 2 |
| 4 | `04-hooks-registration.md` — hook routing via `trackingProfile`, daily unpaired message, plugin-declared MCP server, removal of `RegisterMCPServer`, docs, binary rebuild | 1, 3 | last |

All steps are sequential (each builds on the previous); none may run in
parallel. After each step: `go test ./...` green, `go vet ./...` clean. The
implementer may ask the user questions if anything is unclear. Precision
matters; nothing outside the plugin repo may change, and existing hook-tracking
behaviour (event folding, flush policy, interval semantics) must not regress.
