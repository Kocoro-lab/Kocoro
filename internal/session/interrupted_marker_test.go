package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func writeLegacySessionWithoutMarker(t *testing.T, dir string, sess *Session) {
	t.Helper()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sess.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestInterruptedSessionsMigratesMarkersAndRemovesOnCompletion(t *testing.T) {
	dir := t.TempDir()
	id := "legacy-interrupted-001"
	now := time.Now()
	writeLegacySessionWithoutMarker(t, dir, &Session{
		SchemaVersion: 1,
		ID:            id,
		CreatedAt:     now,
		UpdatedAt:     now,
		Title:         "Interrupted",
		InProgress:    true,
		InterruptedTurn: &InterruptedTurn{
			Source:    "desktop",
			UpdatedAt: now,
		},
	})
	writeLegacySessionWithoutMarker(t, dir, &Session{
		SchemaVersion: 1,
		ID:            "legacy-complete-001",
		CreatedAt:     now,
		UpdatedAt:     now,
		Title:         "Complete",
	})

	mgr := NewManager(dir)
	defer mgr.Close()
	got, err := mgr.InterruptedSessions()
	if err != nil {
		t.Fatalf("InterruptedSessions: %v", err)
	}
	if len(got) != 1 || got[0].ID != id {
		t.Fatalf("interrupted sessions = %#v, want %s", got, id)
	}

	markerDir := filepath.Join(dir, interruptedMarkerDirName)
	if _, err := os.Stat(filepath.Join(markerDir, id+interruptedMarkerSuffix)); err != nil {
		t.Fatalf("migration marker missing: %v", err)
	}
	if _, err := os.Stat(filepath.Join(markerDir, interruptedMarkerScanStamp)); err != nil {
		t.Fatalf("migration scan stamp missing: %v", err)
	}

	sess, err := mgr.Resume(id)
	if err != nil {
		t.Fatal(err)
	}
	sess.InProgress = false
	sess.InterruptedTurn = nil
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(markerDir, id+interruptedMarkerSuffix)); !os.IsNotExist(err) {
		t.Fatalf("completed-session marker still exists: %v", err)
	}
	got, err = mgr.InterruptedSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("completed session still discovered: %#v", got)
	}
}

func TestInterruptedSessionsSelfHealsStaleMarker(t *testing.T) {
	dir := t.TempDir()
	mgr := NewManager(dir)
	defer mgr.Close()
	sess := mgr.NewSessionWithID("stale-marker-session-001")
	sess.InProgress = true
	sess.InterruptedTurn = &InterruptedTurn{UpdatedAt: time.Now()}
	if err := mgr.Save(); err != nil {
		t.Fatal(err)
	}

	// Simulate a crash after the completed JSON write but before the
	// best-effort marker removal.
	sess.InProgress = false
	sess.InterruptedTurn = nil
	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, sess.ID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}

	got, err := mgr.InterruptedSessions()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("stale marker scheduled a completed session: %#v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, interruptedMarkerDirName, sess.ID+interruptedMarkerSuffix)); !os.IsNotExist(err) {
		t.Fatalf("stale marker was not removed: %v", err)
	}
}

func TestInterruptedMarkerMigrationDoesNotLoseConcurrentSave(t *testing.T) {
	for iteration := 0; iteration < 20; iteration++ {
		dir := t.TempDir()
		store := NewStore(dir)
		sess := &Session{
			SchemaVersion: 1,
			ID:            "concurrent-save-001",
			CreatedAt:     time.Now(),
			UpdatedAt:     time.Now(),
			Title:         "Concurrent save",
		}
		writeLegacySessionWithoutMarker(t, dir, sess)

		start := make(chan struct{})
		var wg sync.WaitGroup
		wg.Add(2)
		var scanErr, saveErr error
		go func() {
			defer wg.Done()
			<-start
			_, scanErr = store.InterruptedSessions()
		}()
		go func() {
			defer wg.Done()
			<-start
			sess.InProgress = true
			sess.InterruptedTurn = &InterruptedTurn{UpdatedAt: time.Now()}
			saveErr = store.Save(sess)
		}()
		close(start)
		wg.Wait()
		if scanErr != nil || saveErr != nil {
			t.Fatalf("iteration %d: scan=%v save=%v", iteration, scanErr, saveErr)
		}

		got, err := store.InterruptedSessions()
		if err != nil {
			t.Fatalf("iteration %d: final scan: %v", iteration, err)
		}
		if len(got) != 1 || got[0].ID != sess.ID {
			t.Fatalf("iteration %d: interrupted session lost: %#v", iteration, got)
		}
		_ = store.Close()
	}
}
