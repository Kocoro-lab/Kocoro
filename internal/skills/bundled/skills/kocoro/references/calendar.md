# Calendar Tools

## What is this?

Calendar tools let the agent operate the user's macOS system calendars via Kocoro Desktop. They reach iCloud / Google / Microsoft 365 / Exchange / Outlook calendars that the user has configured under **System Settings → Internet Accounts** with the "Calendars" toggle on — no per-provider OAuth, no third-party API.

The actual EventKit access happens in Kocoro Desktop (the macOS .app); the daemon talks to Desktop over a local Unix domain socket (see `desktop-rpc.md` in this references/ directory). When the daemon is **not** running as a Desktop subprocess (TUI / one-shot CLI / MCP server / scheduled task), `calendar_*` tools are not registered — fall back to the `applescript` tool driving Calendar.app for that scope (see "Fallback for non-daemon modes" below).

## Permission flow

macOS gates calendar access through TCC. First-use flow:

1. Call `calendar_check_permission` (always free of side effects).
2. If status is `not_determined`, either:
   - **Path A (preferred)**: tell the user to open Kocoro Desktop → Settings → Permissions → Calendar and click "Grant access"
   - **Path B**: call `calendar_request_permission` directly. This triggers the macOS TCC system dialog and **blocks up to 5 minutes** waiting for the user's decision.
3. Once status is `granted` (or `write_only` — see below), the other calendar tools work.

`write_only` (macOS 14+): the user granted "Add Events" but not "Full Access". Write tools work; read tools return `calendar_permission_denied` with `details.status = "write_only"`. Tell the user to upgrade to full access in Kocoro Desktop settings.

## Tools

### `calendar_check_permission`
- **Returns**: `{"status": "not_determined" | "restricted" | "denied" | "granted" | "write_only"}`
- **Approval**: none
- **Use when**: deciding whether other calendar tools will work; before triggering `request_permission`.

### `calendar_request_permission` ⚠ (approval-required, **5-min timeout**)
- **Params**: `{"description": "..."}` (model-generated approval card text, 5-15 words)
- **Returns**: `{"status": "<new status>"}`
- **Behavior**: Triggers the macOS TCC dialog. Blocks for up to 5 minutes (longer than other tools — user may pause to decide). If user takes longer, returns `timeout` error; the dialog is still showing, and the user's eventual decision flips state via `calendar_permission_changed` event.
- **Use when**: `check_permission` returned `not_determined` and the user has signaled they're ready to grant.

### `calendar_list_sources`
- **Returns**: `{"sources": [{"id": "...", "title": "...", "account_type": "icloud|google|exchange|outlook|local|subscription|other", "color_hex": "#RRGGBB", "writable": true, "default_for_new_events": false}]}`
- **Approval**: none
- **Use when**: user asks "which calendars do I have" or you need a specific calendar ID for filtering / creation.

### `calendar_list_events`
- **Params**:
  ```
  {
    "start": "2026-05-26T00:00:00+08:00",  // RFC 3339 with offset, required
    "end":   "2026-05-26T23:59:59+08:00",
    "calendar_ids": null | ["id1", ...],   // null/omitted = all; [] = empty result
    "query": null | "keyword",             // case-insensitive substring (title+notes)
    "limit": 500                            // default 500, max 2000
  }
  ```
- **Returns**: `{"events": [...], "truncated": false}`
- **Event fields**: `id`, `calendar_id`, `title`, `start`, `end`, `all_day`, `location`, `notes`, `url` (often Teams/Zoom join link), `is_recurring`, `is_recurring_instance`, `series_master_id` (set when instance), `attendees: [{email,name,status}]`, `organizer_email`, `has_alarms`.
- **Approval**: none
- **All-day events**: `end` is the **inclusive end-of-day** (e.g. `2026-05-26T23:59:59+08:00` for a one-day event), NOT exclusive next-midnight.
- **Use when**: any "what's on my calendar" / "find a meeting" / "do I have time" query.

