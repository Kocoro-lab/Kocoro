// Package share renders Kocoro sessions into self-contained HTML suitable
// for publishing via the cloud uploads API. Two pieces collaborate here:
//
//   - Sanitize strips sensitive content (paths, thinking blocks, system
//     reminders, env-var-looking secrets, document attachments) from
//     a session's message stream.
//   - Render takes sanitized messages plus a Haiku-generated summary
//     and produces a single static HTML page with images inlined as data
//     URIs.
//
// The daemon endpoint POST /sessions/{id}/share wires both pieces to the
// uploads client and returns a public CDN URL.
package share

import (
	"bytes"
	"encoding/json"
	"regexp"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// maxToolResultChars caps the size of any single tool_result text shown in
// the shared HTML. Larger than typical CLI output, small enough that a
// 50K bash dump can't dominate the page or leak entire files.
const maxToolResultChars = 4096

// maxToolInputChars caps tool_use input JSON length the same way.
const maxToolInputChars = 1024

// Pre-compiled regexes. Anchor to recognizable shapes so we redact aggressively
// inside text/tool args while leaving prose alone.
var (
	// `<system-reminder>…</system-reminder>` — daemon-injected guardrails the
	// model sees but the shared audience must not. (?s) makes . match \n.
	reSystemReminder = regexp.MustCompile(`(?s)<system-reminder>.*?</system-reminder>\s*`)

	// `/Users/<name>/.shannon/tmp/attachments/<nonce>/<basename>` or the same
	// path after the home replacement has fired. Replaced with `[attachment: basename]`.
	reAttachmentPath = regexp.MustCompile(`(?:/Users/[^/\s]+|/home/[^/\s]+|~)/\.shannon/tmp/attachments/[a-f0-9]+/([^\s"'\]]+)`)

	// `[User attached <kind>: <name> ...]` daemon-emitted hints — the bracketed
	// envelope is fine to keep but we strip the path so the receiver can't see
	// the sender's filesystem layout.
	reFileHint = regexp.MustCompile(`\[User attached ([^:]+): ([^\s\]]+)[^\]]*\]`)

	// Per-user absolute paths. After this fires, attachment-path regex still
	// catches `~/.shannon/tmp/attachments/...`. Order matters: see sanitizeText.
	reUserPath = regexp.MustCompile(`/Users/[a-zA-Z0-9._-]+|/home/[a-zA-Z0-9._-]+`)

	// FOO=value-style env exports inside bash command strings or tool_use input.
	// Conservatively bias toward over-redaction; a false positive turns a
	// printed shell snippet into `FOO=[REDACTED]`, which is acceptable on a
	// public share page. NOTE: `audit.RedactSecrets` ALSO catches env-var
	// assignments but only when the variable name contains KEY/SECRET/TOKEN/
	// PASSWORD AND replaces the entire `FOO=bar` token. This extra regex runs
	// after audit's pass to (a) catch ARBITRARY env-var assignments (DATABASE_URL,
	// AWS_REGION etc. which may also leak credentials) and (b) preserve the
	// variable name so the share page reader can still tell something was set.
	reEnvVarAssign = regexp.MustCompile(`\b[A-Z][A-Z0-9_]{2,}=[^\s,;"']+`)

	// Supplementary API-key shapes audit.RedactSecrets does not match: GitHub
	// PATs (ghp_*, gho_*, ghu_*, ghs_*, ghr_*), Slack tokens (xoxb-/xoxp-/
	// xoxa-/xoxs-), and Google API keys (AIza*). The audit package owns the
	// AKIA / JWT / sk- / key- / Bearer / PEM patterns.
	reAPIKeyLikeSupplemental = regexp.MustCompile(`\b(?:gh[opusr]_[A-Za-z0-9]{20,}|xox[bpas]-[A-Za-z0-9-]{20,}|AIza[A-Za-z0-9_-]{20,})\b`)
)

// Options configures Sanitize. All fields are optional — defaults skip the
// corresponding pass.
type Options struct {
	// HomeDir, if non-empty, is replaced with "~" in text content (e.g.
	// "/Users/alice" → "~"). Pass os.UserHomeDir() at call site; tests
	// inject a fixed value to keep golden files stable.
	HomeDir string
}

// Sanitize returns a filtered copy of messages + meta safe to publish:
//
//   - Drops messages flagged MessageMeta.SystemInjected (system-injected
//     guardrails, nudges, error notices) entirely.
//   - Drops content blocks of type "thinking", "redacted_thinking", and
//     "document" (PDF / DOCX inline base64 — explicitly out of scope for
//     sharing per the user's data-safety requirement).
//   - Rewrites text blocks: removes <system-reminder> envelopes, replaces
//     HomeDir with "~", replaces attachment paths with "[attachment: name]",
//     redacts env-var assignments and recognizable API-key shapes.
//   - Rewrites tool_use Input: same path/secret redactions; truncates to
//     maxToolInputChars to avoid leaking large arg JSON on the share page.
//   - Recursively sanitizes tool_result content; truncates to
//     maxToolResultChars per result.
//   - Preserves image blocks and their base64 payloads unchanged so the
//     renderer can inline them as data URIs.
//
// Input slices are not mutated. Returned slices are aligned: filteredMeta[i]
// describes filteredMsgs[i]. When meta is shorter than messages (legacy
// sessions predating MessageMeta), missing entries are treated as
// zero-value (not SystemInjected).
func Sanitize(messages []client.Message, meta []session.MessageMeta, opts Options) ([]client.Message, []session.MessageMeta) {
	outMsgs := make([]client.Message, 0, len(messages))
	outMeta := make([]session.MessageMeta, 0, len(messages))

	for i, m := range messages {
		var mm session.MessageMeta
		if i < len(meta) {
			mm = meta[i]
		}
		if mm.SystemInjected {
			continue
		}

		filteredMsg, kept := sanitizeMessage(m, opts)
		if !kept {
			continue
		}
		outMsgs = append(outMsgs, filteredMsg)
		outMeta = append(outMeta, mm)
	}

	return outMsgs, outMeta
}

// sanitizeMessage returns the rewritten message and whether to keep it.
// A message is dropped when its block content is empty after filtering and
// it carried no plain text.
func sanitizeMessage(m client.Message, opts Options) (client.Message, bool) {
	if !m.Content.HasBlocks() {
		text := sanitizeText(m.Content.Text(), opts)
		if strings.TrimSpace(text) == "" {
			return client.Message{}, false
		}
		m.Content = client.NewTextContent(text)
		return m, true
	}

	out := make([]client.ContentBlock, 0, len(m.Content.Blocks()))
	for _, b := range m.Content.Blocks() {
		nb, keep := sanitizeBlock(b, opts)
		if !keep {
			continue
		}
		out = append(out, nb)
	}
	if len(out) == 0 {
		return client.Message{}, false
	}
	m.Content = client.NewBlockContent(out)
	return m, true
}

func sanitizeBlock(b client.ContentBlock, opts Options) (client.ContentBlock, bool) {
	switch b.Type {
	case "thinking", "redacted_thinking":
		return client.ContentBlock{}, false
	case "document":
		// PDF / inline document — out of scope for sharing.
		return client.ContentBlock{}, false
	case "image":
		// Preserve verbatim so renderer can emit a data URI. The base64 data
		// itself carries no path / secret material.
		return b, true
	case "text":
		text := sanitizeText(b.Text, opts)
		if strings.TrimSpace(text) == "" {
			return client.ContentBlock{}, false
		}
		b.Text = text
		return b, true
	case "tool_use":
		b.Input = sanitizeToolInput(b.Input, opts)
		return b, true
	case "tool_result":
		b.ToolContent = sanitizeToolResultContent(b.ToolContent, opts)
		return b, true
	default:
		// Unknown / future block types: keep so renderer can decide what to do.
		// The renderer's switch is closed, so unknowns produce no output —
		// fail-closed on display, not on sanitize.
		return b, true
	}
}

// sanitizeText applies the text-level rewrites in the order they need to run.
// Order matters:
//  1. Strip <system-reminder> envelopes first — their inner contents include
//     paths and reminders that downstream regexes would otherwise garble
//     into partial replacements.
//  2. Collapse attachment paths to "[attachment: name]" — works whether the
//     path starts with "/Users/<u>/" or has already been home-collapsed.
//  3. Strip path tail from "[User attached <kind>: name … path: …]" hints.
//  4. Replace home-dir prefix with "~".
//  5. Redact API-key shapes and env-var assignments.
func sanitizeText(text string, opts Options) string {
	if text == "" {
		return text
	}
	text = reSystemReminder.ReplaceAllString(text, "")
	text = reAttachmentPath.ReplaceAllString(text, "[attachment: $1]")
	text = reFileHint.ReplaceAllStringFunc(text, func(match string) string {
		sub := reFileHint.FindStringSubmatch(match)
		if len(sub) < 3 {
			return match
		}
		return "[User attached " + sub[1] + ": " + sub[2] + "]"
	})
	if opts.HomeDir != "" {
		text = strings.ReplaceAll(text, opts.HomeDir, "~")
	}
	text = reUserPath.ReplaceAllString(text, "~")
	// Env-var assignment redaction runs BEFORE audit.RedactSecrets so the
	// variable name survives ("AWS_SECRET_ACCESS_KEY=[REDACTED]" reads better
	// than "[REDACTED]" on a share page). After this pass, audit's regex
	// won't match the "= secret-keyword-style env var" pattern anymore, but
	// AKIA / JWT / sk- / Bearer / PEM patterns still trip on free-floating
	// values elsewhere in the text.
	text = reEnvVarAssign.ReplaceAllStringFunc(text, func(match string) string {
		eq := strings.IndexByte(match, '=')
		if eq < 0 {
			return "[REDACTED]"
		}
		return match[:eq+1] + "[REDACTED]"
	})
	text = audit.RedactSecrets(text)
	text = reAPIKeyLikeSupplemental.ReplaceAllString(text, "[REDACTED]")
	return text
}

// sanitizeToolInput rewrites tool_use.Input JSON. Walks the parsed value and
// applies sanitizeText to every string leaf. Truncates the serialized result
// to maxToolInputChars.
func sanitizeToolInput(raw json.RawMessage, opts Options) json.RawMessage {
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null")) {
		return raw
	}
	// Skip the unmarshal/marshal round-trip for trivially empty inputs —
	// they're common for tools like `think({})` and have nothing to sanitize.
	if bytes.Equal(trimmed, []byte("{}")) || bytes.Equal(trimmed, []byte("[]")) {
		return raw
	}
	var v any
	if err := json.Unmarshal(trimmed, &v); err != nil {
		// Unparseable input — fall back to a string-level pass on the raw bytes.
		s := sanitizeText(string(trimmed), opts)
		return json.RawMessage(truncateRunes(s, maxToolInputChars))
	}
	v = walkSanitizeJSON(v, opts)
	out, err := json.Marshal(v)
	if err != nil {
		return raw
	}
	if len(out) > maxToolInputChars {
		// Truncate at JSON-object granularity by re-encoding a string
		// placeholder; preserves valid JSON for any downstream consumer that
		// might re-parse.
		return json.RawMessage(`"` + truncateRunes(string(out), maxToolInputChars-2) + `"`)
	}
	return out
}

