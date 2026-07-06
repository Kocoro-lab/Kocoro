# Kocoro Config Reference

Full reference for `~/.shannon/config.yaml`. The README shows a minimal example — this file documents every key, including defaults and tuning notes.

For multi-level merge behavior (global / project / local), see the README's `## Configuration` section.

## Connection

```yaml
endpoint: http://localhost:8080    # Shannon Gateway URL
api_key: ""                        # Gateway API key
model_tier: medium                 # medium | large (default: medium). "small" exists but is reserved for daemon-internal sub-tier calls (skill discovery, micro-compaction); UI surfaces and per-agent overrides should stick to medium / large.
provider: ""                       # empty = gateway; "ollama" for local Ollama
```

When `provider: ollama` is set:

```yaml
ollama:
  endpoint: "http://localhost:11434"   # default, can be omitted
  model: "llama3.1"
```

Kocoro then talks to Ollama's OpenAI-compatible API. Standard function tools work; native Anthropic tool types (computer use) are not available. Thinking models (e.g. Qwen3) surface reasoning with a `[thinking]` prefix.

## Permissions

```yaml
permissions:
  allowed_dirs:
    - ~/Documents/notes
    - ./docs
  allowed_commands:
    - "git *"
    - "go test *"
    - "make *"
  denied_commands:
    - "rm -rf *"
  network_allowlist:
    - "localhost"
    - "127.0.0.1"
    - "api.example.com"
  always_allow_tools: []           # tools to skip approval globally (see README "Daemon Mode" for safety gates)
```

See README `## Permission Engine` for the full resolution order.

## MCP Servers

```yaml
mcp_servers:
  filesystem:
    command: "npx"
    args: ["-y", "@modelcontextprotocol/server-filesystem", "/Users/you/Desktop"]
    context: "Filesystem access to ~/Desktop."

  my-remote:
    type: http
    url: "https://mcp.example.com/sse"
    context: "Remote MCP server providing custom tools."

  optional-server:
    command: "npx"
    args: ["-y", "some-mcp-server"]
    disabled: true
```

Per-server fields: `command` + `args` (stdio), `type: http` + `url` (HTTP), `env` (vars passed to the process), `context` (system-prompt guidance — critical for correct usage), `disabled: true` (skip without removing).

## Agent Behavior

```yaml
agent:
  max_iterations: 25               # max tool calls per turn (default: 25)
  temperature: 0                   # LLM temperature (default: 0)
  max_tokens: 32000                # max output tokens (default: 32000)
  model: ""                        # specific model override (empty = use model_tier)
  model_tier: ""                   # tier override (medium | large); empty = inherit global model_tier. "small" reserved for daemon-internal calls — do not pin via UI.
  reasoning_effort: ""             # "low" / "medium" / "high" (empty = model default)
  context_window: 1_000_000        # seed; auto-adjusted from observed model. Per-agent override locks the cap. (Ollama callers clamp to 200K — see ContextWindowFloorForProvider.)

  # Extended thinking (Anthropic native)
  thinking: true                   # enable extended thinking (default: true)
  thinking_mode: adaptive          # "adaptive" or "enabled" (default: adaptive)
  thinking_budget: 10000           # thinking token budget (default: 10000)
  force_think_tool: false          # re-enable local `think` tool even when native thinking is active. Use only if a workflow depends on the explicit planning tool surface.

  # Idle watchdog (two independent layers)
  idle_soft_timeout_secs: 90       # emit "still working" status after this long waiting on the LLM (0 = disabled)
  idle_hard_timeout_secs: 540      # cancel run as soft/partial failure after this long idle. Default 540 leaves 60s headroom under the 600s gateway transport ceiling so cancellation can propagate before transport bails. Set to 0 to opt out (daemon logs a startup WARN).
  stream_idle_timeout_secs: 90     # abort the SSE streaming body if no chunk arrives for this long. Closes the failure mode idle_hard_timeout_secs cannot catch: silent TCP-level stream drop where the kernel never returns from read(). 0 = disabled (legacy scanner path).

  # Skill matching
  skill_discovery: true            # per-turn small-model skill matching prefetch (default: true)

  # Prompt suggestion (ghost text)
  prompt_suggestion:
    enabled: false                 # default false. See README "Prompt Suggestion" for cost notes.
    cache_cold_threshold_tokens: 10000  # skip when cache is cold
    min_turns: 1                   # earliest turn that can fire

  # Time-based tool_result clearing for cache stability (off by default)
  time_based_compact:
    enabled: false
    gap_threshold_minutes: 60      # fire when last assistant response is older than this
    keep_recent: 5                 # number of trailing tool_result blocks to keep (floor: 1)

  # Browser/GUI context trimming (on by default) — bounds the accumulated
  # page/DOM snapshot history a long browser loop re-sends each iteration.
  observation_window: 3            # keep the N most recent browser/GUI observations at full fidelity; older ones are stubbed. 0 disables. Negative is rejected.
  max_recent_images: 50            # keep the N most recent image-bearing messages (all images); older screenshots become a placeholder. 0 disables (keep all). Negative is rejected.
  max_recent_browser_images: 1     # keep only the N most recent browser/GUI screenshots; user uploads + non-GUI images stay under max_recent_images. 0 disables. Negative is rejected.
```

