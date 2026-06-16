package share

import (
	_ "embed"
	"strings"
)

// artifactHostCSS is the design-system preset (CSS custom properties, base
// element styles, SVG color classes) every artifact iframe gets, mirrored
// verbatim from Desktop so a shared artifact renders identically to the
// WKWebView. See templates/artifact_host.css for the provenance / sync note.
//
//go:embed templates/artifact_host.css
var artifactHostCSS string

// artifactCSP mirrors ARTIFACT_CSP in
// Kocoro Desktop's message-list.js (synced 2026-06-04).
// default-src 'none' with script/style/connect/font limited to the four read-only
// CDNs the generative-ui skill declares, and img additionally allowing
// static.kocoro.ai (conversation-produced images) — NOT widened to the other
// directives because that host accepts uploads. Combined with sandbox without
// allow-same-origin (set on the iframe element), this is the artifact's network
// boundary. Keep in sync with Desktop; see CLAUDE.md (session share).
const artifactCDNAllowlist = "https://cdnjs.cloudflare.com https://esm.sh https://cdn.jsdelivr.net https://unpkg.com"

var artifactCSP = strings.Join([]string{
	"default-src 'none'",
	"script-src 'unsafe-inline' " + artifactCDNAllowlist,
	"style-src 'unsafe-inline' " + artifactCDNAllowlist,
	"img-src data: blob: " + artifactCDNAllowlist + " https://static.kocoro.ai",
	"font-src data: " + artifactCDNAllowlist,
	"connect-src " + artifactCDNAllowlist,
	"object-src 'none'",
	"media-src 'none'",
	"form-action 'none'",
	"base-uri 'none'",
}, "; ")

// artifactSegment is one piece of an assistant text body after splitting out
// html-artifact fenced blocks. Exactly one of (Markdown) or the artifact
// fields is meaningful, keyed by IsArtifact.
type artifactSegment struct {
	IsArtifact bool

	// Markdown is the prose between/around artifacts (IsArtifact == false).
	Markdown string

	// Artifact fields (IsArtifact == true).
	Title string
	// MIME is parsed from the fence (text/html default, or image/svg+xml) for
	// contract completeness, but nothing downstream branches on it yet — SVG
	// fragments render fine through the same HTML wrapper. Reserved for a future
	// SVG-specific path, mirroring Desktop which also parses but doesn't use it.
	MIME string
	// ID is the explicit fence id, if any. The share renderer ignores it and
	// mints a render-unique data-artifact-id instead (see textToViewBlocks); kept
	// here so the parser stays a faithful, testable mirror of the fence contract.
	ID      string
	Content string // the raw fragment / document inside the fence
}

// splitArtifacts scans an assistant text body and splits it into ordered
// segments: prose markdown and html-artifact fenced blocks. An html-artifact
// fence is a markdown code fence whose info string starts with "html-artifact"
// (see internal/skills/bundled/skills/kocoro-generative-ui/SKILL.md). Only that
// explicit marker is treated as a renderable artifact — a plain ```html block
// stays inside the markdown segment and renders as source code, matching the
// Desktop client's behavior.
//
// When no artifact fence is present the result is a single markdown segment, so
// callers can use this unconditionally for any text body.
func splitArtifacts(source string) []artifactSegment {
	if !strings.Contains(source, "html-artifact") {
		return []artifactSegment{{Markdown: source}}
	}

	lines := strings.Split(source, "\n")
	var (
		segs  []artifactSegment
		prose []string
	)
	flushProse := func() {
		if len(prose) > 0 {
			segs = append(segs, artifactSegment{Markdown: strings.Join(prose, "\n")})
			prose = prose[:0]
		}
	}

	// Fence-aware scan: every fenced code block (```lang) is consumed as a whole,
	// so a "```html-artifact" line that appears INSIDE a plain code block (e.g. an
	// assistant teaching the syntax) is kept verbatim as prose, not misread as a
	// real artifact. Only a fence opener at top level whose info string is
	// html-artifact becomes a rendered artifact.
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		fence := countLeadingBackticks(trimmed)
		if fence < 3 {
			prose = append(prose, lines[i])
			i++
			continue
		}

		info := strings.TrimSpace(trimmed[fence:])

		if artInfo, ok := matchArtifactInfo(info); ok {
			// Artifact close detection is LENIENT, matching markdown-it (what
			// Desktop renders with): the closer is the first line that, trimmed,
			// starts with >= `fence` backticks — even if text follows on the same
			// line. Models routinely jam a closing remark onto the ``` line
			// ("```looks good, saved locally."); we treat the backticks as the
			// close and push the remainder back as prose after the artifact. A
			// strict "backticks only" check would miss that and the whole widget
			// would fall back to a raw <pre> code block (the reported bug).
			j := i + 1
			closed := false
			closeTrailing := ""
			for ; j < len(lines); j++ {
				ct := strings.TrimSpace(lines[j])
				if n := countLeadingBackticks(ct); n >= fence {
					closed = true
					closeTrailing = strings.TrimSpace(ct[n:])
					break
				}
			}
			if !closed {
				// No closer at all (truncated transcript / malformed): treat the
				// rest as prose to EOF, matching how a markdown renderer handles an
				// unterminated fence. Critically, this means an artifact opener can
				// never be hoisted out of an enclosing *unclosed* fence.
				prose = append(prose, lines[i:]...)
				break
			}
			flushProse()
			// id stays as the explicit fence id (may be ""); a render-unique
			// auto-id is assigned by textToViewBlocks so ids never collide across
			// messages (a per-body counter would reset and produce duplicate
			// data-artifact-id values, breaking the resize listener).
			title, mime, id := parseArtifactInfo(artInfo)
			segs = append(segs, artifactSegment{
				IsArtifact: true,
				Title:      title,
				MIME:       mime,
				ID:         id,
				Content:    strings.Join(lines[i+1:j], "\n"),
			})
			if closeTrailing != "" {
				prose = append(prose, closeTrailing)
			}
			i = j + 1
			continue
		}

		// Regular (non-artifact) fenced code block: strict close (a backticks-only
		// line), kept verbatim as prose so goldmark renders it as <pre><code>.
		// Strict here avoids mis-closing a code sample that contains a ```-prefixed
		// line.
		j := i + 1
		closed := false
		for ; j < len(lines); j++ {
			ct := strings.TrimSpace(lines[j])
			if n := countLeadingBackticks(ct); n >= fence && ct == strings.Repeat("`", n) {
				closed = true
				break
			}
		}
		if !closed {
			// Unterminated plain fence: everything to EOF is its code body (markdown
			// semantics). Swallow it as prose so a later artifact opener inside this
			// runaway fence is NOT promoted to a live artifact.
			prose = append(prose, lines[i:]...)
			break
		}
		prose = append(prose, lines[i:j+1]...)
		i = j + 1
	}
	flushProse()
	return segs
}

