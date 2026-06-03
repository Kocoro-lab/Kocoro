# Kocoro — Project Guide

## What This Is

Go CLI tool (`shan`) — the runtime for Shannon AI agents. Production stack is **daemon + Kocoro Desktop + Shannon Cloud**: the daemon connects to Cloud via WebSocket, receives channel messages (Slack, LINE, Feishu, Telegram, webhook), runs the agent loop locally with full tool access, and streams results back. Also supports interactive TUI, one-shot CLI, MCP server, and local scheduled tasks.

## Tech Stack

- **Go 1.25.7** — `go.mod` is source of truth
- **Cobra** — CLI (`cmd/`)
- **gorilla/websocket** — daemon WS client (primary production path)
- **Bubbletea + Bubbles** — TUI (`internal/tui/`)
- **adhocore/gronx** — cron validation
- **modernc.org/sqlite** — pure-Go SQLite, FTS5 session index
- **chromedp** — browser automation (isolated profile)
- **mcp-go** — MCP client/server
- **adrg/frontmatter** — YAML frontmatter for SKILL.md

## Project Structure

```
cmd/
  root.go              # entry, --agent, one-shot, mcp serve
  daemon.go            # shan daemon start/stop/status
  schedule.go          # shan schedule CRUD
  update.go            # /update command

internal/
  daemon/                              # ── PRIMARY PRODUCTION PATH ──
    server.go          # HTTP API (agents, config, sessions, reload)
    runner.go          # Run orchestration, session routing, output profiles
    client.go          # WS client (reconnect, bounded concurrency)
    router.go          # SessionKey + SessionCache + route locking
    approval.go        # ApprovalBroker (WS approval round-trip)
    alwaysallow.go     # Always-allow persistence dispatcher (SSE+WS)
    types.go           # Shared wire types
    events.go          # EventBus ring buffer
    bus_handler.go     # EventHandler → EventBus bridge
    multi_handler.go   # Fan-out EventHandler wrapper
    scheduler.go       # Cron tick + scheduleHandler
    safeguard.go       # ?confirm=true gate for destructive edits
    rules.go           # /rules HTTP handlers
    pidfile.go         # Single-instance flock
    permissions.go     # System permission probes (see permissions_{darwin,other}.go)
    project_init.go    # /project/init handler
    memory_audit.go    # memory.AuditLogger → audit.AuditLogger bridge
    memory_fallback.go # session_search + MEMORY.md fallback
    launchd_darwin.go  # plist + launchctl (darwin only; stub elsewhere)
    attachment.go      # Remote file attachments (b64 / extracted / URL)
    session_cwd.go     # Cloud-source scratch CWD allocator
    readtracker_cache.go # Per-session ReadTracker cache
    suggestion_handler.go # GET /suggestion + POST /accept
    uploads_handler.go # /uploads GET (list) + DELETE (retract) proxies
    auth.go            # AuthManager state machine (email/password, macOS-only)
    auth_handlers.go   # /local/auth/* handlers (state, register, login, sign-out, …)
    ws_controller.go   # WS goroutine start/stop driven by AuthManager
    desktop_rpc/       # Unix sock reverse-RPC channel to Kocoro Desktop (Calendar RPC v1)
      types.go         #   Frame / RPCRequest/Result/Error / DesktopEvent + §5.5 string constants
      codec.go         #   length-prefixed JSON framing (4-byte BE uint32, ≤ 4 MiB body)
      broker.go        #   DesktopRPCBroker (Request / Resolve / CancelAll; mirrors ApprovalBroker)
      listener.go      #   Unix sock listen + accept loop + bidirectional method dispatch + system.ping/capabilities responders
  agent/
    loop.go              # AgentLoop.Run(), SwitchAgent()
    tools.go             # Tool interface, ToolRegistry
    partition.go         # Read-only batching, executeBatches
    spill.go             # Per-result spill + 200K per-turn aggregate cap
    toolresult_budget.go # Persisted tool_result replacement state
    toolbudget.go        # Schema-token budget, deferred set
    timebasedcompact.go  # Time-based tool_result clearing
    context_bloat.go     # tool_result_bloat nudge builder
    deferred.go          # Deferred tool loading (tool_search)
    statecache.go        # Tool-result cache keyed by read/write state
    resultshape.go       # Tree result shaping
    microcompact.go      # Tier 2 semantic compaction
    delta.go             # DeltaProvider, TemporalDelta
    loopdetect.go        # 9 stuck-loop detectors
    readtracker.go       # Read-before-edit + same-range dedup
    approval_cache.go    # Per-turn approval cache
    normalize.go         # Response normalization
    skill_discovery.go   # Per-turn small-model skill matching
    phase.go             # Turn phase tracker (fail-closed)
    watchdog.go          # Idle soft/hard timeout
    modelcontext.go      # Model-ID → context-window map
    preflight.go         # MemoryPreflightFunc hook
    cachemetric.go       # CacheTracker per-Run stats
    usage.go             # Per-Run usage aggregation
    warmset.go           # Deferred-schema warm set
    suggestion.go        # Forked suggestion call (cache-safe)
    suggestion_state.go  # Per-session suggestion text/state
    forkedrequest.go     # BuildForkedRequest (byte-equality contract)
  agents/                # AGENT.md loader, CRUD, validate, embed.FS builtins
  keychain/              # macOS Keychain wrapper (Backend interface + osBackend/memBackend); api_key source of truth
  client/                # GatewayClient (Anthropic via Cloud), OllamaClient, SSE, AuthClient (/auth/*)
  cloudflow/             # /research, /swarm Gateway workflow runner
  heartbeat/             # Per-agent HEARTBEAT.md + alerts
  watcher/               # Per-agent debounced FS watcher
  config/                # Config struct, multi-level merge, --setup wizard
  cwdctx/                # Session-scoped CWD propagation
  context/               # EstimateTokens, GenerateSummary, PersistLearnings
  schedule/              # Schedule CRUD + atomic writes (plist gen lives in daemon/)
  permissions/           # bash resolution pipeline (see Permission Model below)
  audit/                 # JSON-lines logger + RedactSecrets
  hooks/                 # PreToolUse/PostToolUse/SessionStart/Stop
  instructions/          # LoadInstructions, LoadMemory, LoadCustomCommands
  prompt/                # BuildSystemPrompt (static/stable/volatile parts)
  session/               # JSON persistence + SQLite FTS5 index
  memory/                # Memory sidecar client (UDS, bundle pull, tenant fingerprint)
  mcp/                   # MCP client manager + JSON-RPC server + Chrome lifecycle
  runstatus/             # User-facing run state codes + 429 sub-shape parser
  skills/                # Skill registry, loader, secrets, marketplace
  tools/                 # Tool implementations (see Local Tools below)
  uploads/               # /api/v1/uploads client (POST/GET/DELETE)
  images/                # /api/v1/images/{generations,edits} client
  tui/                   # Bubbletea TUI + /compact + /doctor
  update/                # GitHub release auto-update
  sync/                  # Daily session JSON upload to Cloud
```

