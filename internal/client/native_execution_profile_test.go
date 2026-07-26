package client

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

func profileFixtureWithDuplicateProvider(
	t *testing.T,
	fixture []byte,
	duplicateKey string,
) []byte {
	t.Helper()
	needle := []byte(`"provider":"anthropic"`)
	replacement := []byte(
		`"provider":"anthropic","` + duplicateKey + `":"anthropic"`,
	)
	duplicated := bytes.Replace(bytes.TrimSpace(fixture), needle, replacement, 1)
	if bytes.Equal(duplicated, bytes.TrimSpace(fixture)) {
		t.Fatal("profile fixture did not contain canonical Anthropic provider member")
	}
	return duplicated
}

func TestResolveExecutionProfileRejectsDuplicateProviderMember(t *testing.T) {
	duplicated := profileFixtureWithDuplicateProvider(
		t,
		loadExecutionProfileFixture(t, "profile.anthropic-native.json"),
		"provider",
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(duplicated)
	}))
	defer server.Close()

	profile, err := NewGatewayClient(server.URL, "").ResolveExecutionProfile(
		context.Background(),
		ResolveExecutionProfileRequest{
			SchemaVersion: 1,
			SpecificModel: "claude-sonnet-5",
			Capability:    ExecutionProfileCapabilityComputer,
		},
	)
	if profile != nil {
		t.Fatalf("duplicate-member resolve profile = %+v, want nil", profile)
	}
	requireExecutionProfileErrorCode(t, err, ExecutionProfileInvalid)
}

func TestResolveExecutionProfileRejectsResponseLargerThan64KiBBeforeDecode(t *testing.T) {
	const maxResolveResponseBytes = 64 * 1024
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(bytes.Repeat([]byte(" "), maxResolveResponseBytes+1))
	}))
	defer server.Close()

	profile, err := NewGatewayClient(server.URL, "").ResolveExecutionProfile(
		context.Background(),
		ResolveExecutionProfileRequest{
			SchemaVersion: 1,
			SpecificModel: "claude-sonnet-5",
			Capability:    ExecutionProfileCapabilityComputer,
		},
	)
	if profile != nil {
		t.Fatalf("oversized resolve profile = %+v, want nil", profile)
	}
	requireExecutionProfileErrorCode(t, err, ExecutionProfileInvalid)
	if !strings.Contains(err.Error(), "exceeds 65536 byte limit") {
		t.Fatalf("oversized resolve error = %v, want explicit size rejection before JSON decode", err)
	}
}

func TestResolveExecutionProfileSendsRequiredOpenAIComputerContract(t *testing.T) {
	var body []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		body, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read request: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(loadExecutionProfileFixture(t, "profile.openai-native.json"))
	}))
	defer server.Close()

	profile, err := NewGatewayClient(server.URL, "").ResolveExecutionProfile(
		context.Background(),
		ResolveExecutionProfileRequest{
			SchemaVersion:        ExecutionProfileSchemaVersion,
			ModelTier:            "medium",
			Capability:           ExecutionProfileCapabilityComputer,
			RequiredToolContract: ToolContractOpenAIComputerV1,
			AllowModelFallback:   true,
		},
	)
	if err != nil {
		t.Fatalf("ResolveExecutionProfile: %v", err)
	}
	if profile.ToolContract() != ToolContractOpenAIComputerV1 {
		t.Fatalf("resolved contract = %q", profile.ToolContract())
	}
	if !bytes.Contains(body, []byte(`"required_tool_contract":"openai.computer.v1"`)) {
		t.Fatalf("resolve body = %s", body)
	}
}

