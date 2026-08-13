# Kocoro Project Guide (AGENTS.md)

**Condensed mirror of `CLAUDE.md` for external coding agents. `CLAUDE.md` in this
directory is the full guide** — open it for reasoning, wire details, incident
history, or any subsystem not listed here. This file carries only rules you can
act on, plus the symbols and constants to grep. If the two disagree, `CLAUDE.md`
and the code win.

**Keep this file under 24 KB.** Harnesses that read `AGENTS.md` do so under a byte
budget shared across every such file from the repo root down (32 KiB by default),
and an over-budget file is truncated **from the tail, with no marker in the injected
text** — the reader cannot tell it got a partial file, and the sections at the bottom
are simply gone. CI asserts the ceiling; if you need more room, cut prose, not rules.

Kocoro is the Go CLI/runtime (`shan`) for Shannon AI agents. Production path:
daemon + Kocoro Desktop + Shannon Cloud — the daemon holds a Cloud WebSocket,
receives channel messages, runs the agent loop locally with full tool access, and
streams back. Also TUI, one-shot CLI, MCP server, local scheduled tasks.

Layout: `cmd/` (Cobra) + `internal/<pkg>/`; use Glob/Grep. Production path is
`internal/daemon/` driving `internal/agent/`.

## Working Rules

- `go.mod` is the source of truth for the Go version.
- Prefer existing repo patterns over new abstractions; keep changes tied to the task.
- Verify API response bodies, not just status codes.
- No parallel `_enhanced` variants — update existing code in place.
- For risky behavior changes, preserve operator-visible flags, rollback paths, focused tests.
- Test locally before pushing when touching dependency or generated-code surfaces.
- New `const max[A-Z]\w+ = <small_int>`: the comment MUST name (1) the workload
  justifying the value, (2) the symptom when it binds, (3) the override path.
  Prefer `viper.SetDefault(...)` for caps a power user might lift. Re-check
  small-int caps on model-family upgrades — 200K-era defaults bind silently at 1M.

## Doc Co-Maintenance

Feature changes update `README.md`, `CLAUDE.md`, and this file.

**The bundled `kocoro` skill is the AI-facing source of truth for the daemon HTTP
API.** Every `mux.HandleFunc(...)` in `internal/daemon/server.go` the agent calls
needs a matching `internal/skills/bundled/skills/kocoro/references/*.md` entry in
the SAME PR. Desktop-only transport endpoints stay out; their contract lives in
`docs/desktop-wire-fixtures/`.

## Tools

- **Required fields**: every `Run()` MUST check each `ToolInfo.Required` field is
  non-zero right after `json.Unmarshal` and return `agent.ValidationError(...)`,
  NOT a bare `ToolResult{IsError: true}`. Go's decoder cannot distinguish missing
  from zero, so a missing string arrives as `""` and `os.WriteFile` accepts it.
  The `[validation error]` prefix is load-bearing — `LoopDetector.isValidationErrorSig`
  force-stops on 3 consecutive, far below the all-errors 2x `ConsecutiveDup` budget.
- **Priority**: local > MCP > gateway, deduped by name. MCP-vs-MCP collisions
  resolve to the alphabetically-first server (`RebuildRegistryForHealth`); the
  shadowed tool is logged, never registered.
- **Exposure** (`agent/exposure.go EffectiveToolExposure`): explicit
  `ToolExposure` first, then source default — local Direct; MCP/gateway/
  integration Deferred. `ask_user_question` is explicitly Direct. `web_search` /
  `web_fetch` are Direct **only when `ToolSource()==SourceGateway`**
  (`tools/exposure.go ServerTool.ToolExposure`); a same-named MCP or integration
  tool keeps its Deferred default so a third-party catalog cannot widen the base
  schema surface. GUI/process automation and calendar/schedule mutations are
  Deferred; calendar/schedule reads stay Direct.
- `tool_search` uses deterministic BM25 (`agent/toolsearch_index.go`,
  `toolSearchDefaultLimit` 8) with exact `select:` lookup. The 16K Direct-schema
  budget is a regression diagnostic and MUST NEVER reclassify tools at runtime.
