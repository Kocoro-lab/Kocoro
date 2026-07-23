# MCP (Model Context Protocol) Servers

## What is this?

MCP servers are bridges that connect agents to external services and tools. There are two types: **stdio** servers run a local process on your machine (like an npm package that talks to Slack), and **http** servers connect to a remote endpoint over the network. Once configured, agents can use the tools the MCP server provides just like built-in tools.

Kocoro can also expose its local tool registry to an MCP client with
`shan mcp serve`. That stdio server supports cancellation, progress tokens,
dynamic tool-list notifications, and form elicitation for single-call approval.
Approval still fails closed when the client did not negotiate form elicitation.

## API Endpoints

MCP servers are configured through the config API — there is no separate MCP endpoint.

### Add an MCP server
- Method: PATCH
- Path: /config
- Body (stdio): `{"mcp_servers": {"my-server": {"command": "npx", "args": ["-y", "@some/mcp-package"], "env": {"TOKEN": "your-token"}}}}`
- Body (http): `{"mcp_servers": {"my-server": {"type": "http", "url": "https://my-mcp-server.example.com/mcp"}}}`
- Response: `{"status": "updated"}`

### Check connection status
- Method: GET
- Path: /config/status
- Response: `{"mcp_servers": {"my-server": "connected"|"enabled"|"disabled"}, "mcp_default_agent_disabled": ["server-x"]}`
- Notes: Shows whether each MCP server connected successfully and how many tools it provides. `mcp_default_agent_disabled` (present only when non-empty) lists servers the DEFAULT agent is configured not to use — see "Disable an MCP server for the default agent" below.

### Activate config changes
- Method: POST
- Path: /config/reload
- Response: `{"status": "reloaded"}`
- Notes: Required after adding/modifying MCP servers to establish connections. **/config/reload returns immediately** — actual MCP server connections run in background goroutines, each with its own `connect_timeout_secs`. Poll `GET /config/status` to observe servers transitioning `disabled` → `enabled` → `connected`. **`POST /config/reload` is also the explicit retry signal**: any server that is `disabled: false` but not currently connected (e.g. a previous async-connect attempt failed) gets a fresh connect attempt. Desktop's "Retry" button should go through this endpoint.

### Per-server connect timeout
- Field: `mcp_servers.<name>.connect_timeout_secs`
- Default: 60s, configurable globally via `mcp.default_connect_timeout_secs`
- Notes: Caps the per-server startup time when `/config/reload` (or daemon startup) fires the background connect goroutine. The daemon ships built-in Intercom with 300s so the user has 5 minutes to complete the browser OAuth flow before the npx subprocess is force-killed. When the timeout fires, the daemon force-closes the client (which SIGTERMs the subprocess for stdio servers) and writes an `mcp_connect` audit entry; the server state stays `enabled` but never reaches `connected`.

### Disable an MCP server (without removing)
- Method: PATCH
- Path: /config
- Body: `{"mcp_servers": {"my-server": {"disabled": true}}}`
- Notes: Server config is preserved but the connection is not established. Set `disabled: false` to re-enable.

### Remove an MCP server
- Method: PATCH
- Path: /config
- Body: `{"mcp_servers": {"my-server": null}}`
- Notes: Setting the server to `null` removes it entirely from config. Built-in servers cannot be removed this way — the daemon re-injects them on the next config load. Use `disabled: true` to turn them off.

### Per-agent MCP server selection
Named agents restrict which MCP servers they use via their per-agent `mcp_servers` config (`_inherit` + per-server entries; see agents.md). As of capability `per_agent_mcp_scope`, the daemon **enforces** this at tool-dispatch time: a named agent can only call tools from servers in its resolved, enabled set — not every globally-connected server. (Pre-`per_agent_mcp_scope` daemons only used `mcp_servers` to shape the prompt context string; the tools were still callable.) Servers disabled globally (`mcp_servers.<name>.disabled`) are off for every agent.

