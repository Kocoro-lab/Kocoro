package session

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const schema = `
PRAGMA journal_mode=WAL;
PRAGMA foreign_keys=ON;

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    title      TEXT NOT NULL DEFAULT '',
    cwd        TEXT NOT NULL DEFAULT '',
    created_at DATETIME NOT NULL,
    updated_at DATETIME NOT NULL,
    msg_count  INTEGER NOT NULL DEFAULT 0
);

-- content: tokenized (space-separated) text for FTS matching.
-- original: user-visible text, used for snippet rendering (never tokenized).
CREATE TABLE IF NOT EXISTS messages (
    rowid      INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    msg_index  INTEGER NOT NULL,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    original   TEXT NOT NULL DEFAULT '',
    UNIQUE(session_id, msg_index)
);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content=messages,
    content_rowid=rowid,
    tokenize='porter unicode61'
);

CREATE TRIGGER IF NOT EXISTS messages_ai AFTER INSERT ON messages BEGIN
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_ad AFTER DELETE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
END;
CREATE TRIGGER IF NOT EXISTS messages_au AFTER UPDATE ON messages BEGIN
    INSERT INTO messages_fts(messages_fts, rowid, content) VALUES('delete', old.rowid, old.content);
    INSERT INTO messages_fts(rowid, content) VALUES (new.rowid, new.content);
END;

CREATE INDEX IF NOT EXISTS idx_messages_session ON messages(session_id);
`

type SearchResult struct {
	SessionID    string    `json:"session_id"`
	SessionTitle string    `json:"session_title"`
	Role         string    `json:"role"`
	Snippet      string    `json:"snippet"`
	MsgIndex     int       `json:"msg_index"`
	CreatedAt    time.Time `json:"created_at"`
}

type Index struct {
	db           *sql.DB
	needsRebuild bool // true when tokenizer version bumped — triggers full reindex
}

func OpenIndex(dir string) (*Index, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return nil, fmt.Errorf("create index dir: %w", err)
	}

	dbPath := filepath.Join(dir, "sessions.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)

	// Tokenizer version gate. A mismatch between the stored user_version and
	// TokenizerVersion means the on-disk content/messages tables were built
	// with an older tokenization pipeline (or pre-CJK schema) and cannot be
	// used by the current code. stored == 0 also counts as a mismatch for
	// existing databases created before user_version tracking was introduced
	// (their schema predates the `original` column). For a brand-new empty
	// database, stored == 0 too — but in that case the drops below are no-ops
	// and Rebuild simply finds no JSON files to reindex.
	var stored int
	_ = db.QueryRow(`PRAGMA user_version`).Scan(&stored)

	needsRebuild := false
	if stored != TokenizerVersion {
		// Drop content tables so schema can recreate them with the current
		// column set. sessions is dropped too so nothing references the old
		// messages table via FK; it will be repopulated by Rebuild.
		for _, stmt := range []string{
			`DROP TABLE IF EXISTS messages_fts`,
			`DROP TABLE IF EXISTS messages`,
			`DROP TABLE IF EXISTS sessions`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				db.Close()
				return nil, fmt.Errorf("migrate drop: %w", err)
			}
		}
		needsRebuild = true
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, TokenizerVersion)); err != nil {
		db.Close()
		return nil, fmt.Errorf("set user_version: %w", err)
	}

	return &Index{db: db, needsRebuild: needsRebuild}, nil
}

func (idx *Index) Close() error {
	return idx.db.Close()
}

// NeedsRebuild reports whether a tokenizer-version mismatch was detected when
// the index was opened. Store uses this to trigger a one-time reindex.
func (idx *Index) NeedsRebuild() bool {
	return idx.needsRebuild
}

func (idx *Index) UpsertSession(sess *Session) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Upsert session row first (FK parent for messages).
	// msg_count reflects the total number of messages in the session (including
	// tool-only assistant turns and system-injected messages). Callers such as
	// the sidebar use it to decide whether a session has ever been used; it is
	// NOT a count of FTS-indexed rows.
	_, err = tx.Exec(
		`INSERT OR REPLACE INTO sessions (id, title, cwd, created_at, updated_at, msg_count)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.Title, sess.CWD,
		sess.CreatedAt.Format(time.RFC3339Nano),
		sess.UpdatedAt.Format(time.RFC3339Nano),
		len(sess.Messages),
	)
	if err != nil {
		return fmt.Errorf("upsert session: %w", err)
	}

	if _, err := tx.Exec(`DELETE FROM messages WHERE session_id = ?`, sess.ID); err != nil {
		return fmt.Errorf("delete old messages: %w", err)
	}

	for i, msg := range sess.Messages {
		// Skip system-injected guardrail/nudge messages to keep them out of search results
		if i < len(sess.MessageMeta) && sess.MessageMeta[i].SystemInjected {
			continue
		}
		original := msg.Content.Text()
		if original == "" {
			continue
		}
		tokenized := Tokenize(original)
		// msg_index is the original position in sess.Messages (may have gaps
		// where system-injected or empty entries were skipped). content holds
		// the tokenized form (FTS-indexed); original holds the raw text used
		// for snippet rendering.
		if _, err := tx.Exec(
			`INSERT INTO messages (session_id, msg_index, role, content, original) VALUES (?, ?, ?, ?, ?)`,
			sess.ID, i, msg.Role, tokenized, original,
		); err != nil {
			return fmt.Errorf("insert message %d: %w", i, err)
		}
	}

	return tx.Commit()
}

func (idx *Index) ListSessions() ([]SessionSummary, error) {
	rows, err := idx.db.Query(
		`SELECT id, title, created_at, msg_count FROM sessions ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	var summaries []SessionSummary
	for rows.Next() {
		var s SessionSummary
		var createdStr string
		if err := rows.Scan(&s.ID, &s.Title, &createdStr, &s.MsgCount); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		s.CreatedAt = parseTime(createdStr)
		summaries = append(summaries, s)
	}
	return summaries, rows.Err()
}

