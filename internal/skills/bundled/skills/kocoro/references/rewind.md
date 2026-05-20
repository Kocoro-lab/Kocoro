# Daemon API: `POST /sessions/{id}/rewind`

Slices a session's history at a chosen prior user message, returning that message's text + attachments. If a run is active on the session's route, the run is cancelled first (synchronously) before the slice happens.

This is the daemon-side primitive for Desktop's double-Esc "pick a prior message to rewind to" UX, but it's also useful programmatically for any caller that wants to undo the last N exchanges in one call.

## Request

```
POST /sessions/{session_id}/rewind?message_id=<msg_id>
```

`message_id` must reference a `role:"user"` message that exists in the session's history.

## Response

**200 OK:**

```json
{
  "ok": true,
  "restored": {
    "text": "the user-message text",
    "attachments": []
  }
}
```

`restored` carries the text + attachments of the truncated message so the caller can put them back in an input box.

**Errors:**
- `400` — `message_id` not found in session, or not a `role:"user"` message.
- `404` — session not found.
- `500` — save failed mid-truncate; session may be in inconsistent state, but the daemon attempts to flush a recovery write before returning.
- `504` — active run did not exit within 5 s of cancel; rewind aborted.

## Semantics

1. Look up the route currently serving this session (if any).
2. If active, call `CancelRoute(routeKey, ReasonUserCancel, restoreLast=false)`. The 5 s timeout applies here too.
3. Load the session from disk.
4. Find the index `idx` of `message_id`; verify `Messages[idx].Role == "user"`.
5. Call `session.TruncateAt(idx)`:
   - Captures `Messages[idx]` into `RestoredMessage`.
   - `Messages = Messages[:idx]`.
   - `MessageMeta = MessageMeta[:idx]` (kept aligned 1:1).
   - Clears `Summary`, resets `ToolResultBudget`, `InProgress = false`.
6. Save the session.
7. Return `RestoredMessage`.

The truncation is **destructive** — historical messages after `idx` are removed from the session file. The daemon does not maintain a multi-step undo history; clients who need that should snapshot the session JSON before calling `rewind`.

## Cache invalidation

Truncation intrinsically invalidates the prompt cache for the session — the next LLM call will replay the cache cascade from the new tail. No client-side cache busting is needed.

## Compared to `/cancel`

| Operation | What it does | When to use |
|---|---|---|
| `POST /cancel` with `restore_last:true` | Slices to JUST before the most recent user message | Esc once, simple "undo my last prompt" |
| `POST /sessions/{id}/rewind` | Slices to a user-chosen message in history | Double-Esc selector, "go back to 3 turns ago" |

Both share the same `session.TruncateAt(idx)` primitive — they differ only in how `idx` is determined.

## Related files

- `internal/daemon/server.go` — `handleRewind`.
- `internal/session/session.go` — `TruncateAt`, `SliceBefore(messageID)` wrapper.
- See `references/cancel.md` for the simpler restore-last variant.
