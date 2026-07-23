# Step 1 — Profiles: storage layout, credentials↔apiBase binding, migration

Create a task list from this plan before starting implementation.

Repo: `~/Documents/Code/Projects/Back/FocusAlly-agent-plugin`. Read first:
`internal/paths/paths.go`, `internal/api/config.go`, `internal/tracker/store.go`,
`internal/pairing/pairing.go`, `cmd/tracker/main.go`, and the master plan
`00-master.md`. Precision matters; existing tracking behaviour must not regress.
The implementer may ask the user questions if anything is unclear.

## Target layout

```
<os-config-dir>/focusally/                      # ~/Library/Application Support/focusally on macOS
├── tracker.json                                # global: {"trackingProfile": "default"}
└── profiles/<name>/
    ├── config.json                             # {baseUrl?, clientId?} — per-profile
    ├── credentials.json                        # + new "baseUrl" binding field
    ├── credentials.lock                        # serializes token refresh (step 2 uses it)
    └── pairing.json / pairing-shown / pairing.lock
<state-dir>/focusally/profiles/<name>/sessions/<sessionId>.json[ .lock]
```

Profile names: `[a-z0-9-]{1,32}`, default name `default`. Reject anything else
(path-traversal safety) — invalid name ⇒ the command exits silently (hook
discipline) or, for `mcp` mode, refuses to start with an error on stderr.

## Steps

1. **`internal/paths/paths.go`** — add profile-aware accessors:
   - `ProfileConfigDir(profile string) (string, error)` →
     `<os-config-dir>/focusally/profiles/<profile>`.
   - `ProfileStateDir(profile string) (string, error)` →
     `<state-root>/focusally/profiles/<profile>` (same XDG/fallback resolution
     as today's `StateDir`).
   - `RootConfigDir() (string, error)` → `<os-config-dir>/focusally` (for
     `tracker.json` and migration).
   - `ValidProfileName(string) bool` implementing the regexp above.
   - Keep the legacy `StateDir`/`ConfigDir` only if migration needs them;
     unexport or delete otherwise — no production code path may use the flat
     layout after this step.

2. **`internal/api/config.go`**:
   - `Credentials` gains `BaseURL string \`json:"baseUrl,omitempty"\``.
   - `LoadCredentials(dir)` keeps its signature but gains a companion:
     `LoadCredentialsBound(dir, wantBaseURL string) (Credentials, bool)` that
     returns `ok == false` when the stored `BaseURL` is non-empty and differs
     from `wantBaseURL`. All callers that know the profile's resolved base URL
     use the bound variant — a profile whose apiBase changed reads as unpaired
     instead of sending data to the wrong place. (Empty stored `BaseURL` =
     legacy credentials migrated from the flat layout: treat as bound to the
     profile's CURRENT resolved base and rewrite the field on next save.)
   - `SaveCredentials` always stamps `BaseURL`.
   - Add `GlobalConfig` (`tracker.json` in the root config dir):
     `{TrackingProfile string \`json:"trackingProfile,omitempty"\`}` with
     `Load/SaveGlobalConfig(rootDir)`. Resolution rule, exposed as
     `(GlobalConfig).ResolvedTrackingProfile() string`: empty → `"default"`;
     the literal `"none"` → tracking disabled (callers check for `"none"`).

3. **`internal/migrate/migrate.go`** (new package) — one-shot idempotent
   migration, callable from every entry point before profile resolution:
   - Guarded by `<root>/migrate.lock` (same stale-lock reclaim pattern as
     `pairing.lock`) and skipped instantly when `<root>/profiles/default`
     already exists or no flat files exist.
   - Moves flat `config.json`, `credentials.json`, `pairing.json`,
     `pairing-shown`, `pairing.lock` from `<root>/` into
     `<root>/profiles/default/` (rename; ignore missing).
   - Moves `<state-root>/focusally/sessions/` →
     `<state-root>/focusally/profiles/default/sessions/`.
   - Deletes the legacy header-based MCP registration: if `claude` is on PATH,
     run `claude mcp remove -s user focusally` (errors ignored). This is the
     ONLY remaining shell-out to `claude` after step 4 removes
     `RegisterMCPServer`.
   - Writes nothing else; never touches per-session json contents.

4. **`cmd/tracker/main.go`** — wire profiles through every mode:
   - Flag parsing: `tracker hook <event>` (no profile flag — hook resolves the
     tracking profile from `tracker.json`), `tracker flush <sessionId>
     --profile <name>`, `tracker pair --profile <name>`. Use the stdlib `flag`
     package on the sub-command tail; default `--profile default`.
   - Every mode starts with `migrate.Run()` (silent on error, consistent with
     the tracker's exit-0 discipline), then resolves its profile dirs via the
     new `paths` accessors.
   - `runHook`: load `GlobalConfig`; `"none"` → return immediately (before any
     state write — tracking fully off). Otherwise use the tracking profile's
     config/state dirs; `spawnSelf("flush", ev.SessionID, "--profile", name)`
     and `spawnSelf("pair", "--profile", name)`.
   - `runFlush`: use bound credentials (`LoadCredentialsBound`); on mismatch
     just return (state stays dirty).
   - Delete the `pairing.RegisterMCPServer` call from `refreshCredentials`
     (registration no longer embeds tokens; full removal of the function is
     step 4's).

5. **`internal/pairing/pairing.go`** — no flow change; it already takes
   `configDir`. Verify every path it touches lives inside the profile dir
   (it does: pending/shown/lock + credentials via `api`). Adjust doc comments
   that mention the flat layout.

6. **`internal/tracker/store.go`** — no change needed (`NewStore(stateDir)`
   already takes the dir); verify only.

7. **Tests**:
   - `internal/paths`: profile dir shapes, name validation (valid, invalid,
     traversal attempts like `../x`).
   - `internal/api`: credentials binding — bound load rejects a mismatched
     baseUrl, accepts empty-legacy and stamps on save; global config
     resolution (`""` → default, `"none"`).
   - `internal/migrate`: end-to-end on a temp dir — flat files land in
     `profiles/default`, second run is a no-op, partial flat layouts (only
     credentials, only sessions) migrate cleanly. Shell-out to `claude` must be
     injectable/skippable in tests (function variable).
   - `cmd` behaviour stays covered by existing tests; update fixtures for the
     new layout.

8. `go test ./...` and `go vet ./...` green.
