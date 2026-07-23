# Agents

## What is this?

Agents are specialized AI assistants that you configure for specific tasks or personas. Each agent has its own instructions, memory, and toolset — for example, a "customer-support" agent that always responds in a friendly tone, or a "code-reviewer" agent that only uses file-reading tools. Agents persist between conversations so they accumulate knowledge over time.

## API Endpoints

### List all agents
- Method: GET
- Path: /agents
- Response: `{"agents": [{"name": "...", "display_name": "...", "description": {"en": "..."}, "avatar": "", "builtin": false, "override": false}]}`
- Notes: Each entry includes `display_name` and `avatar`. When an agent has no display_name set, `display_name` falls back to the slug (`name`). `avatar` is the agent's avatar URL from `PROFILE.yaml`, or `""` when unset.
  - `description`: localized blurb (locale→text map) from `PROFILE.yaml`; empty when unset.

### Get agent details
- Method: GET
- Path: /agents/{name}
- Response: `{"name": "string", "display_name": "string", "prompt": "string", "config": {...}, "skills": [...], "commands": [...], "category": {...}|null, "description": {...}|null, "guide_prompts": [...]|null, "examples": [...]|null}`
- Notes:
  - `display_name` falls back to the slug when not explicitly set.
  - `warnings` contains non-fatal config advisories. In particular, an agent whose configured `cwd` was deleted or is otherwise invalid still returns `200` with its original `config.cwd` plus a warning, so clients can surface and repair it instead of losing access to the whole agent.
  - **Presentation metadata** (`category`, `description`, `guide_prompts`, `examples`) is populated when the agent ships a `PROFILE.yaml`; otherwise all four are `null`. Today only builtin agents (`explorer`, `reviewer`) carry profile data. All four fields are read-only — there is no write endpoint for them in v1. Wire shape:
    - `category`: `{"code": "coding", "label": {"en": "Coding", "zh-Hans": "编程", "ja": "コーディング"}}`. `code` is a slug from the global category registry; `label` is the daemon-inlined three-language display name. Unknown codes never appear in responses — `LoadAgent` fails loud at load time if a `PROFILE.yaml` references an unregistered code.
    - `description`: a `LocalizedString` map (`{"en": "...", "zh-Hans": "...", "ja": "..."}`). Plain text, not markdown.
    - `guide_prompts`: array of `{"title": LocalizedString, "prompt": LocalizedString}` — clickable starter cards.
    - `examples`: array of multi-turn dialogues. Each example has optional `title` (`LocalizedString`) and a `turns` array. Each turn has `role: "user"|"assistant"` and either `text` (user) or `markdown` + optional `tool_runs` (assistant). `tool_runs` is `[{"tool": "grep", "summary": LocalizedString}]` — compact chips, not full tool cards.
  - `LocalizedString` is an open `{locale_code: string}` map. Keys are BCP-47 short ids (`en`, `ja`, `zh-Hans`, `zh-Hant`, …). Clients fall back: current locale → primary language → `en` → first non-empty.
  - Capability gate: Desktop should show the richer profile UI only when `/status.capabilities` includes `agent_profile_v1`.

