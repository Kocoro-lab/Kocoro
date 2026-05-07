# Recipes

Multi-step guides for common Shannon setup scenarios.

---

## Connect to Slack

Give agents the ability to read channels, send messages, and search Slack history.

1. **Create a Slack app**: Go to [api.slack.com/apps](https://api.slack.com/apps) → "Create New App" → "From scratch" → name it (e.g., "Shannon") → select your workspace.

2. **Grant permissions**: In the app settings, go to "OAuth & Permissions" → "Bot Token Scopes". Add: `chat:write`, `channels:read`, `channels:history`, `users:read`.

3. **Install the app**: Click "Install to Workspace" → authorize. Copy the "Bot User OAuth Token" (starts with `xoxb-`).

4. **Configure the MCP server**:
   ```
   PATCH /config
   {"mcp_servers": {"slack": {"command": "npx", "args": ["-y", "@anthropic/slack-mcp"], "env": {"SLACK_BOT_TOKEN": "xoxb-your-token-here"}}}}
   ```

5. **Reload**: POST /config/reload

6. **Verify**: GET /config/status → check `mcp_servers.slack.connected: true` and `tools` count

7. **Invite the bot**: In Slack, invite the bot to the channels you want it to access: `/invite @Shannon`

Agents can now send messages, read channel history, and search Slack.

---

## Create a Full-Featured Agent

Build an agent with a clear purpose, the right skills, and restricted tooling.

1. **Create the agent**:
   ```
   POST /agents
   {"name": "data-analyst", "prompt": "You are a data analyst. You help users understand their data through queries, visualization recommendations, and clear summaries. Always show your work."}
   ```

2. **See what skills are available**:
   ```
   GET /skills/downloadable
   ```

3. **Install relevant skills** (e.g., a spreadsheet skill):
   ```
   POST /skills/install/xlsx
   POST /skills/install/pdf
   ```

4. **Attach skills to the agent**:
   ```
   PUT /agents/data-analyst/skills/xlsx
   PUT /agents/data-analyst/skills/pdf
   ```

5. **Configure tools** (restrict to what's needed):
   ```
   PUT /agents/data-analyst/config
   {"tools": {"allow": ["bash:python *", "bash:psql *", "file_read", "glob"], "deny": ["file_write", "file_edit"]}}
   ```

6. **Verify**:
   ```
   GET /agents/data-analyst
   ```

---

## Set Up a Project

Configure Shannon for a specific codebase so agents understand the project context.

1. **Initialize the project**:
   ```
   POST /project/init
   {"cwd": "/path/to/your/project", "instructions": "This is a TypeScript React app. Use 2-space indentation. Never modify package-lock.json directly."}
   ```

2. **Add project-specific rules**:
   ```
   PUT /rules/no-direct-dependencies
   {"content": "Never add npm dependencies without asking the user first. Always explain what the package does and why it's needed."}
   ```

3. **Set the agent's working directory**:
   ```
   PUT /agents/default/config
   {"cwd": "/path/to/your/project"}
   ```

4. **Verify the setup**: GET /config → check `effective` includes the project instructions

Agents working in this project now have project-specific context and rules.

---

## Set Up Scheduled Monitoring

Automate a regular check-in that runs without you needing to be present.

1. **Create a monitoring agent** (optional but recommended):
   ```
   POST /agents
   {"name": "monitor", "prompt": "You are a monitoring agent. Check for anomalies, failures, and important changes. Be concise — only report things that need attention."}
   ```

2. **Give it the right permissions**:
   ```
   PUT /agents/monitor/config
   {"tools": {"allow": ["bash:git *", "bash:curl *", "file_read", "grep"]}}
   ```

3. **Create the schedule**:
   ```
   POST /schedules
   {"prompt": "Check for: 1) any git commits since last check, 2) any error patterns in the last hour of logs at ~/app/logs/app.log, 3) whether the app is responding (curl http://localhost:3000/health). Report only if something needs attention.", "cron": "0 * * * *", "agent": "monitor"}
   ```

4. **Verify**: GET /schedules → confirm `enabled: true` and the cron expression is correct

The monitor runs every hour automatically. Check `~/.shannon/logs/schedule-{id}.log` to review past runs.

---

## Migrate to a Custom Agent

Move your workflow from the default agent to a specialized one with tailored behavior.

1. **See your current global instructions** (to incorporate them):
   ```
   GET /instructions
   ```

2. **Create the specialized agent** incorporating relevant global context:
   ```
   POST /agents
   {"name": "my-assistant", "prompt": "You are my personal assistant. [paste relevant global instructions here, plus your new specialized instructions]"}
   ```

3. **Attach any skills** the agent needs:
   ```
   GET /skills          ← see what's installed
   PUT /agents/my-assistant/skills/{skill-name}
   ```

4. **Configure tools and working directory**:
   ```
   PUT /agents/my-assistant/config
   {"cwd": "/my/default/project", "agent": {"model": "claude-opus-4-5"}}
   ```

5. **Test with a one-shot command**:
   ```bash
   shan --agent my-assistant -y "Hello, introduce yourself"
   ```

6. **Use the agent**: Pass `--agent my-assistant` to `shan` in the CLI, or select it in the desktop app. In TUI: `shan --agent my-assistant`

The default agent is unchanged — you can switch between agents anytime.

---

## Publish a Generated Artifact to a Public URL

When an agent produces something the user wants to **share externally** (a landing-page draft, a chart PNG, a PDF report, an HTML preview to embed in a Slack/Feishu/LINE reply), use the `publish_to_web` tool.

**Important — when NOT to use:**
- Backup, sync, or "just in case" uploads. There is no DELETE; the URL is permanent.
- Source code, configs, `.env`, credentials, private keys, logs. These are blocklisted client-side and will be rejected.
- Inline previews inside Kocoro Desktop. Use the `kocoro-generative-ui` skill's `html-artifact` blocks instead — they render in the app sandbox without a public URL.

**Flow:**

1. Generate the file locally (typically `file_write` after rendering content).
2. Call `publish_to_web`:
   ```json
   {
     "path": "/tmp/landing.html",
     "purpose": "send landing page draft to user via Slack reply"
   }
   ```
   - `purpose` is **mandatory** and shown to the user during approval. Be specific (who is the recipient, why public). Vague answers ("share", "test", "send it") are rejected.
   - Optional `filename` and `content_type` overrides; defaults inferred from the file path.
3. The user is prompted for approval (always — there is no auto-approve path).
4. On approval, the tool POSTs to Shannon Cloud's `/api/v1/uploads` and returns a permanent HTTPS URL.
5. Embed the URL in the assistant's reply ("Here's the landing page: https://…").

**Constraints:**
- 50 MiB per file (server-enforced; tool pre-checks locally).
- Default extension allowlist: html, md, txt, pdf, png, jpg, jpeg, gif, webp, svg, csv, json, mp4, mp3, wav, webm.
- Path-segment blocklist (`.env`, `.ssh`, `credentials`, `secrets`, `id_rsa`, …) and basename suffix blocklist (`.pem`, `.key`, `.p12`, `.pfx`, …) are enforced before any HTTP call. **Not user-configurable** — denylist intentionally cannot be widened.
- The allowlist **can** be extended via config:
  ```yaml
  cloud:
    publish_allowed_extensions: [".go", ".sql"]
  ```
  This is additive. Even after extending, denylist still applies.
- Requires `cloud.enabled: true` AND a configured `api_key`. Without either, the tool is not registered.
