package session

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// mustUpsert wraps UpsertSession with t.Fatal on error, used by tests that
// only care about seeding rows (not about exercising the upsert path itself).
func mustUpsert(t *testing.T, idx *Index, s *Session) {
	t.Helper()
	if err := idx.UpsertSession(s); err != nil {
		t.Fatalf("UpsertSession(%s): %v", s.ID, err)
	}
}

func TestIndex_OpenClose(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestIndex_UpsertAndList(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	sess := &Session{
		ID:        "sess-1",
		Title:     "First session",
		CWD:       "/tmp/test",
		CreatedAt: now,
		UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello world")},
			{Role: "assistant", Content: client.NewTextContent("hi there")},
		},
	}

	if err := idx.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	summaries, err := idx.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 session, got %d", len(summaries))
	}
	if summaries[0].ID != "sess-1" {
		t.Errorf("expected ID 'sess-1', got %q", summaries[0].ID)
	}
	if summaries[0].Title != "First session" {
		t.Errorf("expected title 'First session', got %q", summaries[0].Title)
	}
	if summaries[0].MsgCount != 2 {
		t.Errorf("expected 2 messages, got %d", summaries[0].MsgCount)
	}
}

func TestIndex_ListOrder(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	older := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	if err := idx.UpsertSession(&Session{
		ID: "old", Title: "Old session", CreatedAt: older, UpdatedAt: older,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertSession(&Session{
		ID: "new", Title: "New session", CreatedAt: newer, UpdatedAt: newer,
	}); err != nil {
		t.Fatal(err)
	}

	summaries, err := idx.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2, got %d", len(summaries))
	}
	if summaries[0].ID != "new" {
		t.Errorf("expected newest first, got %q", summaries[0].ID)
	}
	if summaries[1].ID != "old" {
		t.Errorf("expected oldest second, got %q", summaries[1].ID)
	}
}

func TestIndex_Search(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	sess := &Session{
		ID: "search-1", Title: "Search test", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("tell me about websocket reconnect logic")},
			{Role: "assistant", Content: client.NewTextContent("the reconnect uses exponential backoff")},
		},
	}
	if err := idx.UpsertSession(sess); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search("websocket", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected at least one result")
	}
	if results[0].SessionID != "search-1" {
		t.Errorf("expected session 'search-1', got %q", results[0].SessionID)
	}
	if results[0].SessionTitle != "Search test" {
		t.Errorf("expected title 'Search test', got %q", results[0].SessionTitle)
	}
	if results[0].Role != "user" {
		t.Errorf("expected role 'user', got %q", results[0].Role)
	}
	if !strings.Contains(results[0].Snippet, "websocket") {
		t.Errorf("snippet should contain 'websocket', got %q", results[0].Snippet)
	}
}

func TestIndex_SearchStemming(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	sess := &Session{
		ID: "stem-1", Title: "Stemming", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("the server is running on port 8080")},
		},
	}
	if err := idx.UpsertSession(sess); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search("run", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected stemmed match for 'run' -> 'running'")
	}
}

func TestIndex_SearchNoResults(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "no-match", Title: "No match", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello world")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search("xyzzynonexistent", 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 results, got %d", len(results))
	}
}

func TestIndex_SearchPhraseQuery(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "phrase-1", Title: "Phrase test", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("fix the websocket reconnect issue")},
			{Role: "user", Content: client.NewTextContent("websocket is fine but reconnect is broken elsewhere")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	results, err := idx.Search(`"websocket reconnect"`, 20)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) == 0 {
		t.Fatal("expected phrase match")
	}
	// The phrase "websocket reconnect" only appears adjacent in msg_index 0
	if results[0].MsgIndex != 0 {
		t.Errorf("expected msg_index 0 for phrase match, got %d", results[0].MsgIndex)
	}
}

func TestIndex_SearchMalformed(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	// Insert data so FTS table is non-empty (empty FTS skips MATCH evaluation)
	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "mal-1", Title: "Malformed", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("some content")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	_, err = idx.Search(`"unbalanced`, 20)
	if err == nil {
		t.Fatal("expected error for malformed query")
	}
	if !strings.Contains(err.Error(), "invalid search query") {
		t.Errorf("expected clean error message, got: %v", err)
	}
}

