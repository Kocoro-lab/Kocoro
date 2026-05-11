package context

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestPersistLearnings(t *testing.T) {
	messages := []client.Message{
		{Role: "system", Content: client.NewTextContent("system prompt")},
		{Role: "user", Content: client.NewTextContent("fix the auth bug")},
		{Role: "assistant", Content: client.NewTextContent("Found that tokens expire after 1 hour, not 24.")},
	}

	t.Run("appends learnings to MEMORY.md", func(t *testing.T) {
		dir := t.TempDir()
		mock := &mockCompleter{
			response: &client.CompletionResponse{
				OutputText: "- Auth tokens expire after 1 hour\n- User prefers direct fixes over explanations",
			},
		}

		_, err := PersistLearnings(context.Background(), mock, messages, dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, err := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
		if err != nil {
			t.Fatalf("MEMORY.md not created: %v", err)
		}

		content := string(data)
		if !strings.Contains(content, "Auth tokens expire") {
			t.Error("should contain persisted learning")
		}
		if !strings.Contains(content, "Auto-persisted") {
			t.Error("should contain auto-persisted header")
		}

		// Verify small model used
		if mock.lastReq.ModelTier != "small" {
			t.Errorf("should use small tier, got %q", mock.lastReq.ModelTier)
		}
	})

	t.Run("skips when LLM returns NONE", func(t *testing.T) {
		dir := t.TempDir()
		mock := &mockCompleter{
			response: &client.CompletionResponse{OutputText: "NONE"},
		}

		_, err := PersistLearnings(context.Background(), mock, messages, dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// MEMORY.md should not be created
		if _, err := os.Stat(filepath.Join(dir, "MEMORY.md")); err == nil {
			t.Error("MEMORY.md should not be created when nothing to persist")
		}
	})

	t.Run("skips when memoryDir is empty", func(t *testing.T) {
		mock := &mockCompleter{
			response: &client.CompletionResponse{OutputText: "- something"},
		}

		_, err := PersistLearnings(context.Background(), mock, messages, "")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// Should not even call the completer
		if mock.lastReq != nil {
			t.Error("should not make LLM call when memoryDir is empty")
		}
	})

	t.Run("includes existing memory to avoid duplicates", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- Existing fact"), 0644)

		mock := &mockCompleter{
			response: &client.CompletionResponse{OutputText: "- New fact only"},
		}

		_, err := PersistLearnings(context.Background(), mock, messages, dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// Verify existing memory was included in the prompt
		userMsg := mock.lastReq.Messages[1].Content.Text()
		if !strings.Contains(userMsg, "Existing fact") {
			t.Error("should include existing memory in prompt to avoid duplicates")
		}
	})

	t.Run("overflows to detail file when MEMORY.md is large", func(t *testing.T) {
		dir := t.TempDir()

		// Create a large MEMORY.md close to the limit
		var lines []string
		for i := 0; i < maxMemoryLines-1; i++ {
			lines = append(lines, "- existing line")
		}
		os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(strings.Join(lines, "\n")), 0644)

		mock := &mockCompleter{
			response: &client.CompletionResponse{
				OutputText: "- New learning 1\n- New learning 2\n- New learning 3",
			},
		}

		_, err := PersistLearnings(context.Background(), mock, messages, dir)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// MEMORY.md should have a pointer, not full content
		data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
		content := string(data)
		if !strings.Contains(content, "auto-") {
			t.Error("should contain pointer to detail file")
		}

		// A detail file should exist
		entries, _ := os.ReadDir(dir)
		found := false
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "auto-") && e.Name() != "MEMORY.md" {
				found = true
				detailData, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				if !strings.Contains(string(detailData), "New learning 1") {
					t.Error("detail file should contain the learnings")
				}
			}
		}
		if !found {
			t.Error("should have created a detail file")
		}
	})

	t.Run("returns error on LLM failure", func(t *testing.T) {
		dir := t.TempDir()
		mock := &mockCompleter{err: context.DeadlineExceeded}

		_, err := PersistLearnings(context.Background(), mock, messages, dir)
		if err == nil {
			t.Error("expected error when LLM fails")
		}
	})
}