## Tool Settings

```yaml
tools:
  bash_timeout: 120                # default per-call timeout, seconds (default: 120)
  bash_max_timeout: 600            # hard cap; per-call `timeout` arg above this is clamped and logged
  bash_max_output: 30000           # max chars in bash output (default: 30000)
  result_truncation: 30000         # max chars in tool result
  browser_result_truncation: 24000 # tighter per-observation cap for browser/GUI page/DOM dumps, with a truncation marker (0 = fall back to result_truncation)
  args_truncation: 200             # max chars in displayed args
  server_tool_timeout: 5           # gateway tool timeout, seconds
```

Raise `bash_max_timeout` for slow integration suites; the cap protects UI cards from appearing frozen before SIGKILL.

## Cloud Features

```yaml
cloud:
  enabled: false                   # required for publish_to_web, generate_image, edit_image, list_my_published_files, retract_published_file
  endpoint: ""                     # falls back to `endpoint` if empty
  api_key: ""                      # falls back to `api_key` if empty
  publish_allowed_extensions: []   # extend the default extension allowlist for publish_to_web

memory:
  provider: disabled               # disabled | cloud | local (default: disabled)
  endpoint: ""                     # falls back to cloud.endpoint
  api_key: ""                      # falls back to cloud.api_key
  socket_path: ""                  # default: $TMPDIR/com.kocoro.tlm.sock
  bundle_root: ""                  # default: $HOME/.shannon/memory
  tlm_path: ""                     # empty = PATH lookup; missing binary = silent disable
  bundle_pull_interval: 24h
  bundle_pull_startup_delay: 60s
  sidecar_ready_timeout: 15s
  sidecar_shutdown_grace: 5s
  sidecar_restart_max: 5
  client_request_timeout: 5s

sync:
  enabled: false                   # daily session JSON upload to Cloud
  dry_run: false                   # if true, write batches to ~/.shannon/sync_outbox/ instead of POSTing
  exclude_agents: []               # ["personal", "scratch", "default"]
  exclude_sources: []              # ["local", "cli", "slack"]; legacy sessions with no source treated as "local"
  batch_max_sessions: 25
  batch_max_bytes: 5242880         # 5 MiB
  single_session_max_bytes: 4194304  # 4 MiB; sessions over this are flagged in the marker, not uploaded
  daemon_interval: 24h
  daemon_startup_delay: 60s
```

See `docs/session-sync-launchd.md` for running session sync via launchd when the daemon is off.

## Daemon

```yaml
daemon:
  auto_approve: false              # skip approval round-trip globally (permission engine still enforced)
  capabilities: []                 # opt-in protocol features advertised on the WS handshake
```

## Hooks

```yaml
hooks:
  PreToolUse:
    - matcher: "bash"
      command: ".shannon/hooks/check-bash.sh"
  PostToolUse:
    - matcher: "file_edit|file_write"
      command: ".shannon/hooks/post-edit.sh"
  SessionStart:
    - command: ".shannon/hooks/on-start.sh"
  Stop:
    - command: ".shannon/hooks/on-stop.sh"
```

Hook commands must use `./` prefix (relative) or absolute paths under `~/.shannon/`. Bare names and absolute paths outside `~/.shannon/` are rejected.

## Other

```yaml
auto_update_check: true            # check for updates on launch
```

## UI Settings (`~/.shannon/settings.json`)

Separate file from `config.yaml`:

```json
{
  "spinner_texts": [
    "Thinking deeply...",
    "Exploring possibilities...",
    "Connecting the dots..."
  ]
}
```

## Inspection

Run `/config` in the TUI to see the merged config with sources — which file each value came from.
