# Channel Integration Guide

How to connect any messaging platform to Kocoro via the daemon's local HTTP API.

> **When to use this guide**
>
> This guide covers building a **local bridge bot** that forwards messages from
> a platform (Discord, Matrix, a custom webhook, etc.) into a Kocoro daemon
> running on your own machine via `POST /message`.
>
> **Use this path when:** you're running a personal or dev-team deployment,
> single user or small trusted group, same machine as the daemon, and you want
> full hackability. You own the platform credentials and the bot process.
>
> **Use Shannon Cloud instead when:** you need multi-tenant OAuth, per-user
> quotas, audit logging, or managed channel lifecycle. The official Slack,
> LINE, Feishu, and Lark integrations live in Shannon Cloud for these reasons.
> Cloud also handles interactive tool-approval relay back to the originating
> channel, which the local HTTP path does not provide out of the box (see the
> Interactive Tool Approval section below — in JSON mode, all tool calls are
> auto-approved).
>
> **Rule of thumb:** official channels go through Cloud, DIY channels go
> through local HTTP. If you're unsure, start here — you can always migrate a
> bridge into Cloud later.

## Architecture

```
┌─────────────────────┐         ┌──────────────────┐
│  Messaging Platform  │         │   Kocoro Daemon   │
│  (Discord, Telegram, │  HTTP   │  127.0.0.1:7533   │
│   Slack, custom...)  │◄──────►│                    │
│                      │         │  AgentLoop runs    │
│  Your bridge bot     │         │  locally with full │
│  lives here          │         │  tool access       │
└─────────────────────┘         └──────────────────┘
```

Your bot acts as a bridge: receive a message from your platform, forward it to the daemon via `POST /message`, and relay the response back.

## Quick Start

```bash
# Send a message (JSON mode, auto-approve)
curl -X POST http://127.0.0.1:7533/message \
  -H "Content-Type: application/json" \
  -d '{"text": "Hello, what can you do?"}'

# With source and channel for session continuity
curl -X POST http://127.0.0.1:7533/message \
  -H "Content-Type: application/json" \
  -d '{
    "text": "What files are in the current directory?",
    "source": "my-bot",
    "channel": "general",
    "sender": "alice"
  }'
```

## POST /message

### Request

```json
{
  "text":       "string (required) — the user's message",
  "source":     "string (optional) — platform identifier, e.g. 'discord', 'telegram'",
  "channel":    "string (optional) — channel/room identifier for session routing",
  "sender":     "string (optional) — user identifier, injected into agent context",
  "agent":      "string (optional) — target a named agent (must match ^[a-z0-9][a-z0-9_-]{0,63}$)",
  "session_id": "string (optional) — resume a specific session by ID",
  "thread_id":  "string (optional) — thread context for platforms with threading"
}
```

Only `text` is required. If `source` is omitted, it defaults to `"kocoro"`.

### Response (JSON mode)

```json
{
  "reply": "The agent's final text response",
  "session_id": "2026-04-06-a1b2c3d4",
  "agent": "my-agent",
  "usage": {
    "input_tokens": 523,
    "output_tokens": 87,
    "total_tokens": 610,
    "cost_usd": 0.0045
  }
}
```

Save `session_id` if you want to explicitly resume the same session later.

### Error Responses

| Status | Body | Cause |
|--------|------|-------|
| 400 | `{"error":"text is required"}` | Missing or empty `text` |
| 400 | `{"error":"invalid agent name"}` | `agent` doesn't match naming rules |
| 500 | `{"error":"daemon deps not configured"}` | Daemon not fully started |

## Session Routing & Continuity

The daemon uses a **route key** to map incoming messages to sessions. This enables conversation continuity — messages from the same source + channel share the same session.

### How route keys are computed

| Condition | Route Key | Behavior |
|-----------|-----------|----------|
| `agent` is set | `agent:<name>` | Named agents always resume their single long-lived session |
| `session_id` is set | `session:<id>` | Resumes the exact session |
| `source` + `channel` both set | `default:<source>:<channel>` | Same source+channel = same session |
| Neither set | `""` (empty) | Fresh session every time |

### Best practice for channel mapping

Use a stable, unique identifier for each conversation context:

```
Discord:  source="discord"   channel="<guild_id>:<channel_id>"
Slack:    source="slack"      channel="<workspace>:<channel>" thread_id="<thread_ts>"
Telegram: source="telegram"   channel="<chat_id>"
Custom:   source="my-app"     channel="<room_id>"
```

### Sources that bypass route caching

