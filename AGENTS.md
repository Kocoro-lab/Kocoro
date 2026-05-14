# Kocoro — Project Guide

## What This Is

Go CLI tool (`shan`) — the runtime for Shannon AI agents. The primary production stack is **daemon + Kocoro Desktop + Shannon Cloud**: the daemon connects to Cloud via WebSocket, receives channel messages, runs the agent loop locally with full tool access, and streams results back. Kocoro also supports interactive TUI, one-shot CLI, MCP server mode, and local scheduled tasks.

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
    alwaysallow.go     # single SSE+WS entry point for "Always Allow" decisions: bash (tool-level per-agent for named agents, tool-level global for default agent, never-persisted for always-ask gates) and non-bash (tool-level per-agent)
    types.go           # daemon request/response types, disconnect, approval messages
    events.go          # EventBus ring buffer for daemon/SSE subscribers
    bus_handler.go     # EventHandler -> EventBus bridge
    multi_handler.go   # fan-out EventHandler wrapper
    scheduler.go       # schedule runner and unattended approval handler
    safeguard.go       # destructive config edit confirmation gates
    rules.go           # rules API handlers
    permissions.go     # system permission probes (+ darwin/other impls)
    pidfile.go         # single-instance PID lock
    project_init.go    # project scaffold API handler
    memory_audit.go    # memory audit adapter
    memory_fallback.go # session/MEMORY fallback when memory sidecar is not ready
    launchd_darwin.go  # daemon launchd plist generation / launchctl
    launchd_stub.go    # launchd stubs for non-darwin
    attachment.go      # remote attachment download -> file_ref pipeline
    session_cwd.go     # cloud-source scratch CWD allocator (ephemeral per-session tmp dir)
    readtracker_cache.go # per-session ReadTracker cache; entries released via SessionManager.OnSessionClose
    suggestion_handler.go # GET /suggestion + POST /accept, validateSuggestionRoute, atomic persist on accept
  agent/
    loop.go              # AgentLoop.Run() — core agentic loop
    tools.go             # Tool interface, ToolRegistry, filtering, schemas
    partition.go         # read-only batching, executeBatches
    spill.go             # per-result spill (>50K) and per-turn 200K aggregate cap (rune-counted)
    toolresult_budget.go # persisted query-time tool_result replacement state (Replacements + Seen)
    toolbudget.go        # schema-token budget and warm-set fingerprints
    timebasedcompact.go  # time-based tool_result clearing
    context_bloat.go     # buildContextBloatSuggestion: tool_result_bloat run-status nudges
    deferred.go          # deferred tool loading (tool_search)
    statecache.go        # state-aware tool result cache keyed by read/write state
    resultshape.go       # tree result shaping and stable change summaries
    microcompact.go      # Tier 2 semantic compaction for large native tool results
    delta.go             # DeltaProvider interface, TemporalDelta (date rollover)
    loopdetect.go        # stuck-loop detectors
    readtracker.go       # read-before-edit enforcement + same-range file_read dedup
    approval_cache.go    # per-turn approval caching
    normalize.go         # response normalization
    skill_discovery.go   # Per-turn small-model skill matching (discoverRelevantSkills)
    phase.go             # explicit turn phase tracker
    watchdog.go          # idle soft/hard timeout observer
    modelcontext.go      # model ID -> context window map
    preflight.go         # pre-first-call episodic context hook
    cachemetric.go       # per-run cache stats accumulator
    usage.go             # per-run usage aggregation
    warmset.go           # deferred schema warm-set tracking
    suggestion.go        # SUGGESTION_PROMPT, FilterSuggestion, ShouldGenerateSuggestion, GenerateSuggestion(/WithUsage)
    suggestion_state.go  # SuggestionState — per-session latest suggestion text + accepted timestamp
    forkedrequest.go     # BuildForkedRequest primitive + ForkOptions (byte-equality cache contract)
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
  cloudflow/
    dispatch.go        # gateway workflow runner / SSE bridge
    parse.go           # slash command parser for workflow routes
  heartbeat/
    heartbeat.go       # periodic per-agent heartbeat checks
  watcher/
    watcher.go         # per-agent file-system watcher
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
    register.go        # local + MCP + gateway tool registration; RegisterPublishTool, RegisterGenerateImageTool, RegisterEditImageTool
    schedule.go        # schedule_create/list/update/remove tools
    session_search.go  # session_search tool
    mcp_tool.go        # MCPTool adapter
    server.go          # ServerTool adapter (gateway tools)
    publish_to_web.go  # publish_to_web tool (path/extension guards + purpose validation; uses internal/uploads)
    generate_image.go  # generate_image tool (text-to-image via Cloud; arg validation + error classification; uses internal/images)
    edit_image.go      # edit_image tool (CDN URL prefix check on image_urls; 1–4 sources; uses internal/images)
  uploads/
    client.go          # POST /api/v1/uploads multipart streaming client (typed errors, retry/backoff). Reuses GatewayClient.HTTPClient().
  images/
    client.go          # POST /api/v1/images/{generations,edits} JSON client. `Generate` (text→image) and `Edit` (CDN URLs + prompt→image) share `doWithRetry` + `attempt` + `classifyError`. Typed sentinels, 3-attempt retry on ErrTransient. Disambiguates 502/500 sub-codes plus edits-only 400 invalid_image_url (ErrInvalidImageURL) and 413 source_too_large (ErrSourceTooLarge) so "fix-the-args" failures short-circuit (re-running same args wastes paid quota). Reuses GatewayClient.HTTPClient() (600s timeout meets API spec).
  skills/
    registry.go        # skill metadata
    loader.go          # skill loading
    api.go             # builtin install + HTTP DTOs
    marketplace.go     # marketplace install/uninstall
    provenance.go      # skill install-source tracking
    secrets.go         # per-skill secret storage
    activated.go       # active-skill context for secret injection
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
    runstatus.go       # user-facing run state/error classification (Code constants, friendlyMessages, FriendlyMessageFromError)
    parse.go           # gateway *client.APIError → (Code, Detail) extractor; disambiguates the four 429 sub-shapes
  schedule/
    schedule.go        # schedule CRUD, atomic writes, validation
                       # launchd support lives with daemon runtime
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
The `kocoro` bundled skill (`internal/skills/bundled/skills/kocoro/`) is a platform configuration assistant. Its SKILL.md and reference files (`references/*.md`) describe daemon API endpoints, config fields, and workflows. Kocoro is the AI's only source of truth for the daemon HTTP surface — missing docs cause it to hallucinate workarounds. **Adding a new endpoint or feature counts as a trigger, not only modifying existing ones**; any `mux.HandleFunc(...)` in `internal/daemon/server.go` must have a matching reference entry. See CLAUDE.md for the full mapping.

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
- WS handshake advertises daemon version (`X-Kocoro-Daemon-Version`) and an opt-in capability list (`X-Kocoro-Capabilities`) on the HTTP upgrade. Cloud gates optional protocol features on capability presence so older daemons aren't subjected to flows they can't honor. Add a capability token in the same PR that ships the feature. Pre-v0.1.8 daemons send legacy `X-ShanClaw-*` variants; Cloud accepts both for one release during the Round-2 protocol rename.
- `delivery_ack` capability: after `SendReply` succeeds, daemon emits a `delivery_ack` envelope with the inbound `MessageID` so Cloud can drop the message from its replay buffer. Un-acked messages (crash, network drop pre-reply, ctx cancel) are replayed on reconnect.
- Route keys are computed as (precedence: top wins):
  - `session:<id>` for explicit session resume
  - `default:<source>:<thread>` (or `agent:<name>:<source>:<thread>`) for messaging platforms with a thread
  - `default:<source>:<channel>:<sender>` (or `agent:<name>:<source>:<channel>:<sender>`) for messaging platforms without a thread but with a sender — splits per-user so concurrent senders in a shared channel don't collide on one session
  - `agent:<name>` for agent-scoped sessions
  - `default:<source>:<channel>` legacy fallback (no thread, no sender)
