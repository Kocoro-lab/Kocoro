# ShanClaw — Project Guide

## What This Is

Go CLI tool (`shan`) — the runtime for Shannon AI agents. The primary production stack is **daemon + ShanClaw Desktop + Shannon Cloud**: the daemon connects to Cloud via WebSocket, receives channel messages, runs the agent loop locally with full tool access, and streams results back. ShanClaw also supports interactive TUI, one-shot CLI, MCP server mode, and local scheduled tasks.

## Tech Stack

- **Go 1.25.7** — `go.mod` is source of truth
- **Cobra** — CLI framework
- **gorilla/websocket** — daemon WebSocket client (primary production path)
- **Bubbletea v1.3.10 + Bubbles v1.0.0** — TUI
- **modernc.org/sqlite** — pure-Go SQLite for session FTS5 search index
- **adhocore/gronx** — cron expression validation
- **chromedp** — browser automation (isolated Chrome profile)
- **mcp-go** — MCP client/server
- **adrg/frontmatter** — YAML frontmatter parsing for SKILL.md files

## Project Structure

```
cmd/
  root.go              # entry, one-shot mode, MCP serve
  daemon.go            # shan daemon start/stop/status
  schedule.go          # scheduled task management
  update.go            # self-update command

internal/
  daemon/              # PRIMARY PRODUCTION PATH
    server.go          # HTTP API server
    runner.go          # agent run orchestration, session routing, output format profiles
    client.go          # WebSocket client with reconnect, bounded concurrency
    router.go          # SessionCache, route locking
    approval.go        # interactive tool approval over WS
    types.go           # daemon request/response types, disconnect, approval messages
    events.go          # EventBus ring buffer for daemon/SSE subscribers
    session_cwd.go     # cloud-source scratch CWD allocator (ephemeral per-session tmp dir)
  agent/
    loop.go            # AgentLoop.Run() — core agentic loop
    tools.go           # Tool interface, ToolRegistry, filtering, schemas
    partition.go       # read-only batching, executeBatches
    spill.go           # large tool result spill-to-disk
    deferred.go        # deferred tool loading (tool_search)
    statecache.go      # state-aware tool result cache keyed by read/write state
    resultshape.go     # tree result shaping and stable change summaries
    microcompact.go    # Tier 2 semantic compaction for large native tool results
    delta.go           # DeltaProvider interface, TemporalDelta (date rollover)
    loopdetect.go      # stuck-loop detectors
    readtracker.go     # read-before-edit enforcement
    approval_cache.go  # per-turn approval caching
    normalize.go       # response normalization
    skill_discovery.go # Per-turn small-model skill matching (discoverRelevantSkills)
  agents/
    loader.go          # LoadAgent, ListAgents, ParseAgentMention
    api.go             # daemon-side agent CRUD
    validate.go        # agent validation and builtin commands
    embed.go           # EnsureBuiltins, MaterializeBuiltin, bundled agents
    builtin/           # Bundled agent definitions (explorer, reviewer)
  client/
    gateway.go         # GatewayClient: Complete, CompleteStream, ListTools
    sse.go             # SSE event parsing
    ollama.go          # Ollama provider via OpenAI-compatible chat/tool APIs
  config/
    config.go          # multi-level config loading and merge
    settings.go        # UI settings
    setup.go           # setup wizard
  cwdctx/
    cwdctx.go          # session-scoped CWD: context propagation, path resolution
  context/
    window.go          # token estimation, compaction shaping
    summarize.go       # two-phase conversation summary generation
    persist.go         # write-before-compact memory extraction
  session/
    store.go           # session JSON persistence
    manager.go         # session lifecycle, search, OnClose callbacks
    index.go           # SQLite FTS5 search index
    title.go           # session title generation
  prompt/
    builder.go         # static/stable/volatile prompt assembly, output profiles
  instructions/
    loader.go          # instructions, memory, custom commands
  tools/
    register.go        # local + MCP + gateway tool registration
    schedule.go        # schedule_create/list/update/remove tools
    session_search.go  # session_search tool
    mcp_tool.go        # MCPTool adapter
    server.go          # ServerTool adapter (gateway tools)
  skills/
    registry.go        # skill metadata
    loader.go          # skill loading
    validate.go        # skill name validation
  memory/
    types.go             # Wire schemas mirroring the Kocoro Cloud memory sidecar HTTP contract
    errclass.go          # ErrorClass enum + ClassifyHTTP (sub_code → class)
    config.go            # LoadConfig, ResolveAPIKey, ResolveEndpoint
    tenant.go            # sha256 fingerprint + tenant-switch detection
    audit.go             # AuditLogger interface (boolean-only key/endpoint state)
    client.go            # UDS HTTP client (Query/Reload/Health, X-Request-ID, ctx-cancel dial)
    sidecar.go           # Spawn/WaitReady/Shutdown + AttachPolicy + Supervisor
    bundle.go            # Puller: manifest fetch, tenant check, sandboxed stage, atomic install, retention
    service.go           # Orchestrator: status FSM, supervisor + puller goroutines, NewServiceAttached
  mcp/
    client.go          # MCP client manager
    server.go          # MCP server
    chrome.go          # Playwright Chrome profile/CDP lifecycle management
  runstatus/
    runstatus.go       # user-facing run state/error classification
  schedule/
    schedule.go        # schedule CRUD, atomic writes, validation
    launchd_darwin.go  # plist generation, launchctl
    launchd_stub.go    # non-darwin stub
  permissions/
    permissions.go     # hard-block > denied > split compounds (incl. & and (...)) > always-ask (prefix + dangerous-flag) > allowed (literal/glob + token-prefix family) > default safe > ask
  audit/
    audit.go           # JSON-lines logger, redaction
  hooks/
    hooks.go           # PreToolUse/PostToolUse/SessionStart/Stop
  tui/
    app.go             # Bubbletea app
    doctor.go          # TUI diagnostic checks
    compact.go         # TUI /compact command
  update/
    selfupdate.go      # GitHub release auto-update
  sync/
    sync.go            # Run(ctx, deps) — flock, marker read, scan, batch, upload, write, audit
    marker.go          # Marker + FailedEntry; atomic read/write; sidecar on unknown version
    config.go          # Typed view of sync.* config keys
    scanner.go         # Multi-dir candidate discovery; failed-retry union; exclusions
    batcher.go         # Marshal-once; dual-cap (sessions+bytes) packing; oversized + load-error rejection
    uploader.go        # Uploader interface; CloudUploader + DryRunUploader; response anomaly normalization
    backoff.go         # Reason classification + transient backoff math
```

