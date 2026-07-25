package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

const runnerExecutionProfileFixturesDir = "../../docs/desktop-wire-fixtures/execution-profiles-v1"

func loadRunnerExecutionProfileFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(runnerExecutionProfileFixturesDir, name))
	if err != nil {
		t.Fatalf("read execution profile fixture %q: %v", name, err)
	}
	return data
}

func TestPrepareComputerUseRegistryForRunOldCloudFallsBackToGenericNeverLegacyComputer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/completions/resolve" {
			http.NotFound(w, r)
			return
		}
		t.Fatalf("unexpected path %q", r.URL.Path)
	}))
	defer server.Close()

	cfg := &config.Config{ModelTier: "medium"}
	baseline, _, cleanup := tools.RegisterLocalTools(cfg, nil)
	defer cleanup()

	registry, profile, fallback, err := prepareComputerUseRegistryForRun(
		context.Background(),
		client.NewGatewayClient(server.URL, "test-key"),
		baseline,
		cfg,
		runModelIntent{ModelTier: "medium"},
	)
	if err != nil {
		t.Fatalf("prepareComputerUseRegistryForRun: %v", err)
	}
	if !fallback {
		t.Fatal("old Cloud did not select safe generic fallback")
	}
	if profile != nil {
		t.Fatalf("fallback profile = %+v, want nil", profile)
	}
	if _, ok := registry.Get("computer_use"); !ok {
		t.Fatal("old-Cloud fallback lost computer_use")
	}
	if _, ok := registry.Get("computer"); ok {
		t.Fatal("old-Cloud fallback exposed legacy computer")
	}
	// The generic fallback keeps AX execution behind computer_use, but must not
	// expose the old accessibility wrapper as a competing model-visible tool.
	if _, ok := registry.Get("accessibility"); ok {
		t.Fatal("old-Cloud fallback exposed competing accessibility tool")
	}
	if _, ok := registry.Get("applescript"); ok {
		t.Fatal("old-Cloud fallback exposed legacy applescript GUI automation")
	}
}

func TestPrepareComputerUseRegistryForRunFallbackIsNarrowAndFailClosed(t *testing.T) {
	staleProfileError := string(loadRunnerExecutionProfileFixture(t, "error.execution-profile-stale.json"))
	tests := []struct {
		name         string
		status       int
		body         string
		wantFallback bool
		wantError    bool
	}{
		{
			name:         "old Cloud 404",
			status:       http.StatusNotFound,
			body:         `{"detail":"not found"}`,
			wantFallback: true,
		},
		{
			name:         "old Cloud 405",
			status:       http.StatusMethodNotAllowed,
			body:         `{"detail":"method not allowed"}`,
			wantFallback: true,
		},
		{
			name:      "authentication failure",
			status:    http.StatusUnauthorized,
			body:      `{"error":{"code":"unauthorized"}}`,
			wantError: true,
		},
		{
			name:      "authorization failure",
			status:    http.StatusForbidden,
			body:      `{"error":{"code":"forbidden"}}`,
			wantError: true,
		},
		{
			name:      "stale profile contract",
			status:    http.StatusConflict,
			body:      staleProfileError,
			wantError: true,
		},
		{
			name:      "Cloud server failure",
			status:    http.StatusInternalServerError,
			body:      `{"error":{"code":"internal"}}`,
			wantError: true,
		},
		{
			name:      "malformed successful response",
			status:    http.StatusOK,
			body:      `{"provider":"openai","model":"gpt-5-mini-2025-08-07"}`,
			wantError: true,
		},
		{
			name:   "noncanonical successful response",
			status: http.StatusOK,
			body: `{"schema_version":1,"contract_revision":1,"profile_id":"ep1_0000000000000000000000000000000000000000000000000000000000000000",` +
				`"provider":"openai","model":"gpt-5-mini-2025-08-07","api_surface":"openai_chat_completions","execution_mode":"function_computer_use",` +
				`"tool_contract":"kocoro.computer_use.v1","beta_contract":null,"supports_image_input":true,"supports_tool_result_images":false,` +
				`"supports_function_tools":true,"supports_batched_actions":false}`,
			wantError: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer server.Close()
			cfg := &config.Config{ModelTier: "medium"}
			baseline, _, cleanup := tools.RegisterLocalTools(cfg, nil)
			defer cleanup()

			registry, profile, fallback, err := prepareComputerUseRegistryForRun(
				context.Background(),
				client.NewGatewayClient(server.URL, "test-key"),
				baseline,
				cfg,
				runModelIntent{ModelTier: "medium"},
			)
			if tc.wantError {
				if err == nil {
					t.Fatalf("err = nil, want fail-closed error (registry=%p profile=%+v fallback=%t)", registry, profile, fallback)
				}
				if fallback {
					t.Fatal("fail-closed case incorrectly reported compatibility fallback")
				}
				return
			}
			if err != nil {
				t.Fatalf("prepareComputerUseRegistryForRun: %v", err)
			}
			if fallback != tc.wantFallback {
				t.Fatalf("fallback = %t, want %t", fallback, tc.wantFallback)
			}
		})
	}
}

