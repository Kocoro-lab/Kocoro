package tui

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/charmbracelet/glamour"
	"github.com/charmbracelet/glamour/ansi"
	"github.com/charmbracelet/glamour/styles"
)

// Matches 2+ consecutive blank-looking lines (may contain whitespace or ANSI escapes)
var blankLineRe = regexp.MustCompile(`(\n[ \t]*(\x1b\[[0-9;]*m)*[ \t]*){3,}`)

// sourcesSectionRe matches a Sources/References section at the end of a document.
// Mirrors the patterns used by shannon-desktop's stripSourcesSection.
var sourcesSectionRe = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\n---\s*\n#{1,2}\s*(?:sources?|references?|citations?|resources?|参照|参考文献|引用元)[\s\S]*$`),
	regexp.MustCompile(`(?i)\n#{1,2}\s*(?:sources?|references?|citations?|resources?|参照|参考文献|引用元)\s*\n[\s\S]*$`),
	regexp.MustCompile(`(?i)\n\*\*(?:sources?|references?|citations?|resources?|参照|参考文献|引用元)\*\*:?\s*\n[\s\S]*$`),
}

// sourceEntryRe extracts [N] title (url) from a sources block.
var sourceEntryRe = regexp.MustCompile(`\[(\d+)\]\s+(.+?)\s+\((https?://[^)]+)\)`)

type sourceEntry struct {
	idx   int
	title string
	url   string
}

// stripSourcesSection splits text into the body and the raw sources section.
// Returns (text, "") if no sources section is found.
func stripSourcesSection(text string) (body, raw string) {
	for _, re := range sourcesSectionRe {
		loc := re.FindStringIndex(text)
		if loc != nil {
			return strings.TrimRight(text[:loc[0]], "\n"), text[loc[0]:]
		}
	}
	return text, ""
}

// parseSources extracts all [N] title (url) entries from a sources block.
func parseSources(raw string) []sourceEntry {
	// Join lines so wrapped entries are parsed as one unit.
	joined := strings.ReplaceAll(raw, "\n", " ")
	matches := sourceEntryRe.FindAllStringSubmatch(joined, -1)
	entries := make([]sourceEntry, 0, len(matches))
	for _, m := range matches {
		idx, _ := strconv.Atoi(m[1])
		title := strings.TrimSpace(m[2])
		url := strings.TrimSpace(m[3])
		// Strip trailing LLM truncation markers.
		title = strings.TrimSuffix(title, " ...")
		title = strings.TrimSuffix(title, "...")
		if title == "" {
			title = url
		}
		entries = append(entries, sourceEntry{idx: idx, title: title, url: url})
	}
	return entries
}

// renderSourcesCompact renders a compact sources list with OSC 8 hyperlinks.
// Each entry is a single clickable line showing only the title.
func renderSourcesCompact(entries []sourceEntry, width int) string {
	const maxTitleRunes = 70
	const dim = "\033[38;5;243m"
	const reset = "\033[0m"

	var sb strings.Builder
	label := " Sources "
	dashes := width - len(label) - 4
	if dashes < 0 {
		dashes = 0
	}
	sb.WriteString(dim + "───" + label + strings.Repeat("─", dashes) + reset + "\n")
	for _, e := range entries {
		title := e.title
		if runes := []rune(title); len(runes) > maxTitleRunes {
			title = string(runes[:maxTitleRunes]) + "…"
		}
		link := "\033]8;;" + e.url + "\033\\" + title + "\033]8;;\033\\"
		sb.WriteString(fmt.Sprintf(dim+"  [%d]"+reset+" %s\n", e.idx, link))
	}
	return sb.String()
}

// Renderers cached by (width, dark-background). The custom compactStyle below
// hardcodes light-gray code text (#C4C4C4) and other dark-only colors that are
// invisible on a white terminal, so light backgrounds get glamour's tuned
// light style instead. Detection is warmed in New() before Bubbletea starts.
var (
	rendererCache   = map[string]*glamour.TermRenderer{}
	rendererCacheMu sync.RWMutex
)

