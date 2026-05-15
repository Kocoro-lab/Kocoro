package share

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	ctxwin "github.com/Kocoro-lab/ShanClaw/internal/context"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// summaryTimeout caps Haiku waits. Beyond this the fallback summary kicks in
// so a slow LLM never blocks a user's share. Larger than the agent-loop
// skill-discovery timeout (5s) because share is a user-initiated, foreground
// action where waiting a few extra seconds is acceptable.
const summaryTimeout = 15 * time.Second

// summaryFallbackChars caps the fallback summary's length so a long first
// user message doesn't dominate the page when Haiku fails.
const summaryFallbackChars = 200

// summaryCacheSource tags the Haiku call as a share-feature internal helper
// so Cloud-side billing skips user-quota accounting — parallel to how
// "prompt_suggestion" is already exempted (see CLAUDE.md "Prompt Cache"
// section). Cloud must include "session_share" in its billing exempt list
// for the exemption to take effect; until then the call still bills the
// user but the tag remains stable so the rollout is daemon-no-op.
const summaryCacheSource = "session_share"

// RenderResult bundles the rendered HTML with light telemetry callers may
// want to log or surface to the user (e.g. "summary unavailable, used title").
type RenderResult struct {
	HTML []byte
	// Summary is the text actually rendered into the share page header.
	// May be empty when neither Haiku nor the fallbacks produced anything.
	Summary string
	// SummaryFallback is true when Haiku was unreachable / errored / produced
	// empty output and the page used Title / first-user-message instead.
	SummaryFallback bool
}

// Render is the orchestrator the daemon endpoint calls. It sanitizes the
// session's message stream, asks Haiku (via gw) for a user-facing summary,
// and renders the full HTML. The returned HTML is self-contained and ready
// to upload via internal/uploads.
//
// gw may be nil — useful for tests and as a defense for callers that haven't
// gated on cloud.enabled. In that case the fallback summary is used.
//
// The returned context error from a cancelled ctx is surfaced as-is so callers
// can distinguish "user cancelled the share" from a render bug.
func Render(ctx context.Context, gw ctxwin.Completer, sess *session.Session, opts Options) (RenderResult, error) {
	if sess == nil {
		return RenderResult{}, fmt.Errorf("share: nil session")
	}
	if err := ctx.Err(); err != nil {
		return RenderResult{}, err
	}

	sanitizedMsgs, sanitizedMeta := Sanitize(sess.Messages, sess.MessageMeta, opts)

	summary, fallback := generateSummary(ctx, gw, sanitizedMsgs, sess)

	html, err := RenderHTML(RenderInput{
		Session:     sess,
		Messages:    sanitizedMsgs,
		Meta:        sanitizedMeta,
		Summary:     summary,
		GeneratedAt: time.Now(),
	})
	if err != nil {
		return RenderResult{}, err
	}

	return RenderResult{
		HTML:            html,
		Summary:         summary,
		SummaryFallback: fallback,
	}, nil
}

func generateSummary(ctx context.Context, gw ctxwin.Completer, msgs []client.Message, sess *session.Session) (string, bool) {
	if gw == nil || len(msgs) == 0 {
		return fallbackSummary(sess, msgs), true
	}
	cctx, cancel := context.WithTimeout(ctx, summaryTimeout)
	defer cancel()

	summary, err := ctxwin.SummarizeForUserWithSource(cctx, gw, msgs, summaryCacheSource)
	if err != nil || strings.TrimSpace(summary) == "" {
		return fallbackSummary(sess, msgs), true
	}
	return strings.TrimSpace(summary), false
}

// fallbackSummary picks something readable when Haiku is unavailable.
// Preference order: explicit Title → first non-empty user message (truncated).
// Returns empty if neither yields anything, which renders no summary card.
func fallbackSummary(sess *session.Session, msgs []client.Message) string {
	if sess != nil {
		if t := strings.TrimSpace(sess.Title); t != "" {
			return t
		}
	}
	for _, m := range msgs {
		if m.Role != "user" {
			continue
		}
		if t := strings.TrimSpace(m.Content.Text()); t != "" {
			return truncateRunes(t, summaryFallbackChars)
		}
	}
	return ""
}
