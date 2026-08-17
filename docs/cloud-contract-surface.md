# Contract surfaces this repo owns

Changes in this repo can silently break two sibling repos, and historically have.
This file lists every byte-level surface ShanClaw exposes, and where the code on
the other side lives.

**Before editing, check whether you are touching a surface below. If you are,
open the consumer code and confirm it still works — do not guess.** If you cannot
reach the sibling repo in this session, stop and say so rather than assuming.

The counterpart document, written from the Cloud side, is
`shannon-cloud/contracts/shanclaw-surface.md`. Keep the two consistent: a change
that lands here and not there is how these surfaces drift apart.

## How to reference code in this file

**File plus symbol name, never a line number.** All four cross-repo line
references that previously lived in this list had gone stale — every one of them,
by 165 to 1,100 lines. Symbol names survive refactors and are greppable from
either repo; line numbers rot silently and are worse than no reference at all,
because they read as precise.

The same applies to enumerating a set that lives in code. Do not copy a token
list, a kind enum, or a count into prose — point at the declaration. A number
written down here is wrong the first time someone appends to the slice.

## Landing order

Cloud generally lands first and stays backward compatible, because the daemon
ships to user machines and cannot be updated in lockstep. New cross-version
behaviour is gated by a capability token, never by version sniffing. When you
change a surface:

1. Identify the consumer and open it.
2. Decide which side lands first and what gates the new behaviour.
3. Name the consumer-side file in the commit or PR description so a reviewer can
   trace the contract.

## Towards shannon-cloud (HTTP and WebSocket)

### `/v1/completions` — main conversation path

`internal/client/gateway.go`: `CompletionRequest`, `CompletionResponse`,
`GatewayClient.Complete`, `GatewayClient.CompleteStream`.

Cloud side: `go/orchestrator/cmd/gateway/internal/openai/handler.go` (a thin
proxy) forwarding to `python/llm-service/llm_service/api/completions.py`.

The body carries Anthropic-native content blocks (`tool_use`, `tool_result`,
`thinking`) to an OpenAI-named endpoint, and SSE is parsed by hand with
`bufio.Scanner` rather than an SDK. Both quirks are deliberate; neither is safe
to "clean up" from one side.

### `/api/v1/tasks/stream` — delegated research and swarm

`internal/client/gateway.go`: `TaskRequest`, `TaskStreamResponse`,
`TaskStatusResponse`. Dispatched from `internal/cloudflow/`. Cloud side is the
gateway, then the orchestrator and Temporal.

`TaskRequest` deliberately has **no** `CacheSource` field — this path does not
participate in prompt-cache attribution. Do not add one for symmetry with
`/v1/completions`.

### WebSocket handshake capability tokens

Advertised from `internal/daemon/client.go` (`Capabilities`) on the
`X-Kocoro-Capabilities` header and on `GET /status`. Cloud declares its own
`Cap*` constants in `go/orchestrator/internal/daemon/types.go`.

**The two sets are asymmetric, and the asymmetry is one-way**: every token Cloud
declares is also advertised by the daemon, but the daemon advertises many that
Cloud never names. Cloud forwards the header without interpreting those, and the
actual consumer is usually Desktop. `tool_use_id_events` is the canonical
example — zero Cloud-side constants, entirely consumed by Desktop UI card
pairing.

Two consequences when auditing:

- **A token missing from Cloud is not a bug by itself.** Find the real consumer
  before concluding anything is out of sync.
- **Do not grep a single file to decide a token is missing here.** Most
  constants sit in `internal/daemon/client.go`, but at least two do not:
  `CapIMMessageLifecycleV1` lives in `internal/daemon/lifecycle.go` and
  `CapSkillInstallRecommendationV1` in `internal/daemon/skill_recommendation.go`.
  The `Capabilities` slice is the only complete answer to "what does the daemon
  advertise".

### `ProactivePayload` — scheduled-task proactive IM push

This repo: `internal/daemon/types.go` (`ProactivePayload`) and
`internal/daemon/client.go` (`SendProactive`).
Cloud: `go/orchestrator/internal/daemon/types.go` (`ProactivePayload`) and
`go/orchestrator/internal/channels/outbound.go` (`reconstructProactiveMeta`).

