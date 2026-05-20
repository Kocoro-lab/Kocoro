# Daemon event bus

## What it does

Streams mid-turn progress from running agents to any subscriber. Two transports carry the same event vocabulary:

- **`GET /events`** — global server-sent-events stream. Subscribers see every session's activity. Ring-buffered, so `/events?since=<seq>` lets reconnecting clients replay events they missed (Desktop's background-session progress indicator depends on this).
- **`POST /message` SSE stream** — per-request stream for the message a client just sent. Carries the same vocabulary plus `delta` (text tokens) and `done` (final reply with usage).

Both paths funnel through `multiHandler` on the daemon side: the per-request HTTP handler and `busEventHandler` receive every event, so the global bus is never stale compared to the per-request stream.

## Event types

| `event:` / `type` | When it fires | Transport |
|---|---|---|
| `tool_status` | Tool starts (`running`) or finishes (`completed` / `denied`). | Bus + per-request |
| `cloud_agent` | Shannon Cloud sub-agent status changes (started/thinking/completed). | Bus + per-request |
| `cloud_progress` | Task-list progress (completed/total) for cloud-delegated turns. | Bus + per-request |
| `cloud_plan` | Cloud research plan / updated plan / approved plan. | Bus + per-request |
| `run_status` | Run-level state changes: watchdog timeouts, output-cap truncation, compaction, etc. (see `run_status` section for full code list, non-exhaustive). | Bus |
| `usage` | Per-LLM-call token and cost snapshot (on `OnUsage` boundary). | Bus |
| `message_received` | User message was accepted for a session. Normal run-start messages are already persisted when this fires. Active-run follow-ups injected through the daemon WS path also emit this event with `queued: true` so clients can show a read-only "waiting for current turn" card until the transcript catches up. For Slack/WeCom/Feishu/Lark, the daemon also forwards an in-place Cloud stream status line (`Queued next: ...`) to the active channel message, so the IM thread does not look silent. Payload `{agent, source, sender, session_id, message_id, text, queued?}`. Older daemons omit `queued`; treat missing as false. | Bus |
| `agent_reply` | Agent finished a turn (WS / schedule / Ptfrog sources). | Bus |
| `agent_error` | Agent run failed. | Bus |
| `notification` | Two triggers: (1) agent-authored `notify` tool call; (2) daemon reply-complete banner — fires once per `agent_reply`. The reply-complete path is darwin-only and skipped when (a) the reply text is empty, (b) the source is cloud-distributed (slack/line/feishu/lark/wecom/telegram/webhook — reply delivered through the channel), or (c) the source is an autonomous local trigger that would spam (heartbeat/watcher/mcp). schedule/cron stay opted-in. Both paths route through `tools.SendBanner`, so the Desktop-handler-or-osascript-fallback contract below applies identically. Payload shape: `{session_id, agent, source, title, body, sound}` (`body` is redact + lightweight-markdown-stripped + truncated to 140 chars). | Bus |
| `approval_request` | Tool needs user approval; payload `{request_id, session_id, agent, tool, title, source, channel, args, flags?, ts}`. `title` is parsed from `args.description` (falls back to `tool`). `args` is redacted+truncated (event-bus copy only; the wire copy sent to Cloud stays unredacted so Slack/etc. can render the real command). `flags` carries policy hints. `always_allow_disabled` is emitted only for tools in `agent.DisallowsAutoApproval`; that list is currently empty, so no production tool sets the flag today. Emitted only after the outgoing request reached the transport successfully — failed sends never publish a card. | Bus |
| `approval_resolved` | User answered an approval; payload `{request_id, decision, resolved_by, ts}` where decision ∈ allow / deny / always_allow and `resolved_by` ∈ kocoro / external / `daemon` / Cloud-provided actor. `daemon` resolution with `decision: "deny"` is a synthetic cleanup emitted when the daemon abandons a request (timeout, ctx cancel, WS disconnect via `CancelAll`) — Desktop dismisses the inbox card on it. The broker guarantees **at most one `approval_resolved` per `request_id`**: ingress paths (`POST /approval`, WS `approval_response`) and the daemon-cleanup path race for the same `pending` entry under the broker mutex, and only the winner emits, so subscribers will never see conflicting terminal states like `allow/kocoro` followed by `deny/daemon` for the same id. | Bus |
| `schedule_run` | Scheduled agent run lifecycle; payload `{schedule_id, session_id, agent, phase, error?, ts}` where `phase` ∈ started / succeeded / failed. `session_id` is empty for `started` (no session yet) and for failures that hit before session resolution. `error` is present only on `failed` (redacted + truncated). Two events fire per scheduled run: `started` before `RunAgent`, then exactly one of `succeeded` / `failed` after it returns (or after a panic recovery). | Bus |
| `approval_notice` | Post-decision feedback (e.g. high-risk pattern not persisted). Structured i18n-friendly payload: `{severity, code, tool, message}`. `severity` ∈ info / warn. `code` is the stable i18n key (`high_risk_not_persistable` / `bash_always_ask_not_persisted` / `persist_failed`); `tool` is the offending tool name (for interpolation into localized templates); `message` is the English fallback for clients that don't recognize `code` yet. Older clients reading only `severity` + `message` continue to work — `code` and `tool` are additive. | Bus |
| `delta` | Streaming text tokens for the agent reply. | Per-request only |
| `done` | Final reply payload with accumulated `usage`. | Per-request only |

