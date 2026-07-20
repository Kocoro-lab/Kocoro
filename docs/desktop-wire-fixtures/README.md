# Daemon ↔ Desktop Wire Fixtures

## Purpose

Canonical JSON fixtures for the live wire contracts between the daemon and UI
clients (Kocoro Desktop). Both sides' tests match against the same files:

- **Daemon (Go)**: `go test ./internal/daemon -run TestWireFixture` emits each
  payload through the real production path (event emitters, full HTTP router)
  and asserts the produced bytes are semantically equal to the fixture, then
  decodes the produced bytes into consumer-shaped types.
- **Desktop (Swift)**: decodes the same fixture bytes through its production
  event/response decoders and asserts field-level expectations.

The problem this avoids: each side hand-writes its own sample JSON, misspells
one field (`override` vs `overridden` is a real historical trap in this very
API), both sides' unit tests pass green, and the integration silently breaks.
One error in the fixture → both sides fail → caught before merge.

**Any field-name / type / nesting change goes here first, then both sides
update code in sync.**

## Canonical Home & Sync Direction

This directory (in the daemon repo) is the canonical home: the fixtures
describe the daemon's own public wire protocol, and the daemon's CI runs the
producer-side tests against them. Consumer repos vendor a copy (record the
source commit SHA when copying) and re-sync whenever they bump the daemon
version they target. Sync is one-way — consumer repos never author fixture
changes locally; propose them here first. Fixture contents must describe only
what crosses the wire: no consumer-side type names, file paths, or internals.

## Surfaces

Three transport surfaces, named by file prefix:

| Prefix | Surface | Framing (not in fixture) |
|---|---|---|
| `bus_event.*` | `GET /events` broadcast SSE stream | `id: <n>\nevent: <type>\ndata: <payload>\n\n` — fixture is the `data` payload |
| `sse_event.*` | `POST /message` per-request SSE stream | `event: <name>\ndata: <payload>\n\n` — fixture is the `data` payload. NOTE: per-request event names differ from bus types (`approval` not `approval_request`, `tool` not `tool_status`) |
| `http_get.*` | Plain HTTP GET response body | none — fixture is the whole body |

## File List

### Approval lifecycle

| File | Producer | Notes |
|---|---|---|
| `bus_event.approval_request.json` | `internal/daemon/approval.go makeApprovalRequestEmitter` | `args`/`title` are redacted+truncated bus copies; `flags` (optional, omitted when empty) carries policy hints like `always_allow_disabled` |
| `bus_event.approval_resolved.json` | `server.go handleApproval` (`POST /approval` ingress) | `resolved_by: "kocoro"` = a UI resolved it |
| `bus_event.approval_resolved.daemon_cleanup.json` | `approval.go makeApprovalCleanupEmitter` | timeout / ctx-cancel / disconnect; always `decision: "deny"`, `resolved_by: "daemon"`. Exactly one terminal event per request_id across both files |
| `bus_event.approval_notice.json` | `alwaysallow.go emitAlwaysAllowNotice` | post-decision feedback; `code` is the stable i18n key, `message` is English fallback |
| `sse_event.approval.json` | `server.go handleMessageSSE` per-request broker sendFn | full `ApprovalRequest` struct; `channel`/`thread_id`/`agent` are present-but-empty for foreground runs (no omitempty) |

### Agent run events

| File | Producer | Notes |
|---|---|---|
| `bus_event.tool_status.running.json` | `bus_handler.go OnToolCall` | `tool_use_id` pairs running↔completed frames |
| `bus_event.tool_status.completed.json` | `bus_handler.go OnToolResult` | `elapsed` is float seconds; `preview` is redacted+truncated to 200 |
| `sse_event.tool.running.json` | `server.go sseEventHandler.OnToolCall` | bus shape minus `session_id`/`ts` (stream is request-scoped) |
| `sse_event.tool.completed.json` | `server.go sseEventHandler.OnToolResult` | same |
| `sse_event.done.json` | `server.go handleMessageSSE` (marshals `RunAgentResult`) | optional fields omitted here: `partial`, `failure_code`, `message_start_index`, `message_end_index` (all omitempty, soft-failure metadata) |
| `bus_event.cloud_progress.json` | `bus_handler.go OnCloudProgress` | counts-only today; a future `items` array extension will be additive + capability-gated |
| `bus_event.suggestion_ready.json` | `runner.go fireSuggestionAfterRun` | post-turn suggested next user prompt |
| `bus_event.deliverable.json` | `bus_handler.go makeDeliverableEventHandler` | daemon-validated local regular-file metadata emitted by `present_deliverable`; Desktop dedupes live/replay/history records by `id` |

