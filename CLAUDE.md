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
  koe.go               # shan koe — voice front-brain (OpenAI Realtime + Desktop control + via-daemon mint); --mic-device/--speaker-device pass CoreAudio device UIDs (from koe.mic_device/speaker_device) that the VPIO backend binds, empty = system default; --barge-in (from koe.barge_in) enables reversible native-S2S floor control by setting KOE_VPIO_BARGE_IN=1 + KOE_NATIVE_FLOOR=1 + KOE_INTERRUPT_RESPONSE=0 via applyBargeInEnv (vpio-only, no-op on gate); koe.persona_source ("global" distill default | "custom" → koe.custom_persona verbatim) selects the spoken persona (daemon buildKoePersona)

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
    auth.go            # AuthManager state machine (email/password; macOS + Windows + Linux)
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
  keychain/              # Credential store wrapper — macOS Keychain / Windows Credential Manager via go-keyring (backend_keyring.go), Linux file store at ~/.shannon/credentials.json 0600 (backend_linux.go + backend_file.go); api_key source of truth. Other GOOS → ErrUnsupportedPlatform
  client/                # GatewayClient (Anthropic via Cloud), OllamaClient, SSE, AuthClient (/auth/*)
  cloudflow/             # /research, /swarm Gateway workflow runner
  heartbeat/             # Per-agent HEARTBEAT.md + alerts
  watcher/               # Per-agent debounced FS watcher
  config/                # Config struct, multi-level merge, --setup wizard
  cwdctx/                # Session-scoped CWD propagation
  context/               # EstimateTokens, GenerateSummary, PersistLearnings
  fslock/                # Cross-platform advisory file lock (flock vs LockFileEx); see Cross-Platform Support
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
  skills/                # Skill registry, loader, secrets, marketplace (two sources: static registry → /skills/marketplace/*; ClawHub live catalog → /skills/clawhub/*)
  tools/                 # Tool implementations (see Local Tools below)
  uploads/               # /api/v1/uploads client (POST/GET/DELETE)
  images/                # /api/v1/images/{generations,edits} client
  tui/                   # Bubbletea TUI + /compact + /doctor
  update/                # GitHub release auto-update
  sync/                  # Daily session JSON upload to Cloud
  koe/                   # ── VOICE FRONT-BRAIN (shan koe) ── cgo since audio.go
    link.go              #   DaemonClient: DoTask/Cancel/ListAgents + MintViaDaemon + SendRealtimeUsage (HTTP to daemon, NEVER imports internal/daemon)
    agentresolve.go      #   agent name-resolution ladder (exact → bidirectional-substring → semantic-noop → not-found)
    tools.go             #   6 OpenAI-Realtime voice tools (do_task/cancel/get_status/control_app/switch_agent/end_call) + Dispatcher + CallState. Ledger-mode cancel targets one task_id, or all_running=true for one atomic stop-everything call (per-task partial failures stay explicit and a failed task stays running). end_call = dismiss/hang-up: the model judges the intent from audio (robuster than the garbled input transcription), handled in realtime.go via the onEndCall hook → Desktop endCall (goodbye earcon + teardown); no function_call_output, no spoken reply. Standalone/CLI wires onEndCall to earcon + process exit (sync.Once). The ASR transcript dismiss backstop is opt-in (KOE_ASR_DISMISS_BACKSTOP=1, default off — ASR stays out of every control path; hang-up trusts the model's end_call) — see dismissintent.go / realtime.go. MapDoTaskOutcome maps a partial do_task run (soft idle/deadline timeout, iteration_limit, force-stop — but NOT user_cancelled, which stays silent) to a canned per-language `incomplete` line and seeds no digest, so a cut run's progress tail is never voiced as the result; the daemon logs `koe voice projection kind=authored|mechanical partial=… failure_code=…` per turn for clean-success provenance.
    ledger.go            #   call-scoped task lineages: stable task IDs/routes, parallel work, targeted follow-up/cancel, revision state
    toolloop.go          #   semantic-free per-turn response/tool provenance, four-action continuation budget, newer-turn preemption, replay fuse
    result_mailbox.go    #   owner-leased asynchronous result queue; batches ready task revisions and retries unacknowledged delivery
    floor.go             #   reversible native-S2S interruption state machine: pause exact PCM, then resume_playback or accept_turn without ASR admission; an accepted interruption also truncates the paused assistant item server-side (conversation.item.truncate to the audio actually heard) so the model never treats unspoken text as said
    audio.go             #   malgo duplex (CoreAudio) + Opus codec + half-duplex gate (cgo deps: brew install opus opusfile pkg-config; PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig)
    webrtc.go            #   pion mint + SDP + Opus tracks + oai-events data channel + Connect orchestrator (ConnectOptions)
    realtime.go          #   GA session config (create_response:true auto-respond) + oai-events dispatch + reachy say-and-ask do_task (result is the single function_call_output) + voice_state/usage hooks; dismiss/hang-up has TWO paths both firing onEndCall: the end_call tool (handleFunctionCall, model judgment reinforced in koePersona) + a deterministic backstop in handleInputTranscript (isDismissPhrase on the transcript). onEndCall is idempotent (Desktop callActive guard / standalone sync.Once) so a racing double-fire only hangs up + earcons once. Local-commit fallback (manual input_audio_buffer.commit after local speech end) is opt-in via KOE_LOCAL_COMMIT_FALLBACK=1, default off since 2026-07-09 — far-field fragment gate-opens (Reachy) turned commit_empty rejections into spoken "could not hear you" loops; even when enabled, a commit_empty rejection is classified as a fragment and dropped silently (commitEmptySeq short-circuits the ack wait), never ask-to-repeat
    dismissintent.go     #   isDismissPhrase/normalizeDismissPhrase: whole-utterance closed-vocabulary match (zh simp+trad / en / ja: 闭嘴/停/够了/退出/再见/bye/黙れ…) PLUS strong-token containment — a short utterance (≤16 runes, KOE_DISMISS_CONTAIN_MAX_RUNES) merely CONTAINING 闭嘴/住口/shut up/黙れ with no negator (别闭/没/谁让/didn't…) also hangs up (live miss 2026-07-09: "不需要了,闭嘴吧" got a non-sequitur reply instead of end_call; KOE_DISMISS_CONTAIN=0 kills containment only) → deterministic hang-up, the RELIABLE half beside the model-judged end_call tool (gpt-realtime-mini called end_call for only 1/7 dismissals pre-persona-fix). While do_task is in flight, ambiguous stop words (stop/停/停止/やめて…) are left to the model so they can cancel the task instead of hanging up. KOE_DISMISS_DETECT=0 kills it, KOE_DISMISS_PHRASES extends. Tagless pure Go
    control.go           #   ControlServer: Desktop↔Koe HTTP+SSE (POST /call/start|end|interrupt|mic, GET /events: voice_state[+task_pending/mic]/control_app/call_state/mic_status); optional Bearer auth via KOE_CONTROL_TOKEN env, never argv
    micwatchdog.go       #   MicSilenceState: pure silent-input watchdog core (clamshell/covered mic → mic_status "silent"/"ok" to Desktop; driver ticker in cmd/koe.go; KOE_MIC_SILENCE_FLOOR/_MS tunable; no restart/rebind by design)
    earcon.go            #   "ready" + "dismiss" earcons (go:embed assets/{ready,dismiss}.pcm, 48k mono): shared playEarcon() SetSpeaking-gated so it can't self-trigger VAD. PlayReadyEarcon() at emitReadyLocked (KOE_READY_EARCON=0 disables); PlayDismissEarcon() a soft descending goodbye cue in the Desktop endCall path so every hang-up (Esc / menu Stop / end_call tool) signs off (KOE_DISMISS_EARCON=0 disables)

