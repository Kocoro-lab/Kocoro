package session

import (
	"database/sql"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	_ "modernc.org/sqlite"
)

// indexSchemaVersion is stamped into PRAGMA user_version. A mismatch drops
// stale FTS tables and rebuilds from the JSON on disk, so tokenizer changes
// never leave users with a half-migrated index.
//
// Versions:
//
//	0 — uninitialised.
//	1 — porter+unicode61 (shipped default).
//	2 — trigram tokenizer; native snippet() on original text.
const indexSchemaVersion = 2

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

CREATE TABLE IF NOT EXISTS messages (
    rowid      INTEGER PRIMARY KEY AUTOINCREMENT,
    session_id TEXT NOT NULL REFERENCES sessions(id) ON DELETE CASCADE,
    msg_index  INTEGER NOT NULL,
    role       TEXT NOT NULL,
    content    TEXT NOT NULL,
    UNIQUE(session_id, msg_index)
);

CREATE VIRTUAL TABLE IF NOT EXISTS messages_fts USING fts5(
    content,
    content=messages,
    content_rowid=rowid,
    tokenize='trigram'
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
	needsRebuild bool
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

	var stored int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&stored); err != nil {
		db.Close()
		return nil, fmt.Errorf("read user_version: %w", err)
	}

	needsRebuild := false
	if stored != indexSchemaVersion {
		// Any version mismatch — including stored==0 from shipped versions
		// of main that never stamped user_version and still have a porter
		// FTS table on disk — must drop the stale messages/FTS so the new
		// schema takes effect. Dropping with IF EXISTS is a no-op for fresh
		// installs. Sessions table is left alone (metadata-only) so the
		// session list stays visible while the rebuild runs.
		stmts := []string{
			`DROP TABLE IF EXISTS messages_fts`,
			`DROP TABLE IF EXISTS messages`,
		}
		for _, s := range stmts {
			if _, err := db.Exec(s); err != nil {
				db.Close()
				return nil, fmt.Errorf("drop stale table: %w", err)
			}
		}
		// Only flag rebuild when there was actual prior data to migrate.
		// For a truly fresh DB (stored==0, no sessions rows), NewStore's
		// IsEmpty check will cover the no-op case.
		var n int
		if err := db.QueryRow(`SELECT COUNT(*) FROM sessions`).Scan(&n); err == nil && n > 0 {
			needsRebuild = true
		}
	}

	if _, err := db.Exec(schema); err != nil {
		db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}

	if _, err := db.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, indexSchemaVersion)); err != nil {
		db.Close()
		return nil, fmt.Errorf("stamp user_version: %w", err)
	}

	return &Index{db: db, needsRebuild: needsRebuild}, nil
}

// NeedsRebuild reports whether the tokenizer version changed and the caller
// should invoke Rebuild against a Store to re-seed messages from JSON.
func (idx *Index) NeedsRebuild() bool { return idx.needsRebuild }

func (idx *Index) Close() error {
	return idx.db.Close()
}

