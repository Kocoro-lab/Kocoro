package share

import (
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// TestStripMarkdownForOG covers each markdown construct stripMarkdownForOG
// is supposed to handle. The function operates on Haiku-generated summary
// text destined for og:description, so the bar is "produces clean prose
// for social card preview", not "is a complete markdown renderer".
func TestStripMarkdownForOG(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "empty input",
			in:   "",
			want: "",
		},
		{
			name: "plain prose untouched (modulo whitespace collapse)",
			in:   "Just a normal sentence with no markdown.",
			want: "Just a normal sentence with no markdown.",
		},
		{
			name: "ATX headings dropped, text kept",
			in:   "## Section\nbody text",
			want: "Section body text",
		},
		{
			name: "strong emphasis stripped",
			in:   "This **is bold** prose.",
			want: "This is bold prose.",
		},
		{
			name: "underscore strong emphasis stripped",
			in:   "This __also bold__ prose.",
			want: "This also bold prose.",
		},
		{
			name: "italic emphasis stripped",
			in:   "Some *italic* text.",
			want: "Some italic text.",
		},
		{
			name: "underscore italic stripped",
			in:   "Some _italic_ text.",
			want: "Some italic text.",
		},
		{
			name: "inline code unwrapped",
			in:   "Use `os.Rename` for atomic moves.",
			want: "Use os.Rename for atomic moves.",
		},
		{
			name: "fenced code block removed wholesale",
			in:   "Before\n```go\nfmt.Println(\"hi\")\n```\nAfter",
			want: "Before After",
		},
		{
			name: "tilde fenced code block removed",
			in:   "Before\n~~~\nx = 1\n~~~\nAfter",
			want: "Before After",
		},
		{
			name: "markdown link keeps text drops url",
			in:   "See [the docs](https://example.com/page) for details.",
			want: "See the docs for details.",
		},
		{
			name: "image syntax keeps alt drops url",
			in:   "Diagram: ![architecture](https://example.com/a.png) below.",
			want: "Diagram: architecture below.",
		},
		{
			name: "bullet list markers removed",
			in:   "- first\n- second\n- third",
			want: "first second third",
		},
		{
			name: "ordered list markers removed",
			in:   "1. one\n2. two\n3. three",
			want: "one two three",
		},
		{
			name: "blockquote markers removed",
			in:   "> quoted line\n> second line",
			want: "quoted line second line",
		},
		{
			name: "multiple newlines collapsed",
			in:   "first\n\n\n\nsecond",
			want: "first second",
		},
		{
			name: "mixed Haiku-style summary",
			in:   "## Problem\nPayment **callbacks** were dropping orders.\n\n## Fix\n1. Added idempotency table\n2. Dedupe by `order_id`",
			want: "Problem Payment callbacks were dropping orders. Fix Added idempotency table Dedupe by order_id",
		},
		{
			name: "CJK content preserved",
			in:   "## 问题\n旧的回调链路在**并发**下偶发丢单。",
			want: "问题 旧的回调链路在并发下偶发丢单。",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := stripMarkdownForOG(tc.in)
			if got != tc.want {
				t.Errorf("stripMarkdownForOG(%q)\n  got  %q\n  want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestBuildShareDescription walks the fallback chain: stripped summary →
// session title → first user message → empty. The fallback order matters
// for the social card: a Haiku summary produces the highest-quality OG
// description; only when it's unavailable do we degrade to less-curated
// sources.
func TestBuildShareDescription(t *testing.T) {
	makeMsgs := func(role, text string) []client.Message {
		return []client.Message{{Role: role, Content: client.NewTextContent(text)}}
	}

	t.Run("uses stripped summary when available", func(t *testing.T) {
		got := buildShareDescription("**bold** body", "session title", makeMsgs("user", "first msg"))
		if got != "bold body" {
			t.Errorf("expected stripped summary, got %q", got)
		}
	})

	t.Run("falls back to session title when summary empty", func(t *testing.T) {
		got := buildShareDescription("", "session title", makeMsgs("user", "first msg"))
		if got != "session title" {
			t.Errorf("expected session title fallback, got %q", got)
		}
	})

	t.Run("falls back to first user message when summary and title empty", func(t *testing.T) {
		got := buildShareDescription("", "", makeMsgs("user", "what's the weather"))
		if got != "what's the weather" {
			t.Errorf("expected first user message fallback, got %q", got)
		}
	})

	t.Run("skips assistant messages in fallback", func(t *testing.T) {
		msgs := []client.Message{
			{Role: "assistant", Content: client.NewTextContent("assistant first")},
			{Role: "user", Content: client.NewTextContent("user second")},
		}
		got := buildShareDescription("", "", msgs)
		if got != "user second" {
			t.Errorf("expected user message, got %q", got)
		}
	})

	t.Run("returns empty when nothing usable", func(t *testing.T) {
		got := buildShareDescription("   ", "  ", nil)
		if got != "" {
			t.Errorf("expected empty fallback, got %q", got)
		}
	})

	t.Run("whitespace-only summary falls through to title", func(t *testing.T) {
		// A summary that strips down to nothing but whitespace should not
		// pre-empt the title fallback.
		got := buildShareDescription("   \n\n  ", "real title", nil)
		if got != "real title" {
			t.Errorf("expected title when summary is whitespace, got %q", got)
		}
	})
}

// TestTruncateWithEllipsis covers the rune-safe truncation primitive used
// for both og:title and og:description. The CJK case is the load-bearing
// one — a byte-truncating implementation would leave a "�" replacement
// char in the middle of the og:title that Slack and WeChat render as a
// missing-glyph box.
func TestTruncateWithEllipsis(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		maxRunes int
		want     string
	}{
		{name: "empty stays empty", in: "", maxRunes: 60, want: ""},
		{name: "whitespace-only stays empty", in: "   ", maxRunes: 60, want: ""},
		{name: "under-limit untouched", in: "short", maxRunes: 60, want: "short"},
		{name: "exactly at limit untouched", in: strings.Repeat("a", 60), maxRunes: 60, want: strings.Repeat("a", 60)},
		{name: "over limit gets ellipsis", in: strings.Repeat("a", 100), maxRunes: 60, want: strings.Repeat("a", 60) + "…"},
		{
			name:     "CJK rune-safe truncate",
			in:       strings.Repeat("中", 80), // 80 runes, 240 bytes
			maxRunes: 60,
			want:     strings.Repeat("中", 60) + "…",
		},
		{
			name:     "trailing punctuation trimmed before ellipsis",
			in:       strings.Repeat("a", 58) + ", more text",
			maxRunes: 60,
			want:     strings.Repeat("a", 58) + "…", // ", " trimmed
		},
		{
			// Input: 59中 + "，" + " more". Take first 60 runes →
			// 59中 + "，". TrimRight strips the trailing "，" (it's in
			// the CJK punctuation trim-set), leaving 59中 + "…".
			name:     "trailing CJK comma at rune 60 trimmed",
			in:       strings.Repeat("中", 59) + "， more",
			maxRunes: 60,
			want:     strings.Repeat("中", 59) + "…",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := truncateWithEllipsis(tc.in, tc.maxRunes)
			if got != tc.want {
				t.Errorf("\n  got  %q\n  want %q", got, tc.want)
			}
		})
	}
}

// TestRenderHTML_SiteURLAutoSlash pins the favicon-path-correctness fix:
// an operator-configured SiteURL without a trailing slash must still
// produce a valid favicon href. The risk is silent — favicon 404s don't
// surface in the UI, but they do produce noisy console errors on every
// share-page load.
func TestRenderHTML_SiteURLAutoSlash(t *testing.T) {
	sess := &session.Session{ID: "s1", Title: "t", CreatedAt: time.Now()}
	input := RenderInput{
		Session:  sess,
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Metadata: ShareMetadata{
			SiteName: "Acme",
			SiteURL:  "https://acme.example.com", // no trailing slash
		},
	}
	html, err := RenderHTML(input)
	if err != nil {
		t.Fatalf("RenderHTML: %v", err)
	}
	out := string(html)
	// Concatenation must produce a single "/" between host and "favicon.ico".
	mustContain(t, out, `<link rel="icon" type="image/x-icon" href="https://acme.example.com/favicon.ico">`)
	if strings.Contains(out, `comfavicon.ico`) {
		t.Fatalf("missing trailing slash on SiteURL produced invalid favicon path:\n%s", snippet(out))
	}
}
