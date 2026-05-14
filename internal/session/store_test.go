package session

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestStore_SaveLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()

	sess := &Session{
		ID:    "test-123",
		Title: "Test session",
		CWD:   "/tmp/test",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello")},
			{Role: "assistant", Content: client.NewTextContent("hi there")},
		},
	}

	if err := store.Save(sess); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := store.Load("test-123")
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if loaded.Title != "Test session" {
		t.Errorf("expected 'Test session', got %q", loaded.Title)
	}
	if len(loaded.Messages) != 2 {
		t.Errorf("expected 2 messages, got %d", len(loaded.Messages))
	}
}

func TestStore_RoundTripToolResultReplacements(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()

	now := time.Now()
	sess := &Session{
		ID:        "sess-repl",
		CreatedAt: now,
		UpdatedAt: now,
		Title:     "replacement state",
		ToolResultReplacements: map[string]string{
			"toolu_1": "[Tool result omitted from context: /tmp/x]",
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("sess-repl")
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolResultReplacements["toolu_1"] != "[Tool result omitted from context: /tmp/x]" {
		t.Fatalf("replacement state not persisted: %#v", got.ToolResultReplacements)
	}
}

func TestStore_RoundTripToolResultSeen(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()

	now := time.Now()
	sess := &Session{
		ID:        "sess-seen",
		CreatedAt: now,
		UpdatedAt: now,
		Title:     "seen state",
		ToolResultSeen: map[string]bool{
			"toolu_seen": true,
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load("sess-seen")
	if err != nil {
		t.Fatal(err)
	}
	if !got.ToolResultSeen["toolu_seen"] {
		t.Fatalf("seen state not persisted: %#v", got.ToolResultSeen)
	}
}

func TestStore_List(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()

	store.Save(&Session{ID: "aaa", Title: "First"})
	store.Save(&Session{ID: "bbb", Title: "Second"})

	sessions, err := store.List()
	if err != nil {
		t.Fatalf("list failed: %v", err)
	}
	if len(sessions) != 2 {
		t.Errorf("expected 2 sessions, got %d", len(sessions))
	}
}

func TestStore_Delete(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()

	store.Save(&Session{ID: "del-me", Title: "Delete me"})

	if err := store.Delete("del-me"); err != nil {
		t.Fatalf("delete failed: %v", err)
	}

	if _, err := store.Load("del-me"); err == nil {
		t.Error("expected error loading deleted session")
	}

	// Verify file is gone
	path := filepath.Join(dir, "del-me.json")
	if fileExists(path) {
		t.Error("session file should be deleted")
	}
}

func TestStore_SaveLoadWithImageContent(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()

	sess := &Session{
		ID:    "vision-test",
		Title: "Vision test",
		CWD:   "/tmp",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("take a screenshot")},
			{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "text", Text: "Screenshot captured"},
				{Type: "image", Source: &client.ImageSource{
					Type:      "base64",
					MediaType: "image/png",
					Data:      "iVBORfake",
				}},
			})},
			{Role: "assistant", Content: client.NewTextContent("I see a desktop")},
		},
	}

	if err := store.Save(sess); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := store.Load("vision-test")
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}

	if len(loaded.Messages) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(loaded.Messages))
	}

	// First message: plain string
	if loaded.Messages[0].Content.Text() != "take a screenshot" {
		t.Errorf("msg[0] text mismatch: %q", loaded.Messages[0].Content.Text())
	}

	// Second message: content blocks with image
	if !loaded.Messages[1].Content.HasBlocks() {
		t.Fatal("msg[1] should have blocks")
	}
	blocks := loaded.Messages[1].Content.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks, got %d", len(blocks))
	}
	if blocks[1].Source == nil || blocks[1].Source.Data != "iVBORfake" {
		t.Error("image block data not preserved")
	}

	// Third message: plain string
	if loaded.Messages[2].Content.Text() != "I see a desktop" {
		t.Errorf("msg[2] text mismatch: %q", loaded.Messages[2].Content.Text())
	}
}

func TestStore_SaveLoadWithUsageSummary(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()

	sess := &Session{
		ID:    "usage-test",
		Title: "Usage test",
		CWD:   "/tmp",
		Usage: &UsageSummary{
			LLMCalls:              3,
			InputTokens:           150,
			OutputTokens:          45,
			TotalTokens:           195,
			CostUSD:               0.67,
			CacheReadTokens:       80,
			CacheCreationTokens:   300,
			CacheCreation5mTokens: 100,
			CacheCreation1hTokens: 200,
			Model:                 "claude-test",
		},
	}

	if err := store.Save(sess); err != nil {
		t.Fatalf("save failed: %v", err)
	}

	loaded, err := store.Load("usage-test")
	if err != nil {
		t.Fatalf("load failed: %v", err)
	}
	if loaded.Usage == nil {
		t.Fatal("expected usage summary to be loaded")
	}
	if loaded.Usage.CacheCreationTokens != 300 {
		t.Fatalf("expected legacy cache creation total 300, got %d", loaded.Usage.CacheCreationTokens)
	}
	if loaded.Usage.CacheCreation5mTokens != 100 || loaded.Usage.CacheCreation1hTokens != 200 {
		t.Fatalf("expected split cache creation 100/200, got %d/%d", loaded.Usage.CacheCreation5mTokens, loaded.Usage.CacheCreation1hTokens)
	}
}