func TestPrepareComputerUseRegistryForRunResolvedGenericIsAXOnlyAndSingleSurface(t *testing.T) {
	openAIGeneric := loadRunnerExecutionProfileFixture(t, "profile.openai-generic-ax-only.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(openAIGeneric)
	}))
	defer server.Close()

	cfg := &config.Config{ModelTier: "medium"}
	baseline, _, cleanup := tools.RegisterLocalTools(cfg, nil)
	defer cleanup()

	registry, profile, fallback, err := prepareComputerUseRegistryForRun(
		context.Background(),
		client.NewGatewayClient(server.URL, "test-key"),
		baseline,
		cfg,
		runModelIntent{ModelTier: "medium"},
	)
	if err != nil {
		t.Fatalf("prepareComputerUseRegistryForRun: %v", err)
	}
	if fallback {
		t.Fatal("valid resolved generic profile used fallback")
	}
	if profile == nil || !profile.IsTrustedResolution() {
		t.Fatal("resolved generic profile is not trusted")
	}
	if profile.Model() != "gpt-5-mini-2025-08-07" {
		t.Fatalf("resolved model = %q", profile.Model())
	}
	public, ok := registry.Get("computer_use")
	if !ok {
		t.Fatal("resolved generic profile lost computer_use")
	}
	if _, ok := registry.Get("computer"); ok {
		t.Fatal("resolved generic profile exposed legacy computer")
	}
	if _, ok := registry.Get("accessibility"); ok {
		t.Fatal("resolved generic profile exposed competing accessibility tool")
	}
	result, runErr := public.Run(
		context.Background(),
		`{"action":"screenshot","description":"inspect"}`,
	)
	if runErr != nil {
		t.Fatalf("AX-only rejection returned Go error: %v", runErr)
	}
	if !result.IsError || len(result.Images) != 0 {
		t.Fatalf("AX-only screenshot result = %+v", result)
	}
}

func TestPrepareComputerUseRegistryForRunResolvedAnthropicUsesOnlyNativeAlias(t *testing.T) {
	anthropicNative := loadRunnerExecutionProfileFixture(t, "profile.anthropic-native.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(anthropicNative)
	}))
	defer server.Close()

	cfg := &config.Config{ModelTier: "medium"}
	baseline, _, cleanup := tools.RegisterLocalTools(cfg, nil)
	defer cleanup()

	registry, profile, fallback, err := prepareComputerUseRegistryForRun(
		context.Background(),
		client.NewGatewayClient(server.URL, "test-key"),
		baseline,
		cfg,
		runModelIntent{ModelTier: "medium"},
	)
	if err != nil {
		t.Fatalf("prepareComputerUseRegistryForRun: %v", err)
	}
	if fallback {
		t.Fatal("valid resolved native profile used fallback")
	}
	if profile == nil || profile.ToolContract() != client.ToolContractAnthropicComputer20251124 {
		t.Fatalf("profile = %+v", profile)
	}
	native, ok := registry.Get("computer")
	if !ok {
		t.Fatal("native profile lost computer alias")
	}
	if _, ok := native.(agent.NativeToolProvider); !ok {
		t.Fatalf("computer type = %T, want NativeToolProvider", native)
	}
	if _, ok := registry.Get("computer_use"); ok {
		t.Fatal("native profile exposed competing computer_use")
	}
	if _, ok := registry.Get("accessibility"); ok {
		t.Fatal("native profile exposed competing accessibility tool")
	}
}