### Create agent
- Method: POST
- Path: /agents
- Body: `{"display_name": "My Agent", "prompt": "You are a helpful assistant that..."}`
- Response: `{"name":"...","display_name":"...","prompt":"...","memory":null,"config":null,"commands":null,"skills":null,"builtin":false,"overridden":false}`
- Notes:
  - The slug (`name`) is **always server-generated** — an immutable identifier of the form `agent-<6 hex>` (e.g. `agent-a3f7b2`) minted on the server and returned in the response. Clients do **not** supply it; any `name` sent in the body is ignored. The slug is the on-disk identity (directory name, session routing, Cloud binding) and never changes after creation.
  - `display_name` is **required** — a human-readable label in any language (e.g. Chinese). Stored in `config.yaml` and shown to the user. Missing / whitespace-only `display_name` returns `400`.
  - `display_name` must be globally unique (comparison is case-folded and whitespace-trimmed). A conflict returns `409` with `{"error": "display name \"X\" is already in use", "code": "display_name_taken"}`.
  - `display_name` must be ≤256 runes and contain no control characters → otherwise `400`.
  - `display_name` can **only** be set via the top-level `display_name` field. A `display_name` nested inside the `config` object is silently ignored (it would bypass the uniqueness check).
  - **Error codes**: display_name validation/conflict errors return `{"error": "<english msg>", "code": "<stable code>"}`. `error` is a non-localized English fallback; clients should localize by `code`:

    | code | status | meaning |
    |---|---|---|
    | `display_name_required` | 400 | missing / whitespace-only on create, or rename clearing to empty |
    | `display_name_too_long` | 400 | more than 256 runes |
    | `display_name_invalid_chars` | 400 | contains a control character |
    | `display_name_taken` | 409 | duplicate (case-folded, trimmed) |
  - To customize a built-in agent (`explorer` / `reviewer`), use `PUT /agents/{name}` against its slug — POST always creates a brand-new agent under a fresh auto-slug.
  - `avatar` is **optional** — the agent's avatar image URL, stored in `PROFILE.yaml` and synced to Cloud. Must be an `https://` CDN URL with a host; any other scheme (`http://`, `javascript:`, `data:`) returns `400`. Omit / send `""` for no avatar. Avatar editing is gated on the daemon advertising `agent_avatar_v1` in `/status.capabilities` — Desktop should expose the avatar field only when that capability is present.
  - A non-empty `config.cwd` must be an absolute path to an existing directory. Invalid values return `400` before any agent files are written.

### Update agent prompt / instructions
- Method: PUT
- Path: /agents/{name}
- Body: `{"prompt": "Updated instructions...", "display_name": "New Label"}`
- Response: `{"status": "updated"}`
- Notes:
  - `display_name` is optional (`null` or omitted = unchanged). Supplying it renames the agent's display label. Only `config.yaml` is updated — the slug, directory, sessions, schedules, and Cloud bindings are left untouched.
  - `display_name` cannot be cleared: sending `""` (empty / whitespace-only) returns `400` (`{"error": "display_name cannot be empty", "code": "display_name_required"}`). A named agent must keep a human-readable label rather than fall back to the opaque auto-generated slug. Omit the field (or send `null`) to leave it unchanged.
  - `display_name` can **only** be set/changed via this top-level field (which is uniqueness-checked). A `display_name` nested inside the `config` object is silently ignored.
  - Renaming to a `display_name` already used by another agent returns `409` with `{"error": "...", "code": "display_name_taken"}`. Renaming to the agent's own current `display_name` is a no-op success.
  - display_name errors carry the same `code` table as `POST /agents` above (`display_name_required` 400, `display_name_too_long` 400, `display_name_invalid_chars` 400, `display_name_taken` 409). `error` is a non-localized fallback; clients localize by `code`.
  - `avatar` is optional (omit = unchanged). Sending an `https://` CDN URL sets it; sending `""` clears it. Non-`https` values (`http://`, `javascript:`, `data:`) return `400`. Avatar editing is gated on the daemon advertising `agent_avatar_v1` in `/status.capabilities`.
  - When `config` is supplied, a non-empty `config.cwd` must be an absolute path to an existing directory. Invalid values return `400` before prompt, memory, config, or profile mutations are applied.

### Delete agent
- Method: DELETE
- Path: /agents/{name}?confirm=true
- Response: `{"status": "deleted"}`
- Notes: DESTRUCTIVE. The `?confirm=true` query parameter is required. Agent files are removed but historical sessions and memory snapshots in the sessions directory are preserved.