## Key Conventions

### Doc Co-Maintenance

Feature changes update README.md (user-facing), CLAUDE.md (this file, developer-facing), and AGENTS.md (external-agent-facing, mirrors structure tree + conventions).

**Kocoro skill is the AI's source of truth for the daemon HTTP API** — every `mux.HandleFunc(...)` in `internal/daemon/server.go` must have a matching entry in `internal/skills/bundled/skills/kocoro/references/*.md`. When adding endpoints, update the matching reference file in the same PR. Maps:
- agents/skills/schedules/config endpoints → `references/{agents,skills,schedules,config}.md`
- MCP / permissions / project-init / instructions / recipes / session-sync / memory → matching `references/*.md`
- `/local/auth/*` endpoints → `references/auth.md`
- `calendar_*` tools (8) + protocol → `references/calendar.md` + `references/desktop-rpc.md`; the underlying Unix socket protocol spec lives at `docs/desktop-calendar-rpc.md` (versioned outside the skill bundle because Desktop CC consumes it too)
- Protected config fields, tool filter → `SKILL.md` security section

### Hardcoded Limit Policy

When introducing `const max[A-Z]\w+ = <small_int>` (count caps, retention, retries, concurrency), the comment MUST name (1) the user workload that justifies the value, (2) the symptom when it binds, (3) the override path. Prefer `viper.SetDefault(...)` over `const` for caps a power user might need to lift. Re-check small-int caps whenever the model family upgrades (200K-era defaults often bind silently on 1M-context families).

### Auto-installed Builtin Skills

Skills listed in `builtinSkills` (`internal/skills/api.go`) are sha256-walk synced from `embed.FS` to `~/.shannon/skills/<name>/` on every startup. User edits to a builtin SKILL.md are wiped on next startup — fork under a different name. Current: `kocoro` (daemon API assistant), `kocoro-generative-ui` (`hidden: true`, html-artifact visualizations).

### Agent Names

Must match `^[a-z0-9][a-z0-9_-]{0,63}$`. Validated before any path concatenation to prevent traversal.

### Provider Architecture

`provider` config key selects the LLM backend: default → `GatewayClient` (Cloud); `ollama` → `OllamaClient` (OpenAI-compatible). Both implement `Complete` / `CompleteStream`.

### Tool Priority

Local tools > MCP tools > Gateway tools. Deduplicated by name.

### Tool Concurrency

The dispatcher batches tool calls by `IsConcurrencySafeCall`, not `IsReadOnlyCall`. Tools without an explicit `ConcurrencySafeChecker` implementation fall back to their `IsReadOnlyCall` value — so file_read / grep / glob etc. keep batching concurrently as before, and writers stay serial. Adding the new interface to one tool has no effect on any other tool's grouping.

`BashTool` implements `IsConcurrencySafeCall` via `internal/tools/bash_concurrency.go`. It is gated by `agent.bash_concurrency_enabled` (default `true` in Phase C — Desktop consumes the `tool_use_id_events` capability so id-keyed cards stay correct under concurrent batches). When the flag is on, commands whose first token is in a strict read-only whitelist AND contain no shell metacharacters (including `\n` / `\r`) are eligible for the concurrent batch. Everything else — `&&` / pipes / redirects / command substitution, plus any non-whitelisted leading token (`git push`, `npm install`, `curl`, `rm`, `git remote add`, `go env -w`, ...) — stays in a size-1 serial batch.