- Skill `allowed-tools` is execution-time denial, NOT schema filtering — the tools
  array stays byte-stable for the prompt cache. `agent.SkillExempt` (`think`,
  `tool_search`, `use_skill`) is for pure infrastructure only; do NOT exempt
  side-effecting tools.
- **Concurrency**: the dispatcher batches by `IsConcurrencySafeCall`, not
  `IsReadOnlyCall`; tools without a `ConcurrencySafeChecker` fall back to their
  `IsReadOnlyCall` value. `BashTool` implements it in `tools/bash_concurrency.go`,
  gated by `agent.bash_concurrency_enabled` (default true): only a strict
  read-only leading token AND no shell metacharacters (incl. `\n` / `\r`) is
  eligible; everything else stays in a size-1 serial batch.
- `tool_status` running/completed carry `tool_use_id` (capability
  `tool_use_id_events`).
- `set_work_plan` (`daemon/work_plan.go`): full-snapshot 2–8-step progress
  checklist, daemon non-ephemeral runs only. Runtime owns
  plan_id/revision/lifecycle/close_reason; closure comes from `LastRunStatus`,
  never a model claim, and bumps the revision. Persist-before-emit:
  `work_plan.updated` fires only after the covering save. Not dup-exempt;
  `SkillExempt`; never in the side-effect journal. Capability `work_plan_v1`.
- Every `RequiresApproval()==true` tool needs a `description` (5-15 words,
  model-written). The daemon does NOT block on a missing one; UI clients MUST use
  `description?.trim() || fallback`, NOT nullish coalescing.
- Integration schemas may carry optional trusted `material_side_effect`.
  `false` means an observational call can batch and bypass the durable mutation
  journal; missing stays fail-closed for old Cloud. Execute calls carry stable
  `request_id`; material calls also carry the journal `Idempotency-Key`.
  Never treat billing/provider-error/unknown post-dispatch errors on a material
  integration as known-no-effect; explicit pre-dispatch `provider_unavailable`
  is known-no-effect. Preserve integration provider/model/unit/cost
  usage fields through `ToolResult` and `EmitUsage`.
  `call_in_progress` means wait/query; never resend a material action.
- `x_prepare_post` is a local Deferred URL builder, never an X API write. Keep it
  free of OAuth, HTTP, browser openers, and browser/computer automation. It must
  report that nothing was posted and keep draft text and the generated URL out
  of audit summaries; the user alone clicks the review link and X's Post button.

## MCP

- Probe and reconnect a supervisor-known-disconnected server BEFORE dispatch.
- Every tools/call is bounded by per-server `tool_timeout_secs` >
  `mcp.tool_timeout_secs` (`mcp.DefaultToolCallTimeout` 300s); an earlier caller
  deadline still wins.
- A post-dispatch transport failure repairs the connection but re-dispatches ONLY
  `mcp.ToolReplaySafe` tools (read-only/idempotent annotations) — a transport
  error after dispatch does NOT prove the server never acted, since a stdio server
  can commit its write and die before responding. Everything else returns
  `mcp.OutcomeUnknownError`. Timeouts and protocol errors are never retried.
- A failed async connect is NOT terminal: `ReconnectScheduler`
  (`internal/mcp/reconnect.go`) retries with backoff (5s → 5min,
  `reconnectMaxAttempts` 6). Retries are owned by the manager generation —
  `SwapMCPReconnectScheduler` stops a superseded ladder on reload and
  `ShutdownCleanup` stops it before closing connections, so a timer never respawns
  a subprocess the cleanup is reaping. `ForgetMCPReconnect` re-arms an exhausted
  streak.
- Stdio subprocesses spawn in their own process group, killed via `-pgid` SIGTERM
  + 3s SIGKILL — npx-bridged servers are a process chain.
- Artifact paths: servers with known path semantics (playwright, or any
  `workspace_base`) get "Saved to:" absolute annotations for files that verifiably
  exist. Screenshot filenames default into `~/.shannon/tmp/sessions/<id>/` (swept
  by `daemon.scratch_max_age_days`, default 14). **`browser_snapshot` NEVER gets a
  default filename** — omitting it returns the inline accessibility snapshot.
  Model-supplied absolute paths always win.

