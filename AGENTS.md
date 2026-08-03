# Kocoro Project Guide

## What This Is

Kocoro is the Go CLI/runtime for Shannon AI agents. The main production path is daemon + Kocoro Desktop + Shannon Cloud: the daemon connects to Cloud over WebSocket, receives channel messages, runs the agent loop locally with full tool access, and streams results back. It also supports interactive TUI, one-shot CLI, MCP server mode, and local scheduled tasks.

## Working Rules

- Use the Go version declared in go.mod as source of truth.
- Prefer existing repo patterns over new abstractions. Keep changes small and directly tied to the task.
- Verify API response bodies, not just status codes.
- Do not create parallel `_enhanced` variants; update existing code in place.
- For risky behavior changes, preserve operator-visible flags, rollback paths, and focused tests.
- When touching dependency or generated-code surfaces, test locally before pushing.
- When adding small hardcoded caps, document the workload, binding symptom, and override path. Prefer config defaults for caps power users may need to lift.

## Module Map

- cmd: CLI entry points for one-shot, daemon, scheduling, update, and MCP serve.
- internal/daemon: primary production path; HTTP API server, WebSocket client, routing, approvals + structured user questions (`pending.go` `pendingCore[D]` shared by `ApprovalBroker` and `QuestionBroker`), events, launchd, attachments, session CWD, memory fallback, suggestions, Desktop skill-install recommendations (`skill_recommendation.go`), email/password auth (`auth.go` / `auth_handlers.go` / `ws_controller.go`), Desktop RPC reverse-channel (`desktop_rpc/` subpackage — Unix sock listener + length-prefixed JSON codec + DesktopRPCBroker for Calendar RPC v1).
- internal/agent: core loop; tool batching, compaction, spill/budget state, deferred loading, state cache, read tracking, approvals, phase/watchdog, thinking handling, prompt suggestions, forked requests.
- internal/koe: macOS native speech-to-speech voice front brain; one first-person Kocoro identity; Realtime tool authority; bounded continuation; call-scoped task ledger with one `do_task` per response by default and explicit, disjoint scopes for parallel calls; asynchronous result mailbox; and reversible raw-audio native-floor barge-in. Keep `stop_speaking` (current output), `cancel` (delegated work), and terminal `end_call` (voice session) as separate authorities. ASR transcripts remain asynchronous evidence/logging, not ordinary-turn, barge-in-admission, or default dismissal control.
- internal/tools: local, gateway, cloud, schedule, publish/upload, image, memory, MCP, and document tools.
- internal/keychain: credential store wrapper for daemon api_key — macOS Keychain / Windows Credential Manager via go-keyring (backend_keyring.go), Linux file store at ~/.shannon/credentials.json 0600 (backend_linux.go + backend_file.go); `keychain.Supported()` = darwin||windows||linux, other platforms return ErrUnsupportedPlatform. Constructor `NewOSStoreAt(dir, logger)`; callers pass config.ShannonDir().
- internal/client: gateway/SSE/Ollama clients plus AuthClient (`/api/v1/auth/*` REST wrapper).
- internal/session: session persistence, lifecycle, titles, and SQLite FTS index.
- internal/config: config loading, merging, settings, and setup.
- internal/skills: skill registry, bundled skills, marketplace install, provenance, secrets, validation. Marketplace has two independent API surfaces: `/skills/marketplace/*` (static registry, integer-page pagination — the frozen macOS Desktop contract) and `/skills/clawhub/*` (ClawHub live catalog, opaque cursor pagination + per-version file browsing).
- internal/memory: sidecar client/supervisor, bundle puller, tenant safety, audit, service orchestration.
- internal/sync: opt-in session sync; locking, markers, scanning, batching, upload, backoff.
- internal/mcp: MCP client/server and browser profile lifecycle.
- internal/permissions: command permission model and safety checks.
- internal/runstatus, audit, hooks, prompt, instructions, context, schedule, tui, update: supporting runtime surfaces.
- test/e2e contains offline and live E2E coverage.

## Critical Invariants

### Kocoro Skill Docs

The bundled `kocoro` skill is the AI-facing source of truth for daemon HTTP APIs, config fields, and workflows. Any new daemon endpoint, endpoint behavior change, config surface, or user-facing workflow must update the matching bundled skill reference in the same change. Missing references cause agents to invent API workarounds.

### Builtin Skills

