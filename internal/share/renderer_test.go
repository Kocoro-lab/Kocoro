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

// TestRenderHTML_MessageTimestampsUseUTCFallback pins the per-message
// timestamp fix that paired with the header/footer fix. mm.Timestamp comes
// from MessageMeta and is typically stamped with the daemon's local zone;
// before the fix it rendered as "15:04" without zone info — JS-disabled
// viewers saw the daemon operator's wall-clock hour/minute and assumed it
// was their own timezone. The fallback text now carries a UTC suffix +
// the <time datetime> attribute lets the client-side localizer rewrite to
// viewer-local on load.
func TestRenderHTML_MessageTimestampsUseUTCFallback(t *testing.T) {
	created := time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC)
	// Message stamped at 03:45 UTC. If we ever regress to server-local
	// formatting, a CI runner in UTC would still show "03:45" and the test
	// would pass — so anchor on the UTC suffix instead.
	msgTime := time.Date(2026, 5, 15, 3, 45, 0, 0, time.UTC)

	sess := &session.Session{ID: "s1", Title: "ts test", CreatedAt: created}
	// Assistant role renders the role+time header line; user-with-bubble
	// suppresses the timestamp by design (the bubble layout is timestamp-less
	// for visual compactness). Use assistant so the <time> element actually
	// appears in the output.
	input := RenderInput{
		Session: sess,
		Messages: []client.Message{
			{Role: "assistant", Content: client.NewTextContent("greetings")},
		},
		Meta: []session.MessageMeta{
			{Timestamp: session.TimePtr(msgTime)},
		},
		GeneratedAt: created,
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)

	// Fallback text: "03:45 UTC", carried inside a <time> with ISO datetime
	// and data-fmt="time" so the localizer knows to use time-only formatting.
	mustContain(t, out, `<time datetime="2026-05-15T03:45:00Z" data-fmt="time">03:45 UTC</time>`)

	// Defense-in-depth: a bare "03:45" without a zone suffix would mean the
	// regression came back. The "UTC" suffix is the load-bearing tell.
	if strings.Contains(out, `>03:45<`) {
		t.Errorf("message timestamp regressed to no-zone format:\n%s", snippet(out))
	}
}

// TestRenderHTML_TimestampsUnifiedToUTCWithISO pins the fix for the
// "header CST + footer UTC" bug. Both visible timestamps must:
//  1. Render UTC text as the no-JS fallback (NOT the daemon operator's
//     local timezone — that was the bug).
//  2. Carry a <time datetime="ISO8601"> attribute so the client-side
//     localize-timestamps script can rewrite them to the viewer's browser
//     timezone + locale.
//  3. Use UTC consistently in both places so a JS-disabled viewer never
//     sees mismatched zones.
func TestRenderHTML_TimestampsUnifiedToUTCWithISO(t *testing.T) {
	// Construct fixed UTC times so the assertions can match byte-for-byte
	// regardless of where this test runs (CI in UTC, dev laptop in CST/PST).
	created := time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC)
	generated := time.Date(2026, 5, 18, 8, 11, 0, 0, time.UTC)
	sess := &session.Session{ID: "s1", Title: "ts test", CreatedAt: created}
	input := RenderInput{
		Session:     sess,
		Messages:    []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		GeneratedAt: generated,
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)

	// Header time element: UTC fallback text + RFC3339 datetime attribute.
	mustContain(t, out, `<time datetime="2026-05-15T09:30:00Z">2026-05-15 09:30 UTC</time>`)

	// Footer time element: same shape, no special "UTC" suffix divergence.
	mustContain(t, out, `<time datetime="2026-05-18T08:11:00Z">2026-05-18 08:11 UTC</time>`)

	// Both UTC fallback strings carry the same "UTC" suffix — guards against
	// a future refactor that re-introduces "MST" / "CST" on one side.
	if strings.Contains(out, "MST</time>") || strings.Contains(out, "CST</time>") ||
		strings.Contains(out, "PST</time>") || strings.Contains(out, "EST</time>") {
		t.Errorf("server-local timezone leaked into UTC fallback text:\n%s", snippet(out))
	}

	// The localizer script must be embedded so viewer-side rewrite works.
	mustContain(t, out, `id="localize-timestamps"`)
	mustContain(t, out, "Intl.DateTimeFormat")
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

func TestRenderHTML_ToolUseOmitted(t *testing.T) {
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
	// Tool execution detail is omitted entirely from shares — no tool <details>,
	// no "tool call" badge, no tool name, no input JSON.
	mustNotContain(t, out, `class="tool"`)
	mustNotContain(t, out, "tool call")
	mustNotContain(t, out, "ls -la")
}

