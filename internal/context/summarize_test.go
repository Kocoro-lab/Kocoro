package context

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// mockCompleter implements Completer for testing.
type mockCompleter struct {
	response *client.CompletionResponse
	err      error
	lastReq  *client.CompletionRequest
	usage    client.Usage
}

func (m *mockCompleter) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	m.lastReq = &req
	if m.response != nil {
		m.response.Usage = m.usage
	}
	return m.response, m.err
}

func TestGenerateSummary(t *testing.T) {
	t.Run("produces summary from conversation", func(t *testing.T) {
		mock := &mockCompleter{
			response: &client.CompletionResponse{
				OutputText: "User asked to fix a bug in main.go. Assistant read the file, found a nil pointer, and applied a fix.",
			},
		}

		messages := []client.Message{
			{Role: "system", Content: client.NewTextContent("You are helpful.")},
			{Role: "user", Content: client.NewTextContent("fix the bug in main.go")},
			{Role: "assistant", Content: client.NewTextContent("I'll read the file first.")},
			{Role: "user", Content: client.NewTextContent("file_read result: ...")},
			{Role: "assistant", Content: client.NewTextContent("Found a nil pointer. Fixing now.")},
		}

		summary, _, err := GenerateSummary(context.Background(), mock, messages)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if summary == "" {
			t.Error("summary should not be empty")
		}

		// Verify it used small tier
		if mock.lastReq.ModelTier != "small" {
			t.Errorf("should use small tier, got %q", mock.lastReq.ModelTier)
		}

		// Verify temperature is low
		if mock.lastReq.Temperature != 0.2 {
			t.Errorf("should use temperature 0.2, got %f", mock.lastReq.Temperature)
		}
	})

	t.Run("returns error on LLM failure", func(t *testing.T) {
		mock := &mockCompleter{
			err: context.DeadlineExceeded,
		}

		messages := []client.Message{
			{Role: "user", Content: client.NewTextContent("hello")},
		}

		_, _, err := GenerateSummary(context.Background(), mock, messages)
		if err == nil {
			t.Error("expected error when LLM fails")
		}
	})

	t.Run("skips system message in summary input", func(t *testing.T) {
		mock := &mockCompleter{
			response: &client.CompletionResponse{
				OutputText: "Summary of conversation.",
			},
		}

		messages := []client.Message{
			{Role: "system", Content: client.NewTextContent("long system prompt here")},
			{Role: "user", Content: client.NewTextContent("do something")},
			{Role: "assistant", Content: client.NewTextContent("done")},
		}

		_, _, err := GenerateSummary(context.Background(), mock, messages)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// The summarization request should not include the system prompt in its messages
		// (it's wasteful and the system prompt is always kept separately)
		for _, msg := range mock.lastReq.Messages {
			if msg.Role == "system" && msg.Content.Text() == "long system prompt here" {
				t.Error("should not pass the original system prompt to summarization call")
			}
		}
	})

	t.Run("extracts summary from two-phase response", func(t *testing.T) {
		mock := &mockCompleter{
			response: &client.CompletionResponse{
				OutputText: "<analysis>\nUser asked about X.\nAssistant did Y.\n</analysis>\n<summary>\nThe actual summary here.\n</summary>",
			},
		}
		messages := []client.Message{
			{Role: "user", Content: client.NewTextContent("test")},
		}
		summary, _, err := GenerateSummary(context.Background(), mock, messages)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if strings.Contains(summary, "<analysis>") {
			t.Error("summary should not contain <analysis> tags")
		}
		if !strings.Contains(summary, "actual summary here") {
			t.Errorf("summary should contain extracted content, got: %q", summary)
		}
	})

	t.Run("includes block content in transcript", func(t *testing.T) {
		mock := &mockCompleter{
			response: &client.CompletionResponse{
				OutputText: "Summary with tool context.",
			},
		}

		// Build a message with tool_use and tool_result blocks
		assistantBlocks := []client.ContentBlock{
			{Type: "text", Text: "Let me read the file."},
			client.NewToolUseBlock("call1", "file_read", []byte(`{"path":"/tmp/foo.go"}`)),
		}
		resultBlocks := []client.ContentBlock{
			client.NewToolResultBlock("call1", "package main\nfunc main() {}", false),
		}

		messages := []client.Message{
			{Role: "system", Content: client.NewTextContent("system")},
			{Role: "user", Content: client.NewTextContent("read foo.go")},
			{Role: "assistant", Content: client.NewBlockContent(assistantBlocks)},
			{Role: "user", Content: client.NewBlockContent(resultBlocks)},
		}

		_, _, err := GenerateSummary(context.Background(), mock, messages)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		// The transcript sent to the LLM should contain tool call and result info
		transcript := mock.lastReq.Messages[1].Content.Text()
		if !strings.Contains(transcript, "file_read") {
			t.Error("transcript should include tool_use name 'file_read'")
		}
		if !strings.Contains(transcript, "package main") {
			t.Error("transcript should include tool_result content")
		}
	})

	t.Run("includes tool metadata needed for structured continuation summary", func(t *testing.T) {
		mock := &mockCompleter{
			response: &client.CompletionResponse{
				OutputText: "Structured summary with loaded tools and active skill.",
			},
		}

		assistantBlocks := []client.ContentBlock{
			client.NewToolUseBlock("read1", "file_read", []byte(`{"path":"/tmp/foo.go","offset":10}`)),
			client.NewToolUseBlock("skill1", "use_skill", []byte(`{"skill_name":"test-driven-development"}`)),
			client.NewToolUseBlock("search1", "tool_search", []byte(`{"query":"select:browser_navigate,github_list_prs"}`)),
		}
		resultBlocks := []client.ContentBlock{
			client.NewToolResultBlock("read1", "  11 | package main", false),
			client.NewToolResultBlock("skill1", "Write the failing test first.", false),
			client.NewToolResultBlockWithBlocks("search1", []client.ContentBlock{
				{Type: "tool_reference", ToolName: "browser_navigate"},
				{Type: "tool_reference", ToolName: "github_list_prs"},
			}, false),
		}

		messages := []client.Message{
			{Role: "system", Content: client.NewTextContent("system")},
			{Role: "user", Content: client.NewTextContent("continue the task after compaction")},
			{Role: "assistant", Content: client.NewBlockContent(assistantBlocks)},
			{Role: "user", Content: client.NewBlockContent(resultBlocks)},
		}

		_, _, err := GenerateSummary(context.Background(), mock, messages)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		transcript := mock.lastReq.Messages[1].Content.Text()
		for _, needle := range []string{
			`"/tmp/foo.go"`,
			`"skill_name":"test-driven-development"`,
			"browser_navigate",
			"github_list_prs",
		} {
			if !strings.Contains(transcript, needle) {
				t.Errorf("transcript should include %q, got:\n%s", needle, transcript)
			}
		}
	})
}

