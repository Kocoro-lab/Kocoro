package agent

import (
	"fmt"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestLLMRetryDelayHonorsRateLimitCooldown(t *testing.T) {
	tests := []struct {
		name    string
		err     error
		attempt int
		want    time.Duration
	}{
		{
			name:    "ordinary transient keeps short exponential delay",
			err:     &client.APIError{StatusCode: 503},
			attempt: 1,
			want:    2 * time.Second,
		},
		{
			name:    "rate limit gets a useful local floor",
			err:     &client.APIError{StatusCode: 429},
			attempt: 0,
			want:    5 * time.Second,
		},
		{
			name: "provider hint wins",
			err: fmt.Errorf("wrapped: %w", &client.APIError{
				StatusCode: 429,
				RetryAfter: 17 * time.Second,
			}),
			attempt: 1,
			want:    17 * time.Second,
		},
		{
			name: "provider hint is bounded",
			err: &client.APIError{
				StatusCode: 429,
				RetryAfter: 5 * time.Minute,
			},
			want: 60 * time.Second,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := llmRetryDelay(tt.err, tt.attempt); got != tt.want {
				t.Fatalf("llmRetryDelay() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldFallbackToNonStreaming(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "rate limit", err: &client.APIError{StatusCode: 429}},
		{name: "server error", err: &client.APIError{StatusCode: 503}},
		{name: "bad request", err: &client.APIError{StatusCode: 400}},
		{name: "legacy endpoint missing", err: &client.APIError{StatusCode: 404}, want: true},
		{name: "legacy method unsupported", err: &client.APIError{StatusCode: 405}, want: true},
		{name: "stream transport failure", err: fmt.Errorf("stream read error: unexpected EOF"), want: true},
		{name: "stream idle timeout", err: client.ErrStreamIdleTimeout},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldFallbackToNonStreaming(tt.err); got != tt.want {
				t.Fatalf("fallback = %t, want %t", got, tt.want)
			}
		})
	}
}

func TestIsRetryableLLMError(t *testing.T) {
	tests := []struct {
		name      string
		err       error
		retryable bool
	}{
		{"nil", nil, false},
		// Typed APIError (primary path)
		{"typed 429", &client.APIError{StatusCode: 429, Body: "rate limit"}, true},
		{"typed 500", &client.APIError{StatusCode: 500, Body: "internal"}, true},
		{"typed 502", &client.APIError{StatusCode: 502, Body: "bad gateway"}, true},
		{"typed 503", &client.APIError{StatusCode: 503}, true},
		{"typed 529", &client.APIError{StatusCode: 529, Body: "overloaded"}, true},
		{"typed 400", &client.APIError{StatusCode: 400, Body: "invalid"}, false},
		{"typed 401", &client.APIError{StatusCode: 401, Body: "unauthorized"}, false},
		{"typed 403", &client.APIError{StatusCode: 403, Body: "forbidden"}, false},
		// Wrapped typed APIError (errors.As unwraps)
		{"wrapped 429", fmt.Errorf("LLM call failed: %w", &client.APIError{StatusCode: 429}), true},
		{"wrapped 400", fmt.Errorf("LLM call failed: %w", &client.APIError{StatusCode: 400}), false},
		// Network/stream errors (string-matched via client.TransportErrorShape)
		{"network timeout", fmt.Errorf("request failed: context deadline exceeded"), true},
		{"connection reset", fmt.Errorf("request failed: connection reset"), true},
		{"stream read error", fmt.Errorf("stream read error: unexpected EOF"), true},
		{"stream ended early", fmt.Errorf("stream ended without done event"), true},
		// Truncated-body decode on the stream->non-stream fallback is a
		// transport shape (the body was cut mid-flight), so it is retryable.
		{"decode truncation", fmt.Errorf("decode response: unexpected EOF"), true},
		// ErrStreamIdleTimeout is a transport shape for LABELING but must stay
		// NON-retryable — retrying a silent idle timeout just re-hangs.
		{"stream idle timeout", client.ErrStreamIdleTimeout, false},
		{"wrapped stream idle timeout", fmt.Errorf("stream aborted: %w", client.ErrStreamIdleTimeout), false},
		// Non-retryable
		{"marshal error", fmt.Errorf("marshal request: json error"), false},
		{"generic error", fmt.Errorf("something unexpected"), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isRetryableLLMError(tt.err)
			if got != tt.retryable {
				t.Errorf("isRetryableLLMError(%v) = %v, want %v", tt.err, got, tt.retryable)
			}
		})
	}
}

func TestClassifyLLMError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		expect string
	}{
		{"nil", nil, "unknown"},
		{"rate limit", &client.APIError{StatusCode: 429}, "rate limited"},
		{"overloaded", &client.APIError{StatusCode: 529}, "API overloaded"},
		{"server 500", &client.APIError{StatusCode: 500}, "server error"},
		{"server 502", &client.APIError{StatusCode: 502}, "server error"},
		{"server 503", &client.APIError{StatusCode: 503}, "server error"},
		{"bad request", &client.APIError{StatusCode: 400}, "HTTP 400"},
		{"timeout", fmt.Errorf("request failed: context deadline exceeded"), "request timeout"},
		{"connection reset", fmt.Errorf("request failed: connection reset"), "connection error"},
		{"stream error", fmt.Errorf("stream read error: unexpected EOF"), "stream interrupted"},
		{"generic", fmt.Errorf("something else"), "transient error"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := classifyLLMError(tt.err)
			if got != tt.expect {
				t.Errorf("classifyLLMError(%v) = %q, want %q", tt.err, got, tt.expect)
			}
		})
	}
}

