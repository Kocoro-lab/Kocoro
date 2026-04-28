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
| `run_status` | Watchdog state: `idle_soft`, `idle_hard`, `llm_retry`. | Bus |
| `usage` | Per-LLM-call token and cost snapshot (on `OnUsage` boundary). | Bus |
| `agent_reply` | Agent finished a turn (WS / schedule / Ptfrog sources). | Bus |
| `agent_error` | Agent run failed. | Bus |
| `notification` | Agent-authored notify tool call. | Bus |
| `approval_request` | Tool needs user approval; payload `{request_id, tool, args}`. | Bus |
| `approval_resolved` | User answered an approval; payload `{request_id, decision}` where decision ∈ allow / deny / always_allow. | Bus |
| `approval_notice` | Post-decision feedback (e.g. high-risk pattern not persisted); payload `{severity, message}` where severity ∈ info / warn. | Bus |
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

`code` ∈ {`idle_soft`, `idle_hard`, `llm_retry`}. `detail` is human-readable and may encode the elapsed seconds or retry attempt count; consumers should extract with a tolerant regex rather than parse field-by-field.

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

## Backward compatibility

- `args` / `is_error` / `preview` on `tool_status` are **additive** — older subscribers that ignore unknown fields keep working.
- `ts` is additive on `tool_status` and `usage`.
- Older builds that don't emit the `usage` event type simply don't fire it; new Desktop code degrades gracefully (usage row stays hidden).