Tool events on the SSE/WS wire (`tool_status` running + completed) include a `tool_use_id` field so multi-tool-in-flight UIs (e.g. parallel bash) can pair them correctly. The daemon advertises this on the WS handshake via the `tool_use_id_events` capability token.

### Tool Required-Field Validation

Every tool's `Run()` MUST explicitly check that each field listed in `ToolInfo.Required` is non-zero immediately after `json.Unmarshal`, and return `agent.ValidationError(...)` (NOT a bare `ToolResult{Content: ..., IsError: true}`) on failure. Go's `json.Unmarshal` cannot distinguish "field missing" from "field present with zero value" on a strongly-typed struct, so a missing string `required` field arrives as `""` — which `os.WriteFile`, `exec.Command`, etc. happily accept. The 2026-05-13 production stuck loop was a `file_write` call with no `content` field that wrote 0 bytes, returned `IsError=false`, truncated the user's existing file, and trapped the model into a 16-call retry spin.

The `[validation error]` prefix that `ValidationError` injects is load-bearing: `LoopDetector.isValidationErrorSig` short-circuits a same-tool + same-args + 3-consecutive `[validation error]` run to `LoopForceStop`, well below the all-errors 2x ConsecutiveDup budget at call #7. Returning a hand-rolled `IsError=true` result without the prefix loses this early-stop and falls back to the slower flaky-retry path. Examples: `internal/tools/file_write.go` (content), `internal/tools/file_edit.go` (old_string), `internal/tools/archive.go` (path/dest), `internal/tools/cloud_delegate.go` (task), `internal/tools/edit_image.go` (prompt).

### Skill Discovery

Three layers triggering `use_skill`:
1. **Listing** — full descriptions (4000-char budget) in scaffolded user message on first turn. Not in system prompt (cache stability).
2. **Semantic** — blocking small-tier call on iter 0 (5s timeout), injects `<system-reminder>` hint. Gated by `agent.skill_discovery`.
3. **Catalog fallback** — `use_skill` description includes all skill names.

**Per-request channel suppression** (`internal/daemon/skill_filter.go`): `desktopOnlySkills` is filtered out of `loadedSkills` (use_skill registry + listing + semantic discovery) when `isCloudSource(req.Source)`. A single producer-side filter in `runner.go` keeps all three exposure layers consistent. Only entry today: `kocoro-generative-ui` (its `html-artifact` fences render only in Desktop's WKWebView; on cloud channels they'd surface as a code block). Drift test (`skill_filter_test.go`) walks `desktopOnlySkills × cloudSourceSet`. The divergence lives in the scaffolded user message, not the cached system prompt — revisit this filter if listing ever moves into the system prompt.

**Allowed-tools enforcement** is execution-time denial (`loop.go`), NOT schema filtering — tools array stays full for cache stability. Blocked tools return `[skill restriction]` error.

**Skill exemption** (`agent.SkillExempt` interface): `think`, `tool_search`, `use_skill` always run. Reserved for pure-infrastructure zero-I/O tools. **Do NOT add to tools with side effects** — those must stay skill-restrictable.

### Permission Model

```
hard-block → denied_commands → split compounds (incl. & and (...)) → always-ask (prefix + git-push dangerous-flag scan) → allowed (literal/glob + token-prefix family) → default safe → RequiresApproval + SafeChecker
```

Unknown tools → denied (fail-safe). Always-ask gate runs BEFORE the allowlist, so adding a high-risk command to `allowed_commands` is a no-op; "Always Allow" on always-ask commands is honored once, never persisted (enforced at write-time in `cmd/daemon.go` + `server.go` AND at runtime in `loop.go checkPermissionAndApproval`). Token-prefix family matching (depth 2 for known CLIs, 3 for unknowns) cannot widen scope past the always-ask gate.

### Daemon Architecture