func TestGatewayCompletionRejectsEscapedEquivalentDuplicateProviderBeforeToolExecution(t *testing.T) {
	anthropicNative := loadExecutionProfileFixture(t, "profile.anthropic-native.json")
	profile := mustTrustedExecutionProfileFixture(t, anthropicNative)
	duplicated := profileFixtureWithDuplicateProvider(
		t,
		anthropicNative,
		`pro\u0076ider`,
	)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(
			w,
			`{"provider":"anthropic","model":"claude-sonnet-5","execution_profile":%s,"tool_calls":[{"id":"tool-1","name":"computer","arguments":{"action":"screenshot"}}]}`,
			duplicated,
		)
	}))
	defer server.Close()

	resp, err := NewGatewayClient(server.URL, "").Complete(
		context.Background(),
		profiledNativeComputerRequest(profile),
	)
	if resp != nil {
		t.Fatalf("duplicate-member completion = %+v, want nil before tool execution", resp)
	}
	requireExecutionProfileErrorCode(t, err, ExecutionProfileInvalid)
}

func TestRejectDuplicateJSONMembersRecursesAndNormalizesEscapedKeys(t *testing.T) {
	err := rejectDuplicateJSONMembers(
		[]byte(`{"outer":[{"provider":"anthropic","pro\u0076ider":"anthropic"}]}`),
	)
	if err == nil {
		t.Fatal("nested escaped-equivalent duplicate member was accepted")
	}
}

func TestGatewayExecutionProfilePreflightFailsBeforeHTTP(t *testing.T) {
	profile := mustTrustedExecutionProfileFixture(t, loadExecutionProfileFixture(t, "profile.anthropic-native.json"))
	tests := []struct {
		name   string
		mutate func(*CompletionRequest)
		code   ExecutionProfileErrorCode
	}{
		{
			name: "missing profile id",
			mutate: func(req *CompletionRequest) {
				req.ExecutionProfileID = ""
			},
			code: ExecutionProfileRequired,
		},
		{
			name: "missing resolved profile",
			mutate: func(req *CompletionRequest) {
				req.ResolvedExecutionProfile = nil
			},
			code: ExecutionProfileRequired,
		},
		{
			name: "profile id mismatch",
			mutate: func(req *CompletionRequest) {
				req.ExecutionProfileID = "ep1_0000000000000000000000000000000000000000000000000000000000000000"
			},
			code: ExecutionProfileRequestMismatch,
		},
		{
			name: "specific model mismatch",
			mutate: func(req *CompletionRequest) {
				req.SpecificModel = "claude-opus-4-6"
			},
			code: ExecutionProfileRequestMismatch,
		},
		{
			name: "native profile with function schema",
			mutate: func(req *CompletionRequest) {
				req.Tools = []Tool{{
					Type:     "function",
					Function: FunctionDef{Name: "computer_use"},
				}}
			},
			code: ExecutionProfileRequestMismatch,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				t.Error("execution profile preflight failure reached HTTP server")
			}))
			defer server.Close()

			req := profiledNativeComputerRequest(profile)
			tc.mutate(&req)

			_, err := NewGatewayClient(server.URL, "").Complete(context.Background(), req)
			requireExecutionProfileErrorCode(t, err, tc.code)
			if got := requests.Load(); got != 0 {
				t.Fatalf("HTTP requests = %d, want 0", got)
			}
		})
	}
}

func TestGatewayExecutionProfilePreflightIsSharedByStreaming(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		t.Error("execution profile stream preflight failure reached HTTP server")
	}))
	defer server.Close()

	profile := mustTrustedExecutionProfileFixture(t, loadExecutionProfileFixture(t, "profile.anthropic-native.json"))
	req := profiledNativeComputerRequest(profile)
	req.ResolvedExecutionProfile = nil
	_, err := NewGatewayClient(server.URL, "").CompleteStream(context.Background(), req, nil)
	requireExecutionProfileErrorCode(t, err, ExecutionProfileRequired)
	if got := requests.Load(); got != 0 {
		t.Fatalf("HTTP requests = %d, want 0", got)
	}
}

