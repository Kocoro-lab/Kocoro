package daemon

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
	_ "modernc.org/sqlite"
)

func newCacheWithMailbox(t *testing.T, capacity int) (*SessionCache, *MailboxStore, func()) {
	t.Helper()
	dir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(dir, "mailbox.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	db.SetMaxOpenConns(1)
	store, err := NewMailboxStore(db)
	if err != nil {
		db.Close()
		t.Fatalf("new store: %v", err)
	}
	sc := NewSessionCacheWithMailbox(dir, store, capacity)
	return sc, store, func() {
		db.Close()
	}
}

func newQMsg(id string) agenttypes.QueuedMessage {
	return agenttypes.QueuedMessage{
		ID:         id,
		Source:     "ws",
		Text:       "msg " + id,
		Priority:   agenttypes.PriorityNext,
		EnqueuedAt: time.Now(),
	}
}

func TestEnqueueMessage_HappyPath(t *testing.T) {
	sc, store, cleanup := newCacheWithMailbox(t, 100)
	defer cleanup()

	out, err := sc.EnqueueMessage("r1", newQMsg("a"))
	if err != nil {
		t.Fatalf("enqueue err: %v", err)
	}
	if out != MailboxQueued {
		t.Errorf("outcome: want MailboxQueued, got %s", out)
	}
	if !out.ShouldAck() {
		t.Error("MailboxQueued should ack")
	}
	if got := sc.MailboxLen("r1"); got != 1 {
		t.Errorf("mailbox len: want 1, got %d", got)
	}
	pending, _ := store.LoadPendingByRoute("r1")
	if len(pending) != 1 {
		t.Errorf("store len: want 1, got %d", len(pending))
	}
}

func TestEnqueueMessage_DedupViaCloudMsgID(t *testing.T) {
	sc, _, cleanup := newCacheWithMailbox(t, 100)
	defer cleanup()

	m := newQMsg("a")
	m.CloudMsgID = "cloud-x"
	if out, _ := sc.EnqueueMessage("r1", m); out != MailboxQueued {
		t.Fatalf("first enqueue: %s", out)
	}

	dup := newQMsg("b")
	dup.CloudMsgID = "cloud-x"
	out, _ := sc.EnqueueMessage("r1", dup)
	if out != MailboxDeduped {
		t.Errorf("dup outcome: want Deduped, got %s", out)
	}
	if !out.ShouldAck() {
		t.Error("Deduped must still ShouldAck (Cloud safe to drop replay row)")
	}
	if got := sc.MailboxLen("r1"); got != 1 {
		t.Errorf("mailbox should remain at 1 after dedup, got %d", got)
	}
}

func TestEnqueueMessage_CapacityRollsBackStore(t *testing.T) {
	sc, store, cleanup := newCacheWithMailbox(t, 2)
	defer cleanup()

	if out, _ := sc.EnqueueMessage("r1", newQMsg("a")); out != MailboxQueued {
		t.Fatalf("a: %s", out)
	}
	if out, _ := sc.EnqueueMessage("r1", newQMsg("b")); out != MailboxQueued {
		t.Fatalf("b: %s", out)
	}
	out, _ := sc.EnqueueMessage("r1", newQMsg("c"))
	if out != MailboxQueueFull {
		t.Errorf("c outcome: want QueueFull, got %s", out)
	}
	if out.ShouldAck() {
		t.Error("QueueFull must NOT ack (Cloud should replay)")
	}
	pending, _ := store.LoadPendingByRoute("r1")
	if len(pending) != 2 {
		t.Errorf("store should be rolled back to 2 rows, got %d", len(pending))
	}
}

func TestEnqueueMessage_CWDMismatch(t *testing.T) {
	sc, _, cleanup := newCacheWithMailbox(t, 100)
	defer cleanup()

	// Manually mark the route as having an active CWD (simulates an active run).
	sc.mu.Lock()
	sc.routes["r1"] = &routeEntry{activeCWD: "/proj/A", lastAccess: time.Now()}
	sc.mu.Unlock()

	msg := newQMsg("a")
	msg.CWD = "/proj/B"
	out, err := sc.EnqueueMessage("r1", msg)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out != MailboxRouteMismatch {
		t.Errorf("CWD mismatch: want RouteMismatch, got %s", out)
	}
	if out.ShouldAck() {
		t.Error("RouteMismatch must NOT ack")
	}
}

func TestEnqueueMessage_PersistFailureDoesNotEnqueue(t *testing.T) {
	// Without a store, every Enqueue succeeds in memory. Simulate a store
	// failure by closing the DB and pointing the store at the closed handle.
	sc, _, cleanup := newCacheWithMailbox(t, 100)
	cleanup() // close DB

	out, err := sc.EnqueueMessage("r1", newQMsg("a"))
	if err == nil {
		t.Fatal("expected error on closed DB")
	}
	if out != MailboxPersistFailed {
		t.Errorf("closed DB: want PersistFailed, got %s", out)
	}
	if out.ShouldAck() {
		t.Error("PersistFailed must NOT ack")
	}
	if got := sc.MailboxLen("r1"); got != 0 {
		t.Errorf("mailbox must stay empty on persist failure, got %d", got)
	}
}

func TestDrainMailboxAndMarkConsumed(t *testing.T) {
	sc, store, cleanup := newCacheWithMailbox(t, 100)
	defer cleanup()

	sc.EnqueueMessage("r1", newQMsg("a"))
	sc.EnqueueMessage("r1", newQMsg("b"))

	batch := sc.DrainMailbox("r1", 10)
	if len(batch) != 2 {
		t.Fatalf("drain size: want 2, got %d", len(batch))
	}
	if err := sc.MarkMailboxConsumed([]string{batch[0].ID, batch[1].ID}); err != nil {
		t.Fatalf("mark: %v", err)
	}
	pending, _ := store.LoadPendingByRoute("r1")
	if len(pending) != 0 {
		t.Errorf("after mark, pending should be empty, got %d", len(pending))
	}
}

func TestSeedMailboxRestoresPending(t *testing.T) {
	sc, _, cleanup := newCacheWithMailbox(t, 100)
	defer cleanup()

	loaded, dropped := sc.SeedMailbox("r1", []agenttypes.QueuedMessage{
		newQMsg("recover-a"),
		newQMsg("recover-b"),
	})
	if loaded != 2 || dropped != 0 {
		t.Errorf("seed: loaded=%d dropped=%d", loaded, dropped)
	}
	batch := sc.DrainMailbox("r1", 10)
	if len(batch) != 2 || batch[0].ID != "recover-a" || batch[1].ID != "recover-b" {
		t.Errorf("seed order: %+v", batch)
	}
}

func TestMailboxRetractRemovesBothLayers(t *testing.T) {
	sc, store, cleanup := newCacheWithMailbox(t, 100)
	defer cleanup()

	sc.EnqueueMessage("r1", newQMsg("a"))
	sc.EnqueueMessage("r1", newQMsg("b"))

	if !sc.MailboxRetract("r1", "a") {
		t.Fatal("retract should succeed")
	}
	if got := sc.MailboxLen("r1"); got != 1 {
		t.Errorf("in-memory len after retract: %d", got)
	}
	pending, _ := store.LoadPendingByRoute("r1")
	if len(pending) != 1 || pending[0].ID != "b" {
		t.Errorf("store after retract: %+v", pending)
	}
}

func TestMailboxRetractMissingReturnsFalse(t *testing.T) {
	sc, _, cleanup := newCacheWithMailbox(t, 100)
	defer cleanup()
	if sc.MailboxRetract("r1", "nope") {
		t.Error("retract on missing id should return false")
	}
}
