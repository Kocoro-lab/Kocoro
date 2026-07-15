package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func newRemoteTimelineTestServer(t *testing.T, messages []client.Message, meta []session.MessageMeta) (*Server, *session.Session) {
	t.Helper()
	dir := t.TempDir()
	deps := &ServerDeps{ShannonDir: dir, SessionCache: NewSessionCache(dir)}
	mgr := deps.SessionCache.GetOrCreate("")
	sess := mgr.NewSession()
	sess.Title = "Remote timeline fixture"
	sess.CWD = "/tmp/project"
	sess.Messages = messages
	sess.MessageMeta = meta
	if err := mgr.Save(); err != nil {
		t.Fatalf("save session: %v", err)
	}
	return NewServer(0, &Client{}, deps, "test"), sess
}

func TestRemoteTimeline_BoundsLargeSessionWithoutChangingFullEndpoint(t *testing.T) {
	imageData := strings.Repeat("A", 900*1024)
	toolOutput := strings.Repeat("tool-output-", 90*1024)
	messages := []client.Message{
		{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "text", Text: "please inspect this"},
				{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: imageData}},
			}),
		},
		{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolUseBlock("tool-1", "bash", json.RawMessage(`{"command":"long task"}`)),
			}),
		},
		{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("tool-1", toolOutput, false),
			}),
		},
		{Role: "assistant", Content: client.NewTextContent("done")},
	}
	srv, sess := newRemoteTimelineTestServer(t, messages, nil)

	// The default local endpoint remains lossless.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/sessions/"+sess.ID, nil)
	req.SetPathValue("id", sess.ID)
	srv.handleGetSession(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("full endpoint status=%d body=%s", rec.Code, rec.Body.String())
	}
	var full session.Session
	if err := json.Unmarshal(rec.Body.Bytes(), &full); err != nil {
		t.Fatalf("decode full session: %v", err)
	}
	if got := full.Messages[0].Content.Blocks()[1].Source.Data; got != imageData {
		t.Fatalf("full endpoint changed image bytes: got=%d want=%d", len(got), len(imageData))
	}

	// The legacy remote request still binds at the transport cap, while the
	// explicit projection stays within the reserved page budget.
	legacy := srv.HandleRemoteRequest(context.Background(), RemoteRequest{
		Method: http.MethodGet,
		Path:   "/sessions/" + sess.ID,
	})
	if legacy.Status != http.StatusRequestEntityTooLarge {
		t.Fatalf("legacy remote status=%d, want 413", legacy.Status)
	}
	projected := srv.HandleRemoteRequest(context.Background(), RemoteRequest{
		Method: http.MethodGet,
		Path:   "/sessions/" + sess.ID + "?view=remote_timeline",
	})
	if projected.Status != http.StatusOK {
		t.Fatalf("timeline remote status=%d error=%q", projected.Status, projected.Error)
	}
	if len(projected.Body) > remoteTimelineResponseBudgetBytes {
		t.Fatalf("timeline response=%d exceeds budget=%d", len(projected.Body), remoteTimelineResponseBudgetBytes)
	}
	if strings.Contains(string(projected.Body), imageData[:256]) ||
		strings.Contains(string(projected.Body), toolOutput[:remoteTimelineMaxToolResultBytes+1]) {
		t.Fatal("projected response leaked an unbounded payload")
	}

	var page remoteTimelinePage
	if err := json.Unmarshal(projected.Body, &page); err != nil {
		t.Fatalf("decode timeline page: %v", err)
	}
	if page.PageVersion != 1 || page.HasMore || page.NextCursor != "" {
		t.Fatalf("page metadata=%+v", page)
	}
	if len(page.Messages) != len(messages) || len(page.MessageMeta) != len(messages) {
		t.Fatalf("page alignment messages=%d meta=%d", len(page.Messages), len(page.MessageMeta))
	}
	if page.OmittedContentCount < 2 {
		t.Fatalf("omitted_content_count=%d, want at least image+tool result", page.OmittedContentCount)
	}
	imagePlaceholder := page.Messages[0].Content.Blocks()[1]
	if imagePlaceholder.Type != "text" || !strings.Contains(imagePlaceholder.Text, "image · image/png") {
		t.Fatalf("image placeholder=%+v", imagePlaceholder)
	}
	result := page.Messages[2].Content.Blocks()[0]
	if result.Type != "tool_result" || result.ToolUseID != "tool-1" || len(client.ToolResultText(result)) > remoteTimelineMaxToolResultBytes+len(remoteTimelineTruncationSuffix) {
		t.Fatalf("tool result projection=%+v text_bytes=%d", result, len(client.ToolResultText(result)))
	}
}