func TestStore_SearchIntegration(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir) // auto-creates index + rebuilds (nothing to rebuild)
	defer store.Close()

	sess := &Session{
		ID:    "int-test",
		Title: "Integration",
		CWD:   "/tmp",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("deploy the kubernetes cluster")},
			{Role: "assistant", Content: client.NewTextContent("I'll help you deploy k8s")},
		},
	}
	store.Save(sess)

	// Search should find it
	results, err := store.Search("kubernetes", 10)
	if err != nil {
		t.Fatalf("Search: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result, got %d", len(results))
	}

	// List should use index (fast path)
	summaries, err := store.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected 1 session, got %d", len(summaries))
	}

	// Delete should clean up index
	store.Delete("int-test")
	results, _ = store.Search("kubernetes", 10)
	if len(results) != 0 {
		t.Errorf("expected 0 results after delete, got %d", len(results))
	}
}

func TestStore_GracefulDegradation(t *testing.T) {
	dir := t.TempDir()
	store := &Store{dir: dir, index: nil} // simulate index failure

	sess := &Session{ID: "no-idx", Title: "No index", CWD: "/tmp"}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save should work without index: %v", err)
	}

	summaries, err := store.List()
	if err != nil {
		t.Fatalf("List should fall back to JSON scan: %v", err)
	}
	if len(summaries) != 1 {
		t.Errorf("expected 1 session from JSON fallback, got %d", len(summaries))
	}

	_, err = store.Search("anything", 10)
	if err == nil {
		t.Error("Search should return error when index is nil")
	}
}

func TestStore_ListEmptyDir(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()

	summaries, err := store.List()
	if err != nil {
		t.Fatalf("List on empty dir: %v", err)
	}
	if len(summaries) != 0 {
		t.Errorf("expected 0 sessions, got %d", len(summaries))
	}
}

func TestStore_FirstLaunchMigration(t *testing.T) {
	dir := t.TempDir()

	// Write JSON files WITHOUT an index (simulate pre-SQLite sessions)
	rawStore := &Store{dir: dir, index: nil}
	rawStore.Save(&Session{
		ID:    "legacy-1",
		Title: "Legacy session",
		CWD:   "/tmp",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("legacy migration test")},
		},
	})

	// Now create store normally — should detect empty index and rebuild
	store := NewStore(dir)
	defer store.Close()

	// Should be searchable after migration
	results, err := store.Search("legacy", 10)
	if err != nil {
		t.Fatalf("Search after migration: %v", err)
	}
	if len(results) != 1 {
		t.Fatalf("expected 1 result after migration, got %d", len(results))
	}
}

func TestSessionMessageMetaSerialization(t *testing.T) {
	sess := &Session{
		ID:        "test-meta",
		CreatedAt: time.Now(),
		UpdatedAt: time.Now(),
		Title:     "Test",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello")},
			{Role: "assistant", Content: client.NewTextContent("hi")},
		},
		MessageMeta: []MessageMeta{
			{Source: "slack"},
			{Source: "slack"},
		},
	}

	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}

	var loaded Session
	if err := json.Unmarshal(data, &loaded); err != nil {
		t.Fatal(err)
	}

	if len(loaded.MessageMeta) != 2 {
		t.Fatalf("expected 2 meta entries, got %d", len(loaded.MessageMeta))
	}
	if loaded.MessageMeta[0].Source != "slack" {
		t.Fatalf("expected source 'slack', got %q", loaded.MessageMeta[0].Source)
	}
}

func TestSessionMessageMetaBackwardCompat(t *testing.T) {
	// Old session JSON without message_meta should deserialize cleanly
	oldJSON := `{"id":"old","created_at":"2026-01-01T00:00:00Z","updated_at":"2026-01-01T00:00:00Z","title":"Old","messages":[]}`

	var sess Session
	if err := json.Unmarshal([]byte(oldJSON), &sess); err != nil {
		t.Fatal(err)
	}
	if sess.MessageMeta != nil {
		t.Fatal("expected nil MessageMeta for old session")
	}
}

