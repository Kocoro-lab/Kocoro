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

`execution-profiles-v1/` is the one inverse-direction exception: Shannon Cloud
mints that contract and is its canonical home. This repository vendors the same
named bytes and the same SHA-256 manifest. Both repositories validate their local copy in CI; neither
test suite depends on a sibling checkout or an absolute path.

## Surfaces

Four live transport families, named by file prefix:

| Prefix | Surface | Framing (not in fixture) |
|---|---|---|
| `bus_event.*` | `GET /events` broadcast SSE stream | `id: <n>\nevent: <type>\ndata: <payload>\n\n` — fixture is the `data` payload |
| `sse_event.*` | `POST /message` per-request SSE stream | `event: <name>\ndata: <payload>\n\n` — fixture is the `data` payload. NOTE: per-request event names differ from bus types (`approval` not `approval_request`, `tool` not `tool_status`) |
| `http_get.*` / `http_post.*` / `http_delete.*` | Plain HTTP request or response body | none — fixture is the whole JSON body |
| `computer_use.*` | Local Desktop computer-use control plane | Request/response bodies for the authenticated activity, control, and heartbeat routes. |

## File List

### Approval lifecycle

| File | Producer | Notes |
|---|---|---|
| `bus_event.approval_request.json` | `internal/daemon/approval.go makeApprovalRequestEmitter` | `args`/`title` are redacted+truncated bus copies; `flags` (optional, omitted when empty) carries policy hints like `always_allow_disabled` |
| `bus_event.approval_resolved.json` | `server.go handleApproval` (`POST /approval` ingress) | `resolved_by: "kocoro"` = a UI resolved it |
| `bus_event.approval_resolved.daemon_cleanup.json` | `approval.go makeApprovalCleanupEmitter` | timeout / ctx-cancel / disconnect; always `decision: "deny"`, `resolved_by: "daemon"`. Exactly one terminal event per request_id across both files |
| `bus_event.approval_notice.json` | `alwaysallow.go emitAlwaysAllowNotice` | post-decision feedback; `code` is the stable i18n key, `message` is English fallback |
| `sse_event.approval.json` | `server.go handleMessageSSE` per-request broker sendFn | full `ApprovalRequest` struct; `channel`/`thread_id`/`agent` are present-but-empty for foreground runs (no omitempty) |

### Ask-user question lifecycle

Second request/resolve interaction (`ask_user_question` tool). Same `pendingCore` lifecycle + at-most-one-terminal-event contract as approvals; distinct wire face. Capability token `question_v1`.

| File | Producer | Notes |
|---|---|---|
| `bus_event.question_request.json` | `internal/daemon/question_broker.go makeQuestionRequestEmitter` | `questions` is a bounded replay copy of the model-authored 1-4 array (`id` = `q0`..); non-identity display text is secret-redacted, while bounded option labels stay exact because they are response identities. `auto_resolution_ms` is optional (omitted when 0) |
| `bus_event.question_resolved.json` | `server.go handleQuestion` (`POST /question` ingress) | `action: "answer"`, `resolved_by: "kocoro"` = a UI answered it |
| `bus_event.question_resolved.daemon_cleanup.json` | `question_broker.go makeQuestionCleanupEmitter` | timeout / ctx-cancel / disconnect; always `action: "cancel"`, `resolved_by: "daemon"`. Exactly one terminal event per request_id across both files |
| `sse_event.question.json` | `server.go handleMessageSSE` per-request broker sendFn | full `QuestionRequest` struct on the per-request stream under the shorter frame name `question`; `channel`/`thread_id`/`agent` present-but-empty for foreground runs |

An `answer` response is all-or-nothing: it must contain exactly one entry for
every rendered question ID. Single-select questions carry exactly one value;
multi-select questions carry at least one. Values are full option labels (or
one custom value when `allow_other` is true), never option indexes or tokens.
Invalid responses leave the request pending so the UI can restore the card and
retry. `decline` carries no answers.

