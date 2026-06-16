# Calendar RPC v1 — Shared JSON Fixtures

## Purpose

Both sides' (Daemon Go / Desktop Swift) codec unit tests / round-trip tests / wire-level verification during integration all match against the JSON fixtures in this directory. **Any field-name / type / nested-structure change goes here first, then both sides update code in sync**.

The problem this avoids: hand-writing JSON and misspelling `series_master_id` as `series_master_event_id`, so each side's unit tests pass on their own but integration blows up. One error in the fixture → both sides' unit tests fail → caught early.

## Contents

Each file is **the JSON body of one complete Unix sock frame** (the 4-byte big-endian uint32 length prefix is added by the codec, not in the file). File naming convention:

```
<frame-type>.<method-or-event-name>.<request|result|err>.json
```

- `<frame-type>` ∈ `desktop_rpc_request` / `desktop_rpc_result` / `desktop_event`
- Files with frame type request contain `.request.json` or `.result.json` (paired)
- A result-type frame with `err` is the error form (`.err.json`)
- Event-type frames are named directly by the event name

## File List (since v0.5.1)

### system.* methods (required for reconciliation)

| File | Description |
|---|---|
| `desktop_rpc_request.system_ping.request.json` | echo "hello" |
| `desktop_rpc_result.system_ping.result.json` | pong + server_time |
| `desktop_rpc_request.system_capabilities.request.json` | empty params |
| `desktop_rpc_result.system_capabilities.result.json` | full ProtocolMethods + platform |

### calendar read path

| File | Description |
|---|---|
| `desktop_rpc_request.calendar_list_events.request.json` | one-day time window + null calendar_ids + limit 500 |
| `desktop_rpc_result.calendar_list_events.result.json` | 2 events (one normal + one recurring instance with series_master_id) + truncated:false |

### calendar write path

| File | Description |
|---|---|
| `desktop_rpc_request.calendar_create_event.request.json` | with attendees + alarms + recurrence_rule (weekly) |
| `desktop_rpc_result.calendar_create_event.result.json` | id + pending_remote_sync + invitations_sent:false |
| `desktop_rpc_request.calendar_update_event.request.json` | scope:this + patch + clear_recurrence:false |
| `desktop_rpc_result.calendar_update_event.result.json` | id may change when scope:this_and_future (demo) |

### error forms

| File | Description |
|---|---|
| `desktop_rpc_result.calendar_list_events.err.json` | calendar_permission_denied + details.status:"denied" |

### Desktop-pushed events

| File | Description |
|---|---|
| `desktop_event.desktop_online.json` | the first frame after reconciliation completes |
| `desktop_event.calendar_permission_changed.json` | TCC flip |

## Verification Script (suggested)

```bash
# Daemon Go-side unit test
go test ./internal/daemon/desktop_rpc -run TestFixtureRoundTrip

# Desktop Swift-side unit test
swift test --filter DesktopRPCFixtureTests
```

Both sides' unit tests should do: read fixture → unmarshal → marshal → **semantically equal** to the original file (compare field values after re-parsing into struct/dict), **not byte-equal**.

⚠️ **Why it can't be byte-equal**: JSON map serialization does not guarantee consistent key order across languages (Go's `encoding/json` follows struct field-definition order, but `map[string]any` serializes in random order; Swift Codable follows struct-definition order). Also, the Daemon-side `stripDescription` for write tools goes through `map[string]json.RawMessage` then Marshal, which **reorders keys**. A byte-equal comparison would therefore fail even though the semantics are identical.

Concrete approach (pseudocode):
```
data = readFixture("calendar.list_events.request.json")
struct = decode(data)
reencoded = encode(struct)
decoded_back = decode(reencoded)
assertDeepEqual(struct, decoded_back)   // ✓ semantic
// assertEqual(data, reencoded)         // ✗ may fail on key reorder
```

## Change Rules

- v1.x adds a new method → add a new pair of fixture files, update this README's file list
- Field rename / type change → change spec §5 + all related fixtures in this directory + both sides' constants at the same time
- **Don't bypass the fixtures and change both sides' code directly** — that's exactly what the calendar RPC v0.5.1 protocol contract is meant to avoid