These sources always create fresh sessions regardless of `channel`:

- `""` (empty), `"web"`, `"webhook"`, `"cron"`, `"schedule"`, `"system"`

If you want session continuity, use a specific source name like `"discord"` or `"my-bot"`.

## Output Format

The daemon adjusts the agent's output format based on `source`:

| Source | Format | Reason |
|--------|--------|--------|
| `slack`, `line`, `feishu`, `lark`, `telegram`, `webhook` | `plain` | Shannon Cloud handles final rich rendering |
| Everything else (including `discord`, custom bots) | `markdown` | Direct consumption by the client |

Discord supports standard markdown, so using `source="discord"` gives you properly formatted responses with code blocks, bold, lists, etc.

## JSON vs SSE: Choosing a Mode

| | JSON mode | SSE mode |
|---|---|---|
| **Header** | (default) | `Accept: text/event-stream` |
| **Response** | Single JSON object after agent completes | Stream of events as agent works |
| **Tool approval** | Auto-approved (all tools run without asking) | `approval` events emitted; agent blocks until you POST `/approval` |
| **Real-time status** | None — you wait for the full reply | `tool` events show which tools are running |
| **Text streaming** | No | `delta` events supported, but in practice text arrives in the `done` event (see note below) |
| **Best for** | Simple bots, trusted agents | Interactive bots with approval UI |

**Important:** If you use SSE mode, you **must** handle `approval` events. The agent will block indefinitely (up to 5 min timeout) waiting for your response. If you don't want to implement approval handling, use JSON mode instead.

## SSE Streaming Mode

For real-time streaming of the agent's work, use Server-Sent Events:

```bash
curl -N -X POST http://127.0.0.1:7533/message \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d '{"text": "Explain how HTTP works", "source": "my-bot", "channel": "general"}'
```

### Event types

| Event | Data | Description |
|-------|------|-------------|
| `tool` | `{"tool":"bash","status":"running"}` | Tool execution started |
| `tool` | `{"tool":"bash","status":"completed","elapsed":2.3}` | Tool execution finished |
| `delta` | `{"text":"chunk of text"}` | Streaming text from the LLM (see note) |
| `approval` | `{"request_id":"apr_...","tool":"bash","args":"..."}` | Tool needs user approval — **agent blocks until you respond** |
| `error` | `{"error":"message"}` | Fatal error |
| `done` | Full `RunAgentResult` JSON | Agent completed — contains the final `reply` text |

### Important notes on SSE behavior

1. **Text arrives in `done`, not `delta`.** The daemon's SSE handler supports `delta` events, but in practice the final text is delivered in the `done` event's `reply` field. Do not rely on `delta` events for building the response — always use the `reply` from `done` as the authoritative answer.

2. **Tool events stream in real-time.** `tool` events with `status: "running"` and `status: "completed"` are emitted as tools execute, so you can show live status (e.g., "Running: bash...").

3. **Approval events block the agent.** When an `approval` event arrives, the entire agent loop pauses until you POST `/approval` or the 5-minute timeout expires. Your SSE read loop must handle this event inline.

4. **SSE parsing requires chunked line reading.** HTTP clients may buffer SSE data. Use chunked/streaming reads and split on `\n` boundaries manually rather than relying on line-by-line iterators. Example in Python with aiohttp:
   ```python
   line_buf = b""
   async for chunk in resp.content.iter_any():
       line_buf += chunk
       while b"\n" in line_buf:
           raw_line, line_buf = line_buf.split(b"\n", 1)
           line = raw_line.decode("utf-8").rstrip("\r")
           # parse SSE: "event: ...", "data: ...", "" (empty = end of event)
   ```

### Example SSE stream (with tool calls)

```
event: tool
data: {"tool":"directory_list","status":"running"}

event: tool
data: {"tool":"directory_list","status":"completed","elapsed":0.03}

event: done
data: {"reply":"Here are the files...","session_id":"...","agent":"","usage":{...}}
```

### Example SSE stream (with approval)

```
event: tool
data: {"tool":"bash","status":"running"}

event: approval
data: {"request_id":"apr_f946703221d6a7b3","tool":"bash","args":"{\"command\":\"rm file.txt\"}","agent":""}

... agent is now blocked, waiting for POST /approval ...

event: tool
data: {"tool":"bash","status":"completed","elapsed":0.5}

event: done
data: {"reply":"File deleted.","session_id":"...","agent":"","usage":{...}}
```

## Interactive Tool Approval (SSE only)