- Routed managers are long-lived. Ephemeral runs (for example bypass/heartbeat paths) use short-lived managers.
- Output formatting uses profiles, not per-channel syntax:
  - `markdown` is the default
  - `plain` is used for cloud-distributed channels where Shannon Cloud owns final rendering
- Tool status `running` is emitted at actual execution start, not during approval/permission checks.
- **Tool result sizing**: three layered caps protect the context window.
  - **Per-result spill**: any single tool result over its policy threshold (default 50K runes; `grep` ~20K; `file_read` is unlimited at the budget layer and falls back to the 50K spill threshold) is written to `~/.shannon/tmp/tool_result_<session>_<call_id>.txt` and replaced inline with a 2K rune preview plus the file path. Cleaned up per-run in daemon/TUI, on manager close in one-shot mode.
  - **Per-turn 200K aggregate cap**: when the SUM of all parallel tool results in a turn exceeds 200K runes, the largest results are spilled until the total drops back under the cap.
  - **Persisted budget state**: query-time replacement bookkeeping (`ToolResultReplacements` + `ToolResultSeen` on the session) survives across turns. Mid-turn checkpoints persist via `applyTurnState`; final and hard-error save paths copy the maps explicitly so fast turns and crashed turns also retain dedup state.
  - **Bloat nudge**: `OnRunStatus("tool_result_bloat", …)` is emitted when a single tool's per-turn output exceeds the bloat threshold; surfaces to SSE/Desktop subscribers without forcing compaction.