func TestRemoteTimeline_PagesNewestFirstAndAlignsMetadata(t *testing.T) {
	messages := make([]client.Message, 7)
	meta := make([]session.MessageMeta, 7)
	for i := range messages {
		messages[i] = client.Message{Role: "user", Content: client.NewTextContent(fmt.Sprintf("message-%d", i))}
		meta[i] = session.MessageMeta{Source: fmt.Sprintf("source-%d", i)}
	}
	srv, sess := newRemoteTimelineTestServer(t, messages, meta)

	fetch := func(path string) remoteTimelinePage {
		t.Helper()
		resp := srv.HandleRemoteRequest(context.Background(), RemoteRequest{Method: http.MethodGet, Path: path})
		if resp.Status != http.StatusOK {
			t.Fatalf("GET %s status=%d error=%q body=%s", path, resp.Status, resp.Error, resp.Body)
		}
		var page remoteTimelinePage
		if err := json.Unmarshal(resp.Body, &page); err != nil {
			t.Fatalf("decode %s: %v", path, err)
		}
		return page
	}

	first := fetch("/sessions/" + sess.ID + "?view=remote_timeline&limit=3")
	if first.StartIndex != 4 || first.NextCursor != "4" || !first.HasMore || first.TotalMessages != 7 {
		t.Fatalf("first page metadata=%+v", first)
	}
	if first.Messages[0].Content.Text() != "message-4" || first.MessageMeta[0].Source != "source-4" {
		t.Fatalf("first page misaligned: message=%q meta=%q", first.Messages[0].Content.Text(), first.MessageMeta[0].Source)
	}

	second := fetch("/sessions/" + sess.ID + "?view=remote_timeline&limit=3&before=" + first.NextCursor)
	if second.StartIndex != 1 || second.NextCursor != "1" || !second.HasMore {
		t.Fatalf("second page metadata=%+v", second)
	}
	if second.Messages[0].Content.Text() != "message-1" || second.MessageMeta[0].Source != "source-1" {
		t.Fatalf("second page misaligned: message=%q meta=%q", second.Messages[0].Content.Text(), second.MessageMeta[0].Source)
	}

	last := fetch("/sessions/" + sess.ID + "?view=remote_timeline&limit=3&before=" + second.NextCursor)
	if last.StartIndex != 0 || last.NextCursor != "" || last.HasMore || len(last.Messages) != 1 {
		t.Fatalf("last page metadata=%+v messages=%d", last, len(last.Messages))
	}
}

func TestRemoteTimeline_DoesNotSplitToolUseAndResult(t *testing.T) {
	messages := []client.Message{
		{Role: "user", Content: client.NewTextContent("run it")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{client.NewToolUseBlock("tool-1", "bash", json.RawMessage(`{"command":"pwd"}`))})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{client.NewToolResultBlock("tool-1", "/tmp", false)})},
		{Role: "assistant", Content: client.NewTextContent("finished")},
	}
	srv, sess := newRemoteTimelineTestServer(t, messages, nil)

	first := srv.HandleRemoteRequest(context.Background(), RemoteRequest{Method: http.MethodGet, Path: "/sessions/" + sess.ID + "?view=remote_timeline&limit=2"})
	var firstPage remoteTimelinePage
	if err := json.Unmarshal(first.Body, &firstPage); err != nil {
		t.Fatal(err)
	}
	if len(firstPage.Messages) != 1 || firstPage.StartIndex != 3 {
		t.Fatalf("first page split policy wrong: start=%d messages=%d", firstPage.StartIndex, len(firstPage.Messages))
	}

	second := srv.HandleRemoteRequest(context.Background(), RemoteRequest{Method: http.MethodGet, Path: "/sessions/" + sess.ID + "?view=remote_timeline&limit=2&before=" + firstPage.NextCursor})
	var secondPage remoteTimelinePage
	if err := json.Unmarshal(second.Body, &secondPage); err != nil {
		t.Fatal(err)
	}
	if len(secondPage.Messages) != 2 || secondPage.StartIndex != 1 {
		t.Fatalf("tool pair page start=%d messages=%d", secondPage.StartIndex, len(secondPage.Messages))
	}
	if !remoteTimelineHasToolUse(secondPage.Messages[0]) || !remoteTimelineHasToolResult(secondPage.Messages[1]) {
		t.Fatalf("tool pair was not preserved: %+v", secondPage.Messages)
	}
}

