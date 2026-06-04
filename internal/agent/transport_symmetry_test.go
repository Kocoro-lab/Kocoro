package agent

import (
	"fmt"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
)

// TestTransportError_RetryAndLabelSymmetry pins the invariant that the retry
// decision (isRetryableLLMError) and the user-facing failure label
// (runstatus.CodeFromError) classify the SAME transport shapes consistently —
// the divergence that mislabeled a dropped continuation as the generic
// CodeUnexpected ("Sorry, an unexpected error occurred") while also failing to
// retry it. For each shape: it must be retryable where appropriate AND map to
// CodeNetworkInterrupted (never CodeUnexpected), except ErrStreamIdleTimeout
// which is labeled as a network interruption but stays non-retryable by design.
func TestTransportError_RetryAndLabelSymmetry(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"request failed dial", fmt.Errorf("request failed: dial tcp: connection refused"), true},
		{"request failed reset", fmt.Errorf("request failed: connection reset by peer"), true},
		{"stream read error eof", fmt.Errorf("stream read error: unexpected EOF"), true},
		{"stream read http2", fmt.Errorf("stream read error: stream error: stream ID 7; INTERNAL_ERROR"), true},
		{"stream ended early", fmt.Errorf("stream ended without done event"), true},
		// The incident's signature: streaming dropped, the non-stream fallback
		// got a truncated body, the JSON decode hit EOF. Previously no-retry +
		// CodeUnexpected; now retryable + CodeNetworkInterrupted.
		{"decode truncation", fmt.Errorf("decode response: unexpected EOF"), true},
		// Wrapper-preserved-after-retry: completeWithRetry / the main loop wrap
		// the surfaced error with fmt.Errorf("LLM call failed: %w", ...). %w keeps
		// both the type (errors.As) and the rendered string (strings.Contains)
		// intact, so the shape still classifies correctly through the wrap.
		{"wrapped request failed", fmt.Errorf("LLM call failed: %w", fmt.Errorf("request failed: broken pipe")), true},
		{"wrapped decode truncation", fmt.Errorf("LLM call failed: %w", fmt.Errorf("decode response: unexpected EOF")), true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isRetryableLLMError(tc.err); got != tc.retryable {
				t.Errorf("isRetryableLLMError(%q) = %v, want %v", tc.err, got, tc.retryable)
			}
			if got := runstatus.CodeFromError(tc.err); got != runstatus.CodeNetworkInterrupted {
				t.Errorf("CodeFromError(%q) = %q, want %q (must NOT be CodeUnexpected)",
					tc.err, got, runstatus.CodeNetworkInterrupted)
			}
			if got := runstatus.CodeFromError(tc.err); got == runstatus.CodeUnexpected {
				t.Errorf("CodeFromError(%q) fell through to CodeUnexpected — the original defect", tc.err)
			}
		})
	}
}

// TestStreamIdleTimeout_LabelButNoRetry pins the deliberate asymmetry:
// ErrStreamIdleTimeout classifies as a transport shape for labeling (so the
// user sees "the connection was interrupted", not "unexpected error") but the
// retry path keeps its veto on top because retrying a silent idle timeout just
// re-hangs the same upstream.
func TestStreamIdleTimeout_LabelButNoRetry(t *testing.T) {
	for _, err := range []error{
		client.ErrStreamIdleTimeout,
		fmt.Errorf("stream aborted: %w", client.ErrStreamIdleTimeout),
	} {
		if isRetryableLLMError(err) {
			t.Errorf("isRetryableLLMError(%q) = true, want false (idle timeout must stay non-retryable)", err)
		}
		if got := runstatus.CodeFromError(err); got != runstatus.CodeNetworkInterrupted {
			t.Errorf("CodeFromError(%q) = %q, want %q", err, got, runstatus.CodeNetworkInterrupted)
		}
	}
}

// TestNonTransportErrors_LabelUnchanged guards that the symmetry change did NOT
// pull non-transport shapes (status-coded APIErrors, marshal errors,
// context-length overflow, generic errors) into the transport bucket. Their
// existing labels and retry verdicts must be preserved.
func TestNonTransportErrors_LabelUnchanged(t *testing.T) {
	cases := []struct {
		name      string
		err       error
		retryable bool
		wantCode  runstatus.Code
	}{
		{"api 429", &client.APIError{StatusCode: 429, Body: "rate limited"}, true, runstatus.CodeRateLimited},
		{"api 503", &client.APIError{StatusCode: 503}, true, runstatus.CodeServiceTemporaryError},
		{"api 529", &client.APIError{StatusCode: 529}, false /*see note*/, runstatus.CodeProviderOverloaded},
		{"api 400", &client.APIError{StatusCode: 400, Body: "invalid"}, false, runstatus.CodeUnexpected},
		{"marshal error", fmt.Errorf("marshal request: json error"), false, runstatus.CodeUnexpected},
		{"generic error", fmt.Errorf("something unexpected"), false, runstatus.CodeUnexpected},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// 529 is retryable per isRetryableLLMError's status switch; correct the
			// expectation inline rather than special-casing the table above.
			wantRetry := tc.retryable
			if apiErr, ok := tc.err.(*client.APIError); ok && apiErr.StatusCode == 529 {
				wantRetry = true
			}
			if got := isRetryableLLMError(tc.err); got != wantRetry {
				t.Errorf("isRetryableLLMError(%v) = %v, want %v", tc.err, got, wantRetry)
			}
			if got := runstatus.CodeFromError(tc.err); got != tc.wantCode {
				t.Errorf("CodeFromError(%v) = %q, want %q", tc.err, got, tc.wantCode)
			}
		})
	}
}