func countLeadingBackticks(s string) int {
	n := 0
	for n < len(s) && s[n] == '`' {
		n++
	}
	return n
}

// matchArtifactInfo reports whether a fence info string marks an html-artifact
// and, if so, returns the remainder after the "html-artifact" token. The marker
// must be the first token (so ```html and ```html-foo do not match).
func matchArtifactInfo(info string) (rest string, ok bool) {
	if !strings.HasPrefix(info, "html-artifact") {
		return "", false
	}
	after := info[len("html-artifact"):]
	if after != "" && !strings.HasPrefix(after, " ") {
		return "", false // e.g. "html-artifactish"
	}
	return strings.TrimSpace(after), true
}

// parseArtifactInfo pulls title / mime / id out of an html-artifact info string
// like: title="Q1 revenue" id=art_a1 mime=text/html theme=auto. Values may be
// double-quoted (for spaces) or bare. Unknown keys are ignored. mime defaults
// to text/html.
func parseArtifactInfo(info string) (title, mime, id string) {
	mime = "text/html"
	for _, kv := range splitInfoTokens(info) {
		eq := strings.IndexByte(kv, '=')
		if eq <= 0 {
			continue
		}
		key := kv[:eq]
		val := strings.Trim(kv[eq+1:], `"`)
		switch key {
		case "title":
			title = val
		case "mime":
			if val != "" {
				mime = val
			}
		case "id":
			id = val
		}
	}
	return title, mime, id
}

// splitInfoTokens splits an info string on spaces while keeping double-quoted
// values (which may contain spaces) intact: title="a b" id=c -> [title="a b", id=c].
func splitInfoTokens(info string) []string {
	var (
		toks  []string
		cur   strings.Builder
		inStr bool
	)
	for _, r := range info {
		switch {
		case r == '"':
			inStr = !inStr
			cur.WriteRune(r)
		case r == ' ' && !inStr:
			if cur.Len() > 0 {
				toks = append(toks, cur.String())
				cur.Reset()
			}
		default:
			cur.WriteRune(r)
		}
	}
	if cur.Len() > 0 {
		toks = append(toks, cur.String())
	}
	return toks
}

