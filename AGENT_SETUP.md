# FocusAlly setup — instructions for the AI agent

You (the coding agent) are setting up FocusAlly for this user's Claude
Code: session-activity tracking plus the `focusally` MCP server (tasks,
sessions, priorities). Follow the steps in order. Every step is
idempotent — safe to re-run if something was already done. The ONLY
action the user performs themselves is approving a short code in the
FocusAlly app at the end.

## 1. Install the plugin

```bash
claude plugin marketplace add AllyApps/FocusAlly-agent-plugin
claude plugin install focusally@focusally
```

"Already exists" / "already installed" outcomes are success — continue.

## 2. Enable auto-update for the marketplace

So the user receives plugin updates without manual steps:

```bash
python3 - <<'EOF'
import json, os
path = os.path.expanduser("~/.claude/settings.json")
data = json.load(open(path)) if os.path.exists(path) else {}
entry = data.setdefault("extraKnownMarketplaces", {}).setdefault("focusally", {
    "source": {"source": "github", "repo": "AllyApps/FocusAlly-agent-plugin"}
})
entry["autoUpdate"] = True
json.dump(data, open(path, "w"), indent=2, ensure_ascii=False)
print("auto-update enabled")
EOF
```

## 3. Mint the pairing code

The installed plugin lives at
`~/.claude/plugins/cache/focusally/focusally/<version>/`. Run its MCP
binary once to request a login code:

```bash
plugin_dir=$(ls -d ~/.claude/plugins/cache/focusally/focusally/*/ | sort -V | tail -1)
printf '%s\n%s\n' \
  '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"setup","version":"1"}}}' \
  '{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"auth.login"}}' \
  | "$plugin_dir/scripts/run-mcp.sh"
```

Read the `auth.login` result text:

- It contains a pairing code formatted `XXXX-XXXX` — extract it.
- If it says "already connected", this machine is paired: skip to
  step 5 and tell the user everything is ready.
- If it says the code "is not ready yet", wait 3 seconds and re-run the
  same command.

## 4. Hand the code to the user

Show the code prominently and tell the user, in their language:

> Approve code **XXXX-XXXX** in the FocusAlly app: Profile → MCP keys →
> enter code — on any device where you are signed in.

Notes for you:

- On macOS with the FocusAlly app installed locally, an approval window
  may already have popped via deeplink — the user just confirms it.
- Do NOT wait for the approval in a loop; a detached poller finishes
  the exchange automatically once the user approves. The code stays
  valid for ~15 minutes.

## 5. Finish

Tell the user:

- Approve the code (if step 4 issued one), then restart the Claude Code
  session — or run `/reload-plugins` — so the hooks and the `focusally`
  MCP server load.
- From then on everything is automatic: their Claude sessions appear on
  the FocusAlly Day timeline, and the agent can manage tasks, sessions,
  and priorities through the `focusally` MCP tools.
- If the login ever expires, any FocusAlly tool call pops a native
  dialog with a fresh code, or they can ask the agent to call the
  `auth.login` tool (`force: true` re-pairs / switches accounts).

To verify a live session later, call the `auth.status` tool on the
`focusally` MCP server — it reports pairing, token expiry, and pending
tracking uploads.