func TestIndex_Delete(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)
	sess := &Session{
		ID: "del-1", Title: "Delete me", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("unique deletable content")},
		},
	}
	if err := idx.UpsertSession(sess); err != nil {
		t.Fatal(err)
	}

	if err := idx.DeleteSession("del-1"); err != nil {
		t.Fatalf("DeleteSession: %v", err)
	}

	// Verify gone from list
	summaries, err := idx.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 0 {
		t.Errorf("expected 0 sessions after delete, got %d", len(summaries))
	}

	// Verify gone from FTS
	results, err := idx.Search("deletable", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 0 {
		t.Errorf("expected 0 FTS results after delete, got %d", len(results))
	}
}

func TestIndex_UpsertUpdatesExisting(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)

	// First upsert
	if err := idx.UpsertSession(&Session{
		ID: "upd-1", Title: "Original", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("original content alpha")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Second upsert with updated title and new messages
	if err := idx.UpsertSession(&Session{
		ID: "upd-1", Title: "Updated", CreatedAt: now, UpdatedAt: now.Add(time.Minute),
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("original content alpha")},
			{Role: "assistant", Content: client.NewTextContent("new content bravo")},
		},
	}); err != nil {
		t.Fatal(err)
	}

	summaries, err := idx.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 session, got %d", len(summaries))
	}
	if summaries[0].Title != "Updated" {
		t.Errorf("expected title 'Updated', got %q", summaries[0].Title)
	}
	if summaries[0].MsgCount != 2 {
		t.Errorf("expected 2 messages, got %d", summaries[0].MsgCount)
	}

	// Both terms should be searchable
	r1, err := idx.Search("alpha", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(r1) == 0 {
		t.Error("expected 'alpha' to still be searchable")
	}

	r2, err := idx.Search("bravo", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(r2) == 0 {
		t.Error("expected 'bravo' to be searchable after upsert")
	}
}

func TestIndex_Rebuild(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir}

	// Create sessions via Store (JSON files)
	now := time.Now().Truncate(time.Second)
	s1 := &Session{
		ID: "rb-1", Title: "Rebuild one", CreatedAt: now, UpdatedAt: now,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("rebuild test alpha")},
		},
	}
	s2 := &Session{
		ID: "rb-2", Title: "Rebuild two", CreatedAt: now.Add(time.Second), UpdatedAt: now.Add(time.Second),
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("rebuild test bravo")},
		},
	}
	if err := store.Save(s1); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(s2); err != nil {
		t.Fatal(err)
	}

	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	if err := idx.Rebuild(store); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	summaries, err := idx.ListSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 2 {
		t.Fatalf("expected 2 sessions after rebuild, got %d", len(summaries))
	}

	// Verify FTS works
	results, err := idx.Search("bravo", 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Error("expected FTS results after rebuild")
	}
}