func TestIsContextLengthError(t *testing.T) {
	tests := []struct {
		name   string
		err    error
		expect bool
	}{
		{"nil", nil, false},
		{"prompt too long", &client.APIError{StatusCode: 400, Body: `{"error":"prompt is too long"}`}, true},
		{"context_length_exceeded", &client.APIError{StatusCode: 400, Body: `{"error":"context_length_exceeded"}`}, true},
		{"case insensitive", &client.APIError{StatusCode: 400, Body: `Prompt Is Too Long`}, true},
		{"wrapped", fmt.Errorf("call failed: %w", &client.APIError{StatusCode: 400, Body: "prompt is too long"}), true},
		// Must NOT match
		{"max_tokens", &client.APIError{StatusCode: 400, Body: `{"error":"max_tokens exceeded"}`}, false},
		{"unrelated 400", &client.APIError{StatusCode: 400, Body: `{"error":"invalid request"}`}, false},
		{"server error", &client.APIError{StatusCode: 500, Body: "prompt is too long"}, false},
		{"non-api error", fmt.Errorf("prompt is too long"), false},
		{"rate limit", &client.APIError{StatusCode: 429, Body: "rate limited"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isContextLengthError(tt.err)
			if got != tt.expect {
				t.Errorf("isContextLengthError(%v) = %v, want %v", tt.err, got, tt.expect)
			}
		})
	}
}

func TestIsOpenAIComputerContinuationExpired(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "typed envelope",
			err: &client.APIError{
				StatusCode: 409,
				Body: `{"error":{"type":"computer_continuation_expired",` +
					`"code":"invalid_request","message":"spent","status":409}}`,
			},
			want: true,
		},
		{
			name: "wrapped typed envelope",
			err: fmt.Errorf("complete: %w", &client.APIError{
				StatusCode: 409,
				Body:       `{"error":{"type":"computer_continuation_expired"}}`,
			}),
			want: true,
		},
		{
			name: "unrelated conflict",
			err: &client.APIError{
				StatusCode: 409,
				Body:       `{"error":{"type":"conflict"}}`,
			},
		},
		{
			name: "type on wrong status",
			err: &client.APIError{
				StatusCode: 400,
				Body:       `{"error":{"type":"computer_continuation_expired"}}`,
			},
		},
		{
			name: "text mention is not structured contract",
			err: &client.APIError{
				StatusCode: 409,
				Body:       `computer_continuation_expired`,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isOpenAIComputerContinuationExpired(tt.err); got != tt.want {
				t.Fatalf(
					"isOpenAIComputerContinuationExpired(%v)=%v, want %v",
					tt.err,
					got,
					tt.want,
				)
			}
		})
	}
}
