package share

import (
	"encoding/json"
	"html/template"
	"regexp"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// Social-meta sizing knobs. Numbers chosen to sit safely under platform
// caps (X capped Twitter cards at ~70/200; WeChat trims to ~30/100;
// Facebook recommends ≤95 for og:title). 60/150 is a comfortable middle
// that displays in full on every target we care about while still leaving
// a readable amount of content on platforms that don't truncate.
const (
	ogTitleMaxRunes       = 60
	ogDescriptionMaxRunes = 150
)

// stripMarkdownForOG removes markdown syntax from Haiku summary text so the
// raw output we hand to og:description is human-readable prose, not the
// model's hash/asterisk/list-marker scaffolding.
//
// Goldmark-rendered HTML is the wrong source for this — escaping it back to
// text loses paragraph boundaries and produces "Heading1Body1Heading2..."
// run-ons. Strip from the markdown source instead, keeping a tight set of
// transformations that cover what Haiku actually emits (we're not trying to
// be a complete CommonMark renderer).
func stripMarkdownForOG(s string) string {
	if s == "" {
		return ""
	}
	// Drop fenced code blocks wholesale — they're machine output, not prose,
	// and including them in og:description costs the user-visible budget for
	// no descriptive value. Handles both ```lang\n…``` and ~~~ forms.
	s = reFencedCode.ReplaceAllString(s, " ")
	// Inline code → bare text.
	s = reInlineCode.ReplaceAllString(s, "$1")
	// Heading marker at start of line: `# `, `## `, ... → drop the prefix,
	// keep the heading text inline.
	s = reHeading.ReplaceAllString(s, "")
	// Bold/italic emphasis: `**x**`, `__x__`, `*x*`, `_x_` → x.
	s = reEmphasisStrong.ReplaceAllString(s, "$1")
	s = reEmphasisItalic.ReplaceAllString(s, "$1")
	// Links: `[text](url)` → text. Image syntax `![alt](url)` similarly.
	s = reMarkdownLink.ReplaceAllString(s, "$1")
	// Bullet / ordered list markers at line start.
	s = reListMarker.ReplaceAllString(s, "")
	// Blockquote markers.
	s = reBlockquote.ReplaceAllString(s, "")
	// Collapse runs of whitespace (incl. newlines) to single spaces — og
	// description renders inline anyway, embedded newlines just bloat.
	s = reWhitespace.ReplaceAllString(s, " ")
	return strings.TrimSpace(s)
}

var (
	// Fenced code: opening ``` (or ~~~) up to closing on its own line. The
	// non-greedy [\s\S]*? handles the cross-newline span without (?s).
	reFencedCode     = regexp.MustCompile("(?m)^(?:```|~~~)[^\n]*\n[\\s\\S]*?\n(?:```|~~~)[ \t]*$")
	reInlineCode     = regexp.MustCompile("`([^`]+)`")
	reHeading        = regexp.MustCompile(`(?m)^#{1,6}[ \t]+`)
	reEmphasisStrong = regexp.MustCompile(`(?:\*\*|__)([^*_\n]+?)(?:\*\*|__)`)
	reEmphasisItalic = regexp.MustCompile(`(?:\*|_)([^*_\n]+?)(?:\*|_)`)
	reMarkdownLink   = regexp.MustCompile(`!?\[([^\]]*)\]\([^)]*\)`)
	reListMarker     = regexp.MustCompile(`(?m)^[ \t]*(?:[-*+]|\d+\.)[ \t]+`)
	reBlockquote     = regexp.MustCompile(`(?m)^[ \t]*>[ \t]*`)
	reWhitespace     = regexp.MustCompile(`\s+`)
)

// buildShareDescription produces the plain-text body that feeds og:description /
// twitter:description / <meta name="description">. Preference order matches
// fallbackSummary() but operates on the post-H1-split summary body:
//
//  1. The de-markdowned Haiku summary body (best quality)
//  2. The session.Title (user's first prompt verbatim — still meaningful)
//  3. The first user message in the transcript, truncated
//  4. Empty (template skips the meta tags)
//
// Returned text is not yet length-bounded; caller passes it through
// truncateOGDescription.
func buildShareDescription(summaryBody, sessionTitle string, msgs []client.Message) string {
	if s := strings.TrimSpace(stripMarkdownForOG(summaryBody)); s != "" {
		return s
	}
	if s := strings.TrimSpace(sessionTitle); s != "" {
		return s
	}
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		if s := strings.TrimSpace(m.Content.Text()); s != "" {
			return s
		}
	}
	return ""
}

// truncateOGTitle / truncateOGDescription are rune-safe ellipsis truncators
// for the two social-meta length budgets. Unicode safety matters: a byte
// truncation that splits a CJK codepoint would leave a "�" replacement char
// in the og:title that Slack and WeChat both render as a missing-glyph box.
func truncateOGTitle(s string) string       { return truncateWithEllipsis(s, ogTitleMaxRunes) }
func truncateOGDescription(s string) string { return truncateWithEllipsis(s, ogDescriptionMaxRunes) }

func truncateWithEllipsis(s string, maxRunes int) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	// Trim trailing punctuation/whitespace before the ellipsis so we don't
	// end up with "hello,…" or "hello …" — same trim-set as truncatePageTitle
	// in renderer.go for consistency across the two truncation paths.
	return strings.TrimRight(string(r[:maxRunes]), " \t,，。、;；:：") + "…"
}

// buildJSONLD constructs the schema.org Article body for the
// <script type="application/ld+json"> block. Marshaling through encoding/json
// guarantees correct escaping of quotes / backslashes / `<`/`>`/`&` (Go's
// default Marshal HTMLEscapes the latter three), so a Haiku-generated title
// containing literal `</script>` cannot break out of the script tag.
//
// The returned value is template.JS so html/template emits it verbatim. This
// is safe specifically because json.Marshal already produced safe-to-embed
// output; passing arbitrary strings as template.JS is generally unsafe.
func buildJSONLD(d viewData, in RenderInput) template.JS {
	publisher := map[string]any{
		"@type": "Organization",
		"name":  d.SiteName,
		"url":   d.SiteURL,
	}
	if d.LogoURL != "" {
		publisher["logo"] = map[string]any{
			"@type": "ImageObject",
			"url":   d.LogoURL,
		}
	}

	doc := map[string]any{
		"@context":   "https://schema.org",
		"@type":      "Article",
		"name":       d.Title,
		"headline":   d.OGTitle,
		"publisher":  publisher,
		"inLanguage": d.Lang,
	}
	if d.Description != "" {
		doc["description"] = d.Description
	}
	if d.OGImage != "" {
		doc["image"] = d.OGImage
	}
	if in.Session != nil {
		if !in.Session.CreatedAt.IsZero() {
			doc["dateCreated"] = d.CreatedAtISO
			doc["datePublished"] = d.CreatedAtISO
		}
	}
	if !in.GeneratedAt.IsZero() {
		doc["dateModified"] = d.GeneratedAtISO
	}

	b, err := json.Marshal(doc)
	if err != nil {
		// Marshal of a map[string]any with string-only leaf values cannot
		// fail under normal Go behaviour. If it ever does, emit an empty
		// object so the <script> tag stays valid JSON rather than producing
		// a hard-to-debug parse error in social-platform validators.
		return template.JS("{}")
	}
	return template.JS(b)
}