### Disable an MCP server for the default agent
- Method: POST (disable) / DELETE (enable)
- Path: /mcp/default-disabled
- Body: `{"server": "<server-name>"}`
- Response: `{"status": "disabled"}` (POST) / `{"status": "removed"}` (DELETE)
- Notes: Controls which MCP servers the **default agent** uses. The default agent uses every globally-enabled server by default; POST adds the server to `config.mcp.default_agent_disabled` so the default agent can no longer call its tools, and DELETE re-enables it. Both are idempotent and persist to `~/.shannon/config.yaml`. **Default-agent-only** — named agents use their own `mcp_servers` config and are unaffected. GET /config/status reflects the state via `mcp_default_agent_disabled`. Requires capability `per_agent_mcp_scope`.

## Built-in MCP Servers

The daemon ships with a small catalog of pre-bundled MCP servers (currently: `intercom`). These appear in `GET /config/status` even when the user has never edited their config:

- `mcp_servers` shows the runtime state (`disabled` / `enabled` / `connected`). Built-ins ship as `disabled` on first launch.
- `mcp_server_info` adds metadata for built-in entries only: `{"<name>": {"builtin": true, "display_name": "Intercom", "description": "...", "auth_hint": "...", "requires_auth": true, "authorized": true|false}}`. Older clients that don't know about this field ignore it.

`requires_auth: true` means activation kicks off an out-of-process OAuth flow that the user needs to be primed for. **Desktop UIs SHOULD show a confirm dialog BEFORE flipping the toggle from off to on**, using `auth_hint` verbatim as the modal body and `display_name` to compose the title (e.g. "Enable Intercom?"). Only after the user confirms should Desktop send `PATCH /config` + `POST /config/reload`. Without this confirm step the browser appears to pop up on its own a few seconds after the toggle moves, which is jarring on cold-cache `npx` installs (5–20s gap).

`authorized` (only emitted when `requires_auth: true`) is the dynamic counterpart: it reports whether the daemon detected an existing OAuth credential cache for this server (for mcp-remote-based servers like Intercom, this means a valid token file exists in `~/.mcp-auth/`). **When `authorized: true`, Desktop SHOULD skip the confirm modal** — re-enabling will silently reuse the cached token, no browser will open, and showing the modal makes the toggle look broken when the user clicks "Authorize" and nothing visible happens. Pseudocode for the Desktop guard:

```ts
const needsConfirm = info.requires_auth && info.authorized !== true && currentlyDisabled;
```

Activating a built-in:
1. (Desktop) If `mcp_server_info.<name>.requires_auth === true` AND `authorized !== true`, show a confirm modal with `auth_hint` as the body. Bail out on cancel.
2. `PATCH /config` body `{"mcp_servers": {"intercom": {"disabled": false}}}`
3. `POST /config/reload` — daemon spawns the configured subprocess. For Intercom this runs `npx mcp-remote https://mcp.intercom.com/mcp`, which opens the user's default browser for OAuth on first run. On re-enable after a previous successful authorization the cached token is used silently.
4. After OAuth completes (or right away when re-enabling), `GET /config/status` reports `"connected"`. Tools become available to agents.

The `command` / `args` / `type` / `url` / `context` fields of a built-in are owned by the daemon binary: PATCH /config rejects edits to those keys with `409 {"error": "builtin_mcp_immutable"}`. Users can still patch `disabled`, `env`, and `keep_alive`, and the yaml file only persists those user-set fields — daemon upgrades pick up any catalog changes (command tweaks, new URL, etc.) automatically without yaml surgery.

## Common Scenarios

### "Connect to Slack"
1. Get a Slack bot token: go to api.slack.com → Create App → OAuth & Permissions → Bot Token Scopes (add `chat:write`, `channels:read`, `channels:history`) → Install App → copy Bot User OAuth Token
2. PATCH /config with:
   ```json
   {"mcp_servers": {"slack": {"command": "npx", "args": ["-y", "@anthropic/slack-mcp"], "env": {"SLACK_BOT_TOKEN": "xoxb-your-token"}}}}
   ```
3. POST /config/reload
4. GET /config/status → verify `mcp_servers.slack.connected: true`
5. Agents can now send messages, read channels, and search Slack history.

