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
- internal/daemon: primary production path; HTTP API server, WebSocket client, routing, approvals, events, launchd, attachments, session CWD, memory fallback, suggestions.
- internal/agent: core loop; tool batching, compaction, spill/budget state, deferred loading, state cache, read tracking, approvals, phase/watchdog, thinking handling, prompt suggestions, forked requests.
- internal/tools: local, gateway, cloud, schedule, publish/upload, image, memory, MCP, and document tools.
- internal/client: gateway/SSE/Ollama clients.
- internal/session: session persistence, lifecycle, titles, and SQLite FTS index.
- internal/config: config loading, merging, settings, and setup.
- internal/skills: skill registry, bundled skills, marketplace install, provenance, secrets, validation.
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

Bundled skills are overlaid into the user skill directory on startup and user edits are overwritten. Fork under a new skill name to customize. The hidden generative UI skill emits `html-artifact` blocks for Desktop's sandboxed WKWebView.

### Agent Names

Agent names must match `^[a-z0-9][a-z0-9_-]{0,63}$`. Validate before path construction.

### Tool Registry

Tool priority is local tools, then MCP tools, then gateway tools. Deduplicate by name. Skill `allowed-tools` is enforced at execution time, not by schema filtering, to keep prompt-cache tool arrays stable.

Skill-exempt tools must be pure infrastructure with no external side effects. Do not exempt side-effecting tools from skill restrictions.

### Tool Concurrency

The dispatcher batches tool calls by `IsConcurrencySafeCall`, not `IsReadOnlyCall`. Tools without an explicit `ConcurrencySafeChecker` implementation fall back to their `IsReadOnlyCall` value — so file_read / grep / glob etc. keep batching concurrently as before, and writers stay serial. Adding the new interface to one tool has no effect on any other tool's grouping.

`BashTool` implements `IsConcurrencySafeCall` via `internal/tools/bash_concurrency.go`. It is gated by `agent.bash_concurrency_enabled` (default `true` in Phase C — Desktop consumes the `tool_use_id_events` capability so id-keyed cards stay correct under concurrent batches). When the flag is on, commands whose first token is in a strict read-only whitelist AND contain no shell metacharacters (including `\n` / `\r`) are eligible for the concurrent batch. Everything else — `&&` / pipes / redirects / command substitution, plus any non-whitelisted leading token (`git push`, `npm install`, `curl`, `rm`, `git remote add`, `go env -w`, ...) — stays in a size-1 serial batch.

Tool events on the SSE/WS wire (`tool_status` running + completed) include a `tool_use_id` field so multi-tool-in-flight UIs (e.g. parallel bash) can pair them correctly. The daemon advertises this on the WS handshake via the `tool_use_id_events` capability token.

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

### Thinking Blocks

When native thinking is enabled, preserve assistant `thinking` and `redacted_thinking` content blocks in order across the conversation trajectory. Sanitizers, compaction, fork builders, and session persistence must not rewrite them. Strip thinking only before session sync upload.

The local `think` tool is skipped on the default gateway + native-thinking path, but remains available when thinking is disabled, with Ollama, or with the force flag.

### Browser Preview Bridge

File previews served to browser automation must stay fail-closed: allow only effective session CWD and attached paths, resolve symlinks on both sides, use random tokens, avoid directory listing, and tear down on session close.

### Config Merge

Global config, project config, and local project config merge in that order. Scalars override, lists merge/dedup, structs merge field-by-field. MCP server env var casing is preserved by direct YAML re-read.

### Atomic Writes

Persistent JSON indexes use write-temp, rename, and flock on a stable lock file. Never delete lock files; doing so can split locks across different inodes.

### Memory

Memory sidecar lifecycle belongs to the daemon. CLI/TUI attach or probe; they do not spawn unless explicitly designed to. API key bytes must never hit disk or audit logs; only content-free state and fingerprints are logged.

Implicit episodic preflight is in-message-only: it may inject private memory into the current request, but it must never persist to transcripts, replay, or summaries. Audit only content-free counts/status/error class.

### Session Sync

Session sync is opt-in. It uses a single Run entry point, flock, atomic marker writes, per-session ACKs, and persistent failed-entry bookkeeping. Permanent failures remain until the source session changes.

### Prompt Suggestions

Suggestion generation is a forked request after a successful main turn. Cache safety invariant: the fork must be byte-equal to the main request except for the appended assistant reply, suggestion prompt, skip-cache-write flag, and debug-only fork kind. Do not change tools, max tokens, thinking budget, or ordering in the fork.

### Prompt Cache

Source-routed TTLs matter: channels/TUI use long cache, one-shot/subagent/helper paths use cheaper short cache. Preserve `cache_source` propagation and canonical tool input normalization.

Any in-place message content rewrite that can affect prompt bytes must emit cache-compaction/debug instrumentation so drift remains attributable.

### Context Management

Context-window defaults are only seeds; model responses may auto-adjust the active window unless a per-agent override explicitly locks it. Preserve proactive, pre-flight, and reactive compaction as separate gates so context errors do not cascade.

### Anti-Hallucination

Keep random XML tool execution delimiters, suppress preamble when tool calls are present, and strip fabricated tool calls.

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

Schedule tests use temp directories and do not write to the real LaunchAgents directory. Run live E2E before releases.

## Build And Release

- GoReleaser builds releases.
- The npm package is `@kocoro/kocoro`.
- Default version bumps are patch-only unless explicitly directed otherwise.
- Release flow: tag, push tag, let CI build and publish.

## Tools

Core local tools include file ops, archive inspect/extract, document extraction, shell/system, macOS GUI, schedules, memory, and skills. Runtime-conditional tools include session search, cloud delegation, publish/list/retract uploads, image generation/editing, and deferred tool search.

Every approval-required tool must expose a short human-readable description or equivalent purpose field for approval UI. Destructive or paid/public cloud tools require approval.
