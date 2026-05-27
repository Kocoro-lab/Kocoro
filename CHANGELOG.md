# Changelog

All notable changes to Kocoro (`shan` CLI / daemon) are documented here. Format follows [Keep a Changelog](https://keepachangelog.com/).

## v0.1.17 — 2026-05-27 — Built-in MCP catalog, async MCP startup, desktop-only skill suppression

Ships a daemon-owned catalog of pre-bundled MCP servers (first entry: Intercom, disabled by default) and reworks MCP startup to be non-blocking with reliable subprocess cleanup — the daemon-side foundation for a client-driven "toggle MCP server on/off with OAuth confirm" flow. Also adds a producer-side filter that hides desktop-only skills (whose output only renders in a GUI WebView host) from cloud-distributed channels so they can't leak raw HTML into Slack / LINE / Feishu / Lark / WeCom / Telegram / webhook. No wire-protocol break — the new `GET /config/status` fields are additive and older clients ignore them.

> Backlog note: v0.1.13, v0.1.14, and v0.1.16 were tagged without CHANGELOG entries; see their annotated tag messages and the GitHub Releases page until backfilled.

### Added

- **Built-in MCP catalog** (PR #194, `internal/mcp/builtins.go`, `internal/config/config.go mergeBuiltinMCPServers`) — `BuiltinMCPServers` is the in-binary source of truth for command/args/type/url/context; user yaml only persists `disabled` / `env` / `keep_alive` / `connect_timeout_secs`, so daemon upgrades pick up catalog edits without yaml surgery. `config.Load` field-merges the catalog onto user yaml (user wins on tunable fields, Go source wins on immutable fields; `env` is deep-copied + key-by-key merged). `PATCH /config` rejects edits to daemon-owned fields (`409 builtin_mcp_immutable`, `safeguard.go`). `GET /config/status` grows a parallel `mcp_server_info` map `{builtin, display_name, description, auth_hint, requires_auth?, authorized?}` so a client can render a toggle + OAuth confirm modal without hard-coding the catalog. First entry: Intercom (`npx mcp-remote https://mcp.intercom.com/mcp`), `requires_auth: true`, 300s connect timeout.
- **Async MCP startup** (PR #194, `internal/mcp/client.go StartConnectAll`, `internal/tools/register.go RegisterAllWithBaselineAsync`) — daemon startup and `POST /config/reload` no longer block on MCP handshakes; they build the registry with local + gateway tools, swap deps, then fire per-server connect goroutines. A per-server `inFlight` set (`tryReserveInFlight`) prevents a reload mid-connect from spawning a duplicate subprocess that would crash `EADDRINUSE` on the OAuth loopback port. Per-server timeout resolves `connect_timeout_secs` > `mcp.default_connect_timeout_secs` > 60s floor. Successful connects flip the supervisor to Healthy and rebuild the live registry so tools appear as each server finishes; failed connects write an `mcp_connect` audit row and stay enabled-but-disconnected.
- **MCP subprocess group reaping** (PR #194, `internal/mcp/processgroup_unix.go`) — stdio MCP servers spawn under `Setpgid=true` with a `cmd.Cancel` that signals `-pgid` SIGTERM then a `WaitDelay=3s` SIGKILL backstop (ladder: SIGTERM-group → SIGKILL-group → SIGKILL-leader). Without this, npx-bridged chains (npx → npm exec → node mcp-remote) leak the grandchild holding the OAuth loopback port. `Close` / `Disconnect` / `Reconnect` cancel the group before `c.Close()`.
- **Desktop-only skill suppression on cloud channels** (PR #193, #42, `internal/daemon/skill_filter.go`) — a `desktopOnlySkills` registry is filtered out of `loadedSkills` at the producer side in `runner.go` when `isCloudSource(req.Source)` is true, so all three exposure layers (use_skill registry, listing, semantic discovery) see the same view. Only entry today is `kocoro-generative-ui` — its html-artifact fences only render in a GUI WebView host, so activating it from a cloud-distributed channel would surface raw HTML/CSS/JS. Drift test walks `desktopOnlySkills × cloudSourceSet`.

### Fixed

- **Blank Chrome tab at agent-turn start after async startup** (commits `6374747`, `2255ecf`, `internal/daemon/runner.go`) — the async connect flow left playwright at a `Degraded` rest state (post-discovery Disconnect with no intervening probe to demote it). RunAgent's turn-start preflight saw `Degraded != Disconnected`, fired `ProbeNow`, and relaunched CDP Chrome (an `about:blank` tab). Preflight now skips when there is no live client; lazy `ensureChromeDebugPort` still launches Chrome when the agent actually calls a browser tool.
- **Duplicate connect goroutine on reload** (PR #194, `internal/mcp/client.go`) — `POST /config/reload` fired while a daemon-startup async connect was still inside Initialize/ListTools spawned a second connect for the same server, racing for the OAuth loopback port (`EADDRINUSE`). `tryReserveInFlight` dedups; `Reconnect` honors the same gate.
- **OAuth re-enable UX** (commit `c986534`, `internal/mcp/oauth_state.go`) — new `MCPRemoteHasToken` helper (md5(serverURL) + glob across `~/.mcp-auth/mcp-remote-*/`) backs the `authorized` field so a client can skip the confirm modal on re-enable. Previously: confirm modal → user clicks Authorize → mcp-remote silently reuses the cached token → no browser opens → looks broken.

### Docs

- `CLAUDE.md` + `AGENTS.md`: four new Daemon Architecture rows (built-in catalog, async startup, subprocess reaping, reload-as-retry) and a Skill Discovery "per-request channel suppression" note.
- `references/mcp.md` (bundled `kocoro` skill): built-in servers section, `authorized` semantics + client guard pseudocode, per-server connect timeout, and `/config/reload`-as-retry.

### Cross-repo consumers

- **Desktop clients**: read `mcp_server_info.{requires_auth, authorized}` from `GET /config/status`; show the OAuth confirm modal (using `auth_hint` as the body) only when `requires_auth && authorized !== true && currentlyDisabled`. Map the "Retry" affordance to `POST /config/reload`, and poll `GET /config/status` for `disabled → enabled → connected` transitions since reload now returns immediately. All new fields are additive — older clients ignore them.

---

## v0.1.12 — 2026-05-21 — Empty-response 400 fix, language-drift mitigation, session sort

Three internal-only fixes (no wire-protocol changes, no cross-repo coordination required). The largest is an Anthropic-side 400 root-cause fix: when the model emitted an assistant turn containing only `thinking` blocks (no text / tool_use), the next request carried a `cache_control` on empty `content[]` and the API rejected it. The daemon now refuses to persist empty assistant content, surfaces a neutral friendly fallback, and the context sanitizer repairs the same shape on historical messages so existing sessions keep loading.

### Fixed

- **Empty-assistant `cache_control` 400** (PR #175, `internal/context/sanitize.go`, `internal/agent/loop.go`) — `RepairEmptyAssistantContent` detects empty- and whitespace-only assistant blocks and rewrites them with a neutral marker; rewrites are instrumented via `LogCacheCompactEvent` so cache-debug attributes the change. Loop refuses to persist empty turns and emits a friendly fallback message plus the new `runstatus.EmptyFinalResponse` audit code.
- **Language drift from multilingual MEMORY.md** (PR #177, #157, `internal/prompt/builder.go`, `internal/instructions/loader.go`, `internal/agent/loop.go`) — system-prompt assembly reorders memory injection and tags memory blocks so a mixed-language MEMORY.md no longer pulls the model into the wrong reply language. `appendDynamicUserBlocks` extracted with explicit ordering test.
- **`GET /sessions` sort by `updated_at`** (`internal/session/{index,manager,store}.go`, `internal/tui/{app,header}.go`) — recently-active sessions surface first. TUI session list and startup-header "Recent activity" updated to display `updated_at` so the visible timestamp matches the new sort order. `kocoro` skill reference (`references/agents.md`) updated.

### Note

Tag `v0.1.12` was cut before this CHANGELOG entry landed; the entry is on `main` post-tag (the tag's commit message carries the same summary).

---

## v0.1.11 — 2026-05-18 — Async share, mid-turn attachments, streaming bypass, max-tokens handling

Ships an async session-share path so the publish round-trip no longer blocks the caller (daemon owns the share state machine end-to-end), adds mid-turn attachment threading so a user can drop a file into an already-running turn, switches the daemon to streaming end-to-end so completions are no longer capped by the Anthropic non-streaming 16K ceiling, and tightens behavior around `stop_reason=max_tokens` so truncated tool calls don't get retried into a stuck loop. Three security/correctness fixes ship alongside: session_id path traversal (#158), Authorization-header leak on cross-host redirect, and `file_read` runaway via the spill exemption + 500K rune hard cap (#161).

### Added

- **Async session share + approval/share unification** (PR #170, `internal/daemon/share_async.go`, `internal/daemon/share_handler.go`) — `POST /sessions/{id}/share` returns immediately with a job state; the daemon renders, uploads, and finalizes in the background. Daemon is the single source of truth for share state (summary, status, retry, finalize); the post-upload `LIST` lookup filters by `kind=session_share` so concurrent landing-page / image uploads can't shove the row off the first page. Also: timestamp consistency across summary / approval / share events, and approval denylist cleanup (`publish_to_web`, `generate_image`, `edit_image` no longer special-cased — they go through ordinary approval-required-tool flow, with always-allow persistence available).
- **Mid-turn attachments** (PR #162, `internal/agent/inject_types.go`, `internal/daemon/runner.go`) — `InjectedMessage` carries a `Files []InjectedFile` slice; `ConvertFilesToInjected` materializes them as content blocks into the in-flight user turn (subject to the same `oversize_image.go` guards as initial-turn attachments). A `HasActiveRun` probe runs before the download so cancelled runs don't pull bytes (`f3bad5b`).
- **Streaming bypasses 16K non-streaming cap** (commit `c0f0c87`) — daemon → cloud is now streaming by default so completions can exceed Anthropic's non-streaming completion ceiling. Truncated trailing `tool_use` blocks under `stop_reason=max_tokens` are now suppressed (`343fae4`, PR #155) and the continuation-prompt is flagged as injected (`556e9dc`, PR #172) so the next turn's input is correctly attributed.
- **Reply-complete banner notification** (commits `2c87de3`, `bac68ad`, `5e9e0c5`) — emits a system notification when a reply finishes. Darwin-only guard, channel-source filtering (TUI/CLI/web suppressed; only daemon-distributed sources notify), markdown stripped from the body.
- **审批事件 + 定时任务生命周期通知** (PR #156) — richer approval lifecycle events on the wire, scheduled-task pre-run / completion / failure events surfaced through the bus. `internal/daemon/approval_events_test.go` (+855 LOC) covers the new event matrix.

### Fixed

- **Path traversal via `session_id`** (PR #158, `internal/daemon/validate.go`) — `safeSessionPath` now rejects `.`, `..`, and embedded traversal sequences before any join. `validate_test.go` asserts the rejection message.
- **`file_read` spill exemption + 500K rune hard cap** (PR #161, `internal/tools/file_read.go`) — `file_read` no longer routes through the per-result spill path (which had been silently shortening large reads into 2K previews). It now self-bounds at `fileReadHardCapRunes = 500_000` with a clear truncation marker. Rationale (workload / symptom / override) documented inline per the new hardcoded-limit policy (`fb1836f`).
- **Chrome teardown after browser-using runs** (PR #166, `internal/mcp/chrome.go`) — Playwright MCP child + Chrome instance are reliably torn down at end of run; previously could leak processes across multiple browser-using runs. New `chrome_test.go` covers the lifecycle (+387 LOC).
- **Skill installs retry on transient git failure** (PR #171, `internal/skills/marketplace.go`) — `MarketplaceInstall` now retries on transient `git fetch` failures with bounded backoff, and emits an audit row for every install operation (success or failure). Eliminates a class of false-failure user reports.
- **Authorization stripped on cross-host redirect** (commit `86e09f3`, `internal/daemon/client.go`) — daemon HTTP client now mirrors Go stdlib's `CheckRedirect` policy: when redirected to a different host, the `Authorization` header is dropped before the redirected request. Defense-in-depth against accidental token leak to an external host.
- **Per-turn truncation recoveries capped at 3** (commit `f7c51e9`, `internal/agent/loop.go`) — prevents a pathological model output from causing unbounded "truncation → continue → truncation" recoveries within a single turn.

### Docs

- `run_status` codes documented in `internal/skills/bundled/skills/kocoro/references/events.md`; truncation comments tightened (commit `7e54db5`).
- `file_read` hard-cap rationale inlined per the new hardcoded-limit policy (commit `fb1836f`).
- Inject-priority comment and dev-guide spill row corrected (commit `8485fc9`).
- Stdlib redirect-strip gating clarified in `CheckRedirect` comment (commit `ca6322d`).

### Cross-repo consumers

- **Kocoro Desktop**: the async-share state machine now lives daemon-side. The Desktop client should poll the share endpoint for terminal state rather than awaiting the original POST. Approval denylist removal means `publish_to_web` / `generate_image` / `edit_image` will surface "Always Allow" buttons on first approval; UI copy should reflect that these are now persistable like other approval-required tools.
- **Shannon Cloud**: streaming-by-default daemons can now receive responses beyond the legacy 16K non-streaming cap. Cloud should not introduce a regression cap on the streaming path.

---

## v0.1.7 — v0.1.10

These releases were tagged without CHANGELOG entries; see annotated tag messages
(`git tag -l v0.1.10 -n50`) and the
[GitHub Releases page](https://github.com/Kocoro-lab/Kocoro/releases) for the
per-release "Highlights" notes. Major themes across this window:

- **v0.1.10** (2026-05-15) — Session share to web (#152), persistent notification history JSONL, bash command concurrency Phase C default-on (#151), image dimension cap (#153).
- **v0.1.9** (2026-05-14) — `PUT /skills/{name}` returns 409 on conflict instead of silent upsert (#139, with `?force=true` opt-in and `403 skill_is_builtin` for builtin slugs).
- **v0.1.8** (2026-05-13) — Kocoro rebrand Round 1 follow-ups.
- **v0.1.7** (2026-05-13) — ShanClaw → Kocoro rebrand Round 1.

---

## v0.1.6 — 2026-05-12 — Inbound attachments + skill ZIP upload + episodic-memory default revert

Ships inbound attachment support so cloud-fed PDFs and Office documents arrive over the WebSocket path with the right rendering treatment (PDF as a native Anthropic `document` block, DOCX/XLSX/PPTX as pre-extracted text), plus six new local document and archive tools so the daemon can handle the same file types locally. Adds a `POST /skills/upload` endpoint so users can install a skill from a local ZIP without going through ClawHub. Reverts the v0.1.5 "session sync + episodic memory on by default" change after operator feedback — both now default off, opt-in via Kocoro Desktop's Beta toggle.

### Added

- **Inbound attachment protocol** (`internal/daemon/attachment.go`, PR #132) — WS-path `RemoteFile` gained three optional cloud-populated fields: `document_b64` (PDF base64 for a native Anthropic `document` content block, ≤25 MB raw), `extracted_text` (cloud's pre-extracted DOCX/XLSX/PPTX/CSV text), `extraction_note` (audit-only metadata). HTTP-path `RequestContentBlock` accepts a new `document` type that flows straight through `resolveContentBlocks`. Caps: 500 MB / file, 20 files / message; daemon-side rune cap of 500K on inline extracted text as defense-in-depth. New capability tokens `inline_document_b64` and `inline_extracted_text` (alongside the existing `delivery_ack`) tell Cloud the daemon can decode the new fields — older daemons fall back to URL download cleanly.
- **Local document extractors** (`internal/tools/doc_extract.go`) — `pdf_to_text` (poppler `pdftotext -layout`, install-hint fallback), `docx_to_text` / `pptx_to_text` (pandoc primary, unzip + XML-strip fallback), `xlsx_to_text` (xlsx2csv primary, unzip + sharedStrings/sheet XML fallback). Fixed-argv `exec.Command` (no shell injection), 60s timeout per call, output capped at 100K runes with a `[Truncated: ...]` marker.
- **Local archive tools** (`internal/tools/archive.go`) — `archive_inspect` (read-only entry listing, no approval needed) and `archive_extract` (approval-gated, atomic stage-then-rename) for `.zip / .tar / .tar.gz / .tgz`. Rejects encrypted zips, absolute-path / symlink / device / setuid entries; caps at 500 entries, 50 MB per entry, 200 MB total. Single-layer only.
- **`POST /skills/upload` endpoint** (`internal/daemon/server.go`, PR #133) — multipart upload installs a skill from a local ZIP. 50 MB body cap (enforced both at `MaxBytesReader` and inside `extractZipToSkill`). Reuses the existing extractor so zipbomb guards, symlink rejection, path-escape checks, and `__MACOSX` / `.git*` exclusion are inherited. Handles GitHub/Finder single-top-level-dir layout. Per-slug `SlugLocks` serialize concurrent uploads of the same slug.
- **`SkillConflictError` 409 response with side-by-side compare** (`internal/skills/marketplace.go`) — when a slug already exists and `force=false`, returns existing vs. uploaded name / description / prompt so Kocoro Desktop can render a side-by-side compare sheet. Prompt fields truncated to 8 KB via `truncatePromptPreview`; callers needing the full body fetch `GET /skills/{slug}`.
- **`IsBuiltinSkill` guard** (`internal/skills/api.go`) — unconditionally rejects uploads targeting `kocoro` / `kocoro-generative-ui` even when `force=true` (`EnsureBuiltinSkills` would silently revert any override on next restart, so the upload would be useless).

### Changed

- **`sync.enabled` defaults back to `false`** (commit `1f5958a`) — reverses the v0.1.5 default-on change. Operator feedback was that the implicit upload-on-by-default behavior was surprising for cloud-connected installs that hadn't yet opted into episodic memory. Enable explicitly via `sync.enabled: true` or the Episodic Memory toggle in Kocoro Desktop's Settings → Advanced → Beta.
- **`memory.provider` defaults back to disabled** (commit `1f5958a`) — same rationale; pairs with the `sync.enabled` revert so episodic memory is fully off until the Beta toggle is enabled.
- **`<private_memory>` injection body bounded to 8 KiB** (`internal/agent/preflight.go`, commit `2c6f22c`) — the implicit episodic preflight previously could inject an unbounded body into the in-flight user message when the sidecar returned a verbose recall. Now capped at 8 KiB with a `[truncated]` marker; oversized recalls trim the lowest-scoring entries first.

### Fixed

- **`truncatePromptPreview` rune walk is now O(1) per step, bounded to 3 iterations** (`internal/skills/marketplace.go`) — the initial conflict-truncation helper called `utf8.ValidString` in a loop, rescanning the full prefix each step (O(n²) worst case on invalid UTF-8 input). Replaced with a `utf8.DecodeLastRuneInString` walk-back; UTF-8 runes are ≤4 bytes, so a cut into a partial sequence leaves at most 3 trailing bytes to strip.

### Cross-repo consumers

- **Shannon Cloud**: must populate `RemoteFile.document_b64` (for PDFs ≤18 MB) and `RemoteFile.extracted_text` (for DOCX/XLSX/PPTX/CSV) when serving cloud-fed attachments to daemons advertising the new capability tokens. Older daemons (no `inline_document_b64` / `inline_extracted_text` capability) get the legacy URL-only path. The originally planned `/extract` round-trip is no longer needed — daemons handle the same file types locally via `internal/tools/doc_extract.go`.
- **ShanClaw Desktop**: helper bundle rebuilt against this tag. The Episodic Memory toggle in Settings → Advanced → Beta now controls `memory.provider` + `sync.enabled` together, both defaulting to off in this release.

---

## v0.1.5 — 2026-05-11 — Episodic memory (TLM sidecar + session sync default-on)

Ships the full local episodic memory pipeline. The TLM sidecar is now managed by the daemon — it spawns, health-probes, restarts on crash, pulls fresh bundles from Kocoro Cloud every 24h, and hot-reloads the sidecar on install. Session sync is on by default for cloud-connected installs so the training pipeline runs without manual config. CLI and TUI paths now correctly apply cwd-local memory overlays.

### Added

- **TLM sidecar lifecycle management** (`internal/memory/`) — daemon spawns the `tlm` binary, probes `/health`, restarts on crash (up to `memory.sidecar_restart_max` attempts), and tracks `MemoryStatus` (provider, reason, restart_attempts) on `GET /status`. Sidecar process is isolated via `SysProcAttr` + `Pdeathsig` so orphaned sidecar processes are reaped on daemon exit.
- **`memory_recall` tool** — structured long-term memory lookup via the TLM sidecar's `/query` Unix socket. Modes: `direct_relation` (one-hop predicate) and `path_query` (multi-hop). Returns `memory_block.groups[]` with `via_relations` / `observed_path[]`, `no_data_reason`, and `supporting_event_ids`. Falls back to `session_search` + `MEMORY.md` when sidecar is unavailable.
- **Bundle puller loop** (`internal/memory/bundle.go`) — 24h ticker with configurable startup delay; `NotifySyncDone()` channel wakes the puller out-of-schedule after a successful session sync. Atomic install via staging dir → `rename` → `current` symlink swap (POSIX-atomic). SHA256-verifies every file. `retain(3)` prunes old bundles to the newest 3.
- **`OnSyncDone` hook** (`internal/daemon/server.go`) — wires `memSvc.NotifySyncDone()` into the sync loop so a successful session upload immediately triggers a bundle freshness check.
- **`MemoryStatus` on `GET /status`** — `{ provider: "enabled"|"disabled", reason: null|"startup_timeout"|"repeated_crash"|"tlm_binary_missing"|..., detail: { restart_attempts: N } }`. Updated every 5s by the existing polling loop.

### Fixed

- **`memory_recall` string-encoded array coercion** — TLM occasionally returned `relation_candidates` / `scope_clues` as JSON-encoded strings (`"[...]"`) instead of arrays. Input validator now detects and re-parses these before the pydantic validation step, eliminating `extraction_tool_invalid_input` skips on those sessions.
- **`direct_relation` no longer requires `relation_constraints`** — the field is optional for direct-relation queries; requiring it was blocking valid queries. `relation_constraints` remains required for `path_query`.
- **CLI / TUI memory config now uses runtime overlays** (`cmd/root.go`, `internal/tui/app.go`) — both paths now call `memory.LoadConfigFromRuntime(runCfg)` instead of reading from process-global viper. Project-local `.shannon/config.yaml` memory overrides (`socket_path`, `provider`, `tlm_path`) now take effect for one-shot and TUI runs.

### Changed

- **`sync.enabled` default is now `true`** — session sync is on by default when `cloud.api_key` and `cloud.endpoint` are configured. OSS users without credentials skip each tick with a single log line; no user-visible impact. Disable with `sync.enabled: false` or the Episodic Memory toggle in Kocoro Desktop settings.

### Cross-repo consumers

- **ShanClaw Desktop 0.1.5**: helper bundle rebuilt against this tag. Episodic Memory toggle in Settings → Advanced → Beta controls `memory.provider` + `sync.enabled` together via `PATCH /config`.
- **Shannon Cloud**: `UpsertTenantTrainState` (PR #128) ensures the first accepted session sync immediately schedules training. `cloud_memory_enabled` feature flag must be set per tenant for the manifest endpoint to serve bundles.
- **tensorlogic-memory**: sidecar binary (`tlm`) must be at `v0.6.0`; bundle format version `0.6.x` required. Earlier bundle versions are rejected at the version gate (`versionInRange`).

---

## v0.1.4 — 2026-05-09 — Image generation + approval broker hardening

Adds `generate_image` and `edit_image` cloud tools, fixes the approval broker for `DisallowsAutoApproval` tools so they always route through a human decision, and patches the memory bundle gate to accept v0.6 bundles.

### Added

- **`generate_image` tool** — calls Shannon Cloud `POST /api/v1/images/generations`. Requires `cloud.enabled: true` + `api_key`. Returns an inline image result; per-call approval gated via `DisallowsAutoApproval`.
- **`edit_image` tool** — calls Shannon Cloud `POST /api/v1/images/edits`. Same cloud + approval requirements as `generate_image`. Accepts an existing image path + prompt; returns edited image.

### Fixed

- **`DisallowsAutoApproval` tools now route through approval broker** (`internal/daemon`) — image tools and other per-call-gated tools were bypassing the broker on the daemon WS path. Now correctly sends an `approval_request` envelope and waits for the human decision rather than auto-approving.
- **Memory bundle gate accepts v0.6 downloads** (`internal/memory`) — `versionInRange` was rejecting `0.6.x` bundles; upper bound raised to accept the current TLM bundle format.
- **Prompt length uses rune count** (`internal/tools`) — image prompt length validation was byte-counting; switched to `utf8.RuneCountInString` so CJK prompts are not incorrectly rejected.
- **Generative UI skill scoped to visualization only** — skill description tightened to prevent the model from using the HTML artifact path for general-purpose output.

### Docs

- Image tool registration guide added to CLAUDE.md / AGENTS.md.

---

## v0.1.3 — 2026-05-08 — Cross-repo coordination + publish_to_web

Bundles two cross-repo tracks and one major new tool. The WS handshake + `delivery_ack` capability close the loop with Shannon Cloud's Phase 4 inbound queue / replay buffer (Cloud-side ships in parallel, gates on the capability so old daemons stay on legacy fire-and-forget). The new **publish_to_web** tool (#116) ships permanent-public-URL file upload with multi-layer guards and a framework-level per-call approval gate. 429 sub-codes are now properly disambiguated so quota / credits-exhausted users see actionable messages instead of the generic "try again in a moment". Plus the **agent preamble** feature (#115) that gives Slack / Feishu / Desktop users live "about to run X" narration between tool calls.

### Added

- **`publish_to_web` tool** (#116) — uploads a file to Shannon Cloud's `POST /api/v1/uploads` and returns a permanent, public URL. Activated when `cloud.enabled: true` AND `api_key` is configured. Defense-in-depth: required `purpose` arg surfaced in approval UI; path-segment blocklist (`.env`/`.ssh`/`credentials`/`id_rsa`/...) on user-supplied AND symlink-resolved path; basename suffix blocklist (`.pem`/`.key`/`.p12`/`.pfx`/`.jks`/`.keystore`/`.asc`/`.gpg`) including disguised double-extensions like `*.key.txt`; extension allowlist (html/md/txt/pdf/png/jpg/svg/csv/json/mp4/... by default, extensible via `cloud.publish_allowed_extensions`); 50 MiB pre-check; multipart streaming via `io.Pipe`; 3-attempt retry with 1s/2s/4s backoff.
- **`agent.SkillExempt` framework interface** (#116) — pure-infrastructure tools (`think`, `tool_search`, `use_skill`) opt out of skill `allowed-tools` enforcement. An inventory test pins the allow/deny set across 22 production tools (file / shell / network / macOS-GUI / schedule / cloud / MCP wrappers); copy-pasting `SkillExempt() bool { return true }` onto a side-effecting tool is now a test failure.
- **`agent.DisallowsAutoApproval` framework helper** — names tools requiring a fresh human decision per call. Wired into every previously-blanket-returns-true approval gate: scheduler, heartbeat TranscriptCollector, daemon `auto_approve` config, daemon WS handler, CLI `--yes`, TUI session-allow + always-allow, HTTP one-shot, SSE handler. Per-call tools also reject session-level "always-allow" persistence; users see a one-shot notice via `EventApprovalNotice`. Currently lists `publish_to_web`.
- **WS upgrade handshake** (`User-Agent`, `X-ShanClaw-Daemon-Version`, `X-ShanClaw-Capabilities`) — daemon advertises version + capability tokens on every connect so Shannon Cloud can gate optional protocol features per-connection. Empty / absent header = legacy mode (forward-compat with older daemons).
- **`delivery_ack` capability + emission** — daemon sends a `MsgTypeDeliveryAck` envelope (top-level `MessageID`, no payload) after every successful `SendReply`. Cloud's 5-min replay buffer drops the entry on ack; un-acked messages (crash, network drop pre-reply, ctx cancel) are replayed on reconnect. Capability advertised by default.
- **Sender-suffix routing for messaging platforms without thread** — `ComputeRouteKey` now appends `<sender>` for messaging-source + no-ThreadID + Sender-present. New shapes: `default:<source>:<channel>:<sender>` and `agent:<name>:<source>:<channel>:<sender>`. Backward-compat: empty Sender keeps the legacy `default:<source>:<channel>`. Fixes WeCom group multi-user collisions (WeCom has no thread concept).
- **Agent preamble** (#115) — agents narrate "about to run X" between tool calls. New `OnPreamble(text)` callback split off from `OnText`; daemon emits `agent_text` SSE event; TUI renders preamble in dim style; system prompt rebalanced to permit brief narration without flooding prose.
- **`CodeQuotaExceeded` + `CodeCreditsExhausted` run-status codes** (`internal/runstatus`) — replace the everything-is-`CodeRateLimited` collapse for HTTP 429 responses.
- **`runstatus.FriendlyMessageFromError` with templated rendering** — substitutes `reset_at` + `window` into the quota message; renders the auto-refill variant for credits. Stable prefixes preserved so `IsFriendlyMessage` (and thus context-shaping drop logic) recognizes templated forms.
- **`cloud.publish_allowed_extensions` overlay merge** — project + local config can extend the default extension allowlist for publish; endpoint, API key, enablement, and timeout remain process-scoped.

### Fixed

- **429 sub-code disambiguation** (`internal/runstatus/parse.go`) — was substring-matching `"429"` and collapsing four very different gateway responses (token quota exceeded, credits exhausted, per-window throttle, upstream Anthropic throttle) onto `CodeRateLimited`. Quota-locked and credits-exhausted users were getting the actively misleading "please try again in a moment" — the cap was locked until the next reset, retrying did nothing. Now uses `errors.As(*client.APIError)` first, parses the JSON body, routes by `error` field shape (object = upstream; string = switch on value). Plain string-wrapping (no `%w`) loses the type and falls back to the coarse `CodeRateLimited`, documented in tests.
- **`multiHandler.OnPreamble` fan-out test gap** — `TestMultiHandlerFansOutBaseMethods` declared a preamble counter but never invoked / asserted it. If the fan-out were ever silently no-op'd, every daemon channel (Slack / Feishu / Desktop bus) would drop preamble events with no test failure. Added the missing invocation + assertion.
- **TUI session-level "always-allow" now respects `DisallowsAutoApproval`** — closes a path where prior approvals on other tools could re-grant the per-call gate.
- **Sensitive-name guards catch disguised double extensions** — `id_rsa.key.txt`, `server.key.txt`, `credentials.json`, `.env.local.txt` now rejected via the suffix-anywhere check + reused `permissions.IsSensitiveFile` patterns.

### Changed

- **`runstatus.CodeFromError`** now prefers `errors.As(*client.APIError)` for structured extraction; substring-matching is the fallback for errors without the type wrapper.
- **`runstatus.IsFriendlyMessage`** extended with `HasPrefix` matching so templated quota / credits messages are recognized as friendly errors and dropped during context shaping.
- **Default `daemon.Capabilities`** is now `["delivery_ack"]`. Old daemons stay legacy; new daemons activate Phase 4 tracking automatically when Cloud's side ships.
- **`vaguePurposes` blocklist now reachable** — vagueness check moved before length check; whitespace normalization added; longer phrases (`"for testing"`, `"share with team"`, `"send to user"`, etc.) added so realistic LLM fallback purposes are caught.

### Docs

- CLAUDE.md / AGENTS.md updated for: WS handshake & capabilities, `delivery_ack` contract, sender-suffix route-key precedence ladder, `runstatus/parse.go` file purpose.
- Kocoro skill `references/agents.md` Reset note now mentions clearing the persisted route binding.

### Cross-repo consumers

- **Shannon Cloud**: capability handshake is the prerequisite for Phase 4 unacked-tracking + replay-on-reconnect. Cloud-side gates on `"delivery_ack" in conn.capabilities`; old daemons → no tracking → legacy fire-and-forget. The 429 body schemas Cloud emits (per `middleware/quota.go`, `middleware/ratelimit.go`, `openai/handler.go`) are now parsed properly on the daemon side.
- **ShanClaw Desktop**: helper bundle should rebuild against this tag's SHA to pick up the daemon changes. Templated quota / credits messages currently render as the static fallback in the TUI — full templating needs `RunStatus` to carry `*runstatus.Detail`, deferred to a follow-up.
- **npm `@kocoro/shanclaw`**: release CI publishes against this tag.

### Versioning note

Patch bump in the v0.1.x line. `publish_to_web` is additive (cloud-gated), the `SkillExempt` + `DisallowsAutoApproval` framework is BC, and the WS handshake is forward-compat. No breaking runtime contracts.

## v0.1.2 — 2026-05-07 — Tool-layer cost optimization + release-blocker fixes

Bundles PR #114 (tool-layer cost optimization), PR #113 (webhook agent isolation), the daemon WS approval-message fix, and the five release-blocker fixes that came out of the cross-branch code review.

### Added
- **Per-turn 200K aggregate cap on tool results** (`internal/agent/spill.go`) — mirrors Claude Code's `MAX_TOOL_RESULTS_PER_MESSAGE_CHARS`. When parallel tools return >200K runes total, the largest results spill until the aggregate drops back under the cap.
- **Per-tool result spill policy + unified spill path** — `MaxResultSizeChars` per tool: default 50K runes; `grep` ~20K; `file_read` is `UnlimitedToolResultSizeChars` and falls back to the 50K spill threshold. Spill files at `~/.shannon/tmp/tool_result_<session>_<call_id>.txt`.
- **Persisted tool-result budget state** (`internal/agent/toolresult_budget.go`) — `ToolResultReplacements` + `ToolResultSeen` on `session.Session` survive across turns and resume; mid-turn checkpoints (`applyTurnState`) and both terminal save paths persist them.
- **Context-bloat run-status nudge** (`internal/agent/context_bloat.go`) — `OnRunStatus("tool_result_bloat", …)` surfaces when a single tool's per-turn output exceeds the bloat threshold; SSE/Desktop subscribers can show why a loop slowed.
- **`file_read` dedup with daemon session cache** (`internal/agent/readtracker.go` + `internal/daemon/readtracker_cache.go`) — repeat reads of the same `(path, offset, limit)` return a short "unchanged since last read" stub when mtime/size match; one tracker per session, released via `SessionManager.OnSessionClose`.
- **`grep` precise search controls** — `output_mode` (default `files_with_matches`, also `content`/`count`), `glob` filter list, `head_limit`, `offset`, `type`, `ignore_case`, `multiline`, `before_context`/`after_context`, and `sort_by` (`mtime` newest-first). VCS metadata (`.git`, etc.) auto-skipped; rg uses `--max-columns 500` to cap minified-line output.
- **`file_edit` `replace_all` parameter** — opt-in to rewrite every occurrence (useful for renames); `old_string` uniqueness still enforced by default.
- **`bash` caller-controlled output cap** — default 30K-char head+tail truncation; `max_output_chars` overrides (raise or lower).
- **`file_read` streaming + oversized-error guard** — bounded reads stream via `bufio.Scanner`; reads estimated above ~25K tokens return an error directing the caller to use `offset+limit` instead of falling back to spill.
- **`think` ack-only result** — thought is captured in the tool call; result returns a short ack so the prose does not echo back into context. ~50% reduction in think-related cache writes.

### Fixed
- **`CancelBySessionID` data race** — `routeEntry.sessionID` is now `atomic.Pointer[string]`; the cancel scan reads lock-free instead of taking `sc.mu` and reading a field protected by `entry.mu`. Reviewer-flagged on PR #113.
- **`Manager.Delete` callback wiring** (`internal/session/manager.go`) — fires registered `OnSessionClose` callbacks, holds the manager lock across `store.Delete` so concurrent `Save` cannot recreate the file mid-delete, and leaves in-memory state intact when the disk delete fails.
- **`ReadTrackerCache.Forget` lifecycle** — daemon registers `Forget(sessionID)` as an `OnSessionClose` hook so per-session tracker entries no longer leak for the daemon's lifetime.
- **`applyAggregateCap` byte/rune unit mismatch** — char counting now uses `utf8.RuneCountInString`, matching per-result spill and `applyToolResultBudget`. CJK/emoji content no longer fires the cap ~3x early.
- **Final-save and hard-error save paths persist budget state** — both terminal `runner.go` save paths copy `ToolResultReplacements` + `ToolResultSeen` from the loop, so fast turns and crashed turns retain dedup/replacement bookkeeping on resume (was previously only saved by mid-turn checkpoints).
- **`file_read` offset-without-limit slicing** — when `offset > 0` and `limit <= 0`, the unlimited-read branch now slices `lines[start:]` before printing; line numbers are correct rather than shifted by `offset`.
- **WS envelope `MessageID` on `approval_request`** — `cmd/daemon.go` passes the inbound claim's MessageID into `ApprovalBroker.Request` and `Client.SendApprovalRequest` stamps it onto the envelope. Empty MessageID triggered Cloud's fail-closed drop; users never saw the approval card and the tool call hung until timeout.
- **Webhook agent isolation + thread-route bindings** (#113) — `ComputeRouteKey` no longer collapses webhook/cron/schedule traffic onto `agent:<name>`; persisted thread-route bindings prevent silent cross-channel session sharing.
- **Inject ack suppression on messaging platforms** — `InjectMessage` no longer surfaces a confusing "ok" reply on follow-up turns to messaging channels.

### Changed
- **Default grep `output_mode` flipped to `files_with_matches`** — previously returned match lines; users/agents that relied on the old default need to pass `output_mode: "content"` explicitly.
- **`file_read` now hard-errors on oversized reads** instead of spilling — historically a >256KB read fell through to spill; now returns `"file is too large… Use offset+limit"` to nudge ranged reads.
- **Kocoro skill** — instructions forbid translating user-provided agent slugs (e.g. Pinyin → Chinese); pass byte-for-byte or ask for a valid slug.

### Docs
- README, CLAUDE.md, AGENTS.md updated for the tool-description changes (grep `output_mode`, `file_edit replace_all`, `bash max_output_chars`, `think` ack-only, `file_read` dedup + 25K throw) and for the new agent files (`toolresult_budget.go`, `context_bloat.go`) and daemon file (`readtracker_cache.go`). New "Tool Result Sizing" subsection in README.

## v0.1.1 — 2026-05-06 — Messaging-platform routing hardening

### Fixed
- **Per-thread route keys for messaging platforms** (`internal/daemon/router.go`) — `ComputeRouteKey` ignored `ThreadID` for default-agent traffic on Slack, WeCom, Feishu, LINE, etc., collapsing every group/DM/thread under one bot/source onto a single route key. A second message arriving while the first was in-flight was silently injected into the running loop via `SessionCache.InjectMessage`; two prompts merged into one LLM call, the reply landed only in the originating thread, and the other thread saw the friendly-error fallback. New shape: `agent:<name>:<source>:<thread>` (or `default:<source>:<thread>`) for messaging platforms with a non-empty ThreadID. `isPlainAgentRouteKey` distinguishes plain `agent:<name>` from the new thread-scoped form at the cold-start switch arms.
- **`ShapeHistory` orphaned tool-pair guard** — the positional `keepLast*2` cut could land between an assistant `tool_use` and the matching user `tool_result`, leaving an orphan that Anthropic rejects with HTTP 400. Runs `stripOrphanedToolPairs` on the assembled output of `buildShaped` — intentionally narrower than `SanitizeHistory`, which would merge consecutive role=user messages and drop the original first prompt.
- **`@mention` agent fallback skipped on messaging platforms** (#112) — for Slack/Feishu/Lark/WeCom/LINE/WeChat/Teams/Discord/Telegram the gateway delivers an explicit `AgentName` (empty = "use default"). Dispatch no longer falls back to `ParseAgentMention(msg.Text)`, which previously broke group chats where the literal `@<botname>` prefix is part of the inbound text.

## v0.1.0 — 2026-05-01 — Prompt-cache stability + observability

### Added
- **Time-gated `tool_result` compaction** (#108) — replaces the per-iteration in-place rewrite that was busting the prompt-cache prefix every turn. New `internal/agent/timebasedcompact.go` fires only when the gap since the last assistant response exceeds a threshold, and keeps a configurable trailing window of full-fidelity blocks. Off by default — opt-in per rollout via `agent.time_based_compact.{enabled, gap_threshold_minutes, keep_recent}` (defaults `false`, `60`, `5`). Companion idempotency suite (`cache_idempotence_test.go`, `microcompact_test.go` updates, `compact_event_test.go`) locks that re-running compaction never re-mutates already-compacted blocks.
- **Cache-debug instrumentation layer** — `SHANNON_CACHE_DEBUG=1` writes JSON-lines logs with per-tool / per-message / per-block hash ladders + `cache_summary` rows; `SHANNON_CACHE_DEBUG_RAW=1` adds full request bytes per call (LRU 100 dirs, override `SHANNON_CACHE_DEBUG_RAW_MAX`). All in-place `messages[idx].Content` rewrites in the agent loop are now required to call `client.LogCacheCompactEvent` so cache-debug.log explains every prefix-byte drift; uninstrumented mutation paths break drift attribution silently. Operator guide at `docs/cache-debug.md`. Logs use `0700/0600` perms.
- **BP #1 byte stability for cross-user cache hits** (#110) — tool listing moved out of the system prompt (where per-user tool sets were invalidating the cache) and into the user message via `BuildToolListing`; `## Deferred Tools` section likewise relocated. `PromptOptions` now takes `LocalToolNames` / `MCPToolNames` / `GatewayToolNames` partitioned by source instead of a merged list (dead `ServerTools` / `ToolNames` fields removed). `cache_summary` audit row gains `system_stable_hash` for cross-user CHR analysis. Re-runnable token-distribution audit at `internal/agent/promptaudit_test.go`.
- **`http` tool: `body_from_file` param** (#111) — sends file bytes verbatim, fixing JSON-string escape errors on long structured payloads. `IsSafeArgs` tightened: any request body now requires approval. `kocoro` SKILL.md + `references/instructions.md` updated to teach `body_from_file` for long content (otherwise the model keeps re-trying inline JSON and hitting the same escape failure).
- **Daemon `PUT /instructions` accepts raw markdown** (#111) — `Content-Type: text/markdown` or `text/plain` lands raw bytes on disk; existing JSON contract preserved as the default. Test coverage in `internal/daemon/instructions_test.go`.
- **`wait_for` joins the macOS GUI defer family** in `toolbudget.go` so `computer/screenshot/applescript/accessibility/wait_for` cold-start defers as a unit.

### Fixed
- Reactive compaction events from in-place message rewrites are now wired to the cache-debug compact-event API; previously these mutations were invisible in drift attribution.
- Time-gated tool_result clearing replaces a per-iteration compaction path that mutated already-compacted blocks under certain corner cases.
- `macOSAutomationGuidance` no longer reads the stale `ToolNames` field after the system-prompt refactor.
- `cache_summary` audit rows force `WarmStart` onto the wire (regression-locked by `TestAuditLogger_CacheSummary_WarmStartTrue_RoundTrips` — `omitempty` made the false case indistinguishable from "field always missing").

### Changed
- `applySkillFilter` removed from the schema-filtering path (it was already disabled, but dead code is gone). Skill `allowed-tools` enforcement remains execution-time-denial only — the tools array stays full for the life of `Run()` so `toolSchemas` stays byte-stable for the cache.

## v0.0.102 — 2026-04-28

### Added
- **HTTP slash routing for `/research` and `/swarm`** — `POST /message` now recognizes `/research [strategy] <query>` and `/swarm <query>` slash prefixes (SSE only) and dispatches directly to Shannon Cloud's Gateway, bypassing the local agent loop. Previously slash commands were TUI-only; HTTP clients (including Kocoro Desktop) had to rely on the model invoking `cloud_delegate`. The done event carries the same `RunAgentResult` JSON shape as regular agent runs, so existing SSE consumers need no changes. New `internal/cloudflow/` package extracts the shared Gateway SSE bridge from `cloud_delegate`.
- **Permissions: always-ask gate for high-risk prefixes + token-prefix family matching** (#106) — high-risk prefixes (e.g. `git push`, dangerous flags/refspecs) and bare `&` / `(...)` subshell splitting now precede the allowlist; `IsAlwaysAskPrefix` blocks daemon/CLI from persisting these into `permissions.allowed_commands`. Token-prefix family matching for the allowlist (depth N=2 for known CLIs, N=3 for unknowns) cannot widen scope past the always-ask gate.

### Fixed
- **Slash-workflow plumbing** — slash workflows honor `cloud.timeout`, support cancel, populate agent metadata, support warm-resume on reconnect, and reach run-state parity with the local agent path.
- **Router race**: `cancelPending` is now cleared under `sc.mu` in `TryLockRouteWithManager` (prevents a window where a cancellation token leaks to the next route holder).

## v0.0.101 — 2026-04-27

### Added
- **Event bus enrichment** — `tool_status` (running/completed), `run_status`, and `usage` snapshot events emitted to the EventBus ring buffer; `multiHandler` fan-out wires `busEventHandler` into all RunAgent paths so SSE subscribers and Desktop get a unified real-time event stream.
- **Per-request SSE tool events enriched** — elapsed time, `is_error`, and redaction-boundary semantics aligned between per-request SSE and bus emissions.
- **Hidden skills flag** — `hidden: true` in skill frontmatter excludes internal skills (e.g. `kocoro-generative-ui`) from `GET /skills` listing while keeping them loadable via `use_skill`; flag preserved across `WriteGlobalSkill` round-trips; `GET /skills/{name}` exposes it on `SkillDetail`.
- **kocoro-generative-ui bundled skill** — inline visualization assistant teaching the agent to emit `html-artifact` fenced blocks rendered in Kocoro Desktop's sandboxed WKWebView; reference files cover charts, diagrams, maps, SVG, and UI components.
- **Kocoro identity + language anti-drift policy** — persona rebrand to Kocoro; language policy added to prevent identity drift across long sessions.
- Skill secrets API endpoints: `PUT/DELETE /skills/{name}/secrets` and `GET /skills` returns `required_secrets` + `configured_secrets` (values never exposed).
- `metadata.clawdis` accepted as third ClawHub spec alias alongside `openclaw` and `clawdbot`.
- heatmap-analyze skill: API-key acquisition walkthrough; EN+JA official copy with reply-language rule.

### Fixed
- **Agent reliability triad**: loop-detector args-uniqueness gate prevents batch-tolerant tool thrash; force-stop now synthesizes a structured partial report; empty-result rule narrowed to distinguish retry vs diversify (user-named scope wins, `http` excluded).
- `writeVerbs` blacklist expanded; compound-verb MCP tool names rejected from batch-tolerance.
- Benchmark analyzer unifies synthesis detection and handles `force_stop` audit events.
- Skills: frontmatter `name` decoupled from marketplace slug — `Slug` used everywhere directory/URL/manifest identity is needed; secrets lookup uses `Slug`.
- Daemon: `daemon.auto_approve` settable via `PATCH /config`.
- Kocoro skill: drop sticky-instructions after opt-in revert; post-create hint steers to ShanClaw Desktop.

## v0.0.98 — 2026-04-20

### Added
- **Phase 2.3 memory client** — sidecar lifecycle (spawn / health / restart / shutdown), 24h bundle puller with tenant fingerprint, `memory_recall` tool with `session_search` + `MEMORY.md` fallback, CLI/TUI attach-only path via `NewServiceAttached`, full daemon wire-up.
- **Daily session sync** — opt-in upload of `~/.shannon/sessions/` to Shannon Cloud with flock + atomic marker, per-session ACK, persistent failed-entry bookkeeping, oversized + load-error permanent rejection.
- **Three-layer skill discovery** — skill descriptions embedded in scaffolded first user message (4000-char budget, rune-safe), semantic prefetch on iteration 0 (`model_tier: small`, 5s timeout, gated by `agent.skill_discovery`), fallback catalog in `use_skill` tool description.
- **Skill secrets management** — per-skill API keys stored in the macOS Keychain via `zalando/go-keyring` (pure Go, no CGo; password passed via stdin not argv). Plaintext index at `~/.shannon/secrets-index.json` tracks configured key names; values are env-var-injected into `bash` only for skills activated via `use_skill` within the current run.
- **heatmap-analyze bundled skill** — Ptengine heatmap analysis with `install.sh`.
- **kocoro setup skill** — platform-configuration assistant teaching the agent to manage ShanClaw via the daemon HTTP API.
- **Cache-source TTL routing** — `cache_source` tags every LLM call; 1h cache for channel/TUI, 5m for one-shot/subagent; `SHANNON_FORCE_TTL` override.

### Fixed
- Runtime hardening: skill-discovery guards, sticky policy routing, tool error semantics.
- MaxIter graceful finalize synthesizes a partial report; `Partial` flag corrected.
- Sync CLI path: `config.Load()` runs before sync; `cloud.*` aliases canonicalized.
- Memory cold-start bootstrap via `os.Stat`.
- Usage accounting pipeline and cache breakdown corrections.

## v0.0.96 — 2026-04-14

### Added
- Inline base64 image blocks materialized to `~/.shannon/tmp/attachments/<nonce>/` with model-visible path hints, so agents use real attachment tools instead of hallucinating replicas (#62).
- MCP workspace roots advertised to servers honoring the roots capability — `browser_file_upload` accepts staged attachment paths (#63).
- CJK-aware FTS5 session search via trigram + short-query fallback (#60).
- Family-aware no-progress nudges; `[system]` prefix on harness-injected messages.

### Fixed
- Session-edit API preserves multimodal content on resend (#61).
- Reanchor message preserves current-turn text blocks across deferred-tool / post-compaction / retry boundaries.
- Browser upload recovery hints and loop-detector scoping prevent retries into closed file choosers.

## v0.0.95 — 2026-04-13

### Added
- Remote file attachment download pipeline for Slack and Feishu (#54).

### Fixed
- `bash` NoProgress threshold raised to prevent premature force-stop.
- Double-encoded `tool_use` input unwrapped for OpenAI-shaped providers.
- Request config preserved and partial state surfaced on force-stop.

## v0.0.94 — 2026-04-11

### Fixed
- Playwright Chrome profile clone lifecycle: update ordering and sync, state kept consistent during reset (#52).
- Closed remaining process-cwd leaks in readtracker and session manager (#51).

## v0.0.93 — 2026-04-11

### Fixed
- `readtracker` no longer falls back to daemon process CWD when no session CWD is set — scopeless relative paths stay distinct from their absolute form.
- Removed dead `getCWD()` helper from session manager.
- Regression test locks in the new contract.

## v0.0.92 — 2026-04-06

### Added
- **Delta injection** — `DeltaProvider` interface polled at loop iteration boundary. Ships `TemporalDelta` (date rollover detection). Delta messages visible to model mid-run but excluded from session persistence.
- **Contrast examples** — 5 GOOD/BAD behavioral pairs targeting cowork failure modes (over-engineering, coding-default bias, premature completion, narrating instead of acting, wrong cloud/local boundary). Cloud/local pair conditional on `cloud_delegate` availability.
- **Bundled specialist agents** — `@explorer` (read-only orientation) and `@reviewer` (critical evaluation) embedded via `embed.FS`, synced to `_builtin/` on startup. Two-step `LoadAgent` resolution (user > builtin). CRUD protection with full-snapshot materialization before writes.
- **Session-scoped CWD** — each run carries its own project directory, resolving the daemon CWD gap. Priority cascade: request `cwd` → resumed session → agent config `cwd` → process fallback.
- **Structured inject payload** — follow-up injection uses `InjectedMessage` instead of raw text. Active-run CWD is immutable (different-CWD follow-ups return `cwd_conflict` 409).
- **Project config overlay** — project-local config loaded at runtime from session CWD, scoped to session-safe fields (`model_tier`, `agent.*`, `tools.*`, `permissions.*`). Process-global settings (`endpoint`, `api_key`, `mcp_servers`, `daemon.*`) no longer overridden.

### Fixed
- `listAgentNames` returns `([]string, error)` — propagates I/O errors, only swallows `os.IsNotExist`.
- `EnsureBuiltins` uses `os.CreateTemp` for race-safe temp files.
- `GET /agents/{name}` matches `ListAgents` semantics: `Builtin=true` only when no user override exists.
- Path traversal canonicalization and symlink escape prevention in `IsUnderSessionCWD`.
- Cold-start resume treats empty resumed session as fresh.
- Heartbeat CWD carryover and one-shot validation.
- `cloud_delegate` deep-copied per-run to prevent concurrent daemon route races.

## v0.0.91

### Added
- **Context quality Phase 1–3** — compaction floor, session-scoped tool warming, reactive compaction recovery

### Fixed
- Agent skill CRUD aligned with manifest-based attachment model
- Spill cleanup lifetime scoped to session, spurious `OnToolCall` suppressed
- TUI rendering: header duplication, resize, response positioning

## v0.0.9

### Added
- **Prompt cache stability** — `PromptParts` (static/stable/volatile) split, `ToolSourcer` sorted ordering, cache telemetry
- **Context management** — tiered compaction with head+tail truncation, reactive compaction on overflow, two-phase compression with analysis scratchpad, micro-compact LLM summary, memory staleness annotation
- **Tool safety** — partitioned batch execution (read-only parallel, writes serialized), disk spill for large results (>50K), deferred tool loading (`tool_search` meta-tool)
- **Output format profiles** — channel-aware formatting (`markdown` for TUI/web, `plain` for Slack/LINE/Telegram/webhook)
- **Self-awareness and system reminders** — reinforcement hints in long sessions
- `OnToolCall` fires at actual execution start (post-semaphore)
- `ax_server` bundled mode with Unix socket transport
- `cloud_delegate` terminal param for loop continuation control

### Fixed
- Deferred `tool_search` continuation (model proceeds after schema load)
- Cache ratio formula corrected for Anthropic token semantics
- Volatile context stripped from persisted session history
- API key whitespace trimmed in all config load/save paths
- Per-message timestamps in session persistence

## v0.0.8

### Added
- **Manifest-based skill attachment** for agents (name-only attachment, replace semantics)
- Bundled skills moved to installable, hidden from default skills list

### Fixed
- Playwright CDP lifecycle: lazy-launch, race conditions, Chrome cleanup
- CDP Chrome launched offscreen to prevent window flash
- Orphaned CDP Chrome cleanup after daemon hard kill
- Bundled skills removed from runtime loading (global-only resolution)