func TestRemoteTimeline_RejectsCursorThatSplitsToolUseAndResult(t *testing.T) {
	messages := []client.Message{
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{client.NewToolUseBlock("tool-1", "bash", json.RawMessage(`{"command":"pwd"}`))})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{client.NewToolResultBlock("tool-1", "/tmp", false)})},
		{Role: "assistant", Content: client.NewTextContent("finished")},
	}
	srv, sess := newRemoteTimelineTestServer(t, messages, nil)

	resp := srv.HandleRemoteRequest(context.Background(), RemoteRequest{
		Method: http.MethodGet,
		Path:   "/sessions/" + sess.ID + "?view=remote_timeline&before=1",
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400; body=%s", resp.Status, resp.Body)
	}
	if !strings.Contains(string(resp.Body), "invalid_remote_timeline_page") ||
		!strings.Contains(string(resp.Body), "splits a tool-use/result pair") {
		t.Fatalf("body=%s", resp.Body)
	}
}

func TestRemoteTimeline_RejectsMalformedCursor(t *testing.T) {
	srv, sess := newRemoteTimelineTestServer(t, []client.Message{{Role: "user", Content: client.NewTextContent("hello")}}, nil)
	resp := srv.HandleRemoteRequest(context.Background(), RemoteRequest{
		Method: http.MethodGet,
		Path:   "/sessions/" + sess.ID + "?view=remote_timeline&before=not-a-cursor",
	})
	if resp.Status != http.StatusBadRequest {
		t.Fatalf("status=%d, want 400", resp.Status)
	}
	if !strings.Contains(string(resp.Body), "invalid_remote_timeline_page") {
		t.Fatalf("body=%s", resp.Body)
	}
}

func TestRemoteTimeline_DropsReasoningWithoutUserVisibleOmission(t *testing.T) {
	projected, omitted := projectRemoteTimelineMessage(client.Message{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "thinking", Thinking: "private reasoning"},
			{Type: "text", Text: "visible answer"},
		}),
	})
	if omitted != 0 {
		t.Fatalf("reasoning-only projection should not raise the UI omission banner: %d", omitted)
	}
	if len(projected.Content.Blocks()) != 1 || projected.Content.Blocks()[0].Text != "visible answer" {
		t.Fatalf("projected blocks=%+v", projected.Content.Blocks())
	}
	encoded, err := json.Marshal(projected)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private reasoning") || strings.Contains(string(encoded), "thinking") {
		t.Fatalf("reasoning leaked into remote timeline: %s", encoded)
	}
}

