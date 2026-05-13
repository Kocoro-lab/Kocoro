package session

import (
	"os"
	"path/filepath"
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

func TestManager_ResumeLatestByRouteKey_FindsLatestMatchingRoute(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()

	routeKey := "default:slack:C123-1710000000.000100"
	store.Save(&Session{
		ID:       "route-old",
		Title:    "Old route session",
		RouteKey: routeKey,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("first")},
		},
	})
	time.Sleep(10 * time.Millisecond)
	store.Save(&Session{
		ID:       "other-route",
		Title:    "Other route session",
		RouteKey: "default:slack:C999",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("other")},
		},
	})
	time.Sleep(10 * time.Millisecond)
	store.Save(&Session{
		ID:       "route-new",
		Title:    "New route session",
		RouteKey: routeKey,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("latest")},
		},
	})

	m := NewManager(dir)
	defer m.Close()
	sess, err := m.ResumeLatestByRouteKey(routeKey)
	if err != nil {
		t.Fatalf("ResumeLatestByRouteKey: %v", err)
	}
	if sess == nil {
		t.Fatal("expected a session, got nil")
	}
	if sess.ID != "route-new" {
		t.Fatalf("resumed %q, want route-new", sess.ID)
	}
	if current := m.Current(); current == nil || current.ID != "route-new" {
		t.Fatalf("current session = %#v, want route-new", current)
	}
}