func TestPersistLearningsReturnsUsage(t *testing.T) {
	dir := t.TempDir()
	mock := &mockCompleter{
		response: &client.CompletionResponse{
			OutputText: "- Durable fact",
			Model:      "claude-small",
		},
		usage: client.Usage{
			InputTokens:           80,
			OutputTokens:          20,
			CacheCreation5mTokens: 10,
			CacheCreation1hTokens: 15,
		},
	}

	u, err := PersistLearnings(context.Background(), mock, []client.Message{
		{Role: "user", Content: client.NewTextContent("remember this")},
	}, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.CacheCreation5mTokens != 10 || u.CacheCreation1hTokens != 15 {
		t.Fatalf("expected split cache creation 10/15, got %d/%d", u.CacheCreation5mTokens, u.CacheCreation1hTokens)
	}
}

// TestPersistLearnings_CapsOversizedTranscript verifies that the transcript
// fed to the small-tier summarizer is bounded by summarizeInputCapChars even
// when the source history is way over that. Without this, a long session
// crossing the 0.90 compaction threshold (180K tokens on a 200K cap) would
// 400 the small model with "prompt is too long" — observed as the
// "phase=proactive_persist_learnings" compaction_failed audit row during
// 2026-05-11 stress testing.
func TestPersistLearnings_CapsOversizedTranscript(t *testing.T) {
	// Build a transcript far over the small-tier 200K cap, same shape as
	// TestGenerateSummary_CapsOversizedTranscript so the two cap sites stay
	// in lockstep.
	huge := strings.Repeat("padding text ", 100_000) // ~1.3M chars
	messages := []client.Message{
		{Role: "user", Content: client.NewTextContent(huge)},
		{Role: "assistant", Content: client.NewTextContent("ok")},
	}

	dir := t.TempDir()
	mock := &mockCompleter{
		response: &client.CompletionResponse{OutputText: "- fact"},
	}

	_, err := PersistLearnings(context.Background(), mock, messages, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The user message body sent to the small-tier model must include
	// the truncation marker AND its total length must stay under the cap
	// (plus the existing-memory header overhead).
	sentBody := mock.lastReq.Messages[1].Content.Text()
	if !strings.Contains(sentBody, "transcript truncated") {
		head := sentBody
		if len(head) > 200 {
			head = head[:200]
		}
		t.Errorf("expected truncation marker in capped transcript, got: %q...", head)
	}
	// The capped transcript itself must respect summarizeInputCapChars; the
	// userMsg wraps it with a static header (~70 chars) — allow a small
	// headroom for the wrapper without forcing this test to track its exact
	// byte count.
	const wrapperHeadroom = 256
	if len(sentBody) > summarizeInputCapChars+wrapperHeadroom {
		t.Errorf("sent user-message len = %d, want <= %d (cap %d + wrapper headroom)",
			len(sentBody), summarizeInputCapChars+wrapperHeadroom, summarizeInputCapChars)
	}
}

func TestConsolidateMemoryReturnsUsage(t *testing.T) {
	dir := t.TempDir()
	for i := 0; i < consolidateThreshold; i++ {
		name := fmt.Sprintf("auto-2026-01-%02d.md", i+1)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("- fact"), 0644); err != nil {
			t.Fatalf("write auto file: %v", err)
		}
	}

	mock := &mockCompleter{
		response: &client.CompletionResponse{
			OutputText: "- Consolidated fact",
			Model:      "claude-small",
		},
		usage: client.Usage{
			InputTokens:           90,
			OutputTokens:          25,
			CacheCreation5mTokens: 12,
			CacheCreation1hTokens: 18,
		},
	}

	u, err := ConsolidateMemory(context.Background(), mock, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u.CacheCreation5mTokens != 12 || u.CacheCreation1hTokens != 18 {
		t.Fatalf("expected split cache creation 12/18, got %d/%d", u.CacheCreation5mTokens, u.CacheCreation1hTokens)
	}
}

func TestBoundedAppend(t *testing.T) {
	t.Run("appends content directly when under limit", func(t *testing.T) {
		dir := t.TempDir()
		os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- existing\n"), 0644)

		err := BoundedAppend(dir, "- new entry")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
		content := string(data)
		if !strings.Contains(content, "existing") || !strings.Contains(content, "new entry") {
			t.Error("should contain both existing and new content")
		}
	})

	t.Run("respects boundary when existing memory has trailing newline", func(t *testing.T) {
		dir := t.TempDir()

		// existing has 149 lines and a trailing newline; without this fix, one extra
		// non-prefixed line could incorrectly fit the cap.
		lines := make([]string, maxMemoryLines-1)
		for i := 0; i < maxMemoryLines-1; i++ {
			lines[i] = "- line"
		}
		existing := strings.Join(lines, "\n") + "\n"
		os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(existing), 0644)

		err := BoundedAppend(dir, "- new line")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
		content := string(data)
		if !strings.Contains(content, "auto-") {
			t.Error("should overflow to detail file when append would exceed cap")
		}
		if strings.Contains(content, "new line") {
			t.Error("overflow content should be in detail file, not MEMORY.md")
		}
	})

	t.Run("overflows to detail file at boundary", func(t *testing.T) {
		dir := t.TempDir()

		// Fill MEMORY.md to just under the limit
		var lines []string
		for i := 0; i < maxMemoryLines-1; i++ {
			lines = append(lines, "- line")
		}
		os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(strings.Join(lines, "\n")), 0644)

		// This 3-line append should overflow
		err := BoundedAppend(dir, "- new1\n- new2\n- new3")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
		content := string(data)
		if !strings.Contains(content, "auto-") {
			t.Error("should contain pointer to detail file")
		}
		if strings.Contains(content, "new1") {
			t.Error("overflow content should be in detail file, not MEMORY.md")
		}

		// Detail file should exist with the content
		entries, _ := os.ReadDir(dir)
		found := false
		for _, e := range entries {
			if strings.HasPrefix(e.Name(), "auto-") {
				found = true
				detail, _ := os.ReadFile(filepath.Join(dir, e.Name()))
				if !strings.Contains(string(detail), "new1") {
					t.Error("detail file should contain overflow content")
				}
			}
		}
		if !found {
			t.Error("should have created a detail file")
		}
	})

	t.Run("creates MEMORY.md if missing", func(t *testing.T) {
		dir := t.TempDir()

		err := BoundedAppend(dir, "- first entry")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
		if !strings.Contains(string(data), "first entry") {
			t.Error("should create file with content")
		}
	})
}