// compactStyle is a compact style: no margins, minimal spacing,
// bold headings without color backgrounds, compact lists.
var compactStyle = ansi.StyleConfig{
	Document: ansi.StyleBlock{
		// No Color — use terminal's default foreground (white on dark backgrounds).
		// Setting an explicit color (e.g. 252) dims all text below terminal default.
		Margin: uintPtr(0),
	},
	BlockQuote: ansi.StyleBlock{
		Indent:      uintPtr(1),
		IndentToken: stringPtr("│ "),
		StylePrimitive: ansi.StylePrimitive{
			Italic: boolPtr(true),
		},
	},
	Paragraph: ansi.StyleBlock{},
	List: ansi.StyleList{
		LevelIndent: 2,
	},
	Heading: ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{
			Bold: boolPtr(true),
		},
	},
	H1: ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{
			Bold:      boolPtr(true),
			Italic:    boolPtr(true),
			Underline: boolPtr(true),
		},
	},
	H2: ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{
			Bold: boolPtr(true),
		},
	},
	H3: ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{
			Bold: boolPtr(true),
		},
	},
	H4: ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{
			Bold: boolPtr(true),
		},
	},
	H5: ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{
			Bold: boolPtr(true),
		},
	},
	H6: ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{
			Bold: boolPtr(true),
		},
	},
	Strikethrough: ansi.StylePrimitive{
		CrossedOut: boolPtr(true),
	},
	Emph: ansi.StylePrimitive{
		Italic: boolPtr(true),
	},
	Strong: ansi.StylePrimitive{
		Bold: boolPtr(true),
	},
	HorizontalRule: ansi.StylePrimitive{
		Color:  stringPtr("240"),
		Format: "--------",
	},
	Item: ansi.StylePrimitive{
		BlockPrefix: "• ",
	},
	Enumeration: ansi.StylePrimitive{
		BlockPrefix: ". ",
	},
	Task: ansi.StyleTask{
		Ticked:   "[✓] ",
		Unticked: "[ ] ",
	},
	Link: ansi.StylePrimitive{
		Color:     stringPtr("30"),
		Underline: boolPtr(true),
	},
	LinkText: ansi.StylePrimitive{
		Bold: boolPtr(true),
	},
	Image: ansi.StylePrimitive{
		Color:     stringPtr("212"),
		Underline: boolPtr(true),
	},
	Code: ansi.StyleBlock{
		StylePrimitive: ansi.StylePrimitive{
			Color: stringPtr("203"),
		},
	},
	CodeBlock: ansi.StyleCodeBlock{
		StyleBlock: ansi.StyleBlock{
			StylePrimitive: ansi.StylePrimitive{
				Color: stringPtr("244"),
			},
			Margin: uintPtr(0),
		},
		Chroma: &ansi.Chroma{
			Text:              ansi.StylePrimitive{Color: stringPtr("#C4C4C4")},
			Error:             ansi.StylePrimitive{Color: stringPtr("#F1F1F1"), BackgroundColor: stringPtr("#F05B5B")},
			Comment:           ansi.StylePrimitive{Color: stringPtr("#676767")},
			CommentPreproc:    ansi.StylePrimitive{Color: stringPtr("#FF875F")},
			Keyword:           ansi.StylePrimitive{Color: stringPtr("#00AAFF")},
			KeywordReserved:   ansi.StylePrimitive{Color: stringPtr("#FF5FD2")},
			KeywordNamespace:  ansi.StylePrimitive{Color: stringPtr("#FF5F87")},
			KeywordType:       ansi.StylePrimitive{Color: stringPtr("#6E6ED8")},
			Operator:          ansi.StylePrimitive{Color: stringPtr("#EF8080")},
			Punctuation:       ansi.StylePrimitive{Color: stringPtr("#E8E8A8")},
			Name:              ansi.StylePrimitive{Color: stringPtr("#C4C4C4")},
			NameBuiltin:       ansi.StylePrimitive{Color: stringPtr("#FF8EC7")},
			NameTag:           ansi.StylePrimitive{Color: stringPtr("#B083EA")},
			NameAttribute:     ansi.StylePrimitive{Color: stringPtr("#7A7AE6")},
			NameClass:         ansi.StylePrimitive{Color: stringPtr("#F1F1F1"), Underline: boolPtr(true), Bold: boolPtr(true)},
			NameDecorator:     ansi.StylePrimitive{Color: stringPtr("#FFFF87")},
			NameFunction:      ansi.StylePrimitive{Color: stringPtr("#00D787")},
			LiteralNumber:     ansi.StylePrimitive{Color: stringPtr("#6EEFC0")},
			LiteralString:     ansi.StylePrimitive{Color: stringPtr("#C69669")},
			LiteralStringEscape: ansi.StylePrimitive{Color: stringPtr("#AFFFD7")},
			GenericDeleted:    ansi.StylePrimitive{Color: stringPtr("#FD5B5B")},
			GenericEmph:       ansi.StylePrimitive{Italic: boolPtr(true)},
			GenericInserted:   ansi.StylePrimitive{Color: stringPtr("#00D787")},
			GenericStrong:     ansi.StylePrimitive{Bold: boolPtr(true)},
			GenericSubheading: ansi.StylePrimitive{Color: stringPtr("#777777")},
		},
	},
	Table:  ansi.StyleTable{},
}