`use_thread` is a `*bool` and its **presence** is the contract, not its
companion token. `nil` MUST map to current behaviour (anchored thread); only an
explicit `&false` goes top-level. Cloud reads the field directly — the
`proactive_thread_mode` token is observability only and is not load-bearing.

### `cache_source` attribution

Every LLM call tags a `cache_source`. Cloud owns TTL policy; the canonical
resolver is `resolve_prompt_cache_ttl_block` in
`python/llm-service/llm_provider/anthropic_provider.py`. This repo MUST treat the
label as attribution only and never as a TTL selector. Do not re-implement the
mapping on either side.

### Upload `kind` enum

This repo: `internal/uploads/client.go` (`validUploadKinds`, `IsValidKind`, and
the `Kind*` constants).
Cloud: the handler allowlist in
`go/orchestrator/cmd/gateway/internal/handlers/uploads.go`, the OpenAPI schema in
the same package, and a Postgres `CHECK` constraint under `migrations/postgres/`.

**This enum fails in a misleading way.** `validUploadKinds` short-circuits with
`ErrBadRequest` *before any HTTP call is made*. So when the two sides disagree,
the symptom looks like Cloud rejecting the upload, but the request never left the
process. Check the local enum first.

**Known drift, present as of 2026-08-13**: Cloud accepts `agent_avatar` — added
by the `139_user_uploads_kind_agent_avatar` migration extending the `CHECK`
constraint, and present in the handler allowlist — while `validUploadKinds` in
this repo still lists only `session_share`, `report`, `landing_page`, `image`,
and `other`. Cloud landed first, as intended; this side has not caught up. Any
upload from this repo with `kind=agent_avatar` fails locally.

### Usage fields and cost consistency

`internal/client/gateway.go` usage parsing, persisted onto the session. Four
points must agree on `cost_usd` and cache-token counts: the Python
`compute_token_cost`, the gateway JSON response, the session file under
`~/.shannon/sessions/`, and the `token_usage` table. The end-to-end recipe lives
in `CLAUDE.local.md` (not tracked — ask a maintainer if you need it).

Two failure modes worth knowing: `completions.py` must pass through the request
body's `cache_source` rather than hardcoding it, and the gateway handler's usage
struct must carry the cache-read and cache-creation fields or the database insert
silently drops them while `cost_usd` still looks correct.

### 429 sub-shape

`internal/runstatus/` (`parse429` in `parse.go`, and the `Code`/`Detail` types)
against the Cloud rate-limit response body. Unparseable or unknown bodies fall
back to a generic rate-limited code, so a Cloud-side shape change degrades
quietly rather than erroring — which means it will not be caught by tests that
only assert "an error was returned".

## Towards ShanClawDesktop (localhost HTTP and SSE)

Desktop never talks to Cloud directly on the main path; all of it flows through
the daemon Desktop embeds. Desktop does call Cloud REST directly for its control
plane (billing, onboarding, pairing), which is not this repo's surface.

### HTTP routes

Every `mux.HandleFunc(...)` in `internal/daemon/server.go` is consumed by
Desktop's `ShanClawBridge` package (`DaemonClient`). Adding, renaming, or
changing the response shape of a route is a contract change.

Routes the kocoro agent itself calls additionally need a matching reference under
`internal/skills/bundled/skills/kocoro/references/`; see the Doc Co-Maintenance
section of `CLAUDE.md`.

Conversation context actions are Desktop-only and gated by
`conversation_context_actions_v1`: `POST /sessions/{id}/fork` copies model
history through a complete assistant turn into a normal persisted session.
Its optional `agent` identifies the source session directory; optional
`target_agent` selects the destination agent directory (use `"default"` for
Default, and omit it to branch within the source agent).
`POST /sessions/{id}/side-chat` runs against that same bounded history plus the
panel's temporary user/assistant history. Side-chat runs carry the normal tool
registry and permission engine — identical capability to the primary
conversation. Tool approvals flow over the per-request SSE stream
(`approval` frames, resolved via `POST /approval`) exactly like `/message`;
`ask_user_question` gets no asker on ephemeral runs (the panel has no question
UI) and degrades to its clean "can't ask here" result. The runs stay ephemeral:
no session is persisted and no global bus events are published. The
implementation is `internal/daemon/conversation_context.go`; the Desktop
consumer is `DaemonClient+ConversationContext.swift`. Neither route belongs in
the bundled Kocoro skill references because the model never calls them.

