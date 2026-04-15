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
    permissions.go     # hard-block > denied > split compounds > allowed > default safe > ask
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
```

## Key Conventions

### Agent Names

Must match `^[a-z0-9][a-z0-9_-]{0,63}$`. Validate before any path concatenation to prevent traversal.

### Tool Priority

Local tools > MCP tools > Gateway tools. Deduplicate by name in the registry.

### Permission Model

```
hard-block constants → denied_commands → compound-command splitting → allowed_commands → default safe → RequiresApproval + SafeChecker
```

Unknown tools are denied by default.

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
- Audit log: `~/.shannon/logs/audit.log`
- Schedule logs: `~/.shannon/logs/schedule-<id>.log`

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