## Key Conventions

### Auto-installed Builtin Skills

Skills in `builtinSkills` (`internal/skills/api.go`) are content-addressed-overlaid from `embed.FS` to `~/.shannon/skills/<name>/` on every startup by `EnsureBuiltinSkills`; concurrent callers serialize on `~/.shannon/skills/.builtin.lock`. Two are currently shipped:

- `kocoro` — daemon HTTP API + config assistant (see below).
- `kocoro-generative-ui` — inline visualization assistant. Emits `html-artifact` fenced blocks that Kocoro Desktop renders in a sandboxed WKWebView; covers charts, diagrams, maps, SVG setup, and UI components. `hidden: true` (omitted from default `GET /skills` listings, still callable via `use_skill`).

User edits to either are wiped on next startup — fork under a different skill name to customize.

### Kocoro Skill Co-Maintenance
The `kocoro` bundled skill (`internal/skills/bundled/skills/kocoro/`) is a platform configuration assistant. Its SKILL.md and reference files (`references/*.md`) describe daemon API endpoints, config fields, and workflows. Kocoro is the AI's only source of truth for ShanClaw's HTTP surface — missing docs cause it to hallucinate workarounds. **Adding a new endpoint or feature counts as a trigger, not only modifying existing ones**; any `mux.HandleFunc(...)` in `internal/daemon/server.go` must have a matching reference entry. See CLAUDE.md for the full mapping.

### Agent Names

Must match `^[a-z0-9][a-z0-9_-]{0,63}$`. Validate before any path concatenation to prevent traversal.

### Tool Priority

Local tools > MCP tools > Gateway tools. Deduplicate by name in the registry.

### Skill Discovery

Three-layer system for triggering `use_skill`:
1. **Skill listing** — full descriptions embedded in the scaffolded user message on first turn.
2. **Semantic discovery** — blocking `model_tier: "small"` call on iteration 0 (5s timeout). Gated by `agent.skill_discovery` config (default `true`).
3. **Fallback catalog** — `use_skill` tool description includes all loaded skill names.

**Skill allowed-tools** uses execution-time denial, not schema filtering, to keep the tools array byte-stable for prompt cache.

### Permission Model

```
hard-block constants → denied_commands → compound-command splitting (incl. bare & and (...) subshells) → always-ask (alwaysAskPrefixes + git-push dangerous-flag/refspec scan) → allowed_commands (literal/glob + token-prefix family fallback) → default safe → RequiresApproval + SafeChecker
```