`message_index` on both routes is a boundary in the RAW archive index space —
the `messages` array of the session file, system-injected entries included —
equal to (index of the last included message) + 1, and it must land on a
complete assistant turn. Desktop derives it from `SessionDisplayMapper`'s
`rawIndex` (raw `enumerated()` position, injected entries skipped for display
but never renumbered), so the two sides share one basis; this was cross-checked
against the Desktop implementation on 2026-08-17. Both routes honor a
compaction checkpoint whose coverage ends at or before the boundary: the fork
carries the checkpoint (deep-copied) and side-chat feeds the model
checkpoint+tail, never the full raw archive.

Desktop text replies use a transient head-only `<kocoro_replies>` prompt
envelope. `RunAgent` removes that envelope from the archived user message but
persists its decoded quotes and comments in the parallel
`message_meta[].conversation_annotations` display metadata. The metadata never
enters `HistoryForLoop`; it lets Desktop reconstruct the compact annotation
attachment immediately and after reload without exposing model-only markup.
The envelope limits are enforced server-side at both Desktop run ingresses
(`POST /message`, `POST /queue`) on the exact bytes the
model would receive: ≤ 100 replies per envelope run, quotes ≤ 8,000 runes,
comments ≤ 2,000 runes. Violations are 400s with stable codes
`conversation_replies_too_many` / `conversation_reply_quote_too_long` /
`conversation_reply_comment_too_long`, and a delimited-but-unparseable
envelope is `conversation_replies_malformed`. On paths that cannot reject
(queue drain, non-Desktop sources) a malformed envelope is kept verbatim as
ordinary text — user bytes are never silently dropped.

### Event payload shapes

`tool_status`, `approval_request`, `approval_request.flags`, and
`EventApprovalNotice` — Desktop UI cards bind to these field names. Canonical
JSON for every payload a UI client decodes lives in `docs/desktop-wire-fixtures/`
and is verified by `internal/daemon/wire_fixtures_test.go`, which emits through
the real producer path and decodes the produced bytes into consumer-shaped
structs. **Change a payload, update the fixture and the test in the same PR.**

### Work plans (`work_plan.updated` + `Session.work_plan`)

The daemon-owned durable progress checklist for one run (`internal/daemon/
work_plan.go`; session shape `session.WorkPlanSnapshot`). Desktop binds to the
`work_plan.updated` bus event (fixture `bus_event.work_plan.updated.json`) and
to the optional `work_plan` field on `GET /sessions/{id}`. Contract points:

- Full snapshot + monotonic `revision` is the recovery unit: consumers drop
  lower-or-equal revisions, may coalesce under backpressure, and refetch the
  session detail after a gap/reconnect. Closure bumps the revision.
- The event is emitted only after the covering durable save succeeded — a
  displayed revision can never vanish in a daemon crash.
- `lifecycle`/`close_reason` are runtime-owned; plan state never feeds
  Desktop's outer `TaskPresentationState`.
- Gated by capability token `work_plan_v1`.

### Approval-card `description`

The daemon does not block on a missing or empty `description`. Desktop's fallback
is `description?.trim() || fallback` — a logical OR, **not** nullish coalescing,
because an empty string must also fall through. Preserve that asymmetry when
touching either side.

### Filesystem layout

Desktop reads some paths under `~/.shannon/` directly, so the layout is a
contract and not an implementation detail. See the File Paths section of
`CLAUDE.md`.

### CLI surface

Desktop spawns the `shan` binary from its own helper bundle, so subcommands and
flags are a contract — including the hidden daemon isolation flags. Renaming a
flag breaks a shipped Desktop build that is pinned to the old name.

### Capability tokens gate Desktop UI

Desktop checks the daemon's advertised tokens before enabling matching cards, so
a feature that ships without minting a token half-renders on partially deployed
pairs. This is the historical trap that `display_name` and `model_tier` fell into
when they shipped as HTTP contract changes with no token.