func TestSessionSourceAtSafety(t *testing.T) {
	sess := &Session{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello")},
		},
		// No MessageMeta — simulates legacy session
	}

	// Negative index should not panic
	if sess.SourceAt(-1) != "unknown" {
		t.Fatal("expected 'unknown' for negative index")
	}

	// Out of bounds should not panic
	if sess.SourceAt(999) != "unknown" {
		t.Fatal("expected 'unknown' for out-of-bounds index")
	}

	// Valid index but no meta
	if sess.SourceAt(0) != "unknown" {
		t.Fatal("expected 'unknown' for missing meta")
	}

	// With meta
	sess.MessageMeta = []MessageMeta{{Source: "slack"}}
	if sess.SourceAt(0) != "slack" {
		t.Fatalf("expected 'slack', got %q", sess.SourceAt(0))
	}
}

func TestStore_LoadLegacyStringContent(t *testing.T) {
	dir := t.TempDir()
	legacyJSON := `{
		"id": "legacy-test",
		"title": "Legacy",
		"cwd": "/tmp",
		"messages": [
			{"role": "user", "content": "hello"},
			{"role": "assistant", "content": "hi there"}
		]
	}`
	os.WriteFile(filepath.Join(dir, "legacy-test.json"), []byte(legacyJSON), 0600)

	store := NewStore(dir)
	defer store.Close()
	loaded, err := store.Load("legacy-test")
	if err != nil {
		t.Fatalf("load legacy failed: %v", err)
	}
	if loaded.Messages[0].Content.Text() != "hello" {
		t.Errorf("expected 'hello', got %q", loaded.Messages[0].Content.Text())
	}
	if loaded.Messages[1].Content.Text() != "hi there" {
		t.Errorf("expected 'hi there', got %q", loaded.Messages[1].Content.Text())
	}
}

func TestHistoryForLoop_FiltersInjected(t *testing.T) {
	sess := &Session{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("real user 1")},
			{Role: "assistant", Content: client.NewTextContent("real assistant 1")},
			{Role: "user", Content: client.NewTextContent("STOP. You wrote out tool calls as text…")},
			{Role: "assistant", Content: client.NewTextContent("sorry, trying again")},
			{Role: "user", Content: client.NewTextContent("real user 2")},
		},
		MessageMeta: []MessageMeta{
			{Source: "local"},
			{Source: "local"},
			{Source: "local", SystemInjected: true},
			{Source: "local"},
			{Source: "local"},
		},
	}
	got := sess.HistoryForLoop()
	if len(got) != 4 {
		t.Fatalf("got %d messages, want 4 (injected nudge must be filtered)", len(got))
	}
	for _, m := range got {
		if m.Content.Text() == "STOP. You wrote out tool calls as text…" {
			t.Errorf("injected nudge leaked into HistoryForLoop output: %q", m.Content.Text())
		}
	}
	// Real user/assistant messages must survive intact in order.
	want := []string{"real user 1", "real assistant 1", "sorry, trying again", "real user 2"}
	for i, m := range got {
		if m.Content.Text() != want[i] {
			t.Errorf("msg[%d] = %q, want %q", i, m.Content.Text(), want[i])
		}
	}
}

func TestHistoryForLoop_NoMeta(t *testing.T) {
	// Legacy sessions with no meta must pass through unchanged.
	sess := &Session{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hi")},
			{Role: "assistant", Content: client.NewTextContent("hello")},
		},
	}
	got := sess.HistoryForLoop()
	if len(got) != 2 {
		t.Errorf("got %d messages, want 2", len(got))
	}
}

func TestHistoryForLoop_ShortMeta(t *testing.T) {
	// Meta shorter than Messages (partial legacy migration): keep unannotated tail.
	sess := &Session{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("legacy1")},
			{Role: "user", Content: client.NewTextContent("legacy2")},
			{Role: "user", Content: client.NewTextContent("new nudge")},
		},
		MessageMeta: []MessageMeta{
			{}, // legacy1 — no flag
			// legacy2 and new nudge have no meta entries at all
		},
	}
	got := sess.HistoryForLoop()
	if len(got) != 3 {
		t.Errorf("got %d messages, want 3 (unannotated positions must survive)", len(got))
	}
}

func TestFilterInjected_NoFlagsFastPath(t *testing.T) {
	// When nothing is flagged, FilterInjected takes the fast path: it aliases
	// the input slice (no allocation) but caps the capacity so a later append
	// on the result cannot silently extend into the caller's backing array
	// past the visible length.
	backing := make([]client.Message, 2, 10) // extra capacity on purpose
	backing[0] = client.Message{Role: "user", Content: client.NewTextContent("a")}
	backing[1] = client.Message{Role: "assistant", Content: client.NewTextContent("b")}
	meta := []MessageMeta{{Source: "x"}, {Source: "x"}}

	got := FilterInjected(backing, meta)
	if len(got) != 2 {
		t.Fatalf("got len %d, want 2", len(got))
	}
	if &got[0] != &backing[0] {
		t.Error("expected fast path to alias the original slice (no allocation)")
	}
	if cap(got) != len(got) {
		t.Errorf("expected capped capacity %d, got %d — append on result could corrupt caller's backing array", len(got), cap(got))
	}
}