func TestIndex_LatestUpdated(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)

	if err := idx.UpsertSession(&Session{
		ID: "old", Title: "Old", CreatedAt: t1, UpdatedAt: t1,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertSession(&Session{
		ID: "new", Title: "New", CreatedAt: t1, UpdatedAt: t2,
	}); err != nil {
		t.Fatal(err)
	}

	id, err := idx.LatestUpdatedID()
	if err != nil {
		t.Fatalf("LatestUpdatedID: %v", err)
	}
	if id != "new" {
		t.Errorf("expected 'new', got %q", id)
	}
}

func TestIndex_LatestUpdatedByRouteKey(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	t1 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	routeKey := "default:slack:C123-1710000000.000100"

	if err := idx.UpsertSession(&Session{
		ID: "route-old", Title: "Old", CreatedAt: t1, UpdatedAt: t1, RouteKey: routeKey,
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertSession(&Session{
		ID: "other", Title: "Other", CreatedAt: t1, UpdatedAt: t2, RouteKey: "default:slack:C999",
	}); err != nil {
		t.Fatal(err)
	}
	if err := idx.UpsertSession(&Session{
		ID: "route-new", Title: "New", CreatedAt: t1, UpdatedAt: t2, RouteKey: routeKey,
	}); err != nil {
		t.Fatal(err)
	}

	id, err := idx.LatestUpdatedIDByRouteKey(routeKey)
	if err != nil {
		t.Fatalf("LatestUpdatedIDByRouteKey: %v", err)
	}
	if id != "route-new" {
		t.Errorf("expected 'route-new', got %q", id)
	}
}

func TestIndex_SearchFTSSyntaxError(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	// Insert data so FTS actually runs the query
	idx.UpsertSession(&Session{
		ID: "data", Title: "Data", CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("some data")},
		},
	})

	// Various malformed queries
	badQueries := []string{
		`"unbalanced`,
		`AND`,
		`OR OR`,
		`NOT`,
	}
	for _, q := range badQueries {
		_, err := idx.Search(q, 10)
		if err == nil {
			continue // some "bad" queries may be valid in FTS5, that's OK
		}
		// If it errors, the message should be clean (not raw sqlite)
		if !strings.Contains(err.Error(), "invalid search query") {
			t.Errorf("query %q: expected clean error, got: %v", q, err)
		}
	}
}

func TestIndex_UpsertSkipsSystemInjected(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().Truncate(time.Second)

	tests := []struct {
		name         string
		messages     []client.Message
		meta         []MessageMeta
		wantMsgCount int
		searchHit    string
		searchMiss   string
	}{
		{
			name: "no meta (legacy session) indexes all",
			messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("legacy alpha")},
				{Role: "assistant", Content: client.NewTextContent("legacy bravo")},
			},
			meta:         nil,
			wantMsgCount: 2,
			searchHit:    "alpha",
			searchMiss:   "",
		},
		{
			// msg_count reflects total messages (len(sess.Messages)), not
			// indexed rows, so the desktop sidebar's "used session" filter
			// stays consistent with what the user sees in the conversation.
			name: "injected messages excluded from index",
			messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("visible unicorn")},
				{Role: "assistant", Content: client.NewTextContent("injected giraffe")},
				{Role: "assistant", Content: client.NewTextContent("visible elephant")},
			},
			meta: []MessageMeta{
				{},
				{SystemInjected: true},
				{},
			},
			wantMsgCount: 3,
			searchHit:    "unicorn",
			searchMiss:   "giraffe",
		},
		{
			name: "short meta (fewer than messages) indexes unmatched positions",
			messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("first penguin")},
				{Role: "assistant", Content: client.NewTextContent("second dolphin")},
				{Role: "user", Content: client.NewTextContent("third falcon")},
			},
			meta: []MessageMeta{
				{SystemInjected: true},
			},
			wantMsgCount: 3,
			searchHit:    "dolphin",
			searchMiss:   "penguin",
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sessID := fmt.Sprintf("injected-%d", i)
			sess := &Session{
				ID: sessID, Title: tt.name, CreatedAt: now, UpdatedAt: now,
				Messages:    tt.messages,
				MessageMeta: tt.meta,
			}
			if err := idx.UpsertSession(sess); err != nil {
				t.Fatalf("UpsertSession: %v", err)
			}

			summaries, err := idx.ListSessions()
			if err != nil {
				t.Fatalf("ListSessions: %v", err)
			}
			var found bool
			for _, s := range summaries {
				if s.ID == sessID {
					found = true
					if s.MsgCount != tt.wantMsgCount {
						t.Errorf("msg_count = %d, want %d", s.MsgCount, tt.wantMsgCount)
					}
				}
			}
			if !found {
				t.Fatalf("session %q not found in list", sessID)
			}

			if tt.searchHit != "" {
				results, err := idx.Search(tt.searchHit, 20)
				if err != nil {
					t.Fatalf("Search(%q): %v", tt.searchHit, err)
				}
				if len(results) == 0 {
					t.Errorf("expected hit for %q", tt.searchHit)
				}
			}
			if tt.searchMiss != "" {
				results, err := idx.Search(tt.searchMiss, 20)
				if err != nil {
					t.Fatalf("Search(%q): %v", tt.searchMiss, err)
				}
				for _, r := range results {
					if r.SessionID == sessID {
						t.Errorf("expected no hit for %q in session %q, but got one", tt.searchMiss, sessID)
					}
				}
			}
		})
	}
}