func TestRemoteTimelinePageSizerMatchesJSONMarshal(t *testing.T) {
	messages := []client.Message{
		{Role: "user", Content: client.NewTextContent("first <message> \"quoted\"")},
		{Role: "assistant", Content: client.NewTextContent("第二条消息")},
		{Role: "user", Content: client.NewTextContent("last")},
	}
	meta := []session.MessageMeta{
		{Source: `source"<>&`, MessageID: "message-1"},
		{Source: "mobile", MessageID: "message-2", SystemInjected: true},
		{},
	}
	sess := &session.Session{
		ID:          `session"<>&`,
		Title:       "Timeline <title> \"quoted\"",
		CWD:         `/tmp/project"<>&`,
		Messages:    messages,
		MessageMeta: meta,
	}
	page := newRemoteTimelinePage(sess, len(messages))
	sizer, err := newRemoteTimelinePageSizer(page)
	if err != nil {
		t.Fatal(err)
	}

	assertExact := func() {
		t.Helper()
		got, err := sizer.encodedBytes(page.StartIndex, page.HasMore, page.NextCursor, page.OmittedContentCount)
		if err != nil {
			t.Fatal(err)
		}
		encoded, err := json.Marshal(page)
		if err != nil {
			t.Fatal(err)
		}
		if got != len(encoded) {
			t.Fatalf("incremental size=%d, json.Marshal size=%d, page=%s", got, len(encoded), encoded)
		}
	}
	assertExact()

	firstMessages := messages[1:]
	firstMeta := meta[1:]
	sizer, err = sizer.withGroup(firstMessages, firstMeta)
	if err != nil {
		t.Fatal(err)
	}
	page.Messages = firstMessages
	page.MessageMeta = firstMeta
	page.StartIndex = 1
	page.HasMore = true
	page.NextCursor = "1"
	page.OmittedContentCount = 12
	assertExact()

	sizer, err = sizer.withGroup(messages[:1], meta[:1])
	if err != nil {
		t.Fatal(err)
	}
	page.Messages = messages
	page.MessageMeta = meta
	page.StartIndex = 0
	page.HasMore = false
	page.NextCursor = ""
	page.OmittedContentCount = 123
	assertExact()
}

func TestRemoteTimelineIncrementalSizerStopsAtExactBudgetBoundary(t *testing.T) {
	messages := make([]client.Message, remoteTimelineMaxLimit)
	meta := make([]session.MessageMeta, remoteTimelineMaxLimit)
	for i := range messages {
		messages[i] = client.Message{Role: "user", Content: client.NewTextContent(strings.Repeat("x", 12*1024))}
		meta[i] = session.MessageMeta{Source: "mobile", MessageID: fmt.Sprintf("message-%d", i)}
	}
	sess := &session.Session{ID: "budget", Title: "Budget boundary", Messages: messages, MessageMeta: meta}
	req := httptest.NewRequest(http.MethodGet, "/sessions/budget?view=remote_timeline&limit=100", nil)

	page, err := buildRemoteTimelinePage(sess, req)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded)+1 > remoteTimelineResponseBudgetBytes {
		t.Fatalf("page bytes=%d exceed budget=%d", len(encoded)+1, remoteTimelineResponseBudgetBytes)
	}
	if page.StartIndex <= 0 || !page.HasMore {
		t.Fatalf("byte budget did not bind: start=%d has_more=%v messages=%d", page.StartIndex, page.HasMore, len(page.Messages))
	}

	nextMessage, omitted := projectRemoteTimelineMessage(messages[page.StartIndex-1])
	candidate := *page
	candidate.Messages = append([]client.Message{nextMessage}, page.Messages...)
	candidate.MessageMeta = append([]session.MessageMeta{meta[page.StartIndex-1]}, page.MessageMeta...)
	candidate.StartIndex--
	candidate.OmittedContentCount += omitted
	candidate.HasMore = candidate.StartIndex > 0
	candidate.NextCursor = remoteTimelineCursor(candidate.StartIndex)
	candidateEncoded, err := json.Marshal(candidate)
	if err != nil {
		t.Fatal(err)
	}
	if len(candidateEncoded)+1 <= remoteTimelineResponseBudgetBytes {
		t.Fatalf("page stopped early: current=%d candidate=%d budget=%d", len(encoded)+1, len(candidateEncoded)+1, remoteTimelineResponseBudgetBytes)
	}
}

func BenchmarkBuildRemoteTimelinePage(b *testing.B) {
	messages := make([]client.Message, remoteTimelineMaxLimit)
	meta := make([]session.MessageMeta, remoteTimelineMaxLimit)
	for i := range messages {
		messages[i] = client.Message{
			Role:    "user",
			Content: client.NewTextContent(strings.Repeat("message payload ", 400)),
		}
		meta[i] = session.MessageMeta{Source: "mobile", MessageID: fmt.Sprintf("message-%d", i)}
	}
	sess := &session.Session{ID: "benchmark", Title: "Benchmark", Messages: messages, MessageMeta: meta}
	req := httptest.NewRequest(http.MethodGet, "/sessions/benchmark?view=remote_timeline&limit=100", nil)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := buildRemoteTimelinePage(sess, req); err != nil {
			b.Fatal(err)
		}
	}
}