| Subsystem | Where | One-line invariant |
|---|---|---|
| Desktop RPC channel | `internal/daemon/desktop_rpc/` + `cmd/daemon.go` + `internal/daemon/launchd_darwin.go` (header note) | Local Unix domain socket (`~/Library/Application Support/run.shannon.shanclaw/daemon.sock` 0600 + parent 0700) reverse-RPC to Kocoro Desktop's EventKit. Daemon spawned by Desktop's `DaemonManager` with `--rpc-socket` + `--rpc-pidfile` CLI flags. Length-prefixed JSON framing (4-byte big-endian uint32 prefix, body ≤ 4 MiB, no application-layer ping). DesktopRPCBroker mirrors ApprovalBroker race-safety. Single-instance accept (second client closed immediately). On sock disconnect → `CancelAll` unblocks all pending RPCs with `desktop_disconnected`. `system.ping` + `system.capabilities` are bidirectional; `system.capabilities` powers spec §4.1.1 reconciliation. ProtocolVersion = `"1.0.0"`; ProtocolMethods (10 entries) returned byte-identically by both responder sides. Sock listen failures are fatal (non-zero exit, no silent retry per spec §7.1). Lifecycle is semi-bound: daemon outlives Desktop UI quit (becomes launchd-orphan), Slack/LINE channels keep working until Mac reboot. Calendar `calendar_*` tools registered conditionally on broker existence (`tools.RegisterCalendarTools`); TUI/one-shot/MCP/scheduled paths skip. `launchd_darwin.go` is the npm-CLI standalone path and is NOT used by Desktop-bundled deployments. Spec: `docs/desktop-calendar-rpc.md` v0.5.1. |
| WS handshake | `client.go` | Sends `User-Agent: kocoro/<ver>` + `X-Kocoro-Daemon-Version` + `X-Kocoro-Capabilities`. Pre-v0.1.8 daemons used `X-ShanClaw-*`; Cloud accepts both for one release. |
| Built-in MCP catalog | `internal/mcp/builtins.go` + `internal/config/config.go mergeBuiltinMCPServers` | `BuiltinMCPServers` ships pre-bundled MCP servers (first: `intercom`) disabled by default. `config.Load` field-merges the Go catalog onto user yaml: command/args/url come from the binary (auto-upgrade), disabled/env/keep_alive persist from yaml. PATCH /config refuses to mutate daemon-owned fields on a builtin (`builtin_mcp_immutable`, 409). GET /config/status exposes `mcp_server_info` (`{builtin, display_name, requires_auth?, authorized?}`) so Desktop renders a toggle + OAuth modal without hard-coding the catalog; `authorized` reflects a detected token cache (skip the confirm modal on re-enable). |
| MCP async startup | `internal/mcp/client.go StartConnectAll` + `register.go RegisterAllWithBaselineAsync` | Startup and `/config/reload` don't block on MCP handshakes — they build the registry with local+gateway tools synchronously, then fire per-server connect goroutines. A per-server `inFlight` set dedups concurrent attempts (a reload during an in-flight connect skips rather than spawning a duplicate subprocess). Timeout: `ConnectTimeoutSeconds` > `DefaultConnectTimeoutSecs` > 60s (Intercom ships 300s for cold-cache npx + OAuth). Success → `Supervisor.ProbeNow` → registry rebuild, tools appear live. Failed connects do NOT auto-reconnect; recovery is user re-toggle or `POST /config/reload`. |
| MCP subprocess reaping | `internal/mcp/processgroup_unix.go` + `client.go cancellers` | Stdio MCP subprocesses spawn in their own process group (Setpgid) with `-pgid` SIGTERM + 3s SIGKILL backstop. Needed because npx-bridged servers are a process chain (npx → npm exec → node): killing only the direct child orphans a grandchild holding the OAuth loopback port (EADDRINUSE on re-toggle). `Disconnect` / `Close` / `Reconnect` all cancel the group before `c.Close()`. |
| Reload as explicit retry | `internal/daemon/server.go retryDisconnectedEnabledMCPServers` | When `/config/reload` runs without an MCP config delta (`mcpChanged=false`), the existing mgr+supervisor stays. The supervisor's no-auto-reconnect policy would otherwise leave previously-failed servers stuck in "enabled" forever. The reload tail now fires a fresh `StartConnectAll` for every `disabled: false` server not present in `mgr.ConnectedServers()` — Desktop's "Retry" button maps to this endpoint and works against the canonical lifecycle. |
| `delivery_ack` capability | `client.go:sendDeliveryAck` | After `SendReply` succeeds for `MsgTypeMessage`, emit ack so Cloud drops the replay-buffer entry. Reply-failure paths skip the ack so replay is correct. |
| Attachments | `attachment.go` | Priority `document_b64` → `extracted_text` → URL download. Caps: 500 MB/file, 20/msg, inline doc ≤ 25 MB raw. Capability tokens `inline_document_b64` / `inline_extracted_text` gate the new fields. DOCX/XLSX/PPTX/CSV extraction is daemon-local via `internal/tools/doc_extract.go`. Cloud fills PDF `DocumentB64` + transcodes HEIC/AVIF. |
| Session routing | `router.go` | `ComputeRouteKey` precedence: `PinnedRouteKey` (sticky schedule) → `session:<id>` → thread → sender → plain `agent:<name>` (only when NOT `new_session`) → channel. Web/webhook/cron bypass (always fresh). **Named agents are multi-session** (honor `session_id`/`new_session` like the default agent post-D1); the plain `agent:<name>` lane resolves to the latest `kind=interactive` session via `Manager.ResumeLatestMatching(isInteractiveSource)` (never a schedule/IM session). **Schedule `Stateful`** is the single remember-across-runs switch (`schedule.IsSticky`): `false` / legacy-nil → fresh session each run (`NewSession`, `OmitHistory`); `true` → dedicated `agent:<name>:schedule:<id>` / `schedule:<id>` session that accumulates, pinned via `PinnedRouteKey`, with the LLM seeing its history. One switch drives both the route key and the history view. **Heartbeat** reads/appends the latest `kind=interactive` session (`router.go` `ResolveLatestSession`/`AppendToSession`); a session switch mid-run hits `ErrSessionChanged` and is dropped silently. |
| Session share uploads | `daemon/share_handler.go` + `share_async.go` | Render HTML → POST `/api/v1/uploads` with `kind=session_share`; the post-upload LIST also filters by that kind so concurrent uploads can't bump our row off page 1. publish_to_web uses `kind=other`. Template `<head>` carries OG / Twitter Card / JSON-LD for social previews (`internal/share/social_meta.go`). Config: `daemon.share_metadata.{site_name, site_url, default_og_image, twitter_image, logo_url}`. |
| Output format | `runner.go outputFormatForSource` | `plain` for cloud-distributed channels (slack/line/feishu/lark/telegram/webhook); `markdown` default. |
| Tool result sizing | `spill.go` + `toolresult_budget.go` + `context_bloat.go` | Per-result spill at policy threshold (default 50K, grep 20K) → tmp file + 2K preview. `file_read` is `UnlimitedToolResultSizeChars` (no spill); it self-bounds via `fileReadHardCapRunes = 500_000` in the tool itself, with truncation marker. Per-turn 200K-rune aggregate cap skips Unlimited tools. `ToolResultReplacements` + `ToolResultSeen` persisted across checkpoints AND terminal saves. |
| file_read dedup | `agent/readtracker.go` + `daemon/readtracker_cache.go` | Records `(path, offset, limit, mtime, size)`; re-reads return a stub. Per-session, released via `SessionManager.OnSessionClose`. |
| Image size guard | `imaging_compress.go` + `oversize_image.go` | Three layers: source-time compression (`EncodeImage` decode→2000×2000→JPEG ladder), wire-time sanitizer (`filterOversizeImages` in `messagesForLLM`), persist-time guard (`SanitizedRunMessages`). |
| Skill secrets | `skills/secrets.go` | Keychain `com.shannon.skill.<name>` + plaintext index of key NAMES only. Env-var-only injection, scoped to skills activated by `use_skill` in the current run. |
| Turn phase tracker | `agent/phase.go` | Only `PhaseAwaitingLLM` and `PhaseForceStop` idle-counted. Fail-closed: panics under `testing.Testing()` or `SHANNON_PHASE_STRICT=1`. |
| Idle watchdog | `agent/watchdog.go` + `client/gateway.go` | Two layers. Turn-elapsed: `OnRunStatus("idle_soft")` at `agent.idle_soft_timeout_secs` (default 90), `ctx.Cancel(ErrHardIdleTimeout)` at hard (default 540; opt out via `0` + daemon startup WARN). Streaming chunk-gap: `CompleteStream` returns `ErrStreamIdleTimeout` if no SSE chunk arrives within `agent.stream_idle_timeout_secs` (default 90). The agent loop short-circuits the streaming→Complete fallback on `ErrStreamIdleTimeout` (silent upstream drop would re-hang the non-streaming path) and `isRetryableLLMError` refuses to retry it. `completeWithRetry` prefers `context.Cause(ctx)`. |
| Mid-turn checkpoint | runner `applyTurn*` helpers | Fires at three phase-exit boundaries; 2s debounce. Same helpers run from checkpoint, final save, and hard-error save. `session.InProgress` non-zero on reload = crash recovery. |
| Schedule proactive push | `scheduler.go` `broadcastReply` + `broadcast_gate.go` `shouldBroadcast` | After a successful run, push is gated by `shouldBroadcast`: explicit `Schedule.Broadcast *bool` wins; else smart default by `CreatedFromSource` (IM sources → broadcast, else silent; empty/pre-feature → silent). LLM tools `schedule_create` / `_update` and `PATCH /schedules/{id}` accept `broadcast: "auto"\|"on"\|"off"`. Capability token `schedule_broadcast_gate`. |
| Playwright file:// bridge | `tools/filepreview.go` | Loopback HTTP rewrites `browser_navigate(file://…)`. Symlink-resolved allowlist; loopback-only `r.RemoteAddr` check. |
| Session sync | `internal/sync/` | Daily upload (opt-in `sync.enabled`). flock + atomic marker. Permanent failure reasons (`size_limit_exceeded`, `load_error`) self-heal on session edit. |
| Memory client | `internal/memory/` | Daemon owns sidecar lifecycle + 24h bundle pull. `memory_recall` → `Service.Query` over UDS; falls back to `session_search` + MEMORY.md when not `Ready`. API key never on disk — only `sha256[:16]` fingerprint. Schema-mismatch lockout surfaces as `memory.reason=tlm_binary_too_old` on `GET /status` and triggers a one-shot self-heal pull before degrading (instead of burning the restart budget). `Sidecar.Shutdown` is idempotent, so failed children don't accumulate as orphan processes. |
| Episodic preflight | `agent/preflight.go` + `tools/memory_preflight.go` | Small-tier helper compiles `QueryIntent`s before first main call; `<private_memory>` injected into in-flight user message, NEVER persisted to transcript, stripped from compaction inputs. Audit row content-free. |
| Loop detector | `agent/loopdetect.go` | 9 detectors. `dupExemptTools` skip dup detection; all-errors 2× budget; rolling nudge window (max 3 within trailing 5). |
| Empty-think force-stop | `loopdetect.go` rule "0a" | Two consecutive `think({})` → `LoopForceStop`. Defends against Sonnet 4.6 / Opus 4.7 ritual empty think after native interleaved thinking. |
| Thinking blocks | `client.ContentBlock` + `agent.buildAssistantMessage` | Cloud relays full ordered `content_blocks` incl. `thinking`/`redacted_thinking`. Persisted verbatim; `internal/sync/strip_thinking.go` removes from upload-side copy before size check. Sanitizers in `messagesForLLM` / time-based / micro-compact / `BuildForkedRequest` preserve them. |
| Conditional `think` tool | `tools/register.go shouldRegisterThinkTool` | Not registered on default gateway+thinking path. Still registered when thinking disabled, Ollama provider, or `ForceThinkTool=true`. `operationalRules()` strips `### Planning` bullet only when think absent, keeping prompt byte-equal otherwise. |
| Prompt suggestion | `agent/suggestion.go` | Forked LLM call after each main turn. **CACHE SAFETY**: byte-equal to main request except 2 appended messages + `SkipCacheWrite: true`. Any other divergence fragments the cache. |
| Email/password auth (macOS only) | `internal/daemon/auth.go` + `auth_handlers.go` + `ws_controller.go` + `internal/keychain/` | `/local/auth/*` proxy to Cloud `/api/v1/auth/*`. AuthManager state machine drives WS lifecycle — WS runs only in `signed_in`. api_key is the source-of-truth credential in Keychain (`ai.kocoro.daemon.api_key/<user_id>`); the yaml field is migrated away on first launch. Non-darwin: AuthManager nil, endpoints 503, legacy `cfg.APIKey` path. |