func TestGatewayOpenAINativeProfileUsesExactResponsesComputerSchema(t *testing.T) {
	wire := executionProfileWire{
		SchemaVersion:            ExecutionProfileSchemaVersion,
		ContractRevision:         ExecutionProfileContractRevision,
		Provider:                 "openai",
		Model:                    "gpt-5.6-sol",
		APISurface:               APISurfaceOpenAIResponses,
		ExecutionMode:            ExecutionModeNativeComputer,
		ToolContract:             ToolContractOpenAIComputerV1,
		BetaContract:             nil,
		SupportsImageInput:       true,
		SupportsToolResultImages: true,
		SupportsFunctionTools:    false,
		SupportsBatchedActions:   true,
	}
	var err error
	wire.ProfileID, err = canonicalExecutionProfileID(wire)
	if err != nil {
		t.Fatal(err)
	}
	profile, err := admitResolvedExecutionProfile(wire)
	if err != nil {
		t.Fatal(err)
	}

	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		var body struct {
			Tools []Tool `json:"tools"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Fatalf("decode OpenAI native request: %v", err)
		}
		if len(body.Tools) != 1 || body.Tools[0].Type != "computer" {
			t.Fatalf("OpenAI native tools = %+v, want exact computer schema", body.Tools)
		}
		_ = json.NewEncoder(w).Encode(CompletionResponse{
			Provider:         "openai",
			Model:            profile.Model(),
			OutputText:       "ok",
			ExecutionProfile: profile,
		})
	}))
	defer server.Close()

	req := profiledNativeComputerRequest(profile)
	req.Tools = []Tool{{Type: "computer"}}
	resp, err := NewGatewayClient(server.URL, "").Complete(context.Background(), req)
	if err != nil {
		t.Fatalf("OpenAI native completion: %v", err)
	}
	if resp == nil || resp.OutputText != "ok" {
		t.Fatalf("OpenAI native completion = %+v", resp)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
}

func TestGatewayFunctionToolsWithoutExecutionProfileRemainBackwardCompatible(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		_ = json.NewEncoder(w).Encode(CompletionResponse{OutputText: "ok"})
	}))
	defer server.Close()

	resp, err := NewGatewayClient(server.URL, "").Complete(
		context.Background(),
		CompletionRequest{
			Messages: []Message{{Role: "user", Content: NewTextContent("ping")}},
			Tools: []Tool{{
				Type:     "function",
				Function: FunctionDef{Name: "bash"},
			}},
		},
	)
	if err != nil {
		t.Fatalf("Complete(function tools) error: %v", err)
	}
	if resp.OutputText != "ok" {
		t.Fatalf("OutputText = %q, want ok", resp.OutputText)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("HTTP requests = %d, want 1", got)
	}
}

func TestGatewayExactExecutionProfileEchoAllowsCompletion(t *testing.T) {
	anthropicNative := loadExecutionProfileFixture(t, "profile.anthropic-native.json")
	profile := mustTrustedExecutionProfileFixture(t, anthropicNative)
	var echo ExecutionProfile
	if err := json.Unmarshal(anthropicNative, &echo); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(CompletionResponse{
			Provider:         "anthropic",
			Model:            "claude-sonnet-5",
			ExecutionProfile: &echo,
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
	if err != nil {
		t.Fatalf("Complete: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
}

func TestGatewayExactExecutionProfileEchoAllowsStreamingCompletion(t *testing.T) {
	anthropicNative := loadExecutionProfileFixture(t, "profile.anthropic-native.json")
	profile := mustTrustedExecutionProfileFixture(t, anthropicNative)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintf(w, "data: {\"type\":\"done\",\"provider\":\"anthropic\",\"model\":\"claude-sonnet-5\",\"execution_profile\":%s,\"tool_calls\":[{\"id\":\"tool-1\",\"name\":\"computer\",\"arguments\":{\"action\":\"screenshot\"}}]}\n\n", bytes.TrimSpace(anthropicNative))
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	resp, err := NewGatewayClient(server.URL, "").CompleteStream(
		context.Background(),
		profiledNativeComputerRequest(profile),
		nil,
	)
	if err != nil {
		t.Fatalf("CompleteStream: %v", err)
	}
	if len(resp.ToolCalls) != 1 {
		t.Fatalf("tool calls = %d, want 1", len(resp.ToolCalls))
	}
}