func TestRenderHTML_ToolResultTextOmitted(t *testing.T) {
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
	// The textual tool output (and the error badge/class) is dropped from shares.
	mustNotContain(t, out, "boom")
	mustNotContain(t, out, `<span class="badge">error</span>`)
}

func TestRenderHTML_ToolResultNestedImageKept(t *testing.T) {
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
	// The tool's text output is dropped, but images it produced are kept.
	mustNotContain(t, out, "screenshot below")
	mustContain(t, out, `src="data:image/png;base64,PNG"`)
	// A pure tool_result message bearing only an image is re-tagged to the
	// "tool" role and rendered outside any user-typed bubble.
	mustContain(t, out, `<article class="msg tool">`)
}

// An assistant turn that both narrates and calls a tool should keep the prose
// while dropping the tool call — the common "let me run X" shape.
func TestRenderHTML_AssistantTextKeptToolUseDropped(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	input, _ := json.Marshal(map[string]any{"command": "ls -la"})
	in := RenderInput{
		Session: sess,
		Messages: []client.Message{{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "text", Text: "Let me list the directory."},
				{Type: "tool_use", ID: "toolu_1", Name: "bash", Input: input},
			}),
		}},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, "Let me list the directory.")
	mustNotContain(t, out, "ls -la")
	mustNotContain(t, out, `class="tool"`)
}

// An html-artifact fence in assistant text renders as a sandboxed iframe, with
// the artifact markup confined to the (attribute-escaped) srcdoc — never as an
// active top-level element on the share page.
func TestRenderHTML_ArtifactRendersInSandboxedIframe(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	body := "Here is the chart:\n\n" +
		"```html-artifact title=\"Q1\" id=art_q1 mime=text/html\n" +
		"<div id=c></div><script>render('c')</script>\n" +
		"```\n"
	in := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "assistant", Content: client.NewTextContent(body)}},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)

	// Surrounding prose still renders.
	mustContain(t, out, "Here is the chart:")
	// A sandboxed iframe is emitted, WITHOUT same-origin or popup escalation
	// (matching Desktop's iframe config).
	mustContain(t, out, "<iframe")
	mustContain(t, out, `sandbox="allow-scripts"`)
	mustNotContain(t, out, "allow-same-origin")
	mustNotContain(t, out, "allow-popups")
	// data-artifact-id is render-assigned (art_N), not the fence's own id.
	mustContain(t, out, `data-artifact-id="art_1"`)
	// The Desktop design-system preset is injected into the srcdoc so the
	// fragment's var(--color-*) / .c-* references resolve. Tokens appear
	// attribute-escaped inside srcdoc.
	mustContain(t, out, "--color-background-primary")
	mustContain(t, out, "kocoro-content")
	// CSP meta is present (single quotes attribute-escaped to &#39;).
	mustContain(t, out, "Content-Security-Policy")
	mustContain(t, out, "default-src")
	// The artifact's own <script> must be confined to the srcdoc attribute as
	// escaped text — it must NOT appear as a live top-level script element with
	// the artifact's body. html/template attribute-escapes "<" to "&lt;".
	mustContain(t, out, "&lt;script&gt;render(")
	mustNotContain(t, out, "<script>render('c')</script>")
}

// Artifacts must get distinct data-artifact-id values even when the model
// reuses the SAME fence id across turns (Desktop's "update in place" idiom) —
// a share page shows each version as its own iframe, and a duplicated id would
// leave all but the first clipped at min-height by the resize listener.
func TestRenderHTML_ArtifactIDsUniqueAcrossMessages(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	// Same explicit id=dup in both messages.
	art := func() string { return "```html-artifact title=X id=dup\n<p>x</p>\n```\n" }
	in := RenderInput{
		Session: sess,
		Messages: []client.Message{
			{Role: "assistant", Content: client.NewTextContent(art())},
			{Role: "assistant", Content: client.NewTextContent(art())},
		},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, `data-artifact-id="art_1"`)
	mustContain(t, out, `data-artifact-id="art_2"`)
	// The reused fence id must NOT become the element id (it would collide).
	mustNotContain(t, out, `data-artifact-id="dup"`)
}