### Daemon Approval Protocol

- **Interactive** (default): approval round-trips over WS to Ptfrog.
- **Auto-approve** (`daemon.auto_approve` or per-agent): skips WS round-trip, permission engine still enforced.
- HTTP API handlers auto-approve (localhost-only).

"Always Allow" goes through `alwaysallow.go HandleAlwaysAllowDecision` — single entry point shared by SSE and WS so transports can't drift. Persistence matrix:

| Tool | Agent | Persistence | Notes |
|---|---|---|---|
| bash, always-ask command | any | none | One-time allow + `EventApprovalNotice` warning. Runtime gate in `loop.go` enforces denylist even if hand-written into config. |
| bash, safe command | named | per-agent `permissions.always_allow_tools` | Future bash from this agent skips approval. |
| bash, safe command | default (`req.Agent==""`) | GLOBAL `permissions.always_allow_tools` | Affects all agents. PR 6 fix for non-technical users on default agent. |
| non-bash | named | per-agent tool-level | `agent.DisallowsAutoApproval` (currently empty) refuses persistence + emits warn notice. publish_to_web / generate_image / edit_image used to be on this list — moved off 2026-05-18, see `internal/agent/tools.go` for trade-off rationale. |
| non-bash | default | global tool-level | Same path bash takes. SSE handler creates fresh broker per request, so broker-only persistence evaporates. |

