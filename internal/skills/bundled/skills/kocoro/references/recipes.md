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

---

## Generate an Image from a Text Prompt

When the user asks the agent to **draw / generate / paint / create a picture of** something (illustration, banner, decorative artwork, photorealistic scene), use the `generate_image` tool. The output is a permanent public CDN URL — already shareable, no follow-up `publish_to_web` call needed.

**Important — when NOT to use:**
- Charts, diagrams, data visualization, structural figures. Use the `kocoro-generative-ui` skill's `html-artifact` blocks instead — they render inline as SVG/HTML inside Kocoro Desktop, no public URL, and you can encode real data.
- Editing or annotating an existing image. The endpoint is text-to-image only; no input image is supported.
- Backup, sync, or "just in case" generations. Each call consumes paid quota.

**Flow:**

1. Call `generate_image`:
   ```json
   {
     "prompt": "A serene cyberpunk cat sitting on a neon-lit Tokyo rooftop at dusk, photorealistic, soft volumetric lighting",
     "size": "1024x1024",
     "quality": "low"
   }
   ```
2. The user is prompted for approval (always — paid quota + permanent public URL).
3. On approval, the tool POSTs to Shannon Cloud's `/api/v1/images/generations` and returns one or more permanent HTTPS URLs.
4. Embed the URL(s) in the assistant's reply with markdown: `![](https://static.kocoro.ai/...)` so Desktop renders the image inline.

**Parameters:**
- `prompt` (required, 1–32000 chars): be specific about subject, style, composition, lighting. Vague prompts produce vague images.
- `size` (optional): `1024x1024` (default), `1024x1536`, `1536x1024`, or `auto`.
- `quality` (optional): `auto` (default), `low`, `medium`, `high`.
- `n` (optional, 1–10): number of images. **Default 1.** Each image is a separate paid generation — only set `n>1` when the user explicitly asks for multiple variants.
- `background` (optional): `transparent` (logos / icons that need to composite), `opaque`, `auto`.

⚠️ **Do not pass a `model` field.** The server pins `gpt-image-2` and silently drops any `model` in the request.

**Latency vs. quality** (pick the lowest that satisfies the request):

| `quality` | Time per image (1024×1024) |
|---|---|
| `low`    | 30–50s |
| `medium` | 60–90s |
| `auto`   | 80–150s |
| `high`   | 120–180s |

**Failure modes the LLM should adapt to:**
- 504 upstream timeout — **do not retry the same args.** Drop `quality` (high → medium / low) or set `n=1`.
- 502 no_images_returned — content-moderation hit. **Revise the prompt**; retrying the same text produces the same outcome.
- Transient 502 upstream_error / 500 image_failed / 503 / network — the client retries internally; if you still get a transient error, a fresh attempt later may succeed.

**Constraints:**
- Requires `cloud.enabled: true` AND a configured `api_key`. Without either, the tool is not registered.
- The returned URL is permanent and public. Do not put confidential context into prompts that produce identifying images.

---

## Edit an Existing Image

When the user asks the agent to **modify / change / redraw / add to / remove from** an existing image (recolor, swap background, add an element, combine multiple images), use the `edit_image` tool. Output is a new permanent public CDN URL.

**Important — when NOT to use:**
- Generating from scratch (no source). Use `generate_image`.
- Annotating charts / data figures. Use the `kocoro-generative-ui` skill.
- Editing a non-Shannon image. Pipe through `publish_to_web` first to upload it (or `generate_image` to produce a CDN-hosted source).

**Hard requirement:**

`image_urls` must contain **1–4 entries, each starting with `https://static.kocoro.ai/`**. External URLs are rejected by the server and pre-rejected client-side. If the user gives you an external URL or a local path, do this first:

- Local file or arbitrary URL the user can re-host → call `publish_to_web` → use the returned CDN URL.
- "Make me a cat picture and then …" → call `generate_image` first → use that URL as the source.

There is **no mask field**. Describe the region in natural language ("change the cat's color to orange", "add a moon in the upper-left", "remove the watermark in the bottom-right").

**Flow:**

1. Call `edit_image`:
   ```json
   {
     "prompt": "Change the cat's fur to orange and add a small wizard hat",
     "image_urls": ["https://static.kocoro.ai/public/abc123/cat.png"],
     "quality": "low"
   }
   ```
2. The user is prompted for approval (always — paid quota + permanent public URL).
3. On approval, the tool POSTs to `/api/v1/images/edits` and returns the modified image's permanent HTTPS URL.
4. Embed it with markdown: `![](https://static.kocoro.ai/...)`.

**Parameters:**
- `prompt` (required, 1–32000 chars): the modification instruction. The clearer the region/property reference, the better.
- `image_urls` (required, 1–4): Shannon CDN URLs. With multiple entries, the model can blend / composite the sources.
- `size` (optional): `1024x1024` (default), `1024x1536`, `1536x1024`, or `auto`.
- `quality` (optional): `auto` (default), `low`, `medium`, `high`.
- `n` (optional, 1–10): number of variants. Default 1. Each is a separate paid generation.
- `background` (optional): `transparent` / `opaque` / `auto`.

⚠️ Do not pass a `model` field — server pins `gpt-image-2`.

**Latency vs. quality** (single source, 1024×1024; multi-source adds 50–100%):

| `quality` | 1 source | 4 sources |
|---|---|---|
| `low`  | 40–70s   | 60–100s  |
| `auto` | 100–180s | 150–250s |
| `high` | 150–250s | 200–350s |

Each source image charges ~85 image-tokens on top of the prompt — multi-image requests get expensive fast.

**Failure modes the LLM should adapt to:**
- 400 `invalid_image_url` — server rejected one of the URLs as not under `https://static.kocoro.ai/`. **Don't retry the same args.** Tell the user, or pipe the source through `publish_to_web` and retry with the returned CDN URL.
- 413 `source_too_large` — a source image exceeds 25 MiB. **Don't retry.** Re-publish a smaller / compressed version first.
- 504 upstream timeout — drop `quality` (high → medium / low), reduce `n` to 1, or use fewer source images. Same args won't succeed.
- 502 `no_images_returned` — content-moderation hit on prompt or one of the sources. Revise the input.
- Transient 502 upstream_error / 502 source_fetch_failed / 500 image_failed / 503 / network — client retries 3× internally; a fresh attempt later may succeed.

**Constraints:**
- Requires `cloud.enabled: true` AND a configured `api_key`.
- Output URL is permanent and public. Be deliberate about confidential subject matter.
