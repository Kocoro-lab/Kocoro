package share

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestRenderHTML_BasicShape(t *testing.T) {
	ts := time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC)
	sess := &session.Session{
		ID:        "sess_abc123def456",
		Title:     "Demo session",
		CreatedAt: ts,
	}
	input := RenderInput{
		Session: sess,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello there")},
			{Role: "assistant", Content: client.NewTextContent("greetings")},
		},
		Summary:     "Brief recap of the session.",
		GeneratedAt: ts,
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, "<title>Demo session · Kocoro</title>")
	mustContain(t, out, "Brief recap of the session.")
	mustContain(t, out, "hello there")
	mustContain(t, out, "greetings")
	mustContain(t, out, `class="msg user"`)
	mustContain(t, out, `class="msg assistant"`)
	mustContain(t, out, "2 messages")
}

func TestRenderHTML_OmitsSummaryWhenEmpty(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	input := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if strings.Contains(string(html), `<section class="summary">`) {
		t.Fatalf("empty summary should not render the summary section")
	}
}

func TestRenderHTML_ImageBecomesDataURI(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	const data = "iVBORw0KGgoAAAANSUhEUgAAA"
	input := RenderInput{
		Session: sess,
		Messages: []client.Message{{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: data}},
			}),
		}},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if !strings.Contains(string(html), `src="data:image/png;base64,`+data+`"`) {
		t.Fatalf("expected data URI for image, got:\n%s", html)
	}
}

func TestRenderHTML_DisallowedImageMediaTypeDropped(t *testing.T) {
	// A malicious media_type like "javascript" would normally be rendered
	// inside a "data:" URI prefix; the whitelist drops the block entirely.
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	input := RenderInput{
		Session: sess,
		Messages: []client.Message{{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "text/html", Data: "evil"}},
				{Type: "text", Text: "carrier text"},
			}),
		}},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	if strings.Contains(string(html), "evil") {
		t.Fatalf("disallowed image MIME data leaked into output")
	}
	if !strings.Contains(string(html), "carrier text") {
		t.Fatalf("other blocks should still render")
	}
}

func TestRenderHTML_ToolUseFolded(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	input, _ := json.Marshal(map[string]any{"command": "ls -la"})
	in := RenderInput{
		Session: sess,
		Messages: []client.Message{{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "tool_use", ID: "toolu_1", Name: "bash", Input: input},
			}),
		}},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, `<details class="tool">`)
	mustContain(t, out, "tool call") // badge label in the new template
	mustContain(t, out, "bash")
	mustContain(t, out, "ls -la")
	// The <pre> wrapping should be present so the JSON renders monospace.
	mustContain(t, out, "<pre>")
}

func TestRenderHTML_ToolResultErrorClass(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	in := RenderInput{
		Session: sess,
		Messages: []client.Message{{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "tool_result", ToolUseID: "toolu_1", IsError: true, ToolContent: "boom"},
			}),
		}},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, "tool error") // class indicator on <details>
	mustContain(t, out, "boom")       // the error text itself, in <pre>
	// The badge text "error" appears for IsError tool_results in the new
	// template; a more precise assertion than the previous "(error)" literal.
	if !strings.Contains(out, `<span class="badge">error</span>`) {
		t.Fatalf("expected error badge, sample:\n%s", snippet(out))
	}
}

func TestRenderHTML_ToolResultNestedImage(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	in := RenderInput{
		Session: sess,
		Messages: []client.Message{{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				{
					Type:      "tool_result",
					ToolUseID: "toolu_1",
					ToolContent: []client.ContentBlock{
						{Type: "text", Text: "screenshot below"},
						{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "PNG"}},
					},
				},
			}),
		}},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, "screenshot below")
	mustContain(t, out, `src="data:image/png;base64,PNG"`)
}

func TestRenderHTML_TextEscapesHTMLAndScripts(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "<x>", CreatedAt: time.Now()}
	in := RenderInput{
		Session: sess,
		Messages: []client.Message{{
			Role: "user",
			Content: client.NewTextContent(`<script>alert("xss")</script>`),
		}},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	// goldmark with WithUnsafe disabled elides raw HTML entirely (replaces it
	// with a "raw HTML omitted" comment) rather than escaping. Either outcome
	// satisfies the security goal — no executable script tag in the output —
	// but we assert the active-content shape is absent regardless of strategy.
	if strings.Contains(out, "<script>alert") {
		t.Fatalf("raw <script> survived markdown rendering:\n%s", snippet(out))
	}
	if strings.Contains(out, `alert("xss")`) {
		t.Fatalf("script body should be elided or escaped, not pass through:\n%s", snippet(out))
	}
	// Title is escaped by html/template's auto-escape.
	mustContain(t, out, "&lt;x&gt;")
}