Global and per-agent always-allow lists are **unioned at injection** in `SetAlwaysAllowTools` (called from runner.go / tui/app.go / cmd/root.go after `SwitchAgent`). `SwitchAgent` resets the field so reuse can't leak.

**Two auto-approval deny-lists, both currently empty** (as of 2026-05-18): `agent.DisallowsAutoApproval` (refuses "always allow" persistence) and `agent.DisallowsUnattendedAutoApproval` (refuses auto-approval on scheduled / heartbeat / watcher / `auto_approve` paths). Plumbing is preserved for a future high-risk tool (account deletion, payment auth, etc.) — both are no-ops while empty. `approval_request.flags` carries `always_allow_disabled` for tools on the first list (none today).

**`EventApprovalNotice`** payload is `{severity, code, tool, message}`. `code` is a stable i18n key (`high_risk_not_persistable` / `bash_always_ask_not_persisted` / `persist_failed`); daemon NEVER ships translated text. `message` is English fallback.

**Approval-card `description` field**: every tool whose `RequiresApproval()` returns true requires a `description` field (5-15 words, user-facing intent, model writes it, daemon passes through). UI clients render it prominently; raw args behind a toggle. Spec in `internal/agent/approval_description.go`. Exemptions: `bash` keeps its bespoke schema (cache-stability), `computer` is a native tool (Parameters not wire-transmitted — UI synthesizes from action/x/y), `publish_to_web` declares both `description` and `purpose`. Daemon does NOT block on missing/empty `description`; UI must fall back to tool-specific args using `description?.trim() || fallback` (NOT nullish coalescing).

### Config Merge Order

1. `~/.shannon/config.yaml` (global)
2. `.shannon/config.yaml` (project)
3. `.shannon/config.local.yaml` (local, gitignored)

Scalars override, lists merge+dedup, structs field-level merge. MCP server env-var casing preserved via direct YAML re-read.

### File Paths

