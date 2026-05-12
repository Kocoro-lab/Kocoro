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
- Response: `{"mcp_servers": {"slack": "connected"|"enabled"|"disabled"}}`
- Notes: Shows live connection status for MCP servers and provider health.

## Key Config Fields

| Field | Description | Protected |
|-------|-------------|-----------|
| `provider` | LLM backend: `""` (Shannon Cloud/Gateway) or `"ollama"` | No |
| `endpoint` | Shannon Cloud or custom gateway URL | YES |
| `api_key` | API key for the configured provider | YES |
| `agent.model` | Default model for all agents (e.g., `claude-sonnet-4-5`) | No |
| `agent.temperature` | Creativity level 0.0–1.0. Lower = more predictable. | No |
| `agent.max_iterations` | Max tool-use rounds per conversation turn | No |
| `agent.context_window` | **Seed** value for the context window in tokens (default 200000). On every main-tier LLM response the loop auto-adjusts to the observed model's known cap (1M for `claude-sonnet-4-6`/`opus-4-6`/`opus-4-7`; 200K for `claude-sonnet-4-5`/`haiku-4-5`/`opus-4-5`/`opus-4-1`; per-model values for OpenAI/Gemini/Grok). So you usually do NOT need to set this manually for long-context models — the loop will discover the right value from response 2 onward. | No |
| `agent.context_window` **per-agent override** | When set in `~/.shannon/agents/<name>/config.yaml`, the value locks against auto-detect — use this for cost caps (e.g. force 50000 tokens even on a 1M model) or for Ollama / custom-cap models where the global auto-detect table doesn't apply. Global `agent.context_window` is a seed; per-agent value is a lock. | No |
| `agent.skill_discovery` | Enable small-model skill matching on first turn (default: true) | No |
| `agent.time_based_compact.enabled` | Master switch for time-gated tool_result clearing (default: false) | No |
| `agent.time_based_compact.gap_threshold_minutes` | Fire when (now − last assistant response) exceeds this; matches the Anthropic 1h cache TTL ceiling so no extra cache miss is forced (default: 60) | No |
| `agent.time_based_compact.keep_recent` | Most-recent compactable tool_results to retain verbatim; older ones are replaced with a placeholder marker (default: 5, floor: 1) | No |
| `tools.bash_timeout` | Max seconds a bash command can run (default: 120) | No |
| `daemon.auto_approve` | Skip approval prompts for all tool calls | No |
| `cloud.publish_allowed_extensions` | Extra file extensions allowed for `publish_to_web` (e.g. `[".go", ".sql"]`). Additive on top of the built-in default; denylist is **not** user-configurable. | No |
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
input field. Optional speculation pre-runs the response so acceptance is instant.

| Key | Type | Default | Description |
|---|---|---|---|
| `enabled` | bool | `false` | Master switch. When `false`, no suggestion calls fire. |
| `speculation_enabled` | bool | `false` | Pre-run the response assuming the user accepts. Doubles per-turn cost. Requires `enabled: true`. |
| `cache_cold_threshold_tokens` | int | `10000` | Skip suggestion when previous turn's uncached input tokens exceed this. Protects against full-price calls. `0` disables the gate. |
| `min_turns` | int | `2` | Skip suggestion until this many turns have completed. First-turn predictions are usually unhelpful. |

Example:

```yaml
agent:
  prompt_suggestion:
    enabled: true
    speculation_enabled: false
    cache_cold_threshold_tokens: 10000
    min_turns: 2
```

**Cost note:** With a warm prompt cache, each suggestion call ≈ 80% of one
main-turn cost. Speculation adds another ~80%. Disabled by default — opt in
explicitly per user preference (Desktop has a global toggle wired to this key).

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
