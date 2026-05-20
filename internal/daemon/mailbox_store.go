package daemon

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
)

// MailboxStore persists QueuedMessage rows to SQLite so the daemon can recover
// pending mailbox state after a crash or restart.
//
// Schema (one row per enqueue):
//
//	id            TEXT PRIMARY KEY        -- ULID, also the in-memory QueuedMessage.ID
//	route_key     TEXT NOT NULL           -- mailbox demux key
//	session_id    TEXT                    -- may be empty for routes still resolving
//	source        TEXT                    -- ws | http | sse | tui | recovery
//	cloud_msg_id  TEXT                    -- Cloud wire-level msg id, "" for non-WS
//	priority      INTEGER NOT NULL        -- agenttypes.Priority
//	enqueued_at   INTEGER NOT NULL        -- unix milli
//	editable      INTEGER NOT NULL        -- 0/1
//	payload_json  TEXT NOT NULL           -- JSON-marshaled QueuedMessage (full fidelity)
//	consumed_at   INTEGER                 -- NULL until drained
//
// Indices: (route_key, consumed_at) for LoadPendingByRoute, (consumed_at)
// for the daily purge sweep.
//
// Dedup: a UNIQUE index on (cloud_msg_id, route_key) lets Append use
// INSERT OR IGNORE so Cloud-replay of an already-ack'd message becomes a
// no-op. Empty cloud_msg_id is never compared because we filter the
// constraint with `WHERE cloud_msg_id != ''`.
type MailboxStore struct {
	db *sql.DB
}

const mailboxSchema = `
CREATE TABLE IF NOT EXISTS mailbox (
	id            TEXT PRIMARY KEY,
	route_key     TEXT NOT NULL,
	session_id    TEXT,
	source        TEXT,
	cloud_msg_id  TEXT NOT NULL DEFAULT '',
	priority      INTEGER NOT NULL DEFAULT 1,
	enqueued_at   INTEGER NOT NULL,
	editable      INTEGER NOT NULL DEFAULT 1,
	payload_json  TEXT NOT NULL,
	consumed_at   INTEGER
);
CREATE INDEX IF NOT EXISTS idx_mailbox_pending  ON mailbox(route_key, consumed_at);
CREATE INDEX IF NOT EXISTS idx_mailbox_consumed ON mailbox(consumed_at);
CREATE UNIQUE INDEX IF NOT EXISTS idx_mailbox_cloud_dedup
	ON mailbox(cloud_msg_id, route_key)
	WHERE cloud_msg_id != '';
`

// NewMailboxStore opens / creates the schema on the given *sql.DB. The caller
// owns the connection lifecycle. Tests pass a temp-dir-backed DB; production
// passes the daemon-startup DB rooted at ~/.shannon/sessions/mailbox.db.
func NewMailboxStore(db *sql.DB) (*MailboxStore, error) {
	if _, err := db.Exec(mailboxSchema); err != nil {
		return nil, fmt.Errorf("mailbox schema: %w", err)
	}
	return &MailboxStore{db: db}, nil
}

