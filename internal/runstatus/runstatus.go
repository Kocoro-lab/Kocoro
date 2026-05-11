package runstatus

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type Code string

const (
	CodeNone                  Code = ""
	CodeUserCancelled         Code = "user_cancelled"
	CodeDeadlineExceeded      Code = "deadline_exceeded"
	CodeRateLimited           Code = "rate_limited"
	CodeQuotaExceeded         Code = "quota_exceeded"
	CodeCreditsExhausted      Code = "credits_exhausted"
	CodeProviderOverloaded    Code = "provider_overloaded"
	CodeServiceTemporaryError Code = "service_temporary_error"
	CodeNetworkInterrupted    Code = "network_interrupted"
	CodeIterationLimit        Code = "iteration_limit"
	CodeUnexpected            Code = "unexpected"

	CodeContextCompactionFailed Code = "compaction_failed"
)

// Static fallback messages used when no Detail is available (parser
// could not extract structured fields from the gateway body, or the
// error did not flow through *client.APIError). Templated variants
// live in formatFriendly() and must keep these prefixes stable so
// IsFriendlyMessage's HasPrefix check still recognizes them.
var friendlyMessages = map[Code]string{
	CodeUserCancelled:         "The request was cancelled.",
	CodeDeadlineExceeded:      "The request timed out.",
	CodeRateLimited:           "Sorry, the AI service is currently rate-limited. Please try again in a moment.",
	CodeQuotaExceeded:         "You've reached your usage quota. Please upgrade or wait for the quota to reset.",
	CodeCreditsExhausted:      "Your credits are exhausted. Please top up to continue.",
	CodeProviderOverloaded:    "Sorry, the AI service is temporarily overloaded. Please try again shortly.",
	CodeServiceTemporaryError: "Sorry, the AI service encountered a temporary error. Please try again.",
	CodeNetworkInterrupted:    "Sorry, the connection to the AI service was interrupted. Please try again.",
	CodeIterationLimit:        "The request reached its iteration limit and returned a partial result.",
	CodeUnexpected:            "Sorry, an unexpected error occurred. Please try again.",

	CodeContextCompactionFailed: "Context compaction encountered an issue but the conversation continued.",
}

// friendlyPrefixes lists stable opening clauses shared between every
// templated variant and its static fallback. IsFriendlyMessage uses
// these to recognize templated forms (which embed variable values like
// reset_at) as friendly errors so context shaping can drop them. Keep
// in sync with formatFriendly() — a templated message that doesn't
// share its static fallback's prefix will leak into compaction.
var friendlyPrefixes = []string{
	"You've reached your usage quota.",
	"Your credits are exhausted.",
}

func FriendlyMessage(code Code) string {
	if msg, ok := friendlyMessages[code]; ok {
		return msg
	}
	return friendlyMessages[CodeUnexpected]
}

// FriendlyMessageFromError combines structured parsing of the gateway's
// 429 body with templated rendering. Use this for end-user error
// surfacing — it disambiguates the four 429 sub-codes (rate-limited,
// quota-exceeded, credits-exhausted, upstream provider) and substitutes
// concrete values (reset_at, window, auto_refill_started) when present.
//
// Falls back to FriendlyMessage(code) when no Detail was extractable —
// typically when the error was wrapped past *client.APIError or the
// body was malformed.
func FriendlyMessageFromError(err error) string {
	code, detail := codeAndDetailFromError(err)
	if detail != nil {
		return formatFriendly(code, detail)
	}
	return FriendlyMessage(code)
}

// formatFriendly renders a templated friendly message when Detail
// fields are populated; otherwise returns the static fallback so
// the prefix invariant in friendlyPrefixes still holds.
func formatFriendly(code Code, d *Detail) string {
	switch code {
	case CodeQuotaExceeded:
		if !d.ResetAt.IsZero() {
			window := d.Window
			if window == "" {
				window = "current"
			}
			return fmt.Sprintf("You've reached your usage quota. The %s quota resets at %s.", window, d.ResetAt.Format("2006-01-02 15:04 UTC"))
		}
	case CodeCreditsExhausted:
		if d.AutoRefillStarted {
			return "Your credits are exhausted. Auto-refill has started — please retry shortly."
		}
	}
	return FriendlyMessage(code)
}

func IsFriendlyMessage(text string) bool {
	for _, msg := range friendlyMessages {
		if text == msg {
			return true
		}
	}
	for _, prefix := range friendlyPrefixes {
		if strings.HasPrefix(text, prefix) {
			return true
		}
	}
	return false
}

func CodeFromError(err error) Code {
	code, _ := codeAndDetailFromError(err)
	return code
}

func codeAndDetailFromError(err error) (Code, *Detail) {
	if err == nil {
		return CodeNone, nil
	}
	if errors.Is(err, context.Canceled) {
		return CodeUserCancelled, nil
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return CodeDeadlineExceeded, nil
	}

	// Prefer structured extraction from *client.APIError. The gateway
	// returns differentiated JSON bodies for each 4xx/5xx variant, which
	// substring-matching on err.Error() collapses (e.g. "429" hides
	// quota-exceeded vs credits-exhausted vs per-min-throttle).
	var apiErr *client.APIError
	if errors.As(err, &apiErr) {
		return classifyAPIError(apiErr)
	}

	// Fallback: substring match on the rendered error string. Reaches
	// errors that didn't carry an APIError wrapper (HTTP transport
	// failures, decode errors).
	msg := err.Error()
	switch {
	case strings.Contains(msg, "429"):
		return CodeRateLimited, nil
	case strings.Contains(msg, "529") || strings.Contains(msg, "overloaded"):
		return CodeProviderOverloaded, nil
	case strings.Contains(msg, "500") || strings.Contains(msg, "502") || strings.Contains(msg, "503"):
		return CodeServiceTemporaryError, nil
	case strings.Contains(msg, "request failed:") || strings.Contains(msg, "stream read error"):
		return CodeNetworkInterrupted, nil
	default:
		return CodeUnexpected, nil
	}
}
