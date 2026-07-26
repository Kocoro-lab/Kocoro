package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

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

func TestResolveOpenAIComputerProfileRequestsOnlyNativeWithoutParentModelPin(t *testing.T) {
	var resolveBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var err error
		resolveBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read resolve body: %v", err)
		}
		_, _ = w.Write(loadRunnerExecutionProfileFixture(t, "profile.openai-native.json"))
	}))
	defer server.Close()

	profile, err := resolveOpenAIComputerProfileForTask(
		context.Background(),
		client.NewGatewayClient(server.URL, "test-key"),
		"medium",
	)
	if err != nil {
		t.Fatalf("resolveOpenAIComputerProfileForTask: %v", err)
	}
	if profile == nil ||
		profile.ToolContract() != client.ToolContractOpenAIComputerV1 {
		t.Fatalf("profile=%+v", profile)
	}
	if bytes.Contains(resolveBody, []byte(`"specific_model":"claude-sonnet-5"`)) {
		t.Fatalf("resolve body pinned parent model: %s", resolveBody)
	}
	if !bytes.Contains(resolveBody, []byte(
		`"required_tool_contract":"openai.computer.v1"`,
	)) {
		t.Fatalf("resolve body omitted OpenAI contract: %s", resolveBody)
	}
}

func TestResolveOpenAIComputerProfileSurfacesEveryCloudFailure(t *testing.T) {
	for _, status := range []int{
		http.StatusNotFound,
		http.StatusUnauthorized,
		http.StatusConflict,
		http.StatusInternalServerError,
	} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				http.Error(w, "unavailable", status)
			}))
			defer server.Close()
			profile, err := resolveOpenAIComputerProfileForTask(
				context.Background(),
				client.NewGatewayClient(server.URL, "test-key"),
				"medium",
			)
			if err == nil || profile != nil {
				t.Fatalf("profile=%+v err=%v", profile, err)
			}
		})
	}
}

func TestResolveOpenAIComputerProfileRejectsEveryNonOpenAINativeProfile(t *testing.T) {
	for _, fixture := range []string{
		"profile.anthropic-native.json",
		"profile.openai-generic-ax-only.json",
	} {
		t.Run(fixture, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				_, _ = w.Write(loadRunnerExecutionProfileFixture(t, fixture))
			}))
			defer server.Close()
			profile, err := resolveOpenAIComputerProfileForTask(
				context.Background(),
				client.NewGatewayClient(server.URL, "test-key"),
				"medium",
			)
			if err == nil || profile != nil {
				t.Fatalf("profile=%+v err=%v", profile, err)
			}
		})
	}
}

func TestRunAgentKeepsSonnetParentAndExposesOnlyHighLevelComputerTask(t *testing.T) {
	streamResponse := []byte(
		"data: {\"type\":\"content_delta\",\"text\":\"ok\"}\n\n" +
			"data: {\"type\":\"done\",\"output_text\":\"ok\",\"provider\":\"anthropic\"," +
			"\"model\":\"claude-sonnet-5\",\"usage\":{\"cache_aware_total_tokens\":0,\"billable_tokens\":0}}\n\n" +
			"data: [DONE]\n\n",
	)
	var mu sync.Mutex
	var completionRequests []client.CompletionRequest
	resolveRequests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/completions/resolve":
			mu.Lock()
			resolveRequests++
			mu.Unlock()
			http.Error(w, "ordinary turns must not resolve computer use", http.StatusInternalServerError)
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
			Model:         "claude-sonnet-5",
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
			Text:          "What is 1+1?",
			Source:        "schedule",
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
	resolves := resolveRequests
	mu.Unlock()
	if resolves != 0 {
		t.Fatalf("ordinary Sonnet turn made %d Computer Use resolve request(s)", resolves)
	}
	if len(requests) == 0 {
		t.Fatal("parent sent no completion request")
	}
	request := requests[0]
	if request.SpecificModel != "claude-sonnet-5" {
		t.Fatalf("parent specific_model = %q", request.SpecificModel)
	}
	if request.ExecutionProfileID != "" || request.ResolvedExecutionProfile != nil {
		t.Fatalf("parent leaked child profile: %+v", request)
	}
	var computer *client.Tool
	for index := range request.Tools {
		schema := &request.Tools[index]
		if schema.Type == client.NativeComputerToolType ||
			schema.Function.Name == "accessibility" ||
			schema.Function.Name == "applescript" ||
			schema.Function.Name == "computer" {
			t.Fatalf("parent exposed legacy/native GUI tool: %+v", *schema)
		}
		if schema.Type == "function" && schema.Function.Name == "computer_use" {
			computer = schema
		}
	}
	if computer == nil {
		t.Fatal("parent omitted high-level computer_use")
	}
	properties, _ := computer.Function.Parameters["properties"].(map[string]any)
	if _, ok := properties["task"]; !ok {
		t.Fatalf("computer_use schema = %+v", computer.Function.Parameters)
	}
	if _, leaked := properties["action"]; leaked {
		t.Fatalf("low-level action leaked into parent schema: %+v", properties)
	}
	if _, leaked := properties["state_id"]; leaked {
		t.Fatalf("state_id leaked into parent schema: %+v", properties)
	}
}
