package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestResolveExecutionProfileUsesCanonicalWireContractAndMintsTrustedProfile(t *testing.T) {
	anthropicNative := loadExecutionProfileFixture(t, "profile.anthropic-native.json")
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		if r.URL.Path != "/v1/completions/resolve" {
			t.Fatalf("path = %q, want /v1/completions/resolve", r.URL.Path)
		}
		if got := r.Header.Get("X-API-Key"); got != "test-key" {
			t.Fatalf("X-API-Key = %q, want test-key", got)
		}
		var got ResolveExecutionProfileRequest
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Fatalf("decode resolve request: %v", err)
		}
		want := ResolveExecutionProfileRequest{
			SchemaVersion:      1,
			ModelTier:          "medium",
			SpecificModel:      "claude-sonnet-5",
			Capability:         "computer_use",
			AllowModelFallback: true,
		}
		if got != want {
			t.Fatalf("resolve request = %+v, want %+v", got, want)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(anthropicNative)
	}))
	defer server.Close()

	gateway := NewGatewayClient(server.URL, "test-key")
	profile, err := gateway.ResolveExecutionProfile(context.Background(), ResolveExecutionProfileRequest{
		SchemaVersion:      1,
		ModelTier:          "medium",
		SpecificModel:      "claude-sonnet-5",
		Capability:         "computer_use",
		AllowModelFallback: true,
	})
	if err != nil {
		t.Fatalf("ResolveExecutionProfile: %v", err)
	}
	if requestCount != 1 {
		t.Fatalf("request count = %d, want 1", requestCount)
	}
	if !profile.IsTrustedResolution() {
		t.Fatal("profile returned by authenticated resolve was not sealed as trusted")
	}
	if got := profile.ProfileID(); got != "ep1_f759f5d61edd8dc5e043498f84c24c67ede6affd0f942a1017db4776164edcff" {
		t.Fatalf("profile id = %q", got)
	}
	if got := profile.Provider(); got != "anthropic" {
		t.Fatalf("provider = %q, want anthropic", got)
	}
	if got := profile.Model(); got != "claude-sonnet-5" {
		t.Fatalf("model = %q, want claude-sonnet-5", got)
	}
	if got := profile.ExecutionMode(); got != ExecutionModeNativeComputer {
		t.Fatalf("execution mode = %q, want %q", got, ExecutionModeNativeComputer)
	}
	if got := profile.ToolContract(); got != ToolContractAnthropicComputer20251124 {
		t.Fatalf("tool contract = %q", got)
	}
}

func TestResolveExecutionProfileRejectsNonCanonicalProfileID(t *testing.T) {
	var body map[string]any
	if err := json.Unmarshal(loadExecutionProfileFixture(t, "profile.openai-generic-ax-only.json"), &body); err != nil {
		t.Fatal(err)
	}
	body["profile_id"] = "ep1_0000000000000000000000000000000000000000000000000000000000000000"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(body)
	}))
	defer server.Close()

	profile, err := NewGatewayClient(server.URL, "").ResolveExecutionProfile(
		context.Background(),
		ResolveExecutionProfileRequest{
			SchemaVersion:      1,
			ModelTier:          "medium",
			Capability:         "computer_use",
			AllowModelFallback: true,
		},
	)
	if profile != nil {
		t.Fatalf("profile = %+v, want nil", profile)
	}
	requireExecutionProfileErrorCode(t, err, ExecutionProfileIDMismatch)
}

func TestGatewayCompletionCarriesProfileIDAndRejectsMissingEchoBeforeToolExecution(t *testing.T) {
	profile := mustTrustedExecutionProfileFixture(t, loadExecutionProfileFixture(t, "profile.anthropic-native.json"))
	var requestCount int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requestCount++
		var body map[string]json.RawMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode completion: %v", err)
		}
		var gotID string
		if err := json.Unmarshal(body["execution_profile_id"], &gotID); err != nil {
			t.Fatalf("decode execution_profile_id: %v", err)
		}
		if gotID != profile.ProfileID() {
			t.Fatalf("execution_profile_id = %q, want %q", gotID, profile.ProfileID())
		}
		if _, leaked := body["resolved_execution_profile"]; leaked {
			t.Fatal("trusted local profile leaked in completion request")
		}
		_ = json.NewEncoder(w).Encode(CompletionResponse{
			Provider: "anthropic",
			Model:    "claude-sonnet-5",
			ToolCalls: []FunctionCall{{
				ID:        "tool-1",
				Name:      "computer",
				Arguments: json.RawMessage(`{"action":"screenshot"}`),
			}},
		})
	}))
	defer server.Close()

	req := profiledNativeComputerRequest(profile)
	resp, err := NewGatewayClient(server.URL, "").Complete(context.Background(), req)
	requireExecutionProfileErrorCode(t, err, ExecutionProfileResponseMissing)
	if resp != nil {
		t.Fatalf("response = %+v, want nil so tool calls cannot execute", resp)
	}
	if requestCount != 1 {
		t.Fatalf("request count = %d, want 1", requestCount)
	}
}