// A plain ```html fence is NOT an artifact: it stays a source code block, no iframe.
func TestRenderHTML_PlainHTMLFenceStaysCode(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	body := "example:\n\n```html\n<div>just code</div>\n```\n"
	in := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "assistant", Content: client.NewTextContent(body)}},
	}
	html, err := RenderHTML(in)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, "<pre><code")
	mustContain(t, out, "&lt;div&gt;just code")
	// No artifact iframe for a plain html fence. A literal "<iframe" only ever
	// comes from a rendered artifact (the autosize script / CSS reference the
	// selector and class as strings, never as a literal "<iframe" element).
	mustNotContain(t, out, "<iframe")
}

func TestRenderHTML_TextEscapesHTMLAndScripts(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "<x>", CreatedAt: time.Now()}
	in := RenderInput{
		Session: sess,
		Messages: []client.Message{{
			Role:    "user",
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

	// The original guard ("appears exactly twice") was for the pre-OG-meta
	// template — with OG/Twitter/JSON-LD tags the title now legitimately
	// appears in <title>, <h1>, og:title, twitter:title, and JSON-LD
	// name/headline (six places). Replace the count check with a precise
	// "title text must NOT appear inside the rendered summary card body"
	// guard — that's the actual regression we want to catch (H1 duplicated
	// as the first paragraph of the summary section).
	if idx := strings.Index(out, `<section class="summary">`); idx >= 0 {
		// Find the matching </section>. Sections are not nested in this
		// template, so the next </section> after the open is ours.
		rest := out[idx:]
		end := strings.Index(rest, "</section>")
		if end < 0 {
			t.Fatalf("malformed summary section in output:\n%s", snippet(out))
		}
		summaryBody := rest[:end]
		if strings.Contains(summaryBody, "桌面目录清单") {
			t.Fatalf("title H1 leaked back into the summary card body:\n%s", snippet(summaryBody))
		}
	}
	// And the secondary heading from the summary body should still render.
	mustContain(t, out, "文件列表")
}

// TestRenderHTML_SocialMetaPresent pins the baseline shape of the OG /
// Twitter Card / JSON-LD tags injected into <head>. The test pretends to
// be the daemon (passing a Metadata block with SiteName / SiteURL set) and
// asserts every load-bearing tag is present so a future refactor that
// drops a category (e.g. removes Twitter Card to "simplify") is caught.
func TestRenderHTML_SocialMetaPresent(t *testing.T) {
	created := time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC)
	sess := &session.Session{ID: "s1", Title: "Reviewing migration plan", CreatedAt: created}
	input := RenderInput{
		Session:     sess,
		Messages:    []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Summary:     "# Migration plan review\nDiscussed two-phase rollout and rollback drill.",
		GeneratedAt: created,
		Metadata: ShareMetadata{
			SiteName: "Kocoro",
			SiteURL:  "https://www.kocoro.ai/",
		},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)

	// Core OG tags
	mustContain(t, out, `<meta property="og:type" content="article">`)
	mustContain(t, out, `<meta property="og:title" content="Migration plan review">`)
	mustContain(t, out, `<meta property="og:description"`)
	mustContain(t, out, `<meta property="og:site_name" content="Kocoro">`)
	mustContain(t, out, `<meta property="og:locale" content="en_US">`)
	mustContain(t, out, `<meta property="article:published_time" content="2026-05-15T09:30:00Z">`)
	mustContain(t, out, `<meta property="article:modified_time" content="2026-05-15T09:30:00Z">`)

	// Twitter Card (no image → summary, not summary_large_image)
	mustContain(t, out, `<meta name="twitter:card" content="summary">`)
	mustContain(t, out, `<meta name="twitter:title" content="Migration plan review">`)
	mustContain(t, out, `<meta name="twitter:description"`)

	// SEO basics
	mustContain(t, out, `<meta name="description"`)
	mustContain(t, out, `<meta name="author" content="Kocoro">`)

	// Favicon link uses configured SiteURL
	mustContain(t, out, `<link rel="icon" type="image/x-icon" href="https://www.kocoro.ai/favicon.ico">`)

	// JSON-LD block
	mustContain(t, out, `<script type="application/ld+json">`)
}

// TestRenderHTML_OGImageOptional confirms the conditional rendering: empty
// DefaultOGImage skips every image-related tag and downgrades the Twitter
// card to "summary"; a non-empty value emits og:image + Twitter image and
// upgrades the card to "summary_large_image".
func TestRenderHTML_OGImageOptional(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	base := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Summary:  "Quick recap.",
	}

	t.Run("no image", func(t *testing.T) {
		input := base
		input.Metadata = ShareMetadata{SiteName: "Kocoro", SiteURL: "https://www.kocoro.ai/"}
		html, err := RenderHTML(input)
		if err != nil {
			t.Fatalf("RenderHTML: %v", err)
		}
		out := string(html)
		if strings.Contains(out, `property="og:image"`) {
			t.Fatalf("og:image should be absent when DefaultOGImage is empty:\n%s", snippet(out))
		}
		if strings.Contains(out, `name="twitter:image"`) {
			t.Fatalf("twitter:image should be absent when DefaultOGImage is empty:\n%s", snippet(out))
		}
		mustContain(t, out, `<meta name="twitter:card" content="summary">`)
	})

	t.Run("with image", func(t *testing.T) {
		input := base
		input.Metadata = ShareMetadata{
			SiteName:       "Kocoro",
			SiteURL:        "https://www.kocoro.ai/",
			DefaultOGImage: "https://static.kocoro.ai/og/default.png",
		}
		html, err := RenderHTML(input)
		if err != nil {
			t.Fatalf("RenderHTML: %v", err)
		}
		out := string(html)
		mustContain(t, out, `<meta property="og:image" content="https://static.kocoro.ai/og/default.png">`)
		mustContain(t, out, `<meta property="og:image:alt"`)
		// og:image:width / og:image:height intentionally not emitted —
		// the configured image may not be 1200×630 (e.g. the Kocoro logo
		// used as a temporary stand-in), so we let platforms measure the
		// actual asset rather than seeding them a wrong dimension hint.
		if strings.Contains(out, `og:image:width`) {
			t.Errorf("og:image:width should not be emitted; let platforms measure the image")
		}
		mustContain(t, out, `<meta name="twitter:image" content="https://static.kocoro.ai/og/default.png">`)
		mustContain(t, out, `<meta name="twitter:card" content="summary_large_image">`)
	})
}

