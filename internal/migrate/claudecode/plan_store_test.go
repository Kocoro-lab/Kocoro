package claudecode

import (
	"errors"
	"testing"
	"time"
)

func TestPlanStore_PutGetDelete(t *testing.T) {
	store := NewPlanStore()
	now := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	p := &Plan{ID: "mig-test", ExpiresAt: now.Add(time.Minute)}

	store.Put(p)
	got, err := store.Get(p.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got != p {
		t.Fatal("Get returned different plan pointer")
	}

	store.Delete(p.ID)
	if _, err := store.Get(p.ID); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("Get after delete err=%v, want ErrPlanNotFound", err)
	}
}

func TestPlanStore_ExpiresAndSweeps(t *testing.T) {
	store := NewPlanStore()
	now := time.Date(2026, 5, 14, 0, 0, 0, 0, time.UTC)
	store.now = func() time.Time { return now }
	store.Put(&Plan{ID: "old", ExpiresAt: now.Add(-time.Second)})
	store.Put(&Plan{ID: "fresh", ExpiresAt: now.Add(time.Minute)})

	if _, err := store.Get("old"); !errors.Is(err, ErrPlanExpired) {
		t.Fatalf("expired Get err=%v, want ErrPlanExpired", err)
	}
	if _, err := store.Get("old"); !errors.Is(err, ErrPlanNotFound) {
		t.Fatalf("expired plan should be deleted, err=%v", err)
	}

	store.Put(&Plan{ID: "old2", ExpiresAt: now.Add(-time.Second)})
	if n := store.SweepExpired(); n != 1 {
		t.Fatalf("SweepExpired = %d, want 1", n)
	}
	if _, err := store.Get("fresh"); err != nil {
		t.Fatalf("fresh plan should remain: %v", err)
	}
}