## Permissions and Approvals

```text
hard-block -> denied commands -> compound splitting -> always-ask gates
  -> allowed commands -> default safe -> approval + safe checker
```

- Always-ask runs BEFORE allowlists, so adding a high-risk command to
  `allowed_commands` is a no-op. Unknown tools are denied.
- "Always Allow" on an always-ask command is honored once and NEVER persisted
  (enforced at write time in `cmd/daemon.go` + `server.go`, at runtime in
  `loop.go checkPermissionAndApproval`).
- `HandleAlwaysAllowDecision` (`alwaysallow.go`) is the single decision path
  shared by SSE and WS so transports cannot drift.
- Two deny-lists in `internal/agent/tools.go`: `autoApprovalDenyList`
  (`DisallowsAutoApproval`) = `computer`, `accessibility`, `applescript`,
  `ghostty`; `unattendedAutoApprovalDenyList` (`DisallowsUnattendedAutoApproval`)
  = those four plus `computer_use` and `screenshot`.
- `computer_use` is deliberately absent from the FIRST list — its persisted global
  grant IS the product's Computer Use permission, honored even unattended, scoped
  BY NAME via `unattendedGrantHonored`. A blanket "any persisted always-allow"
  rule would silently re-open unattended desktop capture for `screenshot`. Legacy
  GUI names can never use the global grant; `computer_use` is rejected at
  per-agent scope (`agents.ValidateAgentPermissionsConfig`) as the grant is
  global-only.

## Wire Contracts

- **The surface list is `docs/cloud-contract-surface.md`** — what this repo owes Shannon
  Cloud and Kocoro Desktop, with the counterpart symbol for each. Read it before changing
  anything that crosses a process boundary.
- Payloads decoded by UI clients are pinned by fixtures in
  `docs/desktop-wire-fixtures/`, verified by `internal/daemon/wire_fixtures_test.go`
  through the REAL producer path. Change a payload → update fixture + test in the
  SAME PR.
- Every cross-version contract change mints a capability token in `Capabilities`
  (`internal/daemon/client.go`), surfaced on the WS handshake and `GET /status`.
  Clients gate on tokens — NEVER version sniffing or decode-failure probing.
- New event domains use dotted types with a common envelope; existing flat types
  are additive-only, never repurposed.
- `ApprovalBroker` and `QuestionBroker` are thin faces over ONE shared
  `pendingCore[D]` (`pending.go`). A third interaction kind MUST build on it — do
  NOT copy a broker.
- `ask_user_question` gates on `CanPresentQuestionUI` / `questionUISources`, an
  ALLOW-list, NOT the approval predicate: every source without a question UI
  DECLINES, because a question has no safe auto-answer. Slack/Feishu/Lark/Teams/
  LINE render approval cards but have NO question channel. Capability `question_v1`.
- `delivery_ack`: ack an inbound message only AFTER reply delivery succeeds.
  Reply-failure paths skip the ack so replay stays correct.

## Daemon Routing

- Precedence: explicit session, threaded route, per-sender route, agent route,
  legacy channel route. Named agents are multi-session. Routed managers are
  long-lived; bypass/heartbeat use short-lived managers. `tool_status` `running`
  fires only when execution actually starts.
- Config reload is revision-based over ONLY `~/.shannon/config.yaml`
  (`config_reload_state_v1`). Never infer project/local overlays are watched, and
  never advance the applied revision after an unrelated internal mutation.
- Schedule proactive push: `broadcast` (`auto`/`on`/`off`) gates whether a run
  pushes to IM. **The target is always the channel the schedule was created in**
  (snapshotted `im_status_context`), so a schedule created outside an IM chat
  NEVER pushes, even with `broadcast=on`. `thread` resolves to
  `ProactivePayload.UseThread *bool`: `nil` means anchored (current behavior),
  only explicit off goes top-level. Capabilities `schedule_broadcast_gate` /
  `proactive_thread_mode` are observability only.