- **file_read dedup**: `internal/agent/readtracker.go` records `(path, offset, limit, mtime, size)` per successful read. The daemon owns one tracker per session via `internal/daemon/readtracker_cache.go`, registered through `SessionManager.OnSessionClose` so per-session state is released on session switch, manager close, and explicit delete.
- **Image size guard** (`internal/tools/imaging_compress.go` + `internal/agent/oversize_image.go`): three layered defenses for Anthropic's per-image 5 MB inline limit.
  - **Source-time compression**: `EncodeImage` / `EncodeImageBytes` decode any image whose raw bytes exceed `TargetRawImageBytes` (3.75 MB → 5 MB base64), resize to ≤ 2000×2000, JPEG quality ladder `[80,60,40,20]`, hard fallback 1000×1000 q=20. Covers `file_read`, `screenshot`, PDF page render, `computer`, `browser`, `accessibility`, `applescript`, AND `resolveFileRef` (Desktop drag-drop attachments via `internal/daemon/runner.go`).
  - **Wire-time sanitizer** (`filterOversizeImages` in `internal/agent/oversize_image.go`): runs inside `(*AgentLoop).messagesForLLM` so every `CompletionRequest` builder (main loop, retry, force-stop synthesis) gets sanitized output. Replaces any image whose `Source.Data` length exceeds `client.MaxInlineImageBase64Bytes` with a text placeholder.
  - **Persist-time guard** (`(*AgentLoop).SanitizedRunMessages`): used by `applyTurnMessages` in `internal/daemon/runner.go` so a 400-causing image never lands in `~/.shannon/sessions/*.json`.
- **Session sync** (`internal/sync/`): uploads local session JSON to Shannon Cloud once per day (opt-in via `sync.enabled`). Single entry point `sync.Run`; called from the daemon ticker and the `shan sessions sync` CLI; flock + atomic marker write serialize concurrent callers. Per-session ACK with persistent `marker.failed` bookkeeping; permanent reasons (`size_limit_exceeded`, `load_error`) stay forever and self-heal on session edit.
- **Memory client** (`internal/memory/`, Phase 2.3): daemon owns sidecar lifecycle (spawn / health / restart / shutdown) and the 24h bundle pull loop. Tool `memory_recall` (`internal/tools/memory.go`) delegates to `memory.Service.Query` via UDS; falls back to `session_search` + MEMORY.md whenever `Service.Status() != Ready`. CLI/TUI use `memory.AttachPolicy` (probe-only, never spawn) and connect via `memory.NewServiceAttached`. Privacy invariant: the resolved API key bytes never reach disk or audit logs (only `sha256[:16]` fingerprint in `<bundle_root>/.tenant_fingerprint`).
- **Implicit episodic preflight** (`internal/agent/preflight.go` + `internal/tools/memory_preflight.go`): before the first main-model call on a memory-relevant turn, `agent.MemoryPreflightFunc` (wired via `tools.NewMemoryPreflight`) runs a small-tier helper that compiles `memory.QueryIntent`s via forced `tool_use` (`compile_memory_intents`), the sidecar resolves them, and a `<private_memory>` block is injected into the in-flight user message via `injectPrivateMemoryContext`. The block is never persisted to the session transcript, never replayed, and stripped from compaction summary inputs at every `GenerateSummary` call site via `stripPrivateMemoryForSummary`. User-derived body content runs through `prompt.SanitizeUserBlock` before the envelope is wrapped (defense in depth — same pattern as `<user_instructions>`). Audit event `memory_preflight` records a content-free trace (`attempted` / `helper_used` / `intents_count` / `results_count` / `context_injected` / `outcome` / `error_class` / `http_status`); query text, anchors, relation labels, and recalled content are never logged.
- **Prompt suggestion** (`internal/agent/suggestion.go`): forked LLM call after each main turn produces a 2-12 word ghost-text suggestion. **CACHE SAFETY INVARIANT**: the forked `CompletionRequest` is byte-equal to the main turn except for two appended messages (just-completed assistant reply + SuggestionPrompt) + `SkipCacheWrite: true` + `ForkedKind` (debug-only, `json:"-"`). Any other field divergence (`thinking.budget_tokens`, `max_tokens`, `tools` order) fragments the Anthropic prompt cache. Per-session state in `internal/agent/suggestion_state.go` (text + accepted-at + generation counter); lifecycle hooked from `internal/daemon/runner.go:fireSuggestionAfterRun` (post-`RunAgent` success, fire-and-forget). Gated by `agent.prompt_suggestion.enabled` + `cache_cold_threshold_tokens` + `min_turns` + last-turn error state via `ShouldGenerateSuggestion`. HTTP API: `GET /agents/{name}/sessions/{id}/suggestion` (or `/sessions/{id}/...` for default agent) + `POST` /accept. Accept returns the suggestion text for Desktop to fill the input — user still presses Enter, normal POST /message handles persistence.

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
- **Disk spill + aggregate cap**: see "Tool result sizing" above — single results over policy spill to disk; the per-turn 200K aggregate cap clamps the worst case across parallel tools.