- Agent: `~/.shannon/agents/<name>/{AGENT.md, MEMORY.md, config.yaml, commands/*.md, _attached.yaml}`
- Global skills: `~/.shannon/skills/<name>/SKILL.md`
- Sessions: `~/.shannon/sessions/` (default) or `~/.shannon/agents/<name>/sessions/` (per-agent); SQLite FTS5 index at `<sessions-dir>/sessions.db` (auto-rebuilt)
- Spill: `~/.shannon/tmp/tool_result_<session>_<call_id>.txt`
- Attachments: `~/.shannon/tmp/attachments/<nonce>/`
- Schedules: `~/.shannon/schedules.json` + `~/Library/LaunchAgents/com.shannon.schedule.<id>.plist`
- Notification history: `~/.shannon/notifications.jsonl` (JSONL append-only, capped at 500 entries; trimmed + atomically rewritten on daemon startup, survives restarts)
- Skill secrets index: `~/.shannon/secrets-index.json` (chmod 600, flock-protected, names only); values in macOS Keychain (service `com.shannon.skill.<name>`)
- Daemon api_key (macOS only): macOS Keychain service `ai.kocoro.daemon.api_key`, account = Cloud user_id (UUID). Active user pointer at service `ai.kocoro.daemon.state`, account `current_user_id`. `cfg.APIKey` (yaml) is now empty after the v1 migration; Bootstrap reads Keychain instead
- Sync: marker `~/.shannon/sync_marker.json`, lock `~/.shannon/sync.lock` (never delete), dry-run outbox `~/.shannon/sync_outbox/`
- Logs: `~/.shannon/logs/audit.log`, `~/.shannon/logs/schedule-<id>.log`
- Memory: socket `~/.shannon/memory.sock`, bundle root `~/.shannon/memory/`
- Desktop RPC (when daemon spawned by Kocoro Desktop): sock `~/Library/Application Support/run.shannon.shanclaw/daemon.sock` (0600) + pidfile `daemon.pid` in same dir (0700). Paths passed via `--rpc-socket` + `--rpc-pidfile` CLI flags; daemon never derives one from the other.

### Atomic Writes

`schedules.json` and `secrets-index.json` use write-to-temp + `os.Rename` + `syscall.Flock` on a persistent `.lock` file. **Never delete the lock file** (causes flock race on different inodes).

### Prompt Cache

See `docs/cache-strategy.md` (4-breakpoint allocation, source→TTL routing, byte stability) and `docs/cache-debug.md` (env flags, log fields, drift patterns). Invariants:

- Every LLM call tags `cache_source` (TTL routing, `SHANNON_FORCE_TTL`/`SHANNON_CACHE_DEBUG` overrides all covered in the docs above).
- `normalizeToolInput` (`gateway.go`) canonicalizes nested JSON key ordering for byte-stability.
- Skill allowed-tools = execution-time denial, not schema filtering (tools array stays byte-stable).
- Skill listing lives in the scaffolded user message, not system prompt.
- All in-place `messages[idx].Content` rewrites MUST call `client.LogCacheCompactEvent` — uninstrumented rewrites silently break drift attribution.

### Context Management

- **Context window**: `agent.context_window` (default 1_000_000 — matches the Opus 4.7 / Sonnet 4.6 / Gemini-3-Pro 1M-context families that medium/large tiers route to) is a seed; `maybeAutoAdjustContextWindow` resets from `response.model` via `modelcontext.go` (Anthropic/OpenAI/Google/xAI; 1M and 200K families). Catches Cloud tier-failover (e.g. Haiku 200K) in either direction. Per-agent override calls `SetContextWindowExplicit` (lock); auto-detect skips locked loops. For Ollama (model names absent from `LookupModelContextWindow`), callers wrap the fallback with `agent.ContextWindowFloorForProvider` which clamps to 200K so a local 128K model is not seeded at 1M.
- **Proactive compaction** at 90%: `PersistLearnings` → `GenerateSummary` (two-phase analysis→summary) → `ShapeHistory`.
- **Pre-flight compaction** at 95% (`shouldPreflightCompact`): backup gate before each main LLM call + force-stop turn. Emits `OnRunStatus("preflight_compaction")`.
- **Reactive compaction** on context-length error: emergency compress + single retry; `reactiveCompacted` prevents loops. Summarize input itself capped at `summarizeInputCapChars=540_000` rune-safe head+tail (else cascade re-overflows on small tier).
- **Failure telemetry**: `recordCompactionFailure` emits `OnRunStatus("compaction_failed")` + audit row. 9 phase tags.
- **Tiered result compression**: Tier 1 (>10 msg old) metadata only; Tier 2 (3-10) head+tail; Tier 3 (0-2) full.
- **Memory staleness**: `annotateStaleness()` appends `[N days ago]` to memory headings.
- **Deferred tool loading**: when count > 30, MCP/gateway tools sent as name+description; model calls `tool_search`.
- **System reminders**: short `<system-reminder>` hints appended to `file_read`/`file_write`/`file_edit`/`bash` results; skipped for `cloud_delegate`.

### Anti-Hallucination

XML `<tool_exec>` delimiters with random hex call_id. Preamble suppressed when response has tool calls. Fabricated tool calls detected and stripped.