func TestFilterInjected_NoMetaFastPath(t *testing.T) {
	// The len(meta) == 0 branch must also cap capacity, same reasoning.
	backing := make([]client.Message, 1, 5)
	backing[0] = client.Message{Role: "user", Content: client.NewTextContent("a")}
	got := FilterInjected(backing, nil)
	if cap(got) != len(got) {
		t.Errorf("expected capped capacity %d, got %d", len(got), cap(got))
	}
}

// TestSession_LastSeenModel guards the accessor used by the daemon
// runner / TUI to carry an auto-detected context window across new
// AgentLoop instances. Each routed daemon turn builds a fresh loop;
// without this method (and a stable Usage.Model in session.json) the
// seed would always fall back to the static config and over-truncate
// 1M-context sessions until the first response arrived.
func TestSession_LastSeenModel(t *testing.T) {
	t.Run("nil session returns empty", func(t *testing.T) {
		var s *Session
		if got := s.LastSeenModel(); got != "" {
			t.Fatalf("nil session: got %q, want \"\"", got)
		}
	})

	t.Run("session without usage returns empty", func(t *testing.T) {
		s := &Session{ID: "x"}
		if got := s.LastSeenModel(); got != "" {
			t.Fatalf("no usage: got %q, want \"\"", got)
		}
	})

	t.Run("session with usage but no model returns empty", func(t *testing.T) {
		s := &Session{ID: "x", Usage: &UsageSummary{InputTokens: 100}}
		if got := s.LastSeenModel(); got != "" {
			t.Fatalf("usage without model: got %q, want \"\"", got)
		}
	})

	t.Run("session with model returns it", func(t *testing.T) {
		s := &Session{ID: "x", Usage: &UsageSummary{Model: "claude-sonnet-4-6"}}
		if got := s.LastSeenModel(); got != "claude-sonnet-4-6" {
			t.Fatalf("got %q, want claude-sonnet-4-6", got)
		}
	})
}

// TestStore_RoundTripInterleavedThinkingBlocks confirms that assistant
// messages with interleaved thinking blocks survive a Save → Load cycle
// with text + signatures intact. The Session struct already serializes
// client.Message via ContentBlock JSON tags; this test pins that contract.
func TestStore_RoundTripInterleavedThinkingBlocks(t *testing.T) {
	store := NewStore(t.TempDir())
	sess := &Session{
		ID: "test-thinking",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hi")},
			{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "thinking", Thinking: "first reasoning", Signature: "sigA"},
				{Type: "text", Text: "visible reply"},
				{Type: "tool_use", ID: "t1", Name: "file_read", Input: json.RawMessage(`{"path":"/x"}`)},
				{Type: "thinking", Thinking: "interleaved reasoning", Signature: "sigB"},
				{Type: "redacted_thinking", Data: "opaque-blob"},
			})},
		},
	}
	if err := store.Save(sess); err != nil {
		t.Fatalf("Save: %v", err)
	}
	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Messages) != 2 {
		t.Fatalf("Messages count = %d, want 2", len(loaded.Messages))
	}
	asst := loaded.Messages[1]
	if asst.Role != "assistant" {
		t.Fatalf("Role = %q, want assistant", asst.Role)
	}
	blocks := asst.Content.Blocks()
	wantTypes := []string{"thinking", "text", "tool_use", "thinking", "redacted_thinking"}
	if len(blocks) != len(wantTypes) {
		t.Fatalf("blocks count = %d, want %d", len(blocks), len(wantTypes))
	}
	for i, want := range wantTypes {
		if blocks[i].Type != want {
			t.Errorf("blocks[%d].Type = %q, want %q (order must be preserved)", i, blocks[i].Type, want)
		}
	}
	if blocks[0].Signature != "sigA" || blocks[3].Signature != "sigB" {
		t.Errorf("thinking signatures lost on reload: [0].sig=%q [3].sig=%q", blocks[0].Signature, blocks[3].Signature)
	}
	if blocks[4].Data != "opaque-blob" {
		t.Errorf("redacted_thinking data lost on reload: %q", blocks[4].Data)
	}
	if blocks[0].Thinking != "first reasoning" {
		t.Errorf("thinking text lost: %q", blocks[0].Thinking)
	}
}