### Anti-Hallucination

- XML `<tool_exec>` delimiters use random hex call IDs.
- Preamble text is suppressed when the response contains tool calls.
- Fabricated tool calls are detected and stripped.

## Testing

```bash
go test ./...                    # all tests
go test ./internal/agent/ -v     # agent loop, batching, compaction, spill, deferred
go test ./internal/daemon/ -v    # daemon WS client, router, runner, launchd
go test ./internal/agents/ -v    # agent loader
go test ./internal/schedule/ -v  # schedule CRUD
go test ./test/ -v               # E2E coverage
go test ./test/e2e/ -v           # E2E offline: agents, schedule, session, MCP, cache
SHANNON_E2E_LIVE=1 go test ./test/e2e/ -v  # E2E live: one-shot, bundled agents (daemon skipped until isolated)
go build ./...                   # build check
```

Schedule tests use temp directories and never write to the real LaunchAgents directory.

E2E tests in `test/e2e/` split into offline (no API) and live (`SHANNON_E2E_LIVE=1`). Run live tests before releases.

## Building & Releasing

- GoReleaser: `.goreleaser.yaml`
- npm package: `@kocoro/kocoro` (previously `@kocoro/shanclaw`, deprecated post-v0.1.7)
- Versioning is PATCH-only by default unless explicitly directed otherwise
- Release flow: tag → push tag → CI builds and publishes
- `docs/` is gitignored — documentation lives locally only

## Tool Inventory

### Core Local Tools

- File ops: `file_read` (auto-compresses image files > 3.75 MB raw via decode → resize ≤ 2000×2000 → JPEG quality ladder; output stays under Anthropic's 5 MB inline limit. See `internal/tools/imaging_compress.go`), `file_write`, `file_edit`, `glob`, `grep`, `directory_list`
- Archive: `archive_inspect` (read-only, no approval), `archive_extract` (requires approval) — supports `.zip / .tar / .tar.gz / .tgz`; atomic staging-dir + rename; rejects encrypted, symlink, absolute-path, setuid, device entries; caps 50 MB/entry, 200 MB total, 500 entries
- Documents: `pdf_to_text`, `docx_to_text`, `xlsx_to_text`, `pptx_to_text` — read-only convenience extractors. Each prefers an external tool (poppler `pdftotext`, `pandoc`, `xlsx2csv`) and falls back to unzip + raw-XML strip when that tool is missing; PDF has no fallback and suggests uploading the file for cloud's native document block. Fixed-argv exec (no shell), 60s timeout per call, output capped at 100K runes with a `[Truncated: ...]` marker. See `internal/tools/doc_extract.go`.
- Shell/system: `bash`, `system_info`, `process`, `http`, `think`. All approval-required tools (bash / file_read / file_write / file_edit / glob / grep / directory_list / http / browser / applescript / process / clipboard / computer / ghostty / notify / cloud_delegate / generate_image / edit_image / archive_extract / schedule_*) declare a required `description` field — a 5-15 word natural-language summary surfaced on approval cards instead of the raw args. Shared spec: `internal/agent/approval_description.go`; cross-tool enforcement test: `internal/tools/description_field_test.go`.
- macOS GUI: `accessibility`, `applescript`, `screenshot`, `computer`, `clipboard`, `notify`, `browser`, `wait_for`, `ghostty`
- Schedule: `schedule_create`, `schedule_list`, `schedule_update`, `schedule_remove`
- Memory: `memory_append`
- Skills: `use_skill`

### Runtime-Conditional Tools

- Session: `session_search` when a session manager is present
- Cloud: `cloud_delegate` when gateway/cloud access is enabled
- Cloud: `publish_to_web` when `cloud.enabled` AND `api_key` is configured. Always requires approval; `purpose` is mandatory. Path blocklist + extension allowlist are enforced client-side; details live with the tool and upload client.
- Cloud: `generate_image` when `cloud.enabled` AND `api_key` is configured. Always requires approval; returns a permanent public CDN URL and consumes paid quota. Server pins `gpt-image-2`; args are validated client-side; retry/error policy lives with the images client.
- Cloud: `edit_image` when `cloud.enabled` AND `api_key` is configured. Always requires approval; requires 1–4 `https://static.kocoro.ai/` source URLs and has no mask field. Shares the images client with `generate_image`.
- Meta: `tool_search` in deferred mode only