// walkSanitizeJSON recurses into the decoded JSON value, sanitizing every
// string leaf. Maps and slices are mutated in place.
func walkSanitizeJSON(v any, opts Options) any {
	switch t := v.(type) {
	case string:
		return sanitizeText(t, opts)
	case map[string]any:
		for k, child := range t {
			t[k] = walkSanitizeJSON(child, opts)
		}
		return t
	case []any:
		for i, child := range t {
			t[i] = walkSanitizeJSON(child, opts)
		}
		return t
	default:
		return v
	}
}

// sanitizeToolResultContent rewrites the polymorphic ToolContent field on a
// tool_result block. It may be:
//   - nil (no content) — pass through
//   - string — sanitize + truncate
//   - []ContentBlock — recurse: sanitize each block, drop documents/images/etc
//     according to the share policy
//   - some other shape (legacy / future) — pass through unchanged
func sanitizeToolResultContent(content any, opts Options) any {
	switch t := content.(type) {
	case nil:
		return nil
	case string:
		return truncateRunes(sanitizeText(t, opts), maxToolResultChars)
	case []client.ContentBlock:
		out := make([]client.ContentBlock, 0, len(t))
		used := 0
		for _, child := range t {
			nb, keep := sanitizeBlock(child, opts)
			if !keep {
				continue
			}
			// Apply the per-result text budget proportionally across remaining
			// text blocks — first ones get full budget, later ones are clipped
			// once the running total crosses the cap.
			if nb.Type == "text" {
				remaining := maxToolResultChars - used
				if remaining <= 0 {
					continue
				}
				nb.Text = truncateRunes(nb.Text, remaining)
				used += len([]rune(nb.Text))
			}
			out = append(out, nb)
		}
		return out
	default:
		return content
	}
}

// truncateRunes caps s to maxRunes runes and appends an ellipsis marker when
// truncation actually happened. Rune-safe so multi-byte content (CJK, emoji)
// is never cut mid-codepoint.
func truncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	const marker = "… [truncated]"
	if maxRunes <= len(marker) {
		return string(r[:maxRunes])
	}
	return string(r[:maxRunes-len([]rune(marker))]) + marker
}