func TestIndex_IsEmpty(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer idx.Close()

	empty, err := idx.IsEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Error("expected empty index")
	}

	now := time.Now().Truncate(time.Second)
	if err := idx.UpsertSession(&Session{
		ID: "x", Title: "X", CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	empty, err = idx.IsEmpty()
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Error("expected non-empty index after insert")
	}
}

// TestIndex_V2MigrationRebuildsFromJSON verifies that an existing v2
// sessions.db (no `source` column, PRAGMA user_version = 2) can be opened by
// the current schema without error: the rebuild path drops messages tables, the
// ALTER TABLE steps backfill new columns, and subsequent UpsertSession calls
// populate the new metadata correctly.
func TestIndex_V2MigrationRebuildsFromJSON(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	{
		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open raw sqlite: %v", err)
		}
		// Exact v2 CREATE TABLE — no source column.
		if _, err := raw.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, msg_count INTEGER NOT NULL DEFAULT 0)`); err != nil {
			t.Fatalf("create v2 sessions table: %v", err)
		}
		if _, err := raw.Exec(`PRAGMA user_version = 2`); err != nil {
			t.Fatalf("stamp v2 user_version: %v", err)
		}
		now := time.Now().UTC()
		if _, err := raw.Exec(
			`INSERT INTO sessions (id, title, created_at, updated_at, msg_count) VALUES (?, ?, ?, ?, ?)`,
			"s1", "old session", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), 1,
		); err != nil {
			t.Fatalf("seed v2 row: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Fatalf("close raw sqlite: %v", err)
		}
	}

	// Open via the actual API. The version mismatch MUST trigger the
	// drop-and-rebuild path AND backfill new columns.
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex migration: %v", err)
	}
	defer idx.Close()

	// Confirm new column is queryable. Rebuild reads JSON files on disk
	// (none seeded here), so an empty result set is expected — the assertion
	// is that the query SUCCEEDS against the migrated schema.
	rows, err := idx.ListUpdatedSince(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("ListUpdatedSince after migration: %v", err)
	}
	_ = rows

	// Confirm a fresh Upsert populates the source and route key columns.
	if err := idx.UpsertSession(&Session{
		ID: "s2", Source: "slack", RouteKey: "default:slack:T1",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertSession with metadata: %v", err)
	}
	rows2, err := idx.ListUpdatedSince(context.Background(), time.Time{})
	if err != nil {
		t.Fatalf("ListUpdatedSince after upsert: %v", err)
	}
	found := false
	for _, r := range rows2 {
		if r.ID == "s2" && r.Source == "slack" {
			found = true
		}
	}
	if !found {
		t.Errorf("expected s2 with Source=slack after migration; got %+v", rows2)
	}
	id, err := idx.LatestUpdatedIDByRouteKey("default:slack:T1")
	if err != nil {
		t.Fatalf("LatestUpdatedIDByRouteKey after migration: %v", err)
	}
	if id != "s2" {
		t.Fatalf("route lookup after migration = %q, want s2", id)
	}
}

// TestIndex_V3ToV4MigrationAddsRouteKey covers the production upgrade path
// from v0.1.1 (schema v3: has `source`, no `route_key`) to v0.1.2+ (schema v4:
// adds `route_key`). The v2 migration test exercises the same code by way of
// "duplicate column" tolerance, but daemons running the just-shipped v0.1.1
// enter migration at v3 specifically — this asserts that entry point.
func TestIndex_V3ToV4MigrationAddsRouteKey(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "sessions.db")
	{
		raw, err := sql.Open("sqlite", dbPath)
		if err != nil {
			t.Fatalf("open raw sqlite: %v", err)
		}
		// Exact v3 CREATE TABLE — has `source`, no `route_key`.
		if _, err := raw.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT NOT NULL DEFAULT '', cwd TEXT NOT NULL DEFAULT '', created_at DATETIME NOT NULL, updated_at DATETIME NOT NULL, msg_count INTEGER NOT NULL DEFAULT 0, source TEXT NOT NULL DEFAULT '')`); err != nil {
			t.Fatalf("create v3 sessions table: %v", err)
		}
		if _, err := raw.Exec(`PRAGMA user_version = 3`); err != nil {
			t.Fatalf("stamp v3 user_version: %v", err)
		}
		now := time.Now().UTC()
		if _, err := raw.Exec(
			`INSERT INTO sessions (id, title, created_at, updated_at, msg_count, source) VALUES (?, ?, ?, ?, ?, ?)`,
			"s-pre", "v3 session", now.Format(time.RFC3339Nano), now.Format(time.RFC3339Nano), 1, "slack",
		); err != nil {
			t.Fatalf("seed v3 row: %v", err)
		}
		if err := raw.Close(); err != nil {
			t.Fatalf("close raw sqlite: %v", err)
		}
	}

	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex v3→v4 migration: %v", err)
	}
	defer idx.Close()

	// Pre-existing v3 row survives the migration with empty route_key.
	id, err := idx.LatestUpdatedIDByRouteKey("default:slack:Tnew")
	if err != nil {
		t.Fatalf("LatestUpdatedIDByRouteKey on empty: %v", err)
	}
	if id != "" {
		t.Errorf("expected no match for fresh route_key, got %q", id)
	}

	// Fresh upsert populates route_key, and the new column is queryable.
	if err := idx.UpsertSession(&Session{
		ID: "s-post", Source: "slack", RouteKey: "default:slack:Tnew",
		CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("UpsertSession post-migration: %v", err)
	}
	id, err = idx.LatestUpdatedIDByRouteKey("default:slack:Tnew")
	if err != nil {
		t.Fatalf("LatestUpdatedIDByRouteKey post-upsert: %v", err)
	}
	if id != "s-post" {
		t.Fatalf("route lookup after migration = %q, want s-post", id)
	}
}

