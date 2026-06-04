package client

import (
	"fmt"
	"testing"
)

func TestTransportErrorShape(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		// Transport shapes — the rendered strings the gateway client wraps with.
		{"request failed", fmt.Errorf("request failed: dial tcp: connection refused"), true},
		{"stream read error", fmt.Errorf("stream read error: unexpected EOF"), true},
		{"stream ended without done", fmt.Errorf("stream ended without done event"), true},
		{"decode truncation", fmt.Errorf("decode response: unexpected EOF"), true},
		{"wrapped request failed", fmt.Errorf("LLM call failed: %w", fmt.Errorf("request failed: broken pipe")), true},
		// ErrStreamIdleTimeout is a transport shape (matched via errors.Is).
		{"stream idle timeout", ErrStreamIdleTimeout, true},
		{"wrapped stream idle timeout", fmt.Errorf("stream aborted: %w", ErrStreamIdleTimeout), true},
		// A status-coded APIError reached the server — NOT a transport shape.
		{"api error 503", &APIError{StatusCode: 503}, false},
		{"api error 429", &APIError{StatusCode: 429, Body: "rate limited"}, false},
		{"wrapped api error", fmt.Errorf("LLM call failed: %w", &APIError{StatusCode: 500}), false},
		// Non-transport application errors.
		{"marshal error", fmt.Errorf("marshal request: json error"), false},
		{"generic", fmt.Errorf("something unexpected"), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := TransportErrorShape(tc.err); got != tc.want {
				t.Errorf("TransportErrorShape(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