Unknown tools are denied by default. The always-ask gate runs BEFORE the allowlist, so high-risk commands (`python -c`, `bash -c`, `pip install`, destructive `git push` flags such as `--force*`/`--delete`/`--prune`/`--prune-tags`, `rm -rf`, trailing `&`, etc.) cannot be silenced via `allowed_commands` or token-prefix family expansion — the only way to disable the gate for a given prefix or flag is a code change.

### Daemon Architecture

- Daemon connects to Shannon Cloud via WebSocket, receives channel messages, and runs the agent loop locally.
- Route keys are computed as:
  - `agent:<name>` for agent-scoped sessions
  - `session:<id>` for explicit session resume
  - `default:<source>:<channel>` for routed channel sessions
- Routed managers are long-lived. Ephemeral runs (for example bypass/heartbeat paths) use short-lived managers.
- Output formatting uses profiles, not per-channel syntax:
  - `markdown` is the default
  - `plain` is used for cloud-distributed channels where Shannon Cloud owns final rendering
- Tool status `running` is emitted at actual execution start, not during approval/permission checks.
- Large tool results spill to `~/.shannon/tmp/` and are cleaned up:
  - per-run in daemon and TUI
  - on manager close in one-shot mode
- **Session sync** (`internal/sync/`): uploads local session JSON to Shannon Cloud once per day (opt-in via `sync.enabled`). Single entry point `sync.Run`; called from the daemon ticker and the `shan sessions sync` CLI; flock + atomic marker write serialize concurrent callers. Per-session ACK with persistent `marker.failed` bookkeeping; permanent reasons (`size_limit_exceeded`, `load_error`) stay forever and self-heal on session edit.
- **Memory client** (`internal/memory/`, Phase 2.3): daemon owns sidecar lifecycle (spawn / health / restart / shutdown) and the 24h bundle pull loop. Tool `memory_recall` (`internal/tools/memory.go`) delegates to `memory.Service.Query` via UDS; falls back to `session_search` + MEMORY.md whenever `Service.Status() != Ready`. CLI/TUI use `memory.AttachPolicy` (probe-only, never spawn) and connect via `memory.NewServiceAttached`. Privacy invariant: the resolved API key bytes never reach disk or audit logs (only `sha256[:16]` fingerprint in `<bundle_root>/.tenant_fingerprint`).

### Turn Lifecycle

The agent loop declares an explicit phase state machine (`internal/agent/phase.go`) that external observers can reason about:

- **Phases**: `PhaseAwaitingLLM`, `PhaseExecutingTools`, `PhaseCompacting`, `PhaseAwaitingApproval`, `PhaseRetryingLLM`, `PhaseForceStop`, `PhaseInjectingMessage`, etc. Only `PhaseAwaitingLLM` and `PhaseForceStop` count as idle for the watchdog.
- **Idle watchdog**: with `agent.idle_soft_timeout_secs` > 0 the daemon fires an `EventRunStatus` event (code `idle_soft`) after that long in an idle-counted phase. With `agent.idle_hard_timeout_secs` > 0 the run is cancelled with `ErrHardIdleTimeout` — the partial transcript is still persisted (soft error). Defaults: soft=90, hard=0 (visibility-only).
- **Mid-turn checkpoint**: after each tool batch, after successful reactive compaction, and before a force-stop, the daemon rebuilds the on-disk session from a baseline + current loop snapshot. The same rebuild runs at final save so a turn is never persisted twice. `session.Session.InProgress=true` on reload indicates a crash-recovered session with a partial transcript.
- **Event types**: `EventRunStatus` (watchdog soft/hard, LLM retries) joins the existing `EventAgentReply`, `EventToolStatus`, `EventApprovalRequest` stream.

### Browser Preview Bridge

For daemon runs with Playwright, `browser_navigate(file://…)` is transparently rewritten to a short-lived `http://127.0.0.1/<token>/<name>` URL served by a loopback HTTP server bound per-session:
- Fail-closed allowlist populated from the effective session CWD + user-attached paths. Browser reach never exceeds `permissions.CheckFilePath`.
- Symlinks resolved on both sides of the allowlist check; escapes rejected.
- Random 16-byte hex token per file; no directory listing; teardown on session close.

### Config Merge Order

1. `~/.shannon/config.yaml` (global)
2. `.shannon/config.yaml` (project)
3. `.shannon/config.local.yaml` (local, gitignored)

Scalars override, lists merge+dedup, structs merge field-by-field. MCP server env var casing is preserved via direct YAML re-read.

### File Paths

