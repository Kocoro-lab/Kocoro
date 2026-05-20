package agenttypes

import (
	"testing"
	"time"
)

func TestMailbox_EnqueueDequeueFIFO(t *testing.T) {
	mb := NewMailbox(100)
	t0 := time.Now()
	if ok, err := mb.Enqueue(QueuedMessage{ID: "a", Text: "first", Priority: PriorityNext, EnqueuedAt: t0}); !ok || err != nil {
		t.Fatalf("first enqueue: ok=%v err=%v", ok, err)
	}
	if ok, err := mb.Enqueue(QueuedMessage{ID: "b", Text: "second", Priority: PriorityNext, EnqueuedAt: t0.Add(time.Millisecond)}); !ok || err != nil {
		t.Fatalf("second enqueue: ok=%v err=%v", ok, err)
	}

	batch := mb.DequeueBatch(10)
	if len(batch) != 2 {
		t.Fatalf("want 2, got %d", len(batch))
	}
	if batch[0].ID != "a" || batch[1].ID != "b" {
		t.Errorf("FIFO violated: got %s, %s", batch[0].ID, batch[1].ID)
	}
}

func TestMailbox_EnqueueRespectsCapacity(t *testing.T) {
	mb := NewMailbox(2)
	if ok, _ := mb.Enqueue(QueuedMessage{ID: "a"}); !ok {
		t.Fatal("first enqueue should succeed")
	}
	if ok, _ := mb.Enqueue(QueuedMessage{ID: "b"}); !ok {
		t.Fatal("second enqueue should succeed")
	}
	ok, err := mb.Enqueue(QueuedMessage{ID: "c"})
	if ok {
		t.Error("third enqueue should fail (cap=2)")
	}
	if err != ErrMailboxFull {
		t.Errorf("expected ErrMailboxFull, got %v", err)
	}
	// mailbox should still hold the first two
	if mb.Len() != 2 {
		t.Errorf("mailbox mutated despite cap error: len=%d", mb.Len())
	}
}

func TestMailbox_PriorityOrder(t *testing.T) {
	mb := NewMailbox(100)
	mb.Enqueue(QueuedMessage{ID: "later", Priority: PriorityLater})
	mb.Enqueue(QueuedMessage{ID: "next", Priority: PriorityNext})
	mb.Enqueue(QueuedMessage{ID: "now", Priority: PriorityNow})

	batch := mb.DequeueBatch(10)
	want := []string{"now", "next", "later"}
	for i, m := range batch {
		if m.ID != want[i] {
			t.Errorf("position %d: want %s, got %s", i, want[i], m.ID)
		}
	}
}

func TestMailbox_Retract(t *testing.T) {
	mb := NewMailbox(100)
	mb.Enqueue(QueuedMessage{ID: "a", Text: "keep"})
	mb.Enqueue(QueuedMessage{ID: "b", Text: "retract"})

	if ok := mb.Retract("b"); !ok {
		t.Fatal("retract returned false for existing id")
	}
	batch := mb.DequeueBatch(10)
	if len(batch) != 1 || batch[0].ID != "a" {
		t.Errorf("retract failed: %+v", batch)
	}
}

func TestMailbox_RetractMissing(t *testing.T) {
	mb := NewMailbox(100)
	if ok := mb.Retract("does-not-exist"); ok {
		t.Error("retract on missing id should return false")
	}
}

func TestMailbox_Snapshot(t *testing.T) {
	mb := NewMailbox(100)
	mb.Enqueue(QueuedMessage{ID: "a", Text: "one"})
	mb.Enqueue(QueuedMessage{ID: "b", Text: "two"})

	snap := mb.Snapshot()
	if len(snap) != 2 {
		t.Fatalf("snapshot size: want 2, got %d", len(snap))
	}
	snap[0].Text = "mutated"
	again := mb.Snapshot()
	if again[0].Text != "one" {
		t.Error("snapshot is not a defensive copy")
	}
}

func TestMailbox_DequeueBatchEmpty(t *testing.T) {
	mb := NewMailbox(100)
	if got := mb.DequeueBatch(10); len(got) != 0 {
		t.Errorf("empty dequeue: want nil/empty, got %+v", got)
	}
}

func TestMailbox_SeedFromStore(t *testing.T) {
	mb := NewMailbox(100)
	mb.Enqueue(QueuedMessage{ID: "live"})
	loaded, dropped := mb.SeedFromStore([]QueuedMessage{
		{ID: "s1"},
		{ID: "s2"},
	})
	if loaded != 2 || dropped != 0 {
		t.Errorf("seed: loaded=%d dropped=%d", loaded, dropped)
	}
	batch := mb.DequeueBatch(10)
	if len(batch) != 3 || batch[0].ID != "s1" || batch[1].ID != "s2" || batch[2].ID != "live" {
		t.Errorf("seed order wrong: %+v", batch)
	}
}

func TestMailbox_SeedFromStoreRespectsCapacity(t *testing.T) {
	mb := NewMailbox(2)
	loaded, dropped := mb.SeedFromStore([]QueuedMessage{
		{ID: "s1"}, {ID: "s2"}, {ID: "s3"},
	})
	if loaded != 2 || dropped != 1 {
		t.Errorf("seed over-cap: loaded=%d dropped=%d", loaded, dropped)
	}
}