func TestConsolidateMemory_SkipsWhenFewFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- user fact 1\n"), 0644)
	// Only 2 auto files (threshold is 12)
	os.WriteFile(filepath.Join(dir, "auto-2026-03-01-aaaaaa.md"), []byte("- fact a\n"), 0644)
	os.WriteFile(filepath.Join(dir, "auto-2026-03-02-bbbbbb.md"), []byte("- fact b\n"), 0644)

	_, err := ConsolidateMemory(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// MEMORY.md unchanged
	data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if string(data) != "- user fact 1\n" {
		t.Errorf("MEMORY.md should be unchanged, got: %q", string(data))
	}
}

func TestConsolidateMemory_SkipsWhenRecent(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- user fact\n"), 0644)
	// 12 auto files
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("auto-2026-03-%02d-%06x.md", i+1, i)
		os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("- fact %d\n", i)), 0644)
	}
	// Touch marker as recent
	os.WriteFile(filepath.Join(dir, ".memory_gc"), []byte(""), 0644)

	_, err := ConsolidateMemory(context.Background(), nil, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// Auto files should still exist (GC skipped)
	matches, _ := filepath.Glob(filepath.Join(dir, "auto-*.md"))
	if len(matches) != 12 {
		t.Errorf("expected 12 auto files (GC skipped), got %d", len(matches))
	}
}

func TestConsolidateMemory_Consolidates(t *testing.T) {
	dir := t.TempDir()

	// User content + auto-persisted section in MEMORY.md
	memory := "- user preference: dark mode\n- user prefers Go\n\n" +
		"## Auto-persisted (2026-03-01 10:00)\n\n- old auto fact 1\n- old auto fact 2\n\n" +
		"- [2026-03-01] See [auto-2026-03-01-aaaaaa.md](auto-2026-03-01-aaaaaa.md) for details\n"
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte(memory), 0644)

	// 12 auto files
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("auto-2026-03-%02d-%06x.md", i+1, i)
		content := fmt.Sprintf("# Auto-persisted Learnings\n\n- detail fact %d\n", i)
		os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
	}

	// Set marker to 8 days ago so cooldown passes
	markerPath := filepath.Join(dir, ".memory_gc")
	os.WriteFile(markerPath, []byte(""), 0644)
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(markerPath, oldTime, oldTime)

	mock := &mockCompleter{
		response: &client.CompletionResponse{
			OutputText: "- consolidated fact A\n- consolidated fact B",
		},
	}

	_, err := ConsolidateMemory(context.Background(), mock, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Ensure inline auto-persisted content from MEMORY.md is included in LLM input.
	if !strings.Contains(mock.lastReq.Messages[1].Content.Text(), "old auto fact 1") {
		t.Error("consolidation should include inline auto section in LLM input")
	}

	// Auto files should be deleted
	matches, _ := filepath.Glob(filepath.Join(dir, "auto-*.md"))
	if len(matches) != 0 {
		t.Errorf("expected 0 auto files after GC, got %d", len(matches))
	}

	// MEMORY.md should have user content + consolidated auto content
	data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	result := string(data)
	if !strings.Contains(result, "user preference: dark mode") {
		t.Error("user content should be preserved")
	}
	if !strings.Contains(result, "user prefers Go") {
		t.Error("user content should be preserved")
	}
	if !strings.Contains(result, "consolidated fact A") {
		t.Error("consolidated content should be present")
	}
	if !strings.Contains(result, "Auto-consolidated") {
		t.Error("should have Auto-consolidated header")
	}
	if strings.Contains(result, "old auto fact 1") {
		t.Error("old auto content should be replaced by consolidated version")
	}
	if strings.Contains(result, "See [auto-") {
		t.Error("auto-*.md pointer lines should be removed")
	}

	// Marker file should be updated
	if _, err := os.Stat(filepath.Join(dir, ".memory_gc")); os.IsNotExist(err) {
		t.Error(".memory_gc marker should exist")
	}
}