Bundled skills are overlaid into the user skill directory on startup and user edits are overwritten. Fork under a new skill name to customize. The hidden generative UI skill emits `html-artifact` blocks for Desktop's sandboxed WKWebView; session-share pages render the same blocks in a sandboxed iframe via `internal/share/artifact.go` (host CSS/CSP/bridge mirrored verbatim from Desktop — see CLAUDE.md). Shared pages strip tool runs (prose + images only).

`internal/daemon/skill_filter.go` maintains a `desktopOnlySkills` registry. Daemon filters these out of the per-request skill list when `req.Source` is a cloud-distributed channel (Feishu / Lark / WeCom / Slack / LINE / Telegram / webhook), keeping the use_skill tool registry, scaffolded listing, and semantic discovery consistent. Filter is applied once on the producer side immediately after `LoadGlobalSkills` so all consumers see the same view. Drift test (`skill_filter_test.go`) walks the `desktopOnlySkills × cloudSourceSet` cross product.

### Agent Names

Agent names must match `^[a-z0-9][a-z0-9_-]{0,63}$`. Validate before path construction.

### Tool Registry

Tool priority is local tools, then MCP tools, then gateway tools. Deduplicate by name. Skill `allowed-tools` is enforced at execution time, not by schema filtering, to keep prompt-cache tool arrays stable.

MCP tool dispatch is resilience-guarded: a supervisor-known-disconnected server is probed and reconnected before the call is sent (never dispatch onto a known-dead connection), every tools/call attempt is bounded by per-server `tool_timeout_secs` falling back to `mcp.tool_timeout_secs` (default 300s), and a failed call probes fresh health and retries once. Artifact paths from file-producing MCP servers are made unambiguous on both sides: results from servers with known path semantics (playwright, or any server with `workspace_base`) get "Saved to:" absolute-path annotations for files that verifiably exist, and screenshot output filenames default into the per-session artifact scratch dir (`~/.shannon/tmp/sessions/<id>/`, created lazily, swept by age at daemon startup) on daemon-served runs so intermediates stay out of user-visible folders. browser_snapshot never receives a default filename — its filename is a mode switch and omitting it returns the accessibility snapshot inline, the model's primary page-reading channel.

Skill-exempt tools must be pure infrastructure with no external side effects. Do not exempt side-effecting tools from skill restrictions.

Tool exposure resolves per tool: explicit `ToolExposure` first, then source
defaults. Local tools default Direct; MCP, gateway, and integration tools
default Deferred. `tool_search` discovers cold Deferred tools through
deterministic BM25 over canonical name, description, schema, source, and
namespace metadata, with exact `select:` lookup for known names. Schema-size
budgets are regression diagnostics only and must never reclassify tools at
runtime.

### Tool Concurrency

The dispatcher batches tool calls by `IsConcurrencySafeCall`, not `IsReadOnlyCall`. Tools without an explicit `ConcurrencySafeChecker` implementation fall back to their `IsReadOnlyCall` value — so file_read / grep / glob etc. keep batching concurrently as before, and writers stay serial. Adding the new interface to one tool has no effect on any other tool's grouping.

`BashTool` implements `IsConcurrencySafeCall` via `internal/tools/bash_concurrency.go`. It is gated by `agent.bash_concurrency_enabled` (default `true` in Phase C — Desktop consumes the `tool_use_id_events` capability so id-keyed cards stay correct under concurrent batches). When the flag is on, commands whose first token is in a strict read-only whitelist AND contain no shell metacharacters (including `\n` / `\r`) are eligible for the concurrent batch. Everything else — `&&` / pipes / redirects / command substitution, plus any non-whitelisted leading token (`git push`, `npm install`, `curl`, `rm`, `git remote add`, `go env -w`, ...) — stays in a size-1 serial batch.

Tool events on the SSE/WS wire (`tool_status` running + completed) include a `tool_use_id` field so multi-tool-in-flight UIs (e.g. parallel bash) can pair them correctly. The daemon advertises this on the WS handshake via the `tool_use_id_events` capability token.

### Tool Required-Field Validation

Every tool's `Run()` MUST explicitly check that each field listed in `ToolInfo.Required` is non-zero immediately after `json.Unmarshal`, and return `agent.ValidationError(...)` (NOT a bare `ToolResult{Content: ..., IsError: true}`) on failure. Go's json decoder cannot distinguish "field missing" from "field present with zero value" on a strongly-typed struct, so a missing required string arrives as `""` and downstream syscalls happily accept it. The 2026-05-13 production stuck loop was a `file_write` with no `content` that wrote 0 bytes, returned `IsError=false`, truncated the user's file, and trapped the model into a 16-call retry spin.

