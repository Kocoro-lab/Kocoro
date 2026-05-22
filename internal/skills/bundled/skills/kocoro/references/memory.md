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
- `memory_sidecar_degraded` — restart budget exhausted OR confirmed
  schema-mismatch short-circuit; the tool falls back until daemon restart.
  Carries `reason` field (see enum below) and optional
  `compatibility`/`sub_code`/`bundle_version` when reason is
  `tlm_binary_too_old`.
- `memory_self_heal_attempt` / `memory_self_heal_ok` /
  `memory_self_heal_failed` — emitted when the supervisor's one-shot
  `onIncompatible` hook fires after detecting `incompatible_bundle`.
  Cloud-mode pulls a fresh bundle once before declaring the lockout
  unrecoverable from the daemon side.
- `memory_tenant_switch` — fingerprint mismatch detected, bundles wiped
- `memory_bundle_unsafe_path` — manifest contained a path that escaped
  the sandbox; install aborted
- `memory_reload_failed` — bundle installed but `/bundle/reload` POST
  failed; sidecar's own poller will pick up the new symlink eventually

## `memory_sidecar_degraded` reason enum

The `reason` field surfaced on both the audit event and `GET /status`
`memory.reason`:

- `tlm_binary_missing` — `tlm` binary not resolvable at all.
- `tlm_exec_error` — Spawn returned an OS-level error (e.g. permission).
- `tlm_health_failed` — `/health` probe failed in a way that wasn't a
  ready-window timeout.
- `startup_timeout` — sidecar never became Ready within the configured
  window for a non-schema reason (slow disk, hung process).
- `repeated_crash` — sidecar became Ready, then exited, repeatedly.
- `cloud_misconfigured` — `cloud` mode with empty endpoint or API key.
- `tlm_binary_too_old` — sustained `compatibility="incompatible"` with
  `sub_code in {"no_manifest","version_out_of_range"}` from the
  sidecar's `/health`. The most common cause is a local `tlm` binary
  older than the bundle manifest's schema (newer dataclass fields the
  binary can't unmarshal). Kocoro Desktop consumes this via
  `GET /status memory.reason` and prompts the user to re-run its
  on-demand `tlm` install. The supervisor fires its self-heal hook
  once (cloud-mode bundle pull) before short-circuiting to degraded
  — five restart attempts won't make a stale binary compatible.

## `GET /status memory.detail.repair_needed` shape

When `memory.reason == "tlm_binary_too_old"`, the `detail` map also
carries a `repair_needed` block with the last `/health` observation
from before the supervisor short-circuited:

```json
{
  "memory": {
    "provider": "disabled",
    "reason": "tlm_binary_too_old",
    "detail": {
      "restart_attempts": 2,
      "repair_needed": {
        "compatibility": "incompatible",
        "sub_code": "no_manifest",
        "bundle_version": ""
      }
    }
  }
}
```

Desktop polls this endpoint and routes the user into the on-demand
`tlm` reinstall flow when the block is present. The CLI prints the
same data via `shan daemon status`:

```
Memory:    disabled (tlm_binary_too_old)
           restart_attempts=2
Repair:    bundle_version= compatibility=incompatible sub_code=no_manifest
```

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
