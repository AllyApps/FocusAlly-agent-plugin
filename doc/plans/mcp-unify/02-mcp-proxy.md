# Step 2 — `tracker mcp`: stdio JSON-RPC passthrough proxy

Create a task list from this plan before starting implementation.

Repo: `~/Documents/Code/Projects/Back/FocusAlly-agent-plugin`. Requires step 1
(profiles) to be complete. Read first: `internal/api/client.go`,
`internal/api/config.go`, `cmd/tracker/main.go`, master plan `00-master.md`, and
the backend contract in
`~/Documents/Code/Projects/Back/FocusAlly-back/Services/ApiGateway/Sources/Lib/MCP/MCPRoutes.swift`
(read-only — the backend is NOT modified). Precision matters; the raw HTTP MCP
must keep working for anyone who connects it directly.

## Contract facts (verified against the backend)

- `POST <apiBase>/mcp` is a single-shot stateless JSON-RPC 2.0 endpoint: no
  SSE, no GET, no `Mcp-Session-Id`. Methods: `initialize`, `ping`,
  `tools/list`, `tools/call`; anything else → `-32601`.
- Auth: opaque Bearer; HTTP 401 = token rejected (only this triggers refresh);
  JSON-RPC `-32001` covers rate-limit/insufficient-scope and must NOT trigger
  refresh.
- `tools/list` is already scope-filtered server-side; protocol version
  `2025-06-18`; MCP stdio transport is newline-delimited JSON.

Therefore the proxy is a pipe, not an MCP framework: no SDK, zero new module
dependencies. Tools are NEVER enumerated or modeled in Go.

## Steps

1. **`internal/mcpproxy/proxy.go`** (new package) — the serve loop:
   - `Serve(in io.Reader, out io.Writer, deps Deps)` where `Deps` carries the
     profile name, config/credentials accessors, and an `http.Client`
     (injectable for tests). `bufio.Scanner` with a generous buffer
     (≥ 4 MiB, matching `maxHookPayload`) reads newline-delimited messages;
     all writes to `out` go through one mutex-guarded writer that appends
     `\n` and compacts JSON (`json.Compact`) so a message can never span lines.
   - Per message, unmarshal ONLY the envelope: `{jsonrpc, id, method}` via
     `json.RawMessage` for `id`; keep the raw bytes for forwarding — params
     pass through byte-identical.
   - Routing:
     - message without `id` (client notification, e.g.
       `notifications/initialized`, `notifications/cancelled`) → drop silently
       (the backend does not know notifications).
     - `ping` → answer locally `{"jsonrpc":"2.0","id":…,"result":{}}` (works
       unpaired, saves a round-trip).
     - `initialize` → step 3 owns final behaviour; in THIS step: forward via
       the same `forward()` path as any request (so an expired-but-refreshable
       token is refreshed-and-retried instead of presenting as offline), then
       patch the successful result before writing: set
       `result.capabilities.tools.listChanged = true` (needed later for
       `tools/list_changed`). Only after the refresh-retry fails fall through
       to the local offline initialize (see step 3; for this step a minimal
       local result with `protocolVersion` echoing the client's requested
       version if it is `"2025-06-18"`, else `"2025-06-18"`,
       `capabilities.tools.listChanged`, `serverInfo {name:"FocusAlly",
       version:"tracker"}` is enough).
     - everything else with an `id` → `forward()`.
   - Requests are processed sequentially in arrival order — the backend calls
     are short-lived; note this is deliberate (no pipelining state to get
     wrong).

2. **`forward()`** — POST the raw message to `<apiBase>/mcp` with
   `Authorization: Bearer <access>` and `Content-Type: application/json`:
   - HTTP 200 → relay the response body verbatim (compacted) to `out`.
   - HTTP 401 → run the serialized refresh (below), retry ONCE; a second 401 →
     unpaired-shaped reply: for `tools/call` a JSON-RPC **result** with
     `{"content":[{"type":"text","text":"FocusAlly is not connected for
     profile <name>. Call the auth.login tool to connect."}],"isError":true}`;
     for other methods a JSON-RPC error `-32002` with the same message. (Tool
     errors as `isError` results keep agents able to self-recover — they can
     read the text and call `auth.login`.)
   - Transport error / non-200-non-401 → JSON-RPC error `-32000` with a short
     `"focusally proxy: <cause>"` message. Never crash the loop.

3. **Serialized refresh** — `internal/api/refresh.go` (new): extract the
   refresh logic out of `cmd/tracker/main.go` `refreshCredentials` into
   `api.RefreshUnderLock(profileConfigDir, base, clientID) (Credentials, bool)`:
   - Take `credentials.lock` in the profile dir (create-excl + stale reclaim,
     same pattern as `pairing.lock`).
   - After acquiring, RE-READ credentials; if the access token on disk differs
     from the one that just got 401 (another process already refreshed) →
     return the on-disk pair without hitting the network.
   - Otherwise call `api.Refresh`; on `IsDefinitiveTokenRejection` delete
     credentials (forces re-pairing) and return `ok=false`; transient errors
     leave credentials in place, `ok=false`.
   - `cmd/tracker/main.go` `runFlush` switches to this helper (its private
     `refreshCredentials` is deleted) — flush and proxy now share one
     refresh path and cannot double-rotate against each other.

4. **`cmd/tracker/main.go`** — new mode `tracker mcp [--profile <name>]`:
   - Runs migration, validates the profile name, resolves dirs, then
     `mcpproxy.Serve(os.Stdin, os.Stdout, deps)` until stdin EOF. Unlike hook
     mode this process MAY write errors to stderr (Claude Code surfaces MCP
     server stderr in logs), but must never write non-JSON-RPC bytes to
     stdout.
   - An unpaired profile is a VALID serve state (step 3 gives it tools); do
     not exit.

5. **Pairing scope** (`internal/pairing/pairing.go`) — per the user decision
   recorded in the master plan, `Scope` becomes the full set:
   `"sessions:read sessions:write tasks:read tasks:write priorities:read
   priorities:write devices:read sync:read agent:write"`. One approval powers
   tracking AND the interactive tools; `auth.login` re-pairing loses nothing.
   Update the pairing test fixtures accordingly.

6. **`scripts/run-mcp.sh`** (new) — same per-platform dispatcher shape as
   `scripts/run.sh`, but `exec "$bin" mcp "$@"` (no hook argument). Used by the
   plugin MCP declaration in step 4.

7. **Tests** (`internal/mcpproxy/proxy_test.go`, table-driven, using
   `net/http/httptest` as the fake backend and in-memory pipes for stdio):
   - passthrough: `tools/list` and `tools/call` bodies arrive at the backend
     byte-identical (including unknown future fields in params) and the reply
     relays verbatim; Bearer header injected.
   - notifications dropped; `ping` answered locally; sequential ordering
     (ids answered in request order).
   - 401 → refresh → retry-once success; 401 → definitive refresh rejection →
     unpaired-shaped reply and credentials deleted; `-32001` passes through
     WITHOUT refresh.
   - refresh lock: two concurrent 401 handlers perform exactly one token
     round-trip (assert via fake token endpoint counter).
   - initialize patch: `capabilities.tools.listChanged == true` in the relayed
     result; initialize with an expired token → refresh → retry → relayed
     server result (not the offline stub).
   - oversized / malformed input line → loop survives, JSON-RPC parse error
     for requests with an id.

8. `go test ./...`, `go vet ./...` green.
