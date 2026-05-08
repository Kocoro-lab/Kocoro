# ShanClaw — Project Guide

## What This Is

Go CLI tool (`shan`) — the runtime for Shannon AI agents. Production stack is **daemon + ShanClaw Desktop + Shannon Cloud**: the daemon connects to Cloud via WebSocket, receives channel messages (Slack, LINE, Feishu, Telegram, webhook), runs the agent loop locally with full tool access, and streams results back. Also supports interactive TUI, one-shot CLI, MCP server, and local scheduled tasks.

## Tech Stack

- **Go 1.25.7** — `go.mod` is source of truth
- **Cobra** — CLI framework (`cmd/root.go`, `cmd/daemon.go`, `cmd/schedule.go`)
- **gorilla/websocket** — daemon WebSocket client (primary production path)
- **Bubbletea v1.3.10 + Bubbles v1.0.0** — TUI (`internal/tui/app.go`)
- **adhocore/gronx** — cron expression validation
- **modernc.org/sqlite** — pure-Go SQLite for session FTS5 search index
- **chromedp** — browser automation (isolated Chrome profile)
- **mcp-go** — MCP client/server
- **adrg/frontmatter** — YAML frontmatter parsing for SKILL.md files

## Project Structure

```
cmd/
  root.go              # entry, --agent flag, one-shot, mcp serve
  daemon.go            # shan daemon start/stop/status
  schedule.go          # shan schedule create/list/update/remove/enable/disable/sync
  update.go            # /update command

internal/
  daemon/                # ── PRIMARY PRODUCTION PATH ──
    server.go          # HTTP API server (agent CRUD, config, instructions, session edit/retry/reset, reload)
    runner.go          # Agent run orchestration, session routing, output format profiles
    client.go          # WebSocket client with reconnect, bounded concurrency
    router.go          # SessionKey, SessionCache, route locking
    approval.go        # ApprovalBroker: interactive tool approval over WS
    types.go           # Shared daemon types (disconnect, approval_request/response/resolved)
    events.go          # EventBus ring buffer for daemon/SSE subscribers
    attachment.go      # Download remote file attachments (Slack/Feishu) → file_ref pipeline
    session_cwd.go     # Cloud-source scratch CWD allocator (ephemeral, per-session tmp dir)
    readtracker_cache.go # Per-session ReadTracker cache; entries released via SessionManager.OnSessionClose
  agent/
    loop.go              # AgentLoop.Run() — core agentic loop, SwitchAgent()
    tools.go             # Tool interface, ToolRegistry, FilterByAllow/Deny, Schemas()
    partition.go         # partitionToolCalls (read-only batching), executeBatches
    spill.go             # Per-result spill (>50K → temp file + preview) and per-turn 200K aggregate cap (rune-counted)
    toolresult_budget.go # Persisted query-time tool_result replacement state (Replacements + Seen) shared across turns
    context_bloat.go     # buildContextBloatSuggestion: surfaces "tool_result_bloat" run-status nudges
    deferred.go          # Deferred tool loading (tool_search schema merging)
    statecache.go        # state-aware tool result cache keyed by read/write state
    resultshape.go       # tree result shaping and stable change summaries
    microcompact.go      # Tier 2 semantic compaction for large native tool results
    delta.go             # DeltaProvider interface, TemporalDelta (date rollover detection)
    loopdetect.go        # 9 stuck-loop detectors (dupExempt for use_skill, IsError-aware dup, silent-below-15 for repeatable+result-only)
    readtracker.go       # read-before-edit enforcement + same-range file_read dedup
    approval_cache.go    # per-turn approval caching
    normalize.go         # response normalization
    skill_discovery.go   # Per-turn small-model skill matching (discoverRelevantSkills)
  agents/
    loader.go          # LoadAgent (config.yaml, commands/, _attached.yaml), ListAgents, ParseAgentMention
    api.go             # Agent CRUD operations for daemon API
    validate.go        # Agent name/field validation, BuiltinCommands
    embed.go           # EnsureBuiltins, MaterializeBuiltin, embed.FS bundled agents
    builtin/           # Bundled agent definitions (explorer, reviewer)
  client/
    gateway.go         # GatewayClient: Complete, CompleteStream, ListTools
    sse.go             # SSE event parsing
    ollama.go          # Ollama provider via OpenAI-compatible chat/tool APIs
  config/
    config.go          # Config struct, Load(), multi-level merge (global/project/local)
    settings.go        # UI settings
    setup.go           # --setup wizard
  cwdctx/
    cwdctx.go          # Session-scoped CWD: context propagation, path resolution helpers
  context/
    window.go          # EstimateTokens, ShouldCompact, ShapeHistory
    summarize.go       # GenerateSummary (two-phase: analysis scratchpad → summary)
    persist.go         # PersistLearnings: write-before-compact memory extraction
  schedule/
    schedule.go        # Schedule CRUD, atomic writes, file locking, validation
    launchd_darwin.go  # plist generation, launchctl (darwin only)
    launchd_stub.go    # no-op stub for non-darwin
  permissions/
    permissions.go     # bash resolution: hard-block > denied > split compounds > always-ask (prefix + dangerous-flag tokens) > allowed (literal/glob + token-prefix family) > default safe > ask
  audit/
    audit.go           # JSON-lines logger, RedactSecrets
  hooks/
    hooks.go           # PreToolUse/PostToolUse/SessionStart/Stop
  instructions/
    loader.go          # LoadInstructions, LoadMemory, LoadCustomCommands
  prompt/
    builder.go         # BuildSystemPrompt — PromptParts (static/stable/volatile), output format profiles
  session/
    store.go           # Session JSON persistence + SQLite index integration
    manager.go         # NewSession, Resume, Save, List, Search, Close, OnClose callbacks
    index.go           # SQLite FTS5 search index (sessions.db)
    title.go           # Session title generation helper
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
    client.go          # MCP client manager (stdio + HTTP transports)
    server.go          # MCP server (JSON-RPC 2.0 over stdio)
    chrome.go          # Playwright Chrome profile/CDP lifecycle management
  runstatus/
    runstatus.go       # user-facing run state/error classification (Code constants, friendlyMessages, FriendlyMessageFromError)
    parse.go           # gateway *client.APIError → (Code, Detail) extractor; parse429 disambiguates the four 429 sub-shapes (quota / credits / throttle / upstream)
  skills/
    registry.go        # Skill struct (Anthropic spec), SkillMeta DTO, SecretSpec, RequiredSecrets()
    loader.go          # LoadSkills from SKILL.md dirs (source-order merge; usually global > bundled)
    secrets.go         # SecretsStore: per-skill API key CRUD (Keychain via zalando/go-keyring) + plaintext index file
    activated.go       # ActivatedSet + context helpers for scoping secret injection per-run
    validate.go        # ValidateSkillName (Anthropic spec regex)
  tools/
    register.go        # RegisterLocalTools, RegisterAll, CompleteRegistration, ApplyToolFilter, RegisterPublishTool, RegisterGenerateImageTool, RegisterEditImageTool
    # Tool files: file_read, file_write, file_edit, glob, grep, bash,
    # directory_list, think, http, system_info, clipboard, notify, process,
    # applescript, accessibility, ghostty, browser, screenshot, computer,
    # wait (wait_for), cloud_delegate, publish_to_web, generate_image, edit_image,
    # imaging (helper), pinchtab (legacy), safe_path, skill (use_skill), memory_append
    schedule.go        # schedule_create/list/update/remove tools
    session_search.go  # session_search tool (FTS5 keyword search)
    mcp_tool.go        # MCPTool adapter
    server.go          # ServerTool adapter (gateway remote tools)
  uploads/
    client.go          # POST /api/v1/uploads multipart streaming client (typed errors + retry/backoff). Used by publish_to_web tool. Reuses GatewayClient.HTTPClient() — does not own its own *http.Client.
  images/
    client.go          # POST /api/v1/images/{generations,edits} JSON client (typed sentinel errors + 3-attempt retry on ErrTransient). `Generate` (text→image) and `Edit` (CDN URLs + prompt→image) share `doWithRetry` + `attempt` + `classifyError`. Disambiguates 502 sub-codes (upstream_error/no_images_returned/decode_failed/source_fetch_failed), 500 sub-codes (image_failed vs server_misconfigured), and edits-only sub-codes (400 invalid_image_url → ErrInvalidImageURL, 413 source_too_large → ErrSourceTooLarge) so retriable failures retry and "fix-the-args" failures (504, no_images_returned, invalid_image_url, source_too_large) short-circuit. Reuses GatewayClient.HTTPClient() (600s timeout meets API spec).
  tui/
    app.go             # Bubbletea Model — Init/Update/View, slash commands
    doctor.go          # TUI diagnostic checks
    compact.go         # TUI /compact command flow
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

### Doc Co-Maintenance
When a feature is added, refactored, or significantly changed, check and update all three doc files in the same change:
- **README.md** — user-facing: tool descriptions, TUI command tables, config options, daemon capabilities, setup instructions, permission engine
- **CLAUDE.md** — developer-facing: project structure tree, conventions, file paths, architecture notes
- **AGENTS.md** — external-agent-facing: overlaps with CLAUDE.md (structure tree, conventions, tool inventory). Keep in sync.

### Auto-installed Builtin Skills

Skills listed in `builtinSkills` (`internal/skills/api.go`) are synced from `embed.FS` to `~/.shannon/skills/<name>/` on every daemon/TUI/CLI startup via `EnsureBuiltinSkills`. The mechanism is content-addressed: a sha256 walk over the embed subtree is compared against the on-disk subtree, and any drift triggers a wipe-and-overlay (per-file `temp+rename`, dest-dir `RemoveAll` first to evict orphans). Concurrent callers serialize on `~/.shannon/skills/.builtin.lock`. User edits to a builtin SKILL.md are wiped on next startup — fork under a different skill name to customize. Current builtins:

- `kocoro` — daemon HTTP API + config assistant (see "Kocoro Skill Co-Maintenance" below).
- `kocoro-generative-ui` — inline visualization assistant. Teaches the LLM how to emit `html-artifact` fenced blocks that Kocoro Desktop renders in a sandboxed WKWebView. `hidden: true` (excluded from end-user `GET /skills` listings, still loadable via `use_skill`). Reference files cover charts, structural / illustrative diagrams, geographic maps, SVG setup, and UI components.

### Kocoro Skill Co-Maintenance
The `kocoro` bundled skill (`internal/skills/bundled/skills/kocoro/`) is a platform configuration assistant that teaches the AI how to manage ShanClaw via the daemon HTTP API. Its SKILL.md and 11 reference files (`references/*.md`) describe available API endpoints, config fields, and workflows. **Kocoro is the AI's only source of truth for ShanClaw's HTTP surface — if it doesn't know an endpoint exists, it will hallucinate a workaround (e.g., telling users to edit `.env` when the API handles secrets).**

**Mechanical check**: every `mux.HandleFunc(...)` line in `internal/daemon/server.go` must have a matching entry in some `references/*.md`. When adding a new endpoint or feature (not just modifying existing ones), update the corresponding kocoro reference file in the same PR:
- Daemon API endpoints — main resources AND sub-resources (`/skills/{name}/secrets`, `/skills/{name}/scripts`, `/agents/{name}/skills/*`, `/agents/{name}/commands/*`, `/skills/marketplace/*`) → `references/agents.md`, `references/skills.md`, `references/schedules.md`, `references/config.md`
- MCP server config schema or validation → `references/mcp.md`
- Permission model or safety gates → `references/permissions.md`
- Project init behavior → `references/project-init.md`
- Instructions/rules file structure → `references/instructions.md`
- Multi-step setup recipes → `references/recipes.md`
- Session sync behavior (`internal/sync/`, config keys under `sync.*`) → `references/session-sync.md`
- Memory client config or sidecar protocol (`internal/memory/`, config keys under `memory.*`) → `references/memory.md`
- Protected config fields or tool filter (`allowed-tools`) → `SKILL.md` security section

### Agent Names
Must match `^[a-z0-9][a-z0-9_-]{0,63}$`. Validated before any path concatenation to prevent traversal.

### Provider Architecture
`provider` config key selects the LLM backend: default (empty) uses `GatewayClient` via Shannon Cloud/Gateway; `ollama` uses `OllamaClient` via Ollama's OpenAI-compatible `/v1/chat/completions`. OllamaClient strips native Anthropic tool types (computer use) and handles thinking-model edge cases (Qwen3). Both implement the same `Complete`/`CompleteStream` interface.

### Tool Priority
Local tools > MCP tools > Gateway tools. Deduplication by name in registry.

### Skill Discovery
Three-layer system for triggering `use_skill`:
1. **Skill listing** — full descriptions (rune-safe truncated, 4000 char total budget) embedded in the scaffolded user message on first turn. Not in the system prompt (moved out for cache stability).
2. **Semantic discovery** — blocking `model_tier: "small"` call on iteration 0 (5s timeout). Matches user intent against loaded skill descriptions, injects `<system-reminder>` hint into the scaffolded user message. Gated by `agent.skill_discovery` config (default `true`).
3. **Fallback catalog** — `use_skill` tool description dynamically includes all loaded skill names.

**Skill allowed-tools enforcement** uses execution-time denial (`loop.go`), NOT schema filtering. The tools array stays full for the entire `Run()` to preserve Anthropic prompt cache stability. Blocked tools receive an error `ToolResult` with `[skill restriction]` message. A `<system-reminder>` hint is also appended to the `use_skill` result for soft guidance.

**Framework-level skill exemption** (`agent.SkillExempt` interface, opt-in): `think`, `tool_search`, `use_skill` always run regardless of the active skill's allowed-tools. Reserved for pure-infrastructure tools with zero I/O — restricting them would only force the model to substitute plan text into assistant messages or lock it out of skill switching. **Do NOT add `SkillExempt()` to tools with side effects** (file/network/publish/bash) — those must remain skill-restrictable so authors of confidential-context skills can lock them out.

### Permission Model
```
hard-block constants → denied_commands → compound-command splitting (incl. bare & and (...) subshells) → always-ask (alwaysAskPrefixes + git-push dangerous-flag/refspec scan) → allowed_commands (literal/glob + token-prefix family fallback) → default safe → RequiresApproval + SafeChecker
```
Unknown tools → denied by default (fail-safe). The always-ask gate runs BEFORE the allowlist, so adding a high-risk command to `allowed_commands` is a no-op; "Always Allow" on these is honored once and not persisted (`IsAlwaysAskPrefix` blocks `cmd/daemon.go` and `internal/daemon/server.go` from writing them to `permissions.allowed_commands`). Token-prefix family matching for the allowlist (depth N=2 for known CLIs like git/docker/npm, N=3 for unknowns) cannot widen scope past the always-ask gate.

### Daemon Architecture (Production Path)
- Daemon connects to Shannon Cloud via WebSocket, receives channel messages, runs agent loop locally.
- **WS handshake & capabilities**: on connect, daemon sends `User-Agent: shanclaw/<version> (<os>; <arch>)`, `X-ShanClaw-Daemon-Version: <semver>` (sourced from `cmd.Version` → `daemon.Version` at startup; `dev` for local builds), and — when `daemon.Capabilities` is non-empty — `X-ShanClaw-Capabilities: <token>,<token>` listing optional protocol features the daemon honors. Cloud reads these on the WS upgrade handler to gate features that would otherwise break older daemons. Headers are additive: empty / absent header = legacy mode. Add a capability token in the same PR that lands the feature it advertises — advertising before implementing causes Cloud to activate flows the daemon cannot satisfy.
- **`delivery_ack` capability** (advertised by default): after a `MsgTypeMessage` reaches a terminal state — `SendReply` succeeded, regardless of agent-loop outcome — the daemon emits a `MsgTypeDeliveryAck` envelope with the inbound `MessageID` (top-level, no payload). Cloud uses this to drop the message from its 5-min replay buffer; un-acked messages (daemon crashed mid-loop, network drop before reply, ctx cancel before terminal state) are replayed on the next reconnect. Reply-failure paths intentionally skip the ack — the user wasn't informed yet, so replay is correct. Implementation: `client.go:sendDeliveryAck` called immediately after `SendReply` in the success branch of `handleMessage`.
- **Session routing**: `SessionCache` with per-route locking. `ComputeRouteKey` precedence (top wins): `session:<id>` → messaging+thread `default:<source>:<thread>` (or `agent:<name>:<source>:<thread>`) → messaging+sender `default:<source>:<channel>:<sender>` (or `agent:<name>:<source>:<channel>:<sender>`, splits per-user when no thread is present so concurrent senders in a shared channel don't collide) → `agent:<name>` → `default:<source>:<channel>`. Web/webhook/cron/schedule sources bypass routing (always fresh). Routed managers are long-lived; ephemeral managers (heartbeat, bypass) get `defer Close()`.
- **Output format profiles**: `outputFormatForSource()` maps `req.Source` to `"markdown"` (default) or `"plain"` (cloud-distributed channels: slack, line, feishu, lark, telegram, webhook). Cloud owns final channel rendering — ShanClaw outputs neutral text for those paths.
- **Tool status events**: `OnToolCall("running")` fires at actual execution start (inside `executeBatches`, after semaphore acquire), not during permission checks.
- **Tool result sizing** (`internal/agent/spill.go` + `toolresult_budget.go` + `context_bloat.go`): three layered caps protect the context window.
  - **Per-result spill**: any single tool result over its `MaxResultSizeChars` policy (default 50K runes; `grep` ~20K; `file_read` is `UnlimitedToolResultSizeChars` and falls back to the 50K spill threshold) is written to `~/.shannon/tmp/tool_result_<session>_<call_id>.txt` and replaced inline with a 2K rune preview plus the file path. Cleaned up per-run (daemon/TUI) or on manager close (one-shot).
  - **Per-turn aggregate cap (200K runes)**: `applyAggregateCap` mirrors CC's `MAX_TOOL_RESULTS_PER_MESSAGE_CHARS`. When the SUM of all parallel tool results in a turn exceeds 200K, the largest results are spilled until the total drops back under the cap. Counted in runes via `utf8.RuneCountInString` so multibyte content (CJK/emoji) is measured fairly.
  - **Persisted budget state** (`ToolResultReplacements` + `ToolResultSeen` on `session.Session`): query-time replacement bookkeeping survives across turns and resumes. Saved by `applyTurnState` at mid-turn checkpoints AND by both terminal save paths (final save + hard-error save) — a fast turn that finishes before the first checkpoint must still persist these maps, otherwise dedup state is lost on resume.
  - **Bloat nudge**: `buildContextBloatSuggestion` emits `OnRunStatus("tool_result_bloat", …)` when a single tool's per-turn output exceeds the bloat threshold; surfaces to SSE/Desktop subscribers without forcing compaction.
- **file_read dedup**: `internal/agent/readtracker.go` records `(path, offset, limit, mtime, size)` on each successful read. Re-reading the same range returns a short "unchanged since last read" stub. The daemon owns one tracker per session via `internal/daemon/readtracker_cache.go`, registered through `SessionManager.OnSessionClose` so per-session state is released on session switch, manager close, and explicit delete.
- **Skill secrets**: `SecretsStore` (`internal/skills/secrets.go`) manages per-skill API keys. Values stored in **macOS Keychain** (encrypted; service = `com.shannon.skill.<name>`, account = env var name) via `zalando/go-keyring` (pure Go, no CGo; passes password via stdin not argv so `ps` cannot observe values). A plaintext index file `~/.shannon/secrets-index.json` tracks which key names are configured per skill so `ConfiguredKeys()` can answer without triggering Keychain access prompts. Skills declare required env vars via ClawHub metadata — three interchangeable parent aliases: `metadata.openclaw.requires.env` / `metadata.clawdbot.requires.env` / `metadata.clawdis.requires.env`. Daemon exposes `PUT/DELETE /skills/{name}/secrets` for CRUD (all three write handlers call `auditHTTPOp` with key names only, never values); `GET /skills` returns `required_secrets` + `configured_secrets` (values never exposed). **Runtime injection is env-var-only, scoped to active skills**: secrets never enter the prompt body or session transcript. `AgentLoop.Run` initializes a per-run `skills.ActivatedSet` in context; `use_skill` registers the skill name when invoked; `BashTool.Run` reads the set via context and fetches only those skills' secrets from `SecretsStore` on each invocation, injecting as child-process env vars. A skill loaded but never activated contributes no env vars to bash. Secrets cleaned up on skill deletion.
- **Turn phase tracker** (`internal/agent/phase.go`): explicit state machine for `AgentLoop.Run`. Every blocking boundary calls `tracker.Enter(phase)` or `tracker.EnterTransient(phase)()`. Only `PhaseAwaitingLLM` and `PhaseForceStop` are idle-counted (watched by the watchdog). Fail-closed: forgotten transient restore and `Enter`-inside-transient mark the tracker `invalid`; observers self-disable. Panics under `testing.Testing()` or `SHANNON_PHASE_STRICT=1`, logs otherwise.
- **Idle watchdog** (`internal/agent/watchdog.go`): observer goroutine. Fires `OnRunStatus("idle_soft", …)` after `agent.idle_soft_timeout_secs` (default 90) in an idle-counted phase. Cancels ctx with `ErrHardIdleTimeout` via `context.WithCancelCause` after `agent.idle_hard_timeout_secs` (default 0 = disabled; flip to 540 after dogfood). Dedups soft fire by tracker `seq`, re-arms on every phase transition. `completeWithRetry` prefers `context.Cause(ctx)` over `ctx.Err()`. `isSoftRunError` in the runner includes `ErrHardIdleTimeout` so the partial transcript is persisted (not replaced by a friendly error stub). Daemon emits `EventRunStatus` to SSE/Desktop subscribers via `daemonEventHandler.OnRunStatus`.
- **Mid-turn checkpoint**: `AgentLoop.SetCheckpointFunc(func(ctx) error)` fires at three phase-exit boundaries (after each `executeBatches`, after successful reactive compaction, before `runForceStopTurn`), gated by `tracker.TakeDirty()`. Agent-side `SetCheckpointMinInterval(2s)` debounce; failed save (callback returns error) leaves dirty set and skips the time stamp so the next fire retries. Runner uses `captureTurnBaseline` + `applyTurnMessages` + `applyTurnUsage` — the SAME helpers run from the normal final save AND the hard-error save so a turn is never persisted twice via different paths. The mid-turn checkpoint additionally calls `applyTurnState` (which persists `ToolResultReplacements` + `ToolResultSeen`); the final and hard-error save paths copy those two maps explicitly from `loop` so a fast turn that finishes before any checkpoint fires still ends up with the budget bookkeeping on disk. `session.Session.InProgress` is set mid-turn, cleared on final save; a non-zero flag on reload indicates a crash-recovered session with a partial transcript.
- **Playwright `file://` preview bridge** (`internal/tools/filepreview.go`): loopback HTTP server rewrites `browser_navigate(file://…)` → `http://127.0.0.1/<token>/<name>`. Fail-closed allowlist via `AllowRoot(dir)` / `AllowFile(path)`, both symlink-resolved via `filepath.EvalSymlinks`. Daemon populates per-run from effective CWD + user-attached paths so browser reach never exceeds `permissions.CheckFilePath`. Uses `http.ServeContent` (not `http.ServeFile`) to avoid the `index.html` internal redirect. Defense-in-depth: `r.RemoteAddr` loopback check in the handler.
- **Session sync** (`internal/sync/`): uploads local session JSON to Shannon Cloud once per day (opt-in via `sync.enabled`). Single entry point `sync.Run`; called from daemon ticker and `shan sessions sync` CLI; flock + atomic marker write serialize concurrent callers. Per-session ACK with persistent `marker.failed` bookkeeping; permanent reasons (`size_limit_exceeded`, `load_error`) stay forever and self-heal on session edit.
- **Memory client** (`internal/memory/`, Phase 2.3): daemon owns sidecar lifecycle (spawn / health / restart / shutdown) and the 24h bundle pull loop. Tool `memory_recall` (`internal/tools/memory.go`) delegates to `memory.Service.Query` via UDS; falls back to `session_search` + MEMORY.md whenever `Service.Status() != Ready`. CLI/TUI use `memory.AttachPolicy` (probe-only, never spawn) and connect via `memory.NewServiceAttached`. Privacy invariant: the resolved API key bytes never reach disk or audit logs (only `sha256[:16]` fingerprint in `<bundle_root>/.tenant_fingerprint`).
- **Loop detector** (`internal/agent/loopdetect.go`): 9 detectors trigger nudge or force-stop. **Threshold policy v2 (2026-04-22)**: defaults raised across the board for Claude 4.X self-recovery — `consecDupThreshold=3`, `exactDupThreshold=5`, `sameToolErrThreshold=6`, `noProgressThreshold=12`, `semiRepeatableThreshold=16`; FamilyNoProgress non-repeatable path 5/8/12 (was 3/5/7); SearchEscalation 7/12 (was 5/8); Sleep 2/4 (unchanged). Key tuning: `dupExemptTools` (currently `use_skill`) skip both ConsecutiveDup and ExactDup entirely (pure idempotent loaders); ConsecutiveDup tail-success skip + all-errors 2x budget lets fail/fail/success retries survive; ExactDup also skips the first success after a recent same-args error streak, even when retries are spread across intervening tool calls; ExactDup all-errors 2x budget mirrors ConsecutiveDup for spread-out retries; FamilyNoProgress fires force-stop-only at progressCount>=15 for repeatable tools when there is no prior same-topic collision beyond the current call itself (no intermediate nudge — prevents stacking with rolling-window escalation); nudge escalation in `loop.go` uses a rolling window (`nudgeWindow`: max 3 nudges within trailing 5 iterations) instead of a flat counter.

### Daemon Approval Protocol
- **Interactive mode** (default): Tools requiring approval send `approval_request` over WS → Cloud relays to Ptfrog → user responds → `approval_response` relayed back. Agent loop blocks until response.
- **Auto-approve mode** (`daemon.auto_approve: true` or per-agent `auto_approve: true`): Skips WS round-trip, permission engine still enforced.
- `ApprovalBroker` in `internal/daemon/approval.go` manages pending requests with context cancellation and WS disconnect cleanup.
- "Always Allow" for bash: persists command to `permissions.allowed_commands` via `config.AppendAllowedCommand`. Non-bash: in-memory only (session lifetime).
- HTTP API handlers auto-approve (localhost-only, local-trusted).

### Config Merge Order
1. `~/.shannon/config.yaml` (global)
2. `.shannon/config.yaml` (project)
3. `.shannon/config.local.yaml` (local, gitignored)

Scalars override, lists merge+dedup, structs field-level merge. MCP server env var casing preserved via direct YAML re-read.

### File Paths
- Agent definitions: `~/.shannon/agents/<name>/AGENT.md` + `MEMORY.md` + `config.yaml` + `commands/*.md` + `_attached.yaml`
- Global skills: `~/.shannon/skills/<skill-name>/SKILL.md` (shared across agents)
- Sessions: `~/.shannon/sessions/` (default) or `~/.shannon/agents/<name>/sessions/` (per-agent)
- Session index: `<sessions-dir>/sessions.db` (SQLite FTS5, auto-rebuilt from JSON if deleted)
- Spill files: `~/.shannon/tmp/tool_result_<session>_<call_id>.txt` (cleaned up per-run in daemon/TUI, on manager close in one-shot)
- Attachments: `~/.shannon/tmp/attachments/<nonce>/` (downloaded Slack/Feishu files, cleaned up on session close)
- Schedule index: `~/.shannon/schedules.json`
- Schedule plists: `~/Library/LaunchAgents/com.shannon.schedule.<id>.plist`
- Skill secrets index: `~/.shannon/secrets-index.json` (key names only, no values, chmod 600, flock-protected)
- Skill secret values: macOS Keychain (service `com.shannon.skill.<skill-name>`, account = env var name)
- Sync marker: `~/.shannon/sync_marker.json`
- Sync lock (flock): `~/.shannon/sync.lock` (never delete)
- Sync dry-run outbox: `~/.shannon/sync_outbox/` (only when `sync.dry_run=true`)
- Audit log: `~/.shannon/logs/audit.log`
- Schedule logs: `~/.shannon/logs/schedule-<id>.log`
- Memory sidecar socket: `~/.shannon/memory.sock`
- Memory bundle root: `~/.shannon/memory/` (with `bundles/<ts>/`, `current` symlink, `.tenant_fingerprint`, `bundle.lock`)

### Atomic Writes
`schedules.json` and `secrets-index.json` use write-to-temp + `os.Rename` + `syscall.Flock` on a persistent `.lock` file. Never delete the lock file (causes flock race on different inodes).

### Build Tags
`internal/schedule/launchd_darwin.go` uses `//go:build darwin`. `launchd_stub.go` provides no-op stubs for non-darwin. Tests that touch launchctl go in `_darwin_test.go`.

### Prompt Cache
See `docs/cache-strategy.md` for the authoritative design (4-breakpoint allocation, source→TTL routing, byte stability, session_id propagation, env-var overrides). When investigating CER drops, see `docs/cache-debug.md` for the diagnostic instrumentation layer (env flags, log fields, drift patterns). One-line invariants:
- `cache_source` tags every LLM call; `_ttl_block(request)` routes 1h for channel/TUI, 5m for one-shot/subagent (fail cheap).
- `SHANNON_FORCE_TTL=off|5m|1h` overrides for operator debug / A-B.
- `SHANNON_CACHE_DEBUG=1` → JSON-lines log with hash ladders + per-tool / per-message / per-block hashes + compaction events; `SHANNON_CACHE_DEBUG_RAW=1` adds full request bytes per call (LRU 100 dirs, override `SHANNON_CACHE_DEBUG_RAW_MAX`).
- `normalizeToolInput` in `gateway.go` canonicalizes nested JSON key ordering so cross-turn `system_h` / tool_use-input stays byte-stable.
- **Skill allowed-tools** uses execution-time denial (not schema filtering) to keep `toolSchemas` byte-stable. Previous `applySkillFilter` shrank the tools array after `use_skill`, causing ~$0.10 cache rebuild per activation; now tools stay full ($0.02).
- **Skill listing** is embedded in the scaffolded user message (not system prompt) so different skill sets don't invalidate the system prefix cache.
- All in-place `messages[idx].Content` rewrites in the agent loop call `client.LogCacheCompactEvent` so cache-debug.log explains every prefix-byte drift. New mutation paths must wire this — uninstrumented rewrites break drift attribution silently.

### Context Management
- **Proactive compaction**: `PersistLearnings` → `GenerateSummary` (two-phase: `<analysis>` scratchpad → `<summary>`) → `ShapeHistory` at 85% context window.
- **Reactive compaction**: On context-length error, emergency compress + single retry. `reactiveCompacted` flag prevents loops.
- **Tiered result compression**: Tier 1 (>10 msg old) → metadata only. Tier 2 (3–10) → head+tail truncation. Tier 3 (0–2) → full.
- **Memory staleness**: `annotateStaleness()` appends `[N days ago]` to memory headings.
- **Deferred tool loading**: When tool count > 30, MCP/gateway tools sent as name+description only. Model calls `tool_search` to load full schemas on demand.
- **System reminders**: Short `<system-reminder>` hints appended to `file_read`, `file_write`, `file_edit`, `bash` results. Reinforces key instructions in long sessions. Skipped for `cloud_delegate` (user-visible output).

### Anti-Hallucination
XML `<tool_exec>` delimiters in conversation context with random hex call_id. Preamble text suppressed when response has tool calls. Fabricated tool calls detected and stripped.

## Testing

```bash
go test ./...                              # all tests
go test ./internal/daemon/ -v              # daemon: WS client, router, E2E routing
go test ./internal/agent/ -v               # agent loop, partitioning, spill, deferred
go test ./internal/agents/ -v              # agent loader
go test ./internal/schedule/ -v            # schedule CRUD + plist tests
go test ./test/ -v                         # E2E: vision pipeline, persist learnings
go test ./test/e2e/ -v                     # E2E offline: agents, schedule, session, MCP, cache
SHANNON_E2E_LIVE=1 go test ./test/e2e/ -v  # E2E live: one-shot, bundled agents (daemon tests skipped until --port/--home isolation)
go build ./...                             # build check
```

Schedule tests use `t.TempDir()` as `plistDir` — they never write to real `~/Library/LaunchAgents/`.

E2E tests in `test/e2e/` are split into offline (no API, runs in CI) and live (needs `SHANNON_E2E_LIVE=1` + configured endpoint). Run live tests before each release.

## Building & Releasing

- GoReleaser: `.goreleaser.yaml`
- npm: `@kocoro/shanclaw` → `npm install -g @kocoro/shanclaw`
- **Versioning: PATCH-only by default** — do NOT bump minor/major unless explicitly asked
- Release: `git tag -a vX.Y.Z` → `git push origin vX.Y.Z` → CI builds + publishes
- `docs/` is gitignored — documentation lives locally only

## Local Tools (26 base + conditional)

**File ops:** file_read, file_write, file_edit, glob, grep, directory_list
**Shell/system:** bash, system_info, process, http, think
**macOS GUI:** accessibility (primary), applescript, screenshot, computer, clipboard, notify, browser, wait_for, ghostty
**Schedule:** schedule_create, schedule_list, schedule_update, schedule_remove
**Memory:** memory_append (flock-protected append to MEMORY.md)
**Skills:** use_skill

**Conditional (registered outside `RegisterLocalTools`):**
- session_search — added when a session manager is available
- cloud_delegate — added when `cloud.enabled: true`
- publish_to_web — added when `cloud.enabled: true` AND `cfg.APIKey != ""`. Lives in `internal/tools/publish_to_web.go`; HTTP plumbing in `internal/uploads/client.go` (multipart streaming via `io.Pipe`, 3-attempt retry on `ErrTransient`, sentinel errors for 401/400/413/500-s3_unconfigured/transient). Always requires approval (`RequiresApproval=true`, `IsSafeArgs=false`). Tool-side guards: required `purpose` arg shown to user during approval; path-segment blocklist (`.env`/`.ssh`/`credentials`/…); basename suffix blocklist (`.pem`/`.key`/…); extension allowlist (default html/md/txt/pdf/png/jpg/svg/csv/json/mp4/…). Allowlist extensible via `cloud.publish_allowed_extensions: [".go", ...]`; **denylist is not user-configurable by design**. Registered alongside `cloud_delegate` at all 5 call sites (`cmd/daemon.go`, `cmd/root.go`, `internal/tui/app.go`, `internal/daemon/server.go` reload paths).
- generate_image — added when `cloud.enabled: true` AND `cfg.APIKey != ""`. Lives in `internal/tools/generate_image.go`; HTTP plumbing in `internal/images/client.go` (POST JSON to `/api/v1/images/generations`, 3-attempt retry on `ErrTransient`, sentinel-typed errors). Always requires approval (`RequiresApproval=true`, `IsSafeArgs=false`) — output is a permanent public CDN URL plus paid quota consumption. Reuses `GatewayClient.HTTPClient()` (600s timeout meets the API spec ≥600s requirement). Args: `prompt` (1–32000 chars, required), `size`/`quality`/`n`/`background` (enum-validated client-side). **Never sends a `model` field** — server pins `gpt-image-2`. Error policy: 504 `upstream_timeout` and 502 `no_images_returned` are `BusinessError` (not retried — re-running same args wastes paid quota); 502 `upstream_error`/`decode_failed`, 500 `image_failed`, 503, network → `Transient`. Registered alongside `cloud_delegate` / `publish_to_web` at all 5 call sites.
- edit_image — added when `cloud.enabled: true` AND `cfg.APIKey != ""`. Lives in `internal/tools/edit_image.go`; shares `internal/images/client.go` with `generate_image` (`Edit` method posts to `/api/v1/images/edits`; success schema reuses `GenerateResponse`). Always requires approval (paid + permanent public URL). Args: `prompt` (1–32000 chars), `image_urls` (**required, 1–4 entries**, every entry must start with `https://static.kocoro.ai/` — pre-validated client-side via `kocoroCDNPrefix` constant in `edit_image.go` to avoid wasted round-trips), plus the same `size`/`quality`/`n`/`background` enums as `generate_image`. **Never sends a `model` field**, **no mask field** (region described in natural language). New sentinels: `ErrInvalidImageURL` (400 invalid_image_url → `BusinessError`, "rebuild the URL pipeline" — server may also reject for non-prefix reasons), `ErrSourceTooLarge` (413 source_too_large → `ValidationError`, source > 25 MiB OpenAI cap). 502 `source_fetch_failed` falls into the existing `ErrTransient` default branch (3-attempt retry covers spec's "可重试 1 次"). Latency reaches 200–350s for 4 sources at `quality=high`. Registered alongside `generate_image` at all 5 call sites.
- tool_search — added in deferred mode when tool count > 30 (lives in `internal/agent/deferred.go`, not `tools/`)