### Update agent config
- Method: PUT
- Path: /agents/{name}/config
- Body: `{"cwd": "/path/to/project", "agent": {"model": "claude-opus-4-5", "model_tier": "large"}, "tools": {"allow": ["bash:git *"], "deny": ["bash:rm *"]}}`
- Response: `{"status": "updated"}`
- Notes: Supports `cwd`, `agent.model`, `agent.model_tier`, `agent.language`, `agent.temperature`, `tools.allow`, `tools.deny`, `mcp_servers`, `permissions.always_allow_tools`. `agent.model_tier` (one of `small` | `medium` | `large`) overrides the global `model_tier` for this agent only; omit to inherit. When both are set, `agent.model` (specific model id) wins over `agent.model_tier`. **`agent.model` must be a concrete model id (e.g. `claude-opus-4-8`), NEVER a tier word — to select a tier use `agent.model_tier`. A tier keyword (`small`/`medium`/`large`) in `agent.model` is rejected with HTTP 400; if it ever slipped through it would reach the Gateway as `specific_model` and fail every run with `model_id_unknown`.** Full precedence (highest first): `agent.model` (specific model id always wins) → `RunAgentRequest.ModelOverride` (heartbeat tier override) → `agent.model_tier` → local / project / global `model_tier` → viper default `"medium"`. `agent.language` locks the reply language (three-state: omit = inherit global; `""` = force mirror the user; native name like `日本語` = lock).
- **This endpoint REPLACES the whole agent config (PUT semantics), except `display_name`.** To change a single config field, GET /agents/{name} first, merge your field into the returned config, then PUT the full body — otherwise sibling fields (`cwd`, `tools`, `mcp_servers`, `permissions`, `watch`, `heartbeat`, other `agent.*`) are dropped. `display_name` is preserved and can only be changed by the top-level `PUT /agents/{name}` `display_name` field; nested `config.display_name` is ignored.
- A non-empty `cwd` must be an absolute path to an existing directory. Invalid values return `400` before the existing config is changed.
- `cwd` is device-local and is excluded from cross-device agent sync. Pulling a newer cloud config updates every synced config field but preserves this device's current `cwd`; even a cloud config clear does not clear a local `cwd`. Older cloud payloads that still contain `cwd` are ignored. This complete behavior is advertised by `agent_default_cwd_v1`; clients should gate editable default-working-folder UI on that capability.
- Deleting or nulling config (`DELETE /agents/{name}/config`, or `PUT /agents/{name}` with `"config": null`) clears config fields but preserves `display_name` so the agent does not fall back to its opaque slug.

### Add tool to agent's always-allow list
- Method: POST
- Path: /agents/{name}/permissions/always-allow
- Body: `{"tool": "file_write"}`
- Response: `{"status": "added"}` on success; `400` if the tool is in `agent.DisallowsAutoApproval` (currently empty as of 2026-05-18).
- Notes: Appends the tool name to `permissions.always_allow_tools` in the agent's `config.yaml`. Next time this agent calls the named tool, the approval prompt is skipped. Idempotent (duplicate add is a no-op). Distinct from `tools.allow` — that's a schema filter (controls what the LLM can see); this is an approval bypass (controls whether the user is prompted at run time). Also written automatically when the user clicks "Always Allow" on an approval prompt (both bash and non-bash tools, as long as the message routed to a named agent) — Desktop/Cloud do not need to call this endpoint directly in that flow. **Safety gate that remains even with `bash` in this list**: high-risk bash commands (`pip install`, `rm -rf`, `python -c`, `git push --force`, etc.) still prompt every call — see the always-ask gate in `permissions.md`. `publish_to_web`, `generate_image`, and `edit_image` used to be non-persistable but are now ordinary approval-required tools.

### Remove tool from agent's always-allow list
- Method: DELETE
- Path: /agents/{name}/permissions/always-allow
- Body: `{"tool": "file_write"}`
- Response: `{"status": "removed"}`. No-op (200) if the tool is not in the list.
- Notes: Future calls to this tool from this agent will prompt for approval again.

### Add tool to GLOBAL always-allow list (all agents, incl. default)
- Method: POST
- Path: /permissions/always-allow
- Body: `{"tool": "bash"}`
- Response: `{"status": "added"}` on success; `400` if the tool is in `agent.DisallowsAutoApproval` (currently empty as of 2026-05-18).
- Notes: Appends to `permissions.always_allow_tools` in `~/.shannon/config.yaml` (global scope). Applies to EVERY agent including the default agent that has no per-agent config. Use this for tools the user trusts broadly (e.g. `bash`, `file_write`) so non-technical users on the default agent don't get re-prompted on every command-string variant. Use the per-agent endpoint when trust should be limited to a single agent. Same safety gates apply: high-risk bash commands (`pip install`, `rm -rf`, etc.) still prompt every call regardless.