### Agent run events

| File | Producer | Notes |
|---|---|---|
| `bus_event.tool_status.running.json` | `bus_handler.go OnToolCall` | `tool_use_id` pairs running↔completed frames |
| `bus_event.tool_status.completed.json` | `bus_handler.go OnToolResult` | `elapsed` is float seconds; `preview` is redacted+truncated to 200 |
| `sse_event.tool.running.json` | `server.go sseEventHandler.OnToolCall` | bus shape minus `session_id`/`ts` (stream is request-scoped) |
| `sse_event.tool.completed.json` | `server.go sseEventHandler.OnToolResult` | same |
| `sse_event.usage.json` | `server.go sseEventHandler.OnUsage` | `web_search_usage_v1`: live usage always includes `web_search_calls`, including zero |
| `sse_event.done.json` | `server.go handleMessageSSE` (marshals `RunAgentResult`) | `web_search_usage_v1`: terminal usage always includes `web_search_calls`; optional fields omitted here: `partial`, `failure_code`, `message_start_index`, `message_end_index` (all omitempty, soft-failure metadata) |
| `sse_event.done.partial.json` | `server.go handleMessageSSE` (marshals `RunAgentResult`) | explicit soft-stop result: usable reply plus `partial: true` and stable `failure_code`; this constructed fixture leaves `message_start_index` / `message_end_index` at zero so `omitempty` removes them, while a live soft-stop result normally populates both; UI must not render it as verified completion |
| `bus_event.agent_reply.json` | `runner.go RunAgent` after the final session save | canonical clean persisted reply; `partial` and `failure_code` are absent |
| `bus_event.agent_reply.partial.json` | `runner.go RunAgent` after the final session save | persisted soft-stop reply on the broadcast bus; carries the same `partial`/`failure_code` classification as the per-request done payload |
| `bus_event.agent_error.json` | `runner.go RunAgent` hard-error path after saving the friendly error stub | failed run notification; unlike `agent_reply`, always carries a non-empty `failure_code` plus diagnostic and user-facing error strings |
| `sse_event.done.with_deliverable.json` | `server.go handleMessageSSE` (marshals `RunAgentResult`) | `message_idempotency_receipt_v2`: an empty chat reply plus daemon-validated `present_deliverable` metadata. A client that persists a local artifact requires this receipt before deleting its retained source |
| `bus_event.cloud_progress.json` | `bus_handler.go OnCloudProgress` | counts-only today; a future `items` array extension will be additive + capability-gated |
| `bus_event.suggestion_ready.json` | `runner.go fireSuggestionAfterRun` | post-turn suggested next user prompt |
| `bus_event.deliverable.json` | `bus_handler.go makeDeliverableEventHandler` | daemon-validated local regular-file metadata emitted by `present_deliverable`; Desktop dedupes live/replay/history records by `id` |
| `bus_event.computer_use.activity.json` | `guicontrol.Coordinator` through the `Server` event sink | Dotted event type `computer_use.activity`; schema v1 redacted activity payload. `coordinator_instance_id` is immutable for one daemon coordinator process, while `revision` is coordinator-owned and independent from the SSE event ID. Pointer geometry is bound to `topology_id` + `topology_generation`. Nullable result/path/pointer/failure fields never carry action content. |
| `bus_event.computer_use.activity.scroll.json` | same | Verified Accessibility scroll activity. It pins `action_kind: scroll`, `execution_path: accessibility`, and an explicit null pointer so Desktop never invents a click/move pulse for a semantic AX scroll. |

`agent_reply` includes `failure_code` only when `partial` is true;
`agent_error` always includes a non-empty `failure_code`. The value is an open
string enum on terminal run payloads. Current producer values are declared in
`internal/runstatus/runstatus.go`. Consumers may localize known values, but
must preserve forward compatibility by decoding an unknown non-empty value and
showing a generic partial/failure fallback instead of rejecting or dropping
the event.

### Computer-use control plane

