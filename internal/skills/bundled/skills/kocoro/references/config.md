# Config

## What is this?

Global settings control how Shannon behaves across all agents — which AI model to use, how to connect to the AI service, how long tools are allowed to run, and whether tools need approval before running. Settings are layered: global config, project config, and local config, with later layers overriding earlier ones.

## API Endpoints

### Get current config
- Method: GET
- Path: /config
- Response: `{"global": {...}, "effective": {...}, "sources": [...], "reload_required": false, "reload_reason": "..."}`
- Notes: `effective` is the daemon's currently loaded result. `sources` shows which config files contributed settings. If `reload_required` is true, `global` contains newer on-disk content that has not been applied to `effective`; call POST /config/reload or restart the daemon. This revision signal tracks only the global `~/.shannon/config.yaml`; it does not watch project or local overlay files. Clients gate this contract on `config_reload_state_v1`.

### Update config (deep merge)
- Method: PATCH
- Path: /config
- Body: `{"agent": {"model": "claude-opus-4-5"}}`
- Response: `{"status": "updated"}`
- Notes: PATCH merges deeply — you only need to include the fields you want to change. It writes the global file; follow it with POST /config/reload, then verify `effective` with GET /config. Protected fields (`endpoint`, `api_key`, their nested aliases `cloud.endpoint` / `cloud.api_key`, the legacy alias `gateway_url`, `sync.endpoint`, and `permissions.denied_commands`) return HTTP 409 and cannot be changed through this API. Unknown keys are rejected with HTTP 400 `{"error":"unknown_config_field","field":"<dotted.path>"}` — the daemon does not read such keys, so writing them would silently change nothing (e.g. `daemon.endpoint` is invalid; the real key is top-level `endpoint`). Setting a NON-protected key to `null` deletes it, including unknown/stray keys (that is the cleanup path for leftovers like a misplaced `daemon.endpoint`); protected fields return 409 even for `null` — removing a stray `gateway_url` requires editing `~/.shannon/config.yaml` directly. Keys are exact-case snake_case (`{"agent":{"Model":...}}` is rejected as unknown; protected fields are matched case-insensitively so case variants 409 rather than bypassing).

### Reload config from disk
- Method: POST
- Path: /config/reload
- Response: `{"status": "reloaded", "restart_required": true, "restart_reason": "..."}` (`restart_*` fields appear only when needed)
- Notes: Picks up changes made directly to config files on disk and clears `reload_required`. Also reconnects MCP servers. Endpoint changes and some legacy API-key changes still require the daemon restart reported by the response.

### Get config status
- Method: GET
- Path: /config/status
- Response: `{"reload_required": false, "reload_reason": "...", "mcp_servers": {"slack": "connected"|"enabled"|"disabled"}, "koe": {"enabled": bool, "model": "...", "voice": "...", "agent": "...", "language": "...", "audio_processing": "auto"|"mac_voice"|"clean_device", "fast_effort": bool|null}}`
- Notes: Shows whether the global `~/.shannon/config.yaml` is pending reload, plus live connection status for MCP servers and provider health. Project/local overlay edits are not represented by `reload_required`. The `koe` block reflects the voice front brain's settings (managed by Kocoro Desktop's settings panel; credential-free — Koe mints via the daemon, no key here). Clients gate the reload fields on `config_reload_state_v1`.