- Output profiles, not per-channel syntax: `markdown` default, `plain` for
  Cloud-distributed channels. Feishu/Lark/Teams are cloud sources that stay
  `markdown` (`markdownCloudSources`); WeChat (iLink) stays `plain`.

## Turn Lifecycle and Context

- Only `PhaseAwaitingLLM` / `PhaseForceStop` count as idle (`agent/phase.go`;
  fail-closed, panics under `testing.Testing()` or `SHANNON_PHASE_STRICT=1`).
  Defaults: `agent.idle_soft_timeout_secs` 90, `agent.idle_hard_timeout_secs` 540,
  `agent.stream_idle_timeout_secs` 90.
- Mid-turn checkpoints run after tool batches, after reactive compaction, and
  before force-stop; the final save rebuilds from the same baseline.
- Interrupted turns auto-resume at daemon start (newest first, serial) only within
  `agent.interrupted_resume_max_age_hours` (default 4) — older checkpoints are
  abandoned, NEVER executed. `agent.interrupted_resume_max_attempts` (default 3)
  persists BEFORE the LLM call. Recovered runs ALWAYS classify as unattended so
  the deny-list applies, and pin the session's original route key.
- `agent.context_window` (default 1_000_000) is only a seed;
  `maybeAutoAdjustContextWindow` resets it from `response.model` via
  `agent/modelcontext.go` unless a per-agent override locks it.
- Keep proactive / pre-flight / reactive compaction as SEPARATE gates so context
  errors do not cascade. Thresholds are absolute-buffer complements of the
  fractional lines (`compactAbsoluteBufferTokens`), sized so a compaction cannot
  immediately re-trigger; see CLAUDE.md for the exact formulas.
- Estimate-based gates MUST share the trigger's scale via `estOverheadTokens`,
  persisted in `Session.CompactionCalibration` and restored through
  `SetEstOverheadState` with model-pin and registry-fingerprint validation.
- Summarizer input is budgeted in TOKENS (`summarizeInputCapTokens` 150_000 via
  `conservativeBytesPerToken` 2.5). **Do NOT gate this on `EstimateTokens`** — its
  3.5 chars/token is representative, not conservative. Over-cap transcripts fold
  chunk by chunk (tool pairs atomic, `maxSummaryFoldChunks`) with head+tail fallback.
- Compaction NEVER rewrites the lossless `Session.Messages`;
  `Session.CompactionCheckpoint` is the separate model-live state. Proactive and
  pre-flight re-inject recently read files; reactive skips restoration. Every
  applied compaction snapshots pre-replacement state with `<private_memory>`
  stripped and images replaced by text markers; snapshot failures never block it.
- Tool-result budgeting: per-result spill (`DefaultMaxToolResultSizeChars` 50K,
  grep 20K), per-turn `aggregateCapThreshold` 200K runes, persisted
  replacement/seen maps surviving turns and crash recovery. `file_read` is
  `UnlimitedToolResultSizeChars`, self-bounding at `fileReadHardCapRunes` 500_000.
- Images/attachments: source-time compression, wire-time `filterOversizeImages`,
  persist-time `SanitizedRunMessages`. Any new path MUST pass all three. Caps:
  500 MB/file, 20/msg, inline doc <= 25 MB raw.
- Thinking blocks: preserve `thinking` / `redacted_thinking` in order; sanitizers,
  compaction, fork builders, and persistence MUST NOT rewrite them. Strip only
  before sync upload (`internal/sync/strip_thinking.go`). The local `think` tool is
  skipped on the default gateway+thinking path (`shouldRegisterThinkTool`) but
  stays for disabled-thinking, Ollama, or `ForceThinkTool=true`.

## Prompt Cache and Suggestions

- Cloud owns TTL policy. Preserve `cache_source` as attribution, never a
  Kocoro-side TTL selector, and preserve `normalizeToolInput` canonicalization.
  Skill listing lives in the scaffolded user message, not the system prompt.
- Any in-place `messages[idx].Content` rewrite MUST call
  `client.LogCacheCompactEvent` — uninstrumented rewrites break drift attribution.