func TestRenderHTML_SkipsMessagesWithOnlyUnknownBlocks(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	in := RenderInput{
		Session: sess,
		Messages: []client.Message{
			{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "future_block_type_xyz", Text: "hi"},
			})},
			{Role: "user", Content: client.NewTextContent("real message")},
		},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	if strings.Contains(out, "future_block_type_xyz") {
		t.Fatalf("unknown block type leaked: %s", out)
	}
	mustContain(t, out, "real message")
	mustContain(t, out, "1 message")
}

func TestRenderHTML_NilSessionErrors(t *testing.T) {
	_, err := RenderHTML(RenderInput{})
	if err == nil {
		t.Fatalf("expected error for nil session")
	}
}

func TestSplitPageTitleFromSummary(t *testing.T) {
	cases := []struct {
		name      string
		summary   string
		sessTitle string
		wantTitle string
		wantBody  string
	}{
		{
			name:      "summary H1 wins over long session title",
			summary:   "# 对话总结：查询支持的模型\n## 问题\n用户询问...",
			sessTitle: "查看我的桌面文件夹（~/Desktop）, 列出里面所有的文件和文件夹，每项用一句话描述",
			wantTitle: "对话总结：查询支持的模型",
			wantBody:  "## 问题\n用户询问...",
		},
		{
			name:      "no H1 falls back to session title untruncated",
			summary:   "Plain prose summary without any heading.",
			sessTitle: "Short title",
			wantTitle: "Short title",
			wantBody:  "Plain prose summary without any heading.",
		},
		{
			name:      "no H1 + long session title gets truncated",
			summary:   "Plain prose.",
			sessTitle: strings.Repeat("a", 80),
			wantTitle: strings.Repeat("a", 50) + "…",
			wantBody:  "Plain prose.",
		},
		{
			name:      "H1-only summary leaves empty body",
			summary:   "# Title alone",
			sessTitle: "ignored",
			wantTitle: "Title alone",
			wantBody:  "",
		},
		{
			name:      "summary with trailing newline after H1",
			summary:   "# Heading\n\nBody line.",
			sessTitle: "x",
			wantTitle: "Heading",
			wantBody:  "Body line.",
		},
		{
			name:      "empty summary uses session title",
			summary:   "",
			sessTitle: "fallback",
			wantTitle: "fallback",
			wantBody:  "",
		},
		{
			name:      "everything empty yields empty (caller handles default)",
			summary:   "",
			sessTitle: "",
			wantTitle: "",
			wantBody:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			title, body := splitPageTitleFromSummary(tc.summary, tc.sessTitle)
			if title != tc.wantTitle {
				t.Errorf("title:\n  got  %q\n  want %q", title, tc.wantTitle)
			}
			if body != tc.wantBody {
				t.Errorf("body:\n  got  %q\n  want %q", body, tc.wantBody)
			}
		})
	}
}

func TestRenderHTML_UsesSummaryH1AsTitle(t *testing.T) {
	// End-to-end: a Haiku summary with leading H1 should become the page <h1>;
	// the H1 must not also appear inside the summary card (would be visually
	// redundant — same heading twice in a row).
	sess := &session.Session{
		ID:        "s1",
		Title:     "查看我的桌面文件夹（~/Desktop），列出里面所有的文件和文件夹，每项用一句话描述", // long, would be ugly as page title
		CreatedAt: time.Now(),
	}
	in := RenderInput{
		Session: sess,
		Summary: "# 桌面目录清单\n## 文件列表\n包含 5 个文件夹和 3 个文件。",
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hi")},
		},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)

	// Page <title> and header <h1> both use the extracted H1.
	mustContain(t, out, "<title>桌面目录清单 · Kocoro</title>")
	if !strings.Contains(out, ">桌面目录清单</h1>") {
		t.Fatalf("expected H1 to be the page header, got:\n%s", snippet(out))
	}

	// The long original sess.Title must not leak into header — that was the
	// bug we're guarding against.
	if strings.Contains(out, "查看我的桌面文件夹") {
		t.Fatalf("long session title leaked into page header:\n%s", snippet(out))
	}

	// Title text appears exactly twice: once in <title> (head meta), once
	// in <h1> (header). Three+ occurrences would mean the H1 also leaked
	// back into the summary card body — the bug we're guarding against.
	if got := strings.Count(out, "桌面目录清单"); got != 2 {
		t.Fatalf("title text should appear exactly twice (<title> + <h1>); got %d:\n%s",
			got, snippet(out))
	}
	// And the secondary heading from the summary body should still render.
	mustContain(t, out, "文件列表")
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("output missing %q. Sample:\n%s", needle, snippet(haystack))
	}
}

func snippet(s string) string {
	if len(s) <= 800 {
		return s
	}
	return s[:800] + "..."
}
