package session

import (
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestManager_ResumeLatest_EmptyDir(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	sess, err := m.ResumeLatest()
	if err != nil {
		t.Fatalf("unexpected error on empty dir: %v", err)
	}
	if sess != nil {
		t.Error("expected nil session for empty directory")
	}
}

func TestManager_ResumeLatest_FindsMostRecentByUpdatedAt(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()

	// Create "older-created" session first, then update it later
	// Create "newer-created" session second, but don't update it
	// ResumeLatest should pick "older-created" because it was updated more recently.

	olderCreated := &Session{
		ID:        "older-created",
		Title:     "Created first",
		CreatedAt: time.Now().Add(-2 * time.Hour),
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("first message")},
		},
	}
	store.Save(olderCreated) // UpdatedAt = now

	// Simulate passage of time
	time.Sleep(10 * time.Millisecond)

	newerCreated := &Session{
		ID:        "newer-created",
		Title:     "Created second",
		CreatedAt: time.Now(),
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("second message")},
		},
	}
	store.Save(newerCreated) // UpdatedAt = now (slightly later)

	// Now update the older-created session (simulating daemon appending a turn)
	time.Sleep(10 * time.Millisecond)
	olderCreated.Messages = append(olderCreated.Messages,
		client.Message{Role: "assistant", Content: client.NewTextContent("reply")},
	)
	store.Save(olderCreated) // UpdatedAt = now (latest)

	m := NewManager(dir)
	defer m.Close()
	sess, err := m.ResumeLatest()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess == nil {
		t.Fatal("expected a session, got nil")
	}
	// Should pick "older-created" because it has the latest UpdatedAt
	if sess.ID != "older-created" {
		t.Errorf("expected 'older-created' (most recently updated), got %q", sess.ID)
	}
	if len(sess.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(sess.Messages))
	}
	if m.Current() == nil || m.Current().ID != "older-created" {
		t.Error("ResumeLatest should set the session as current")
	}
}

func TestManager_ResumeLatest_SingleSession(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()

	store.Save(&Session{
		ID:    "only-one",
		Title: "Only session",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello")},
		},
	})

	m := NewManager(dir)
	defer m.Close()
	sess, err := m.ResumeLatest()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if sess.ID != "only-one" {
		t.Errorf("expected 'only-one', got %q", sess.ID)
	}
}

func TestManager_OnSessionClose_FiresOnSessionSwitch(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	s1 := m.NewSession()
	calls := 0
	m.OnSessionClose(s1.ID, func() { calls++ })

	s2 := m.NewSession()
	if s2 == nil {
		t.Fatal("expected second session")
	}
	if calls != 1 {
		t.Fatalf("expected callback to fire once when switching sessions, got %d", calls)
	}
}

func TestManager_OnSessionClose_AppendsCallbacks(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)

	sess := m.NewSession()
	total := 0
	// Multiple subsystems (spill cleanup, file-preview teardown, etc.)
	// each register their own close hook. All must fire — replace
	// semantics would silently leak resources.
	m.OnSessionClose(sess.ID, func() { total += 1 })
	m.OnSessionClose(sess.ID, func() { total += 10 })

	if err := m.Close(); err != nil {
		t.Fatalf("close failed: %v", err)
	}
	if total != 11 {
		t.Fatalf("expected both callbacks to fire (append semantics), got total %d", total)
	}
}

func TestManager_OnSessionClose_AppendFiresOnSessionSwitch(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	sess := m.NewSession()
	total := 0
	m.OnSessionClose(sess.ID, func() { total += 1 })
	m.OnSessionClose(sess.ID, func() { total += 100 })

	// Switching sessions fires the close hooks for the previous session.
	_ = m.NewSession()
	if total != 101 {
		t.Fatalf("append-on-close after switch: want 101, got %d", total)
	}
}

func TestManager_WorkingSet_IsScopedPerSession(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	s1 := m.NewSession()
	if err := m.Save(); err != nil {
		t.Fatalf("save first session: %v", err)
	}
	ws1 := m.WorkingSet(s1.ID)
	if ws1 == nil {
		t.Fatal("expected working set for first session")
	}
	ws1.Add("browser_click", client.Tool{Type: "function", Function: client.FunctionDef{Name: "browser_click"}})

	s2 := m.NewSession()
	if err := m.Save(); err != nil {
		t.Fatalf("save second session: %v", err)
	}
	ws2 := m.WorkingSet(s2.ID)
	if ws2 == nil {
		t.Fatal("expected working set for second session")
	}
	if ws2.Contains("browser_click") {
		t.Fatal("second session should not inherit first session's warmed tools")
	}

	if _, err := m.Resume(s1.ID); err != nil {
		t.Fatalf("resume first session: %v", err)
	}
	ws1Again := m.CurrentWorkingSet()
	if ws1Again == nil {
		t.Fatal("expected working set after resuming first session")
	}
	if !ws1Again.Contains("browser_click") {
		t.Fatal("resumed first session should retain its working set")
	}
}
