# Config

## What is this?

Global settings control how Shannon behaves across all agents — which AI model to use, how to connect to the AI service, how long tools are allowed to run, and whether tools need approval before running. Settings are layered: global config, project config, and local config, with later layers overriding earlier ones.

## API Endpoints

### Get current config
- Method: GET
- Path: /config
- Response: `{"global": {...}, "effective": {...}, "sources": {"provider": "global", "endpoint": "global"}}`
- Notes: `effective` is the merged result. `sources` shows which config file each setting came from.

### Update config (deep merge)
- Method: PATCH
- Path: /config
- Body: `{"agent": {"model": "claude-opus-4-5"}}`
- Response: `{"status": "updated"}`
- Notes: PATCH merges deeply — you only need to include the fields you want to change. Protected fields (`endpoint`, `api_key`, `permissions.denied_commands`) return HTTP 409 and cannot be changed through this API.

### Reload config from disk
- Method: POST
- Path: /config/reload
- Response: `{"status": "reloaded"}`
- Notes: Picks up changes made directly to config files on disk. Also reconnects MCP servers.

### Get config status
- Method: GET
- Path: /config/status
- Response: `{"mcp_servers": {"slack": "connected"|"enabled"|"disabled"}, "koe": {"enabled": bool, "model": "...", "voice": "...", "agent": "...", "language": "..."}}`
- Notes: Shows live connection status for MCP servers and provider health. The `koe` block reflects the voice front brain's settings (managed by Kocoro Desktop's settings panel; credential-free — Koe mints via the daemon, no key here).

### Get daemon status
- Method: GET
- Path: /status
- Response: `{"is_connected": bool, "active_agent": string, "uptime": int_seconds, "version": string, "capabilities": [string], "memory": {...}}`
- Notes: `capabilities` is the list of daemon capability tokens this binary advertises — the same set the WS handshake sends to Cloud. UI clients read it to gate features behind a token rather than a version string, so a feature lights up only when the running daemon actually supports it. Current tokens include `delivery_ack`, `inline_document_b64`, `inline_extracted_text`, `tool_use_id_events`, `client_message_queue`, `im_message_lifecycle_v1`, `im_timeline_v1`, `agent_profile_v1`, `agent_avatar_v1`, `schedule_broadcast_gate`, `proactive_targeting`, `proactive_thread_mode`, `reply_delivery_result_v1`, `channel_state_event_v1`, and `deliverable_event_v1`. `schedule_broadcast_gate` advertises that the daemon honors the per-schedule broadcast gate (see `schedules.md`). `im_timeline_v1` means the daemon's final answer travels only via `WORKFLOW_COMPLETED`, while Cloud renders each mid-turn `LLM_OUTPUT` as a discrete timeline narration segment interleaved with tool lines. `agent_avatar_v1` gates avatar editing in Desktop. `deliverable_event_v1` gates the live SSE path for the Deliverables sidebar; clients still dedupe live, replayed, and persisted deliverable records by `id`. `memory` is present only when the memory sidecar is configured (its `reason` / `detail` fields are documented in `memory.md`).

## Key Config Fields

