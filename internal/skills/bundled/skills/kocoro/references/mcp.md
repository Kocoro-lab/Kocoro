# MCP (Model Context Protocol) Servers

## What is this?

MCP servers are bridges that connect agents to external services and tools. There are two types: **stdio** servers run a local process on your machine (like an npm package that talks to Slack), and **http** servers connect to a remote endpoint over the network. Once configured, agents can use the tools the MCP server provides just like built-in tools.

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
- Response: `{"mcp_servers": {"my-server": "connected"|"enabled"|"disabled"}}`
- Notes: Shows whether each MCP server connected successfully and how many tools it provides.

### Activate config changes
- Method: POST
- Path: /config/reload
- Response: `{"status": "reloaded"}`
- Notes: Required after adding/modifying MCP servers to establish connections.

### Disable an MCP server (without removing)
- Method: PATCH
- Path: /config
- Body: `{"mcp_servers": {"my-server": {"disabled": true}}}`
- Notes: Server config is preserved but the connection is not established. Set `disabled: false` to re-enable.

### Remove an MCP server
- Method: PATCH
- Path: /config
- Body: `{"mcp_servers": {"my-server": null}}`
- Notes: Setting the server to `null` removes it entirely from config.

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