The `[validation error]` prefix that `ValidationError` injects is load-bearing: `LoopDetector.isValidationErrorSig` short-circuits a same-tool + same-args + 3-consecutive `[validation error]` run to `LoopForceStop`, well below the all-errors 2x ConsecutiveDup budget at call #7.

### Providers

The default provider is the Cloud gateway client. Ollama uses an OpenAI-compatible local client. Both implement complete and streaming completion paths; keep provider-specific behavior behind those interfaces.

### Permissions

Permission ordering is load-bearing:

```text
hard-block constants -> denied commands -> compound splitting -> always-ask gates -> allowed commands -> default safe -> approval + safe checker
```

Always-ask runs before allowlists. High-risk prefixes and destructive git/rm patterns cannot be silenced by config alone.

Always Allow uses one shared decision path for streaming and WebSocket approval flows. High-risk paid/public tools must refuse persisted auto-approval. Always-ask shell commands may be allowed once, but that decision must not be persisted.

### Daemon Routing

The daemon is the production integration point for Cloud channels. Route precedence is explicit session, threaded route, per-sender route, agent route, then legacy channel route. Routed managers are long-lived; bypass/heartbeat paths use short-lived managers. Tool status `running` is emitted only when execution actually starts.

Capabilities are advertised during the WebSocket handshake. Add a capability token in the same PR that ships an optional protocol feature. `delivery_ack` means the daemon acknowledges an inbound message only after reply delivery succeeds, so Cloud can safely drop it from replay.

Schedule proactive push has two independent three-state knobs (`auto`/`on`/`off`, set via natural language through `schedule_create`/`schedule_update`). `broadcast` gates whether a successful run pushes to IM at all; the push target is always the channel the schedule was created in (its snapshotted `im_status_context` blob) — schedules created outside an IM chat have no target and never push, even with `broadcast=on`. `thread` controls IM thread anchoring: `auto` follows session state (stateful → one thread, stateless → each run top-level), `on`/`off` force it. The daemon resolves `thread` to `ProactivePayload.UseThread *bool` and sends it to Cloud; `nil` means current behavior (anchored thread) and only an explicit off goes top-level. Platforms without threads ignore it. Capability tokens `schedule_broadcast_gate` and `proactive_thread_mode` are observability only.

### Output Profiles

Use output profiles rather than per-channel syntax. `markdown` is default; `plain` is for Cloud-distributed channels where Cloud owns final rendering.

### Tool Result Budgeting

Three layers protect context:

- Single large results spill to disk and are replaced with previews.
- Per-turn aggregate output spills the largest results until under budget.
- Persisted replacement/seen maps survive turns, final saves, and crash recovery.

Keep bloat run-status events, read-before-edit tracking, and same-range file-read dedup working when changing tool execution or persistence.

### Images And Attachments

Image size safety has source-time compression, wire-time sanitization, and persist-time guards. Any new image/attachment path must pass through the same protection before reaching the LLM or session JSON.

### Turn Lifecycle

The agent loop exposes explicit phases. Only LLM-wait and force-stop phases count as idle for the watchdog. Mid-turn checkpoints run after tool batches, reactive compaction, and before force-stop; final save rebuilds from the same baseline so turns are not double-persisted.

Interrupted turns auto-resume at daemon start (newest first, serial) but only within the `agent.interrupted_resume_max_age_hours` staleness window (default 4h) — older checkpoints are abandoned, never executed. Recovered runs always classify as unattended so the unattended tool deny-list applies, and they pin the session's original route key so concurrent inbound traffic for the same session is serialized. `agent.interrupted_resume_enabled: false` disables auto-resume entirely.

### Thinking Blocks

When native thinking is enabled, preserve assistant `thinking` and `redacted_thinking` content blocks in order across the conversation trajectory. Sanitizers, compaction, fork builders, and session persistence must not rewrite them. Strip thinking only before session sync upload.

The local `think` tool is skipped on the default gateway + native-thinking path, but remains available when thinking is disabled, with Ollama, or with the force flag.

### Browser Preview Bridge

File previews served to browser automation must stay fail-closed: allow only effective session CWD and attached paths, resolve symlinks on both sides, use random tokens, avoid directory listing, and tear down on session close.