| Field | Description | Protected |
|-------|-------------|-----------|
| `provider` | LLM backend: `""` (Shannon Cloud/Gateway) or `"ollama"` | No |
| `endpoint` | Shannon Cloud or custom gateway URL | YES |
| `api_key` | API key for the configured provider | YES |
| `model_tier` | Global LLM tier (default `medium`). User-facing tiers: `medium` / `large`. `small` exists but is reserved for daemon-internal sub-tier calls (skill discovery, micro-compaction); do not pin it via user config. Per-agent override is available via `agent.model_tier` in `~/.shannon/agents/<name>/config.yaml`. Precedence (highest first): `agent.model` (specific model id always wins) → `RunAgentRequest.ModelOverride` (heartbeat tier override) → `agent.model_tier` → local `config.yaml model_tier` → project `config.yaml model_tier` → global `config.yaml model_tier` → viper default `"medium"`. | No |
| `agent.model` | Default model for all agents (e.g., `claude-sonnet-4-5`). Must be a **concrete model id, NEVER a tier word** — to change the tier use `model_tier`. A tier keyword (`small`/`medium`/`large`) here is rejected by `PATCH /config` with HTTP 400 (and would fail config load if hand-written to yaml). | No |
| `agent.model_tier` | **Per-agent tier override.** When set in `~/.shannon/agents/<name>/config.yaml`, overrides the global `model_tier` for this agent only. User-pinnable values: `medium` / `large`. (`small` is reserved for daemon-internal sub-tier calls — do not use it as a per-agent override.) Omit to inherit. `agent.model` (specific model id) wins over `agent.model_tier` when both are set. | No |
| `agent.language` | **Reply-language lock** as a native name (e.g. `简体中文` / `日本語`); empty = mirror the user's current-message language (the default). When set, the agent replies in this language regardless of the input language and regardless of any other-language content in tool output / memory / skill descriptions / earlier turns. The one exception is an explicit user request to change the reply language (e.g. "reply in English") — the agent honors that for the rest of the conversation; a new session resets to the configured language. Per-agent override lives at `agent.language` in `~/.shannon/agents/<name>/config.yaml` (three-state: omit = inherit global; `""` = force mirror even when the global default is locked; value = lock). A global change via PATCH /config needs `POST /config/reload` to take effect; a per-agent change via PUT applies on the next turn. | No |
| `agent.temperature` | Creativity level 0.0–1.0. Lower = more predictable. | No |
| `agent.max_iterations` | Max tool-use rounds per conversation turn | No |
| `agent.context_window` | **Seed** value for the context window in tokens (default 1_000_000 — matches the 1M-context families that the medium/large tiers route to by default). On every main-tier LLM response the loop auto-adjusts to the observed model's known cap (1M for `claude-sonnet-4-6`/`opus-4-6`/`opus-4-7`; 200K for `claude-sonnet-4-5`/`haiku-4-5`/`opus-4-5`/`opus-4-1`; per-model values for OpenAI/Gemini/Grok). So you usually do NOT need to set this manually — the loop will discover the right value from response 2 onward. (Ollama callers automatically clamp the fallback to 200K because Ollama model names are absent from the auto-detect table; see `agent.ContextWindowFloorForProvider`.) | No |
| `agent.context_window` **per-agent override** | When set in `~/.shannon/agents/<name>/config.yaml`, the value locks against auto-detect — use this for cost caps (e.g. force 50000 tokens even on a 1M model) or for Ollama / custom-cap models where the global auto-detect table doesn't apply. Global `agent.context_window` is a seed; per-agent value is a lock. | No |
| `agent.skill_discovery` | Enable small-model skill matching on first turn (default: true) | No |
| `agent.idle_soft_timeout_secs` | Emit `OnRunStatus("idle_soft")` after this many seconds waiting on the LLM. 0 = disabled. Default: 90. | No |
| `agent.idle_hard_timeout_secs` | Cancel the run with `ErrHardIdleTimeout` after this many seconds idle. 0 = disabled (daemon startup WARN). Default: 540 (60s headroom under the 600s gateway transport ceiling). | No |
| `agent.stream_idle_timeout_secs` | Abort the SSE streaming body when no chunk arrives for this many seconds. Closes the silent-TCP-drop failure mode that `idle_hard_timeout_secs` can't catch. 0 = disabled (legacy scanner path). Default: 90. | No |
| `agent.time_based_compact.enabled` | Master switch for time-gated tool_result clearing (default: false) | No |
| `agent.time_based_compact.gap_threshold_minutes` | Fire when (now − last assistant response) exceeds this; matches the Anthropic 1h cache TTL ceiling so no extra cache miss is forced (default: 60) | No |
| `agent.time_based_compact.keep_recent` | Most-recent compactable tool_results to retain verbatim; older ones are replaced with a placeholder marker (default: 5, floor: 1) | No |
| `agent.observation_window` | Keep the N most recent browser/GUI tool observations (navigate/snapshot/etc.) at full fidelity; older ones are replaced with a one-line stub, bounding the accumulated page/DOM history a long browser loop re-sends each iteration. 0 disables the window. Default: 3. | No |
| `agent.max_recent_images` | Keep the N most recent image-bearing messages; older screenshots become a `[previous screenshot removed to save context]` placeholder. Applies to all images (browser screenshots, uploads). Default: 50. 0 disables (keep all); negative is rejected at config load. | No |
| `agent.max_recent_browser_images` | Keep only the N most recent browser/GUI screenshots (scoped by tool); user uploads and non-GUI tool images stay under `agent.max_recent_images`. Default: 1. 0 disables the browser-scoped filter; negative is rejected at config load. | No |
| `tools.bash_timeout` | Max seconds a bash command can run (default: 120) | No |
| `tools.browser_result_truncation` | Per-observation capture cap (chars) for browser/GUI page/DOM dumps — tighter than `tools.result_truncation` because page dumps are large and front-loaded; truncation adds a self-describing marker. 0 = fall back to `result_truncation`. Default: 24000. | No |
| `daemon.auto_approve` | Skip approval prompts for all tool calls | No |
| `permissions.allowed_commands` | Bash command-string allowlist (literal/glob + token-prefix family). See `permissions.md`. | No |
| `permissions.denied_commands` | Bash blocklist | YES |
| `permissions.always_allow_tools` | **Tool-level approval bypass** (global scope, applies to every agent). List of tool names whose approval prompt is skipped — e.g. `[bash, file_write, http]`. Companion per-agent field lives at `~/.shannon/agents/<name>/config.yaml permissions.always_allow_tools`; the runtime unions both. Persistence is gated by `agent.DisallowsAutoApproval` (currently empty as of 2026-05-18) — no tool is currently refused, but the plumbing stays for future use. High-risk bash command prefixes (`pip install`, `rm -rf`, `python -c`, etc.) still prompt every call regardless. Endpoints: `POST/DELETE /permissions/always-allow` (global), `POST/DELETE /agents/{name}/permissions/always-allow` (per-agent). | No |
| `cloud.publish_allowed_extensions` | Extra file extensions allowed for `publish_to_web` (e.g. `[".go", ".sql"]`). Additive on top of the built-in default; denylist is **not** user-configurable. | No |
| `cloud.stream_idle_timeout_secs` | Abort a cloud-delegate SSE connection when no line (event or 10s heartbeat) arrives for this many seconds, then reconnect via Last-Event-ID. Per-connection liveness probe, NOT a workflow time limit (`cloud.timeout` bounds total duration). 0 = disabled. Default: 45. | No |
| `mcp_servers` | External service integrations (see mcp reference) | No |

