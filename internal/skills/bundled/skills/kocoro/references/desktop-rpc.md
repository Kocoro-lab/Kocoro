# Desktop RPC Channel

## What is this?

The Desktop RPC channel is a **local Unix domain socket** that the daemon uses to call methods on Kocoro Desktop (and vice-versa for system handshake). It powers the calendar tools (and future Reminders / Contacts in v2) by giving the Go daemon a way to ask the Swift app — which holds TCC permissions and EventKit — to do work on its behalf.

This is daemon ↔ local-machine Desktop only. **Never goes through Cloud relay**. Even if a calendar query is triggered by a Slack message that arrives via the daemon's WS connection to Shannon Cloud, the actual EventKit call goes directly local-machine daemon → local-machine Desktop over the sock.

Authoritative spec: `docs/desktop-calendar-rpc.md` v0.5.1.

## Deployment model

The daemon is a **subprocess of Kocoro Desktop** (bundled `.app` ships both binaries; Desktop's `DaemonManager` spawns `shan` with `--rpc-socket` + `--rpc-pidfile` flags). Lifecycle is "semi-bound":

- Desktop launches → spawns daemon → daemon binds sock
- Desktop UI quits → daemon **continues running** (becomes a launchd-orphan child). Slack/LINE channel triggers still work; calendar tools still work as long as Desktop UI is reopened.
- Desktop relaunches → reconciliation flow re-establishes the sock connection (§4.1.1)

When running outside this model (npm CLI install, TUI, one-shot, MCP server, scheduled task), the RPC channel is not opened and calendar tools are not registered.

## Paths

| Artifact | Path |
|---|---|
| Socket | `~/Library/Application Support/run.shannon.shanclaw/daemon.sock` (0600) |
| PID file | `~/Library/Application Support/run.shannon.shanclaw/daemon.pid` (single-line PID, atomic write) |
| Parent dir | `~/Library/Application Support/run.shannon.shanclaw/` (0700) |

Daemon receives both paths as CLI flags (`--rpc-socket` and `--rpc-pidfile`) — never derives one from the other. Desktop side reads the pidfile only via path that was passed to daemon (no hardcoding).

## Protocol version

`"1.0.0"`. Both ends hardcode this and compare on reconciliation; mismatch triggers SIGTERM-and-respawn (Desktop side; see §4.1.1).

## Frame schema

All frames are **length-prefixed JSON**: 4-byte big-endian uint32 body length (≤ 4 MiB), then UTF-8 JSON body. Each frame has a `type` field:

| Type | Direction | Purpose |
|---|---|---|
| `desktop_rpc_request` | bidirectional | RPC call (daemon→Desktop for `calendar.*`, Desktop→daemon for `system.*`) |
| `desktop_rpc_result` | bidirectional | Response to a request, matched by `request_id` |
| `desktop_event` | Desktop→daemon | Async notification (no request_id); e.g. `desktop_online`, `calendar_permission_changed`, `calendar_data_changed` |
| `desktop_rpc_cancel` | *v1.x placeholder* | Cancel an in-flight request (schema TBD) |

### `desktop_rpc_request` payload
```
{
  "request_id": "drpc_<16hex>",
  "method": "calendar.list_events",
  "params": { ... },
  "timeout_ms": 30000,
  "session_id": "sess_...",   // optional context
  "agent": "default",
  "source": "slack",          // RunAgentRequest.Source
  "ts": "2026-05-26T10:00:00+08:00"
}
```

`calendar_request_permission` overrides `timeout_ms = 300000` (5 minutes) — TCC system dialog can sit for a long time.

### `desktop_rpc_result` payload (success)
```
{ "request_id": "...", "ok": true, "result": { ... } }
```

### `desktop_rpc_result` payload (error)
```
{
  "request_id": "...",
  "ok": false,
  "error": {
    "code": "calendar_permission_denied",
    "message": "human-readable",
    "retriable": false,
    "details": { ... }   // optional structured
  }
}
```

### `desktop_event` payload
```
{
  "event": "desktop_online",
  "data": { "version": "1.0.0", "platform": {...} },
  "ts": "2026-05-26T10:00:00+08:00"
}
```

## Frame fixtures (round-trip dry-run)

`docs/desktop-calendar-rpc-fixtures/` contains canonical JSON examples for every method / event. Both Daemon Go side and Desktop Swift side run their codec tests against these fixtures — any schema drift fails on both sides.

## Reconciliation flow (§4.1.1)

On Desktop launch:

1. Read pidfile.
2. If PID exists and process is alive:
   - Connect sock.
   - First frame: `system.capabilities` request (Desktop → daemon).
   - Compare `result.version` with hardcoded expected.
   - **Match** → reuse; send `desktop_event { event: "desktop_online" }`.
   - **Mismatch** → SIGTERM old daemon (5s grace), SIGKILL fallback (2s grace), then respawn.
3. If no live PID: remove stale sock + pidfile, spawn new daemon.

Daemon-side responsibilities (steps 5a-e in spec):
1. `os.Remove` stale sock
2. `net.Listen("unix", path)`
3. `os.Chmod(path, 0600)`
4. Atomically write pidfile (tmp + rename)
5. Begin accept loop

## Frame size + keepalive

- Single frame body ≤ **4 MiB** (`MaxFrameBodyBytes` in `internal/daemon/desktop_rpc/types.go`). list_events truncates at 2000 events to stay well under.
- No application-layer ping — Unix socket EOF/EPIPE is immediate.
- Daemon `CancelAll` fires on sock disconnect, unblocking all pending RPCs with `desktop_disconnected` error.

## Single-instance assumption

The daemon listener accepts exactly one Desktop client at a time. A second concurrent `net.Dial` connection is closed immediately. This matches Apple's expectation that LaunchServices reuses a single Kocoro Desktop instance.

## Method registry (spec §5.5.2)

Both sides return these 10 method names byte-identically from `system.capabilities`:

```
system.ping
system.capabilities
calendar.list_sources
calendar.list_events
calendar.get_event
calendar.create_event
calendar.update_event
calendar.delete_event
calendar.check_permission
calendar.request_permission
```

Daemon Go side: constants in `internal/daemon/desktop_rpc/types.go` (`Method*` and `ProtocolMethods`).
Desktop Swift side: constants in `Packages/ShanClawBridge/Sources/ShanClawBridge/DesktopRPC/ProtocolConstants.swift`.

**Both sides must `grep` to ensure no inline literal strings remain after Phase 1** — drift between the two arrays = silent runtime mismatch.

## Failure modes & responses

| Failure | What daemon does |
|---|---|
| Listener bind fails (sock already taken, FS error) | Non-zero exit + stderr error message. Desktop's `DaemonManager` surfaces to user. **Daemon does NOT silently retry.** |
| Pending request when Desktop disconnects | `desktop_disconnected` RPC error, all unblocked together |
| Method handler returns Go error | Translated to `internal_error` with the Go error message |
| Unknown method received (e.g. Desktop calls a method daemon doesn't implement) | `invalid_argument` with `details.method` naming the unknown method (spec §5.5.4 has 8 codes; we don't introduce `unknown_method`) |
| Body > 4 MiB | Connection closed immediately by reader |
| Malformed JSON | Connection closed |

## Out of scope for v1

- `desktop_rpc_cancel` frame (architecture diagram placeholder)
- Async `calendar.request_permission` (currently synchronous with 5-min timeout)
- `attendees` invitation auto-sending (always `invitations_sent: false` in v1)

All three are tracked as v1.x patches; order: cancel → async permission → attendees.

## See also

- `docs/desktop-calendar-rpc.md` v0.5.1 — full protocol spec
- `calendar.md` (in this same references/ directory) — calendar tool reference
- `docs/desktop-calendar-rpc-fixtures/` — JSON fixtures for round-trip testing