func TestGatewayCompletionAcceptsOnlyExactExecutionProfileEcho(t *testing.T) {
	anthropicNative := loadExecutionProfileFixture(t, "profile.anthropic-native.json")
	profile := mustTrustedExecutionProfileFixture(t, anthropicNative)
	var echoed ExecutionProfile
	if err := json.Unmarshal(anthropicNative, &echoed); err != nil {
		t.Fatal(err)
	}
	echoed.wire.Model = "claude-opus-4-6"
	var err error
	echoed.wire.ProfileID, err = canonicalExecutionProfileID(echoed.wire)
	if err != nil {
		t.Fatalf("canonical alternate profile: %v", err)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CompletionResponse{
			Provider:         "anthropic",
			Model:            "claude-opus-4-6",
			ExecutionProfile: &echoed,
			ToolCalls: []FunctionCall{{
				ID:        "tool-1",
				Name:      "computer",
				Arguments: json.RawMessage(`{"action":"screenshot"}`),
			}},
		})
	}))
	defer server.Close()

	resp, err := NewGatewayClient(server.URL, "").Complete(
		context.Background(),
		profiledNativeComputerRequest(profile),
	)
	requireExecutionProfileErrorCode(t, err, ExecutionProfileResponseMismatch)
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
}

func TestGatewayStreamingCompletionChecksExecutionProfileEcho(t *testing.T) {
	profile := mustTrustedExecutionProfileFixture(t, loadExecutionProfileFixture(t, "profile.anthropic-native.json"))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `data: {"type":"done","provider":"anthropic","model":"claude-sonnet-5","tool_calls":[{"id":"tool-1","name":"computer","arguments":{"action":"screenshot"}}]}`+"\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	resp, err := NewGatewayClient(server.URL, "").CompleteStream(
		context.Background(),
		profiledNativeComputerRequest(profile),
		nil,
	)
	requireExecutionProfileErrorCode(t, err, ExecutionProfileResponseMissing)
	if resp != nil {
		t.Fatalf("response = %+v, want nil", resp)
	}
}

func TestGatewayAcceptsCanonicalGenericCompletionFixture(t *testing.T) {
	profile := mustTrustedExecutionProfileFixture(
		t,
		loadExecutionProfileFixture(t, "profile.openai-generic-ax-only.json"),
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if surface, ok := body["preferred_api_surface"]; ok {
			t.Errorf("sealed request carried preferred_api_surface = %v", surface)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadExecutionProfileFixture(t, "completion.openai-generic-ax-only.json"))
	}))
	defer server.Close()

	resp, err := NewGatewayClient(server.URL, "").Complete(
		context.Background(),
		profiledGenericComputerUseRequest(profile),
	)
	if err != nil {
		t.Fatalf("Complete canonical generic fixture: %v", err)
	}
	if resp.OutputText != "ok" || resp.ExecutionProfile == nil {
		t.Fatalf("canonical generic completion = %+v", resp)
	}
}

func profiledNativeComputerRequest(profile *ExecutionProfile) CompletionRequest {
	return CompletionRequest{
		Messages:      []Message{{Role: "user", Content: NewTextContent("inspect the screen")}},
		SpecificModel: profile.Model(),
		Tools: []Tool{{
			Type:            NativeComputerToolType,
			Name:            NativeComputerToolName,
			DisplayWidthPx:  1280,
			DisplayHeightPx: 800,
		}},
		ExecutionProfileID:       profile.ProfileID(),
		ResolvedExecutionProfile: profile,
	}
}

func profiledGenericComputerUseRequest(profile *ExecutionProfile) CompletionRequest {
	return CompletionRequest{
		Messages:      []Message{{Role: "user", Content: NewTextContent("inspect the screen")}},
		SpecificModel: profile.Model(),
		Tools: []Tool{{
			Type: "function",
			Function: FunctionDef{
				Name:       "computer_use",
				Parameters: map[string]any{"type": "object"},
			},
		}},
		ExecutionProfileID:       profile.ProfileID(),
		ResolvedExecutionProfile: profile,
	}
}

func mustTrustedExecutionProfileFixture(t *testing.T, fixture []byte) *ExecutionProfile {
	t.Helper()
	var wire executionProfileWire
	if err := json.Unmarshal(fixture, &wire); err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	profile, err := admitResolvedExecutionProfile(wire)
	if err != nil {
		t.Fatalf("admit fixture: %v", err)
	}
	return profile
}

func requireExecutionProfileErrorCode(t *testing.T, err error, want ExecutionProfileErrorCode) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected execution profile error %q", want)
	}
	var profileErr *ExecutionProfileError
	if !errors.As(err, &profileErr) {
		t.Fatalf("error type = %T (%v), want *ExecutionProfileError", err, err)
	}
	if profileErr.Code != want {
		t.Fatalf("error code = %q, want %q (error: %v)", profileErr.Code, want, err)
	}
}