### Remove tool from GLOBAL always-allow list
- Method: DELETE
- Path: /permissions/always-allow
- Body: `{"tool": "bash"}`
- Response: `{"status": "removed"}`. No-op (200) if absent.

### Attach skill to agent
- Method: PUT
- Path: /agents/{name}/skills/{skill}
- Response: `{"status": "attached"}`
- Notes: Skill must exist. See skills reference for how to install skills.

### Detach skill from agent
- Method: DELETE
- Path: /agents/{name}/skills/{skill}
- Response: `{"status": "deleted"}`

### Create agent command
- Method: PUT
- Path: /agents/{name}/commands/{cmd}
- Body: `{"content": "When user says /report, generate a daily summary..."}`
- Response: `{"status": "updated"}`
- Notes: Command name becomes a slash command the agent recognizes (e.g., `/report`).

### List sessions
- Method: GET
- Path: /sessions[?agent={name}][?scope=all][?schedule_id={id}][?project_cwd={path}][?limit={n}][?offset={n}]
- Response: `{"sessions": [{"id", "title", "cwd", "created_at", "updated_at", "msg_count", "agent", "source"?, "schedule_id"?, "kind"?, "in_progress"?, "awaiting_approval"?, "pinned"?, "favorite"?}], "projects": [{"cwd", "updated_at", "session_count"}], "total", "has_more"}`
- Notes: Empty sessions (msg_count == 0) are filtered out. List is ordered by `pinned DESC, updated_at DESC` — pinned sessions float to the top, otherwise the most recently active session comes first. `updated_at` reflects the last conversation activity (not metadata edits like rename / pin / favorite).
  - **Scope.** By default the list is a single scope: default-agent sessions, or a named agent's sessions when `agent={name}` is given. `scope=all` instead merges the default scope with every named agent's sessions into one cross-agent list (still ordered `pinned DESC, updated_at DESC`); `agent` is ignored when `scope=all` is set. Any `scope` value other than `all` (or omitting it) preserves the single-scope behavior.
  - **`agent` field** (always emitted): the scope each row belongs to — empty string `""` for the default agent, otherwise the agent slug. On the single-scope path it is the queried scope; on `scope=all` it attributes each merged row to its owning agent.
  - **Schedule filter.** `schedule_id={id}` returns only sessions created by that exact scheduled task. Matching scheduler sessions carry the same `schedule_id` in each row. The association is session metadata and survives deletion of the schedule configuration; deleting a schedule never deletes its sessions. Omit `schedule_id` for the normal unfiltered list.
  - **Pagination.** `limit` / `offset` window the result AFTER the merge+sort. `total` is the full count (after empty-session filtering, before paging); `has_more` is true when more rows exist beyond this page (`offset+limit < total`). Defaults: `offset=0`; `limit` defaults to `100` for `scope=all` but is **unlimited for a single scope** — omitting `limit` on the single-scope path returns ALL sessions (the historical pre-pagination behavior; existing callers are unaffected). Pass an explicit positive `limit` to opt into pagination on either path. Invalid / non-positive `limit`/`offset` fall back to these defaults (never a 400). In the unlimited case `total = len(sessions)` and `has_more = false`.
  - **Projects.** `cwd` is normalized and always emitted; `""` means a legacy/unlinked session. Supplying `project_cwd` filters the session rows before paging (a present empty value selects unlinked sessions). The `projects` catalog is computed before that filter and before paging, so it always describes every non-empty session in the selected agent scope(s), including projects outside the current page. `updated_at` is the project's most recent activity.
  - `source` identifies the originating IM / surface. Canonical values are the `Channel*` constants in `internal/daemon/types.go`, currently `slack`, `line`, `teams`, `wechat`, `wecom`, `web`, `feishu`, `lark`, `discord`, `telegram`, `schedule`, `system`, `webhook` — plus `kocoro` (set by `POST /messages` when the inbound request omits a source, i.e. the Desktop / TUI path). Empty / omitted on legacy sessions written before the column existed; frontends should treat empty as "unknown" and fall back to a generic icon. `kind` is a daemon-derived classification — `"interactive"` (manual chat; the catch-all, includes empty/`desktop`/`kocoro`/`tui`/`cli` sources), `"im"` (a messaging-platform push), or `"schedule"` (a scheduled run) — computed from `source` via an exclusion rule; clients should read `kind` directly for session grouping rather than re-deriving it from `source`. `in_progress` is true when the daemon currently owns an in-flight agent run for the session; `awaiting_approval` is true when the agent loop is blocked waiting for the user to approve a tool call. The two runtime flags are omitted (not emitted as false) when not set. A turn that reached a durable checkpoint before a crash is claimed and continued by the daemon's sequential startup-recovery worker; completed tool results are reused rather than replayed. `pinned` / `favorite` are user-set persistence flags managed via `PATCH /sessions/{id}` — both default to false and are omitted when false.

