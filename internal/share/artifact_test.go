package share

import (
	"strings"
	"testing"
)

func TestSplitArtifacts_NoArtifact(t *testing.T) {
	segs := splitArtifacts("just some **markdown** text\n```html\n<div>code</div>\n```")
	if len(segs) != 1 || segs[0].IsArtifact {
		t.Fatalf("plain text + ```html should be a single markdown segment, got %+v", segs)
	}
}

func TestSplitArtifacts_Basic(t *testing.T) {
	src := "intro prose\n\n" +
		"```html-artifact title=\"My Chart\" id=art_x mime=text/html\n" +
		"<b>hi</b>\n<script>doThing()</script>\n" +
		"```\n\n" +
		"trailing prose"
	segs := splitArtifacts(src)
	if len(segs) != 3 {
		t.Fatalf("want 3 segments (prose, artifact, prose), got %d: %+v", len(segs), segs)
	}
	if segs[0].IsArtifact || !strings.Contains(segs[0].Markdown, "intro prose") {
		t.Fatalf("segment 0 should be intro prose: %+v", segs[0])
	}
	a := segs[1]
	if !a.IsArtifact {
		t.Fatalf("segment 1 should be the artifact: %+v", a)
	}
	if a.Title != "My Chart" {
		t.Errorf("title = %q, want %q", a.Title, "My Chart")
	}
	if a.MIME != "text/html" {
		t.Errorf("mime = %q, want text/html", a.MIME)
	}
	if a.ID != "art_x" {
		t.Errorf("id = %q, want art_x", a.ID)
	}
	if !strings.Contains(a.Content, "<b>hi</b>") || !strings.Contains(a.Content, "doThing()") {
		t.Errorf("content missing fragment body: %q", a.Content)
	}
	if segs[2].IsArtifact || !strings.Contains(segs[2].Markdown, "trailing prose") {
		t.Fatalf("segment 2 should be trailing prose: %+v", segs[2])
	}
}

func TestSplitArtifacts_Defaults(t *testing.T) {
	// No id, no mime: mime defaults to text/html; id stays empty (the
	// render-wide counter in textToViewBlocks assigns the unique auto-id, not
	// splitArtifacts — see TestRenderHTML_ArtifactIDsUniqueAcrossMessages).
	src := "```html-artifact title=Plain\n<p>x</p>\n```"
	segs := splitArtifacts(src)
	if len(segs) != 1 || !segs[0].IsArtifact {
		t.Fatalf("want single artifact segment, got %+v", segs)
	}
	if segs[0].MIME != "text/html" {
		t.Errorf("default mime = %q, want text/html", segs[0].MIME)
	}
	if segs[0].ID != "" {
		t.Errorf("id = %q, want empty (auto-id assigned at render level)", segs[0].ID)
	}
}

// Models often write the closing ``` with a trailing remark on the SAME line
// ("```looks good, saved locally."). markdown-it (what Desktop renders with)
// still closes the fence there; we must too, and surface the remark as prose —
// otherwise the whole widget falls back to a raw code block.
func TestSplitArtifacts_CloserWithTrailingText(t *testing.T) {
	src := "Redesigned.\n\n" +
		"```html-artifact title=\"404\" id=art_v2\n" +
		"<style>.x{color:var(--color-text-primary)}</style><div>404</div>\n" +
		"```效果满意的话，同步写入本地文件。"
	segs := splitArtifacts(src)
	var art *artifactSegment
	var proseJoined string
	for i := range segs {
		if segs[i].IsArtifact {
			art = &segs[i]
		} else {
			proseJoined += segs[i].Markdown
		}
	}
	if art == nil {
		t.Fatalf("the fence must render as an artifact, not a code block: %+v", segs)
	}
	if strings.Contains(art.Content, "```") || strings.Contains(art.Content, "效果满意") {
		t.Errorf("artifact content should be clean HTML, got: %q", art.Content)
	}
	if !strings.Contains(art.Content, "var(--color-text-primary)") {
		t.Errorf("artifact content lost: %q", art.Content)
	}
	if !strings.Contains(proseJoined, "效果满意的话，同步写入本地文件。") {
		t.Errorf("trailing remark on the closer line should surface as prose, got: %q", proseJoined)
	}
}