func (idx *Index) Search(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}

	// Tokenize the query the same way we tokenized the indexed content,
	// unless the user is using FTS5 operators (quotes, AND/OR/NOT, *).
	// Otherwise CJK queries like "机器学习" would be one token that never
	// matches the segmented index.
	ftsQuery := query
	if !hasFTSOperator(query) {
		ftsQuery = Tokenize(query)
		if strings.TrimSpace(ftsQuery) == "" {
			return nil, nil
		}
	}

	rows, err := idx.db.Query(
		`SELECT m.session_id, s.title, m.role, m.msg_index, s.created_at, m.original
		 FROM messages_fts
		 JOIN messages m ON m.rowid = messages_fts.rowid
		 JOIN sessions s ON s.id = m.session_id
		 WHERE messages_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		ftsQuery, limit,
	)
	if err != nil {
		if isFTSSyntaxError(err) {
			return nil, fmt.Errorf("invalid search query: %s", query)
		}
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	// Build query-term list for snippet highlighting. Strip FTS operators
	// so the highlight matcher only sees actual content terms.
	terms := extractTerms(query)

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var createdStr, original string
		if err := rows.Scan(&r.SessionID, &r.SessionTitle, &r.Role, &r.MsgIndex, &createdStr, &original); err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		r.CreatedAt = parseTime(createdStr)
		r.Snippet = buildSnippet(original, terms)
		results = append(results, r)
	}
	return results, rows.Err()
}

func (idx *Index) DeleteSession(id string) error {
	_, err := idx.db.Exec(`DELETE FROM sessions WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

func (idx *Index) Rebuild(store *Store) error {
	// Clear stale entries before re-indexing from disk
	if _, err := idx.db.Exec(`DELETE FROM sessions`); err != nil {
		return fmt.Errorf("clear index for rebuild: %w", err)
	}

	entries, err := os.ReadDir(store.dir)
	if err != nil {
		return fmt.Errorf("read store dir: %w", err)
	}

	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		id := strings.TrimSuffix(e.Name(), ".json")
		sess, err := store.Load(id)
		if err != nil {
			continue // skip corrupt files
		}
		if err := idx.UpsertSession(sess); err != nil {
			return fmt.Errorf("index session %s: %w", id, err)
		}
	}
	idx.needsRebuild = false
	return nil
}

func (idx *Index) LatestUpdatedID() (string, error) {
	var id string
	err := idx.db.QueryRow(
		`SELECT id FROM sessions ORDER BY updated_at DESC LIMIT 1`,
	).Scan(&id)
	if err == sql.ErrNoRows {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("latest updated: %w", err)
	}
	return id, nil
}

func (idx *Index) IsEmpty() (bool, error) {
	var count int
	err := idx.db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&count)
	if err != nil {
		return false, fmt.Errorf("check empty: %w", err)
	}
	return count == 0, nil
}

func parseTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t, _ = time.Parse("2006-01-02 15:04:05.999999999-07:00", s)
	}
	return t
}

func isFTSSyntaxError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "fts5: syntax error") ||
		strings.Contains(msg, "fts5 syntax error") ||
		strings.Contains(msg, "fts5:") ||
		strings.Contains(msg, "unterminated string")
}

// hasFTSOperator reports whether the query likely uses FTS5 syntax (quoted
// phrases, boolean operators, wildcards). Such queries should be passed to
// FTS5 verbatim without tokenization.
func hasFTSOperator(q string) bool {
	if strings.ContainsAny(q, `"*():^`) {
		return true
	}
	// Boolean operators must appear as separate words.
	for _, op := range []string{" AND ", " OR ", " NOT ", "NEAR("} {
		if strings.Contains(q, op) {
			return true
		}
	}
	return false
}
