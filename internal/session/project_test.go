package session

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestIndex_ProjectIDRoundTrip verifies project_id survives an upsert→list
// cycle and that the narrow UpdateSessionProjectID re-files (and unfiles) a row
// without touching the rest of the record.
func TestIndex_ProjectIDRoundTrip(t *testing.T) {
	idx, err := OpenIndex(t.TempDir())
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	now := time.Now()
	sess := &Session{
		ID:        "2026-07-24-aaaaaaaaaaaa",
		Title:     "Kyoto trip",
		ProjectID: "proj-abc123",
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
	}
	if err := idx.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}

	got := findSummary(t, idx, sess.ID)
	if got.ProjectID != "proj-abc123" {
		t.Fatalf("project_id after upsert = %q, want proj-abc123", got.ProjectID)
	}

	// Re-file to a different project.
	if err := idx.UpdateSessionProjectID(sess.ID, "proj-xyz789"); err != nil {
		t.Fatalf("UpdateSessionProjectID re-file: %v", err)
	}
	if got := findSummary(t, idx, sess.ID); got.ProjectID != "proj-xyz789" {
		t.Fatalf("project_id after re-file = %q, want proj-xyz789", got.ProjectID)
	}

	// Unfile (empty string).
	if err := idx.UpdateSessionProjectID(sess.ID, ""); err != nil {
		t.Fatalf("UpdateSessionProjectID unfile: %v", err)
	}
	if got := findSummary(t, idx, sess.ID); got.ProjectID != "" {
		t.Fatalf("project_id after unfile = %q, want empty", got.ProjectID)
	}
}

// TestIndex_UpdateProjectID_MissingRow ensures a narrow update on an unknown id
// reports os.ErrNotExist so callers (Store.PatchProjectID) can fall back to a
// full UpsertSession.
func TestIndex_UpdateProjectID_MissingRow(t *testing.T) {
	idx, err := OpenIndex(t.TempDir())
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	defer idx.Close()

	err = idx.UpdateSessionProjectID("2026-07-24-nonexistent0", "proj-abc123")
	if err == nil {
		t.Fatal("UpdateSessionProjectID on missing row: want error, got nil")
	}
}

// TestIndex_ReopenIdempotent guards the unconditional `ALTER TABLE ... ADD
// COLUMN project_id` that runs on every OpenIndex without a schema-version
// bump: reopening an existing DB (where the column already exists) must not
// error, and existing rows must survive.
func TestIndex_ReopenIdempotent(t *testing.T) {
	dir := t.TempDir()

	idx, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex (first): %v", err)
	}
	now := time.Now()
	sess := &Session{
		ID:        "2026-07-24-bbbbbbbbbbbb",
		Title:     "Persisted",
		ProjectID: "proj-keep01",
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
	}
	if err := idx.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	if err := idx.Close(); err != nil {
		t.Fatalf("Close (first): %v", err)
	}

	// Reopen the same directory — the ALTER must be a no-op, not an error.
	idx2, err := OpenIndex(dir)
	if err != nil {
		t.Fatalf("OpenIndex (reopen): %v", err)
	}
	defer idx2.Close()

	got := findSummary(t, idx2, sess.ID)
	if got.ProjectID != "proj-keep01" {
		t.Fatalf("project_id after reopen = %q, want proj-keep01", got.ProjectID)
	}
}

// TestStore_PatchProjectID exercises the store-level re-file used by
// PATCH /sessions: set, re-file, unfile via "", nil no-op, and that
// re-filing never bumps UpdatedAt (project_id is a logical tag, not an edit).
func TestStore_PatchProjectID(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)

	now := time.Now().Add(-time.Hour).Round(time.Second)
	sess := &Session{
		ID:        "2026-07-24-cccccccccccc",
		Title:     "Filing target",
		CreatedAt: now,
		UpdatedAt: now,
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	baseUpdated := loadSession(t, store, sess.ID).UpdatedAt

	// Set.
	blue := "proj-blue01"
	if err := store.PatchProjectID(sess.ID, &blue); err != nil {
		t.Fatalf("PatchProjectID set: %v", err)
	}
	if got := loadSession(t, store, sess.ID); got.ProjectID != blue {
		t.Fatalf("project_id after set = %q, want %q", got.ProjectID, blue)
	}

	// UpdatedAt must be unchanged — re-filing is not an edit.
	if got := loadSession(t, store, sess.ID); !got.UpdatedAt.Equal(baseUpdated) {
		t.Fatalf("UpdatedAt changed by re-file: got %v, want %v", got.UpdatedAt, baseUpdated)
	}

	// Unfile via empty string.
	empty := ""
	if err := store.PatchProjectID(sess.ID, &empty); err != nil {
		t.Fatalf("PatchProjectID unfile: %v", err)
	}
	if got := loadSession(t, store, sess.ID); got.ProjectID != "" {
		t.Fatalf("project_id after unfile = %q, want empty", got.ProjectID)
	}

	// nil pointer is a no-op — refile first, then a nil call must not clear it.
	if err := store.PatchProjectID(sess.ID, &blue); err != nil {
		t.Fatalf("PatchProjectID re-set: %v", err)
	}
	if err := store.PatchProjectID(sess.ID, nil); err != nil {
		t.Fatalf("PatchProjectID nil: %v", err)
	}
	if got := loadSession(t, store, sess.ID); got.ProjectID != blue {
		t.Fatalf("project_id after nil no-op = %q, want %q", got.ProjectID, blue)
	}

	// The re-file must be visible through the index too (ListSessions).
	if got := findSummary(t, store.index, sess.ID); got.ProjectID != blue {
		t.Fatalf("index project_id after re-file = %q, want %q", got.ProjectID, blue)
	}
}

// findSummary returns the summary for id from idx.ListSessions, failing if absent.
func findSummary(t *testing.T, idx *Index, id string) SessionSummary {
	t.Helper()
	summaries, err := idx.ListSessions()
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}
	for _, s := range summaries {
		if s.ID == id {
			return s
		}
	}
	t.Fatalf("session %q not found in index", id)
	return SessionSummary{}
}

func loadSession(t *testing.T, store *Store, id string) *Session {
	t.Helper()
	sess, err := store.Load(id)
	if err != nil {
		t.Fatalf("Load %q: %v", id, err)
	}
	return sess
}