func TestIndex_ListUpdatedSince(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now().UTC().Truncate(time.Second)
	older := now.Add(-2 * time.Hour)
	newer := now.Add(-30 * time.Minute)
	newest := now.Add(-1 * time.Minute)

	mustUpsert(t, idx, &Session{ID: "old", CreatedAt: older, UpdatedAt: older})
	mustUpsert(t, idx, &Session{ID: "mid", CreatedAt: newer, UpdatedAt: newer})
	mustUpsert(t, idx, &Session{ID: "new", CreatedAt: newest, UpdatedAt: newest})

	cutoff := now.Add(-1 * time.Hour)
	rows, err := idx.ListUpdatedSince(context.Background(), cutoff)
	if err != nil {
		t.Fatalf("ListUpdatedSince: %v", err)
	}

	gotIDs := map[string]bool{}
	for _, r := range rows {
		gotIDs[r.ID] = true
	}
	if gotIDs["old"] {
		t.Errorf("ListUpdatedSince should exclude sessions with updated_at <= cutoff")
	}
	if !gotIDs["mid"] || !gotIDs["new"] {
		t.Errorf("ListUpdatedSince should include sessions with updated_at > cutoff; got %v", gotIDs)
	}
	if len(rows) != 2 {
		t.Errorf("expected 2 rows, got %d: %+v", len(rows), rows)
	}
}