// TestRenderHTML_TwitterImageOverridesOGImage covers the dual-image setup:
// a square brand mark for og:image (which Slack / Teams / Facebook /
// LinkedIn unfurl well as a thumbnail) plus a 1200×630 wide hero for
// twitter:image (what summary_large_image cards actually want). The two
// meta tags must point at different URLs without affecting each other.
func TestRenderHTML_TwitterImageOverridesOGImage(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	input := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Summary:  "Quick recap.",
		Metadata: ShareMetadata{
			SiteName:       "Kocoro",
			SiteURL:        "https://www.kocoro.ai/",
			DefaultOGImage: "https://static.kocoro.ai/square.png",
			TwitterImage:   "https://static.kocoro.ai/wide-1200x630.png",
		},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, `<meta property="og:image" content="https://static.kocoro.ai/square.png">`)
	mustContain(t, out, `<meta name="twitter:image" content="https://static.kocoro.ai/wide-1200x630.png">`)
	mustContain(t, out, `<meta name="twitter:card" content="summary_large_image">`)
	// And the cross-contamination guard: og:image must NOT carry the wide
	// Twitter URL, and twitter:image must NOT carry the square URL.
	if strings.Contains(out, `property="og:image" content="https://static.kocoro.ai/wide-1200x630.png"`) {
		t.Fatalf("og:image incorrectly received the TwitterImage URL")
	}
	if strings.Contains(out, `name="twitter:image" content="https://static.kocoro.ai/square.png"`) {
		t.Fatalf("twitter:image incorrectly received the DefaultOGImage URL")
	}
}

// TestRenderHTML_TwitterImageFallsBackToOGImage pins the back-compat
// fallback: a caller that only sets DefaultOGImage (the pre-split-image
// API) still gets a working twitter:image. Without this fallback the
// split would silently break every existing yaml config that only knows
// about the older single-image knob.
func TestRenderHTML_TwitterImageFallsBackToOGImage(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	input := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Summary:  "Quick recap.",
		Metadata: ShareMetadata{
			SiteName:       "Kocoro",
			SiteURL:        "https://www.kocoro.ai/",
			DefaultOGImage: "https://static.kocoro.ai/only-one.png",
			// TwitterImage intentionally omitted
		},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, `<meta property="og:image" content="https://static.kocoro.ai/only-one.png">`)
	mustContain(t, out, `<meta name="twitter:image" content="https://static.kocoro.ai/only-one.png">`)
}

