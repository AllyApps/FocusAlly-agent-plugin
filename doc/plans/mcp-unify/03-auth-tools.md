# Step 3 — Local `auth.login` / `auth.status` tools, unpaired mode, list_changed

Create a task list from this plan before starting implementation.

Repo: `~/Documents/Code/Projects/Back/FocusAlly-agent-plugin`. Requires step 2
(proxy) complete. Read first: `internal/mcpproxy/proxy.go`,
`internal/pairing/pairing.go`, master plan `00-master.md`. Precision matters;
the passthrough contract from step 2 must not regress — local tools are an
overlay, never a replacement for server tools.

## Behaviour

Two LOCAL tools exist on every profile's proxy, always callable, appended to the
server's `tools/list` result (they are the only tool definitions living in Go —
everything else keeps coming from the server):

- `auth.status` — no input. Returns one `text` content item with a compact JSON
  object: `{profile, apiBase, paired, tokenExpiresAt?, credentialsBoundTo?,
  trackingProfile, dirtySessions}`. (`tokenExpiresAt` answers the original
  "login silently expired" complaint; `credentialsBoundTo` surfaces an apiBase
  mismatch. Pending-pairing details are NOT reported — that is `auth.login`'s
  job.)
  `dirtySessions` = count of `*.json` session files with `"dirty":true` in the
  TRACKING profile's state dir (that is where hook data accumulates);
  `trackingProfile` comes from `tracker.json` (`"none"` reported as-is).
- `auth.login` — no input. If paired: returns `already connected` text (with
  apiBase). Otherwise: resume-or-mint a pairing code exactly like the
  SessionStart path (reuse `pairing.LoadPending` / the spawn of a detached
  `tracker pair --profile <name>` poller; wait up to ~2 s for the code file),
  then return text: the formatted code, its expiry, and the approval
  instructions (FocusAlly app → Profile → MCP keys → enter code; any device).
  Never blocks until approval — the detached poller finishes the exchange.

Unpaired mode (from this step on):

- `initialize` while unpaired → local result: negotiated `protocolVersion`
  (client's if `2025-06-18`, else `2025-06-18`), `capabilities.tools.listChanged
  = true`, `serverInfo {name: "FocusAlly", version: "tracker"}`, and
  `instructions` explaining: not connected, which profile, call `auth.login`.
- `tools/list` while unpaired → local list containing ONLY the two auth tools.
- After pairing completes, the proxy emits
  `{"jsonrpc":"2.0","method":"notifications/tools/list_changed"}` so the client
  re-fetches the now-full list.

## Steps

1. **`internal/mcpproxy/localtools.go`** (new) — definitions and handlers:
   - Static JSON schemas for the two tools (empty-object input schemas),
     descriptions written for agent consumption (mention re-login and that
     approval happens in the FocusAlly app).
   - `handleAuthStatus(deps)` / `handleAuthLogin(deps)` returning MCP
     `tools/call` results (single `text` content). Dirty-session scan reads
     each `*.json` in the tracking profile's sessions dir, tolerating unreadable
     files.
   - `auth.login` pairing bootstrap: factor the mint-or-resume +
     `spawnSelf("pair", "--profile", …)` + bounded wait (reuse the existing
     `waitForPending` logic) into a small helper shared with `cmd`'s
     `surfacePairing` — one code path, one behaviour. The spawn function is
     injectable for tests.

2. **`internal/mcpproxy/proxy.go`** — routing extensions:
   - `tools/call` with `params.name` ∈ {`auth.login`, `auth.status`} → local
     handler (peek at `name` via a minimal struct decode; other params stay
     raw). Local tools work in BOTH paired and unpaired states.
   - `tools/list`: paired → forward, then decode ONLY `result.tools` array as
     `[]json.RawMessage`, append the two local tool objects, re-emit (server
     tool objects stay byte-preserved); unpaired/second-401 → local-only list.
   - `initialize`: implement the full unpaired local result (replacing step 2's
     minimal stub) per Behaviour above.
   - Forwarded `tools/call` that ends in the unpaired-shaped reply (second 401,
     step 2) — keep, but the text now also names the exact tool to call.

3. **Pairing-completion watcher** — in `Serve`, a goroutine that, WHILE
   unpaired, polls `LoadCredentialsBound` every 3 s (cheap stat+read). On
   transition to paired: emit `notifications/tools/list_changed` via the
   serialized writer, and stop polling until unpaired again (a later definitive
   refresh rejection flips it back on and emits `list_changed` again so the
   client drops the server tools). No polling at all while paired — the 401
   path already detects loss.

4. **`cmd/tracker/main.go`** — `surfacePairing` switches to the shared
   bootstrap helper from step 1 (message text unchanged here; step 4 owns the
   throttle/text changes).

5. **Tests** (`internal/mcpproxy/localtools_test.go` + extensions):
   - unpaired: `initialize` local shape, `tools/list` = exactly the two auth
     tools, forwarded `tools/call` → unpaired-shaped `isError` result.
   - paired: `tools/list` = server tools (byte-preserved) + the two appended;
     name collision guard — if the server ever ships `auth.login`, the local
     one wins and the server duplicate is dropped (assert the dedup).
   - `auth.status`: reports paired/unpaired, counts dirty sessions from a
     fixture state dir, reflects `trackingProfile: none`.
   - `auth.login`: unpaired mints/resumes and returns the code (fake spawn
     records invocation; pending file injected); paired returns
     already-connected; expired pending file is treated as absent.
   - watcher: creds file appearing → exactly one `list_changed` notification
     on the wire; disappearing after definitive rejection → one more.

6. `go test ./...`, `go vet ./...` green.