func TestIndex_UpdateSessionFlags(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)
	mustUpsert(t, idx, &Session{ID: "s1", Title: "one", CreatedAt: now, UpdatedAt: now})
	tru, fls := true, false

	t.Run("set pinned only leaves favorite", func(t *testing.T) {
		if err := idx.UpdateSessionFlags("s1", &tru, nil); err != nil {
			t.Fatalf("UpdateSessionFlags: %v", err)
		}
		sums, _ := idx.ListSessions()
		if len(sums) != 1 || !sums[0].Pinned || sums[0].Favorite {
			t.Errorf("expected Pinned=true Favorite=false, got %+v", sums)
		}
	})

	t.Run("set favorite preserves pinned", func(t *testing.T) {
		if err := idx.UpdateSessionFlags("s1", nil, &tru); err != nil {
			t.Fatalf("UpdateSessionFlags: %v", err)
		}
		sums, _ := idx.ListSessions()
		if !sums[0].Pinned || !sums[0].Favorite {
			t.Errorf("expected both true, got %+v", sums[0])
		}
	})

	t.Run("clear both", func(t *testing.T) {
		if err := idx.UpdateSessionFlags("s1", &fls, &fls); err != nil {
			t.Fatalf("UpdateSessionFlags: %v", err)
		}
		sums, _ := idx.ListSessions()
		if sums[0].Pinned || sums[0].Favorite {
			t.Errorf("expected both false, got %+v", sums[0])
		}
	})

	t.Run("missing id returns os.ErrNotExist", func(t *testing.T) {
		err := idx.UpdateSessionFlags("does-not-exist", &tru, nil)
		if err == nil {
			t.Fatal("expected error")
		}
		if !os.IsNotExist(err) {
			t.Errorf("expected os.ErrNotExist, got %v", err)
		}
	})

	t.Run("both nil is no-op", func(t *testing.T) {
		if err := idx.UpdateSessionFlags("s1", nil, nil); err != nil {
			t.Errorf("nil/nil should be no-op, got %v", err)
		}
	})

	t.Run("does not touch messages table", func(t *testing.T) {
		// Seed messages, toggle flags, confirm rowid + count are unchanged.
		seed := &Session{
			ID:        "s2",
			Title:     "two",
			CreatedAt: now,
			UpdatedAt: now,
			Messages: []client.Message{
				{Role: "user", Content: client.NewTextContent("hello world")},
			},
		}
		mustUpsert(t, idx, seed)

		var rowidBefore int64
		idx.db.QueryRow(`SELECT rowid FROM messages WHERE session_id='s2'`).Scan(&rowidBefore)
		if rowidBefore == 0 {
			t.Fatal("seed message not indexed")
		}

		if err := idx.UpdateSessionFlags("s2", &tru, &tru); err != nil {
			t.Fatalf("UpdateSessionFlags: %v", err)
		}

		var rowidAfter int64
		idx.db.QueryRow(`SELECT rowid FROM messages WHERE session_id='s2'`).Scan(&rowidAfter)
		if rowidAfter != rowidBefore {
			t.Errorf("messages rowid changed: before=%d after=%d (narrow UPDATE must not rebuild messages)", rowidBefore, rowidAfter)
		}
	})
}

func TestIndex_ListSessions_PinnedFirst(t *testing.T) {
	dir := t.TempDir()
	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	old := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	mid := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
	newer := time.Date(2026, 5, 1, 0, 0, 0, 0, time.UTC)

	// "old" gets pinned; "newer" stays unpinned and would otherwise sort first.
	mustUpsert(t, idx, &Session{ID: "old", Title: "Old", CreatedAt: old, UpdatedAt: old, Pinned: true})
	mustUpsert(t, idx, &Session{ID: "mid", Title: "Mid", CreatedAt: mid, UpdatedAt: mid})
	mustUpsert(t, idx, &Session{ID: "newer", Title: "Newer", CreatedAt: newer, UpdatedAt: newer})

	sums, err := idx.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	if len(sums) != 3 {
		t.Fatalf("expected 3, got %d", len(sums))
	}
	if sums[0].ID != "old" || !sums[0].Pinned {
		t.Errorf("expected pinned 'old' first, got %q (pinned=%v)", sums[0].ID, sums[0].Pinned)
	}
	if sums[1].ID != "newer" {
		t.Errorf("expected 'newer' second among unpinned, got %q", sums[1].ID)
	}
	if sums[2].ID != "mid" {
		t.Errorf("expected 'mid' last, got %q", sums[2].ID)
	}
}
