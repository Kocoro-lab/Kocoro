# Daemon API: `POST /cancel`

Cancels an in-flight agent run on a specific route. Extends the original simpler `/cancel` (which only accepted `route_key`) with **reason classification** and **optional last-user-message restore**.

## Request body

```json
{
  "route_key": "string (required)",
  "reason":    "user_cancel | interrupt | background",
  "restore_last": false
}
```

`reason` defaults to `user_cancel` if omitted.

## Behavior by `reason`

| `reason` | Use case | Cleanup |
|---|---|---|
| `user_cancel` | Typical Esc-from-prompt. UI user pressed Esc with empty input + empty queue. | Loop's `context.Cause` returns `ReasonUserCancel`. Partial assistant text is preserved as an `assistant` message with `metadata.incomplete=true`. Tools tagged `InterruptBlock` finish; tools tagged `InterruptCancel` abort. |
| `interrupt` | User submitted a new message while a tool was running. The daemon publishes `cancel.restored=false` and immediately drains the mailbox into the next user turn. | Same as `user_cancel` for tool dispatch, but the "Request interrupted by user" sentinel is suppressed in the transcript (the queued message provides the context). |
| `background` | Programmatic detach (Desktop's "minimize agent" action). | NO partial-text preservation, NO restore, NO sentinel — silent. |

The daemon never silently maps unknown `reason` values. Unknown reason → 400.

## `restore_last`

When `true`, after the run exits cleanly the daemon may slice the session at the most recent `role:"user"` message and emit a `cancel.restored` SSE event containing that message's text + attachments (for the UI to put back in the input box).

The slice is **conditional**:
1. There must be at least one user message in the session.
2. The slice happens via `session.TruncateAt(idx)` which synchronously truncates `Messages`, `MessageMeta`, clears `Summary`, resets `ToolResultBudget`, sets `InProgress=false`.
3. If the agent loop did not exit within 5 s of `cancel()`, the slice is **aborted** (returns 504 Gateway Timeout) — slicing optimistically while the finalizer is still running would race the final-save and overwrite the rewind.

## Status codes

- `200 { "ok": true, "restored": false }` — cancel issued, conditions for restore not met.
- `200 { "ok": true, "restored": true }` — cancel issued, session truncated, `cancel.restored` SSE published.
- `400` — bad json, unknown `reason`, or missing `route_key`.
- `404` — no active run for this route_key (already cancelled or never started).
- `504` — loop did not exit within 5 s; restore aborted; daemon may still finalize asynchronously.

## SSE event: `cancel.restored`

Fires only when `restore_last=true` AND the slice succeeded.

```json
{
  "type": "cancel.restored",
  "session_id": "sess_abc",
  "text": "the truncated user message text",
  "attachments": []
}
```

Desktop client behavior: replace input box content with `text`, attach files via local URIs from `attachments[].nonce`, focus cursor at end.

Phase 4 will populate `attachments` once queued attachments ship; until then the array is always empty.

## Concurrency notes

`CancelRoute` acquires `routeEntry.mu` (the per-route session-mutation lock held by the active `RunAgent`) before touching session — this enforces serialization with the in-flight loop's mid-turn checkpoints. The 5s timeout exists precisely because `entry.mu` may be held by a slow LLM call; rather than blocking indefinitely, we surface 504 and let the client decide whether to retry.

`QueryGuard.ForceEnd()` is invoked alongside `cancel(reason)` so that stale finalizers from the cancelled run cannot reset state back to `running`.

## Related files

- `internal/daemon/server.go` — `handleCancel`.
- `internal/daemon/router.go` — `CancelRoute(routeKey, reason, restoreLast)`.
- `internal/agenttypes/cancel_reason.go` — `CancelReason` enum + `CancelError`.
- `internal/session/session.go` — `TruncateAt(idx int) (*RestoredMessage, error)`.
- See also `references/rewind.md` for the related slice-to-arbitrary-message endpoint.