### Get daemon status
- Method: GET
- Path: /status
- Response: `{"is_connected": bool, "active_agent": string, "uptime": int_seconds, "version": string, "capabilities": [string], "memory": {...}}`
- Notes: `capabilities` is the list of daemon capability tokens this binary advertises — the same set the WS handshake sends to Cloud. UI clients read it to gate features behind a token rather than a version string, so a feature lights up only when the running daemon actually supports it. The canonical complete list is pinned by `docs/desktop-wire-fixtures/http_get.status.response.json`; never copy a partial list into a client. `config_reload_state_v1` gates the global-config revision fields on GET `/config` and GET `/config/status`. `agent_service_tier_v1` gates the global OpenAI Standard/Fast processing selector and means the daemon validates, checkpoints, and forwards it without leaking it into sealed Koe/computer profiles. `web_search_usage_v1` means live and terminal usage payloads always include `web_search_calls`, including an explicit zero. `computer_use_topology_v1` gates the strict read-only display-topology contract. `computer_use_control_v1` gates the local-presence-protected activity snapshot, heartbeat, and Pause/Resume/Take Over/Stop control plane; once present, clients fail closed on endpoint errors instead of falling back to legacy inference. `koe_fast_profile_v1` means Koe defaults semantic work to Fast, treats Realtime's valid Fast/Full mode as a soft routing judgment, records the closed `full_reason` only as diagnostic telemetry, resolves the trusted Fast profile through Cloud, pins the result across continuation/recovery, and fails closed to the unchanged Full Agent configuration on protocol/configuration/capability failure. `remote_session_timeline_v1` means `GET /sessions/{id}?view=remote_timeline` returns a byte-bounded mobile history page with explicit cursors and omission counts while the default session-detail endpoint remains lossless. `agent_default_cwd_v1` means named-agent cwd writes are validated, stale cwd loads return a warning instead of taking the agent down, and agent sync treats cwd as device-local. `schedule_broadcast_gate` advertises that the daemon honors the per-schedule broadcast gate (see `schedules.md`). `compaction_status_events_v1` means every automatic compaction pass is bracketed by `compaction_started`/`compaction_finished` run-status events (see `events.md`); without it a client cannot distinguish an older daemon from a run that never compacted. `im_timeline_v1` means the daemon's final answer travels only via `WORKFLOW_COMPLETED`, while Cloud renders each mid-turn `LLM_OUTPUT` as a discrete timeline narration segment interleaved with tool lines. `agent_avatar_v1` gates avatar editing in Desktop. `deliverable_event_v1` gates the live SSE path for the Deliverables sidebar; clients still dedupe live, replayed, and persisted deliverable records by `id`. `memory` is present only when the memory sidecar is configured (its `reason` / `detail` fields are documented in `memory.md`).

## Key Config Fields