### Config Merge

Global config, project config, and local project config merge in that order. Scalars override, lists merge/dedup, structs merge field-by-field. MCP server env var casing is preserved by direct YAML re-read.

### Atomic Writes

Persistent JSON indexes use write-temp, rename, and an exclusive lock (via `internal/fslock`, not raw `syscall.Flock`) on a stable lock file. Never delete lock files; doing so can split locks across different inodes. Rename targets are read lock-free (the rename is atomic), and never locked directly — a mandatory `LockFileEx` on the destination would block the rename-over-open on Windows.

### Cross-Platform Support

The daemon cross-compiles to macOS / Linux / Windows with `CGO_ENABLED=0`. POSIX-only syscalls live behind build tags: file locking goes through `internal/fslock` (`flock` vs `LockFileEx`); process-group kill uses per-package `*_proc_{unix,windows}.go` helpers (`Setpgid`+`Kill(-pid)` vs `CREATE_NEW_PROCESS_GROUP`+`taskkill /T /F`); `shan daemon stop` uses `cmd/proc_signal_{unix,windows}.go`; the memory bundle `current` pointer uses `internal/memory/bundle_link_{unix,windows}.go` (symlink vs unprivileged directory junction); the daemon api_key credential store uses `internal/keychain/backend_keyring.go` (`darwin || windows`, go-keyring → Keychain / Credential Manager), `backend_linux.go` (`linux`, file store via `backend_file.go` at ~/.shannon/credentials.json 0600), and `backend_other.go` (returns `ErrUnsupportedPlatform`), with runtime callers gating on `keychain.Supported()` (darwin||windows||linux) and passing `config.ShannonDir()` to `NewOSStoreAt` (Linux uses a deterministic file store, NOT go-keyring's Secret Service which fails headless). macOS-only GUI tools, including `computer_use`, gate on `runtime.GOOS != "darwin"` with a clean error (except `notify`, which keeps its cross-platform Desktop route). Do not reintroduce raw `syscall.Flock`/`syscall.Kill`/`Setpgid`/`os.Symlink` outside a `_unix.go` file — it breaks the Windows build or fails unprivileged on Windows.

### Memory

Memory sidecar lifecycle belongs to the daemon. CLI/TUI attach or probe; they do not spawn unless explicitly designed to. API key bytes must never hit disk or audit logs; only content-free state and fingerprints are logged.

### Auth (macOS + Windows + Linux)

`/local/auth/*` endpoints proxy to Shannon Cloud `/api/v1/auth/*`. `AuthManager` (`internal/daemon/auth.go`) owns the state machine (`signed_out` / `pending_verification` / `logging_in` / `bootstrapping_key` / `signed_in`); transitions emit `auth_state_changed` on the event bus. WS connection runs only in `signed_in` — `WSController.Start` / `Stop` are the only allowed call sites for spinning the reconnect loop. api_key is the source-of-truth credential and lives in the credential store (service `ai.kocoro.daemon.api_key` — macOS Keychain / Windows Credential Manager / Linux file store at ~/.shannon/credentials.json 0600); access_token / refresh_token are RAM only. On platforms without a credential store (i.e. `!keychain.Supported()`) `AuthManager` is nil, the endpoints respond 503 `platform_unsupported`, and the legacy `cfg.APIKey` path continues to drive WS. Linux IaC caveat: the yaml→store migration moves `api_key` out of config.yaml; config-managed yaml that re-adds it gets re-stripped each launch. See `internal/skills/bundled/skills/kocoro/references/auth.md` for the full endpoint matrix and recovery scenarios.

Implicit episodic preflight is in-message-only: it may inject private memory into the current request, but it must never persist to transcripts, replay, or summaries. Audit only content-free counts/status/error class.

### Session Sync

Session sync is opt-in. It uses a single Run entry point, flock, atomic marker writes, per-session ACKs, and persistent failed-entry bookkeeping. Permanent failures remain until the source session changes.

### Prompt Suggestions

Suggestion generation is a forked request after a successful main turn. Cache safety invariant: the fork must be byte-equal to the main request except for the appended assistant reply, suggestion prompt, skip-cache-write flag, and debug-only fork kind. Do not change tools, max tokens, thinking budget, or ordering in the fork. The fork is also source-gated (`wantsPromptSuggestion` allow-list in `daemon/runner.go`): only foreground sources with a UI consumer — `desktop`, `kocoro`, `web` — generate a suggestion; IM channels, schedule/cron, and autonomous local sources (heartbeat/watcher/mcp) are skipped. It is an allow-list, so any new background source defaults to skipped.

### Wire Contracts

Payloads decoded by UI clients (bus events, per-request SSE events, HTTP responses) are pinned by canonical fixtures in `docs/desktop-wire-fixtures/` and verified by `internal/daemon/wire_fixtures_test.go`, which emits through the real producer path and decodes producer bytes into consumer-shaped structs. Change a payload → update fixture + test in the same PR. Cross-version contract changes mint a capability token in `Capabilities` (`internal/daemon/client.go`; surfaced on the WS handshake and `GET /status`) — clients gate on tokens, never version-sniff. New event domains use dotted namespaced types with a common envelope; existing flat types are additive-only and never repurposed. See the kocoro skill `references/events.md` for full rules.

### Prompt Cache

Cloud owns the prompt-cache TTL policy. Preserve `cache_source` as attribution
(not a Kocoro-side TTL selector) and preserve canonical tool input
normalization. Any future TTL-policy change belongs in the Cloud service and
must update `docs/cache-strategy.md` in the same rollout.

Any in-place message content rewrite that can affect prompt bytes must emit cache-compaction/debug instrumentation so drift remains attributable.

### Context Management

Context-window defaults are only seeds; model responses may auto-adjust the active window unless a per-agent override explicitly locks it. Preserve proactive, pre-flight, and reactive compaction as separate gates so context errors do not cascade.

### Anti-Hallucination

Keep random XML tool execution delimiters and strip fabricated tool calls. For attended runs, preserve model-authored preambles; if the first tool batch is silent, a local tool may surface its required user-facing `description` without exposing other arguments. External tool descriptions are never used as runtime fallback text. Unattended runs remain silent.

## Tests

```bash
go test ./...
go test ./internal/agent/ -v
go test ./internal/daemon/ -v
go test ./internal/agents/ -v
go test ./internal/schedule/ -v
go test ./test/ -v
go test ./test/e2e/ -v
SHANNON_E2E_LIVE=1 go test ./test/e2e/ -v
go build ./...
```

Koe tests link cgo audio deps. On macOS, install them with `brew install opus opusfile pkg-config` and set `PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig` if pkg-config cannot find the Homebrew files.

Schedule tests use temp directories and do not write to the real LaunchAgents directory. Run live E2E before releases.

## Build And Release

- GoReleaser builds releases.
- The npm package is `@kocoro/kocoro`.
- Default version bumps are patch-only unless explicitly directed otherwise.
- Release flow: tag, push tag, let CI build and publish.

## Tools

Core local tools include file ops, archive inspect/extract, document extraction, shell/system, macOS GUI, schedules, memory, and skills. Common openers and compact local utilities are Direct; uncommon mutations are Deferred and loaded through `tool_search` when needed. On daemon runs, `computer_use` is the Deferred, high-level native-GUI contract discoverable through `tool_search` on the ordinary parent model. The parent keeps its configured model; only an invoked desktop task lazily resolves `openai.computer.v1` and starts a private OpenAI Responses trajectory. One exact task app binds background-first: semantic press/scroll and ordinary target-bound input stay in the background lane, while `foreground_allowed` may activate the app only for an action without an exact background primitive. Multi-app tasks retain foreground switching. The private trajectory owns screenshots, foreground coordinate pointer actions, typing, re-observation, continuation, and internal state/frame authority. NSWorkspace + CGWindow supplies exact target identity when AX is incomplete. Ambient physical interference requires a fresh exact observation before another mutation; explicit Pause/Take Over/Stop keeps user-owned quiescence. The whole task serializes on the shared GUI-operation lock. Unattended runs can invoke it ONLY via an explicit persisted GLOBAL `computer_use` always-allow grant. The legacy `computer` / `accessibility` / `applescript` / `ghostty` wrappers cannot be persisted as Always Allow and remain denied for unattended execution; daemon parent runs remove the legacy GUI wrappers and reject shell `osascript` / `cliclick`. The standalone `screenshot` tool remains separate, approval-required, and denied unattended. Runtime-conditional tools include session search, cloud delegation, publish/list/retract uploads, image generation/editing, and deferred tool search.

Every approval-required tool must expose a short human-readable description or equivalent purpose field for approval UI. Destructive or paid/public cloud tools require approval.