### HTTP responses

| File | Producer | Notes |
|---|---|---|
| `http_get.status.response.json` | `server.go handleStatus` | pins the FULL `capabilities` token list — adding a token without updating this fixture fails the daemon test, which is the point (minting discipline made mechanical). `memory.reason` is explicit-null, not omitted. `uptime` is dynamic (normalized in tests) |
| `http_get.agents.response.json` | `server.go handleAgents` | list items use `override` |
| `http_get.agent_detail.response.json` | `server.go handleGetAgent` (`AgentAPI`) | detail uses `overridden` — historical field-name divergence, pinned here so neither side "fixes" it unilaterally. `memory`/`config`/`commands`/`skills` are explicit-null when absent |
| `http_get.sessions.scope_all.response.json` | `server.go handleSessions` (`GET /sessions?scope=all`) | cross-agent merged list, sorted `pinned DESC, updated_at DESC`. Each row carries `agent` (empty = default scope, slug otherwise) and normalized `cwd` (empty = unlinked) — always emitted. Wrapper carries the complete pre-page `projects` catalog plus `total` and `has_more`. `id`/`created_at`/`updated_at` are dynamic (normalized in tests). Paginated via `limit` (default 100) / `offset` (default 0) — page/offset applied AFTER optional `project_cwd` filtering and merge+sort; single-scope `GET /sessions` and `GET /sessions?agent=<slug>` return the same wrapper+row shape with `agent` set to the queried scope |
| `http_get.sessions.schedule.response.json` | `server.go handleSessions` (`GET /sessions?schedule_id=<id>`) | exact scheduled-task session filter. Each matching row carries persistent `schedule_id`; deleting the schedule configuration does not delete or rewrite the session. |
| `http_get.session.remote_timeline.response.json` | `server.go handleGetSession` (`GET /sessions/{id}?view=remote_timeline`) | capability-gated mobile projection. Returns a byte-bounded newest page with aligned `messages` / `message_meta`, absolute `start_index`, opaque `next_cursor`, `has_more`, and explicit `omitted_content_count`; the default session-detail response remains lossless |

### Quick-panel surfaces (POST request bodies + error responses)

| File | Producer | Notes |
|---|---|---|
| `local_screenshot_window_request.json` | Desktop → `POST /local/screenshot/window` | `screenshotWindowRequest` struct; `window_title` included as empty string; `pid` + `app_name` both present (either is sufficient for the handler) |
| `local_screenshot_window_denied.json` | `screenshot_window.go handleScreenshotWindow` (403 branch) | `writeErrorCode` shape: `{"error":…,"code":…}`; `code` is the stable i18n key Desktop localises on; emitted when ax_server returns `screen_recording_denied` |
| `local_screenshot_window_success.json` | `screenshot_window.go handleScreenshotWindow` (200 branch) | `{"image_base64":…,"width":…,"height":…}`; anchors key names consumed by Desktop's `CaptureWindowResult` |
| `message_foreground_hint_request.json` | Desktop → `POST /message` | `RunAgentRequest` with `foreground_hint` populated; `source: "kocoro"` is the quick-panel source string; `foreground_hint` is folded into `StickyContext` by the runner, never forwarded to Cloud |

## Comparison Rule: Semantic Equality, Not Byte Equality

Go map serialization does not guarantee key order, and several producers build
payloads via `map[string]any`. Compare after re-parsing into a struct/dict,
never byte-by-byte:

```
fixture  = parse(readFixture(name))
produced = parse(bytesFromProductionEmitter())
normalize(produced, dynamicFields)   // ts, elapsed, uptime, generated ids
assertDeepEqual(fixture, produced)
```

Dynamic fields (`ts`, `elapsed`, `uptime`, generated `request_id`s) are
asserted by format (RFC3339 / numeric / prefix) and then normalized to the
fixture's value before the deep compare.

## Change Rules

- New event type or endpoint consumed by a UI client → add a fixture + both
  sides' decode tests in the same change, and mint a capability token if the
  change is cross-version (see CLAUDE.md "Capability token discipline").
- Field rename / type change → change the fixture first, then both sides'
  code in sync. Additive optional fields are back-compat safe (UI clients
  ignore unknown keys); removals and renames are breaking.
- **Don't bypass the fixtures and change both sides' code directly** — that is
  exactly the failure mode this directory exists to prevent.