- `agent.response_detail` renders provider-neutral final-answer guidance in BP3
  StableContext: global missing/empty → `balanced`, named-agent missing/empty
  inherits global, provider request effort unchanged. Strict machine-readable
  internal loops suppress it explicitly.
- The suggestion fork MUST be byte-equal to the main request except the appended
  assistant reply, suggestion prompt, `SkipCacheWrite`, and debug-only fork kind.
  Do not change tools, max tokens, thinking budget, or ordering. Source-gated by
  `wantsPromptSuggestion` / `promptSuggestionSources`: only `desktop`, `kocoro`,
  `shanclaw` (legacy Desktop alias), `web`. It is an ALLOW-list, so new background
  sources default to skipped.

## Skills

- `builtinSkills` (`internal/skills/api.go`: `kocoro`, `kocoro-generative-ui`) are
  sha256-walk synced from `embed.FS` every startup — user edits are WIPED. Fork
  under a different name.
- `internal/daemon/skill_filter.go` filters `desktopOnlySkills` from the
  per-request list on cloud-distributed sources, applied ONCE producer-side right
  after `LoadGlobalSkills` so registry, listing, and semantic discovery stay
  consistent. Drift test `skill_filter_test.go`.
- Skill-install recommendation discovery is binary-pinned and OFFLINE: production
  uses only the embedded eligible catalog (`official_catalog.json`), never the
  GitHub/static/ClawHub registries.
- Two marketplace surfaces that MUST NEVER share a response shape:
  `/skills/marketplace/*` (static registry, integer-page — **the frozen macOS
  Desktop contract; do not add source-conditional branches**) and
  `/skills/clawhub/*` (live catalog, opaque cursor). Catalog GETs retry 429/5xx +
  network via `doGETWithRetry`; **4xx is never retried**. `isTransientListErr`
  mirrors `isRetryableStatus`. `exclude_installed` is ClawHub-only and
  page-granular: when `skills.marketplace.clawhub_exclude_fill_max_pages`
  (default 5) binds, the page is short or empty with a non-empty cursor and the
  client MUST keep paging.
- Skill secrets: Keychain `com.shannon.skill.<name>` + a plaintext index of key
  NAMES only, env-var-only injection scoped to the current run's `use_skill`.

## Memory, Sync, Browser Bridge

- Sidecar lifecycle belongs to the daemon; CLI/TUI attach or probe, never spawn.
  **API key bytes MUST never hit disk or audit logs** — only a `sha256[:16]`
  fingerprint.
- Episodic recall is model-driven: production paths expose `memory_recall` and
  `session_search` directly and MUST NOT install the implicit small-model
  preflight. Route unnamed references to session search; stop after a structured
  no-data instead of retrying relation variants.
- Session sync is opt-in: single Run entry point, flock, atomic markers,
  per-session ACKs. Permanent failures remain until the source session changes.
- Browser file previews stay fail-closed: only effective session CWD and attached
  paths, symlinks resolved on both sides, random tokens, no directory listing,
  teardown on session close.

## Config, Locking, Cross-Platform

- Merge order: global → project → local project. Scalars override, lists
  merge/dedup, structs merge field-by-field. MCP env var casing is preserved by
  direct YAML re-read.
- Persistent JSON indexes use write-temp + rename + an exclusive lock via
  `internal/fslock`, NOT raw `syscall.Flock`. **Never delete lock files** — that
  splits locks across inodes. Rename targets are read lock-free and never locked
  directly; a mandatory `LockFileEx` on the destination would block
  rename-over-open on Windows.
- Cross-compiles to macOS / Linux / Windows with `CGO_ENABLED=0`. Build-tagged:
  `internal/fslock` (flock vs `LockFileEx`), per-package `*_proc_{unix,windows}.go`
  (`Setpgid`+`Kill(-pid)` vs `CREATE_NEW_PROCESS_GROUP`+`taskkill /T /F`),
  `cmd/proc_signal_{unix,windows}.go`,
  `internal/memory/bundle_link_{unix,windows}.go` (symlink vs directory junction),
  `internal/keychain/backend_{keyring,linux,other}.go`.
