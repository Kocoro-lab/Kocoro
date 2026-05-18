package share

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// fakeCompleter satisfies ctxwin.Completer (and client.LLMClient.Complete)
// without touching the network. Returns canned text or a canned error.
//
// share.Render fires TWO parallel Haiku calls (summary + slug) through
// the same gateway, so the fake routes by system prompt: requests whose
// system prompt mentions "slug generator" get slugOut, others get out.
// This lets tests verify both calls independently without bespoke mocks.
type fakeCompleter struct {
	out     string // default response (summary path)
	slugOut string // response for the slug-prompt path (optional)
	err     error
	// seenSystemPrompt captures the most recent NON-slug system prompt
	// (i.e. the summary path) so existing tests can keep asserting on it.
	seenSystemPrompt string
	// seenCacheSource captures the CacheSource tag from the most recent call.
	// Tests use it to confirm the share endpoint pins "session_share" so
	// Cloud-side billing can route it into the user-quota exempt bucket.
	seenCacheSource string

	mu sync.Mutex
}

func (f *fakeCompleter) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	var sys string
	for _, m := range req.Messages {
		if m.Role == "system" {
			sys = m.Content.Text()
			break
		}
	}
	isSlug := strings.Contains(sys, "URL slug generator")

	f.mu.Lock()
	f.seenCacheSource = req.CacheSource
	if !isSlug {
		f.seenSystemPrompt = sys
	}
	f.mu.Unlock()

	if f.err != nil {
		return nil, f.err
	}
	if isSlug {
		return &client.CompletionResponse{OutputText: f.slugOut}, nil
	}
	return &client.CompletionResponse{OutputText: f.out}, nil
}

func TestRender_HappyPath(t *testing.T) {
	sess := &session.Session{
		ID:    "sess_test",
		Title: "Refactor the loader",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("walk me through the loader")},
			{Role: "assistant", Content: client.NewTextContent("it has three stages…")},
		},
	}
	gw := &fakeCompleter{out: "We refactored the session loader into three stages."}
	res, err := Render(context.Background(), gw, sess, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.SummaryFallback {
		t.Fatalf("expected Haiku path used, got fallback")
	}
	if !strings.Contains(string(res.HTML), "We refactored the session loader") {
		t.Fatalf("Haiku summary missing from output")
	}
	// Confirm we hit SummarizeForShare (2-3 sentence plain-text overview)
	// rather than the longer SummarizeForUser Markdown variant or the
	// structured GenerateSummary path.
	if !strings.Contains(gw.seenSystemPrompt, "2-3 sentence overview") {
		t.Fatalf("expected SummarizeForShare system prompt (looking for '2-3 sentence overview'), got: %s", gw.seenSystemPrompt)
	}
}

func TestRender_TagsCallWithSessionShareCacheSource(t *testing.T) {
	// Contract test: share's Haiku call MUST be tagged "session_share" so
	// Cloud-side billing can route it into the not-user-billed bucket
	// (parallel to "prompt_suggestion"). If this drifts, users get charged
	// for every share they generate — a silent regression that's worth a
	// fail-loud test.
	sess := &session.Session{
		ID:    "s1",
		Title: "any",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello")},
		},
	}
	gw := &fakeCompleter{out: "# summary\nbody"}
	_, err := Render(context.Background(), gw, sess, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if gw.seenCacheSource != "session_share" {
		t.Fatalf("expected cache_source=session_share for billing exemption, got %q", gw.seenCacheSource)
	}
}

func TestRender_HaikuErrorFallsBackToTitle(t *testing.T) {
	sess := &session.Session{
		ID:    "s1",
		Title: "Title-based fallback",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hi")},
		},
	}
	gw := &fakeCompleter{err: errors.New("upstream 500")}
	res, err := Render(context.Background(), gw, sess, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !res.SummaryFallback {
		t.Fatalf("expected SummaryFallback=true when Haiku errors")
	}
	if res.Summary != "Title-based fallback" {
		t.Fatalf("expected fallback to title, got %q", res.Summary)
	}
}

func TestRender_HaikuEmptyOutputFallsBack(t *testing.T) {
	sess := &session.Session{
		ID:    "s1",
		Title: "title",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hi")},
		},
	}
	gw := &fakeCompleter{out: "   \n  "}
	res, err := Render(context.Background(), gw, sess, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !res.SummaryFallback {
		t.Fatalf("whitespace-only summary should trigger fallback")
	}
}