### Get session detail / remote timeline
- Method: GET
- Path: /sessions/{id}[?agent={name}][&view=remote_timeline][&before={cursor}][&limit={n}]
- Default response: the complete persisted session record, including its full `messages` array. This historical response is unchanged and may be large.
- `view=remote_timeline` response: `{"page_version":1,"id","title","cwd","created_at","updated_at","messages","message_meta","start_index","total_messages","has_more","next_cursor"?,"omitted_content_count"}`.
- Notes: The remote timeline view is advertised by `remote_session_timeline_v1` and is intended for Cloud-relayed mobile history. It returns the newest page first, keeps `messages` and `message_meta` index-aligned, never splits an adjacent assistant `tool_use` / user `tool_result` pair, and guarantees the encoded page stays below the daemon remote-response budget. `next_cursor` is opaque; pass it back as `before` to load the preceding page. `limit` defaults to 60 and caps at 100, but the byte budget may return fewer messages. Large inline images/documents become explicit text placeholders and verbose text/tool payloads are truncated; those user-visible projections increment `omitted_content_count`. Thinking blocks are silently omitted because they are not part of the user-visible transcript. The projection is response-only and never mutates the session on disk. Invalid cursors — including a cursor that falls inside an adjacent tool-use/result pair — and invalid limits return `400` with code `invalid_remote_timeline_page`.

### Search sessions
- Method: GET
- Path: /sessions/search?q={query}[?agent={name}][?scope=all][?project_cwd={path}]
- Response: `{"results": [{"session_id", "session_title", "cwd", "role", "snippet", "msg_index", "created_at", "updated_at", "agent"}]}`
- Notes: Full-text (trigram) search over session message history. Single-scope by default (default agent, or `agent={name}`); `scope=all` searches every scope (default + all named agents) and merges hits, ordered by `updated_at DESC`. Each result carries `agent`, normalized `cwd`, and `updated_at`, matching the List-sessions project convention. `project_cwd` optionally restricts hits to one exact project (present empty selects unlinked sessions). `snippet` wraps the matched span in `>>>…<<<`. Search is intentionally **unpaginated** — no `limit`/`offset`/`total`/`has_more` — so buried older sessions stay reachable regardless of the list-view page window (a per-scope cap of 20 matches applies without a project filter; project-filtered search scans a wider local window before filtering). `q` is required (400 if empty).

### Rename / pin / favorite a session
- Method: PATCH
- Path: /sessions/{id}[?agent={name}]
- Body: `{"title": "...", "pinned": true, "favorite": true}` — every field is OPTIONAL; supply only the ones you want to change. At least one of `title` / `pinned` / `favorite` must be present.
- Response: `{"status": "updated", "title"?, "pinned"?, "favorite"?}` — echoes only the fields that were changed.
- Notes: `title` must be a non-empty string after trimming. Setting `title` marks it user-chosen and permanently locks it against the automatic smart-title upgrade — once a session is renamed here, the daemon never overwrites that title (every new session otherwise gets a machine-derived title that an async small-model call upgrades to a smart summary on the first/third turn). `pinned` / `favorite` are independent booleans (a session can be one, both, or neither). Updates do NOT bump `updated_at`, so list order is preserved when only flags change; pinned sessions float to the top of `GET /sessions` regardless. Use `agent={name}` query parameter to target a named-agent session; omit for default-agent sessions.