// Append inserts a row for msg. Returns (true, nil) on insert,
// (false, nil) when an existing row with the same (cloud_msg_id, route_key)
// already exists (dedup no-op), and (false, err) on any other failure.
//
// CRITICAL: callers must treat any error return as ack-blocking. Persistence
// failure is the only signal the daemon has that durability is compromised
// — if Append fails, the message MUST NOT be in-memory enqueued and
// MUST NOT be ack'd to its source. Cloud will replay; HTTP returns 5xx.
func (s *MailboxStore) Append(msg agenttypes.QueuedMessage) (bool, error) {
	payload, err := json.Marshal(msg)
	if err != nil {
		return false, fmt.Errorf("marshal: %w", err)
	}
	editable := 0
	if msg.Editable {
		editable = 1
	}
	res, err := s.db.Exec(`
		INSERT OR IGNORE INTO mailbox
			(id, route_key, session_id, source, cloud_msg_id, priority, enqueued_at, editable, payload_json)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		msg.ID,
		msg.RouteKey,
		msg.SessionID,
		msg.Source,
		msg.CloudMsgID,
		int(msg.Priority),
		msg.EnqueuedAt.UnixMilli(),
		editable,
		string(payload),
	)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

// MarkConsumed sets consumed_at = now() for the given ids that are still
// pending. Idempotent; rows already consumed are not re-stamped.
//
// CRITICAL: callers MUST invoke this only AFTER the queued user message has
// been appended to the session AND session.Save returned without error.
// Stamping consumed before persistence opens a crash window in which the
// row is "gone" from recovery but never made it to the conversation.
func (s *MailboxStore) MarkConsumed(ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]
	args := make([]any, 0, len(ids)+1)
	args = append(args, time.Now().UnixMilli())
	for _, id := range ids {
		args = append(args, id)
	}
	q := fmt.Sprintf(
		`UPDATE mailbox SET consumed_at = ? WHERE id IN (%s) AND consumed_at IS NULL`,
		placeholders,
	)
	_, err := s.db.Exec(q, args...)
	return err
}

// Delete removes a row entirely. Used by retract paths (DELETE /queue/{id})
// and by the Append failure rollback in router.go.
func (s *MailboxStore) Delete(id string) error {
	_, err := s.db.Exec(`DELETE FROM mailbox WHERE id = ?`, id)
	return err
}

// LoadPendingByRoute returns pending rows for a route in (priority, enqueued_at)
// order. Pending == consumed_at IS NULL.
func (s *MailboxStore) LoadPendingByRoute(routeKey string) ([]agenttypes.QueuedMessage, error) {
	rows, err := s.db.Query(`
		SELECT payload_json FROM mailbox
		WHERE route_key = ? AND consumed_at IS NULL
		ORDER BY priority ASC, enqueued_at ASC`,
		routeKey,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanMailboxRows(rows)
}

// LoadAllPending returns all pending rows, grouped by route_key. The map's
// value slices are already sorted by (priority, enqueued_at). Used by daemon
// startup recovery (see internal/daemon/server.go).
func (s *MailboxStore) LoadAllPending() (map[string][]agenttypes.QueuedMessage, error) {
	rows, err := s.db.Query(`
		SELECT route_key, payload_json FROM mailbox
		WHERE consumed_at IS NULL
		ORDER BY priority ASC, enqueued_at ASC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string][]agenttypes.QueuedMessage)
	for rows.Next() {
		var route, payload string
		if err := rows.Scan(&route, &payload); err != nil {
			return nil, err
		}
		var msg agenttypes.QueuedMessage
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			return nil, fmt.Errorf("unmarshal mailbox row: %w", err)
		}
		out[route] = append(out[route], msg)
	}
	return out, rows.Err()
}

// PurgeConsumedBefore removes mailbox rows marked consumed before the cutoff.
// Called from a daily background goroutine in server.go; default cutoff is
// 7 days ago. Returns the number of rows deleted for logging.
func (s *MailboxStore) PurgeConsumedBefore(cutoff time.Time) (int64, error) {
	r, err := s.db.Exec(
		`DELETE FROM mailbox WHERE consumed_at IS NOT NULL AND consumed_at < ?`,
		cutoff.UnixMilli(),
	)
	if err != nil {
		return 0, err
	}
	return r.RowsAffected()
}

func scanMailboxRows(rows *sql.Rows) ([]agenttypes.QueuedMessage, error) {
	var out []agenttypes.QueuedMessage
	for rows.Next() {
		var payload string
		if err := rows.Scan(&payload); err != nil {
			return nil, err
		}
		var msg agenttypes.QueuedMessage
		if err := json.Unmarshal([]byte(payload), &msg); err != nil {
			return nil, fmt.Errorf("unmarshal mailbox row: %w", err)
		}
		out = append(out, msg)
	}
	return out, rows.Err()
}