- Runtime callers MUST gate on `keychain.Supported()` (darwin||windows||linux),
  never a raw `runtime.GOOS == "darwin"`; kept in sync with build tags by
  `TestSupportedMatchesBuildTag`. Pass `config.ShannonDir()` to `NewOSStoreAt`.
  Linux uses a deterministic 0600 file store, NOT go-keyring's Secret Service,
  which reports success at construction then fails every read/write headless.
- **Do not reintroduce raw `syscall.Flock` / `syscall.Kill` / `Setpgid` /
  `os.Symlink` outside a `_unix.go` file** — it breaks the Windows build or fails
  unprivileged there.
- macOS-only GUI tools, including `computer_use`, gate on
  `runtime.GOOS != "darwin"` with a clean error; `notify` is exempt.

## Auth

`/local/auth/*` proxies to Cloud `/api/v1/auth/*`. `AuthManager`
(`internal/daemon/auth.go`) owns the state machine and emits `auth_state_changed`.
**WS runs ONLY in `signed_in`, and `WSController.Start` / `Stop` are the only
allowed call sites for the reconnect loop.** api_key is the source-of-truth
credential in the credential store (`ai.kocoro.daemon.api_key`); access/refresh
tokens are RAM only. On `!keychain.Supported()`: `AuthManager` nil, endpoints 503
`platform_unsupported`, legacy `cfg.APIKey` drives WS. The yaml→store migration
strips `api_key` from `config.yaml` on every launch, so config-managed yaml that
re-adds it keeps getting stripped. Endpoint matrix:
`internal/skills/bundled/skills/kocoro/references/auth.md`.

## Anti-Hallucination

Keep random XML tool-execution delimiters and strip fabricated tool calls. In
attended runs preserve model-authored preambles; a silent first tool batch may
surface a local tool's required `description` without exposing other arguments.
External tool descriptions and generic `purpose` fields are NEVER runtime fallback
text. Unattended runs remain silent.

## Voice Front-Brain

`internal/koe`, macOS native speech-to-speech. Keep `stop_speaking` (current
output), `cancel` (delegated work, by `task_id` or `all_running=true`), and
terminal `end_call` (whole session) as SEPARATE authorities. `do_task` defaults to
exactly one call per response; parallel calls need an explicit user request and
disjoint scopes. `MapDoTaskOutcome` maps a partial run to a canned `incomplete`
line and seeds no digest, so a cut run's progress tail is never voiced as the
result. ASR transcripts stay asynchronous evidence — not turn control, not
barge-in admission, not default dismissal.

## Tests

```bash
go test ./...                                          # or ./internal/{agent,daemon,agents,schedule}/ -v
go test ./test/ -v && go test ./test/e2e/ -v
SHANNON_E2E_LIVE=1 go test -tags=live ./test/e2e/ -v   # live suite; the build tag is required
go build ./...
```

Koe tests link cgo audio deps: `brew install opus opusfile pkg-config`, and set
`PKG_CONFIG_PATH=/opt/homebrew/lib/pkgconfig` if pkg-config cannot find them.
Schedule tests use temp dirs and MUST NOT write to the real LaunchAgents directory.

Live E2E uses the hidden foreground-only isolation flags in
`docs/live-e2e-testing.md`. `--isolated` isolates filesystem state, port ownership,
AND credential-store access (it calls `config.DisableCredentialStoreForProcess`,
so config load skips OS credential reads and the yaml→keychain migration); the
authorized key is piped in via `--isolated-api-key-stdin`. Cloud/background
automation is suppressed, agent tool capabilities stay real, and MCP is disabled
unless `--isolated-mcp` supplies an explicit startup allowlist.

## Build and Release

- GoReleaser builds releases; npm package is `@kocoro/kocoro`.
- **Versioning is PATCH-only by default** — do not bump minor/major unless asked.
- Release: tag, push tag, CI builds and publishes.
- `docs/` is gitignored by default; tracked docs are allowlisted in `.gitignore`.
  Add new docs there before committing.