### List sessions awaiting approval
- Method: GET
- Path: /approvals
- Response: `{"sessions": ["sess_abc", "sess_def"]}` (empty array when nothing pending)
- Notes: Returns the set of session IDs whose agent loop is currently blocked on an approval prompt. Use this on first connect / page refresh to re-sync — the `approval_request` SSE event covers live updates but is lossy across reconnect. Pairs with `POST /approval` (resolve a pending request_id) for the full approval flow.

### Reset agent session history (in place)
- Method: POST
- Path: /sessions/{id}/reset?agent={name}
- Response: `{"status": "reset", "id": "..."}`
- Notes: Clears the session's conversation history while keeping the session ID, title, CWD, source, channel, and cumulative usage. Cancels any active run on that session first. Also clears any persisted route binding (the link from a messaging-platform thread/sender to this session) and the live in-memory binding, so the next inbound message on that route starts a fresh session. The `agent` query parameter is REQUIRED — default-agent sessions do not use this endpoint; delete and recreate them via `DELETE /sessions/{id}` instead. Use when the user says "reset", "clear history", or "start over" on a named agent whose routing identity must survive the wipe.

### Share session as HTML
- Method: POST
- Path: /sessions/{id}/share[?agent={name}][&async=true|false]
- Async response (default — `async=true` or omitted, controlled by `daemon.share_async_default`):
  `202 {"task_id": "share-...", "session_id": "...", "agent": "...", "phase": "accepted", "status": "accepted"}`
- Sync response (`async=false`):
  `200 {"url": "https://...", "key": "...", "size": 12345, "upload_id": "uuid-or-empty", "summary_fallback": false}`
- Notes: Renders the session as a self-contained HTML page and uploads it to the cloud uploads endpoint, returning a public CDN URL. The HTML embeds a Haiku-generated 2-3 sentence overview at the top (≤120 chars, plain text — generated by `SummarizeForShareWithSource`, MaxTokens=200), inlines image attachments as `data:` URIs, drops file/PDF attachments and `thinking` blocks, folds every tool call into a collapsible `<details>` element, and strips home-dir paths, attachment paths, env-var assignments, and recognizable API-key shapes.

  **Async mode (default)** returns `202 Accepted` immediately and runs render+upload+list in a background goroutine with a 180s ceiling and a dedicated 180s HTTP client (vs. the global 600s default — share-path-only). Progress reports are emitted on `GET /events` as `share_progress` events with `phase` cycling through `accepted → rendering → uploading → listing → completed | failed | cancelled`. The terminal event carries `url` + `upload_id` (on `completed`) or `error` (on `failed`). The task snapshot is also polleable via `GET /sessions/{id}/share/tasks/{task_id}` for 5 minutes after the terminal phase.

  **Sync mode (`?async=false`)** is the legacy blocking shape — single 200 with the full body. Inherits the GatewayClient's 600s HTTPClient timeout (no 180s shortcut). Kept for scripted clients (curl, CLI tests) that cannot subscribe to SSE.

  `upload_id` is resolved via a follow-up `GET /uploads` lookup matched by URL — usually populated, occasionally empty under concurrent uploads (in which case retract via `GET /uploads` + `DELETE /uploads/{id}`). When `upload_id` is non-empty, the daemon also appends a `PublishedShareEntry` to the session file (see `GET /sessions/{id}/shares` below) so the UI has a fallback source-of-truth for retraction. `summary_fallback` is true when the Haiku call timed out or errored and the page used the session title (or first user message) as the summary instead. Requires `cloud.enabled` and a valid `api_key`; returns 503 otherwise. Returns 413 when rendered HTML exceeds 45 MiB (lower than the 50 MiB upload cap to leave headroom — usually means the session has too many large inline images). The same session may be shared repeatedly; each call uploads a new file (timestamp suffix in the name) and produces an independent `upload_id`.