func TestConsolidateMemory_LLMReturnsNONE(t *testing.T) {
	dir := t.TempDir()

	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- user fact\n"), 0644)
	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("auto-2026-03-%02d-%06x.md", i+1, i)
		os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("- stale %d\n", i)), 0644)
	}

	// Set marker to 8 days ago so cooldown passes explicitly
	markerPath := filepath.Join(dir, ".memory_gc")
	os.WriteFile(markerPath, []byte(""), 0644)
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(markerPath, oldTime, oldTime)

	mock := &mockCompleter{
		response: &client.CompletionResponse{OutputText: "NONE"},
	}

	_, err := ConsolidateMemory(context.Background(), mock, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// MEMORY.md should only have user content
	data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	result := string(data)
	if !strings.Contains(result, "user fact") {
		t.Error("user content should be preserved")
	}
	if strings.Contains(result, "Auto-consolidated") {
		t.Error("should not have auto section when LLM returned NONE")
	}
}

func TestConsolidateMemory_UnreadableFileConsumed(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- user fact\n"), 0644)

	// 11 readable auto files + 1 unreadable = 12 total (meets threshold)
	for i := 0; i < 11; i++ {
		name := fmt.Sprintf("auto-2026-03-%02d-%06x.md", i+1, i)
		os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("- fact %d\n", i)), 0644)
	}
	// Create an unreadable file (directory with .md name — can't ReadFile a dir)
	unreadable := filepath.Join(dir, "auto-2026-03-12-ffffff.md")
	os.Mkdir(unreadable, 0755)

	// Set marker to 8 days ago
	markerPath := filepath.Join(dir, ".memory_gc")
	os.WriteFile(markerPath, []byte(""), 0644)
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(markerPath, oldTime, oldTime)

	mock := &mockCompleter{
		response: &client.CompletionResponse{OutputText: "- consolidated fact"},
	}

	_, err := ConsolidateMemory(context.Background(), mock, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The unreadable "file" (dir) should have been removed so threshold drops
	remaining, _ := filepath.Glob(filepath.Join(dir, "auto-*.md"))
	if len(remaining) != 0 {
		t.Errorf("expected 0 auto files remaining (including unreadable), got %d", len(remaining))
	}

	// Consolidation should have produced output from the 11 readable files
	data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if !strings.Contains(string(data), "consolidated fact") {
		t.Error("should contain consolidated output from readable files")
	}
}

func TestConsolidateMemory_MarkerWriteFailure(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "MEMORY.md"), []byte("- user fact\n"), 0644)

	for i := 0; i < 12; i++ {
		name := fmt.Sprintf("auto-2026-03-%02d-%06x.md", i+1, i)
		os.WriteFile(filepath.Join(dir, name), []byte(fmt.Sprintf("- fact %d\n", i)), 0644)
	}

	// Set old marker so cooldown passes
	markerPath := filepath.Join(dir, ".memory_gc")
	os.WriteFile(markerPath, []byte(""), 0644)
	oldTime := time.Now().Add(-8 * 24 * time.Hour)
	os.Chtimes(markerPath, oldTime, oldTime)

	// Make marker path a directory so WriteFile fails
	os.Remove(markerPath)
	os.Mkdir(markerPath, 0755)
	os.Chtimes(markerPath, oldTime, oldTime) // must be old so cooldown passes

	mock := &mockCompleter{
		response: &client.CompletionResponse{OutputText: "- consolidated"},
	}

	_, err := ConsolidateMemory(context.Background(), mock, dir)
	if err == nil {
		t.Fatal("expected error when marker write fails")
	}
	if !strings.Contains(err.Error(), "write marker") {
		t.Errorf("error should mention marker write, got: %v", err)
	}

	// MEMORY.md should still be updated (consolidation succeeded, only marker failed)
	data, _ := os.ReadFile(filepath.Join(dir, "MEMORY.md"))
	if !strings.Contains(string(data), "consolidated") {
		t.Error("MEMORY.md should still contain consolidated content despite marker failure")
	}
}

