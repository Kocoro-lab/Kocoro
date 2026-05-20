package runstatus

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestCodeFromError_Context(t *testing.T) {
	if got := CodeFromError(context.Canceled); got != CodeUserCancelled {
		t.Fatalf("expected %q, got %q", CodeUserCancelled, got)
	}
	if got := CodeFromError(context.DeadlineExceeded); got != CodeDeadlineExceeded {
		t.Fatalf("expected %q, got %q", CodeDeadlineExceeded, got)
	}
}

func TestCodeFromError_ClassifiesProviderFailures(t *testing.T) {
	tests := []struct {
		err  error
		want Code
	}{
		{errors.New("API returned 429"), CodeRateLimited},
		{errors.New("API returned 529 overloaded"), CodeProviderOverloaded},
		{errors.New("API returned 503"), CodeServiceTemporaryError},
		{errors.New("request failed: upstream disconnected"), CodeNetworkInterrupted},
	}

	for _, tc := range tests {
		if got := CodeFromError(tc.err); got != tc.want {
			t.Fatalf("error %q: expected %q, got %q", tc.err, tc.want, got)
		}
	}
}

func TestIsFriendlyMessage(t *testing.T) {
	for code := range friendlyMessages {
		if !IsFriendlyMessage(FriendlyMessage(code)) {
			t.Fatalf("expected friendly message for %q to be recognized", code)
		}
	}
	if IsFriendlyMessage("plain user text") {
		t.Fatal("unexpected friendly-message match for plain text")
	}
}

// TestCodeFromError_429Disambiguation covers the four shapes the
// gateway returns on 429 (per shannon-cloud middleware/quota.go,
// ratelimit.go, openai/handler.go). Substring-matching on "429"
// previously collapsed all four into CodeRateLimited, causing a real
// UX bug for quota-exceeded and credits-exhausted users.
func TestCodeFromError_429Disambiguation(t *testing.T) {
	tests := []struct {
		name       string
		body       string
		wantCode   Code
		wantDetail bool
	}{
		{
			name:       "case A — token quota exceeded with reset_at",
			body:       `{"error":"Token quota exceeded","message":"Your monthly token quota has been exceeded. Please wait until the quota resets.","window":"monthly","reset_at":"2026-06-01T00:00:00Z"}`,
			wantCode:   CodeQuotaExceeded,
			wantDetail: true,
		},
		{
			name:       "case A — daily quota with reset_at",
			body:       `{"error":"Token quota exceeded","window":"daily","reset_at":"2026-05-08T00:00:00Z"}`,
			wantCode:   CodeQuotaExceeded,
			wantDetail: true,
		},
		{
			name:       "case B — credits exhausted, no auto-refill",
			body:       `{"error":"credits_exhausted","code":"credits_exhausted","auto_refill_started":false,"retry_after_seconds":0}`,
			wantCode:   CodeCreditsExhausted,
			wantDetail: true,
		},
		{
			name:       "case B — credits exhausted with auto-refill",
			body:       `{"error":"credits_exhausted","code":"credits_exhausted","auto_refill_started":true,"retry_after_seconds":5}`,
			wantCode:   CodeCreditsExhausted,
			wantDetail: true,
		},
		{
			name:     "case C — per-minute rate throttle",
			body:     `{"error":"Rate limit exceeded","message":"Too many requests. minute limit exceeded.","window":"minute"}`,
			wantCode: CodeRateLimited,
		},
		{
			name:     "case D — upstream Anthropic 429 (wrapped object)",
			body:     `{"error":{"message":"Rate limit exceeded","type":"rate_limit_error","code":"rate_limit_exceeded"}}`,
			wantCode: CodeRateLimited,
		},
		{
			name:     "malformed body falls back to rate-limited",
			body:     "not json",
			wantCode: CodeRateLimited,
		},
		{
			name:     "empty body falls back to rate-limited",
			body:     "",
			wantCode: CodeRateLimited,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := &client.APIError{StatusCode: 429, Body: tc.body}
			gotCode, gotDetail := codeAndDetailFromError(err)
			if gotCode != tc.wantCode {
				t.Errorf("code = %q, want %q", gotCode, tc.wantCode)
			}
			if tc.wantDetail && gotDetail == nil {
				t.Errorf("expected non-nil Detail for body %q", tc.body)
			}
			if !tc.wantDetail && gotDetail != nil {
				t.Errorf("expected nil Detail for body %q, got %+v", tc.body, gotDetail)
			}
		})
	}
}