func TestManager_ResetClearsRouteKey(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	sess := m.NewSession()
	sess.RouteKey = "default:slack:C123-1710000000.000100"
	sess.Messages = []client.Message{{Role: "user", Content: client.NewTextContent("hello")}}
	if err := m.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	if err := m.Reset(sess.ID); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	loaded, err := m.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.RouteKey != "" {
		t.Fatalf("RouteKey = %q, want empty", loaded.RouteKey)
	}
	resumed, err := m.ResumeLatestByRouteKey("default:slack:C123-1710000000.000100")
	if err != nil {
		t.Fatalf("ResumeLatestByRouteKey: %v", err)
	}
	if resumed != nil {
		t.Fatalf("expected no session for cleared route, got %q", resumed.ID)
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

// TestManager_Delete_FailedDeletePreservesState: when store.Delete fails
// (e.g. the file is already gone), the manager must NOT clear m.current
// or fire cleanup callbacks — those would tear down live per-session
// resources for a session the caller can still see.
func TestManager_Delete_FailedDeletePreservesState(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	sess := m.NewSession()
	if err := m.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	calls := 0
	m.OnSessionClose(sess.ID, func() { calls++ })

	// Pretend the session was already removed from disk by some external
	// path; store.Delete will return os.ErrNotExist via os.Remove.
	if err := os.Remove(filepath.Join(dir, sess.ID+".json")); err != nil {
		t.Fatalf("setup remove: %v", err)
	}

	if err := m.Delete(sess.ID); err == nil {
		t.Fatal("expected Delete to return error when disk file missing")
	}
	if calls != 0 {
		t.Errorf("close callbacks fired on failed delete; want 0, got %d", calls)
	}
	if cur := m.Current(); cur == nil || cur.ID != sess.ID {
		t.Error("m.current was cleared despite failed delete")
	}
}

// TestManager_OnSessionClose_FiresOnDelete verifies that explicitly
// deleting a session releases registered cleanup callbacks. Subsystems
// like ReadTrackerCache.Forget rely on this hook to release per-session
// memory; pre-fix, Delete only touched the runtime map and store.
func TestManager_OnSessionClose_FiresOnDelete(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	sess := m.NewSession()
	if err := m.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}
	calls := 0
	m.OnSessionClose(sess.ID, func() { calls++ })

	if err := m.Delete(sess.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if calls != 1 {
		t.Fatalf("expected close callback to fire once on Delete, got %d", calls)
	}
	// Second Delete is a no-op on callbacks (the entry was already taken).
	_ = m.Delete(sess.ID)
	if calls != 1 {
		t.Fatalf("Delete should be idempotent on callbacks, got %d", calls)
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

func TestManager_Reset_ClearsHistoryInPlace(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	// Seed a session with messages, meta, summary cache, and usage.
	sess := m.NewSession()
	origID := sess.ID
	sess.Title = "Kept title"
	sess.CWD = "/keep/here"
	sess.Source = "slack"
	sess.Channel = "C123"
	sess.Messages = []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("hi")},
	}
	sess.MessageMeta = []MessageMeta{{Source: "local"}, {Source: "local"}}
	sess.RemoteTasks = []string{"task-1"}
	sess.SummaryCache = "cached summary"
	sess.SummaryCacheKey = "key-1"
	sess.InProgress = true
	if err := m.Save(); err != nil {
		t.Fatalf("seed save failed: %v", err)
	}
	m.AddUsage(origID, UsageSummary{InputTokens: 100, CostUSD: 0.5})
	if err := m.Save(); err != nil {
		t.Fatalf("seed usage save failed: %v", err)
	}

	if err := m.Reset(origID); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	cur := m.Current()
	if cur == nil || cur.ID != origID {
		t.Fatalf("current session should still be %q, got %+v", origID, cur)
	}
	if cur.Title != "Kept title" {
		t.Errorf("Title should be preserved, got %q", cur.Title)
	}
	if cur.CWD != "/keep/here" {
		t.Errorf("CWD should be preserved, got %q", cur.CWD)
	}
	if cur.Source != "slack" || cur.Channel != "C123" {
		t.Errorf("Source/Channel should be preserved, got %q/%q", cur.Source, cur.Channel)
	}
	if cur.Usage == nil || cur.Usage.InputTokens != 100 {
		t.Errorf("Usage should be preserved, got %+v", cur.Usage)
	}
	if len(cur.Messages) != 0 {
		t.Errorf("Messages should be cleared, got %d", len(cur.Messages))
	}
	if len(cur.MessageMeta) != 0 {
		t.Errorf("MessageMeta should be cleared, got %d", len(cur.MessageMeta))
	}
	if len(cur.RemoteTasks) != 0 {
		t.Errorf("RemoteTasks should be cleared, got %d", len(cur.RemoteTasks))
	}
	if cur.SummaryCache != "" || cur.SummaryCacheKey != "" {
		t.Errorf("Summary cache should be cleared, got %q/%q", cur.SummaryCache, cur.SummaryCacheKey)
	}
	if cur.InProgress {
		t.Error("InProgress should be cleared")
	}

	// Reload from disk to confirm persistence.
	m2 := NewManager(dir)
	defer m2.Close()
	loaded, err := m2.Load(origID)
	if err != nil {
		t.Fatalf("reload failed: %v", err)
	}
	if len(loaded.Messages) != 0 {
		t.Errorf("persisted messages should be cleared, got %d", len(loaded.Messages))
	}
	if loaded.Title != "Kept title" {
		t.Errorf("persisted title should be preserved, got %q", loaded.Title)
	}
	if loaded.Usage == nil || loaded.Usage.InputTokens != 100 {
		t.Errorf("persisted usage should be preserved, got %+v", loaded.Usage)
	}
}

func TestManager_Reset_NotFound(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	err := m.Reset("does-not-exist")
	if err == nil {
		t.Fatal("expected error for missing session")
	}
}

func TestManager_Reset_EmptyID(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	if err := m.Reset(""); err == nil {
		t.Fatal("expected error for empty id")
	}
}

func TestManager_Reset_ResetsWorkingSet(t *testing.T) {
	dir := t.TempDir()
	m := NewManager(dir)
	defer m.Close()

	sess := m.NewSession()
	if err := m.Save(); err != nil {
		t.Fatalf("save failed: %v", err)
	}
	ws := m.WorkingSet(sess.ID)
	ws.Add("browser_click", client.Tool{Name: "browser_click"})
	if !ws.Contains("browser_click") {
		t.Fatal("seed: working set should contain browser_click")
	}

	if err := m.Reset(sess.ID); err != nil {
		t.Fatalf("Reset failed: %v", err)
	}

	wsAfter := m.WorkingSet(sess.ID)
	if wsAfter == nil {
		t.Fatal("working set should exist after reset")
	}
	if wsAfter.Contains("browser_click") {
		t.Error("working set should be cleared after reset")
	}
}