func TestSplitMemory(t *testing.T) {
	input := "- user fact 1\n- user fact 2\n\n" +
		"## Auto-persisted (2026-03-01 10:00)\n\n- auto fact 1\n- auto fact 2\n\n" +
		"- [2026-03-01] See [auto-2026-03-01-aaaaaa.md](auto-2026-03-01-aaaaaa.md) for details\n"

	user, auto := splitMemory(input)

	if !strings.Contains(user, "user fact 1") {
		t.Error("user content should contain user fact 1")
	}
	if !strings.Contains(user, "user fact 2") {
		t.Error("user content should contain user fact 2")
	}
	if strings.Contains(user, "auto fact") {
		t.Error("user content should not contain auto facts")
	}
	if strings.Contains(user, "See [auto-") {
		t.Error("user content should not contain pointer lines")
	}
	if !strings.Contains(auto, "auto fact 1") {
		t.Error("auto content should contain auto fact 1")
	}
	if !strings.Contains(auto, "See [auto-") {
		t.Error("auto content should contain pointer lines")
	}
}

func TestSplitMemory_NoAutoContent(t *testing.T) {
	input := "- user fact 1\n- user fact 2\n"
	user, auto := splitMemory(input)

	if !strings.Contains(user, "user fact 1") {
		t.Error("user content should contain user fact 1")
	}
	if auto != "" {
		t.Errorf("auto content should be empty, got %q", auto)
	}
}

func TestSplitMemory_OnlyAutoContent(t *testing.T) {
	input := "## Auto-persisted (2026-03-01 10:00)\n\n- auto fact 1\n"
	user, auto := splitMemory(input)

	if user != "" {
		t.Errorf("user content should be empty, got %q", user)
	}
	if !strings.Contains(auto, "auto fact 1") {
		t.Error("auto content should contain auto fact 1")
	}
}

func TestPersistLearnings_ReturnsUsage(t *testing.T) {
	dir := t.TempDir()
	mock := &mockCompleter{
		response: &client.CompletionResponse{
			OutputText: "## Learned\n- test fact",
		},
		usage: client.Usage{InputTokens: 300, OutputTokens: 50, CostUSD: 0.001},
	}
	messages := []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
		{Role: "assistant", Content: client.NewTextContent("world")},
	}
	usage, err := PersistLearnings(context.Background(), mock, messages, dir)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 300 || usage.CostUSD != 0.001 {
		t.Errorf("usage not propagated: got %+v", usage)
	}
}

// Helper-tier callers must tag CacheSource="helper". See cache-action-plan §1.1.
func TestPersistLearnings_TagsHelperCacheSource(t *testing.T) {
	dir := t.TempDir()
	mock := &mockCompleter{
		response: &client.CompletionResponse{OutputText: "- learned thing"},
	}
	_, _ = PersistLearnings(context.Background(), mock,
		[]client.Message{
			{Role: "user", Content: client.NewTextContent("q")},
			{Role: "assistant", Content: client.NewTextContent("a")},
		}, dir)
	if mock.lastReq == nil {
		t.Fatal("mockCompleter never received a request")
	}
	if got := mock.lastReq.CacheSource; got != "helper" {
		t.Errorf("PersistLearnings CacheSource = %q, want %q", got, "helper")
	}
}

func TestConsolidateMemory_TagsHelperCacheSource(t *testing.T) {
	dir := t.TempDir()
	// Seed enough auto-*.md files to clear consolidateThreshold so the LLM call fires.
	for i := 0; i < consolidateThreshold+1; i++ {
		fname := filepath.Join(dir, fmt.Sprintf("auto-2026-01-%02d-12-00-aaaaaaaa.md", i+1))
		if err := os.WriteFile(fname, []byte(fmt.Sprintf("- entry %d", i)), 0644); err != nil {
			t.Fatalf("seed file: %v", err)
		}
	}
	mock := &mockCompleter{
		response: &client.CompletionResponse{OutputText: "- consolidated"},
	}
	_, _ = ConsolidateMemory(context.Background(), mock, dir)
	if mock.lastReq == nil {
		t.Fatal("ConsolidateMemory did not invoke the completer — check threshold/cooldown gates")
	}
	if got := mock.lastReq.CacheSource; got != "helper" {
		t.Errorf("ConsolidateMemory CacheSource = %q, want %q", got, "helper")
	}
}