### "Connect to a database"
1. Find or set up an MCP server for your database (e.g., `@anthropic/postgres-mcp`)
2. PATCH /config with the server config and connection string in `env`
3. POST /config/reload
4. Attach the server's tools to the agent that needs database access.

### "Temporarily disable an MCP server"
1. PATCH /config with `{"mcp_servers": {"slack": {"disabled": true}}}`
2. POST /config/reload
3. Server config is saved; re-enable by setting `disabled: false`.

### "Check which MCP tools are available"
1. GET /config/status → `mcp_servers` section shows `tools` count per server
2. GET /agents/{name} → `tools` section lists all available tool names including MCP tools

## Safety Notes

- **Stdio command safety**: Shannon only allows safe commands for stdio servers: `node`, `npx`, `python`, `python3`, `uv`, `uvx`, `deno`, `bun`, `go`, `docker`, `pip`, `pipx`, and absolute paths to executables. Shell metacharacters (`;`, `|`, `&`, `` ` ``) are always blocked. Commands outside the safe list require `X-Confirm: true` header. **Always blocked regardless of confirmation**: shells (`sh`, `bash`, `zsh`, etc.), wrapper commands (`env`, `nohup`, `sudo`), eval flags (`-c`, `-e`, `--eval`) in args, and shell names in args.
- **Token security**: Tokens and API keys in `env` are stored in `~/.shannon/config.yaml`. Ensure this file is not committed to version control.
- **Process lifecycle**: Stdio MCP servers are started when Shannon daemon starts and restarted on reload. If the server crashes, Shannon attempts reconnection automatically.
- **HTTP MCP servers**: These connect to remote endpoints — make sure you trust the server operator, as agents will send conversation context to it.
- **Scope creep**: Each MCP server's tools become available to all agents unless you restrict tools via the agent's `tools.allow` / `tools.deny` config.

## Tool Naming and the Loop Detector

Kocoro's loop detector classifies MCP tool names as read-only or write-capable by the verb word at position 0, 1, or 2 of the name (tokens split on `_` and `-`). **Read-only tools** (names whose primary verb is `get`/`list`/`search`/`query`/`fetch`/`read`/`describe`/`find`/`count`/`head`/`show`/`resolve`/`lookup`/`inspect`) get relief from the count-based NoProgress nudge so legitimate batch enumeration with unique arguments (e.g. iterating over 20 distinct database IDs) is not force-stopped.

**Write verbs dominate**: names containing `create`/`delete`/`update`/`remove`/`insert`/`append`/`archive`/`modify`/`rename`/`replace`/`drop`/`prune`/`clear`/`send`/`move`/`upload`/`write`/`push`/`publish`/`submit`/`post`/`add`/`set`/`patch`/`put`/`execute`/`run` in the first three tokens are treated as writes regardless of any read-verb also present. This is deliberate defence-in-depth: the permission engine does not gate MCP calls, so NoProgress is the main guard against unique-arg write loops.

**Practical consequence for operators**:

- **`run_*` / `execute_*` lose NoProgress relief.** Snowflake/ClickHouse-style MCP servers that expose SELECT tools as `run_query` or `run_sql` will see the tool nudged after ~8 unique queries instead of being permitted to enumerate freely. To restore relief, rename the tool to a clear read verb: `query_database`, `search_records`, `fetch_rows`.
- **Compound-verb names are rejected on the write half.** `get_and_create_item`, `list_and_delete_old`, `search_and_replace` all fall under the write guard even though they start with a read verb. If the tool is genuinely read-only despite the name, rename it.
- **Unknown verbs fail closed.** Tools whose name does not start with any recognized verb (e.g. `transform_data`, `process_batch`) are treated as writes by default. Rename with a clear read verb if the tool needs NoProgress relief.

The loop detector's other defences (consecutive-duplicate, exact-duplicate, same-tool-error, family-no-progress) still apply to all MCP tools regardless of naming — the naming heuristic only affects the batch-tolerance relief layer.
