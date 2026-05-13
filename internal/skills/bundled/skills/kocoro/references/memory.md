# Memory feature

Kocoro includes a `memory_recall` agent tool backed by a local memory
sidecar. The daemon manages the sidecar's lifecycle and (in cloud mode)
periodically pulls fresh memory bundles from Kocoro Cloud. Episodic Memory is
**opt-in** — disabled by default; users enable it from Kocoro Desktop
Settings, which also flips on session sync as part of the same toggle. Three
modes:

- **disabled** (default): no sidecar; tool falls back to `session_search`
  and the agent's MEMORY.md.
- **cloud**: paid feature. Daemon polls Kocoro Cloud for bundle manifests
  every 24h, downloads + verifies + atomically installs, then triggers a
  sidecar reload.
- **local**: self-host. User builds + publishes bundles themselves; daemon
  spawns the sidecar but never calls Cloud.

## Required configuration (cloud mode)

```yaml
cloud:
  api_key: <your-tenant-key>
  endpoint: https://api.shannon.run
memory:
  provider: cloud
```

Optionally override the key/endpoint just for memory:

```yaml
memory:
  api_key: <separate-memory-key>      # falls back to cloud.api_key when empty
  endpoint: https://memory.shannon.run # falls back to cloud.endpoint when empty
```

## Diagnostics

Health probe via curl:

```bash
curl --unix-socket ~/.shannon/memory.sock http://unix/health
```

Expected `ready: true` once the sidecar has loaded a bundle. If the
probe fails, the daemon log will show one of these audit events:

- `memory_tlm_missing` — `tlm` binary unresolved (set `memory.tlm_path` or
  add to `$PATH`)
- `memory_cloud_misconfigured` — `cloud` mode with empty endpoint or key
  (boolean fields `endpoint_resolved`, `api_key_present` indicate which)
- `memory_sidecar_degraded` — restart budget exhausted (3 crashes); the
  tool falls back until daemon restart
- `memory_tenant_switch` — fingerprint mismatch detected, bundles wiped
- `memory_bundle_unsafe_path` — manifest contained a path that escaped
  the sandbox; install aborted
- `memory_reload_failed` — bundle installed but `/bundle/reload` POST
  failed; sidecar's own poller will pick up the new symlink eventually

## Implicit episodic preflight

Before the first main-model call on a memory-relevant turn, the daemon runs
a preflight: a small-tier helper compiles `QueryIntent`s via forced
`tool_use`, the sidecar resolves them, and a `<private_memory>` block is
injected into the current user message before it reaches the main model.
Many memory questions are answered on turn 0 without an explicit
`memory_recall` invocation.

- Fires only when sidecar status is `Ready`. With sidecar unavailable, the
  agent falls back to the `memory_recall` tool's degraded path described
  below.
- The `<private_memory>` block is in-message-only — never persisted to the
  session transcript, never replayed, and stripped from compaction summaries
  at every `GenerateSummary` site.
- Audit event `memory_preflight` records a content-free trace:
  `attempted` / `helper_used` / `intents_count` / `results_count` /
  `context_injected` / `outcome` / `error_class` / `http_status`. Query
  text, anchor mentions, relation labels, and recalled content are never
  logged.
- Outcomes worth tracing (the rich set is set inside the preflight; loop.go
  only fills `Outcome` if still empty):
  - `context_injected` — happy path, model received the block
  - `context_returned` — preflight produced a block but injection was
    skipped upstream (rare)
  - `no_results` — intents compiled but the sidecar found nothing
  - `no_context` — results returned but every group was filtered
  - `no_intents` / `helper_declined` / `gate_declined` — preflight
    intentionally skipped (greeting / task-text / non-memory prompt)
  - `query_timeout` — sidecar exceeded its per-intent budget
  - `helper_error` — small-tier helper call failed; cross-reference
    `error_class` (`no_tool_call`, `wrong_tool`, `invalid_tool_args`,
    `nil_response`, `unknown`)
  - `memory_unavailable` / `helper_unavailable` / `querier_unavailable` —
    degraded path; agent fell back to the explicit `memory_recall` tool

## Behavior when memory is unavailable

The `memory_recall` tool degrades gracefully — it returns a JSON
envelope with `source: "fallback"`, `evidence_quality: "text_search"`,
and a non-empty `fallback_reason`. The agent sees lower-confidence
results from session keyword search instead of structured candidates.

Switching `memory.provider` requires a daemon restart in v1.

## Privacy

The resolved API key bytes are never written to disk or audit payloads.
A truncated SHA256 fingerprint (`<bundle_root>/.tenant_fingerprint`)
serves as the cache-key for tenant-switch detection. When the
fingerprint changes, the bundle directory is wiped and re-pulled. Session sync
defaults to disabled and is flipped on alongside Episodic Memory by the
Desktop toggle; turning off Episodic Memory also disables session sync.
