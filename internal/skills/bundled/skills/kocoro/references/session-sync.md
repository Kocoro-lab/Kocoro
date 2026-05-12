# Session sync configuration

## What it does

Uploads local session JSON from `~/.shannon/sessions/` and `~/.shannon/agents/*/sessions/` to Shannon Cloud once per day. **Opt-in** — disabled by default; flipped on alongside Episodic Memory by the Kocoro Desktop toggle. Used for Cloud-side analytics, replay, and per-user memory training.

## Config keys (under `sync:`)

| Key | Default | Notes |
|---|---|---|
| `enabled` | `false` | Master switch. Set `true` to enable uploads (Desktop toggle does this in tandem with `memory.provider`). |
| `dry_run` | `false` | If `true`, writes batches to `~/.shannon/sync_outbox/` instead of POSTing. Useful for local verification. |
| `endpoint` | `""` | Cloud endpoint. Empty falls back to `{cloud.endpoint}/api/v1/sessions/sync`. |
| `exclude_agents` | `[]` | List of agent dir names to skip. Use `"default"` for the root sessions dir. |
| `exclude_sources` | `[]` | List of session sources to skip. Legacy sessions with no source are treated as `"local"`. |
| `batch_max_sessions` | `25` | Max sessions per HTTP batch. |
| `batch_max_bytes` | `5242880` | Max marshaled bytes per batch (5 MiB). |
| `single_session_max_bytes` | `4194304` | Max marshaled bytes for one session (4 MiB). Sessions over this are flagged in the marker and never uploaded. |
| `daemon_interval` | `24h` | How often the daemon's ticker fires. |
| `daemon_startup_delay` | `60s` | Wait after daemon startup before first sync. |
| `failed_max_attempts_transient` | `5` | Drop transient-failed sessions after this many attempts. |
| `lock_timeout` | `30s` | Max flock wait. |

## Workflows

### Disable sync

```yaml
sync:
  enabled: false
```

The daemon picks this up on next start. The Desktop Episodic Memory toggle writes this together with `memory.provider`.

### Verify locally before uploading

```yaml
sync:
  enabled: true
  dry_run: true
```

Then `shan sessions sync` writes batches to `~/.shannon/sync_outbox/`. Inspect the JSON files to confirm what would be uploaded.

### Add a launchd schedule for daemon-off coverage (macOS)

`shan schedule create` is only for scheduling agent prompts — it doesn't run arbitrary OS commands. Install a launchd plist directly. Create `~/Library/LaunchAgents/com.shannon.sessions-sync.plist` with `StartCalendarInterval` set to your preferred time, then `launchctl load` it. See README's "Session sync to Cloud" section for the full plist template.

On Linux, use cron:
```
30 3 * * * /usr/local/bin/shan sessions sync
```

### Triage failures

```bash
cat ~/.shannon/sync_marker.json | jq .failed
```

Each entry shows `reason`, `category` (transient or permanent), `attempts`, and `size_bytes`. Permanent failures (`size_limit_exceeded`, `load_error`) stay until the session is edited or the entry is manually removed.

## Privacy posture (v1)

There is **no built-in redaction in v1**. Sessions are uploaded as-is, including tool output, file contents, and bash command results. Skill secrets are never included (they live in the macOS Keychain, not in transcripts). Users who care about tighter privacy should set `sync.enabled: false` until built-in redaction ships in v1.1.