### `calendar_get_event`
- **Params**: `{"id": "<EKEvent.eventIdentifier>"}`
- **Returns**: full `calendar_list_events` event fields + `recurrence_rule` + `alarms`
- **Recurrence rule shape**:
  ```
  {
    "frequency": "daily|weekly|monthly|yearly",
    "interval": 1,
    "by_day": ["MO","WE"] | null,
    "end_date": "2026-12-31T00:00:00+08:00" | null,
    "occurrence_count": null | N,
    "raw_rrule": "FREQ=WEEKLY;BYDAY=MO,WE;UNTIL=20261231T000000"
  }
  ```
  `raw_rrule` is the authoritative RFC 5545 form (Desktop generates it from EventKit's internal model). Use it when you need to faithfully round-trip a rule.
- **Approval**: none
- **Use when**: after `list_events`, when you need recurrence info or full alarm/attendee detail.

### `calendar_create_event` ⚠ (approval-required)
- **Params**:
  ```
  {
    "calendar_id": "<id>" | null,    // null = user's default calendar
    "title": "...",                   // required
    "start": "...",                   // required, RFC 3339 with offset
    "end":   "...",                   // required
    "all_day": false,
    "location": null,
    "notes":    null,
    "url":      null,                 // pasted into Calendar.app's URL field
    "attendees": [{"email":"...","name":"..."}],
    "alarms":    [{"minutes_before": 15}],
    "recurrence_rule": null,          // structured or {"raw_rrule": "..."}
    "description": "..."              // approval card text, 5-15 words — REQUIRED
  }
  ```
- **Returns**: `{"id": "...", "pending_remote_sync": true, "invitations_sent": false}`
- **⚠ `invitations_sent` is always `false` in v1**: EventKit on macOS does not auto-send calendar invitations through Google CalDAV / Exchange. The event is created and any `attendees` are written as metadata, but the recipients **will not receive an invitation email**. Tell the user "event created — invitations need to be sent manually" if they passed attendees.

### `calendar_update_event` ⚠ (approval-required)
- **Params**:
  ```
  {
    "id": "<EKEvent.eventIdentifier>",   // required
    "scope": "this" | "this_and_future", // required, no "all"
    "patch": { ...fields to change... },
    "clear_recurrence": false,            // true = remove recurrence (don't put null in patch)
    "description": "..."                  // required
  }
  ```
- **Patch semantics (v1)**:
  - Key absent or value `null` → no change
  - String `""` or list `[]` → clear field (NOT for start/end, which can never be cleared)
  - `attendees` / `alarms` non-empty list → **replace** the whole list (not merge)
  - `recurrence_rule` non-null → replace
- **⚠ `scope: "this_and_future"` splits the recurring series**: the returned `id` may be a NEW id. Use the result's `id` going forward.
- **No `scope: "all"`**: client rejects with `invalid_argument`. To rewrite an entire series, use `calendar_delete_event(scope="all")` + `calendar_create_event` — explicit, audited, avoids silently overwriting user's per-instance edits.

### `calendar_delete_event` ⚠ (approval-required)
- **Params**: `{"id":"...", "scope":"this|this_and_future|all", "description":"..."}`
- **Returns**: `{"ok": true, "pending_remote_sync": true}`
- **`scope: "all"`**: Desktop resolves to the series master automatically — pass either the master event id or any instance id.

## Error codes (spec §5.3)

When a tool returns `IsError: true`, the message is keyed off the underlying RPC error:

| code | What to tell the user |
|---|---|
| `calendar_permission_denied` | "Open Kocoro Desktop → Settings → Permissions → Calendar to grant access." Distinguishes `denied` / `restricted` / `write_only` via internal `details.status`. |
| `calendar_permission_not_determined` | "I haven't asked for calendar permission yet — should I trigger the system dialog?" Then call `calendar_request_permission`. |
| `not_found` | "That calendar event or source doesn't exist anymore." |
| `invalid_argument` | Specific message included — usually a time format or scope issue. |
| `read_only_calendar` | "That calendar is read-only (e.g. Birthdays, subscription). Pick a writable calendar." |
| `internal_error` | Desktop reported an unexpected error — bubble the message to the user. |
| `timeout` | Desktop took too long. Suggest retrying. |
| `desktop_disconnected` | "Kocoro Desktop is not running. Please open it." |

## Pending-remote-sync semantics

All write tools return `pending_remote_sync: true`. EventKit writes are **immediately local** (Desktop sees them in Calendar.app right away), but the change to Google / Exchange propagates asynchronously through macOS CalendarAgent. If the user creates an event and then immediately asks "did it sync to Google?", the answer is "started syncing, may take a few seconds". Don't read back the event immediately to verify — the local copy is authoritative for the agent.

## Fallback for non-daemon modes

When the agent is running in **TUI / one-shot CLI / MCP server / scheduled task** mode, the daemon is not a Kocoro Desktop subprocess, so `calendar_*` tools are not registered (the model literally cannot see them in its tool list).

In those modes, if the user asks for calendar operations, the model can fall back to driving **Calendar.app via AppleScript** using the daemon's existing `applescript` tool:

```
applescript({
  script: "tell application \"Calendar\" to list of name of calendars",
  description: "list calendar names via Calendar.app"
})
```

Notes:
- AppleScript route goes through **Automation TCC** (the prompt says "shan wants to control Calendar"), not **Calendars TCC** — separate permission grant from the EventKit path.
- Calendar.app must be open and signed into the user's accounts.
- AppleScript is slower, less structured, and gives weaker access to recurrence / attendees — use only as a fallback.

When `calendar_*` tools ARE available (daemon mode), prefer them over `applescript`.

## Examples

**"What's on my calendar today?"**
```
calendar_check_permission       → granted
calendar_list_events            → params: {start: today 00:00, end: today 23:59:59, limit: 50}
```

**"Schedule a meeting with Alice tomorrow at 3pm for 30 minutes"**
```
calendar_check_permission       → granted (or trigger request flow)
calendar_create_event           →
  {
    title: "Meeting with Alice",
    start: "2026-05-27T15:00:00+08:00",
    end:   "2026-05-27T15:30:00+08:00",
    attendees: [{"email":"alice@example.com","name":"Alice"}],
    alarms: [{"minutes_before": 10}],
    description: "create meeting with Alice tomorrow at 3pm"
  }
→ result: {id: "evt_...", pending_remote_sync: true, invitations_sent: false}
→ Tell user: "Created. Heads up — invitations weren't auto-sent; you'll need to open Calendar and click 'Send' if you want Alice to receive an invite."
```

**"Cancel the weekly team standup"**
```
calendar_list_events            → find the recurring event
                                  (note series_master_id on instances)
calendar_get_event              → confirm recurrence_rule
calendar_delete_event           →
  {
    id: "<master_id or any instance_id>",
    scope: "all",
    description: "cancel the entire weekly standup series"
  }
```

## See also

- `desktop-rpc.md` (in this same references/ directory) — RPC channel details (protocol v0.5.1)
