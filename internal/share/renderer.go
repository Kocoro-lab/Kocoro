package share

import (
	"bytes"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// templateFS embeds the share-page template. Single template, no partials —
// keep the asset tree flat so the package has nothing runtime-loadable.
//
//go:embed templates/session.html.tmpl
var templateFS embed.FS

// sessionTemplate is parsed once at package load. Reparsing on every share
// would needlessly compile the same static asset against the same FS bytes
// for every request.
var sessionTemplate = template.Must(template.ParseFS(templateFS, "templates/session.html.tmpl"))

// allowedImageMediaTypes restricts data: URIs to known image MIME types.
// Without this restriction a malicious or buggy upstream that controlled
// ImageSource.MediaType could synthesize a "data:javascript;…" URI; here
// we fail closed and skip rendering instead.
var allowedImageMediaTypes = map[string]bool{
	"image/png":  true,
	"image/jpeg": true,
	"image/gif":  true,
	"image/webp": true,
}

// RenderInput is the contract between share.Render and the daemon endpoint.
// Pass sanitized messages — the renderer trusts its input.
type RenderInput struct {
	Session     *session.Session
	Messages    []client.Message      // already sanitized
	Meta        []session.MessageMeta // already sanitized, aligned with Messages
	Summary     string                // Haiku-generated; empty when generation failed
	GeneratedAt time.Time
}

// RenderHTML produces the full HTML page bytes for sharing. The returned bytes
// are self-contained — all CSS is inlined, all images embedded as data URIs —
// and ready to upload as-is.
func RenderHTML(input RenderInput) ([]byte, error) {
	if input.Session == nil {
		return nil, fmt.Errorf("share: nil session")
	}

	data := buildViewData(input)

	var buf bytes.Buffer
	if err := sessionTemplate.Execute(&buf, data); err != nil {
		return nil, fmt.Errorf("share: execute template: %w", err)
	}
	return buf.Bytes(), nil
}

// viewData is the top-level template payload.
//
// Timestamp strategy: every visible date is rendered as a <time datetime=ISO>
// element with a UTC fallback text. Client-side JavaScript in the template
// rewrites each <time> element to the viewer's browser timezone + locale on
// load, so a Tokyo viewer sees JST and a London viewer sees BST off the same
// HTML. UTC is the fallback (rather than the daemon operator's local CST/MST)
// so a JS-disabled viewer at least sees a consistent, unambiguous timezone
// instead of "what wall-clock did the daemon happen to be in".
type viewData struct {
	Lang           string
	Title          string
	AgentName      string
	SummaryHTML    template.HTML // markdown-rendered, pre-escaped; empty template.HTML is falsy in {{if}}
	CreatedAt      string        // UTC fallback text ("2026-05-15 17:34 UTC")
	CreatedAtISO   string        // RFC3339 for <time datetime>, used by the client-side localizer
	GeneratedAt    string        // UTC fallback text ("2026-05-18 08:11 UTC")
	GeneratedAtISO string        // RFC3339, parallel to CreatedAtISO
	MessageCount   int
	Messages       []viewMessage
}

// viewMessage represents a single rendered message card. Role drives the
// visual treatment: "user" → right-aligned bubble for text/image (tool blocks
// rendered below); "assistant" → full-width with accent dot; "tool" → for
// messages that arrived under role=user but contain ONLY tool_result blocks
// (Anthropic's API shape — those are agent-loop artifacts, not user input).
type viewMessage struct {
	Role         string // "user" / "assistant" / "tool"
	RoleLabel    string
	Timestamp    string
	TimestampISO string
	// BubbleBlocks are rendered inside the user bubble (text/image only).
	// AsideBlocks are rendered outside the bubble (tool_use/tool_result).
	// For assistant/tool roles, all renderable blocks go to AsideBlocks and
	// the bubble is suppressed by the template.
	BubbleBlocks []viewBlock
	AsideBlocks  []viewBlock
}

// viewBlock is the rendered form of one content block. Only the field(s)
// relevant to the Kind are populated; the template switches on Kind.
type viewBlock struct {
	Kind string // "text" / "image" / "tool_use" / "tool_result"
	// HTML carries markdown-rendered content for text blocks. Used for both
	// user and assistant text — most user inputs are short plain prose that
	// markdown leaves untouched, and assistant output benefits from heading/
	// list/code-fence formatting.
	HTML              template.HTML
	ImageDataURI      template.URL
	ToolName          string
	ToolInput         string // pretty-printed JSON for <pre>
	ToolResultText    string // raw terminal-style output for <pre>
	ToolResultIsError bool
	NestedImages      []template.URL
}

func buildViewData(in RenderInput) viewData {
	sess := in.Session

	// Prefer the H1 line at the top of the Haiku summary as the page title —
	// it's a hand-shaped 5-10 word distillation that fits the H1 visual
	// budget. sess.Title is typically the user's first prompt verbatim and
	// can run several sentences long; falling back to a hard rune truncation
	// is much better than letting CSS clip a multi-sentence header.
	pageTitle, summaryBody := splitPageTitleFromSummary(strings.TrimSpace(in.Summary), sess.Title)
	if pageTitle == "" {
		pageTitle = "Untitled session"
	}

	// Render both timestamps in UTC for the static text. Client-side JS in
	// the template rewrites these to the viewer's local timezone + locale
	// (see <script id="localize-timestamps"> at the bottom of the template);
	// the UTC text is the fallback when JS is disabled or fails. Critically,
	// the SERVER's local zone (CST, MST, ...) is NOT used here — that would
	// leak the daemon operator's wall-clock to every share viewer and
	// produce the "header CST + footer UTC" mismatch this comment was
	// written to prevent.
	created, createdISO := "", ""
	if !sess.CreatedAt.IsZero() {
		created = sess.CreatedAt.UTC().Format("2006-01-02 15:04 UTC")
		createdISO = sess.CreatedAt.UTC().Format(time.RFC3339)
	}

	generated, generatedISO := "", ""
	if !in.GeneratedAt.IsZero() {
		generated = in.GeneratedAt.UTC().Format("2006-01-02 15:04 UTC")
		generatedISO = in.GeneratedAt.UTC().Format(time.RFC3339)
	}

	// Compute renderedMessages first so the header count reflects what the
	// reader actually sees (a message whose every block was unrenderable is
	// skipped downstream — promising "N messages" and then showing fewer
	// would be confusing).
	rendered := buildViewMessages(in.Messages, in.Meta)
	return viewData{
		Lang:           "en",
		Title:          pageTitle,
		AgentName:      agentNameFromSession(sess),
		SummaryHTML:    renderMarkdown(summaryBody),
		CreatedAt:      created,
		CreatedAtISO:   createdISO,
		GeneratedAt:    generated,
		GeneratedAtISO: generatedISO,
		MessageCount:   len(rendered),
		Messages:       rendered,
	}
}

// pageTitleMaxRunes caps the rune length of the page <title> and the
// header h1. 50 runes accommodates a typical CJK heading or ~6-9 English
// words at the chosen header font-size without wrapping awkwardly.
const pageTitleMaxRunes = 50

// splitPageTitleFromSummary picks a page title from (1) the leading "# H1"
// of the Haiku summary, falling back to (2) sess.Title truncated. If the
// H1 is consumed, it's stripped from the returned summary body so the
// share page doesn't show the same heading twice (once as <h1>, once as
// the first rendered summary heading).
func splitPageTitleFromSummary(summary, sessionTitle string) (title, body string) {
	if h1, rest, ok := splitLeadingH1(summary); ok {
		return truncatePageTitle(h1), rest
	}
	return truncatePageTitle(sessionTitle), summary
}

// splitLeadingH1 returns the text of a leading "# ..." line if present
// (matching both "# heading" and "# heading\n..." forms) along with the
// remaining markdown body. Returns ok=false when the input doesn't start
// with a level-1 ATX heading.
func splitLeadingH1(s string) (h1, rest string, ok bool) {
	if !strings.HasPrefix(s, "# ") {
		return "", s, false
	}
	line, after, _ := strings.Cut(s[2:], "\n")
	return strings.TrimSpace(line), strings.TrimLeft(after, "\n"), true
}

// truncatePageTitle clips a candidate title to pageTitleMaxRunes runes,
// appending an ellipsis when truncation actually happens. Rune-safe so
// CJK input never gets cut mid-codepoint.
//
// The trailing trim set covers BOTH ASCII (`, ; : .`) and CJK (`，；：、。`)
// punctuation so a Haiku summary in either language doesn't leave an
// orphaned half-clause comma right before the ellipsis ("hello,…").
func truncatePageTitle(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	r := []rune(s)
	if len(r) <= pageTitleMaxRunes {
		return s
	}
	return strings.TrimRight(string(r[:pageTitleMaxRunes]), " \t,，。、;；:：") + "…"
}

// agentNameFromSession returns a display string for the agent. We don't have
// a dedicated field on Session, so fall back to a directory hint embedded in
// CWD when available — otherwise empty (template hides the label).
func agentNameFromSession(sess *session.Session) string {
	// Sessions stored under ~/.shannon/agents/<name>/sessions/ have CWD-
	// independent identity, but the agent name is not currently persisted on
	// the Session struct itself. The Desktop UI knows the routing context
	// when calling /sessions/{id}/share?agent=X, but that doesn't reach here.
	// Leaving blank for now — caller can set Session.Title to include it.
	return ""
}

func buildViewMessages(msgs []client.Message, meta []session.MessageMeta) []viewMessage {
	out := make([]viewMessage, 0, len(msgs))
	for i, m := range msgs {
		var mm session.MessageMeta
		if i < len(meta) {
			mm = meta[i]
		}

		role := m.Role
		if role != "user" && role != "assistant" {
			role = "system"
		}

		blocks := buildViewBlocks(m)
		if len(blocks) == 0 {
			continue
		}
		bubble, aside := partitionForBubble(blocks)

		// Pure tool_result-bearing "user" messages are Anthropic-API artifacts
		// (the next turn's reply to an assistant tool_use). They aren't actual
		// user typing — re-tag visually so they don't get a user-typed bubble.
		if role == "user" && len(bubble) == 0 && len(aside) > 0 {
			role = "tool"
		}

		vm := viewMessage{
			Role:         role,
			RoleLabel:    roleLabel(role),
			BubbleBlocks: bubble,
			AsideBlocks:  aside,
		}
		if mm.Timestamp != nil && !mm.Timestamp.IsZero() {
			// Same UTC-only fallback policy as the header/footer timestamps —
			// the JS localizer rewrites this to viewer-local on load, but if
			// JS is disabled a viewer must not see the daemon operator's
			// wall-clock hour/minute and mistake it for their own timezone.
			// "UTC" suffix is dropped to keep message-row chrome compact;
			// the data-fmt="time" attribute the template sets disambiguates
			// for the localizer.
			vm.Timestamp = mm.Timestamp.UTC().Format("15:04 UTC")
			vm.TimestampISO = mm.Timestamp.UTC().Format(time.RFC3339)
		}
		out = append(out, vm)
	}
	return out
}

// partitionForBubble splits the blocks into the bubble portion (text + image,
// the kinds that represent direct conversation content) and the aside portion
// (tool_use + tool_result, rendered outside the bubble as collapsible details).
func partitionForBubble(blocks []viewBlock) (bubble, aside []viewBlock) {
	for _, b := range blocks {
		switch b.Kind {
		case "text", "image":
			bubble = append(bubble, b)
		default:
			aside = append(aside, b)
		}
	}
	return bubble, aside
}

func roleLabel(role string) string {
	switch role {
	case "user":
		return "User"
	case "assistant":
		return "Assistant"
	case "tool":
		return "Tool"
	default:
		return strings.Title(role) //nolint:staticcheck — role is ASCII, deprecation note doesn't bite.
	}
}

func buildViewBlocks(m client.Message) []viewBlock {
	if !m.Content.HasBlocks() {
		text := m.Content.Text()
		if strings.TrimSpace(text) == "" {
			return nil
		}
		return []viewBlock{{Kind: "text", HTML: renderMarkdown(text)}}
	}

	out := make([]viewBlock, 0, len(m.Content.Blocks()))
	for _, b := range m.Content.Blocks() {
		vb, ok := renderBlock(b)
		if !ok {
			continue
		}
		out = append(out, vb)
	}
	return out
}

func renderBlock(b client.ContentBlock) (viewBlock, bool) {
	switch b.Type {
	case "text":
		if strings.TrimSpace(b.Text) == "" {
			return viewBlock{}, false
		}
		return viewBlock{Kind: "text", HTML: renderMarkdown(b.Text)}, true

	case "image":
		uri, ok := imageDataURI(b.Source)
		if !ok {
			return viewBlock{}, false
		}
		return viewBlock{Kind: "image", ImageDataURI: uri}, true

	case "tool_use":
		return viewBlock{
			Kind:      "tool_use",
			ToolName:  b.Name,
			ToolInput: prettyToolInput(b.Input),
		}, true

	case "tool_result":
		text, images := flattenToolResult(b.ToolContent)
		// Tool results stay as raw <pre> text (terminal-style) rather than
		// markdown — they're usually CLI / file dumps and shouldn't grow
		// headings or bold from accidental hash/asterisk characters.
		return viewBlock{
			Kind:              "tool_result",
			ToolResultText:    text,
			NestedImages:      images,
			ToolResultIsError: b.IsError,
		}, true

	default:
		return viewBlock{}, false
	}
}

func imageDataURI(src *client.ImageSource) (template.URL, bool) {
	if src == nil || src.Data == "" {
		return "", false
	}
	mt := strings.ToLower(strings.TrimSpace(src.MediaType))
	if !allowedImageMediaTypes[mt] {
		return "", false
	}
	// template.URL bypasses html/template's URL sanitizer. Safe here because:
	//   (a) the "data:" prefix is hardcoded, so a malicious src.MediaType
	//       can't produce a javascript: URI;
	//   (b) MediaType is whitelisted above to a fixed set of image MIME types;
	//   (c) src.Data is base64 — at worst a malformed image, not script.
	return template.URL("data:" + mt + ";base64," + src.Data), true
}

func prettyToolInput(raw json.RawMessage) string {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) || bytes.Equal(trimmed, []byte("{}")) {
		return ""
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		return string(trimmed)
	}
	pretty, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return string(trimmed)
	}
	return string(pretty)
}

// flattenToolResult converts a sanitized tool_result ToolContent into the
// renderer's two-channel view: a concatenated text body and an ordered list
// of nested images. Accepts the three shapes sanitizeToolResultContent emits:
// nil, string, []ContentBlock.
func flattenToolResult(content any) (string, []template.URL) {
	switch t := content.(type) {
	case nil:
		return "", nil
	case string:
		return t, nil
	case []client.ContentBlock:
		var sb strings.Builder
		var images []template.URL
		for _, child := range t {
			switch child.Type {
			case "text":
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(child.Text)
			case "image":
				if uri, ok := imageDataURI(child.Source); ok {
					images = append(images, uri)
				}
			}
		}
		return sb.String(), images
	default:
		// Future shape; surface its JSON form rather than panicking.
		if data, err := json.Marshal(content); err == nil {
			return string(data), nil
		}
		return "", nil
	}
}