func TestPrepareComputerUseRegistryForRunResolvedOpenAIKeepsPrivateRuntimeAndNativeAlias(t *testing.T) {
	openAINative := loadRunnerExecutionProfileFixture(t, "profile.openai-native.json")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(openAINative)
	}))
	defer server.Close()

	cfg := &config.Config{ModelTier: "large"}
	baseline, _, cleanup := tools.RegisterLocalTools(cfg, nil)
	defer cleanup()

	registry, profile, fallback, err := prepareComputerUseRegistryForRun(
		context.Background(),
		client.NewGatewayClient(server.URL, "test-key"),
		baseline,
		cfg,
		runModelIntent{ModelTier: "large", SpecificModel: "gpt-5.6-sol"},
	)
	if err != nil {
		t.Fatalf("prepareComputerUseRegistryForRun: %v", err)
	}
	if fallback {
		t.Fatal("valid OpenAI native profile used fallback")
	}
	if profile == nil || profile.ToolContract() != client.ToolContractOpenAIComputerV1 {
		t.Fatalf("profile = %+v", profile)
	}
	native, ok := registry.Get(client.NativeComputerToolName)
	if !ok {
		t.Fatal("OpenAI native profile lost computer alias")
	}
	definer, ok := native.(agent.NativeToolProvider)
	if !ok || definer.NativeToolDef() == nil ||
		definer.NativeToolDef().Type != "computer" {
		t.Fatalf("OpenAI computer type = %T definition=%+v", native, definer)
	}
	if _, ok := registry.Get("computer_use"); !ok {
		t.Fatal("OpenAI native profile lost private guarded computer_use runtime")
	}
	if _, ok := registry.Get("accessibility"); ok {
		t.Fatal("OpenAI native profile exposed competing accessibility tool")
	}
}

func TestRunAgentPinsResolvedProfileOnActualStreamingCompletion(t *testing.T) {
	openAIGeneric := loadRunnerExecutionProfileFixture(t, "profile.openai-generic-ax-only.json")
	streamResponse := loadRunnerExecutionProfileFixture(t, "stream.openai-generic-ax-only.sse")
	var mu sync.Mutex
	var completionRequests []client.CompletionRequest
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/completions/resolve":
			_, _ = w.Write(openAIGeneric)
		case "/v1/completions":
			var req client.CompletionRequest
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Fatalf("decode completion request: %v", err)
			}
			mu.Lock()
			completionRequests = append(completionRequests, req)
			mu.Unlock()
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write(streamResponse)
		case "/channels":
			_, _ = w.Write([]byte(`[]`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	shannonDir := t.TempDir()
	cfg := &config.Config{
		Provider:  "gateway",
		ModelTier: "medium",
		Agent: config.AgentConfig{
			MaxIterations: 2,
		},
	}
	baseline, _, cleanup := tools.RegisterLocalTools(cfg, nil)
	defer cleanup()
	deps := &ServerDeps{
		Config:       cfg,
		GW:           client.NewGatewayClient(server.URL, "test-key"),
		Registry:     baseline,
		BaselineReg:  baseline,
		SessionCache: NewSessionCache(shannonDir),
		ShannonDir:   shannonDir,
		AgentsDir:    filepath.Join(shannonDir, "agents"),
	}
	defer deps.SessionCache.CloseAll()

	result, err := RunAgent(
		context.Background(),
		deps,
		RunAgentRequest{
			Text:          "inspect with accessibility if needed",
			Source:        "heartbeat",
			BypassRouting: true,
		},
		nullEventHandler{},
	)
	if err != nil {
		t.Fatalf("RunAgent: %v", err)
	}
	if result == nil || result.Reply != "ok" {
		t.Fatalf("result = %+v", result)
	}

	mu.Lock()
	requests := append([]client.CompletionRequest(nil), completionRequests...)
	mu.Unlock()
	if len(requests) != 1 {
		t.Fatalf("completion requests = %d, want 1", len(requests))
	}
	request := requests[0]
	if request.ExecutionProfileID != "ep1_217889e2bd16ac438ea11979501af6e2a9c3077d324ef0697e8d606d4847d610" {
		t.Fatalf("execution_profile_id = %q", request.ExecutionProfileID)
	}
	if request.SpecificModel != "gpt-5-mini-2025-08-07" {
		t.Fatalf("specific_model = %q", request.SpecificModel)
	}
	var hasComputerUse, hasLegacyComputer, hasAccessibility bool
	for _, schema := range request.Tools {
		switch {
		case schema.Type == "function" && schema.Function.Name == "computer_use":
			hasComputerUse = true
		case schema.Type == "function" && schema.Function.Name == "computer":
			hasLegacyComputer = true
		case schema.Type == "function" && schema.Function.Name == "accessibility":
			hasAccessibility = true
		}
	}
	if !hasComputerUse || hasLegacyComputer || hasAccessibility {
		t.Fatalf(
			"tool surface computer_use=%t legacy_computer=%t accessibility=%t",
			hasComputerUse,
			hasLegacyComputer,
			hasAccessibility,
		)
	}
}
