# Memory feature

ShanClaw includes a `memory_recall` agent tool backed by a local memory
sidecar. The daemon manages the sidecar's lifecycle and (in cloud mode)
periodically pulls fresh memory bundles from Kocoro Cloud. Episodic Memory is
enabled by default for cloud-connected installs and can be disabled in
settings. Three modes:

- **cloud** (default): paid feature. Daemon polls Kocoro Cloud for bundle manifests
  every 24h, downloads + verifies + atomically installs, then triggers a
  sidecar reload.
- **disabled**: no sidecar; tool falls back to `session_search`
  and the agent's MEMORY.md.
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
is enabled by default for cloud-connected installs; turning off Episodic Memory
also disables session sync.