func TestGenerateSummaryReturnsUsage(t *testing.T) {
	mock := &mockCompleter{
		response: &client.CompletionResponse{
			OutputText: "Summary of conversation.",
			Model:      "claude-small",
		},
		usage: client.Usage{
			InputTokens:           120,
			OutputTokens:          40,
			CacheCreation5mTokens: 30,
			CacheCreation1hTokens: 70,
		},
	}

	summary, u, err := GenerateSummary(context.Background(), mock, []client.Message{
		{Role: "user", Content: client.NewTextContent("hello")},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if summary != "Summary of conversation." {
		t.Fatalf("unexpected summary: %q", summary)
	}
	if u.CacheCreation5mTokens != 30 || u.CacheCreation1hTokens != 70 {
		t.Fatalf("expected split cache creation 30/70, got %d/%d", u.CacheCreation5mTokens, u.CacheCreation1hTokens)
	}
}

func TestExtractSummary(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		contains string
		excludes string
	}{
		{
			"both tags present",
			"<analysis>\nwalkthrough\n</analysis>\n<summary>\nthe summary\n</summary>",
			"the summary",
			"walkthrough",
		},
		{
			"summary only, no analysis",
			"<summary>just the summary</summary>",
			"just the summary",
			"",
		},
		{
			"analysis stripped when no summary tags",
			"<analysis>scratch work</analysis>\nLeftover text here.",
			"Leftover text here",
			"scratch work",
		},
		{
			"unclosed analysis stripped",
			"Some preamble.<analysis>scratch work without closing",
			"Some preamble",
			"scratch work",
		},
		{
			"unclosed summary takes remainder",
			"<summary>everything after tag",
			"everything after tag",
			"",
		},
		{
			"no tags at all — returns raw",
			"plain summary without any tags",
			"plain summary without any tags",
			"",
		},
		{
			"empty after stripping analysis — returns empty",
			"<analysis>only analysis content</analysis>",
			"",
			"analysis",
		},
		{
			"structured summary preserves section headers",
			"<analysis>walkthrough</analysis>\n<summary>\n## Current task & next steps\nFixing the bug.\n\n## Open files / important reads\ninternal/agent/loop.go — core loop\n</summary>",
			"## Open files",
			"walkthrough",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := extractSummary(tt.input)
			if tt.contains != "" && !strings.Contains(got, tt.contains) {
				t.Errorf("expected to contain %q, got: %q", tt.contains, got)
			}
			if tt.contains == "" && tt.excludes != "" && got != "" {
				t.Errorf("expected empty string, got: %q", got)
			}
			if tt.excludes != "" && strings.Contains(got, tt.excludes) {
				t.Errorf("should not contain %q, got: %q", tt.excludes, got)
			}
		})
	}
}