In SSE mode, when the agent wants to use a tool that requires approval, the stream emits an `approval` event. Your bot **must** respond via `POST /approval` for the agent to continue.

**Note:** In JSON mode (without `Accept: text/event-stream`), all tools are auto-approved. You only need this if you want human-in-the-loop control.

### What triggers approval?

The daemon uses a 5-layer permission model. Most read-only tools and common safe commands (`ls`, `cat`, `git status`, `go build`, etc.) are auto-approved. Approval events are only emitted for potentially dangerous operations:

- **bash**: Commands not in the default safe list (e.g., `rm`, `git push`, `pip install`, `mkdir`)
- **file_write / file_edit**: Writing or modifying files
- **http**: Making HTTP requests to non-localhost URLs
- **process**: Killing processes
- **computer**: Mouse/keyboard automation
- **applescript**: Running macOS automation scripts

Safe commands defined in `internal/permissions/permissions.go` never trigger approval events. See the [Kocoro permission model](https://github.com/Kocoro-lab/Kocoro/blob/main/CLAUDE.md#permission-model) for the full list.

### Approval request (SSE event)

```json
{
  "request_id": "apr_a1b2c3d4e5f6g7h8",
  "tool": "bash",
  "args": "{\"command\":\"rm -rf /tmp/data\"}",
  "agent": "my-agent",
  "channel": "",
  "thread_id": ""
}
```

### POST /approval

```bash
curl -X POST http://127.0.0.1:7533/approval \
  -H "Content-Type: application/json" \
  -d '{
    "request_id": "apr_a1b2c3d4e5f6g7h8",
    "decision": "allow"
  }'
```

**Decision values:**

| Decision | Effect |
|----------|--------|
| `allow` | Allow this tool call once |
| `deny` | Reject this tool call |
| `always_allow` | For bash: persists command to `allowed_commands` config. For other tools: auto-approve for this session only |

**Timeout:** 5 minutes. If no response is received, the tool call is denied.

### Approval flow for a bot

```
1. Connect to POST /message with Accept: text/event-stream
2. Read SSE events in a loop
3. On "tool" event: update status in chat ("Running: bash...")
4. On "approval" event:
   a. Show the user: "Agent wants to run: bash rm file.txt [Allow] [Deny] [Always Allow]"
   b. User clicks a button
   c. POST /approval with the decision
   d. Agent resumes (or aborts if denied)
5. On "done" event: send the reply back to the chat
```

**Tip:** The `args` field in approval events is a JSON string. Parse it to show the user a readable description:
- For `bash`: extract the `command` field
- For `file_write`/`file_edit`: extract the `file_path` field
- For `http`: extract the `url` field

## Message Injection

If you send a `POST /message` to a route that is already processing a request, the message is **injected** into the running agent rather than queued as a new run.

Possible responses:

| Status | Body | Meaning |
|--------|------|---------|
| 200 | `{"status":"injected","route":"..."}` | Message queued for mid-turn insertion |
| 429 | `{"status":"rejected","reason":"queue_full"}` | Injection queue is full |
| 409 | `{"status":"rejected","reason":"active_run_not_ready"}` | Agent not ready for injection |

## Platform-Specific Tips

### Discord
- **Message limit:** 2000 characters. Split long responses at newline boundaries.
- **Format:** Markdown supported natively. `source="discord"` gives markdown output.
- **Session mapping:** Use `guild_id:channel_id` as channel key. Decide whether Discord threads should be separate sessions or share the parent channel's session.

### Slack
- **Format:** Use `source="slack"` for plain text output (Slack's Block Kit handles rendering).
- **Threading:** Use `thread_ts` as `thread_id` for thread-level session continuity.

### Telegram
- **Message limit:** 4096 characters.
- **Format:** Supports markdown. Use a custom source name for markdown output.
- **Session mapping:** Use `chat_id` as channel key.

### Generic Webhook
- Set `source` to your platform name for proper routing.
- If you don't need session continuity, you can omit `channel` (each request gets a fresh session).

## Reference Implementation

For a working example, see the [ShanClaw Discord Bot](https://github.com/AlanY1an/shanclaw-discord-bot) by @AlanY1an — a minimal Python bot (discord.py + aiohttp) that bridges Discord to the ShanClaw daemon using the patterns described in this guide. It demonstrates SSE streaming, interactive tool approval via Discord buttons, and session continuity via `guild_id:channel_id` channel mapping.

This is a community reference, not an official Kocoro-lab project — it lives in the author's personal repo and is maintained there. Treat it as a copy-paste starting point for your own bridge bot in any language.