| File | Producer | Notes |
|---|---|---|
| `computer_use.activity_snapshot.active.json` | `GET /local/computer-use/activity` via `EncodeComputerUseActivitySnapshot` | Reconnect snapshot with one active lease. `schema_version`, `coordinator_instance_id`, and `revision` appear only in the envelope; `active` reuses the event's state fields without duplicating them. |
| `computer_use.activity_snapshot.waiting_confirmation.json` | same | Waiting-for-user snapshot with only the content-free consequential-risk marker. Labels, destination, target digest, and tool arguments are forbidden. |
| `computer_use.activity_snapshot.idle.json` | `GET /local/computer-use/activity` via `EncodeComputerUseActivitySnapshot` | Explicit idle state: the coordinator instance and its last revision remain visible while `active` is JSON null. |
| `computer_use.control.stop.request.json` | `POST /local/computer-use/control` via `DecodeComputerUseControlRequest` | Stop command identity only: lease + idempotency key. Stop intentionally has no stale-revision precondition. |
| `computer_use.control.stop.response.json` | `POST /local/computer-use/control` via `EncodeComputerUseControlResponse` | Fixture represents cancellation of an in-flight action: accepted stop advances the revision and reports `lease_state: stopping` until the executor acknowledges quiescence. A stop with no in-flight action reports `terminal` immediately. |
| `computer_use.heartbeat.request.json` | `POST /local/computer-use/heartbeat` via `DecodeComputerUseHeartbeatRequest` | Strict schema-v1 controller heartbeat bound to one active lease. |
| `computer_use.heartbeat.response.json` | `POST /local/computer-use/heartbeat` via `EncodeComputerUseHeartbeatResponse` | Atomic coordinator identity/revision plus refreshed heartbeat and expiry timestamps. |
| `computer_use.app_policy.update.request.json` | `PUT /local/computer-use/app-policy` | Strict canonical bundle id plus the only mutable V1 decisions: `ask` or `blocked`. `always_allow` is not part of this contract. |
| `computer_use.app_policy.revoke.request.json` | `DELETE /local/computer-use/app-policy` | Removes one user entry so that the app returns to default Ask; built-in entries are immutable. |
| `computer_use.app_policy.update.response.json` | `PUT /local/computer-use/app-policy` | Full redacted snapshot returned after an update. Entries contain bundle id, decision, and source only; no titles or AX values. |
| `computer_use.risk_intent.detail.response.json` | `GET /local/computer-use/risk-intents/{intent_id}` | Authoritative, no-store, local-presence-only point-of-risk detail. The committed fixture contains synthetic labels/digest solely to pin the local wire; runtime details are process memory only and never enter activity events or persistence. |
| `computer_use.risk_intent.detail.coordinate.response.json` | same | Synthetic-coordinate variant. `coordinate_authority` binds the single left click to one AX path, immutable frame/image digest, topology/helper/display, source pixel, and mapped Quartz point; the intent/grant may never outlive `frame_expires_at`. Accessibility targets carry explicit JSON null instead. |
| `computer_use.risk_intent.allow.request.json` | `POST /local/computer-use/risk-intents/{intent_id}/decision` | One-shot allow; body must echo the exact path `intent_id`. |
| `computer_use.risk_intent.allow.response.json` | same | Content-free confirmation receipt and grant expiry. The execution grant itself remains process-local and is bound to request id + target digest + the exact canonical consequential detail. |
| `computer_use.risk_intent.deny.request.json` | same | One-shot deny; body must echo the exact path `intent_id`. |
| `computer_use.risk_intent.deny.response.json` | same | Content-free denial receipt; `grant_expires_at` is explicit JSON null. |

All local routes require `X-Kocoro-Local-Presence`; localhost CORS admission
is not authentication. Risk confirmation additionally requires the actual TCP
peer to be loopback. Control, heartbeat, app-policy, and risk-decision bodies
reject unknown, duplicate, missing, and trailing JSON members through strict
codecs.

### HTTP responses