| Field | Description | Protected |
|-------|-------------|-----------|
| `provider` | LLM backend: `""` (Shannon Cloud/Gateway) or `"ollama"` | No |
| `endpoint` | Shannon Cloud or custom gateway URL | YES |
| `api_key` | API key for the configured provider | YES |
| `model_tier` | Global LLM tier (default `medium`). User-facing tiers: `medium` / `large`. `small` exists but is reserved for daemon-internal sub-tier calls (skill discovery, micro-compaction); do not pin it via user config. Per-agent override is available via `agent.model_tier` in `~/.shannon/agents/<name>/config.yaml`. Precedence (highest first): `agent.model` (specific model id always wins) → `RunAgentRequest.ModelOverride` (heartbeat tier override) → `agent.model_tier` → local `config.yaml model_tier` → project `config.yaml model_tier` → global `config.yaml model_tier` → viper default `"medium"`. | No |
| `agent.model` | Default model for all agents (e.g., `claude-sonnet-4-5`). Must be a **concrete model id, NEVER a tier word** — to change the tier use `model_tier`. A tier keyword (`small`/`medium`/`large`) here is rejected by `PATCH /config` with HTTP 400 (and would fail config load if hand-written to yaml). | No |
| `agent.model_tier` | **Per-agent tier override.** When set in `~/.shannon/agents/<name>/config.yaml`, overrides the global `model_tier` for this agent only. User-pinnable values: `medium` / `large`. (`small` is reserved for daemon-internal sub-tier calls — do not use it as a per-agent override.) Omit to inherit. `agent.model` (specific model id) wins over `agent.model_tier` when both are set. | No |
| `agent.effort_tier` | **Unified reasoning-effort tier** (default unset). User-facing values: `low` (轻量) / `high` (均衡) / `xhigh` (深思) / `max` (极致). Cloud translates the tier to each provider's native effort at request time. Anthropic passes it straight through. GPT-5.6 maps `low→low` / `high→medium` / `xhigh→xhigh` / `max→max`; older OpenAI models keep the compatibility mapping `low→low` / `high→medium` / `xhigh→high` / `max→high`. Claude Haiku does not advertise effort support and stays at model default. Distinct from the OpenAI-native `agent.reasoning_effort` (Cloud prefers `effort_tier` when both are set, falling back to `reasoning_effort`). Per-agent override lives at `agent.effort_tier` in `~/.shannon/agents/<name>/config.yaml` (omit / `""` = inherit the global tier). A global change via PATCH /config needs `POST /config/reload` to take effect. | No |
| `agent.service_tier` | **Process-global OpenAI processing lane** (default unset). Accepted values: `""`, `default` (Standard), and `fast` (Fast processing). Desktop exposes this developer selector only with an exact OpenAI model. It is not merged from project/local overlays and is not a named-agent field; a named agent's model tier or exact model clears it. Sealed Koe/computer execution profiles own their processing lane and suppress the global value. Requires `agent_service_tier_v1`; a global PATCH needs `POST /config/reload`. | No |
| `koe.fast_effort` | **Automatic fast-task mode** (bool, default ON) — when enabled, Koe asks Realtime for a soft Fast/Full judgment and defaults bounded work to Fast. The closed `full_reason` label is diagnostic telemetry only and cannot upgrade Fast or downgrade Full. Tool count, file edits, tests, one failure, uncertainty, and elapsed time never upgrade a running Fast loop. The daemon independently normalizes the selected mode, resolves the trusted Fast profile through Cloud, and sends only its opaque id; the caller cannot choose a model. Missing/unknown mode, setting off, resolver/capability failure, or a daemon-validated inherited Full lineage still falls back to the complete normal global/per-agent configuration. This does not change the Realtime voice model and no longer forces normal Agent effort to `low`. | No |
| `agent.language` | **Reply-language lock** as a native name (e.g. `简体中文` / `日本語`); empty = mirror the user's current-message language (the default). When set, the agent replies in this language regardless of the input language and regardless of any other-language content in tool output / memory / skill descriptions / earlier turns. The one exception is an explicit user request to change the reply language (e.g. "reply in English") — the agent honors that for the rest of the conversation; a new session resets to the configured language. Per-agent override lives at `agent.language` in `~/.shannon/agents/<name>/config.yaml` (three-state: omit = inherit global; `""` = force mirror even when the global default is locked; value = lock). A global change via PATCH /config needs `POST /config/reload` to take effect; a per-agent change via PUT applies on the next turn. | No |
| `agent.temperature` | Creativity level 0.0–1.0. Lower = more predictable. | No |
| `agent.max_iterations` | Max tool-use rounds per conversation turn | No |
| `agent.context_window` | **Seed** value for the context window in tokens (default 1_000_000 — matches the 1M-context families that the medium/large tiers route to by default). On every main-tier LLM response the loop auto-adjusts to the observed model's known cap (1M for `claude-sonnet-4-6`/`opus-4-6`/`opus-4-7`; 200K for `claude-sonnet-4-5`/`haiku-4-5`/`opus-4-5`/`opus-4-1`; per-model values for OpenAI/Gemini/Grok). So you usually do NOT need to set this manually — the loop will discover the right value from response 2 onward. (Ollama callers automatically clamp the fallback to 200K because Ollama model names are absent from the auto-detect table; see `agent.ContextWindowFloorForProvider`.) | No |
| `agent.context_window` **per-agent override** | When set in `~/.shannon/agents/<name>/config.yaml`, the value locks against auto-detect — use this for cost caps (e.g. force 50000 tokens even on a 1M model) or for Ollama / custom-cap models where the global auto-detect table doesn't apply. Global `agent.context_window` is a seed; per-agent value is a lock. | No |
| `agent.skill_discovery` | Opt in to small-model skill matching on the first turn (default: false). Skill metadata listing and on-demand `use_skill` loading remain enabled. | No |
| `agent.idle_soft_timeout_secs` | Emit `OnRunStatus("idle_soft")` after this many seconds waiting on the LLM. 0 = disabled. Default: 90. | No |
| `agent.idle_hard_timeout_secs` | Cancel the run with `ErrHardIdleTimeout` after this many seconds idle. 0 = disabled (daemon startup WARN). Default: 540 (60s headroom under the 600s gateway transport ceiling). | No |
| `agent.stream_idle_timeout_secs` | Abort the SSE streaming body when no chunk arrives for this many seconds. Closes the silent-TCP-drop failure mode that `idle_hard_timeout_secs` can't catch. 0 = disabled (legacy scanner path). Default: 90. | No |
| `agent.interrupted_resume_max_attempts` | Maximum automatic daemon-start continuation attempts for one durable interrupted turn. Default: 3; `0`/unset also falls back to 3. The attempt is persisted before the LLM call, so a continuation that repeatedly crashes cannot consume an unbounded call on every restart. Raise this only when diagnosing a long recoverable provider outage. | No |
| `agent.interrupted_resume_max_age_hours` | Staleness window for daemon-start auto-resume. Checkpoints interrupted longer ago than this are abandoned (marker cleared, `interrupted_turn_abandoned` emitted) instead of resumed. Default: 4; `0`/unset also falls back to 4. | No |
| `agent.interrupted_resume_enabled` | Gates daemon-start auto-continuation of interrupted turns entirely. Default: `true`. `false` leaves checkpoints in place with no automatic execution. | No |
| `agent.compaction_snapshot_retention` | How many exact pre-compaction live-context snapshots to keep per session under `<sessions-dir>/.compaction-snapshots/<session-id>/`. The lossless transcript remains separately in `Session.Messages`; snapshots protect replacement of the durable model-live checkpoint. Inline image blocks are replaced by text markers before the synchronous JSON write. With retention ≥ 2 the oldest snapshot is pinned and rotation evicts from the second-oldest up; retention `1` keeps only the NEWEST snapshot. Daemon auto-compaction and TUI `/compact` both write snapshots; deleting a session removes them. Default: 1; `0` disables snapshotting. | No |
| `agent.compaction_snapshot_max_age_days` | Maximum age for compaction snapshot JSON/temp files. The daemon sweeps the default and every named-agent session scope once at startup; age expiry overrides the oldest-snapshot pin. Empty per-session directories are retained to avoid racing snapshot writers. Default: 14 days; `0` disables the age sweep. | No |
| `agent.compact_timeout_secs` | Deadline for one manual TUI `/compact` pass (persist-learnings plus a summarize that may fold an oversized transcript into sequential small-tier calls). Raise on slow gateways. Default: 300; `0` uses the default. | No |
| `agent.time_based_compact.enabled` | Master switch for time-gated tool_result clearing (default: false) | No |
| `agent.time_based_compact.gap_threshold_minutes` | Fire when (now − last assistant response) exceeds this operator-tunable history-retention threshold (default: 60; independent of Cloud cache TTL) | No |
| `agent.time_based_compact.keep_recent` | Most-recent compactable tool_results to retain verbatim; older ones are replaced with a placeholder marker (default: 5, floor: 1) | No |
| `agent.observation_window` | Keep the N most recent browser/GUI tool observations (navigate/snapshot/etc.) at full fidelity; older ones are replaced with a one-line stub, bounding the accumulated page/DOM history a long browser loop re-sends each iteration. 0 disables the window. Default: 3. | No |
| `agent.max_recent_images` | Keep the N most recent image-bearing messages; older screenshots become a `[previous screenshot removed to save context]` placeholder. Applies to all images (browser screenshots, uploads). Default: 50. 0 disables (keep all); negative is rejected at config load. | No |
| `agent.max_recent_browser_images` | Keep only the N most recent browser/GUI screenshots (scoped by tool); user uploads and non-GUI tool images stay under `agent.max_recent_images`. Default: 1. 0 disables the browser-scoped filter; negative is rejected at config load. | No |
| `tools.bash_timeout` | Max seconds a bash command can run (default: 120) | No |
| `tools.browser_result_truncation` | Per-observation capture cap (chars) for browser/GUI page/DOM dumps — tighter than `tools.result_truncation` because page dumps are large and front-loaded; truncation adds a self-describing marker. 0 = fall back to `result_truncation`. Default: 24000. | No |
| `daemon.auto_approve` | Skip approval prompts except for tools on the unattended deny-list (`computer_use`, `computer`, `accessibility`, `applescript`, `ghostty`). This generic switch never grants Computer Use; only an explicit persisted global `computer_use` permission does. | No |
| `daemon.skill_recommendations_enabled` | Immediate kill switch for the Desktop-only `skill.recommendation.v1` protocol. Default: `true` when unset. Setting `false` expires pending recommendations, cancels active continuations, hides discover/offer tools, and makes continue/dismiss unavailable; ordinary chat is unchanged. | No |
| `daemon.scratch_max_age_days` | Age limit for per-session MCP artifact scratch dirs (`~/.shannon/tmp/sessions/<id>/` — where e.g. playwright screenshots land when the model gives no filename). Swept once at daemon startup; only whole session dirs older than this are removed. Default: 14. 0 or negative disables the sweep. If a user asks "why did my screenshot disappear", an old scratch dir reclaimed by this sweep is the likely answer. | No |
| `permissions.allowed_commands` | Bash command-string allowlist (literal/glob + token-prefix family). See `permissions.md`. | No |
| `permissions.denied_commands` | Bash blocklist | YES |
| `permissions.always_allow_tools` | **Tool-level approval bypass** (global scope, applies to every agent). `computer_use` here is the single global Computer Use permission and is honored for interactive and unattended runs; no entry means unattended Computer Use fails closed. A named-agent approval still persists `computer_use` globally. Other tools may also use the companion per-agent field at `~/.shannon/agents/<name>/config.yaml permissions.always_allow_tools`; the runtime unions both. Legacy GUI wrappers (`computer`, `accessibility`, `applescript`, `ghostty`) cannot be persisted. High-risk bash prefixes (`pip install`, `rm -rf`, `python -c`, etc.) still prompt every call. Endpoints: `POST/DELETE /permissions/always-allow` (global), `POST/DELETE /agents/{name}/permissions/always-allow` (per-agent ordinary tools). | No |
| `cloud.publish_allowed_extensions` | Extra file extensions allowed for `publish_to_web` (e.g. `[".go", ".sql"]`). Additive on top of the built-in default; denylist is **not** user-configurable. | No |
| `cloud.stream_idle_timeout_secs` | Abort a cloud-delegate SSE connection when no line (event or 10s heartbeat) arrives for this many seconds, then reconnect via Last-Event-ID. Per-connection liveness probe, NOT a workflow time limit (`cloud.timeout` bounds total duration). 0 = disabled. Default: 45. | No |
| `mcp_servers` | External service integrations (see mcp reference) | No |
| `mcp.tool_timeout_secs` | Global bound (seconds) on a single MCP tools/call attempt; per-server `tool_timeout_secs` overrides it. Bounds one attempt, not the whole tool call — after a transport failure the daemon reconnects, and re-dispatches only read-only/idempotent-annotated tools (others surface an outcome-unknown error instead of a blind retry), so the theoretical worst case is two attempts plus reconnect. Cannot be disabled; `0` means the default. Default: 300. | No |
| `koe.audio_processing` | Voice microphone processing mode: `auto` (default), `mac_voice` (use Apple VoiceProcessingIO voice processing/AEC), or `clean_device` (for microphones/apps that already clean voice; keep VPIO device binding/playback but bypass Apple's voice processing). In `auto`, Koe uses `clean_device` only for a conservative list of known self-processed conference device/app pairs and otherwise keeps Mac voice processing. Kocoro Desktop exposes this under Voice → Advanced and forwards it to `shan koe --audio-processing`. | No |
| `koe.barge_in` | Enables VPIO full-duplex turn-taking. Koe pauses the exact assistant PCM locally, asks the native speech-to-speech model to choose only `resume_playback` (backchannel/no reply) or `accept_turn` (real interruption), then resumes or discards playback. ASR is not an admission dependency. Requires the VPIO backend. The bare `shan koe` CLI stays half-duplex when unset; Kocoro Desktop resolves an unset preference to enabled while preserving an explicit `false`. | No |

## Common Scenarios

### "Change the AI model"
1. PATCH /config with `{"agent": {"model": "claude-opus-4-5"}}`
2. POST /config/reload
3. Verify: GET /config → check `effective.agent.model`

### "Increase bash command timeout"
1. PATCH /config with `{"tools": {"bash_timeout": 300}}`
2. POST /config/reload
3. Verify: GET /config → check `effective.tools.bash_timeout`

### "Check which model is being used"
1. GET /config → look at `effective.agent.model`
2. `sources.agent.model` shows whether it came from global, project, or local config.

## `agent.prompt_suggestion` — Ghost-text "next prompt" suggestion

After each assistant turn, the daemon can generate a single 2-12 word
suggestion for the user's next message and render it as ghost text in the
input field. The user presses Tab / right-arrow to fill the input, then Enter
to send — no speculative pre-run of the next assistant reply.

| Key | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `true` | Master switch. When `false`, no suggestion calls fire. Set `false` to opt out of the per-turn forked completion entirely. |
| `cache_cold_threshold_tokens` | int | `10000` | Skip suggestion when previous turn's uncached input tokens exceed this. Protects against full-price calls. `0` disables the gate. |
| `min_turns` | int | `2` | Skip suggestion until this many turns have completed. First-turn predictions are usually unhelpful. |

Example:

```yaml
agent:
  prompt_suggestion:
    enabled: true
    cache_cold_threshold_tokens: 10000
    min_turns: 2
```

**Cost note:** Each suggestion is a potentially billed helper call. Cost varies
with the configured model, cache state, and whether `agent.thinking` is enabled.
Exact product measurements and cost baselines are maintained outside this
public repository. Disabled by default — opt in explicitly via this config or
the Desktop toggle.

## memory.* (Phase 2.3 — Kocoro Cloud memory feature)

| Key | Default | Notes |
|---|---|---|
| `memory.provider` | `disabled` | `disabled` / `cloud` / `local` — Episodic Memory is opt-in |
| `memory.endpoint` | `""` | Falls back to `cloud.endpoint` |
| `memory.api_key` | `""` | Falls back to `cloud.api_key`; never logged |
| `memory.socket_path` | `$TMPDIR/com.kocoro.tlm.sock` | UDS for sidecar HTTP |
| `memory.bundle_root` | `$HOME/.shannon/memory` | Bundle cache root |
| `memory.tlm_path` | `""` | Empty = `PATH` lookup; missing = silent disable |
| `memory.bundle_pull_interval` | `24h` | Cloud refresh cadence |
| `memory.bundle_pull_startup_delay` | `60s` | First pull delay on daemon boot |
| `memory.sidecar_ready_timeout` | `15s` | /health probe ceiling per spawn |
| `memory.sidecar_shutdown_grace` | `5s` | SIGTERM → SIGKILL grace |
| `memory.sidecar_restart_max` | `5` | Crashes tolerated before degraded |
| `memory.client_request_timeout` | `5s` | Per-request UDS timeout |

See `references/memory.md` for the full mode breakdown, diagnostics, and audit events.

## Safety Notes

- **Protected fields**: `endpoint`, `api_key` (and their `cloud.*` aliases), the legacy `gateway_url`, and `sync.endpoint` are protected. Attempting to modify them returns HTTP 409. These fields cannot be changed through this skill — the user must edit `~/.shannon/config.yaml` directly.
- **Three config levels**: Changes via PATCH /config write to the global config (`~/.shannon/config.yaml`). Project-level settings (`.shannon/config.yaml`) override global settings for that project. Local settings (`.shannon/config.local.yaml`) override both.
- **Reload after file edits**: If you edit config files directly on disk, call POST /config/reload so the daemon picks up the changes.
- **Model names**: Use exact model IDs from your provider. Invalid model names will cause conversations to fail at the start.