func TestGenerateSummary_ReturnsUsage(t *testing.T) {
	mock := &mockCompleter{
		response: &client.CompletionResponse{
			OutputText: "<analysis>thinking</analysis>\n<summary>test summary</summary>",
		},
		usage: client.Usage{InputTokens: 500, OutputTokens: 100, TotalTokens: 600, CostUSD: 0.002},
	}
	messages := []client.Message{{Role: "user", Content: client.NewTextContent("hello")}}
	_, usage, err := GenerateSummary(context.Background(), mock, messages)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if usage.InputTokens != 500 || usage.OutputTokens != 100 || usage.CostUSD != 0.002 {
		t.Errorf("usage not propagated: got %+v", usage)
	}
}

// TestCompactToolInput_FiltersEmptyEquivalents verifies that both empty-object
// ("{}") and empty-array ("[]") inputs are treated as "no args" and omitted
// from the rendered transcript. Without the "[]" filter, an array-rooted
// empty input would render as `[tool_call: name []]` — noise that conveys
// no semantic information.
func TestCompactToolInput_FiltersEmptyEquivalents(t *testing.T) {
	tests := []struct {
		name string
		raw  string
		want string
	}{
		{"empty string", "", ""},
		{"null literal", "null", ""},
		{"empty object", "{}", ""},
		{"empty array", "[]", ""},
		{"whitespace around null", "  null  ", ""},
		{"real object input", `{"path":"/tmp/foo"}`, `{"path":"/tmp/foo"}`},
		{"real array input", `[1,2,3]`, `[1,2,3]`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := compactToolInput([]byte(tt.raw))
			if got != tt.want {
				t.Errorf("compactToolInput(%q) = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// TestSummarizeToolResult_RefsPreservedWithLongText verifies that when a
// tool_result carries both near-limit text AND nested tool_reference blocks,
// the "Loaded tools: ..." line is NOT silently clipped by the 500-rune cap.
// Regression guard for the PR-review finding: the original implementation
// concatenated base text + refs and truncated as one unit, so a long
// tool_result would eat the refs line. The fix truncates base text first,
// leaving explicit room for refs.
func TestSummarizeToolResult_RefsPreservedWithLongText(t *testing.T) {
	longText := strings.Repeat("a", 490)
	block := client.ContentBlock{
		Type: "tool_result",
		ToolContent: []client.ContentBlock{
			{Type: "text", Text: longText},
			{Type: "tool_reference", ToolName: "browser_navigate"},
			{Type: "tool_reference", ToolName: "github_list_prs"},
		},
	}
	got := summarizeToolResult(block)

	wantRefs := "Loaded tools: browser_navigate, github_list_prs"
	if !strings.Contains(got, wantRefs) {
		t.Errorf("expected %q to survive even with long base text; got:\n%s",
			wantRefs, got)
	}
}

// TestSummarizePrompt_RequiresStructuredSections asserts the summarization
// prompt explicitly instructs the LLM to emit three working-state sections
// inside <summary>. This is a guardrail against future edits silently
// dropping the structure — if the sections are removed, post-compaction
// behavior regresses (model re-reads files it had open, re-activates skills).
func TestSummarizePrompt_RequiresStructuredSections(t *testing.T) {
	required := []string{
		"Open files",
		"Active skill",
		"Loaded tool",
		"Never return an empty",
	}
	for _, phrase := range required {
		if !strings.Contains(summarizePrompt, phrase) {
			t.Errorf("summarizePrompt must instruct LLM to include section %q; current prompt:\n%s",
				phrase, summarizePrompt)
		}
	}
}

// Helper-tier callers must tag CacheSource="helper" so Shannon routes them to
// the 5m fallback bucket (not the main session's 1h bucket) and analysts can
// filter them out of cache-debug.log. See docs/issues/cache-action-plan.md §1.1.
func TestGenerateSummary_TagsHelperCacheSource(t *testing.T) {
	mock := &mockCompleter{
		response: &client.CompletionResponse{OutputText: "<summary>x</summary>"},
	}
	_, _, _ = GenerateSummary(context.Background(), mock,
		[]client.Message{{Role: "user", Content: client.NewTextContent("hi")}})
	if mock.lastReq == nil {
		t.Fatal("mockCompleter never received a request")
	}
	if got := mock.lastReq.CacheSource; got != "helper" {
		t.Errorf("GenerateSummary CacheSource = %q, want %q", got, "helper")
	}
}

func TestSummarizeForUser_TagsHelperCacheSource(t *testing.T) {
	mock := &mockCompleter{
		response: &client.CompletionResponse{OutputText: "summary"},
	}
	_, _ = SummarizeForUser(context.Background(), mock,
		[]client.Message{{Role: "user", Content: client.NewTextContent("hi")}})
	if mock.lastReq == nil {
		t.Fatal("mockCompleter never received a request")
	}
	if got := mock.lastReq.CacheSource; got != "helper" {
		t.Errorf("SummarizeForUser CacheSource = %q, want %q", got, "helper")
	}
}

// TestGenerateSummary_CapsOversizedTranscript guards against the 2026-05-07
// production cascade: reactive recovery hands GenerateSummary a transcript
// already over the small-tier (Haiku 4.5, 200K) cap, the summarizer 400s
// with "prompt is too long", and the user sees a hard error. The fix is
// an internal head+tail truncation that keeps the small-tier input under
// summarizeInputCapChars regardless of how large the input is.
func TestGenerateSummary_CapsOversizedTranscript(t *testing.T) {
	// Build a transcript far over the small-tier 200K cap.
	// 1.3M chars is ~371K tokens at 3.5 chars/token — well past Haiku 4.5's 200K.
	huge := strings.Repeat("padding text ", 100_000) // ~1.3M chars
	messages := []client.Message{
		{Role: "user", Content: client.NewTextContent(huge)},
	}

	mock := &mockCompleter{
		response: &client.CompletionResponse{
			OutputText: "<summary>ok</summary>",
		},
	}
	_, _, err := GenerateSummary(context.Background(), mock, messages)
	if err != nil {
		t.Fatalf("GenerateSummary returned error: %v", err)
	}

	// The transcript that actually went to the small-tier model must be
	// at or under the cap, not the raw 1.3M chars.
	sentBody := mock.lastReq.Messages[1].Content.Text()
	if len(sentBody) > summarizeInputCapChars {
		t.Errorf("sent transcript len = %d, want <= %d (cap)",
			len(sentBody), summarizeInputCapChars)
	}
	if !strings.Contains(sentBody, "transcript truncated") {
		head := sentBody
		if len(head) > 200 {
			head = head[:200]
		}
		t.Errorf("expected truncation marker in capped transcript, got: %q...", head)
	}
}

// TestCapTranscriptForSummarize_UTF8Safe verifies that head+tail truncation
// of multi-byte content (Chinese here, but applies to any UTF-8) does not
// split a rune mid-sequence. Without rune-boundary adjustment, json.Marshal
// would replace the dangling bytes with U+FFFD before the summarizer sees
// them. (See 2026-05-08 review Finding 3.)
func TestCapTranscriptForSummarize_UTF8Safe(t *testing.T) {
	// 7 bytes per "你好 " run; 200_000 runs ≈ 1.4M bytes — well over cap.
	chinese := strings.Repeat("你好 ", 200_000)
	if !utf8.ValidString(chinese) {
		t.Fatalf("test setup invariant: input must be valid UTF-8")
	}

	out := capTranscriptForSummarize(chinese)
	if !utf8.ValidString(out) {
		t.Errorf("output is not valid UTF-8 — head/tail truncation split a rune. len=%d", len(out))
	}
	if len(out) > summarizeInputCapChars {
		t.Errorf("output len = %d, want <= %d (cap)", len(out), summarizeInputCapChars)
	}
	if !strings.Contains(out, "transcript truncated") {
		t.Errorf("expected truncation marker in capped output")
	}
}

// TestCapTranscriptForSummarize_BoundaryAtCap verifies the cap is enforced
// at the byte boundary (input == cap → no truncation; input == cap+1 →
// truncation fires).
func TestCapTranscriptForSummarize_BoundaryAtCap(t *testing.T) {
	atCap := strings.Repeat("a", summarizeInputCapChars)
	if got := capTranscriptForSummarize(atCap); got != atCap {
		t.Errorf("input at cap was modified: in=%d out=%d", len(atCap), len(got))
	}

	overCap := strings.Repeat("a", summarizeInputCapChars+1)
	got := capTranscriptForSummarize(overCap)
	if got == overCap {
		t.Errorf("input over cap was NOT truncated: len=%d", len(got))
	}
	if !strings.Contains(got, "transcript truncated") {
		t.Errorf("over-cap input missing truncation marker")
	}
}