func TestSplitArtifacts_UnclosedFenceStaysProse(t *testing.T) {
	// A truncated transcript: opener with no closer must not swallow the rest.
	src := "before\n```html-artifact title=X\n<p>never closed"
	segs := splitArtifacts(src)
	if len(segs) != 1 || segs[0].IsArtifact {
		t.Fatalf("unclosed fence should remain prose, got %+v", segs)
	}
}

// An artifact opener inside an UNCLOSED plain code fence must not be hoisted to
// a live artifact: an unterminated fence runs to EOF as code (markdown
// semantics), so the inner opener is just part of that code body.
func TestSplitArtifacts_ArtifactInsideUnclosedPlainFenceStaysProse(t *testing.T) {
	// The plain ``` fence never gets a backticks-only closer; the inner
	// html-artifact has only a lenient closer ("```done").
	src := "```\n" +
		"```html-artifact title=X\n" +
		"<div>demo</div>\n" +
		"```done"
	segs := splitArtifacts(src)
	for i, s := range segs {
		if s.IsArtifact {
			t.Fatalf("segment %d hoisted to artifact from inside an unclosed plain fence: %+v", i, s)
		}
	}
}

func TestMatchArtifactInfo_Discrimination(t *testing.T) {
	// Input is the fence info string (text after the backticks).
	cases := map[string]bool{
		"html-artifact title=x": true,
		"html-artifact":         true,
		"html":                  false,
		"html-artifactish x":    false,
		"htmlartifact":          false,
	}
	for in, want := range cases {
		if _, got := matchArtifactInfo(in); got != want {
			t.Errorf("matchArtifactInfo(%q) = %v, want %v", in, got, want)
		}
	}
}

// A "```html-artifact" line that lives INSIDE a plain code block (the model
// teaching the fence syntax) must stay prose — not be hoisted into a live
// artifact, and not swallow the enclosing code block.
func TestSplitArtifacts_NestedInPlainFenceStaysProse(t *testing.T) {
	src := "Here's how the syntax looks:\n\n" +
		"````\n" + // outer fence (4 backticks) wrapping an example
		"```html-artifact title=\"Example\"\n" +
		"<div>demo</div>\n" +
		"```\n" +
		"````\n"
	segs := splitArtifacts(src)
	for i, s := range segs {
		if s.IsArtifact {
			t.Fatalf("segment %d was treated as a real artifact; the example fence should stay prose: %+v", i, s)
		}
	}
	// And the example text survives in the prose for source display.
	joined := ""
	for _, s := range segs {
		joined += s.Markdown
	}
	if !strings.Contains(joined, "html-artifact") || !strings.Contains(joined, "<div>demo</div>") {
		t.Fatalf("nested example content lost from prose:\n%s", joined)
	}
}

func TestBuildArtifactSrcdoc_InjectsDesktopPreset(t *testing.T) {
	out := buildArtifactSrcdoc("<p>hi</p>", "art_1")
	checks := []string{
		"<!DOCTYPE html>",
		"<p>hi</p>",                  // the fragment
		`id="kocoro-content"`,        // Desktop container
		"--color-background-primary", // host design tokens
		".c-blue",                    // SVG color classes
		"Content-Security-Policy",    // CSP meta
		"default-src 'none'",         // raw CSP (pre-template-escaping)
		"kocoro-artifact-resize",     // resize bridge protocol (matches Desktop)
		"color-scheme:light dark",    // web adaptation: keeps canvas + tokens consistent
		"#kocoro-content{box-sizing", // web adaptation: inset content from iframe edges
	}
	for _, c := range checks {
		if !strings.Contains(out, c) {
			t.Errorf("srcdoc missing %q\n--- got ---\n%s", c, out)
		}
	}
}

// The skill contract is a fragment; even if a model emits a full document we
// wrap it (matching Desktop's always-wrap behavior) rather than passing through.
func TestBuildArtifactSrcdoc_AlwaysWraps(t *testing.T) {
	out := buildArtifactSrcdoc("<!DOCTYPE html><html><body>full</body></html>", "art_1")
	if !strings.Contains(out, `id="kocoro-content"`) {
		t.Errorf("even a full-doc fragment should be wrapped in the host container")
	}
	if !strings.Contains(out, "--color-background-primary") {
		t.Errorf("host CSS should be injected regardless of fragment shape")
	}
}