// TestCodeFromError_APIErrorOtherStatusCodes confirms classifyAPIError
// routes non-429 status codes correctly.
func TestCodeFromError_APIErrorOtherStatusCodes(t *testing.T) {
	tests := []struct {
		status int
		want   Code
	}{
		{500, CodeServiceTemporaryError},
		{502, CodeServiceTemporaryError},
		{503, CodeServiceTemporaryError},
		{529, CodeProviderOverloaded},
		{418, CodeUnexpected}, // unknown -> unexpected
	}
	for _, tc := range tests {
		err := &client.APIError{StatusCode: tc.status, Body: ""}
		if got := CodeFromError(err); got != tc.want {
			t.Errorf("status %d: got %q, want %q", tc.status, got, tc.want)
		}
	}
}

// TestFriendlyMessageFromError_TemplatedQuota confirms reset_at and
// window are substituted into the user-facing message.
func TestFriendlyMessageFromError_TemplatedQuota(t *testing.T) {
	body := `{"error":"Token quota exceeded","window":"monthly","reset_at":"2026-06-01T00:00:00Z"}`
	err := &client.APIError{StatusCode: 429, Body: body}
	got := FriendlyMessageFromError(err)
	if !strings.Contains(got, "monthly") {
		t.Errorf("expected window in message, got: %s", got)
	}
	if !strings.Contains(got, "2026-06-01") {
		t.Errorf("expected reset_at date in message, got: %s", got)
	}
	// Stable prefix is preserved so IsFriendlyMessage still recognizes it.
	if !strings.HasPrefix(got, "You've reached your usage quota.") {
		t.Errorf("expected stable prefix, got: %s", got)
	}
}

// TestFriendlyMessageFromError_TemplatedCreditsAutoRefill confirms the
// auto-refill variant is rendered when the gateway sets that flag.
func TestFriendlyMessageFromError_TemplatedCreditsAutoRefill(t *testing.T) {
	body := `{"error":"credits_exhausted","auto_refill_started":true}`
	err := &client.APIError{StatusCode: 429, Body: body}
	got := FriendlyMessageFromError(err)
	if !strings.Contains(strings.ToLower(got), "auto-refill") {
		t.Errorf("expected auto-refill mention, got: %s", got)
	}
	if !strings.HasPrefix(got, "Your credits are exhausted.") {
		t.Errorf("expected stable prefix, got: %s", got)
	}
}

