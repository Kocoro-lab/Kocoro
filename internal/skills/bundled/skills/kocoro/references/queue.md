# Daemon API: `/queue`

The daemon maintains a per-route **mailbox** of queued user messages. The mailbox is consumed at agent-loop turn boundaries — a user message sent while a loop is running is not rejected and does not open a new turn; it queues, is acknowledged to its source (Cloud `delivery_ack` or HTTP 200), and is merged into the next user turn the loop reaches.

Persistence lives in `~/.shannon/sessions/mailbox.db`. Daemon crash recovery reloads pending rows on startup; durability boundary is **append-to-SQLite**, not network ack. See `internal/daemon/mailbox_store.go`.

## GET `/queue`

Lists pending messages for one route.

**Query parameters (one required):**
- `route_key` — the route identifier returned by other daemon endpoints.
- `session_id` — alternative lookup; resolves to a route via the active route registry.

**Response 200:**

```json
{
  "route_key": "string",
  "items": [
    {
      "id": "01J6T5K4N0XJ73B2WJEHGP4M85",
      "preview": "first ≤120 chars of text, ending in … if truncated",
      "editable": true,
      "attachment_count": 0,
      "enqueued_at": "2026-05-15T10:11:12.345Z",
      "source": "ws"
    }
  ]
}
```

Items are sorted by priority then enqueue time (FIFO within priority).

`source` is one of `ws | http | sse | tui`. Items from external IM channels (`ws`) have `editable: false` and cannot be retracted via `DELETE` (the user has already sent the message in Slack/LINE/Feishu/Telegram and the daemon does not own it).

**Redaction:** The response is the redacted DTO defined in `internal/daemon/queue_dto.go`. The daemon NEVER returns the raw `OriginPayload`, original Cloud message ID, or full attachment URLs through this endpoint.

## DELETE `/queue/{id}`

Retracts a queued message.

**Query parameter (required):**
- `route_key` — same key used in `GET /queue`.

**Status codes:**
- `200` — retracted successfully.
- `403` — message has `editable: false` (Cloud-sourced).
- `404` — message not found (already drained or invalid id).
- `409` — message was consumed by the drain loop between the time the client read the queue and the retract arrived.

A successful retract removes the row from both the in-memory mailbox and the SQLite store, then publishes `queue.removed` on the SSE bus.

## SSE event vocabulary

The events bus (`GET /events`) publishes three queue-lifecycle events. Each event carries a `snapshot` array of redacted DTOs in current priority/FIFO order — clients can use it to refresh UI state without re-fetching.

| Event `type` | Fires when | Payload |
|---|---|---|
| `queue.added` | `InjectMessage` succeeded (SQLite append + in-memory enqueue both committed) | `{ message_id, snapshot: [DTO] }` |
| `queue.removed` | `DELETE /queue/{id}` succeeded, or the drain skipped this item due to `SourceMailboxID` idempotency check | `{ message_id, snapshot: [DTO] }` |
| `queue.flushed` | Drain consumed one or more items; `consumed_ids` lists them | `{ consumed_ids: [string], snapshot: [DTO] }` |

`session_id` accompanies every event so subscribers can demux.

## Capacity & dedup

- **Per-route cap** — `daemon.mailbox_max_per_route` (viper default `100`). `InjectMessage` returns `InjectQueueFull` when exceeded; daemon does NOT ack the source. Cloud will replay; Desktop should surface the failure.
- **Per-message cap** — text + metadata ≤ 1 MB. Exceeding returns `InjectFailed`.
- **Cloud `msg_id` dedup** — `mailbox` table uses `INSERT OR IGNORE` keyed on `(cloud_msg_id, route_key)`. Cloud-replay of an already-ack'd message becomes a no-op.

## Ordering guarantees

Within a route, items dequeue in `(priority ASC, enqueued_at ASC)` order. Across routes, ordering is independent (each route has its own mailbox).

At-least-once delivery: after daemon crash, recovery reloads `consumed_at IS NULL` rows and re-delivers; session-side dedup uses `MessageMeta.SourceMailboxID` to ensure idempotent re-append.

## Related files

- `internal/agenttypes/queued_message.go` — `QueuedMessage` type (full fidelity, daemon-internal).
- `internal/daemon/queue_dto.go` — redacted DTO used by HTTP/SSE.
- `internal/daemon/mailbox_store.go` — SQLite schema, append, mark-consumed, recovery.
- `internal/daemon/router.go` — `InjectMessage` outcome dispatch.