func TestRender_NilCompleterUsesFallback(t *testing.T) {
	sess := &session.Session{
		ID:    "s1",
		Title: "",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("first message body that becomes the summary")},
		},
	}
	res, err := Render(context.Background(), nil, sess, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if !res.SummaryFallback {
		t.Fatalf("nil completer should trigger fallback")
	}
	if !strings.Contains(res.Summary, "first message body") {
		t.Fatalf("expected first user message as fallback, got %q", res.Summary)
	}
}

func TestRender_DropsSystemInjectedFromHTML(t *testing.T) {
	// End-to-end: a message flagged SystemInjected should not appear in the
	// final HTML, proving the sanitize pass actually runs.
	sess := &session.Session{
		ID:    "s1",
		Title: "Test",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("real")},
			{Role: "assistant", Content: client.NewTextContent("INJECTED NUDGE TEXT")},
		},
		MessageMeta: []session.MessageMeta{
			{},
			{SystemInjected: true},
		},
	}
	gw := &fakeCompleter{out: "summary"}
	res, err := Render(context.Background(), gw, sess, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if strings.Contains(string(res.HTML), "INJECTED NUDGE TEXT") {
		t.Fatalf("SystemInjected message leaked into HTML")
	}
}

func TestRender_NilSessionErrors(t *testing.T) {
	_, err := Render(context.Background(), nil, nil, Options{})
	if err == nil {
		t.Fatalf("expected error for nil session")
	}
}

func TestRender_PropagatesHaikuSlug(t *testing.T) {
	// share.Render fires summary + slug in parallel. Assert the slug call
	// happens, its output is captured into RenderResult.Slug, and the
	// summary path still works concurrently.
	sess := &session.Session{
		ID:    "s1",
		Title: "现在支持哪些模型",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("现在 Kocoro 支持哪些模型？")},
		},
	}
	gw := &fakeCompleter{
		out:     "# 对话总结\n用户询问支持的模型。",
		slugOut: "supported-models-query",
	}
	res, err := Render(context.Background(), gw, sess, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.Slug != "supported-models-query" {
		t.Errorf("Slug = %q, want %q", res.Slug, "supported-models-query")
	}
	if res.SummaryFallback {
		t.Errorf("summary should have succeeded alongside slug")
	}
}

func TestRender_EmptySlugOnHaikuError(t *testing.T) {
	// When Haiku errors, BOTH parallel calls fail (same gateway). slug
	// returns empty so the handler can fall back to title-ASCII slug.
	sess := &session.Session{
		ID:    "s1",
		Title: "fallback",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hi")},
		},
	}
	gw := &fakeCompleter{err: errors.New("upstream 500")}
	res, err := Render(context.Background(), gw, sess, Options{})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	if res.Slug != "" {
		t.Errorf("Slug = %q on error, want empty", res.Slug)
	}
	if !res.SummaryFallback {
		t.Errorf("summary should fall back on same error")
	}
}

func TestCleanSlug(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"already clean", "debug-payment-flow", "debug-payment-flow"},
		{"mixed case lowercased", "Debug-Payment-Flow", "debug-payment-flow"},
		{"strips leading 'slug:' preamble", "slug: refactor-loader", "refactor-loader"},
		{"strips 'Output:' preamble", "Output: machine-learning-basics", "machine-learning-basics"},
		{"strips surrounding quotes", `"loader-refactor"`, "loader-refactor"},
		{"strips backticks", "`payment-bug-fix`", "payment-bug-fix"},
		{"strips trailing period", "list-desktop-files.", "list-desktop-files"},
		{"underscore and space collapse to hyphen", "list desktop files", "list-desktop-files"},
		{"slash separator", "refactor/loader", "refactor-loader"},
		{"collapses consecutive hyphens", "debug--payment---bug", "debug-payment-bug"},
		{"strips leading/trailing hyphens", "-refactor-loader-", "refactor-loader"},
		{"rejects too short (1 char)", "a", ""},
		{"rejects pure punctuation", "...", ""},
		{"rejects unicode-only", "你好世界", ""},
		{"caps at 40 runes", strings.Repeat("a", 60), strings.Repeat("a", 40)},
		{"empty input", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := cleanSlug(tc.in)
			if got != tc.want {
				t.Errorf("cleanSlug(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestGenerateEnglishSlug_NilGateway(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "test"}
	got := generateEnglishSlug(context.Background(), nil, sess, nil)
	if got != "" {
		t.Errorf("nil gw should return empty, got %q", got)
	}
}

func TestRender_CancelledContextErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sess := &session.Session{ID: "s1", Title: "t"}
	_, err := Render(ctx, nil, sess, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