// TestIsFriendlyMessage_RecognizesTemplated guards the prefix invariant
// that lets sanitize.go drop templated quota/credits messages from
// persisted history during compaction. If a templated variant doesn't
// share its static fallback's prefix, this test catches the drift.
func TestIsFriendlyMessage_RecognizesTemplated(t *testing.T) {
	cases := []struct {
		name string
		body string
	}{
		{"templated quota", `{"error":"Token quota exceeded","window":"monthly","reset_at":"2026-06-01T00:00:00Z"}`},
		{"templated credits with auto-refill", `{"error":"credits_exhausted","auto_refill_started":true}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := &client.APIError{StatusCode: 429, Body: tc.body}
			msg := FriendlyMessageFromError(err)
			if !IsFriendlyMessage(msg) {
				t.Errorf("templated message %q not recognized as friendly", msg)
			}
		})
	}
}

// TestFriendlyPrefixes_AlignWithStaticMessages ensures every entry in
// friendlyPrefixes is actually a prefix of the static message it
// claims to anchor. Keeps the prefix slice and friendlyMessages map
// from drifting silently if either is edited in isolation.
func TestFriendlyPrefixes_AlignWithStaticMessages(t *testing.T) {
	for _, prefix := range friendlyPrefixes {
		matched := false
		for _, msg := range friendlyMessages {
			if strings.HasPrefix(msg, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			t.Errorf("prefix %q does not match any static friendly message — keep friendlyPrefixes in sync", prefix)
		}
	}
}

// TestParse429_QuotaResetAtParsing covers the RFC3339 parsing path —
// invalid reset_at must not propagate a zero-time silently as if it
// were valid.
func TestParse429_QuotaResetAtParsing(t *testing.T) {
	t.Run("valid RFC3339 yields populated ResetAt", func(t *testing.T) {
		_, d := parse429(`{"error":"Token quota exceeded","window":"monthly","reset_at":"2026-06-01T00:00:00Z"}`)
		if d == nil || d.ResetAt.IsZero() {
			t.Fatal("expected non-zero ResetAt")
		}
		if got := d.ResetAt.Year(); got != 2026 {
			t.Errorf("ResetAt.Year() = %d, want 2026", got)
		}
	})
	t.Run("invalid RFC3339 leaves ResetAt zero", func(t *testing.T) {
		_, d := parse429(`{"error":"Token quota exceeded","window":"monthly","reset_at":"not a timestamp"}`)
		if d == nil {
			t.Fatal("expected Detail")
		}
		if !d.ResetAt.IsZero() {
			t.Error("expected zero ResetAt for invalid timestamp")
		}
	})
	t.Run("missing reset_at leaves ResetAt zero, falls back to static message", func(t *testing.T) {
		err := &client.APIError{StatusCode: 429, Body: `{"error":"Token quota exceeded","window":"monthly"}`}
		got := FriendlyMessageFromError(err)
		// Static fallback (no reset substitution).
		if got != FriendlyMessage(CodeQuotaExceeded) {
			t.Errorf("got templated message without reset_at: %s", got)
		}
	})
}

// TestCodeFromError_WrappedAPIError covers both wrap shapes the agent
// loop might produce. The structured (errors.As) path is the new
// preferred route; the substring fallback exists for chains that
// dropped the *client.APIError type along the way.
func TestCodeFromError_WrappedAPIError(t *testing.T) {
	inner := &client.APIError{StatusCode: 429, Body: `{"error":"credits_exhausted","auto_refill_started":false}`}

	// String-concatenation wrapping (errors.New(... + .Error())) discards
	// the original type. errors.As cannot recover it; substring fallback
	// matches "429" and returns the coarse CodeRateLimited.
	concatenated := errors.New("complete: " + inner.Error())
	if got := CodeFromError(concatenated); got != CodeRateLimited {
		t.Errorf("string-concat-wrapped APIError = %q, want %q (substring fallback)", got, CodeRateLimited)
	}

	// fmt.Errorf with %w preserves the wrap chain. errors.As reaches
	// *client.APIError → structured path activates → correct sub-code.
	wrapped := fmt.Errorf("complete failed: %w", inner)
	if got := CodeFromError(wrapped); got != CodeCreditsExhausted {
		t.Errorf("%%w-wrapped APIError = %q, want %q", got, CodeCreditsExhausted)
	}
}

// TestCodeFromError_EmptyFinalResponse verifies the string-match path that
// classifies agent.ErrEmptyFinalResponse without importing internal/agent
// (would create a cycle). The error message text is the contract — if the
// agent package's ErrEmptyFinalResponse.Error() string ever changes, this
// test forces an update on both sides simultaneously.
func TestCodeFromError_EmptyFinalResponse(t *testing.T) {
	err := errors.New("agent: LLM returned empty final response")
	if got := CodeFromError(err); got != CodeEmptyResponse {
		t.Fatalf("expected %q, got %q", CodeEmptyResponse, got)
	}

	// Daemon goes through FriendlyMessageFromError to compose the user-
	// facing stub. Verify it picks up the specific friendly text rather
	// than falling back to the generic CodeUnexpected message.
	got := FriendlyMessageFromError(err)
	want := friendlyMessages[CodeEmptyResponse]
	if got != want {
		t.Fatalf("expected friendly message %q, got %q", want, got)
	}
	if got == friendlyMessages[CodeUnexpected] {
		t.Fatalf("empty-response error fell back to CodeUnexpected friendly message — string match broken")
	}
}

// TestFriendlyMessage_EmptyResponseExists guards the friendly-message map
// against accidental removal — the deferred CodeEmptyResponse entry is the
// only differentiator vs CodeUnexpected from the user's POV.
func TestFriendlyMessage_EmptyResponseExists(t *testing.T) {
	msg := FriendlyMessage(CodeEmptyResponse)
	if msg == "" {
		t.Fatal("CodeEmptyResponse has no friendly message")
	}
	if msg == friendlyMessages[CodeUnexpected] {
		t.Fatal("CodeEmptyResponse friendly message equals CodeUnexpected — coaching value lost")
	}
	if !IsFriendlyMessage(msg) {
		t.Fatal("CodeEmptyResponse friendly message is not recognized as friendly — would survive sanitize and pollute LLM context")
	}
}
