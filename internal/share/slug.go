package share

import (
	"context"
	"regexp"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

const (
	// slugTimeout caps the Haiku slug call. Output is tiny (2-5 words, ~30
	// tokens) and the input is a small ~600-char snippet, so warm responses
	// land in 300-800 ms — but cold-path / queue stalls can spike well past
	// the original 5s ceiling, which made falling back to title-ASCII slug
	// (or sess-id, for pure-CJK sessions) common enough to hurt the
	// "Published files" listing UX.
	//
	// 30s is chosen to:
	//   - Sit comfortably under summaryTimeout (45s) so the parallel slug
	//     call never blocks the wg.Wait() longer than the summary does.
	//   - Stay well inside shareTaskTimeout (180s) on the async path.
	//   - Be wide enough to absorb a cold Haiku route (rare but real) plus
	//     a few hundred ms of network jitter.
	slugTimeout = 30 * time.Second

	// slugMaxRunes caps the post-clean slug length. Total filename is
	// "session-<slug>-<YYYYMMDD-HHMMSS>.html"; with timestamp adding 22
	// chars and "session-.html" adding 13, a 40-rune slug keeps total
	// filename under 80 chars — comfortable for any S3 / OS limit.
	slugMaxRunes = 40

	// slugInputUserMessageRunes caps the per-message snippet handed to
	// Haiku. Slug generation only needs the topic, not the full message;
	// truncating keeps the call cheap and fast.
	slugInputUserMessageRunes = 400
)

// slugPrompt instructs Haiku to produce an English URL slug. The format is
// intentionally rigid (lowercase, hyphens only, no explanation) so cleanSlug
// has very little post-processing to do on the typical response.
const slugPrompt = `You are a URL slug generator. Read the conversation context and output a short slug that captures its topic.

Rules:
- 2-5 ENGLISH words joined by hyphens
- ALL lowercase ASCII letters and digits only
- NO other punctuation, NO quotes, NO leading "slug:" or "Output:" prefix
- NO explanation — output ONLY the slug, nothing before or after
- Translate non-English topics to English (e.g. "解释机器学习" → "machine-learning-explanation", "桌面文件列表" → "list-desktop-files")
- Focus on the TOPIC, not the action verb ("loader-refactor", not "user-asks-refactor")

Output the slug only.`

// slugValidator enforces the final shape: 2+ chars, only [a-z0-9-], no
// leading / trailing / consecutive hyphens.
var slugValidator = regexp.MustCompile(`^[a-z0-9]+(?:-[a-z0-9]+)*$`)

// generateEnglishSlug asks Haiku for a short URL-safe slug summarizing the
// session. Used as the filename slug when sharing so "Published files"
// listings stay readable across all input languages without risking the
// CJK→S3-key encoding issues described in slugifyTitleForFilename.
//
// On any failure (gw nil, timeout, error, malformed response) returns "".
// The caller (share_handler.buildShareFilename) falls back to the
// title-based ASCII slug, then to a session-ID prefix.
//
// Runs with cache_source="session_share" so once Cloud adds the exemption
// rule, both the summary and the slug Haiku calls are user-quota-exempt.
func generateEnglishSlug(ctx context.Context, gw ctxwin.Completer, sess *session.Session, msgs []client.Message) string {
	if gw == nil || sess == nil {
		return ""
	}
	input := buildSlugInput(sess, msgs)
	if input == "" {
		return ""
	}

	cctx, cancel := context.WithTimeout(ctx, slugTimeout)
	defer cancel()

	resp, err := gw.Complete(cctx, client.CompletionRequest{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent(slugPrompt)},
			{Role: "user", Content: client.NewTextContent(input)},
		},
		ModelTier:   "small",
		Temperature: 0,
		MaxTokens:   50,
		CacheSource: summaryCacheSource,
	})
	if err != nil {
		return ""
	}
	return cleanSlug(resp.OutputText)
}

// buildSlugInput composes a small context window for the slug generator:
// session title + first non-empty user message snippet. Total typically
// under 600 chars, so the call is cheap and fast.
func buildSlugInput(sess *session.Session, msgs []client.Message) string {
	var b strings.Builder
	if t := strings.TrimSpace(sess.Title); t != "" {
		b.WriteString("Session title: ")
		b.WriteString(t)
		b.WriteString("\n\n")
	}
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		t := strings.TrimSpace(m.Content.Text())
		if t == "" {
			continue
		}
		b.WriteString("First user message: ")
		b.WriteString(truncateRunes(t, slugInputUserMessageRunes))
		break
	}
	return strings.TrimSpace(b.String())
}

// cleanSlug normalizes the model's raw output: strip common preambles,
// quotes, and trailing periods; lowercase; drop any non-[a-z0-9-] char;
// collapse hyphen runs; trim leading/trailing hyphens; cap length; reject
// anything that doesn't match slugValidator after the cleanup. Returns
// "" on rejection — the caller treats empty as "use fallback slug source".
func cleanSlug(raw string) string {
	s := strings.ToLower(strings.TrimSpace(raw))

	// Strip a leading "slug:" / "output:" / "the slug is:" style preamble
	// if the colon is near the start (a real slug never contains ':').
	if idx := strings.Index(s, ":"); idx >= 0 && idx < 16 {
		s = strings.TrimSpace(s[idx+1:])
	}
	// Strip surrounding quotes/backticks the model sometimes adds.
	s = strings.Trim(s, `"'` + "`")
	// Strip a trailing period or comma.
	s = strings.TrimRight(s, ".,;")

	var b strings.Builder
	prevDash := false
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case r == '-' || r == '_' || r == ' ' || r == '/':
			if !prevDash && b.Len() > 0 {
				b.WriteRune('-')
				prevDash = true
			}
		}
	}
	out := strings.Trim(b.String(), "-")

	// Cap length and re-trim any orphan trailing hyphen.
	r := []rune(out)
	if len(r) > slugMaxRunes {
		out = strings.TrimRight(string(r[:slugMaxRunes]), "-")
	}

	if !slugValidator.MatchString(out) {
		return ""
	}
	// Reject obviously junk output. A 1-2 char "slug" usually means Haiku
	// returned a letter or two (e.g. "ok") instead of an actual slug; not
	// worth using as a filename.
	if len(out) < 3 {
		return ""
	}
	return out
}
