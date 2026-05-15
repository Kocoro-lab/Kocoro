package daemon

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
	_ "modernc.org/sqlite"
)

func openTestMailboxStore(t *testing.T) (*MailboxStore, func()) {
	t.Helper()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "test_mailbox.db")
	db, err := sql.Open("sqlite", dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	store, err := NewMailboxStore(db)
	if err != nil {
		db.Close()
		t.Fatalf("new store: %v", err)
	}
	return store, func() {
		db.Close()
		os.Remove(dbPath)
	}
}

func TestMailboxStore_AppendAndLoad(t *testing.T) {
	store, cleanup := openTestMailboxStore(t)
	defer cleanup()

	msg := agenttypes.QueuedMessage{
		ID:         "msg-1",
		RouteKey:   "r1",
		SessionID:  "s1",
		Source:     "ws",
		Text:       "hello",
		Priority:   agenttypes.PriorityNext,
		EnqueuedAt: time.Now().UTC().Truncate(time.Millisecond),
		Editable:   true,
	}
	inserted, err := store.Append(msg)
	if err != nil {
		t.Fatalf("append: %v", err)
	}
	if !inserted {
		t.Error("Append should report inserted=true on first call")
	}

	loaded, err := store.LoadPendingByRoute("r1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(loaded) != 1 || loaded[0].ID != "msg-1" || loaded[0].Text != "hello" {
		t.Errorf("loaded mismatch: %+v", loaded)
	}
}

func TestMailboxStore_DedupByCloudMsgID(t *testing.T) {
	store, cleanup := openTestMailboxStore(t)
	defer cleanup()

	msg := agenttypes.QueuedMessage{
		ID: "id-A", RouteKey: "r1", CloudMsgID: "cloud-X",
		Text: "hello", EnqueuedAt: time.Now(),
	}
	if inserted, err := store.Append(msg); err != nil || !inserted {
		t.Fatalf("first append: inserted=%v err=%v", inserted, err)
	}

	// Cloud replays the same msg_id under a different ULID; should no-op.
	dup := msg
	dup.ID = "id-B"
	inserted, err := store.Append(dup)
	if err != nil {
		t.Fatalf("dup append: %v", err)
	}
	if inserted {
		t.Error("Append should report inserted=false for duplicate cloud_msg_id")
	}

	loaded, _ := store.LoadPendingByRoute("r1")
	if len(loaded) != 1 || loaded[0].ID != "id-A" {
		t.Errorf("dedup failed: %+v", loaded)
	}
}

func TestMailboxStore_EmptyCloudMsgIDNotDeduped(t *testing.T) {
	store, cleanup := openTestMailboxStore(t)
	defer cleanup()

	// Two HTTP/TUI messages with empty cloud_msg_id must both insert.
	if inserted, _ := store.Append(agenttypes.QueuedMessage{ID: "a", RouteKey: "r1", EnqueuedAt: time.Now()}); !inserted {
		t.Fatal("first insert should succeed")
	}
	if inserted, _ := store.Append(agenttypes.QueuedMessage{ID: "b", RouteKey: "r1", EnqueuedAt: time.Now()}); !inserted {
		t.Error("second insert with empty cloud_msg_id should also succeed")
	}
}

func TestMailboxStore_MarkConsumed(t *testing.T) {
	store, cleanup := openTestMailboxStore(t)
	defer cleanup()
	store.Append(agenttypes.QueuedMessage{ID: "a", RouteKey: "r1", Text: "x", EnqueuedAt: time.Now()})
	store.Append(agenttypes.QueuedMessage{ID: "b", RouteKey: "r1", Text: "y", EnqueuedAt: time.Now().Add(time.Millisecond)})

	if err := store.MarkConsumed([]string{"a"}); err != nil {
		t.Fatalf("mark consumed: %v", err)
	}
	pending, _ := store.LoadPendingByRoute("r1")
	if len(pending) != 1 || pending[0].ID != "b" {
		t.Errorf("after mark consumed, want only b, got %+v", pending)
	}
}

func TestMailboxStore_MarkConsumedEmpty(t *testing.T) {
	store, cleanup := openTestMailboxStore(t)
	defer cleanup()
	if err := store.MarkConsumed(nil); err != nil {
		t.Errorf("MarkConsumed(nil): %v", err)
	}
	if err := store.MarkConsumed([]string{}); err != nil {
		t.Errorf("MarkConsumed([]): %v", err)
	}
}

func TestMailboxStore_Delete(t *testing.T) {
	store, cleanup := openTestMailboxStore(t)
	defer cleanup()
	store.Append(agenttypes.QueuedMessage{ID: "a", RouteKey: "r1", EnqueuedAt: time.Now()})

	if err := store.Delete("a"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	pending, _ := store.LoadPendingByRoute("r1")
	if len(pending) != 0 {
		t.Errorf("after delete, want empty, got %+v", pending)
	}
}

func TestMailboxStore_LoadAllPendingGroupsByRoute(t *testing.T) {
	store, cleanup := openTestMailboxStore(t)
	defer cleanup()
	store.Append(agenttypes.QueuedMessage{ID: "a", RouteKey: "r1", EnqueuedAt: time.Now()})
	store.Append(agenttypes.QueuedMessage{ID: "b", RouteKey: "r2", EnqueuedAt: time.Now()})
	store.Append(agenttypes.QueuedMessage{ID: "c", RouteKey: "r1", EnqueuedAt: time.Now().Add(time.Millisecond)})

	all, err := store.LoadAllPending()
	if err != nil {
		t.Fatalf("load all: %v", err)
	}
	if len(all["r1"]) != 2 || len(all["r2"]) != 1 {
		t.Errorf("LoadAllPending shape: r1=%d r2=%d", len(all["r1"]), len(all["r2"]))
	}
	if all["r1"][0].ID != "a" || all["r1"][1].ID != "c" {
		t.Errorf("r1 ordering wrong: %+v", all["r1"])
	}
}

func TestMailboxStore_PurgeConsumedBefore(t *testing.T) {
	store, cleanup := openTestMailboxStore(t)
	defer cleanup()
	store.Append(agenttypes.QueuedMessage{ID: "a", RouteKey: "r1", EnqueuedAt: time.Now()})
	store.MarkConsumed([]string{"a"})
	// Pretend "a" was consumed long ago — adjust DB directly for the test.
	if _, err := store.db.Exec(`UPDATE mailbox SET consumed_at = ? WHERE id = ?`, int64(0), "a"); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	n, err := store.PurgeConsumedBefore(time.Now().Add(-time.Hour))
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if n != 1 {
		t.Errorf("purge: want 1 deleted, got %d", n)
	}
}
