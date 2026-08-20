//go:build darwin && cgo

package koe

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"
)

func TestParseRealtimeProvider(t *testing.T) {
	for raw, want := range map[string]RealtimeProvider{"": ProviderAuto, "AUTO": ProviderAuto, "openai": ProviderOpenAI, "qwen": ProviderQwen} {
		got, err := ParseRealtimeProvider(raw)
		if err != nil || got != want {
			t.Fatalf("ParseRealtimeProvider(%q) = %q, %v; want %q", raw, got, err, want)
		}
	}
	if _, err := ParseRealtimeProvider("other"); err == nil {
		t.Fatal("unknown provider accepted")
	}
}

func TestQwenRealtimeModelAllowlistOnlyIncludesCompatibleModels(t *testing.T) {
	for _, model := range []string{"qwen3.5-omni-flash-realtime", "qwen3.5-omni-plus-realtime"} {
		if err := ValidateRealtimeModel(ProviderQwen, model); err != nil {
			t.Fatalf("compatible Qwen model %q rejected: %v", model, err)
		}
	}
	if err := ValidateRealtimeModel(ProviderQwen, "qwen3-omni-flash-realtime"); err == nil {
		t.Fatal("Qwen3 realtime model without function calling remained selectable")
	}
}

func TestAutoFallbackEligibility(t *testing.T) {
	netFailure := &net.DNSError{Err: "timeout", IsTimeout: true}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"transport", connectError(ProviderOpenAI, "sdp_exchange", netFailure), true},
		{"deadline", connectError(ProviderOpenAI, "sdp_exchange", context.DeadlineExceeded), true},
		{"upstream 500", connectError(ProviderOpenAI, "sdp_exchange", &RealtimeBootstrapError{StatusCode: 502}), true},
		// The daemon's own signed-out 503: Qwen rides the same relay, so falling
		// back cannot succeed and must not open the OpenAI circuit either.
		{"daemon signed out", connectError(ProviderOpenAI, "mint", &RealtimeBootstrapError{StatusCode: 503, Body: `{"error":"cloud not configured (sign in, or set cloud.enabled + api_key)"}`}), false},
		{"genuine 503", connectError(ProviderOpenAI, "mint", &RealtimeBootstrapError{StatusCode: 503, Body: `{"detail":"provider_rate_limited"}`}), true},
		{"auth", connectError(ProviderOpenAI, "sdp_exchange", &RealtimeBootstrapError{StatusCode: 401}), false},
		{"quota", connectError(ProviderOpenAI, "sdp_exchange", &RealtimeBootstrapError{StatusCode: 429}), false},
		{"config rejected", connectError(ProviderOpenAI, "session_config_rejected", errors.New("bad voice")), false},
		{"remote description rejected", connectError(ProviderOpenAI, "remote_description", errors.New("bad answer SDP")), false},
		{"ready timeout", connectError(ProviderOpenAI, "session_ready_timeout", context.DeadlineExceeded), true},
		{"qwen failure", connectError(ProviderQwen, "sdp_exchange", netFailure), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := AutoFallbackEligible(tt.err); got != tt.want {
				t.Fatalf("AutoFallbackEligible(%v) = %t, want %t", tt.err, got, tt.want)
			}
		})
	}
}

func TestOpenAICircuitOnlyCachesEligibleFailure(t *testing.T) {
	now := time.Unix(100, 0)
	c := NewOpenAICircuit(time.Minute)
	c.now = func() time.Time { return now }
	c.RecordFailure(connectError(ProviderOpenAI, "session_config_rejected", errors.New("bad model")))
	if c.Skip() {
		t.Fatal("configuration rejection opened the circuit")
	}
	c.RecordFailure(connectError(ProviderOpenAI, "sdp_exchange", context.DeadlineExceeded))
	if !c.Skip() {
		t.Fatal("eligible connection failure did not open the circuit")
	}
	now = now.Add(time.Minute)
	if c.Skip() {
		t.Fatal("circuit stayed open after cooldown")
	}
}