func (idx *Index) UpsertSession(sess *Session) error {
	tx, err := idx.db.Begin()
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	// Upsert session row first (FK parent for messages).
	// msg_count is set to 0 initially and updated after indexing.
	_, err = tx.Exec(
		`INSERT OR REPLACE INTO sessions (id, title, cwd, created_at, updated_at, msg_count)
		 VALUES (?, ?, ?, ?, ?, 0)`,
		sess.ID, sess.Title, sess.CWD,
		sess.CreatedAt.Format(time.RFC3339Nano),
		sess.UpdatedAt.Format(time.RFC3339Nano),
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
		text := msg.Content.Text()
		if text == "" {
			continue
		}
		// msg_index is the original position in sess.Messages (may have gaps
		// where system-injected or empty entries were skipped).
		if _, err := tx.Exec(
			`INSERT INTO messages (session_id, msg_index, role, content) VALUES (?, ?, ?, ?)`,
			sess.ID, i, msg.Role, text,
		); err != nil {
			return fmt.Errorf("insert message %d: %w", i, err)
		}
	}

	// msg_count is the total message count, not the indexed-row count.
	// The desktop sidebar's "used session" filter relies on this reflecting
	// the full message list rather than what happened to land in the index.
	if _, err := tx.Exec(`UPDATE sessions SET msg_count = ? WHERE id = ?`, len(sess.Messages), sess.ID); err != nil {
		return fmt.Errorf("update msg_count: %w", err)
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

// Search runs the query against the FTS5 trigram index.
//
// Routing: queries are split into terms (whitespace-separated, with quoted
// phrases kept intact). If any term is under 3 runes, trigram cannot index it,
// so we route through a LIKE-based path that AND-intersects one LIKE clause
// per term against messages.content. This handles:
//   - bare short CJK:      登录
//   - quoted short CJK:    "登录"
//   - mixed short+long:    登录 failed  (both terms enforced; 登录 cannot be
//                                       silently dropped by trigram)
//
// When every term is long enough for trigram AND the query doesn't contain
// FTS5 operators that LIKE can't express, the fast MATCH path is used.
func (idx *Index) Search(query string, limit int) ([]SearchResult, error) {
	if limit <= 0 {
		limit = 20
	}
	query = strings.TrimSpace(query)
	if query == "" {
		return nil, nil
	}

	terms := splitQueryTerms(query)
	hasShort := false
	for _, t := range terms {
		if utf8.RuneCountInString(t) < 3 {
			hasShort = true
			break
		}
	}
	hasOperator := containsFTSOperator(query)

	if hasShort && !hasOperator {
		return idx.searchLike(terms, limit)
	}
	if hasShort && hasOperator {
		// Boolean operators (AND/OR/NOT/NEAR) and wildcards cannot be
		// faithfully translated to LIKE AND-intersection. Rather than
		// silently return wrong results — FTS5 trigram ignores terms
		// shorter than 3 characters — reject the query with a clear
		// message so the caller can reformulate.
		return nil, fmt.Errorf("invalid search query: terms shorter than 3 characters cannot be combined with AND/OR/NOT/NEAR or wildcard operators; remove operators or use longer terms")
	}

	rows, err := idx.db.Query(
		`SELECT m.session_id, s.title, m.role, m.msg_index, s.created_at,
		        snippet(messages_fts, 0, '>>>', '<<<', '...', 40)
		 FROM messages_fts
		 JOIN messages m ON m.rowid = messages_fts.rowid
		 JOIN sessions s ON s.id = m.session_id
		 WHERE messages_fts MATCH ?
		 ORDER BY rank
		 LIMIT ?`,
		query, limit,
	)
	if err != nil {
		if isFTSSyntaxError(err) {
			return nil, fmt.Errorf("invalid search query: %s", query)
		}
		return nil, fmt.Errorf("search: %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var createdStr string
		if err := rows.Scan(&r.SessionID, &r.SessionTitle, &r.Role, &r.MsgIndex, &createdStr, &r.Snippet); err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		r.CreatedAt = parseTime(createdStr)
		results = append(results, r)
	}
	return results, rows.Err()
}

// splitQueryTerms breaks a query into whitespace-separated terms, keeping
// double-quoted phrases as one term with the surrounding quotes stripped.
// Punctuation outside quotes is left attached to the term; we only care
// about grouping for the short-term detection.
func splitQueryTerms(q string) []string {
	var terms []string
	var cur strings.Builder
	inQuote := false
	flush := func() {
		s := strings.TrimSpace(cur.String())
		if s != "" {
			terms = append(terms, s)
		}
		cur.Reset()
	}
	for _, r := range q {
		switch {
		case r == '"':
			if inQuote {
				flush()
				inQuote = false
			} else {
				flush()
				inQuote = true
			}
		case !inQuote && (r == ' ' || r == '\t' || r == '\n'):
			flush()
		default:
			cur.WriteRune(r)
		}
	}
	flush()
	return terms
}

// containsFTSOperator reports whether the query uses FTS5 operators that the
// LIKE fallback cannot faithfully reproduce. Quoted phrases alone are fine
// (splitQueryTerms handles them); we only flag AND/OR/NOT/NEAR and wildcards.
func containsFTSOperator(q string) bool {
	if strings.ContainsAny(q, `*()^`) {
		return true
	}
	for _, op := range []string{" AND ", " OR ", " NOT ", "NEAR("} {
		if strings.Contains(q, op) {
			return true
		}
	}
	return false
}

// searchLike runs one LIKE '%term%' clause per term, AND-intersecting on
// message rowid. Every term must match for a row to be returned, so short CJK
// terms (e.g. 登录) cannot be silently dropped the way FTS5 trigram would.
func (idx *Index) searchLike(terms []string, limit int) ([]SearchResult, error) {
	if len(terms) == 0 {
		return nil, nil
	}
	var clauses []string
	args := make([]any, 0, len(terms)+1)
	for _, t := range terms {
		clauses = append(clauses, `m.content LIKE ? ESCAPE '\'`)
		args = append(args, "%"+escapeLike(t)+"%")
	}
	args = append(args, limit)
	sqlText := `SELECT m.session_id, s.title, m.role, m.msg_index, s.created_at, m.content
		 FROM messages m
		 JOIN sessions s ON s.id = m.session_id
		 WHERE ` + strings.Join(clauses, " AND ") + `
		 ORDER BY s.updated_at DESC
		 LIMIT ?`
	rows, err := idx.db.Query(sqlText, args...)
	if err != nil {
		return nil, fmt.Errorf("search (like): %w", err)
	}
	defer rows.Close()

	var results []SearchResult
	for rows.Next() {
		var r SearchResult
		var createdStr, content string
		if err := rows.Scan(&r.SessionID, &r.SessionTitle, &r.Role, &r.MsgIndex, &createdStr, &content); err != nil {
			return nil, fmt.Errorf("scan result: %w", err)
		}
		r.CreatedAt = parseTime(createdStr)
		// Snippet around the first term match; fall back to the first term.
		// Centre the snippet on the earliest matching term so the user can
		// see why the result matched when their query had multiple terms.
		r.Snippet = likeSnippet(content, terms)
		results = append(results, r)
	}
	return results, rows.Err()
}

func escapeLike(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return r.Replace(s)
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
			// Skip unreadable files so one bad session doesn't block the
			// whole rebuild. Log so the user has breadcrumbs if search
			// misses a session after migration.
			log.Printf("session.Rebuild: skip %s: %v", id, err)
			continue
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

// likeSnippet produces a short context window around the earliest match of
// any term in content, wrapping the matched range with >>>…<<<. Used by the
// LIKE fallback path (short CJK queries) where FTS5's native snippet() is
// not available.
//
// Matching is case-insensitive and operates on rune slices end-to-end so it
// stays correct for codepoints whose byte length changes under ToLower
// (e.g. Turkish "İ" → "i\u0307").
func likeSnippet(content string, terms []string) string {
	const window = 40
	if content == "" || len(terms) == 0 {
		return truncateRunes(content, window*2)
	}

	contentRunes := []rune(content)
	contentLower := make([]rune, len(contentRunes))
	for i, r := range contentRunes {
		contentLower[i] = toLowerRune(r)
	}

	// Find earliest match across all non-empty terms.
	bestStart, bestLen := -1, 0
	for _, t := range terms {
		if t == "" {
			continue
		}
		qRunes := []rune(t)
		qLower := make([]rune, len(qRunes))
		for i, r := range qRunes {
			qLower[i] = toLowerRune(r)
		}
		start := indexRune(contentLower, qLower)
		if start < 0 {
			continue
		}
		if bestStart < 0 || start < bestStart {
			bestStart = start
			bestLen = len(qLower)
		}
	}

	if bestStart < 0 {
		return truncateRunes(content, window*2)
	}

	end := bestStart + bestLen
	left := bestStart - window
	if left < 0 {
		left = 0
	}
	right := end + window
	if right > len(contentRunes) {
		right = len(contentRunes)
	}
	var b strings.Builder
	if left > 0 {
		b.WriteString("...")
	}
	b.WriteString(string(contentRunes[left:bestStart]))
	b.WriteString(">>>")
	b.WriteString(string(contentRunes[bestStart:end]))
	b.WriteString("<<<")
	b.WriteString(string(contentRunes[end:right]))
	if right < len(contentRunes) {
		b.WriteString("...")
	}
	return b.String()
}

// toLowerRune lowercases a single rune via unicode.ToLower, which is a 1:1
// mapping and therefore safe for the fixed-rune-count snippet window. Unlike
// strings.ToLower on the full string, it never expands a rune (e.g. Turkish
// "İ" stays as one rune rather than becoming "i\u0307"), so byte-offset
// drift between the lowercased and original strings cannot happen.
func toLowerRune(r rune) rune {
	return unicode.ToLower(r)
}

// indexRune returns the index of sub in s, or -1. Both args are rune slices.
func indexRune(s, sub []rune) int {
	if len(sub) == 0 {
		return 0
	}
	if len(sub) > len(s) {
		return -1
	}
	for i := 0; i <= len(s)-len(sub); i++ {
		match := true
		for j, r := range sub {
			if s[i+j] != r {
				match = false
				break
			}
		}
		if match {
			return i
		}
	}
	return -1
}

func truncateRunes(s string, maxRunes int) string {
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes]) + "..."
}

func isFTSSyntaxError(err error) bool {
	msg := err.Error()
	return strings.Contains(msg, "fts5: syntax error") ||
		strings.Contains(msg, "fts5 syntax error") ||
		strings.Contains(msg, "fts5:") ||
		strings.Contains(msg, "unterminated string")
}