## Common Scenarios

### "Change the AI model"
1. PATCH /config with `{"agent": {"model": "claude-opus-4-5"}}`
2. POST /config/reload (optional — model is picked up on next conversation)
3. Verify: GET /config → check `effective.agent.model`

### "Increase bash command timeout"
1. PATCH /config with `{"tools": {"bash_timeout": 300}}`
2. Bash commands can now run up to 5 minutes before timing out.

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

**Cost note:** Each suggestion call's cost depends on whether `agent.thinking`
is enabled. With thinking off + a warm prompt cache, suggestion ≈ 5-20% of one
main-turn cost (input mostly cache_read, output capped at ~30 tokens). With
thinking on, the fork inherits the same `thinking.budget_tokens` (cannot be
trimmed without invalidating the cache key), so cost rises to ≈ 50-90% of
one main-turn. Disabled by default — opt in explicitly via this config or
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

- **Protected fields**: `endpoint` and `api_key` are protected. Attempting to modify them returns HTTP 409. These fields cannot be changed through this skill — the user must edit `~/.shannon/config.yaml` directly.
- **Three config levels**: Changes via PATCH /config write to the global config (`~/.shannon/config.yaml`). Project-level settings (`.shannon/config.yaml`) override global settings for that project. Local settings (`.shannon/config.local.yaml`) override both.
- **Reload after file edits**: If you edit config files directly on disk, call POST /config/reload so the daemon picks up the changes.
- **Model names**: Use exact model IDs from your provider. Invalid model names will cause conversations to fail at the start.