## Key Conventions

### Doc Co-Maintenance

Feature changes update README.md (user-facing), CLAUDE.md (this file, developer-facing), and AGENTS.md (external-agent-facing, mirrors structure tree + conventions).

**Kocoro skill is the AI's source of truth for the daemon HTTP API** — `references/*.md` are injected into the **kocoro agent's** context, so the rule covers only endpoints the agent calls or must understand: every such `mux.HandleFunc(...)` in `internal/daemon/server.go` needs a matching `references/*.md` entry in the same PR. Maps:
- agents/skills/schedules/config endpoints → `references/{agents,skills,schedules,config}.md`
- MCP / permissions / project-init / instructions / recipes / session-sync / memory → matching `references/*.md`
- `/local/auth/*` endpoints → `references/auth.md`
- `calendar_*` tools (8) + protocol → `references/calendar.md` + `references/desktop-rpc.md` (these skill refs are the public protocol reference); the full design doc `docs/desktop-calendar-rpc.md` is local-only / untracked (rationale + closed-app internals, not shipped)
- Protected config fields, tool filter → `SKILL.md` security section
- Desktop-only transport endpoints the agent never calls → NOT in references; their Desktop↔daemon wire contract lives in `docs/desktop-wire-fixtures/`

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
| Desktop RPC channel | `internal/daemon/desktop_rpc/` + `cmd/daemon.go` | Unix domain socket (`~/Library/Application Support/run.shannon.shanclaw/daemon.sock`, 0600 + parent 0700) reverse-RPC to Kocoro Desktop's EventKit. Daemon spawned by Desktop's `DaemonManager` with `--rpc-socket` + `--rpc-pidfile`. Length-prefixed JSON framing (4-byte BE uint32, body ≤ 4 MiB). `DesktopRPCBroker` mirrors `ApprovalBroker` race-safety; single-instance accept. Sock disconnect → `CancelAll` (`desktop_disconnected`); sock listen failure is fatal. `system.ping`/`system.capabilities` bidirectional. ProtocolVersion `1.0.0`; both responder sides return ProtocolMethods byte-identically. Lifecycle is semi-bound: daemon outlives Desktop UI quit (launchd-orphan), Slack/LINE channels keep working until Mac reboot. `launchd_darwin.go` is the npm-CLI standalone path, NOT used by Desktop-bundled deployments. Calendar `calendar_*` tools registered only when the broker exists (`tools.RegisterCalendarTools`); TUI/one-shot/MCP/scheduled paths skip. Spec: `docs/desktop-calendar-rpc.md` v0.5.1. |
| WS handshake | `client.go` | Sends `User-Agent: kocoro/<ver>` + `X-Kocoro-Daemon-Version` + `X-Kocoro-Capabilities` headers. Pre-v0.1.8 daemons used `X-ShanClaw-*`; Cloud accepts both for one release. |
| Built-in MCP catalog | `internal/mcp/builtins.go` + `config.go mergeBuiltinMCPServers` | `BuiltinMCPServers` ships pre-bundled MCP servers (e.g. `intercom`) disabled by default. `config.Load` field-merges the Go catalog onto user yaml: command/args/url from the binary (auto-upgrade), disabled/env/keep_alive persist from yaml. PATCH /config refuses daemon-owned fields on a builtin (`builtin_mcp_immutable`, 409). GET /config/status exposes `mcp_server_info` (`{builtin, display_name, requires_auth?, authorized?}`) so Desktop renders a toggle + OAuth modal without hard-coding the catalog. |
| MCP async startup | `internal/mcp/client.go StartConnectAll` + `register.go RegisterAllWithBaselineAsync` | Startup and `/config/reload` don't block on MCP handshakes — the registry builds with local+gateway tools synchronously, then per-server connect goroutines fire. A per-server `inFlight` set dedups concurrent attempts. Timeout: `ConnectTimeoutSeconds` > `DefaultConnectTimeoutSecs` > 60s. Success → `Supervisor.ProbeNow` → registry rebuild, tools live. Failed connects do NOT auto-reconnect; recovery is user re-toggle or `POST /config/reload`. |
| MCP subprocess reaping | `internal/mcp/processgroup_unix.go` + `client.go cancellers` | Stdio MCP subprocesses spawn in their own process group (Setpgid), killed via `-pgid` SIGTERM + 3s SIGKILL backstop. Needed because npx-bridged servers are a process chain (npx → npm exec → node): killing only the direct child orphans a grandchild holding the OAuth loopback port (EADDRINUSE on re-toggle). `Disconnect`/`Close`/`Reconnect` cancel the group before `c.Close()`. |
| Reload as explicit retry | `server.go retryDisconnectedEnabledMCPServers` | When `/config/reload` runs without an MCP config delta (`mcpChanged=false`), the reload tail fires a fresh `StartConnectAll` for every `disabled: false` server not in `mgr.ConnectedServers()` — otherwise the no-auto-reconnect policy leaves previously-failed servers stuck "enabled" forever. Desktop's "Retry" button maps here. |
| `delivery_ack` capability | `client.go:SendDeliveryAck` | After `SendReply` succeeds for `MsgTypeMessage`, emit ack so Cloud drops the replay-buffer entry. Reply-failure paths skip the ack so replay is correct. **Per-inbound reply addressing**: a run that absorbed mid-run injected follow-ups completes + acks EACH inbound message under its OWN cloud id — superseded turns via `OnIntermediateAnswer(text, cloudMessageID)` (a real `SendReply`+ack, not an `LLM_OUTPUT` timeline segment), the final answer + co-acks via `RunAgentResult.{ReplyToMessageID,PendingAckMessageIDs}` → `Client.SetReplyPlan` (handleMessage acks every absorbed id ONLY after the final reply is delivered — ack-after-delivery). The injected follow-up's own handler suppresses BOTH its reply and ack via `Client.SuppressReply`; the owning run is solely responsible for acking it, so a reply failure or crash replays it instead of losing the answer. This stops Cloud from collapsing two logically-distinct replies (e.g. group-chat messages from different senders, Slack/Feishu/WeCom/Teams) into one channel message because they shared the primary message_id. The loop tracks the targets via `AgentLoop.SetReplyCloudMessageID` / `ReplyCloudMessageID` / `PendingAckIDs`, advancing them in `commitInjectedTurn`; the error path addresses the failure to the last processed id too (`RunAgent` returns a non-nil result on the post-loop error). |
| Generic integrations broker | `integrations_handler.go` + `client/gateway.go` | Desktop-renderer-driven cloud proxy (mirrors the Slack/Feishu/WeChat BYOA proxies): `POST /integrations/{provider}/connect`, `GET /integrations`, `GET`/`DELETE /integrations/{id}` forward to Cloud `/api/v1/integrations/*` with the user's API key attached server-side (`integrationsCloudReady` gate + `writeCloudPassthrough`). Cloud owns the per-provider OAuth exchange; the daemon has no public URL for the callback. Cloud `connect` returns `{connection_id, oauth_url}` the renderer opens. **The kocoro agent never calls these** (OAuth needs a browser round-trip), so — like the Slack BYOA proxy — they are NOT in the kocoro skill references. |
| Integration tools (local agent) | `tools/register.go RegisterIntegrationTools` + `tools/server.go NewIntegrationTool` + `client/gateway.go` | The local agent loop (`daemon.RunAgent`) does NOT go through Cloud's orchestrator, so Cloud's request-time integration-tool injection never reaches it — the daemon must register the tools itself. `RegisterIntegrationTools` fetches the caller's active integration tool schemas from Cloud `GET /api/v1/integrations/tools` (X-API-Key, **no local allowlist** — Cloud already filters by active connections + whitelist; local tool names still win on collision) and registers each as a `NewIntegrationTool` (a `ServerTool` variant: `ToolSource()==SourceIntegration`, grouped with gateway tools in both partitioners, `RequiresApproval()==false` — Cloud enforces access control). Execution proxies to Cloud `POST /api/v1/integrations/tools/{name}/execute` (X-API-Key) via `GatewayClient.ExecuteIntegrationTool`, reusing `ServerTool`'s error classification / usage / ladder. Registered on startup + `/config/reload` (alongside `RegisterServerTools`), refreshed by `RebuildAuthSensitiveTools` (sign-in `OnAPIKeyChanged`), and — the immediate path — by **`POST /integrations/refresh`** (`Server.RefreshIntegrationTools` → `RegisterIntegrationTools` only; lightweight, does NOT restart MCP like `/config/reload`). **First-time activation is async** (a connection goes active only after the browser OAuth completes, out of band from the daemon), so `connect`/`delete` fire a best-effort refresh but the reliable trigger is Desktop calling `POST /integrations/refresh` once it confirms the connection is active (Desktop already knows — it renders "已连接"). Capability token `integration_tools_v1`. Requires the two Cloud X-API-Key endpoints above (distinct from the internal `X-Internal-Token` `/internal/integrations/call`). |
| Attachments | `attachment.go` | Priority `document_b64` → `extracted_text` → URL download. Caps: 500 MB/file, 20/msg, inline doc ≤ 25 MB raw. Capability tokens `inline_document_b64`/`inline_extracted_text` gate these fields. DOCX/XLSX/PPTX/CSV extraction is daemon-local (`doc_extract.go`); Cloud fills PDF `DocumentB64` + transcodes HEIC/AVIF. |
| Session routing | `router.go` | `ComputeRouteKey` precedence: `PinnedRouteKey` (sticky schedule) → `session:<id>` → thread → sender → plain `agent:<name>` (only when NOT `new_session`) → channel. Web/webhook/cron bypass (always fresh). **Named agents are multi-session** (honor `session_id`/`new_session` like the default agent); the plain `agent:<name>` lane resolves to the latest `kind=interactive` session via `Manager.ResumeLatestMatching(isInteractiveSource)` (never a schedule/IM session). **Schedule `Stateful`** is the single remember-across-runs switch (`schedule.IsSticky`): `false`/legacy-nil → fresh session each run (`NewSession`, `OmitHistory`); `true` → dedicated `agent:<name>:schedule:<id>` / `schedule:<id>` session that accumulates, pinned via `PinnedRouteKey`, with the LLM seeing its history. **Heartbeat** reads/appends the latest `kind=interactive` session (`ResolveLatestSession`/`AppendToSession`); a mid-run session switch hits `ErrSessionChanged` and is dropped silently. |
| IM connection awareness | `connection_state_cache.go` + `message_origin.go` | Per-platform connection state from Cloud `channel_state_event`s, rendered as a `Connection:` Session-Facts line + new-session `Preamble()`. **Binding axis** (install/token revoked — actionable) is stored SEPARATELY from **transport axis** (transient disconnect) so a transport blip can't mask a revocation; binding wins at render, `Preamble()` is sorted for byte-stable prompts. On existing sessions whose blob lacks a chat_id (`origin==nil`), `stickyFromRequest` still surfaces state via `PlatformLine(source)`. |
| Smart session titles | `runner.go fireTitleAfterRun` + `internal/context/title_gen.go` | Async small-tier title upgrade at completed turns {1,3} on `TitleAuto` sessions. **Skipped for autonomous local sources** (watcher/heartbeat/mcp via `isAutonomousLocalSource`) so they never relabel the user's interactive session. `DecorateTitle`/`SourceLabel` prefix the brand (`Slack · …`); image+caption user turns keep the caption in the title transcript. |
| Session share uploads | `daemon/share_handler.go` + `share_async.go` | Render HTML → POST `/api/v1/uploads` with `kind=session_share` (post-upload LIST filters by that kind so concurrent uploads can't bump our row off page 1). publish_to_web uses `kind=other`. Template `<head>` carries OG / Twitter Card / JSON-LD (`internal/share/social_meta.go`). Config: `daemon.share_metadata.{site_name, site_url, default_og_image, twitter_image, logo_url}`. Tool runs are stripped from the page (only prose + images survive); `html-artifact` fences render live in a sandboxed iframe (`internal/share/artifact.go`, **assistant-role messages only** — user/third-party text stays inert escaped markdown to avoid stored-XSS). **The artifact host CSS (`internal/share/templates/artifact_host.css`), the CSP, and the resize bridge are VERBATIM mirrors of `ARTIFACT_HOST_CSS` / `ARTIFACT_CSP` / `buildArtifactSrcdoc` in Kocoro Desktop's `message-list.js` — re-sync when Desktop's artifact design system changes (no cross-repo automated check).** |
| Output format | `runner.go outputFormatForSource` | `plain` for cloud-distributed channels (slack/line/wecom/wechat/telegram/webhook); `markdown` default. **Feishu/Lark/Teams are cloud sources but use `markdown`** (`markdownCloudSources`) — Feishu/Lark cards render standard markdown, and GFM output re-enables Cloud's `[name](url)` → file-attachment conversion (plain raw URLs are never converted); Teams' Bot Framework message body renders markdown incl. tables, so it must stay `markdown` even though it's in `cloudSourceSet`. **WeChat (iLink) stays `plain`** — Cloud's iLink outbound sends native images/files by extracting **raw** CDN URLs from the plain text (shannon-cloud `wechat_streamer.go`/`cdn_images.go`); do NOT move it to `markdownCloudSources`, since a markdown link `[name](url)` would leave a `[name]()` shell after Cloud strips the URL (its cleanup only removes `![]()` image shells + `<>` autolinks, never `[]()` links). |
| Tool result sizing | `spill.go` + `toolresult_budget.go` + `context_bloat.go` | Per-result spill at policy threshold (default 50K, grep 20K) → tmp file + 2K preview. `file_read` is `UnlimitedToolResultSizeChars` (no spill); it self-bounds via `fileReadHardCapRunes = 500_000` in the tool itself, with truncation marker. Per-turn 200K-rune aggregate cap skips Unlimited tools. `ToolResultReplacements` + `ToolResultSeen` persisted across checkpoints AND terminal saves. |
| file_read dedup | `agent/readtracker.go` + `daemon/readtracker_cache.go` | Records `(path, offset, limit, mtime, size)`; re-reads return a stub. Per-session, released via `SessionManager.OnSessionClose`. |
| Image size guard | `imaging_compress.go` + `oversize_image.go` | Three layers: source-time compression (`EncodeImage` decode→2000×2000→JPEG ladder), wire-time sanitizer (`filterOversizeImages` in `messagesForLLM`), persist-time guard (`SanitizedRunMessages`). |
| Skill secrets | `skills/secrets.go` | Keychain `com.shannon.skill.<name>` + plaintext index of key NAMES only. Env-var-only injection, scoped to skills activated by `use_skill` in the current run. |
| Skill marketplace sources | `daemon/server.go` (`s.marketplace` / `s.clawhub`) + `config.MarketplaceConfig` | TWO independent API surfaces, never share a response shape. `/skills/marketplace/*` = static registry (`registry_url`), integer `page` pagination, `{total,page,size,skills}` — **this is the frozen macOS Desktop contract; do not add source-conditional branches here.** `/skills/clawhub/*` = ClawHub live catalog (`clawhub_url`, default `https://clawhub.ai`), opaque `cursor` pagination (`{skills,size,next_cursor}`), plus per-version `/files` + `/file` browsing and `/install/{slug}` via deterministic zip URL. Both back the same `MarketplaceClient` (mode set by constructor) and install to the same on-disk location. **Transient resilience**: every catalog GET (`fetch`/`getJSON`/`getText`) and the zip install download go through `doGETWithRetry` (`marketplace_retry.go`) — retries 429/5xx + network errors with exponential backoff + jitter (honors a numeric `Retry-After`), tuned by `skills.marketplace.max_attempts`/`.retry_base_backoff_secs`. The helper returns the final response on exhaustion so each caller's `status %d` error (and the daemon's 404-vs-503 split) is preserved; 4xx is never retried; the static-registry stale-cache fallback still applies after retries. The install zip download is single-attempt (2-min client; retrying would multiply a hang). **ClawHub reads** additionally have a short-TTL per-URL response cache (`marketplace_cache.go`, `skills.marketplace.clawhub_cache_ttl_secs` default 60s, bounded entries, serve-stale-on-error with cooldown) so burst/repeat browsing doesn't re-hit clawhub.ai; the static registry caches at the `Load()` layer instead. **Default-view resilience** (kills the intermittent "注册表不可达" 503 on the default tab): the daemon's `warmClawHubOnce` (`server.go`, launched from `Start`, gated by `skills.marketplace.clawhub_warm_on_startup` default true) warms the canonical default browse page (`size=20&sort=downloads`, no cursor/query) ONCE at startup — deliberately one-shot, not a poll loop, so an air-gapped / IM-only daemon makes at most one clawhub.ai request (the `viper.GetBool` default is false when `config.Load` hasn't run, so daemon unit tests stay hermetic). `MarketplaceClient` keeps a **view-agnostic last-good first page** (`firstPage`, guarded by `firstPageMu`, max age `clawhubFirstPageMaxAge` 30min): any successful no-query/no-cursor browse is retained and served (single stale page, empty `next_cursor`, handler sets `X-Cache-Stale: true`) when a fresh default first-page fetch fails **for a transient reason** (`isTransientListErr` mirrors `isRetryableStatus`: only 429/5xx + network/parse are transient; every other status — 400/401/403/404/409/410/422/… — surfaces immediately) while its exact-URL cache is cold — so a `size`/`sort` the warm didn't prefetch still degrades to a populated list, not a 503. Deep pages (`cursor` set) / searches (`q` set) have no fallback; a hard-down clawhub before the first successful warm still 503s. **Exclude-installed** (`GET /skills/clawhub?exclude_installed=true`, capability `clawhub_exclude_installed`): opt-in, ClawHub-only (never the frozen `/skills/marketplace/*`). "Installed" is a local-only notion clawhub can't filter on, so `FetchClawHubPageExcludingInstalled` fetches normally then drops installed slugs, **refilling from later pages** bounded by `skills.marketplace.clawhub_exclude_fill_max_pages` (default 5). Cursor is page-granular → a returned page may exceed `size` (last page included whole) and `next_cursor` stays page-aligned (no dup/loss on resume); if the fill cap binds, the page is short/empty with a non-empty cursor and the client must keep paging. |
| Turn phase tracker | `agent/phase.go` | Only `PhaseAwaitingLLM` and `PhaseForceStop` idle-counted. Fail-closed: panics under `testing.Testing()` or `SHANNON_PHASE_STRICT=1`. |
| Idle watchdog | `agent/watchdog.go` + `client/gateway.go` | Two layers. Turn-elapsed: `OnRunStatus("idle_soft")` at `agent.idle_soft_timeout_secs` (default 90), `ctx.Cancel(ErrHardIdleTimeout)` at hard (default 540; opt out via `0` + startup WARN). Streaming chunk-gap: `CompleteStream` returns `ErrStreamIdleTimeout` if no SSE chunk arrives within `agent.stream_idle_timeout_secs` (default 90). The loop short-circuits the streaming→Complete fallback on `ErrStreamIdleTimeout` and `isRetryableLLMError` refuses to retry it. `completeWithRetry` prefers `context.Cause(ctx)`. |
| Mid-turn checkpoint | runner `applyTurn*` helpers | Fires at three phase-exit boundaries; 2s debounce. Same helpers run from checkpoint, final save, and hard-error save. `session.InProgress` non-zero on reload = crash recovery. |
| Schedule proactive push | `scheduler.go` `broadcastReply` + `broadcast_gate.go` `shouldBroadcast` / `resolveThread` | After a successful run, push is gated by `shouldBroadcast`: explicit `Schedule.Broadcast *bool` wins; else smart default by `CreatedFromSource` (IM sources → push, else silent). **Origin-only delivery**: the push target is always the schedule's snapshotted `IMStatusContext` blob (the channel it was created in); a schedule with no blob (Desktop/TUI/CLI/webhook-created, or IM schedules predating v0.2.1) NEVER pushes — even with `broadcast=on` — wrong-audience delivery beats no delivery, results stay in the session. Cloud mirrors this: `RouteProactiveTo` drops (never re-broadcasts) a non-empty blob it cannot honor; only empty-blob proactives (heartbeat, legacy daemons) still broadcast. Tools `schedule_create` / `_update` and `PATCH /schedules/{id}` accept `broadcast: "auto"\|"on"\|"off"` (capability token `schedule_broadcast_gate`). **Thread three-state** (`Schedule.Thread *bool`, parsed via `ParseThreadEnum`, mirrors broadcast): `thread: "auto"\|"on"\|"off"`. `auto` follows session state — stateful (sticky) → one session = one thread; stateless → a fresh top-level message each run. `resolveThread(thread, isSticky, hasBlob)` → `ProactivePayload.UseThread *bool`: explicit `on`/`off` verbatim (ignores stateful); `auto` → `isSticky && hasBlob`. `nil` = anchored thread (current behavior); only `&false` goes top-level. Threadless platforms (LINE/WeCom/Telegram) ignore it. Capability token `proactive_thread_mode` (observability only). |
| Playwright file:// bridge | `tools/filepreview.go` | Loopback HTTP rewrites `browser_navigate(file://…)`. Symlink-resolved allowlist; loopback-only `r.RemoteAddr` check. |
| Session sync | `internal/sync/` | Daily upload (opt-in `sync.enabled`). flock + atomic marker. Permanent failure reasons (`size_limit_exceeded`, `load_error`) self-heal on session edit. |
| Memory client | `internal/memory/` | Daemon owns sidecar lifecycle + 24h bundle pull. `memory_recall` → `Service.Query` over UDS; falls back to `session_search` + MEMORY.md when not `Ready`. API key never on disk — only `sha256[:16]` fingerprint. Schema-mismatch lockout surfaces as `memory.reason=tlm_binary_too_old` on `GET /status` and triggers a one-shot self-heal pull before degrading. `Sidecar.Shutdown` is idempotent, so failed children don't accumulate as orphans. |
| Episodic preflight | `agent/preflight.go` + `tools/memory_preflight.go` | Small-tier helper compiles `QueryIntent`s before first main call; `<private_memory>` injected into in-flight user message, NEVER persisted to transcript, stripped from compaction inputs. Audit row content-free. Records render a `[strength=<tier>]` marker from the sidecar's `evidence_tier` (missing/unknown → `unrated`, treated as weak); answer-fidelity rules shared with memory_recall + system prompt via `prompt.MemoryEvidenceGuidance`. `SHANNON_PREFLIGHT_DUMP=1` (opt-in debug) appends each injected block to `logs/preflight_dump.jsonl` (0600) — the only way to attribute dropped-fact incidents, since the block is otherwise unrecoverable. |
| Loop detector | `agent/loopdetect.go` | 9 detectors. `dupExemptTools` skip dup detection; all-errors 2× budget; rolling nudge window (max 3 within trailing 5). |
| Empty-think force-stop | `loopdetect.go` rule "0a" | Two consecutive `think({})` → `LoopForceStop`. Defends against ritual empty think after native interleaved thinking. |
| Thinking blocks | `client.ContentBlock` + `agent.buildAssistantMessage` | Cloud relays full ordered `content_blocks` incl. `thinking`/`redacted_thinking`. Persisted verbatim; `internal/sync/strip_thinking.go` removes from upload-side copy before size check. Sanitizers in `messagesForLLM` / time-based / micro-compact / `BuildForkedRequest` preserve them. |
| Conditional `think` tool | `tools/register.go shouldRegisterThinkTool` | Not registered on default gateway+thinking path. Still registered when thinking disabled, Ollama provider, or `ForceThinkTool=true`. `operationalRules()` strips `### Planning` bullet only when think absent, keeping prompt byte-equal otherwise. |
| Prompt suggestion | `agent/suggestion.go` + `daemon/runner.go` | Forked LLM call after each main turn. **CACHE SAFETY**: byte-equal to main request except 2 appended messages + `SkipCacheWrite: true`. Any other divergence fragments the cache. **Source-gated** (`wantsPromptSuggestion` allow-list): only foreground sources with a UI consumer — `desktop` (the Desktop message bridge POST /message hardcodes it), `kocoro` (handler backfill when Source omitted), `shanclaw` (legacy Desktop alias, one release), `web` — fork a suggestion. IM channels, schedule/cron, and autonomous local sources (heartbeat/watcher/mcp) are skipped (no consumer → dead work + a billed call). Allow-list, not deny-list: new background sources default to skipped. |
| Email/password auth (macOS + Windows + Linux) | `internal/daemon/auth.go` + `auth_handlers.go` + `ws_controller.go` + `internal/keychain/` | `/local/auth/*` proxy to Cloud `/api/v1/auth/*`. AuthManager state machine drives WS lifecycle — WS runs only in `signed_in`. api_key is the source-of-truth credential in the credential store (`ai.kocoro.daemon.api_key/<user_id>`); the yaml field is migrated away on first launch (with a read-back verify before stripping yaml). The supported-platform set is `keychain.Supported()` (darwin \|\| windows \|\| linux) — it MUST stay in sync with the backend build tags (enforced by `TestSupportedMatchesBuildTag`). On unsupported platforms (anything else): AuthManager nil, endpoints 503 `platform_unsupported`, legacy `cfg.APIKey` path. **Linux uses a file store (`~/.shannon/credentials.json`, 0600), NOT go-keyring's Secret Service** — SS returns success at construction but fails on headless hosts, which would strand the key mid-migration; the file backend is deterministic everywhere. |

### Daemon Approval Protocol

- **Interactive** (default): approval round-trips over WS to Ptfrog.
- **Auto-approve** (`daemon.auto_approve` or per-agent): skips the WS round-trip except for unattended-deny-listed tools such as `computer_use`; the permission engine remains enforced.
- Synchronous HTTP API handlers auto-approve (localhost-only) except for unattended-deny-listed tools.

"Always Allow" goes through `alwaysallow.go HandleAlwaysAllowDecision` — single entry point shared by SSE and WS so transports can't drift. Persistence matrix:

| Tool | Agent | Persistence | Notes |
|---|---|---|---|
| bash, always-ask command | any | none | One-time allow + `EventApprovalNotice` warning. Runtime gate in `loop.go` enforces denylist even if hand-written into config. |
| bash, safe command | named | per-agent `permissions.always_allow_tools` | Future bash from this agent skips approval. |
| bash, safe command | default (`req.Agent==""`) | GLOBAL `permissions.always_allow_tools` | Affects all agents. PR 6 fix for non-technical users on default agent. |
| non-bash | named | per-agent tool-level | `agent.DisallowsAutoApproval` (currently empty) refuses persistence + emits warn notice. See `internal/agent/tools.go` for trade-off rationale. |
| non-bash | default | global tool-level | Same path bash takes. SSE handler creates fresh broker per request, so broker-only persistence evaporates. |

Global and per-agent always-allow lists are **unioned at injection** in `SetAlwaysAllowTools` (called from runner.go / tui/app.go / cmd/root.go after `SwitchAgent`). `SwitchAgent` resets the field so reuse can't leak.

**Two auto-approval deny-lists**: `agent.DisallowsAutoApproval` (refuses "always allow" persistence — currently empty) and `agent.DisallowsUnattendedAutoApproval` (refuses unattended auto-approval — contains `computer_use` since 2026-07-22: a background run must not invoke this unified GUI tool on the strength of an attended consent). Legacy GUI tools stay off the list deliberately so existing unattended applescript/accessibility schedules keep working; the restriction is therefore specific to `computer_use`, not a claim that background runs cannot use any GUI tool. The unattended gate is enforced twice: at every unattended handler's `OnApprovalNeeded` AND in `loop.go checkPermissionAndApproval`, where an unattended run (`SetUnattendedRun`, fed by `runner.go isUnattendedRun` from source classification or a no-broker transport such as synchronous HTTP) skips BOTH the persisted always-allow bypass AND the SafeChecker observation exemption for deny-listed tools so the request actually reaches the handler gate — without the latter, approval-free observation actions (e.g. `computer_use` screenshot) would run unattended without the deny-list ever being consulted. A blanket `auto_approve` cannot approve `computer_use`; an attended Desktop/IM approval round-trip and attended Always Allow continue to work normally. `approval_request.flags` carries `always_allow_disabled` for tools on the first list (none today).

**`EventApprovalNotice`** payload is `{severity, code, tool, message}`. `code` is a stable i18n key (`high_risk_not_persistable` / `bash_always_ask_not_persisted` / `persist_failed`); daemon NEVER ships translated text. `message` is English fallback.

**Approval-card `description` field**: every tool whose `RequiresApproval()` returns true requires a `description` field (5-15 words, user-facing intent, model writes it, daemon passes through). UI clients render it prominently; raw args behind a toggle. Spec in `internal/agent/approval_description.go`. Exemptions: `bash` keeps its bespoke schema (cache-stability), `computer` is a native tool (Parameters not wire-transmitted — UI synthesizes from action/x/y), `publish_to_web` declares both `description` and `purpose`. Daemon does NOT block on missing/empty `description`; UI must fall back to tool-specific args using `description?.trim() || fallback` (NOT nullish coalescing).

### Wire Contract Discipline (daemon ↔ UI clients)

- **Wire fixtures**: canonical JSON for every payload UI clients decode lives in `docs/desktop-wire-fixtures/` (bus event payloads, per-request SSE payloads, HTTP response bodies — see its README for surface framing). `internal/daemon/wire_fixtures_test.go` emits each through the REAL producer path (event emitters / full `Handler()` router), semantic-compares against the fixture (never byte-equal), and decodes the produced bytes into consumer-shaped structs. Any payload-shape change updates fixture + test in the same PR; the Desktop side mirrors with decode tests over the same fixture files.
- **Capability token minting**: every cross-version contract change a UI client must detect mints a token in `Capabilities` (`internal/daemon/client.go`), surfaced on BOTH the WS handshake (`X-Kocoro-Capabilities`) and `GET /status` (`capabilities` array). Clients gate features on tokens — never version sniffing or decode-failure probing. Historical trap this kills: `display_name` / `model_tier` shipped as HTTP contract changes with no token, so partially-deployed Desktop/daemon pairs half-rendered. The `/status` fixture pins the full token list, so minting is enforced mechanically by the fixture test.
- **New event families**: session-scoped events stay flat (`tool_status`, `approval_request`, …). A new domain (e.g. a hypothetical hardware-device integration) uses dotted types (`device.status`, `device.action_request`) with a common envelope — `type`, `ts`, plus `session_id` (session-scoped) or `target_id` (device-scoped) — and the domain payload nested under its own keys. Never repurpose an existing type's fields; additive only. Full rules in the kocoro skill `references/events.md`.
- **Request/resolve interactions**: `ApprovalBroker` is today's only request-lifecycle mechanism (at-most-one terminal event, bus-ID ordering — see `approval.go` doc comments). If a second interaction kind lands (user questions, device action requests), factor the broker into one pending-interaction core with multiple wire faces — do NOT copy it. A `question` event was deliberately deferred on 2026-06-12: the agent loop has no native ask-user semantics, and adding one purely for UI symmetry inverts the dependency.

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
- Daemon api_key (macOS + Windows + Linux): credential store service `ai.kocoro.daemon.api_key`, account = Cloud user_id (UUID). Backend is macOS Keychain / Windows Credential Manager / Linux file store (`~/.shannon/credentials.json`, 0600, fslock-protected). Active user pointer at service `ai.kocoro.daemon.state`, account `current_user_id`. `cfg.APIKey` (yaml) is empty after the v1 migration; Bootstrap reads the credential store instead. On other platforms the credential store is unavailable and `cfg.APIKey` stays in yaml (legacy path). **Linux IaC caveat**: the migration moves `api_key` out of `config.yaml` into `credentials.json`; if `config.yaml` is config-managed (Ansible/Puppet) and re-adds `api_key`, the daemon's self-heal re-strips it each launch — document the key now lives in `credentials.json`
- Sync: marker `~/.shannon/sync_marker.json`, lock `~/.shannon/sync.lock` (never delete), dry-run outbox `~/.shannon/sync_outbox/`
- Logs: `~/.shannon/logs/audit.log`, `~/.shannon/logs/schedule-<id>.log`; `~/.shannon/logs/preflight_dump.jsonl` (only when `SHANNON_PREFLIGHT_DUMP=1`; private memory content, 0600, delete after debugging)
- Memory: socket `~/.shannon/memory.sock`, bundle root `~/.shannon/memory/`
- Desktop RPC (when daemon spawned by Kocoro Desktop): sock `~/Library/Application Support/run.shannon.shanclaw/daemon.sock` (0600) + pidfile `daemon.pid` in same dir (0700). Paths passed via `--rpc-socket` + `--rpc-pidfile` CLI flags; daemon never derives one from the other.

### Atomic Writes

`schedules.json` and `secrets-index.json` use write-to-temp + `os.Rename` + an exclusive lock (via `internal/fslock`, NOT raw `syscall.Flock` — see Cross-Platform Support) on a persistent `.lock` file. **Never delete the lock file** (causes lock race on different inodes). Atomic-rename targets are read lock-free (the rename is atomic, so readers always see a complete file); never hold a lock on the destination file itself — on Windows a mandatory `LockFileEx` would block the rename-over-open.

### Cross-Platform Support

The daemon cross-compiles to macOS / Linux / Windows (`CGO_ENABLED=0`). POSIX-only syscalls are confined behind build tags so the Windows build stays green:

- **File locking** → `internal/fslock` (`Lock`/`RLock`/`TryLock`/`Unlock`/`IsWouldBlock`): `lock_unix.go` wraps `flock(2)`, `lock_windows.go` wraps `LockFileEx`/`UnlockFileEx` (the only `golang.org/x/sys/windows` consumer). All lock call sites go through this — do NOT reintroduce raw `syscall.Flock` (breaks Windows).
- **Process-group kill** → per-package `*_proc_{unix,windows}.go` helpers (`internal/hooks`, `internal/tools` for bash, `internal/memory` for the sidecar; `internal/mcp/processgroup_{unix,windows}.go` is the original): POSIX `Setpgid` + `Kill(-pid)` vs Windows `CREATE_NEW_PROCESS_GROUP` + `taskkill /T /F`. Windows has no usable graceful step for console children (graceful `taskkill` no-ops), so the sidecar force-kills directly.
- **`shan daemon stop`** → `cmd/proc_signal_{unix,windows}.go` (`terminateDaemon`): POSIX SIGTERM vs Windows `taskkill`. HTTP `/shutdown` remains the cross-platform graceful primary; signal/taskkill is the PID-file fallback.
- **macOS-only GUI tools** (`computer_use`/`accessibility`/`applescript`/`clipboard`/`computer`/`screenshot`/`ghostty`) gate on `runtime.GOOS != "darwin"` and return a clean "only available on macOS" error elsewhere. `notify` is NOT gated — it has a cross-platform Desktop route; only its osascript fallback is darwin-gated.
- **Memory bundle `current` pointer** → `internal/memory/bundle_link_{unix,windows}.go` (`swapCurrent`): a symlink (atomic tmp+rename) on POSIX vs an unprivileged directory junction (`mklink /J`, remove+recreate) on Windows — `os.Symlink` would fail with ERROR_PRIVILEGE_NOT_HELD off Developer Mode. Both keep `current/<file>` transparently traversable by the `tlm` sidecar and resolvable by `os.Readlink` (`currentTs`).
- **Credential store (daemon api_key)** → three mutually-exclusive backends partitioning GOOS exactly: `backend_keyring.go` (`//go:build darwin || windows`, go-keyring → Keychain / Credential Manager), `backend_linux.go` (`//go:build linux`, file store via `backend_file.go`), `backend_other.go` (`//go:build !darwin && !windows && !linux`, `NewOSStoreAt` returns `ErrUnsupportedPlatform`). The constructor is `NewOSStoreAt(dir, logger)` — `dir` is the shannon dir (used by the Linux file backend, ignored by Keychain/wincred); callers pass `config.ShannonDir()` rather than letting keychain re-derive it (keychain importing config is a cycle). Runtime callers (config hydrate/save/migrate/setup, auth gating) MUST gate on `keychain.Supported()` (darwin\|\|windows\|\|linux — single source of truth, kept in sync with the build tags, enforced by `TestSupportedMatchesBuildTag`), never a raw `runtime.GOOS == "darwin"`. **Why Linux is a file store, not Secret Service**: go-keyring's SS/dbus backend returns success at construction then fails every read/write on headless hosts (no D-Bus), which would let the yaml→store migration strip the key then fail to persist it; the 0600 file is deterministic on desktop/Docker/SSH alike and is security-parity with the legacy yaml plaintext path (NOT GNOME-Keyring encryption — acceptable trade for headless support). The Linux build links no go-keyring/dbus at all. **Test-coverage note**: CI cross-compiles Windows (`go build`/`go vet`) but its unit suite runs on macOS/Linux — the live wincred round-trip is covered only by manual Windows E2E; the Linux file backend IS unit-tested (`backend_file_test.go`, tagless) and the invariant test is the automated backstop for the gate.
- **Known Windows gaps (not yet ported)**: `bash` runs `sh -c` and requires Git Bash/WSL on PATH (returns a clean error otherwise).

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
- **Deferred tool loading**: when count > 30, MCP/gateway tools sent as name+description; model calls `tool_search`. `web_search`/`web_fetch` are exempt (`neverDeferTools` in `toolbudget.go`) and always ship full schemas, on both the primary tool-ref path and the legacy path (`buildLocalActiveSchemas`).
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

Koe tests link cgo audio deps. On macOS, install them with `brew install opus opusfile pkg-config` and set `PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig` if pkg-config cannot find the Homebrew files.

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
- **macOS GUI**: computer_use (primary native-GUI workflow), accessibility (legacy low-level AX), applescript, screenshot, computer, clipboard, notify, browser, wait_for, ghostty. `computer_use` observations are approval-free; mutations use the normal approval/Always Allow policy. It reopens windowless running apps through LaunchServices, accepts decimal strings for integer tool arguments, supports condition-free bounded waits, and makes pointer actions visible by moving the real cursor. Its `state_id`, refs, and cached screen dimensions are cloned per run, while whole calls — including legacy `accessibility`, `computer`, and `applescript` calls, which acquire the same `computerUseGUIOperationMu` — share one GUI-operation lock so Desktop, iOS remote, Slack, and other concurrent routes neither share state nor interleave a stale-state check with another route's action. Screenshots are explicit-only. `computer_use` is on the unattended auto-approval deny-list (schedule/heartbeat/watcher/mcp, remote/SSE auto-approve, synchronous HTTP, and no-approval-UI IM/voice runs can never invoke it — observation actions included; the approval-free exemption applies only to attended runs).
- **Schedule**: schedule_create / _list / _update / _remove / _show
- **Memory**: memory_append (flock-protected MEMORY.md append)
- **Skills**: use_skill

Conditional:

- `session_search` — when session manager available
- `cloud_delegate` — `cloud.enabled: true`
- `publish_to_web` — `cloud.enabled` + `cfg.APIKey`. Always approval. Path-segment + basename blocklist (`.env`/`.pem`/…); extension allowlist (`cloud.publish_allowed_extensions`). All uploads tagged `kind=other` server-side; the kind enum (`session_share`/`report`/`landing_page`/`image`/`other` — see `internal/uploads/client.go`) is NOT exposed to the model.
- `list_my_published_files` — same gating. Read-only, no approval. `limit` (≤100), `offset`, optional `kind` filter (same enum). Returns paged `UploadEntry` rows keyed by id; rendering surfaces a `kind=…` badge per row so the LLM can answer "which of these are session shares".
- `retract_published_file` — same gating. Destructive, requires approval. Args: `id` (UUID from list) + `description`. 404 conflates not-found/already-retracted/not-yours to avoid existence leak.
- `generate_image` / `edit_image` — same gating. Always approval (paid quota + permanent CDN). Edit requires `image_urls` 1-4 entries starting with `https://static.kocoro.ai/`.
- `tool_search` — deferred mode when tool count > 30 (lives in `agent/deferred.go`)
- **`calendar_*` family (8 tools)** — registered only when daemon is a Kocoro Desktop subprocess (`tools.RegisterCalendarTools` no-ops when the `DesktopRPCBroker` is nil; TUI/one-shot/MCP/scheduled paths fall back to `applescript` + Calendar.app). Tools: `calendar_check_permission`, `calendar_request_permission` (approval, 5-min TCC-dialog timeout), `calendar_list_sources`, `calendar_list_events`, `calendar_get_event`, `calendar_create_event` / `_update_event` / `_delete_event` (approval). Backed by `docs/desktop-calendar-rpc.md` v0.5.1 (Unix socket reverse RPC to Desktop's EventKit). `attendees` is metadata-only — `invitations_sent` always `false` in v1. `update_event` rejects `scope=all`; use delete + create.