// getRenderer returns a glamour renderer sized to the given terminal width and
// matched to the terminal background. Cached per (width, dark); rebuilt only on
// a new combination. Safe to call from multiple goroutines.
func getRenderer(width int) *glamour.TermRenderer {
	if width <= 0 {
		width = 120
	}
	dark := isDarkBackground()
	key := fmt.Sprintf("%d:%t", width, dark)

	rendererCacheMu.RLock()
	if r, ok := rendererCache[key]; ok {
		rendererCacheMu.RUnlock()
		return r
	}
	rendererCacheMu.RUnlock()

	rendererCacheMu.Lock()
	defer rendererCacheMu.Unlock()
	if r, ok := rendererCache[key]; ok {
		return r
	}
	r, err := buildRenderer(width, dark)
	if err != nil {
		return nil
	}
	rendererCache[key] = r
	return r
}

// buildRenderer constructs a glamour renderer for the given width/background.
func buildRenderer(width int, dark bool) (*glamour.TermRenderer, error) {
	if dark {
		styleJSON, err := json.Marshal(compactStyle)
		if err != nil {
			return nil, err
		}
		return glamour.NewTermRenderer(
			glamour.WithStylesFromJSONBytes(styleJSON),
			glamour.WithWordWrap(width),
		)
	}
	// Light terminal: glamour's tuned light palette (dark text on white), with
	// the document margin dropped to keep our compact look.
	light := styles.LightStyleConfig
	light.Document.Margin = uintPtr(0)
	return glamour.NewTermRenderer(
		glamour.WithStyles(light),
		glamour.WithWordWrap(width),
	)
}

// renderMarkdown renders markdown text with ANSI styling.
// Width should be the current terminal width (for correct table rendering).
// Falls back to plain text if the renderer is unavailable.
// A trailing Sources/References section is stripped from glamour and re-rendered
// as a compact OSC 8 hyperlink list (title only, URL hidden).
func renderMarkdown(text string, width int) string {
	r := getRenderer(width)
	if r == nil || text == "" {
		return text
	}

	body, sourcesRaw := stripSourcesSection(text)
	if body == "" {
		body = text
		sourcesRaw = ""
	}

	out, err := r.Render(body)
	if err != nil {
		return text
	}
	// Collapse excessive blank lines (glamour may still produce some)
	out = blankLineRe.ReplaceAllString(out, "\n\n")
	out = strings.TrimRight(out, "\n ")

	if sourcesRaw != "" {
		if entries := parseSources(sourcesRaw); len(entries) > 0 {
			out += "\n\n" + renderSourcesCompact(entries, width)
		}
	}

	return out
}

func stringPtr(s string) *string { return &s }
func uintPtr(u uint) *uint       { return &u }
func boolPtr(b bool) *bool       { return &b }