// TestRenderHTML_OGTitleEscaping defends against a Haiku-generated or user-
// authored title that contains `"`, `<`, `>`, `&`, or even a literal
// `</script>` from breaking out of the meta content="…" attribute or the
// JSON-LD <script> block.
func TestRenderHTML_OGTitleEscaping(t *testing.T) {
	sess := &session.Session{
		ID:        "s1",
		Title:     `Title with "quotes" & <tag> and </script>`,
		CreatedAt: time.Now(),
	}
	input := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Metadata: ShareMetadata{SiteName: "Kocoro", SiteURL: "https://www.kocoro.ai/"},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)

	// Raw "&" must be encoded as "&amp;" inside attribute context — a bare
	// "&" risks being parsed as a partial entity reference by older
	// crawlers.
	if strings.Contains(out, `content="Title with "quotes"`) {
		t.Fatalf("unescaped quote leaked into meta content attribute:\n%s", snippet(out))
	}
	// </script> must not survive raw inside the JSON-LD body — json.Marshal
	// HTML-escapes "<" to "<" by default, which is what we rely on for
	// the security property. Assert the literal didn't slip through.
	if strings.Contains(out, `</script>`) {
		// Count occurrences: three legitimate closing `</script>` tags are
		// expected — the JSON-LD block, the timestamp localizer, and the
		// artifact-autosize listener. Title-derived `</script>` would be a
		// fourth+ hit.
		got := strings.Count(out, `</script>`)
		if got > 3 {
			t.Fatalf("title-injected </script> appears to have leaked (count=%d):\n%s", got, snippet(out))
		}
	}
}

// TestRenderHTML_OGTitleTruncation verifies the og:title length cap. The
// pipeline runs the page title through truncatePageTitle (50 runes) first,
// then truncateOGTitle (60 runes) as a defense-in-depth backstop, so a
// 200-rune sess.Title surfaces at the page-title cap (50 + ellipsis). The
// 60-rune backstop matters when page-title limits get loosened in the
// future or when og:title gets sourced independently — we keep the cap to
// protect Facebook / LinkedIn / WeChat from over-long titles.
func TestRenderHTML_OGTitleTruncation(t *testing.T) {
	long := strings.Repeat("a", 200)
	sess := &session.Session{ID: "s1", Title: long, CreatedAt: time.Now()}
	input := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Metadata: ShareMetadata{SiteName: "Kocoro", SiteURL: "https://www.kocoro.ai/"},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)

	// Extract og:title content to check the actual length rather than guess
	// which cap (50 vs 60) fired — both are valid, and we only care that
	// the final value sits at or below the 60-rune wire cap.
	const prefix = `<meta property="og:title" content="`
	idx := strings.Index(out, prefix)
	if idx < 0 {
		t.Fatalf("og:title tag not found:\n%s", snippet(out))
	}
	rest := out[idx+len(prefix):]
	end := strings.Index(rest, `">`)
	if end < 0 {
		t.Fatalf("og:title not properly closed:\n%s", snippet(out))
	}
	got := rest[:end]
	if got == "" {
		t.Fatalf("og:title content is empty")
	}
	if len([]rune(got)) > ogTitleMaxRunes+1 { // +1 for the ellipsis rune
		t.Fatalf("og:title length %d exceeds %d-rune cap + ellipsis; content=%q",
			len([]rune(got)), ogTitleMaxRunes, got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix on truncated og:title, got %q", got)
	}
}

// TestRenderHTML_OGDescriptionTruncation pins the 150-rune cap on
// og:description. We feed a Haiku summary body (post-H1) longer than 150
// runes and expect the meta content to be truncated.
func TestRenderHTML_OGDescriptionTruncation(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	// Build a summary with H1 (eaten by splitPageTitleFromSummary) plus a
	// body that's well over 150 runes of prose. ASCII-only so each rune is
	// one byte and the length check is straightforward.
	body := strings.Repeat("plain prose body content ", 20) // 25*20 = 500 chars
	input := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Summary:  "# h\n" + body,
		Metadata: ShareMetadata{SiteName: "Kocoro", SiteURL: "https://www.kocoro.ai/"},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)

	// Extract description content. Use the well-known prefix.
	const prefix = `<meta name="description" content="`
	idx := strings.Index(out, prefix)
	if idx < 0 {
		t.Fatalf("meta description tag not found:\n%s", snippet(out))
	}
	rest := out[idx+len(prefix):]
	end := strings.Index(rest, `">`)
	if end < 0 {
		t.Fatalf("meta description tag not properly closed:\n%s", snippet(out))
	}
	got := rest[:end]
	// 150 rune cap + "…"; ASCII-only so rune count == byte count.
	if len([]rune(got)) > 151 {
		t.Fatalf("description length %d exceeds 150-rune cap + ellipsis; content: %q",
			len([]rune(got)), got)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("expected ellipsis suffix on truncated description, got %q", got)
	}
}

