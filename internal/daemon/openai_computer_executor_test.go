package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

const openAIComputerDaemonContinuationToken = "shct_pOIBMOn2gmZdU7TJZm93xdhEM1SNRTRle-n9A0mz76g"

type openAIComputerDaemonProbeTool struct {
	mu              sync.Mutex
	runs            []string
	preflights      []string
	results         map[string]agent.ToolResult
	afterRun        func(string)
	targetBundleID  string
	targetAppName   string
	defaultMutation bool
}

func (t *openAIComputerDaemonProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{Name: "computer_use"}
}

func (t *openAIComputerDaemonProbeTool) RequiresApproval() bool { return true }

func (t *openAIComputerDaemonProbeTool) DescribeGUIAction(
	_ context.Context,
	args string,
) (agent.GUIActionDescriptor, error) {
	var input struct {
		Action   string `json:"action"`
		Mutation bool   `json:"mutation"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	effect := agent.GUIActionObservation
	path := ""
	if input.Mutation || t.defaultMutation {
		effect = agent.GUIActionMutation
		path = "accessibility"
	}
	return agent.GUIActionDescriptor{
		Participates:   true,
		ActionKind:     input.Action,
		Effect:         effect,
		TargetBundleID: t.targetBundleID,
		TargetAppName:  t.targetAppName,
		ExecutionPath:  path,
	}, nil
}

func (t *openAIComputerDaemonProbeTool) PreflightConsequentialRiskV1(
	_ context.Context,
	args string,
	_ string,
) (tools.ConsequentialRiskPreflightResultV1, error) {
	var input struct {
		Action string `json:"action"`
		Kind   string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return tools.ConsequentialRiskPreflightResultV1{}, err
	}
	t.mu.Lock()
	name := input.Action
	if input.Kind != "" {
		name = input.Kind
	}
	t.preflights = append(t.preflights, name)
	t.mu.Unlock()
	return tools.ConsequentialRiskPreflightResultV1{
		Status: tools.ConsequentialRiskPreflightNoneV1,
	}, nil
}

func (t *openAIComputerDaemonProbeTool) Run(
	_ context.Context,
	args string,
) (agent.ToolResult, error) {
	var input struct {
		Action string `json:"action"`
		Kind   string `json:"kind"`
	}
	if err := json.Unmarshal([]byte(args), &input); err != nil {
		return agent.ToolResult{}, err
	}
	t.mu.Lock()
	name := input.Action
	if input.Kind != "" {
		name = input.Kind
	}
	t.runs = append(t.runs, name)
	result := t.results[name]
	afterRun := t.afterRun
	t.mu.Unlock()
	if afterRun != nil {
		afterRun(name)
	}
	return result, nil
}

func (t *openAIComputerDaemonProbeTool) runNames() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.runs...)
}

func (t *openAIComputerDaemonProbeTool) preflightNames() []string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return append([]string(nil), t.preflights...)
}

type openAIComputerDaemonRuntimeProbe struct {
	tool             *openAIComputerDaemonProbeTool
	actionPlans      []tools.OpenAIComputerActionV1
	observationPlans int
	resolvedApps     []string
	launchedApps     []tools.OpenAIComputerTaskAppV1
}

func (r *openAIComputerDaemonRuntimeProbe) ResolveTaskAppV1(
	_ context.Context,
	app string,
) (tools.OpenAIComputerTaskAppV1, error) {
	r.resolvedApps = append(r.resolvedApps, app)
	return tools.OpenAIComputerTaskAppV1{
		App:      app,
		BundleID: "com.example." + strings.ToLower(app),
	}, nil
}

func (r *openAIComputerDaemonRuntimeProbe) LaunchAndFocusTaskAppsV1(
	_ context.Context,
	apps []tools.OpenAIComputerTaskAppV1,
) error {
	r.launchedApps = append(r.launchedApps, apps...)
	return nil
}

func (r *openAIComputerDaemonRuntimeProbe) PlanOpenAIComputerActionV1(
	_ context.Context,
	action tools.OpenAIComputerActionV1,
) (tools.OpenAIComputerActionPlanV1, error) {
	r.actionPlans = append(r.actionPlans, action)
	mutation := action.Type != tools.OpenAIComputerActionScreenshotV1 &&
		action.Type != tools.OpenAIComputerActionWaitV1
	payload, _ := json.Marshal(map[string]any{
		"action":   action.Type,
		"mutation": mutation,
	})
	return tools.OpenAIComputerActionPlanV1{
		Tool: r.tool, Args: string(payload), Mutation: mutation,
	}, nil
}

func (r *openAIComputerDaemonRuntimeProbe) PlanOpenAIComputerObservationV1(
	description string,
	includeScreenshot bool,
) (tools.OpenAIComputerActionPlanV1, error) {
	r.observationPlans++
	kind := "reobserve"
	if includeScreenshot {
		kind = "final_screenshot"
	}
	payload, _ := json.Marshal(map[string]any{
		"action":      "get_app_state",
		"kind":        kind,
		"description": description,
		"mutation":    false,
	})
	return tools.OpenAIComputerActionPlanV1{
		Tool: r.tool, Args: string(payload), Mutation: false,
	}, nil
}

func trustedOpenAIComputerProfileForDaemon(
	t *testing.T,
) *client.ExecutionProfile {
	t.Helper()
	canonical := map[string]any{
		"schema_version":              1,
		"contract_revision":           1,
		"provider":                    "openai",
		"model":                       "gpt-5.6-sol",
		"api_surface":                 "openai_responses",
		"execution_mode":              "native_computer",
		"tool_contract":               "openai.computer.v1",
		"beta_contract":               nil,
		"supports_image_input":        true,
		"supports_tool_result_images": true,
		"supports_function_tools":     false,
		"supports_batched_actions":    true,
	}
	canonicalJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(canonicalJSON)
	canonical["profile_id"] = "ep1_" + hex.EncodeToString(sum[:])
	profileJSON, err := json.Marshal(canonical)
	if err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(profileJSON)
	}))
	defer server.Close()
	profile, err := client.NewGatewayClient(server.URL, "").ResolveExecutionProfile(
		context.Background(),
		client.ResolveExecutionProfileRequest{
			SchemaVersion: 1,
			SpecificModel: "gpt-5.6-sol",
			Capability:    client.ExecutionProfileCapabilityComputer,
		},
	)
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}
	return profile
}

func trustedOpenAIComputerProvenanceForDaemon(
	t *testing.T,
) tools.OpenAIComputerExecutionProvenanceV1 {
	t.Helper()
	provenance, err := tools.NewOpenAIComputerExecutionProvenanceV1(
		trustedOpenAIComputerProfileForDaemon(t),
		openAIComputerDaemonContinuationToken,
	)
	if err != nil {
		t.Fatalf("NewOpenAIComputerExecutionProvenanceV1: %v", err)
	}
	return provenance
}

func openAIComputerDaemonCall(actions string) []byte {
	return []byte(`{
		"type":"computer_call",
		"provider":"openai",
		"api_surface":"openai_responses",
		"tool_contract":"openai.computer.v1",
		"response_id":"` + openAIComputerDaemonContinuationToken + `",
		"call_id":"call_daemon_001",
		"actions":[` + actions + `],
		"pending_safety_checks":[],
		"status":"completed"
	}`)
}

type openAIComputerDaemonLoopLLM struct {
	responses []*client.CompletionResponse
	requests  []client.CompletionRequest
}

func (l *openAIComputerDaemonLoopLLM) Complete(
	_ context.Context,
	request client.CompletionRequest,
) (*client.CompletionResponse, error) {
	l.requests = append(l.requests, request)
	if len(l.responses) == 0 {
		return nil, errors.New("unexpected extra completion request")
	}
	response := l.responses[0]
	l.responses = l.responses[1:]
	return response, nil
}

func (l *openAIComputerDaemonLoopLLM) CompleteStream(
	ctx context.Context,
	request client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return l.Complete(ctx, request)
}

func openAIComputerDaemonLoopResponse(
	t *testing.T,
	profile *client.ExecutionProfile,
	responseID string,
	contentBlocks string,
	outputText string,
) *client.CompletionResponse {
	t.Helper()
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		t.Fatal(err)
	}
	finishReason := "tool_use"
	if outputText != "" {
		finishReason = "stop"
	}
	payload := []byte(`{
		"provider":"openai",
		"model":"gpt-5.6-sol",
		"request_id":` + mustDaemonJSONQuote(t, responseID) + `,
		"finish_reason":` + mustDaemonJSONQuote(t, finishReason) + `,
		"output_text":` + mustDaemonJSONQuote(t, outputText) + `,
		"execution_profile":` + string(profileJSON) + `,
		"content_blocks":[` + contentBlocks + `],
		"usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}
	}`)
	var response client.CompletionResponse
	if err := json.Unmarshal(payload, &response); err != nil {
		t.Fatalf("decode daemon loop response: %v\n%s", err, payload)
	}
	return &response
}

func mustDaemonJSONQuote(t *testing.T, value string) string {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

type openAIComputerDaemonApprovalHandler struct {
	nullEventHandler
	approvals int
}

func (h *openAIComputerDaemonApprovalHandler) OnApprovalNeeded(
	_ string,
	_ string,
) bool {
	h.approvals++
	return true
}

func newOpenAIComputerDaemonExecutorFixture(
	t *testing.T,
) (
	*daemonOpenAIComputerExecutorV1,
	*tools.OpenAIComputerAdapterV1,
	*openAIComputerDaemonProbeTool,
	*guicontrol.Coordinator,
	*int,
) {
	t.Helper()
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(coordinator, "session-openai", "turn-openai")
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.apple.Notes",
		targetAppName:  "Notes",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "clicked",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"type": {
				Content: "typed",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"reobserve": {Content: "observed"},
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "final-image",
				}},
			},
		},
	}
	approvals := 0
	executor, err := newDaemonOpenAIComputerExecutorV1(
		workflow,
		&openAIComputerDaemonRuntimeProbe{tool: probe},
		trustedOpenAIComputerProvenanceForDaemon(t),
		func(_ context.Context, toolName, args string) bool {
			if toolName != "computer_use" || args == "" {
				t.Fatalf("approval identity = %q / %q", toolName, args)
			}
			approvals++
			return true
		},
	)
	if err != nil {
		t.Fatalf("newDaemonOpenAIComputerExecutorV1: %v", err)
	}
	return executor, tools.NewOpenAIComputerAdapterV1(executor), probe, coordinator, &approvals
}

func autoAcknowledgeOpenAIComputerController(
	t *testing.T,
	coordinator *guicontrol.Coordinator,
) {
	t.Helper()
	go func() {
		ticker := time.NewTicker(time.Millisecond)
		defer ticker.Stop()
		timeout := time.NewTimer(2 * time.Second)
		defer timeout.Stop()
		for {
			select {
			case <-ticker.C:
				active := coordinator.Snapshot().Active
				if active == nil {
					continue
				}
				_, _ = coordinator.Heartbeat(active.LeaseID)
				return
			case <-timeout.C:
				return
			}
		}
	}()
}

func TestDaemonOpenAIComputerExecutorRunsOrderedActionsThroughFreshAuthority(t *testing.T) {
	executor, adapter, probe, coordinator, approvals := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20},`+
				`{"type":"type","text":"redacted"}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if result.ToolResult.IsError || len(result.ToolResult.Images) != 1 {
		t.Fatalf("result = %+v", result.ToolResult)
	}
	if got := probe.runNames(); strings.Join(got, ",") !=
		"click,reobserve,type,final_screenshot" {
		t.Fatalf("execution order = %v", got)
	}
	if got := probe.preflightNames(); strings.Join(got, ",") !=
		"click,reobserve,type,final_screenshot" {
		t.Fatalf("risk preflight order = %v", got)
	}
	if *approvals != 2 {
		t.Fatalf("fresh approvals = %d, want 2", *approvals)
	}
	if executor.finalCaptures != 1 {
		t.Fatalf("final captures = %d, want 1", executor.finalCaptures)
	}
	if active := coordinator.Snapshot().Active; active == nil ||
		active.LeaseID != executor.authority.LeaseID {
		t.Fatalf("batch lease was not retained: %+v", active)
	}
}

func TestDaemonOpenAIComputerBatchRunnerBridgesAgentLoopToGuardedWorkflow(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(coordinator, "session-openai-runner", "turn-openai-runner")
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.apple.Notes",
		targetAppName:  "Notes",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "clicked",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"type": {
				Content: "typed",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"reobserve": {Content: "observed"},
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "ZmluYWwtaW1hZ2U=",
				}},
			},
		},
	}
	runner, err := newDaemonOpenAIComputerBatchRunnerV1(
		workflow,
		&openAIComputerDaemonRuntimeProbe{tool: probe},
		probe,
	)
	if err != nil {
		t.Fatalf("newDaemonOpenAIComputerBatchRunnerV1: %v", err)
	}

	profile := trustedOpenAIComputerProfileForDaemon(t)
	call := openAIComputerDaemonCall(
		`{"type":"click","button":"left","x":10,"y":20},` +
			`{"type":"type","text":"redacted"}`,
	)
	llm := &openAIComputerDaemonLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			openAIComputerDaemonContinuationToken,
			string(call),
			"",
		),
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			"resp_daemon_final",
			`{"type":"text","text":"done"}`,
			"done",
		),
	}}
	handler := &openAIComputerDaemonApprovalHandler{}
	loop := agent.NewAgentLoop(
		llm,
		agent.NewToolRegistry(),
		"medium",
		t.TempDir(),
		4,
		2000,
		200,
		nil,
		nil,
		nil,
	)
	loop.SetSkillDiscovery(false)
	loop.SetSpecificModel(profile.Model())
	loop.SetExecutionProfile(profile)
	loop.SetOpenAIComputerBatchExecutor(runner)
	loop.SetHandler(handler)

	reply, _, err := loop.Run(context.Background(), "perform the batch", nil, nil)
	if err != nil {
		t.Fatalf("AgentLoop.Run: %v", err)
	}
	if reply != "done" || len(llm.requests) != 2 {
		t.Fatalf("reply=%q completion_requests=%d", reply, len(llm.requests))
	}
	if got := probe.runNames(); strings.Join(got, ",") !=
		"click,reobserve,type,final_screenshot" {
		t.Fatalf("execution order = %v", got)
	}
	if handler.approvals != 2 {
		t.Fatalf("fresh approvals = %d, want 2", handler.approvals)
	}
	if active := coordinator.Snapshot().Active; active == nil {
		t.Fatal("one batch lease was not retained through the runner")
	}
}

func TestOpenAIComputerTaskToolKeepsParentOutOfClickTypeAndAppSwitchLoop(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-task",
		"turn-openai-task",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.slack",
		targetAppName:  "Slack",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "clicked",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"type": {
				Content: "typed",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"reobserve": {Content: "observed"},
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "ZmluYWwtaW1hZ2U=",
				}},
			},
		},
	}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	call := openAIComputerDaemonCall(
		`{"type":"click","button":"left","x":10,"y":20},` +
			`{"type":"type","text":"hello"}`,
	)
	llm := &openAIComputerDaemonLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			openAIComputerDaemonContinuationToken,
			string(call),
			"",
		),
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			"resp_daemon_task_final",
			`{"type":"text","text":"done"}`,
			"done",
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	handler := &openAIComputerDaemonApprovalHandler{}
	resolveCalls := 0
	taskTool := &openAIComputerTaskToolV1{
		gateway: llm,
		resolveProfile: func(context.Context) (*client.ExecutionProfile, error) {
			resolveCalls++
			return profile, nil
		},
		childTools:   childTools,
		workflow:     workflow,
		runtime:      runtime,
		approvalTool: probe,
		handler:      handler,
		modelTier:    "large",
		maxIter:      4,
		resultTrunc:  2000,
		argsTrunc:    200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Open Slack, type hello, then switch to Calculator and click 7",`+
			`"apps":["Slack","Calculator"],"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if result.IsError || result.Content != "done" {
		t.Fatalf("task result = %+v", result)
	}
	if resolveCalls != 1 {
		t.Fatalf("lazy profile resolve calls = %d", resolveCalls)
	}
	if got := strings.Join(runtime.resolvedApps, ","); got != "Slack,Calculator" {
		t.Fatalf("resolved apps = %q", got)
	}
	if len(runtime.launchedApps) != 2 ||
		runtime.launchedApps[0].App != "Slack" ||
		runtime.launchedApps[1].App != "Calculator" {
		t.Fatalf("launched apps = %+v", runtime.launchedApps)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,reobserve,type,final_screenshot" {
		t.Fatalf("private execution order = %q", got)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("child completion requests = %d, want one batch + one continuation", len(llm.requests))
	}
	for index, request := range llm.requests {
		if request.SpecificModel != profile.Model() ||
			request.ExecutionProfileID != profile.ProfileID() {
			t.Fatalf("child request %d model/profile = %q/%q", index, request.SpecificModel, request.ExecutionProfileID)
		}
		if len(request.Tools) != 1 ||
			request.Tools[0].Type != client.OpenAINativeComputerToolType {
			t.Fatalf("child request %d tools = %+v", index, request.Tools)
		}
		hasExecutorInstructions := false
		for _, message := range request.Messages {
			if strings.Contains(
				message.Content.Text(),
				"execution_role=private_openai_native_computer",
			) {
				hasExecutorInstructions = true
				break
			}
		}
		if !hasExecutorInstructions {
			t.Fatalf("child request %d lacks private executor instructions", index)
		}
	}
	if blocks := llm.requests[0].Messages[len(llm.requests[0].Messages)-1].Content.Blocks(); len(blocks) != 2 || blocks[1].Type != "image" {
		t.Fatalf("child initial user content = %+v", blocks)
	}
}

func TestOpenAIComputerTaskToolResolverFailureDoesNotTouchDesktop(t *testing.T) {
	probe := &openAIComputerDaemonProbeTool{}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	resolveCalls := 0
	taskTool := &openAIComputerTaskToolV1{
		gateway: &openAIComputerDaemonLoopLLM{},
		resolveProfile: func(context.Context) (*client.ExecutionProfile, error) {
			resolveCalls++
			return nil, errors.New("profile unavailable")
		},
		childTools: agent.NewToolRegistry(),
		workflow: testGUIWorkflow(
			guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{}),
			"session-openai-resolve-failure",
			"turn-openai-resolve-failure",
		),
		runtime:      runtime,
		approvalTool: probe,
	}

	for attempt := 0; attempt < 2; attempt++ {
		result, err := taskTool.Run(
			context.Background(),
			`{"task":"Open Slack and type hello","apps":["Slack"],`+
				`"description":"Complete the desktop task"}`,
		)
		if err != nil {
			t.Fatalf("task Run %d: %v", attempt, err)
		}
		if !result.IsError ||
			!strings.Contains(result.Content, "do not retry computer_use in this turn") ||
			!strings.Contains(result.Content, "no desktop action was attempted") {
			t.Fatalf("task result %d = %+v", attempt, result)
		}
	}
	if resolveCalls != 1 {
		t.Fatalf("profile resolver calls = %d, want one per turn", resolveCalls)
	}
	if len(runtime.resolvedApps) != 0 || len(runtime.launchedApps) != 0 ||
		len(probe.runNames()) != 0 {
		t.Fatalf(
			"resolver failure touched desktop: resolved=%v launched=%v runs=%v",
			runtime.resolvedApps, runtime.launchedApps, probe.runNames(),
		)
	}
}

func TestOpenAIComputerTaskToolSchemaRequiresExplicitAppList(t *testing.T) {
	info := (&openAIComputerTaskToolV1{}).Info()
	if got := strings.Join(info.Required, ","); got != "task,apps,description" {
		t.Fatalf("required fields = %q", got)
	}
}

func TestDaemonOpenAIComputerRuntimeIsDetachedBeforeWorkflowWrapping(t *testing.T) {
	baseline, _, cleanup := tools.RegisterLocalTools(nil, nil)
	defer cleanup()
	profile := trustedOpenAIComputerProfileForDaemon(t)
	registry, err := tools.CloneWithResolvedComputerUseProfileForRun(
		baseline,
		nil,
		profile,
	)
	if err != nil {
		t.Fatalf("resolve run registry: %v", err)
	}
	private, err := detachDaemonOpenAIComputerPrivateRuntimeV1(
		registry,
		profile,
	)
	if err != nil {
		t.Fatalf("detach pre-wrap runtime: %v", err)
	}
	if private == nil || private.runtime == nil || private.approvalCore == nil {
		t.Fatalf("detached private runtime = %+v", private)
	}
	if registry.Has("computer_use") {
		t.Fatal("final run registry retained daemon-private computer_use")
	}
	if !baseline.Has("computer_use") {
		t.Fatal("detaching from run clone mutated baseline registry")
	}

	workflow := testGUIWorkflow(
		guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{}),
		"session-openai-order",
		"turn-openai-order",
	)
	wrapDaemonGUITools(registry, workflow)
	approvalTool, err := wrapDetachedDaemonGUIToolV1(
		private.approvalCore,
		workflow,
	)
	if err != nil {
		t.Fatalf("wrap detached approval tool: %v", err)
	}
	if registry.Has("computer_use") {
		t.Fatal("wrapping detached approval tool re-registered computer_use")
	}
	runner, err := newDaemonOpenAIComputerBatchRunnerV1(
		workflow,
		private.runtime,
		approvalTool,
	)
	if err != nil {
		t.Fatalf("construct post-wrap runner: %v", err)
	}
	if runner.approvalTool != approvalTool {
		t.Fatal("runner did not retain the post-wrap approval tool")
	}
}

func TestDetachedOpenAIComputerRuntimeSurvivesAllowComputerFilter(t *testing.T) {
	baseline, _, cleanup := tools.RegisterLocalTools(nil, nil)
	defer cleanup()
	profile := trustedOpenAIComputerProfileForDaemon(t)
	registry, err := tools.CloneWithResolvedComputerUseProfileForRun(
		baseline,
		nil,
		profile,
	)
	if err != nil {
		t.Fatalf("resolve run registry: %v", err)
	}
	private, err := detachDaemonOpenAIComputerPrivateRuntimeV1(
		registry,
		profile,
	)
	if err != nil {
		t.Fatalf("detach OpenAI runtime: %v", err)
	}
	filtered := tools.ApplyToolFilter(
		registry,
		&agents.Agent{Config: &agents.AgentConfig{
			Tools: &agents.AgentToolsFilter{
				Allow: []string{client.NativeComputerToolName},
			},
		}},
	)
	profile, private = retainDaemonOpenAIComputerPrivateRuntimeV1(
		filtered,
		profile,
		private,
	)
	if profile == nil || private == nil || private.runtime == nil {
		t.Fatal("allowing public computer lost detached OpenAI runtime")
	}
	if filtered.Has("computer_use") {
		t.Fatal("named-agent filter exposed daemon-private computer_use")
	}
}

func TestDetachedOpenAIComputerRuntimeDropsWhenPublicMarkerIsFiltered(t *testing.T) {
	baseline, _, cleanup := tools.RegisterLocalTools(nil, nil)
	defer cleanup()
	profile := trustedOpenAIComputerProfileForDaemon(t)
	registry, err := tools.CloneWithResolvedComputerUseProfileForRun(
		baseline,
		nil,
		profile,
	)
	if err != nil {
		t.Fatalf("resolve run registry: %v", err)
	}
	private, err := detachDaemonOpenAIComputerPrivateRuntimeV1(
		registry,
		profile,
	)
	if err != nil {
		t.Fatalf("detach OpenAI runtime: %v", err)
	}
	filtered := tools.ApplyToolFilter(
		registry,
		&agents.Agent{Config: &agents.AgentConfig{
			Tools: &agents.AgentToolsFilter{
				Allow: []string{"computer_use"},
			},
		}},
	)
	profile, private = retainDaemonOpenAIComputerPrivateRuntimeV1(
		filtered,
		profile,
		private,
	)
	if profile != nil || private != nil {
		t.Fatalf(
			"filtered public marker retained profile=%+v private=%+v",
			profile,
			private,
		)
	}
	if filtered.Has("computer_use") {
		t.Fatal("private function tool leaked through named-agent allowlist")
	}
}

func TestDaemonOpenAIComputerExecutorPauseStopsBeforeNextProviderAction(t *testing.T) {
	executor, adapter, probe, coordinator, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	probe.afterRun = func(action string) {
		if action != "click" {
			return
		}
		_, err := coordinator.Control(guicontrol.ComputerUseControlRequest{
			LeaseID:        executor.authority.LeaseID,
			Action:         guicontrol.ComputerUseControlPause,
			IdempotencyKey: "pause-openai-batch",
		})
		if err != nil {
			t.Fatalf("pause: %v", err)
		}
	}
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20},`+
				`{"type":"type","text":"must-not-run"}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError {
		t.Fatalf("paused batch unexpectedly succeeded: %+v", result.ToolResult)
	}
	if got := probe.runNames(); strings.Join(got, ",") != "click" {
		t.Fatalf("actions after pause = %v", got)
	}
}

func TestDaemonOpenAIComputerExecutorStopRevokesCurrentAndRemainingActions(t *testing.T) {
	executor, adapter, probe, coordinator, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	probe.afterRun = func(action string) {
		if action != "click" {
			return
		}
		_, err := coordinator.Control(guicontrol.ComputerUseControlRequest{
			LeaseID:        executor.authority.LeaseID,
			Action:         guicontrol.ComputerUseControlStop,
			IdempotencyKey: "stop-openai-batch",
		})
		if err != nil {
			t.Fatalf("stop: %v", err)
		}
	}
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20},`+
				`{"type":"type","text":"must-not-run"}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "do not retry automatically") {
		t.Fatalf("stopped batch result = %+v", result.ToolResult)
	}
	if got := probe.runNames(); strings.Join(got, ",") != "click" {
		t.Fatalf("actions after stop = %v", got)
	}
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("stop retained active batch lease: %+v", active)
	}
}

func TestDaemonOpenAIComputerExecutorUncertainCommitNeverContinuesOrRetries(t *testing.T) {
	executor, adapter, probe, _, approvals := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	probe.results["click"] = agent.ToolResult{
		Content: "commit uncertain",
		IsError: true,
		GUIOutcome: &agent.GUIActionOutcome{
			Result:      agent.GUIActionResultCompletedUnverified,
			Phase:       agent.GUIActionPhaseInputCommitted,
			FailureCode: "commit_unknown",
		},
	}
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20},`+
				`{"type":"type","text":"must-not-run"}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "do not retry automatically") {
		t.Fatalf("uncertain result = %+v", result.ToolResult)
	}
	if got := probe.runNames(); strings.Join(got, ",") != "click,final_screenshot" {
		t.Fatalf("uncertain action continued or retried: %v", got)
	}
	if *approvals != 1 {
		t.Fatalf("approval count = %d, want 1", *approvals)
	}
}

func TestDaemonOpenAIComputerExecutorPartialPixelScrollCommitNeverContinuesOrRetries(t *testing.T) {
	executor, adapter, probe, _, approvals := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	probe.results["scroll"] = agent.ToolResult{
		Content: "pointer move committed; scroll not committed",
		GUIOutcome: &agent.GUIActionOutcome{
			Result:      agent.GUIActionResultCompletedUnverified,
			Phase:       agent.GUIActionPhaseInputCommitted,
			FailureCode: "scroll_not_committed",
		},
	}
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"scroll","x":10,"y":20,"scroll_x":37,"scroll_y":-618},`+
				`{"type":"type","text":"must-not-run"}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "do not retry automatically") {
		t.Fatalf("partial scroll result = %+v", result.ToolResult)
	}
	if got := probe.runNames(); strings.Join(got, ",") != "scroll,final_screenshot" {
		t.Fatalf("partial scroll continued or retried: %v", got)
	}
	if *approvals != 1 {
		t.Fatalf("approval count = %d, want 1", *approvals)
	}
}

func TestDaemonOpenAIComputerExecutorRejectsMissingOrWrongProvenanceBeforeLease(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(coordinator, "session-openai-bad", "turn-openai-bad")
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	runtime := &openAIComputerDaemonRuntimeProbe{
		tool: &openAIComputerDaemonProbeTool{
			targetBundleID: "com.apple.Notes",
			results:        map[string]agent.ToolResult{},
		},
	}
	if executor, err := newDaemonOpenAIComputerExecutorV1(
		workflow,
		runtime,
		tools.OpenAIComputerExecutionProvenanceV1{},
		func(context.Context, string, string) bool { return true },
	); err == nil || executor != nil {
		t.Fatalf("zero provenance accepted: executor=%v err=%v", executor, err)
	}
	if active := coordinator.Snapshot().Active; active != nil {
		t.Fatalf("invalid provenance acquired a lease: %+v", active)
	}

	executor, err := newDaemonOpenAIComputerExecutorV1(
		workflow,
		runtime,
		trustedOpenAIComputerProvenanceForDaemon(t),
		func(context.Context, string, string) bool { return true },
	)
	if err != nil {
		t.Fatal(err)
	}
	call, err := tools.DecodeOpenAIComputerCallV1(
		openAIComputerDaemonCall(`{"type":"wait"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := executor.AcquireOpenAIComputerBatchAuthorityV1(
		context.Background(),
		call,
	)
	if err != nil {
		t.Fatal(err)
	}
	other := call
	other.CallID = "call_other"
	if _, err := executor.CaptureFinalOpenAIComputerObservationV1(
		context.Background(),
		authority,
		other,
	); err == nil {
		t.Fatal("mismatched call provenance reached final capture")
	}
}

func TestDaemonOpenAIComputerExecutorRejectsMalformedDirectCallBeforePlannerOrLease(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-malformed",
		"turn-openai-malformed",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	runtime := &openAIComputerDaemonRuntimeProbe{
		tool: &openAIComputerDaemonProbeTool{
			targetBundleID: "com.apple.Notes",
			results:        map[string]agent.ToolResult{},
		},
	}
	executor, err := newDaemonOpenAIComputerExecutorV1(
		workflow,
		runtime,
		trustedOpenAIComputerProvenanceForDaemon(t),
		func(context.Context, string, string) bool { return true },
	)
	if err != nil {
		t.Fatal(err)
	}
	call := tools.OpenAIComputerCallV1{
		Type:         tools.OpenAIComputerCallTypeV1,
		Provider:     tools.OpenAIComputerProviderV1,
		APISurface:   client.APISurfaceOpenAIResponses,
		ToolContract: client.ToolContractOpenAIComputerV1,
		ResponseID:   openAIComputerDaemonContinuationToken,
		CallID:       "call_malformed_direct",
		Actions: []tools.OpenAIComputerActionV1{{
			Type: tools.OpenAIComputerActionClickV1,
			// Direct interface callers must not bypass required x/y validation.
			Button: "left",
		}},
		PendingSafetyChecks: []client.OpenAIComputerSafetyCheck{},
		Status:              tools.OpenAIComputerCallStatusCompletedV1,
	}
	if authority, err := executor.AcquireOpenAIComputerBatchAuthorityV1(
		context.Background(),
		call,
	); err == nil || authority.LeaseID != "" {
		t.Fatalf("malformed direct call accepted: authority=%+v err=%v", authority, err)
	}
	if runtime.observationPlans != 0 || len(runtime.actionPlans) != 0 ||
		coordinator.Snapshot().Active != nil {
		t.Fatalf(
			"malformed call reached planner/lease: observation_plans=%d action_plans=%d active=%+v",
			runtime.observationPlans,
			len(runtime.actionPlans),
			coordinator.Snapshot().Active,
		)
	}
}

func TestDaemonOpenAIComputerExecutorFinalCaptureIsSingleUse(t *testing.T) {
	executor, _, _, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	call, err := tools.DecodeOpenAIComputerCallV1(
		openAIComputerDaemonCall(`{"type":"wait"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := executor.AcquireOpenAIComputerBatchAuthorityV1(
		context.Background(),
		call,
	)
	if err != nil {
		t.Fatal(err)
	}
	first, err := executor.CaptureFinalOpenAIComputerObservationV1(
		context.Background(),
		authority,
		call,
	)
	if err != nil || len(first.Images) != 1 {
		t.Fatalf("first final capture = %+v / %v", first, err)
	}
	if _, err := executor.CaptureFinalOpenAIComputerObservationV1(
		context.Background(),
		authority,
		call,
	); err == nil {
		t.Fatal("second final capture was accepted")
	}
}

func TestDaemonOpenAIComputerExecutorRejectsSameTypeTamperedActionBeforePlanningOrTool(t *testing.T) {
	executor, _, probe, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	runtime := executor.runtime.(*openAIComputerDaemonRuntimeProbe)
	call, err := tools.DecodeOpenAIComputerCallV1(
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := executor.AcquireOpenAIComputerBatchAuthorityV1(
		context.Background(),
		call,
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := tools.OpenAIComputerActionScopeV1{
		ResponseID: call.ResponseID, CallID: call.CallID, Provider: call.Provider,
		APISurface: call.APISurface, ToolContract: call.ToolContract,
		ActionID: call.CallID + "/action/1", ActionIndex: 0, ActionCount: 1,
	}
	tampered := call.Actions[0]
	otherX := 999
	tampered.X = &otherX
	beforeObservationPlans := runtime.observationPlans
	execution, err := executor.ExecuteAuthorizedOpenAIComputerActionV1(
		context.Background(),
		authority,
		scope,
		tampered,
	)
	if err == nil ||
		execution.CommitState != tools.OpenAIComputerNotCommittedV1 {
		t.Fatalf("tampered action accepted: execution=%+v err=%v", execution, err)
	}
	if len(runtime.actionPlans) != 0 ||
		runtime.observationPlans != beforeObservationPlans ||
		len(probe.runNames()) != 0 ||
		executor.finalCaptures != 0 {
		t.Fatalf(
			"tampered action reached planner/tool/final: action_plans=%d observation_plans=%d/%d runs=%v final=%d",
			len(runtime.actionPlans),
			runtime.observationPlans,
			beforeObservationPlans,
			probe.runNames(),
			executor.finalCaptures,
		)
	}
}

func TestDaemonOpenAIComputerExecutorDeepFreezesProviderActionBackingStorage(t *testing.T) {
	tests := []struct {
		name       string
		actionJSON string
		mutate     func(*tools.OpenAIComputerActionV1)
	}{
		{
			name:       "coordinate pointer",
			actionJSON: `{"type":"click","button":"left","x":10,"y":20}`,
			mutate: func(action *tools.OpenAIComputerActionV1) {
				*action.X = 999
			},
		},
		{
			name: "drag path",
			actionJSON: `{"type":"drag","path":[` +
				`{"x":10,"y":20},{"x":30,"y":40}]}`,
			mutate: func(action *tools.OpenAIComputerActionV1) {
				action.Path[0].X = 999
			},
		},
		{
			name:       "keypress keys",
			actionJSON: `{"type":"keypress","keys":["META","P"]}`,
			mutate: func(action *tools.OpenAIComputerActionV1) {
				action.Keys[0] = "ALT"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, _, probe, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
			defer executor.EndBatchV1()
			runtime := executor.runtime.(*openAIComputerDaemonRuntimeProbe)
			call, err := tools.DecodeOpenAIComputerCallV1(
				openAIComputerDaemonCall(test.actionJSON),
			)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := executor.AcquireOpenAIComputerBatchAuthorityV1(
				context.Background(),
				call,
			)
			if err != nil {
				t.Fatal(err)
			}
			test.mutate(&call.Actions[0])
			scope := tools.OpenAIComputerActionScopeV1{
				ResponseID: call.ResponseID, CallID: call.CallID, Provider: call.Provider,
				APISurface: call.APISurface, ToolContract: call.ToolContract,
				ActionID: call.CallID + "/action/1", ActionIndex: 0, ActionCount: 1,
			}
			beforeObservationPlans := runtime.observationPlans
			execution, err := executor.ExecuteAuthorizedOpenAIComputerActionV1(
				context.Background(),
				authority,
				scope,
				call.Actions[0],
			)
			if err == nil ||
				execution.CommitState != tools.OpenAIComputerNotCommittedV1 {
				t.Fatalf("in-place mutated action accepted: execution=%+v err=%v", execution, err)
			}
			if len(runtime.actionPlans) != 0 ||
				runtime.observationPlans != beforeObservationPlans ||
				len(probe.runNames()) != 0 {
				t.Fatalf(
					"in-place mutation reached planner/tool: action_plans=%d observation_plans=%d/%d runs=%v",
					len(runtime.actionPlans),
					runtime.observationPlans,
					beforeObservationPlans,
					probe.runNames(),
				)
			}
		})
	}
}

func TestDaemonOpenAIComputerExecutorRejectsTamperedActionSlicesAndText(t *testing.T) {
	tests := []struct {
		name   string
		action string
		tamper func(*tools.OpenAIComputerActionV1)
	}{
		{
			name:   "keypress keys",
			action: `{"type":"keypress","keys":["META","A"]}`,
			tamper: func(action *tools.OpenAIComputerActionV1) {
				action.Keys = append([]string(nil), action.Keys...)
				action.Keys[1] = "B"
			},
		},
		{
			name:   "drag path",
			action: `{"type":"drag","path":[{"x":1,"y":2},{"x":3,"y":4}]}`,
			tamper: func(action *tools.OpenAIComputerActionV1) {
				action.Path = append([]tools.OpenAIComputerPointV1(nil), action.Path...)
				action.Path[1].X = 30
			},
		},
		{
			name:   "typed text",
			action: `{"type":"type","text":"original"}`,
			tamper: func(action *tools.OpenAIComputerActionV1) {
				action.Text = "tampered"
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			executor, _, probe, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
			defer executor.EndBatchV1()
			runtime := executor.runtime.(*openAIComputerDaemonRuntimeProbe)
			call, err := tools.DecodeOpenAIComputerCallV1(
				openAIComputerDaemonCall(test.action),
			)
			if err != nil {
				t.Fatal(err)
			}
			authority, err := executor.AcquireOpenAIComputerBatchAuthorityV1(
				context.Background(),
				call,
			)
			if err != nil {
				t.Fatal(err)
			}
			scope := tools.OpenAIComputerActionScopeV1{
				ResponseID: call.ResponseID, CallID: call.CallID, Provider: call.Provider,
				APISurface: call.APISurface, ToolContract: call.ToolContract,
				ActionID:    call.CallID + "/action/1",
				ActionIndex: 0, ActionCount: 1,
			}
			tampered := call.Actions[0]
			test.tamper(&tampered)
			if execution, err := executor.ExecuteAuthorizedOpenAIComputerActionV1(
				context.Background(),
				authority,
				scope,
				tampered,
			); err == nil ||
				execution.CommitState != tools.OpenAIComputerNotCommittedV1 {
				t.Fatalf("tampered action accepted: execution=%+v err=%v", execution, err)
			}
			if len(runtime.actionPlans) != 0 || len(probe.runNames()) != 0 ||
				executor.finalCaptures != 0 {
				t.Fatalf(
					"tampered action reached planner/tool/final: plans=%d runs=%v final=%d",
					len(runtime.actionPlans),
					probe.runNames(),
					executor.finalCaptures,
				)
			}
		})
	}
}

func TestDaemonOpenAIComputerExecutorConcurrentSameScopeExecutesAtMostOnce(t *testing.T) {
	executor, _, probe, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	call, err := tools.DecodeOpenAIComputerCallV1(
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := executor.AcquireOpenAIComputerBatchAuthorityV1(
		context.Background(),
		call,
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := tools.OpenAIComputerActionScopeV1{
		ResponseID: call.ResponseID, CallID: call.CallID, Provider: call.Provider,
		APISurface: call.APISurface, ToolContract: call.ToolContract,
		ActionID: call.CallID + "/action/1", ActionIndex: 0, ActionCount: 1,
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	probe.afterRun = func(action string) {
		if action == "click" {
			startOnce.Do(func() { close(started) })
			<-release
		}
	}
	type outcome struct {
		execution tools.OpenAIComputerActionExecutionV1
		err       error
	}
	results := make(chan outcome, 2)
	run := func() {
		execution, runErr := executor.ExecuteAuthorizedOpenAIComputerActionV1(
			context.Background(),
			authority,
			scope,
			call.Actions[0],
		)
		results <- outcome{execution: execution, err: runErr}
	}
	go run()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first action did not enter tool")
	}
	go run()
	time.Sleep(30 * time.Millisecond)
	if got := probe.runNames(); len(got) != 1 {
		close(release)
		t.Fatalf("same action scope entered tool %d times concurrently: %v", len(got), got)
	}
	close(release)
	first, second := <-results, <-results
	successes := 0
	rejections := 0
	for _, result := range []outcome{first, second} {
		if result.err == nil &&
			result.execution.CommitState == tools.OpenAIComputerCommitVerifiedV1 {
			successes++
		}
		if result.err != nil &&
			result.execution.CommitState == tools.OpenAIComputerNotCommittedV1 {
			rejections++
		}
	}
	if successes != 1 || rejections != 1 || len(probe.runNames()) != 1 {
		t.Fatalf(
			"concurrent scope outcomes: first=%+v second=%+v runs=%v",
			first,
			second,
			probe.runNames(),
		)
	}
}

func TestDaemonOpenAIComputerExecutorFinalCaptureWaitsForInflightAction(t *testing.T) {
	executor, _, probe, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	call, err := tools.DecodeOpenAIComputerCallV1(
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := executor.AcquireOpenAIComputerBatchAuthorityV1(
		context.Background(),
		call,
	)
	if err != nil {
		t.Fatal(err)
	}
	scope := tools.OpenAIComputerActionScopeV1{
		ResponseID: call.ResponseID, CallID: call.CallID, Provider: call.Provider,
		APISurface: call.APISurface, ToolContract: call.ToolContract,
		ActionID: call.CallID + "/action/1", ActionIndex: 0, ActionCount: 1,
	}
	started := make(chan struct{})
	release := make(chan struct{})
	var startOnce sync.Once
	probe.afterRun = func(action string) {
		if action == "click" {
			startOnce.Do(func() { close(started) })
			<-release
		}
	}
	actionDone := make(chan error, 1)
	go func() {
		_, runErr := executor.ExecuteAuthorizedOpenAIComputerActionV1(
			context.Background(),
			authority,
			scope,
			call.Actions[0],
		)
		actionDone <- runErr
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("action did not enter tool")
	}
	finalDone := make(chan error, 1)
	go func() {
		_, captureErr := executor.CaptureFinalOpenAIComputerObservationV1(
			context.Background(),
			authority,
			call,
		)
		finalDone <- captureErr
	}()
	time.Sleep(30 * time.Millisecond)
	if got := probe.runNames(); len(got) != 1 || got[0] != "click" {
		close(release)
		t.Fatalf("final capture crossed in-flight action boundary: %v", got)
	}
	close(release)
	if err := <-actionDone; err != nil {
		t.Fatalf("action failed: %v", err)
	}
	if err := <-finalDone; err != nil {
		t.Fatalf("final capture failed after action quiesced: %v", err)
	}
	if got := probe.runNames(); strings.Join(got, ",") != "click,final_screenshot" {
		t.Fatalf("final execution order = %v", got)
	}
}

func TestDaemonOpenAIComputerExecutorRejectsMissingApprovalSeam(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(coordinator, "session-openai-approval", "turn-openai-approval")
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	executor, err := newDaemonOpenAIComputerExecutorV1(
		workflow,
		&openAIComputerDaemonRuntimeProbe{tool: &openAIComputerDaemonProbeTool{}},
		trustedOpenAIComputerProvenanceForDaemon(t),
		nil,
	)
	if err == nil || executor != nil {
		t.Fatalf("missing approval seam accepted: executor=%v err=%v", executor, err)
	}
}

func TestDaemonOpenAIComputerExecutorOrdinaryApprovalDenialDoesNotCommit(t *testing.T) {
	executor, adapter, probe, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	executor.approve = func(context.Context, string, string) bool { return false }
	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(`{"type":"click","button":"left","x":10,"y":20}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "action 1 of 1") {
		t.Fatalf("denied result = %+v", result.ToolResult)
	}
	if got := probe.runNames(); len(got) != 1 || got[0] != "final_screenshot" {
		t.Fatalf("denied mutation reached tool: %v", got)
	}
}

func TestDaemonOpenAIComputerExecutorRuntimeErrorsAreRedacted(t *testing.T) {
	executor, _, _, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	executor.runtime = openAIComputerRuntimeErrorProbe{
		err: errors.New("secret typed text must not leak"),
	}
	call, err := tools.DecodeOpenAIComputerCallV1(
		openAIComputerDaemonCall(`{"type":"type","text":"private"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	authority, err := executor.AcquireOpenAIComputerBatchAuthorityV1(
		context.Background(),
		call,
	)
	if err == nil || strings.Contains(err.Error(), "secret typed text") ||
		authority.LeaseID != "" {
		t.Fatalf("runtime error leaked or acquired authority: authority=%+v err=%v", authority, err)
	}
}

type openAIComputerRuntimeErrorProbe struct{ err error }

func (p openAIComputerRuntimeErrorProbe) PlanOpenAIComputerActionV1(
	context.Context,
	tools.OpenAIComputerActionV1,
) (tools.OpenAIComputerActionPlanV1, error) {
	return tools.OpenAIComputerActionPlanV1{}, p.err
}

func (p openAIComputerRuntimeErrorProbe) PlanOpenAIComputerObservationV1(
	string,
	bool,
) (tools.OpenAIComputerActionPlanV1, error) {
	return tools.OpenAIComputerActionPlanV1{}, p.err
}