| File | Producer | Notes |
|---|---|---|
| `http_get.status.response.json` | `server.go handleStatus` | pins the FULL `capabilities` token list — adding a token without updating this fixture fails the daemon test, which is the point (minting discipline made mechanical). `memory.reason` is explicit-null, not omitted. `uptime` is dynamic (normalized in tests) |
| `http_get.config.reload_required.response.json` | `server.go handleGetConfig` | pins the split between newer on-disk `global` config and the daemon's stale in-memory `effective` config, including the actionable `reload_required` / `reload_reason` fields |
| `http_get.config_status.reload_required.response.json` | `server.go handleConfigStatus` | pins the lightweight reload warning exposed alongside runtime config status; gated for clients by `config_reload_state_v1` |
| `http_delete.skill.builtin.response.json` | `server.go handleDeleteGlobalSkill` | stable `skill_is_builtin` code plus a human-readable deletion error |
| `http_delete.skill.invalid_agent_manifest.response.json` | `server.go handleDeleteGlobalSkill` | recoverable 409 for a malformed `_attached.yaml`; the fallback message names the agent update route used to repair it |
| `http_get.computer_use_topology.response.json` | `computer_use_topology.go handleComputerUseTopology` (`GET /local/computer-use/topology`) | daemon-owned transport fixture copied semantically from the helper's canonical `display_topology.mixed_horizontal.v1.json`. The HTTP body is the strict topology object itself, with no wrapper. Desktop vendors this fixture from the daemon repo; the endpoint exposes topology only, never coordinate-capture image bytes. |
| `http_get.agents.response.json` | `server.go handleAgents` | list items use `override` |
| `http_get.agent_detail.response.json` | `server.go handleGetAgent` (`AgentAPI`) | detail uses `overridden` — historical field-name divergence, pinned here so neither side "fixes" it unilaterally. `memory`/`config`/`commands`/`skills` are explicit-null when absent |
| `http_get.sessions.scope_all.response.json` | `server.go handleSessions` (`GET /sessions?scope=all`) | cross-agent merged list, sorted `pinned DESC, updated_at DESC`. Each row carries `agent` (empty = default scope, slug otherwise) and normalized `cwd` (empty = unlinked) — always emitted. Wrapper carries the complete pre-page `projects` catalog plus `total` and `has_more`. `id`/`created_at`/`updated_at` are dynamic (normalized in tests). Paginated via `limit` (default 100) / `offset` (default 0) — page/offset applied AFTER optional `project_cwd` filtering and merge+sort; single-scope `GET /sessions` and `GET /sessions?agent=<slug>` return the same wrapper+row shape with `agent` set to the queried scope |
| `http_get.sessions.schedule.response.json` | `server.go handleSessions` (`GET /sessions?schedule_id=<id>`) | exact scheduled-task session filter. Each matching row carries persistent `schedule_id`; deleting the schedule configuration does not delete or rewrite the session. |
| `http_get.session.response.json` | `server.go handleGetSession` (`GET /sessions/{id}`) | lossless default session detail. Top-level `messages` remain the archive; `compaction_checkpoint` additively exposes the durable model-live state and its exclusive raw archive index. No new capability token is required: clients that ignore the additive field retain the existing archive semantics; consumers that opt into live-context metrics read the checkpoint explicitly. |
| `http_get.session.remote_timeline.response.json` | `server.go handleGetSession` (`GET /sessions/{id}?view=remote_timeline`) | capability-gated mobile projection of the archive. Returns a byte-bounded newest page with aligned `messages` / `message_meta`, absolute `start_index`, opaque `next_cursor`, `has_more`, and explicit `omitted_content_count`; it projects from top-level archived `messages`, not the model-live checkpoint. |

### Quick-panel surfaces (POST request bodies + error responses)

