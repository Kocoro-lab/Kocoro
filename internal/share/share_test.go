package share

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// fakeCompleter satisfies ctxwin.Completer (and client.LLMClient.Complete)
// without touching the network. Returns canned text or a canned error.
type fakeCompleter struct {
	out string
	err error
	// seenSystemPrompt captures the system message from the most recent call
	// so tests can assert that share called SummarizeForUser (not GenerateSummary).
	seenSystemPrompt string
	// seenCacheSource captures the CacheSource tag from the most recent call.
	// Tests use it to confirm the share endpoint pins "session_share" so
	// Cloud-side billing can route it into the user-quota exempt bucket.
	seenCacheSource string
}

func (f *fakeCompleter) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	for _, m := range req.Messages {
		if m.Role == "system" {
			f.seenSystemPrompt = m.Content.Text()
			break
		}
	}
	f.seenCacheSource = req.CacheSource
	if f.err != nil {
		return nil, f.err
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
	// Confirm we hit SummarizeForUser (markdown, in-language) rather than
	// the structured GenerateSummary path.
	if !strings.Contains(gw.seenSystemPrompt, "Markdown summary for a human reader") {
		t.Fatalf("expected SummarizeForUser system prompt, got: %s", gw.seenSystemPrompt)
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

func TestRender_CancelledContextErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	sess := &session.Session{ID: "s1", Title: "t"}
	_, err := Render(ctx, nil, sess, Options{})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
}