- Agent definitions: `~/.shannon/agents/<name>/AGENT.md`, `MEMORY.md`, `config.yaml`, `commands/*.md`, `_attached.yaml`
- Global skills: `~/.shannon/skills/<skill-name>/SKILL.md`
- Sessions: `~/.shannon/sessions/` or `~/.shannon/agents/<name>/sessions/`
- Session index: `<sessions-dir>/sessions.db`
- Spill files: `~/.shannon/tmp/tool_result_<session>_<call_id>.txt`
- Schedule index: `~/.shannon/schedules.json`
- Schedule plists: `~/Library/LaunchAgents/com.shannon.schedule.<id>.plist`
- Sync marker: `~/.shannon/sync_marker.json`
- Sync lock (flock): `~/.shannon/sync.lock` (never delete)
- Sync dry-run outbox: `~/.shannon/sync_outbox/` (only when `sync.dry_run=true`)
- Audit log: `~/.shannon/logs/audit.log`
- Schedule logs: `~/.shannon/logs/schedule-<id>.log`
- Memory sidecar socket: `~/.shannon/memory.sock`
- Memory bundle root: `~/.shannon/memory/` (with `bundles/<ts>/`, `current` symlink, `.tenant_fingerprint`, `bundle.lock`)

### Prompt Cache

Source-routed TTL: channels/TUI get 1h, one-shot/subagent paths get 5m (fail-cheap default). `cache_source` tags every LLM call and propagates on the wire. `normalizeToolInput` canonicalizes nested JSON for byte stability. See `docs/cache-strategy.md` for breakpoint layout, source table, env-var overrides, and maintenance playbook.

### Context Management

- **Proactive compaction**: persist learnings, then generate a two-phase summary, then shape history when nearing the context window.
- **Reactive compaction**: on context-length error, emergency compact once and retry once. `reactiveCompacted` prevents loops.
- **Tiered result compression**:
  - Tier 1: old results collapse to metadata only
  - Tier 2: mid-age results use head+tail truncation, with micro-compact for large native tool results when a small-model completer is available
  - Tier 3: recent results stay full
- **Deferred tool loading**: when the toolset is large and includes deferred sources, MCP/gateway tools are exposed as summaries until the model loads schemas through `tool_search`.
- **Memory staleness**: dated memory headings get freshness annotations like `[today]` and `[N days ago]`.
- **System reminders**: short reminder blocks are appended to high-signal tool results (`file_read`, `file_write`, `file_edit`, `bash`) to reinforce key instructions in long sessions.
- **Disk spill**: very large tool outputs are written to disk and replaced in context with a short preview plus a readable path.

### Anti-Hallucination

- XML `<tool_exec>` delimiters use random hex call IDs.
- Preamble text is suppressed when the response contains tool calls.
- Fabricated tool calls are detected and stripped.

## Testing

```bash
go test ./...                    # all tests
go test ./internal/agent/ -v     # agent loop, batching, compaction, spill, deferred
go test ./internal/daemon/ -v    # daemon WS client, router, runner
go test ./internal/agents/ -v    # agent loader
go test ./internal/schedule/ -v  # schedule CRUD + plist tests
go test ./test/ -v               # E2E coverage
go test ./test/e2e/ -v           # E2E offline: agents, schedule, session, MCP, cache
SHANNON_E2E_LIVE=1 go test ./test/e2e/ -v  # E2E live: one-shot, bundled agents (daemon skipped until isolated)
go build ./...                   # build check
```

Schedule tests use temp directories and never write to the real LaunchAgents directory.

E2E tests in `test/e2e/` split into offline (no API) and live (`SHANNON_E2E_LIVE=1`). Run live tests before releases.

## Building & Releasing

- GoReleaser: `.goreleaser.yaml`
- npm package: `@kocoro/shanclaw`
- Versioning is PATCH-only (`0.0.x`) unless explicitly directed otherwise
- Release flow: tag → push tag → CI builds and publishes
- `docs/` is gitignored — documentation lives locally only

## Tool Inventory

### Core Local Tools

- File ops: `file_read`, `file_write`, `file_edit`, `glob`, `grep`, `directory_list`
- Shell/system: `bash`, `system_info`, `process`, `http`, `think`
- macOS GUI: `accessibility`, `applescript`, `screenshot`, `computer`, `clipboard`, `notify`, `browser`, `wait_for`, `ghostty`
- Schedule: `schedule_create`, `schedule_list`, `schedule_update`, `schedule_remove`
- Memory: `memory_append`
- Skills: `use_skill`

### Runtime-Conditional Tools

- Session: `session_search` when a session manager is present
- Cloud: `cloud_delegate` when gateway/cloud access is enabled
- Meta: `tool_search` in deferred mode only
