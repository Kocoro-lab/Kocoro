package share

import (
	"bytes"
	"html/template"
	"sync"

	"github.com/yuin/goldmark"
	"github.com/yuin/goldmark/extension"
	"github.com/yuin/goldmark/parser"
	"github.com/yuin/goldmark/renderer/html"
)

// md is the package-wide markdown converter. Lazy-init via sync.Once so the
// regex/extension wiring happens at most once per process. We do NOT enable
// html.WithUnsafe() — raw <script>, <iframe>, on* handlers in the input are
// escaped, not rendered. This is the load-bearing safety property for share
// pages: a malicious tool-result text or a model trying to inject HTML lands
// as escaped characters, not active markup.
var (
	mdOnce sync.Once
	md     goldmark.Markdown
)

func mdEngine() goldmark.Markdown {
	mdOnce.Do(func() {
		md = goldmark.New(
			goldmark.WithExtensions(
				extension.GFM, // tables, strikethrough, autolink, task lists
			),
			goldmark.WithParserOptions(
				parser.WithAutoHeadingID(),
			),
			goldmark.WithRendererOptions(
				html.WithHardWraps(),
				// html.WithXHTML() / html.WithUnsafe() deliberately omitted.
			),
		)
	})
	return md
}

// renderMarkdown converts markdown source into HTML. The result is marked
// template.HTML so callers can drop it into html/template output without
// double-escaping. Raw HTML inside the input is escaped (see mdEngine
// comment) — the only HTML produced here comes from markdown constructs.
//
// On any parser/renderer error the original source is returned escaped via
// template.HTMLEscapeString so we never spill markdown source unchanged into
// the page (which could include angle brackets parsed as tags by browsers).
func renderMarkdown(source string) template.HTML {
	if source == "" {
		return ""
	}
	var buf bytes.Buffer
	if err := mdEngine().Convert([]byte(source), &buf); err != nil {
		return template.HTML(template.HTMLEscapeString(source))
	}
	return template.HTML(buf.String())
}