## Payload shapes

All bus events carry `session_id` (string) so subscribers can demux per-session. `ts` (RFC3339) accompanies `tool_status` and `usage`. All tool-call args and result previews are passed through `audit.RedactSecrets` (API keys, AWS keys, bearer tokens, passwords) **before** truncation — the redact-then-truncate order is load-bearing: a secret that straddles the byte-200 boundary would otherwise be cut into a sub-regex fragment and leak past redaction. See `internal/daemon/bus_handler.go:redactAndTruncate` and the `TestBusEventHandlerOnToolCallRedactsSecretSpanningTruncation` regression test.

### `tool_status`

```json
{
  "tool": "bash",
  "status": "running",
  "args": "ls -la /tmp",
  "session_id": "sess_abc",
  "ts": "2026-04-24T01:23:45Z"
}
```

```json
{
  "tool": "bash",
  "status": "completed",
  "elapsed": 1.234,
  "is_error": false,
  "preview": "total 24\ndrwxr-xr-x 5 user wheel 160 Apr 24 01:23 .",
  "session_id": "sess_abc",
  "ts": "2026-04-24T01:23:46Z"
}
```

`args` fires on the `running` event; `preview` and `is_error` fire on `completed` / `denied`. Both are redacted + UTF-8-safe-truncated to 200 bytes.

### `usage`

```json
{
  "input_tokens": 1200,
  "output_tokens": 450,
  "cache_read_tokens": 800,
  "cache_write_tokens": 0,
  "cost_usd": 0.0123,
  "llm_calls": 3,
  "model": "claude-sonnet-4-6",
  "session_id": "sess_abc",
  "ts": "2026-04-24T01:23:50Z"
}
```

Emits once per `OnUsage` boundary (typically once per LLM call, not per token). Consumers aggregate over session if they want a running total.

### `run_status`

```json
{
  "code": "idle_soft",
  "detail": "no LLM activity for 15s (phase=awaiting_llm)",
  "session_id": "sess_abc",
  "agent": "coding"
}
```

`code` is one of the values below. The list is **non-exhaustive**; consumers should treat an unknown `code` as informational rather than erroring out, and extract any numeric data from `detail` with a tolerant regex rather than parsing field-by-field.

| `code` | Meaning |
|---|---|
| `idle_soft` | No LLM activity for the soft-idle threshold (default 90s). Detail typically encodes elapsed seconds and current phase. |
| `idle_hard` | Hard-idle threshold reached; context was cancelled. Detail encodes elapsed seconds. |
| `max_tokens_truncated` | Text-only response hit the output-token cap; continuation budget exhausted. |
| `max_tokens_truncated_tool_call` | Trailing `tool_use` was cut off mid-emission; a synthetic `tool_result` was injected so the model can retry with smaller pieces. Detail encodes the recovery attempt count (e.g. `recovery 2/3`). |
| `max_tokens_recovery_exhausted` | The model kept emitting truncated tool calls past the per-run recovery budget; the run is being force-stopped. |
| `preflight_compaction` | Context was at 95%+ of the model's window; history was compacted before the LLM call. |
| `preflight_user_truncate` | A single user message exceeded the preflight cap and was head+tail truncated before send. |
| `context_bloat` | Tool-result content has dominated context; an inline nudge was added asking the model to summarize or stop. |
| `context_window_autodetect` | The configured context window was overridden after the provider's `response.model` revealed a different family (e.g. 200K → 1M). |
| `compaction_failed` | A compaction attempt failed; detail encodes the phase tag. |

### `cloud_agent` / `cloud_progress` / `cloud_plan`

```json
{ "agent_id": "preparing", "status": "processing", "message": "building context", "session_id": "sess_abc" }
{ "completed": 3, "total": 7, "session_id": "sess_abc" }
{ "type": "research_plan", "content": "1. Gather...\n2. Synthesize...", "needs_review": true, "session_id": "sess_abc" }
```

`cloud_plan.content` is redacted then truncated at 2048 bytes + `"… (truncated)"` marker if exceeded.

## Subscribing