// buildArtifactSrcdoc produces the value for an <iframe srcdoc="...">. The
// returned string is a PLAIN string (not template.HTML) so html/template
// attribute-escapes it on the way into the srcdoc attribute — the browser then
// decodes it back into the iframe's document. Combined with a sandbox that
// omits allow-same-origin (set on the iframe element), the artifact runs in a
// null origin and cannot reach the share page's origin, cookies, or DOM.
//
// The wrapper mirrors buildArtifactSrcdoc in Desktop's message-list.js: the CSP
// meta, the design-system host CSS, the #kocoro-content container, and the
// resize/error bridge are all kept byte-for-byte equivalent so an artifact looks
// the same on the public share page as it does in the Desktop WKWebView. The
// model fragment is ALWAYS wrapped (the skill contract guarantees a fragment);
// we do not special-case a full <!doctype> document, matching Desktop.
//
// The only intentional web-specific addition over Desktop is the viewport meta,
// so the page is legible on mobile browsers.
func buildArtifactSrcdoc(content, id string) string {
	var b strings.Builder
	b.WriteString("<!DOCTYPE html>\n<html>\n<head>\n")
	b.WriteString(`<meta charset="utf-8">`)
	b.WriteString(`<meta name="viewport" content="width=device-width,initial-scale=1">`)
	b.WriteString(`<meta http-equiv="Content-Security-Policy" content="` + artifactCSP + `">`)
	b.WriteString("<style>" + artifactHostCSS + "</style>\n")
	// Web-specific adaptation layer (NOT in Desktop's verbatim host CSS).
	// Desktop gets a consistent theme from the native WKWebView appearance +
	// a forced :root token injection. A public share page has neither, so we
	// must make the iframe self-consistent on its own:
	//   - color-scheme: light dark — declare both so the UA canvas/scrollbars/
	//     form widgets follow the viewer's prefers-color-scheme, MATCHING the
	//     token values the host CSS @media flips. Without this, a dark-OS viewer
	//     gets dark token values (near-white text/borders) painted on the UA's
	//     default WHITE canvas → invisible text, vanished links.
	//   - solid token background so the widget always has a theme-correct surface
	//     instead of relying on the UA canvas color (Desktop body is transparent
	//     because it sits on the app's themed conversation surface).
	//   - inset the content from the iframe edges. Desktop gets this breathing
	//     room from the surrounding conversation padding; a standalone share
	//     iframe has none, so content (e.g. a wide D3 tree) would otherwise sit
	//     flush against the frame. box-sizing keeps width:100% honest under padding.
	b.WriteString("<style>:root{color-scheme:light dark}" +
		"html,body{background:var(--color-background-primary)}" +
		"#kocoro-content{box-sizing:border-box;padding:10px 18px}</style>\n")
	b.WriteString("</head>\n<body>\n")
	b.WriteString(`<main id="kocoro-content">` + "\n")
	b.WriteString(content)
	b.WriteString("\n</main>\n")
	// Resize/error bridge — verbatim mirror of Desktop's message-list.js. Reports
	// document.body.scrollHeight to the parent so the share page can size the
	// iframe to content (parent listener in session.html.tmpl). id is a controlled
	// token (parseArtifactInfo / art_N), JSON-string-escaped defensively.
	b.WriteString("<script>\n(function(){\n")
	b.WriteString("  var id=" + jsString(id) + ";\n")
	b.WriteString("  function postHeight(){\n")
	b.WriteString("    try{parent.postMessage({type:'kocoro-artifact-resize',id:id,height:document.body.scrollHeight},'*')}catch(_){}\n")
	b.WriteString("  }\n")
	b.WriteString("  function postError(m,s,l){\n")
	b.WriteString("    try{parent.postMessage({type:'kocoro-artifact-error',id:id,msg:String(m),src:String(s||''),line:l||0},'*')}catch(_){}\n")
	b.WriteString("  }\n")
	b.WriteString("  window.addEventListener('error',function(e){postError(e.message,e.filename,e.lineno)});\n")
	b.WriteString("  document.addEventListener('securitypolicyviolation',function(e){\n")
	b.WriteString("    try{parent.postMessage({type:'kocoro-artifact-csp',id:id,directive:e.violatedDirective,blockedURI:e.blockedURI},'*')}catch(_){}\n")
	b.WriteString("  });\n")
	b.WriteString("  if(document.body){\n")
	b.WriteString("    try{new ResizeObserver(postHeight).observe(document.body)}catch(_){}\n")
	b.WriteString("  }\n")
	b.WriteString("  setTimeout(postHeight,0);\n")
	b.WriteString("  setTimeout(postHeight,60);\n")
	b.WriteString("  setTimeout(postHeight,300);\n")
	b.WriteString("})();\n</script>\n")
	b.WriteString("</body>\n</html>")
	return b.String()
}

// jsString returns a double-quoted JS string literal for a controlled token
// (artifact id). Besides the usual string escapes, "<" is escaped to "\x3C" so
// a "</script>" sequence in the id can never close the wrapper's <script> early.
func jsString(s string) string {
	r := strings.NewReplacer(
		`\`, `\\`,
		`"`, `\"`,
		"\n", `\n`,
		"\r", `\r`,
		"<", `\x3C`,
	)
	return `"` + r.Replace(s) + `"`
}