// TestRenderHTML_JSONLDParseable does a round-trip through the JSON-LD
// block: extract the body between <script type="application/ld+json"> and
// </script>, json.Unmarshal it, and check the load-bearing fields are set
// (publisher.name = "Kocoro", @type = "Article"). A regression where the
// template emits invalid JSON would cause every social-platform validator
// (Google Rich Results Test, LinkedIn Post Inspector) to warn.
func TestRenderHTML_JSONLDParseable(t *testing.T) {
	created := time.Date(2026, 5, 15, 9, 30, 0, 0, time.UTC)
	sess := &session.Session{ID: "s1", Title: "Plan review", CreatedAt: created}
	input := RenderInput{
		Session:     sess,
		Messages:    []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Summary:     "# Plan review\nQuick recap.",
		GeneratedAt: created,
		Metadata: ShareMetadata{
			SiteName: "Kocoro",
			SiteURL:  "https://www.kocoro.ai/",
			LogoURL:  "https://static.kocoro.ai/logo.png",
		},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)

	const startTag = `<script type="application/ld+json">`
	const endTag = `</script>`
	startIdx := strings.Index(out, startTag)
	if startIdx < 0 {
		t.Fatalf("JSON-LD start tag not found")
	}
	rest := out[startIdx+len(startTag):]
	endIdx := strings.Index(rest, endTag)
	if endIdx < 0 {
		t.Fatalf("JSON-LD end tag not found")
	}
	body := rest[:endIdx]

	var doc map[string]any
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("JSON-LD body did not parse: %v\nbody: %s", err, body)
	}

	if doc["@context"] != "https://schema.org" {
		t.Errorf("@context = %v, want https://schema.org", doc["@context"])
	}
	if doc["@type"] != "Article" {
		t.Errorf("@type = %v, want Article", doc["@type"])
	}
	pub, ok := doc["publisher"].(map[string]any)
	if !ok {
		t.Fatalf("publisher missing or wrong shape: %v", doc["publisher"])
	}
	if pub["name"] != "Kocoro" {
		t.Errorf("publisher.name = %v, want Kocoro", pub["name"])
	}
	if pub["url"] != "https://www.kocoro.ai/" {
		t.Errorf("publisher.url = %v, want https://www.kocoro.ai/", pub["url"])
	}
	logo, ok := pub["logo"].(map[string]any)
	if !ok {
		t.Fatalf("publisher.logo missing or wrong shape: %v", pub["logo"])
	}
	if logo["url"] != "https://static.kocoro.ai/logo.png" {
		t.Errorf("publisher.logo.url = %v", logo["url"])
	}
}

// TestRenderHTML_RobotsPreserved is a regression guard: the existing
// `<meta name="robots" content="noindex, nofollow">` must remain in place
// after the meta-tag injection. Social-platform OG crawlers run under
// different user-agents and read OG tags regardless of robots; conversely
// the share link shouldn't leak into Google's index, so we keep noindex.
func TestRenderHTML_RobotsPreserved(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	input := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	mustContain(t, string(html), `<meta name="robots" content="noindex, nofollow">`)
}

// TestRenderHTML_ZeroMetadataFallback verifies the in-renderer fallback: a
// caller passing Metadata = ShareMetadata{} (every existing test in this
// file, plus any future hand-rolled caller) still produces valid meta tags
// with SiteName=Kocoro / SiteURL=https://www.kocoro.ai/ rather than
// emitting empty content="" attributes.
func TestRenderHTML_ZeroMetadataFallback(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	input := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		// Metadata intentionally omitted (zero value)
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	mustContain(t, out, `<meta property="og:site_name" content="Kocoro">`)
	mustContain(t, out, `<meta name="author" content="Kocoro">`)
	mustContain(t, out, `<link rel="icon" type="image/x-icon" href="https://www.kocoro.ai/favicon.ico">`)
}

func mustContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if !strings.Contains(haystack, needle) {
		t.Fatalf("output missing %q. Sample:\n%s", needle, snippet(haystack))
	}
}

func mustNotContain(t *testing.T, haystack, needle string) {
	t.Helper()
	if strings.Contains(haystack, needle) {
		t.Fatalf("output unexpectedly contains %q. Sample:\n%s", needle, snippet(haystack))
	}
}

func snippet(s string) string {
	if len(s) <= 800 {
		return s
	}
	return s[:800] + "..."
}