### Poll an async share task
- Method: GET
- Path: /sessions/{id}/share/tasks/{task_id}
- Response: `{"task_id": "share-...", "session_id": "...", "agent": "...", "phase": "completed", "url": "...", "upload_id": "...", "created_at": "...", "updated_at": "..."}`
- Notes: Returns the current snapshot of an in-flight or recently-completed async share goroutine. Same payload shape as the `share_progress` SSE event. Primary consumption path is the `/events` stream — this endpoint is the polling fallback for clients that missed the terminal event (network blip, page reload) plus a self-test path for scripts. Snapshot is retained for 5 minutes past the terminal phase; later GETs 404. Returns 404 also when `task_id` does not exist or its `session_id` mismatches the path's session id (defense against UI bugs that probe other sessions' task ids).

### Retract a shared session
- Method: DELETE
- Path: /sessions/{id}/share?upload_id={id}[&agent={name}]
- Response: `{"deleted": true, "id": "uuid", "cdn_eviction_seconds": 300}`
- Notes: Thin session-context wrapper around `DELETE /uploads/{id}`. Records a session-aware audit row instead of a generic upload-retraction log line; otherwise identical semantics. `cdn_eviction_seconds` is the worst-case window during which CloudFront edge nodes may still serve the cached page — surface it to the user so the URL "still working" for a few minutes does not look like a failed retract. Idempotent at the cloud layer: a second retract of the same `upload_id` returns 404 (cloud deliberately conflates not-found / already-retracted / cross-user to avoid existence leaks). On success the daemon also filters the matching `UploadID` out of the session's `PublishedShares` list. `agent={name}` is optional but should match the value used at share time — when retracting a named-agent session without it, cloud-side retract still succeeds but the daemon's `PublishedShares` SoT update lands on the default-agent file (a no-op) instead of the named-agent's session, so `GET /sessions/{id}/shares?agent={name}` will still list the now-deleted upload.

### List a session's published shares
- Method: GET
- Path: /sessions/{id}/shares[?agent={name}]
- Response: `{"shares": [{"upload_id": "uuid", "url": "https://...", "filename": "session-...-20260518-051234.html", "created_at": "2026-05-18T05:12:34Z"}]}`
- Notes: Returns the daemon-side record of currently-active share artifacts for this session (append-only on share, filtered on successful retract). UI clients use this as a fallback when their own `upload_id` storage gets out of sync — a single GET recovers the IDs needed to retract. Always returns a JSON array (never null) under the `shares` key; empty array when nothing has been shared. 404 if the session does not exist. Read-only: this endpoint only reflects what `POST /share` and `DELETE /share` have already written; nothing is mutated.

### `GET /agents/{name}/sessions/{id}/suggestion`

Returns the latest prompt suggestion for the given session, or 404 if none.
Default-agent equivalent: `GET /sessions/{id}/suggestion`.

Response (200):
```json
{
  "text": "rerun the failing test",
  "suggested_at_unix": 1715500000
}
```

Errors:
- 400 if `id` is empty or contains path-traversal characters.
- 400 if `name` is not a valid agent name (regex: `^[a-z0-9][a-z0-9_-]{0,63}$`).
- 404 if the agent does not exist OR no suggestion is currently available for the session.

### `POST /agents/{name}/sessions/{id}/suggestion/accept`

Marks the current suggestion as accepted and returns the suggestion text
so Desktop can fill the input. The user still presses Enter to send — the
normal `POST /agents/{name}/messages` flow handles persistence. There is
no speculative pre-run of the next assistant reply.

Default-agent equivalent: `POST /sessions/{id}/suggestion/accept`.

Response (200):
```json
{
  "text": "rerun the failing test",
  "suggestion": "rerun the failing test",
  "suggested_at_unix": 1715500000
}
```

Errors: same shape as the GET endpoint.