## Testing

```bash
go test ./...                              # all
go test ./internal/daemon/ -v              # daemon: WS, router, E2E routing, launchd
go test ./internal/agent/ -v               # loop, partitioning, spill, deferred
go test ./internal/agents/ -v              # agent loader
go test ./internal/schedule/ -v            # schedule CRUD
go test ./test/ -v                         # E2E: vision pipeline, persist learnings
go test ./test/e2e/ -v                     # E2E offline (CI)
SHANNON_E2E_LIVE=1 go test ./test/e2e/ -v  # E2E live (run before each release)
go build ./...
```

Schedule tests use temp dirs — never write to real `~/Library/LaunchAgents/`. Launchd plist coverage lives with daemon tests.

## Building & Releasing

- GoReleaser: `.goreleaser.yaml`
- npm: `@kocoro/kocoro` (previously `@kocoro/shanclaw`, deprecated post-v0.1.7)
- **Versioning: PATCH-only by default** — do NOT bump minor/major unless explicitly asked
- Release: `git tag -a vX.Y.Z` → `git push origin vX.Y.Z` → CI publishes
- `docs/` is gitignored by default — only `docs/cache-strategy.md` and `docs/cache-debug.md` tracked; add new via explicit `!docs/<file>.md` in `.gitignore`

## Local Tools

Always registered (`internal/tools/register.go RegisterLocalTools`):

- **File**: file_read (auto-compresses images >3.75 MB raw, see `imaging_compress.go`), file_write, file_edit, glob, grep, directory_list
- **Archive**: archive_inspect (read-only), archive_extract (approval). Zip/tar/tar.gz via stdlib. Atomic staging+rename; rejects encrypted/absolute/symlink/device/setuid; zipbomb caps (50 MB/entry, 200 MB total, 500 entries). See `archive.go`.
- **Documents**: pdf_to_text, docx_to_text, xlsx_to_text, pptx_to_text. Prefer poppler/pandoc/xlsx2csv; fall back to unzip+XML strip (no fallback for PDF — surfaces `brew install poppler` hint + suggests upload for native Anthropic document block). Fixed-argv, 60s timeout, 100K-rune output cap. See `doc_extract.go`.
- **Shell/system**: bash, system_info, process, http, think
- **macOS GUI**: accessibility (primary), applescript, screenshot, computer, clipboard, notify, browser, wait_for, ghostty
- **Schedule**: schedule_create / _list / _update / _remove
- **Memory**: memory_append (flock-protected MEMORY.md append)
- **Skills**: use_skill

Conditional:

- `session_search` — when session manager available
- `cloud_delegate` — `cloud.enabled: true`
- `publish_to_web` — `cloud.enabled` + `cfg.APIKey`. Always approval. Path-segment + basename blocklist (`.env`/`.pem`/…); extension allowlist (`cloud.publish_allowed_extensions`). All uploads tagged `kind=other` server-side; the kind enum (`session_share`/`report`/`landing_page`/`image`/`other` — see `internal/uploads/client.go`) is NOT exposed to the model (avoids landing_page misclassification for ad-hoc shares).
- `list_my_published_files` — same gating. Read-only, no approval. `limit` (≤100), `offset`, optional `kind` filter (same enum). Returns paged `UploadEntry` rows keyed by id; rendering surfaces a `kind=…` badge per row so the LLM can answer "which of these are session shares".
- `retract_published_file` — same gating. Destructive, requires approval. `agent.DisallowsAutoApproval` is empty as of 2026-05-18, so retract behaves like other approval-required tools (can be persisted to always-allow if the user chooses). Args: `id` (UUID from list) + `description`. 404 conflates not-found/already-retracted/not-yours to avoid existence leak.
- `generate_image` / `edit_image` — same gating. Always approval (paid quota + permanent CDN). Edit requires `image_urls` 1-4 entries starting with `https://static.kocoro.ai/`.
- `tool_search` — deferred mode when tool count > 30 (lives in `agent/deferred.go`)
- **`calendar_*` family (8 tools)** — registered only when daemon is a Kocoro Desktop subprocess (`cmd/daemon.go` sees `--rpc-socket` + `--rpc-pidfile` flags and constructs a `DesktopRPCBroker` in `internal/daemon/desktop_rpc/`). TUI / one-shot CLI / MCP server / scheduled task paths skip registration (`tools.RegisterCalendarTools` no-ops when broker is nil) — model falls back to `applescript` + Calendar.app naturally. Tools: `calendar_check_permission`, `calendar_request_permission` (approval, 5-min envelope timeout for TCC dialog wait), `calendar_list_sources`, `calendar_list_events`, `calendar_get_event`, `calendar_create_event` / `_update_event` / `_delete_event` (approval). Backed by `docs/desktop-calendar-rpc.md` v0.5.1 (Unix domain socket reverse RPC to Desktop's EventKit). `attendees` field is metadata-only — `invitations_sent` is always `false` in v1 (EventKit doesn't auto-send via CalDAV/Exchange). `update_event` rejects `scope=all` client-side; use `delete + create` instead.