`POST /message` optionally accepts `idempotency_key` together with a
client-minted `session_id` (`message_idempotency_v1`).
For `source=koe`, `koe_fast_profile_v1` additionally accepts the semantic
`execution_mode: "fast"|"full"` field. Missing and unknown values are `full`.
The client never sends provider/model/profile ids; the daemon resolves `fast`
through Cloud and falls back to the unchanged full Agent configuration on any
missing, invalid, or failed resolution.
`message_idempotency_receipt_v2` additionally persists validated deliverable
receipts and returns stable failed/in-progress error codes. The first request
durably records acceptance before running tools. A completed retry returns the
stored terminal result; an accepted-but-interrupted or failed request never
auto-runs again under the same key.

| File | Producer | Notes |
|---|---|---|
| `local_screenshot_window_request.json` | Desktop → `POST /local/screenshot/window` | `screenshotWindowRequest` struct; `window_title` included as empty string; `pid` + `app_name` both present (either is sufficient for the handler) |
| `local_screenshot_window_denied.json` | `screenshot_window.go handleScreenshotWindow` (403 branch) | `writeErrorCode` shape: `{"error":…,"code":…}`; `code` is the stable i18n key Desktop localises on; emitted when ax_server returns `screen_recording_denied` |
| `local_screenshot_window_success.json` | `screenshot_window.go handleScreenshotWindow` (200 branch) | `{"image_base64":…,"width":…,"height":…}`; anchors key names consumed by Desktop's `CaptureWindowResult` |
| `message_foreground_hint_request.json` | Desktop → `POST /message` | `RunAgentRequest` with `foreground_hint` populated; `source: "kocoro"` is the quick-panel source string; `foreground_hint` is folded into `StickyContext` by the runner, never forwarded to Cloud |
| `message_idempotency_request.json` | Desktop → `POST /message` | Crash-safe primary request with a client-minted `session_id` + stable `idempotency_key`; decoded and validated by the daemon and emitted by Desktop's production request builder |
| `message_koe_execution_fast_request.json` | Koe → `POST /message` | `source=koe` request: Koe pins `execution_mode` + `requested_execution_mode` to Fast and supplies client-minted lineage ids (`logical_task_id`, `execution_run_id`). No provider/model/profile fields — the daemon admits the request and resolves the profile itself |
| `message_koe_execution_full_request.json` | Koe → `POST /message` | Compatibility fixture for a follow-up inheriting an already validated Full run: adds `full_reason`, `parent_run_id` lineage, and the untrusted `inherited_execution_mode` claim (admission clears it; only ledger validation restores the Full floor) |
| `sse_event.done.with_execution_run.json` | `handleMessageSSE` `event: done` | `RunAgentResult` carrying the validated `execution_run` (lineage ids + the resolved kfp1 profile incl. `resolution_reason`) and hosted-search usage; Koe records it into the call ledger for follow-up/cancel routing |
| `sse_event.skill.recommendation.v1.json` | `server.go handleEvents` | Device-targeted, account-bound Desktop capability card. It is deliberately not an EventBus replay event: only the authenticated account + declared Desktop device's current SSE stream receives it. |
| `http_post.skill_recommendation_continue.request.json` | Desktop → `POST /skill-recommendations/{id}/continue` | The single **Install and continue** action: account/device/session-bound claim carrying the directed card token. Daemon installs the offer-time descriptor, enables it for the current Agent, records a receipt, then resumes attended SSE. |
| `http_post.skill_recommendation_continue.accepted.response.json` | same | Idempotent retry while the original continuation is running; HTTP 202. |
| `http_post.skill_recommendation_continue.completed.response.json` | same | Idempotent replay after completion; HTTP 200. A first claim instead upgrades this endpoint to the ordinary attended request-scoped SSE stream (`tool`, `approval`, `usage`, `done`/`error`). |
| `http_post.skill_recommendation_dismiss.request.json` | Desktop → `POST /skill-recommendations/{id}/dismiss` | Empty object; ownership comes from verified account plus Desktop device headers, never the body. |
| `http_post.skill_recommendation_dismiss.response.json` | same | Terminal dismissal acknowledgement. |

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