### SSE event `suggestion_ready`

Emitted on `/events` when a new suggestion is generated. Payload:

```json
{
  "session_id": "sess_abc",
  "agent": "myagent",
  "text": "rerun the failing test"
}
```

Wire format follows the HTML5 EventSource spec (two lines per event,
separated by a blank line):

```
id: 42
event: suggestion_ready
data: {"session_id":"sess_abc",...}

```

Desktop's `EventSource.addEventListener("suggestion_ready", ...)` parses
`event.data` as JSON. There is no outer `{"type":...,"payload":...}`
wrapper — `event:` is a header line, `data:` is the JSON body.

## Common Scenarios

### "Create an email writer agent"
1. POST /agents with `{"display_name": "Email Writer", "prompt": "You are an expert email writer. Write professional, concise emails. Always ask for the recipient, purpose, and tone before drafting."}`
2. Read the auto-generated slug from the response (`name`, e.g. `agent-a3f7b2`), then verify with GET /agents/{slug}.

### "Restrict agent to read-only tools"
1. PUT /agents/{name}/config with `{"tools": {"allow": ["file_read", "glob", "grep", "directory_list"], "deny": ["file_write", "file_edit", "bash"]}}`
2. Agent will only be able to read files, never modify them.

### "Give agent access to a specific project"
1. PUT /agents/{name}/config with `{"cwd": "/Users/me/projects/myapp"}`
2. Agent's file operations will default to that directory.

### "Add a slash command"
1. PUT /agents/my-agent/commands/standup with `{"content": "Generate a standup report: what was done yesterday, what's planned today, any blockers. Check git log and open issues."}`
2. Users can now say `/standup` to trigger this workflow.

### "Stop asking me to approve file_write for this agent"
1. POST /agents/{name}/permissions/always-allow with `{"tool": "file_write"}`
2. The next time this agent invokes `file_write`, the approval prompt is skipped.
3. To revert: DELETE /agents/{name}/permissions/always-allow with the same body.

### "Let this agent run any bash command without asking"
1. POST /agents/{name}/permissions/always-allow with `{"tool": "bash"}`
2. From now on, every bash call from this agent skips approval — **except** commands matching the always-ask gate (`pip install`, `rm -rf`, `python -c`, `git push --force`, `npx`, `curl|sh`, etc.), which still prompt every call regardless. This is the tool-level alternative to authorizing individual command strings via `permissions.allowed_commands`.
3. Pair with a clear explanation to the user: this grants broad shell access (subject to the always-ask gate). For finer control, leave `bash` out of the list and use `permissions.allowed_commands` with specific command patterns instead.

### "Stop asking me on the default agent (for non-technical users)"
1. POST /permissions/always-allow with `{"tool": "bash"}` (and `file_write`, `http`, etc. as needed).
2. The global list applies to the default agent (and every other agent). Users without a named agent now also benefit from "click once, never asked again" — exactly mirroring the per-agent flow.
3. Use this when the user is non-technical and isn't going to create or name agents. Per-agent scoping is still preferred when the user explicitly works with multiple named agents.

## Safety Notes

- **Slug is server-generated**: The slug (`name`, `agent-<6 hex>`) is minted by the server on create and is the opaque identity key (directory, routing, Cloud binding). Clients never supply it; send only `display_name` (any language, e.g. `大螃蟹`, `日本茶`, `сергей`) as the user-facing label. After create, read the slug from the response and use it for all subsequent `/agents/{slug}` calls.
- **display_name uniqueness**: `display_name` must be unique across all agents (case-insensitive, whitespace-trimmed). Conflicts return 409.
- **Deletion is permanent**: Agent configuration, instructions, and memory are deleted. Sessions in `~/.shannon/sessions/` are not deleted.
- **`?confirm=true` required**: DELETE without this parameter returns an error, preventing accidental deletion.
- **Config changes take effect immediately**: No restart needed. The next conversation with the agent uses the new settings.
- **Tool restrictions are additive to global restrictions**: Agent-level deny rules combine with global deny rules; both must be satisfied.