```bash
# Tail all events live
curl -N http://localhost:7533/events

# Replay from a known cursor (last event seq)
curl -N "http://localhost:7533/events?since=42"
```

## Notification history

`GET /notifications` returns the history of banner-class events captured by the EventBus. Distinct from `/events?since=` replay: this buffer retains notification-class events **regardless of whether a subscriber was attached at emit time**, so Desktop can show "what notifications did the user receive while offline".

Captured types: `notification`, `approval_request`, `heartbeat_alert`, `agent_error`.

**Persistent across daemon restarts.** Backed by `~/.shannon/notifications.jsonl` (JSON-lines, append-only). On daemon startup the file is loaded and trimmed to the most recent 500 entries (oldest evicted, log atomically rewritten). Event IDs remain monotonic across restarts — clients holding a `next_cursor` from before the restart can keep using it.

### Asymmetry with `/events?since=` replay

The two rings have intentionally different retention rules:

| Event type | `/events?since=` SSE ring | `/notifications` ring + disk |
|---|---|---|
| `notification` (no subscriber at emit) | **dropped** | **dropped** (osascript fallback already fired the banner natively — re-banner on Desktop launch would duplicate) |
| `notification` (subscriber present at emit) | kept | kept |
| `approval_request` / `heartbeat_alert` / `agent_error` | kept | kept (no osascript fallback path, so always safe to retain) |
| `approval_notice` | dropped if undelivered | **never tracked** (post-decision feedback, not a banner) |
| All other event types | kept | not tracked |

The two rings have parallel retention rules for `notification`: both drop it when no subscriber is attached, because in that case the notify tool falls back to `osascript` and macOS already showed the banner. The history endpoint exists to recover notifications the user missed **between SSE sessions while the daemon was running** (e.g. Desktop crashed and relaunched), not to replay banners that the OS already delivered through the fallback path.

Query params:

- `since` — only return events with ID strictly greater than this. Use `next_cursor` from a prior response as the cursor.
- `limit` — cap result count; on truncation the **most recent** are kept. `0` (default) = no cap.
- `types` — comma-separated subset of event types to include (e.g. `types=notification,approval_request`). Default = all four captured types.

```bash
curl http://localhost:7533/notifications?limit=50
curl "http://localhost:7533/notifications?since=120&types=notification,agent_error"
```

Response shape:

```json
{
  "notifications": [
    { "id": 121, "type": "notification", "payload": { /* original event payload */ } },
    { "id": 134, "type": "approval_request", "payload": { /* ... */ } }
  ],
  "next_cursor": 134
}
```

If no events match, `notifications` is `[]` and `next_cursor` echoes the `since` value (or `0`).

**Cursor caveat:** the `next_cursor` advances past all events seen during the call, regardless of the `types` filter. If a client paginates with `types=notification` and later widens to `types=notification,approval_request`, any `approval_request` events with ID ≤ the prior cursor are skipped. Clients that change the `types` filter mid-session should rewind the cursor to `0`.

**Malformed query params** (e.g. `since=abc`, `limit=-1`) silently fall back to defaults — the endpoint never returns 400. This is intentional: a stale Desktop client should degrade to "show latest 500" rather than fail closed and hide all history.

### `approval_request`

```json
{
  "request_id": "apr_1f2a",
  "session_id": "sess_abc",
  "agent": "bot",
  "tool": "bash",
  "title": "check repo state",
  "source": "slack",
  "channel": "C123",
  "args": "{\"command\":\"git status\",\"description\":\"check repo state\"}",
  "flags": ["always_allow_disabled"],
  "ts": "2026-05-15T01:23:45Z"
}
```

### `schedule_run`

```json
{ "schedule_id": "sch_1", "session_id": "", "agent": "bot", "phase": "started", "ts": "2026-05-15T09:00:00Z" }
{ "schedule_id": "sch_1", "session_id": "sess_99", "agent": "bot", "phase": "succeeded", "ts": "2026-05-15T09:00:42Z" }
{ "schedule_id": "sch_2", "session_id": "", "agent": "bot", "phase": "failed", "error": "agent boom", "ts": "2026-05-15T09:00:42Z" }
```

## Backward compatibility

- `args` / `is_error` / `preview` on `tool_status` are **additive** — older subscribers that ignore unknown fields keep working.
- `ts` is additive on `tool_status` and `usage`.
- `session_id`, `agent`, `title`, `source`, `channel`, `args`, `flags`, `ts` on `approval_request` and `resolved_by` / `ts` on `approval_resolved` are additive; older Desktop clients that only read `request_id` / `tool` / `decision` keep working.
- `schedule_run` is a new event type; old clients that don't decode it ignore it.
- Older builds that don't emit the `usage` event type simply don't fire it; new Desktop code degrades gracefully (usage row stays hidden).
