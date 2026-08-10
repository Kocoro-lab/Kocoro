package daemon

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
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

func TestOpenAIComputerTaskOutcomeSeparatesNotCompletedFromUnverified(
	t *testing.T,
) {
	for _, status := range []string{"completed", "not_completed", "unverified"} {
		outcome, err := parseOpenAIComputerTaskOutcomeV1(
			`{"status":"` + status + `","summary":"fixture"}`,
		)
		if err != nil || outcome.Status != status {
			t.Fatalf("status %q outcome=%+v err=%v", status, outcome, err)
		}
	}
	if _, err := parseOpenAIComputerTaskOutcomeV1(
		`{"status":"failed","summary":"ambiguous legacy result"}`,
	); err == nil {
		t.Fatal("legacy failed status still conflates a verified negative with unverified")
	}
}

func TestOpenAIComputerBatchContinuationPolicyUsesStableRecoveryCategory(
	t *testing.T,
) {
	image := agent.ImageBlock{MediaType: "image/png", Data: "final"}
	tests := []struct {
		name      string
		execution agent.OpenAIComputerBatchExecution
		err       error
		want      bool
	}{
		{
			name: "successful batch",
			execution: agent.OpenAIComputerBatchExecution{
				Result: agent.ToolResult{Images: []agent.ImageBlock{image}},
			},
			want: true,
		},
		{
			name: "preflight same app drift can reobserve from fresh image",
			execution: agent.OpenAIComputerBatchExecution{
				Result: agent.ToolResult{
					IsError: true,
					Images:  []agent.ImageBlock{image},
					GUIOutcome: &agent.GUIActionOutcome{
						Result:      agent.GUIActionResultFailed,
						Phase:       agent.GUIActionPhaseObserving,
						FailureCode: "frontmost_window_mismatch",
					},
				},
			},
			want: true,
		},
		{
			name: "precommit physical user input reobserves from fresh image",
			execution: agent.OpenAIComputerBatchExecution{
				ActionEffect: agent.ComputerUseCommitNone,
				Result: agent.ToolResult{
					IsError: true,
					Images:  []agent.ImageBlock{image},
					GUIOutcome: &agent.GUIActionOutcome{
						Result:      agent.GUIActionResultUserInterference,
						Phase:       agent.GUIActionPhaseActing,
						FailureCode: "pointer_interference",
					},
				},
			},
			want: true,
		},
		{
			name: "precommit lane failure can replan from fresh image",
			execution: agent.OpenAIComputerBatchExecution{
				Result: agent.ToolResult{
					IsError: true,
					Images:  []agent.ImageBlock{image},
					GUIOutcome: &agent.GUIActionOutcome{
						Result:      agent.GUIActionResultFailed,
						Phase:       agent.GUIActionPhaseActing,
						FailureCode: "background_target_became_frontmost",
					},
				},
			},
			want: true,
		},
		{
			name: "unknown commit with fresh image returns to specialist",
			execution: agent.OpenAIComputerBatchExecution{
				ActionEffect: agent.ComputerUseCommitUnknown,
				Result: agent.ToolResult{
					IsError: true,
					Images:  []agent.ImageBlock{image},
				},
			},
			want: true,
		},
		{
			name: "explicit cancellation after commit refuses continuation",
			execution: agent.OpenAIComputerBatchExecution{
				ActionEffect: agent.ComputerUseCommitUnknown,
				Result: agent.ToolResult{
					IsError: true,
					Images:  []agent.ImageBlock{image},
					GUIOutcome: &agent.GUIActionOutcome{
						Result:      agent.GUIActionResultCancelled,
						Phase:       agent.GUIActionPhaseInputCommitted,
						FailureCode: "control_cancelled",
					},
				},
			},
		},
		{
			name: "preflight failure without fresh screenshot stops",
			execution: agent.OpenAIComputerBatchExecution{
				Result: agent.ToolResult{
					IsError: true,
					GUIOutcome: &agent.GUIActionOutcome{
						Result:      agent.GUIActionResultFailed,
						Phase:       agent.GUIActionPhaseObserving,
						FailureCode: "frontmost_window_mismatch",
					},
				},
			},
		},
		{
			name: "executor error stops",
			execution: agent.OpenAIComputerBatchExecution{
				Result: agent.ToolResult{Images: []agent.ImageBlock{image}},
			},
			err: errors.New("executor failed"),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := openAIComputerBatchContinuationAllowedV1(
				test.execution,
				test.err,
			); got != test.want {
				t.Fatalf("continuation = %t, want %t", got, test.want)
			}
		})
	}
}

func TestOpenAIComputerCommitStatePreservesUnknownAcknowledgements(t *testing.T) {
	for _, code := range []string{
		"commit_unknown",
		"commit_status_unknown",
		"action_commit_unknown",
		"invalid_helper_result",
	} {
		t.Run(code, func(t *testing.T) {
			result := agent.ToolResult{
				IsError: true,
				GUIOutcome: &agent.GUIActionOutcome{
					Result:      agent.GUIActionResultCompletedUnverified,
					Phase:       agent.GUIActionPhaseInputCommitted,
					FailureCode: code,
				},
			}
			if got := openAIComputerCommitStateV1(result, nil); got != tools.OpenAIComputerCommitUnknownV1 {
				t.Fatalf("commit state = %q", got)
			}
		})
	}
}

func TestOpenAIComputerCommitStateDistinguishesPrecommitIntervention(t *testing.T) {
	for _, result := range []agent.GUIActionResult{
		agent.GUIActionResultUserInterference,
		agent.GUIActionResultCancelled,
	} {
		t.Run(string(result), func(t *testing.T) {
			precommit := agent.ToolResult{
				IsError: true,
				GUIOutcome: &agent.GUIActionOutcome{
					Result:      result,
					Phase:       agent.GUIActionPhaseActing,
					FailureCode: "pointer_interference",
				},
			}
			if got := openAIComputerCommitStateV1(
				precommit,
				nil,
			); got != tools.OpenAIComputerNotCommittedV1 {
				t.Fatalf("precommit intervention state = %q", got)
			}

			afterCommit := precommit
			afterCommit.GUIOutcome = &agent.GUIActionOutcome{
				Result:      result,
				Phase:       agent.GUIActionPhaseInputCommitted,
				FailureCode: "pointer_interference",
			}
			if got := openAIComputerCommitStateV1(
				afterCommit,
				nil,
			); got != tools.OpenAIComputerCommitUnknownV1 {
				t.Fatalf("post-commit intervention state = %q", got)
			}
		})
	}
}

func TestOpenAIComputerNoEffectInterventionIsNotReportedAsUnknown(t *testing.T) {
	result := openAIComputerNoEffectInterventionResultV1(
		"the user moved the pointer before the click committed",
	)
	if !result.IsError ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskNotCompleted ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitNone ||
		result.ComputerUseOutcome.FailureCode !=
			string(agent.OpenAIComputerRecoveryUserIntervenedV1) ||
		result.ComputerUseOutcome.Recovery != agent.ComputerUseRecoveryNone ||
		strings.Contains(result.Content, "action_effect: unknown") ||
		!strings.Contains(result.Content, "computer_use_error: user_intervened") {
		t.Fatalf("precommit intervention result = %+v", result)
	}
}

func TestOpenAIComputerBatchStatsAccumulateTaskEffectMonotonically(t *testing.T) {
	runner := &daemonOpenAIComputerBatchRunnerV1{}
	runner.recordBatchV1(agent.OpenAIComputerBatchExecution{
		ActionEffect: agent.ComputerUseCommitKnown,
	}, nil)
	runner.recordBatchV1(agent.OpenAIComputerBatchExecution{
		ActionEffect: agent.ComputerUseCommitNone,
	}, nil)
	stats := runner.BatchStatsV1()
	if stats.TaskEffect != agent.ComputerUseCommitKnown ||
		stats.LastBatchEffect != agent.ComputerUseCommitNone {
		t.Fatalf("known then observation stats = %+v", stats)
	}
	runner.recordBatchV1(agent.OpenAIComputerBatchExecution{
		ActionEffect: agent.ComputerUseCommitUnknown,
	}, nil)
	runner.recordBatchV1(agent.OpenAIComputerBatchExecution{
		ActionEffect: agent.ComputerUseCommitKnown,
	}, nil)
	stats = runner.BatchStatsV1()
	if stats.TaskEffect != agent.ComputerUseCommitUnknown {
		t.Fatalf("unknown effect was downgraded: %+v", stats)
	}
}

func TestOpenAIComputerBatchStatsPreserveFailedMutationEvidence(t *testing.T) {
	runner := &daemonOpenAIComputerBatchRunnerV1{}
	failure := agent.BusinessError("target not found")
	failure.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultFailed,
		Phase:       agent.GUIActionPhaseActing,
		FailureCode: "target_not_found",
	}
	runner.recordBatchV1(agent.OpenAIComputerBatchExecution{
		MutationAttempted: true,
		ActionEffect:      agent.ComputerUseCommitNone,
		Result:            failure,
	}, nil)
	runner.recordBatchV1(agent.OpenAIComputerBatchExecution{
		ActionEffect: agent.ComputerUseCommitNone,
		Result:       agent.ToolResult{Content: "observed"},
	}, nil)

	stats := runner.BatchStatsV1()
	if !stats.MutationAttempted ||
		stats.TaskEffect != agent.ComputerUseCommitNone ||
		stats.LastFailureCode != "target_not_found" ||
		!strings.Contains(stats.LastFailureDetail, "target not found") {
		t.Fatalf("failed mutation evidence = %+v", stats)
	}
}

func TestOpenAIComputerTaskUnknownCommitIsStructuredUnverified(t *testing.T) {
	result := openAIComputerTaskUnverifiedResultV1(
		"commit_unknown",
		"helper acknowledgement was lost",
		agent.ComputerUseCommitUnknown,
	)
	if result.IsError ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskUnverified ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitUnknown ||
		!strings.Contains(result.Content, "action_effect: unknown") {
		t.Fatalf("unknown task outcome = %+v", result)
	}
}

func TestOpenAIComputerTaskNoEffectAloneDoesNotReleaseAlternateControl(
	t *testing.T,
) {
	result := openAIComputerTaskUnverifiedResultV1(
		tools.ConsequentialRiskCodeUnsupportedPathV1,
		"the requested submit path was rejected before input",
		agent.ComputerUseCommitNone,
	)
	if result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitNone ||
		result.ComputerUseOutcome.Recovery != agent.ComputerUseRecoveryNone {
		t.Fatalf("no-effect safety rejection released alternate control: %+v", result)
	}
}

type openAIComputerDaemonProbeTool struct {
	mu                  sync.Mutex
	runs                []string
	preflights          []string
	results             map[string]agent.ToolResult
	resultQueues        map[string][]agent.ToolResult
	riskResults         map[string]tools.ConsequentialRiskPreflightResultV1
	afterRun            func(string)
	targetBundleID      string
	targetAppName       string
	targetExecutionPath string
	defaultMutation     bool
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
	if input.Action == tools.OpenAIComputerActionWaitV1 {
		return agent.GUIActionDescriptor{}, nil
	}
	effect := agent.GUIActionObservation
	path := ""
	if input.Mutation || t.defaultMutation {
		effect = agent.GUIActionMutation
		path = t.targetExecutionPath
		if path == "" {
			path = "accessibility"
		}
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
	riskResult, found := t.riskResults[name]
	t.mu.Unlock()
	if found {
		return riskResult, nil
	}
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
	if queue := t.resultQueues[name]; len(queue) > 0 {
		result = queue[0]
		t.resultQueues[name] = queue[1:]
	}
	// Production get_app_state always carries local actionability metadata.
	// Keep shared screenshot fixtures faithful to that contract while tests
	// for visual-only or rejected observations provide an explicit outcome.
	if name == "final_screenshot" &&
		len(result.Images) == 1 &&
		result.GUIObservation == nil {
		result.GUIObservation = &agent.GUIObservationOutcome{
			CoordinateActionable: true,
			SemanticActionable:   true,
		}
	}
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
	tool                   *openAIComputerDaemonProbeTool
	actionPlans            []tools.OpenAIComputerActionV1
	observationPlans       int
	targetRefreshPlanErr   error
	keyboardTargets        int
	keyboardTargetErr      error
	resolveErr             error
	launchErr              error
	initialObservationApps []string
	resolvedApps           []string
	launchedApps           []tools.OpenAIComputerTaskAppV1
	preparationOptions     []tools.OpenAIComputerTaskPreparationOptionsV1
}

type backgroundSemanticDaemonRuntimeProbe struct {
	*openAIComputerDaemonRuntimeProbe
}

func (r *backgroundSemanticDaemonRuntimeProbe) PrepareTaskAppsV1(
	_ context.Context,
	apps []tools.OpenAIComputerTaskAppV1,
	options tools.OpenAIComputerTaskPreparationOptionsV1,
) (tools.OpenAIComputerExecutionLaneV1, error) {
	r.launchedApps = append(r.launchedApps, apps...)
	r.preparationOptions = append(r.preparationOptions, options)
	return tools.OpenAIComputerExecutionBackgroundSemanticV1, r.launchErr
}

func (r *openAIComputerDaemonRuntimeProbe) ResolveTaskAppV1(
	_ context.Context,
	app string,
) (tools.OpenAIComputerTaskAppV1, error) {
	r.resolvedApps = append(r.resolvedApps, app)
	if r.resolveErr != nil {
		return tools.OpenAIComputerTaskAppV1{}, r.resolveErr
	}
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
	return r.launchErr
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
	if !includeScreenshot && r.targetRefreshPlanErr != nil {
		return tools.OpenAIComputerActionPlanV1{}, r.targetRefreshPlanErr
	}
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

func (r *openAIComputerDaemonRuntimeProbe) AuthorizeOpenAIComputerTypeAfterKeypressV1(
	tools.OpenAIComputerActionV1,
) error {
	r.keyboardTargets++
	return r.keyboardTargetErr
}

func (r *openAIComputerDaemonRuntimeProbe) PlanOpenAIComputerTaskInitialObservationV1(
	app *tools.OpenAIComputerTaskAppV1,
	description string,
	includeScreenshot bool,
) (tools.OpenAIComputerActionPlanV1, error) {
	appName := ""
	if app != nil {
		appName = app.App
	}
	r.initialObservationApps = append(r.initialObservationApps, appName)
	return r.PlanOpenAIComputerObservationV1(description, includeScreenshot)
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
	return openAIComputerDaemonCallForResponse(
		openAIComputerDaemonContinuationToken,
		"call_daemon_001",
		actions,
	)
}

func openAIComputerDaemonCallForResponse(
	responseID string,
	callID string,
	actions string,
) []byte {
	return []byte(`{
		"type":"computer_call",
		"provider":"openai",
		"api_surface":"openai_responses",
		"tool_contract":"openai.computer.v1",
		"response_id":"` + responseID + `",
		"call_id":"` + callID + `",
		"actions":[` + actions + `],
		"pending_safety_checks":[],
		"status":"completed"
	}`)
}

type openAIComputerDaemonLoopLLM struct {
	responses []*client.CompletionResponse
	requests  []client.CompletionRequest
	errors    []error
	err       error
}

type openAIComputerDaemonBlockingLLM struct{}

func (openAIComputerDaemonBlockingLLM) Complete(
	ctx context.Context,
	_ client.CompletionRequest,
) (*client.CompletionResponse, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

func (l openAIComputerDaemonBlockingLLM) CompleteStream(
	ctx context.Context,
	request client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return l.Complete(ctx, request)
}

type openAIComputerDaemonInitialResponseLLM struct {
	mu         sync.Mutex
	calls      int
	succeedOn  int
	successful *client.CompletionResponse
}

func (l *openAIComputerDaemonInitialResponseLLM) Complete(
	ctx context.Context,
	_ client.CompletionRequest,
) (*client.CompletionResponse, error) {
	l.mu.Lock()
	l.calls++
	call := l.calls
	successful := l.successful
	succeedOn := l.succeedOn
	l.mu.Unlock()
	if succeedOn > 0 && call >= succeedOn {
		return successful, nil
	}
	<-ctx.Done()
	return nil, ctx.Err()
}

func (l *openAIComputerDaemonInitialResponseLLM) CompleteStream(
	ctx context.Context,
	request client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return l.Complete(ctx, request)
}

func (l *openAIComputerDaemonInitialResponseLLM) callCount() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.calls
}

func (l *openAIComputerDaemonLoopLLM) Complete(
	_ context.Context,
	request client.CompletionRequest,
) (*client.CompletionResponse, error) {
	l.requests = append(l.requests, request)
	if len(l.errors) > 0 {
		err := l.errors[0]
		l.errors = l.errors[1:]
		if err != nil {
			return nil, err
		}
	}
	if l.err != nil {
		return nil, l.err
	}
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
	usage     agent.UsageAccumulator
}

func (h *openAIComputerDaemonApprovalHandler) OnApprovalNeeded(
	_ string,
	_ string,
) bool {
	h.approvals++
	return true
}

func (h *openAIComputerDaemonApprovalHandler) OnUsage(usage agent.TurnUsage) {
	h.usage.Add(usage)
}

func TestOpenAIComputerChildHandlerTracksAndForwardsUsage(t *testing.T) {
	parent := &openAIComputerDaemonApprovalHandler{}
	local := &agent.UsageAccumulator{}
	handler := openAIComputerChildHandlerV1{
		parent: parent,
		usage:  local,
	}
	delta := agent.LLMUsageDelta(client.Usage{
		InputTokens:           120,
		OutputTokens:          30,
		TotalTokens:           150,
		CostUSD:               0.42,
		CacheReadTokens:       80,
		CacheCreation5mTokens: 20,
		CacheCreation1hTokens: 40,
	}, "gpt-test")

	handler.OnUsage(delta)

	for name, got := range map[string]agent.TurnUsage{
		"local":  local.Snapshot().LLM,
		"parent": parent.usage.Snapshot().LLM,
	} {
		if got.LLMCalls != 1 || got.InputTokens != 120 ||
			got.OutputTokens != 30 || got.TotalTokens != 150 ||
			got.CostUSD != 0.42 || got.CacheReadTokens != 80 ||
			got.CacheCreationTokens != 60 ||
			got.CacheCreation5mTokens != 20 ||
			got.CacheCreation1hTokens != 40 {
			t.Fatalf("%s usage = %+v", name, got)
		}
	}
}

func newOpenAIComputerDaemonExecutorFixture(
	t *testing.T,
) (
	*daemonOpenAIComputerExecutorV1,
	*tools.OpenAIComputerAdapterV1,
	*openAIComputerDaemonProbeTool,
	*guicontrol.Coordinator,
) {
	t.Helper()
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(coordinator, "session-openai", "turn-openai")
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	mustOpenAIComputerDaemonLease(t, workflow, "com.apple.Notes", "Notes")
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
			"keypress": {
				Content: "keypress committed without a declared postcondition",
				GUIOutcome: &agent.GUIActionOutcome{
					Result:                          agent.GUIActionResultCompletedUnverified,
					Phase:                           agent.GUIActionPhaseVerifying,
					FailureCode:                     "postcondition_not_declared",
					SameObservationContinuationSafe: true,
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
	executor, err := newDaemonOpenAIComputerExecutorV1(
		workflow,
		&openAIComputerDaemonRuntimeProbe{tool: probe},
		trustedOpenAIComputerProvenanceForDaemon(t),
	)
	if err != nil {
		t.Fatalf("newDaemonOpenAIComputerExecutorV1: %v", err)
	}
	return executor, tools.NewOpenAIComputerAdapterV1(executor), probe, coordinator
}

func mustOpenAIComputerDaemonLease(
	t *testing.T,
	workflow *daemonGUIWorkflow,
	bundleID string,
	appName string,
) {
	t.Helper()
	if _, err := workflow.ensureLease(
		context.Background(),
		agent.GUIActionDescriptor{
			Participates:   true,
			ActionKind:     "desktop_task",
			Effect:         agent.GUIActionMutation,
			TargetBundleID: bundleID,
			TargetAppName:  appName,
			ExecutionPath:  "openai_native",
		},
	); err != nil {
		t.Fatalf("establish OpenAI computer task lease: %v", err)
	}
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
	executor, adapter, probe, coordinator := newOpenAIComputerDaemonExecutorFixture(t)
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
		"click,type,final_screenshot" {
		t.Fatalf("execution order = %v", got)
	}
	if got := probe.preflightNames(); strings.Join(got, ",") !=
		"click,type,final_screenshot" {
		t.Fatalf("risk preflight order = %v", got)
	}
	if executor.finalCaptures != 1 {
		t.Fatalf("final captures = %d, want 1", executor.finalCaptures)
	}
	if active := coordinator.Snapshot().Active; active == nil ||
		active.LeaseID != executor.authority.LeaseID {
		t.Fatalf("batch lease was not retained: %+v", active)
	}
}

func TestDaemonOpenAIComputerExecutorRetriesFinalObservationWithoutReplayingActions(
	t *testing.T,
) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	probe.resultQueues = map[string][]agent.ToolResult{
		"final_screenshot": {
			{
				Content: "state_id: must-not-escape\nref=e17\nscreenshot_warning: " +
					"[transient error] computer_use_error: capture_timeout\n" +
					"message: the exact window capture timed out",
				IsRetryable: true,
			},
			{
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "fresh-final-image",
				}},
			},
		},
	}

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20}`,
		),
	)
	if err != nil || result.ToolResult.IsError ||
		len(result.ToolResult.Images) != 1 {
		t.Fatalf("transient final observation result=%+v err=%v", result, err)
	}
	if result.ActionEffect != agent.ComputerUseCommitKnown {
		t.Fatalf("committed click effect = %q", result.ActionEffect)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"click,final_screenshot,final_screenshot" {
		t.Fatalf("observation retry replayed or reordered actions: %q", got)
	}
	if executor.finalCaptures != 1 {
		t.Fatalf("logical final captures = %d, want 1", executor.finalCaptures)
	}
}

func TestDaemonOpenAIComputerExecutorReportsCommittedBatchAsUnverifiedWhenFinalObservationExhausts(
	t *testing.T,
) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	probe.results["final_screenshot"] = agent.ToolResult{
		Content: "state_id: must-not-escape\nref=e17\nscreenshot_warning: " +
			"[transient error] computer_use_error: capture_timeout\n" +
			"message: the exact window capture timed out",
		IsRetryable: true,
	}

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "final_observation_unavailable") ||
		!strings.Contains(result.ToolResult.Content, "capture_timeout") {
		t.Fatalf("exhausted final observation result=%+v", result.ToolResult)
	}
	if result.ActionEffect != agent.ComputerUseCommitKnown {
		t.Fatalf("unverified batch effect = %q", result.ActionEffect)
	}
	if strings.Contains(result.ToolResult.Content, "state_id") ||
		strings.Contains(result.ToolResult.Content, "ref=e17") {
		t.Fatalf("final observation leaked stale authority: %q", result.ToolResult.Content)
	}
	if result.ToolResult.GUIOutcome == nil ||
		result.ToolResult.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.ToolResult.GUIOutcome.Phase != agent.GUIActionPhaseVerifying ||
		result.ToolResult.GUIOutcome.FailureCode != "final_observation_unavailable" {
		t.Fatalf("missing completed-unverified batch outcome: %+v",
			result.ToolResult.GUIOutcome)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"click,final_screenshot,final_screenshot,final_screenshot,"+
			"final_screenshot,final_screenshot" {
		t.Fatalf("final observation exhaustion replayed an action: %q", got)
	}
}

func TestDaemonOpenAIComputerExecutorRefreshesAndAuthorizesTypeAfterKeypress(
	t *testing.T,
) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	runtime := executor.runtime.(*openAIComputerDaemonRuntimeProbe)

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"keypress","keys":["META","L"]},`+
				`{"type":"type","text":"example.com"}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if result.ToolResult.IsError || len(result.ToolResult.Images) != 1 {
		t.Fatalf("result = %+v", result.ToolResult)
	}
	if got := probe.runNames(); strings.Join(got, ",") !=
		"keypress,reobserve,type,final_screenshot" {
		t.Fatalf("execution order = %v", got)
	}
	if runtime.keyboardTargets != 1 {
		t.Fatalf("keyboard target authorizations = %d, want 1",
			runtime.keyboardTargets)
	}
}

func TestDaemonOpenAIComputerExecutorPreservesTargetRefreshProjectionFailure(
	t *testing.T,
) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	runtime := executor.runtime.(*openAIComputerDaemonRuntimeProbe)
	runtime.targetRefreshPlanErr = &tools.OpenAIComputerActionPlanErrorV1{
		FailureCode: "coordinate_focus_window_frame_drift",
		Detail:      "the refreshed window frame no longer matches",
	}

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"keypress","keys":["META","L"]},`+
				`{"type":"type","text":"must-not-appear"}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		result.ActionEffect != agent.ComputerUseCommitKnown ||
		result.ToolResult.GUIOutcome == nil ||
		result.ToolResult.GUIOutcome.Result !=
			agent.GUIActionResultCompletedUnverified ||
		result.ToolResult.GUIOutcome.FailureCode !=
			"coordinate_focus_window_frame_drift" ||
		!strings.Contains(
			result.ToolResult.Content,
			"the refreshed window frame no longer matches",
		) ||
		strings.Contains(result.ToolResult.Content, "must-not-appear") {
		t.Fatalf("target refresh projection failure = %+v", result)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"keypress,final_screenshot" {
		t.Fatalf("target refresh projection execution = %q", got)
	}
}

func TestDaemonOpenAIComputerExecutorPreservesTargetRefreshToolFailure(
	t *testing.T,
) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	refreshFailure := agent.BusinessError(
		"state_id: must-not-escape\nref=e17\nscreenshot_warning: " +
			"computer_use_error: coordinate_focus_window_frame_drift\n" +
			"message: the target window frame changed",
	)
	refreshFailure.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultFailed,
		Phase:       agent.GUIActionPhaseObserving,
		FailureCode: "coordinate_focus_window_frame_drift",
	}
	probe.results["reobserve"] = refreshFailure

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"keypress","keys":["META","L"]},`+
				`{"type":"type","text":"must-not-appear"}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		result.ActionEffect != agent.ComputerUseCommitKnown ||
		result.ToolResult.GUIOutcome == nil ||
		result.ToolResult.GUIOutcome.FailureCode !=
			"coordinate_focus_window_frame_drift" ||
		!strings.Contains(
			result.ToolResult.Content,
			"coordinate_focus_window_frame_drift: the target window frame changed",
		) ||
		strings.Contains(result.ToolResult.Content, "state_id") ||
		strings.Contains(result.ToolResult.Content, "ref=e17") ||
		strings.Contains(result.ToolResult.Content, "must-not-appear") {
		t.Fatalf("target refresh tool failure = %+v", result)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"keypress,reobserve,final_screenshot" {
		t.Fatalf("target refresh tool execution = %q", got)
	}
}

func TestDaemonOpenAIComputerExecutorPreservesKeyboardTargetBindFailure(
	t *testing.T,
) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	runtime := executor.runtime.(*openAIComputerDaemonRuntimeProbe)
	runtime.keyboardTargetErr = &tools.OpenAIComputerActionPlanErrorV1{
		FailureCode: "keyboard_post_keypress_target_identity_invalid",
		Detail:      "the refreshed target lacks a stable window identity",
	}

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"keypress","keys":["META","L"]},`+
				`{"type":"type","text":"must-not-appear"}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		result.ActionEffect != agent.ComputerUseCommitKnown ||
		result.ToolResult.GUIOutcome == nil ||
		result.ToolResult.GUIOutcome.FailureCode !=
			"keyboard_post_keypress_target_identity_invalid" ||
		!strings.Contains(
			result.ToolResult.Content,
			"the refreshed target lacks a stable window identity",
		) ||
		strings.Contains(result.ToolResult.Content, "must-not-appear") {
		t.Fatalf("keyboard target bind failure = %+v", result)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"keypress,reobserve,final_screenshot" {
		t.Fatalf("keyboard target bind execution = %q", got)
	}
}

func TestDaemonOpenAIComputerExecutorAwaitsOneConsequentialClickDecision(t *testing.T) {
	executor, adapter, probe, coordinator := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()

	now := time.Now().UTC()
	broker, err := NewConsequentialRiskBroker(ConsequentialRiskBrokerOptions{
		Now:        func() time.Time { return now },
		Random:     bytes.NewReader(sequentialConsequentialRiskRandom(1)),
		PendingTTL: 5 * time.Second,
		GrantTTL:   5 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	executor.workflow.riskBroker = broker
	probe.targetBundleID = "com.tinyspeck.slackmacgap"
	probe.targetAppName = "Slack"
	probe.targetExecutionPath = "synthetic_coordinate"
	draft := consequentialRiskCoordinateBrokerDraft(
		t,
		openAIComputerActionToolUseIDV1("call_daemon_001/action/1"),
		now.Add(5*time.Second),
	)
	probe.riskResults = map[string]tools.ConsequentialRiskPreflightResultV1{
		"click": {
			Status: tools.ConsequentialRiskPreflightRequiredV1,
			Draft:  &draft,
		},
	}

	type batchResult struct {
		result tools.OpenAIComputerBatchResultV1
		err    error
	}
	done := make(chan batchResult, 1)
	go func() {
		result, executeErr := adapter.ExecuteBatchV1(
			context.Background(),
			openAIComputerDaemonCall(
				`{"type":"click","button":"left","x":320,"y":240}`,
			),
		)
		done <- batchResult{result: result, err: executeErr}
	}()

	var intentID string
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case execution := <-done:
			t.Fatalf("native consequential click returned before confirmation: %+v", execution)
		default:
		}
		active := coordinator.Snapshot().Active
		if active != nil && active.ConsequentialRisk != nil {
			intentID = active.ConsequentialRisk.IntentID
			if active.ActionPhase != guicontrol.ComputerUsePhaseWaitingForUser ||
				active.ConsequentialRisk.Kind != "send" ||
				active.TargetBundleID != draft.Target.BundleID ||
				active.TargetAppName != draft.Target.AppName {
				t.Fatalf("pending activity=%+v draftTarget=%+v", active, draft.Target)
			}
			break
		}
		time.Sleep(time.Millisecond)
	}
	if intentID == "" {
		t.Fatal("native consequential click did not stage one local confirmation")
	}
	if got := probe.runNames(); len(got) != 0 {
		t.Fatalf("native click ran before confirmation: %v", got)
	}
	if _, err := broker.Decide(ConsequentialRiskDecisionRequestV1{
		SchemaVersion: 1,
		IntentID:      intentID,
		Decision:      ConsequentialRiskDecisionAllow,
	}); err != nil {
		t.Fatal(err)
	}

	select {
	case execution := <-done:
		if execution.err != nil || execution.result.ToolResult.IsError ||
			len(execution.result.ToolResult.Images) != 1 {
			t.Fatalf("execution = %+v", execution)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("allow did not unblock native consequential click")
	}
	if got := strings.Join(probe.runNames(), ","); got != "click,final_screenshot" {
		t.Fatalf("native execution order = %q", got)
	}
}

func TestDaemonOpenAIComputerExecutorKeepsPreflightRejectionNotCommitted(t *testing.T) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	probe.results["keypress"] = consequentialRiskDaemonFailure(
		tools.ConsequentialRiskCodeUnsupportedPathV1,
	)

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"keypress","keys":["ENTER"]}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		!strings.Contains(result.ToolResult.Content, "(keypress)") ||
		!strings.Contains(
			result.ToolResult.Content,
			tools.ConsequentialRiskCodeUnsupportedPathV1,
		) {
		t.Fatalf("result = %+v", result.ToolResult)
	}
	if strings.Contains(result.ToolResult.Content, "commit status is unknown") {
		t.Fatalf("known preflight rejection degraded to unknown commit: %q", result.ToolResult.Content)
	}
	if len(result.ToolResult.Images) != 1 {
		t.Fatalf("preflight rejection lacked recovery screenshot: %+v", result.ToolResult)
	}
}

func TestDaemonOpenAIComputerExecutorContinuesKnownAtomicCommitWithoutInternalReobserve(t *testing.T) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	probe.results["click"] = agent.ToolResult{
		Content: "click committed without a declared postcondition",
		GUIOutcome: &agent.GUIActionOutcome{
			Result:                          agent.GUIActionResultCompletedUnverified,
			Phase:                           agent.GUIActionPhaseVerifying,
			FailureCode:                     "click_postcondition_not_declared",
			SameObservationContinuationSafe: true,
		},
	}

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
	if got := strings.Join(probe.runNames(), ","); got !=
		"click,type,final_screenshot" {
		t.Fatalf("execution order = %q", got)
	}
}

func TestDaemonOpenAIComputerExecutorRefreshesTargetBetweenOrderedKeypresses(
	t *testing.T,
) {
	executor, adapter, probe, coordinator := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	keypresses := 0
	probe.afterRun = func(action string) {
		if action != "keypress" {
			return
		}
		keypresses++
		if keypresses == 1 {
			probe.targetBundleID = "com.apple.calculator"
			probe.targetAppName = "Calculator"
		}
	}

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"keypress","keys":["META","TAB"]},`+
				`{"type":"keypress","keys":["9"]}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if result.ToolResult.IsError || len(result.ToolResult.Images) != 1 {
		t.Fatalf("result = %+v", result.ToolResult)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"keypress,reobserve,keypress,final_screenshot" {
		t.Fatalf("execution order = %q", got)
	}
	active := coordinator.Snapshot().Active
	if active == nil ||
		active.TargetBundleID != "com.apple.calculator" {
		t.Fatalf("batch retained the first app target: %+v", active)
	}
}

func TestDaemonOpenAIComputerExecutorRunsProviderWaitWithoutGUIPolicyProjection(
	t *testing.T,
) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	settleCalls := 0
	executor.postBatchSettle = func(context.Context, int) error {
		settleCalls++
		return nil
	}

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(`{"type":"wait"}`),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if result.ToolResult.IsError || len(result.ToolResult.Images) != 1 {
		t.Fatalf("result = %+v", result.ToolResult)
	}
	if got := strings.Join(probe.runNames(), ","); got != "wait,final_screenshot" {
		t.Fatalf("execution order = %q", got)
	}
	if settleCalls != 0 {
		t.Fatalf("observation-only batch settled %d times", settleCalls)
	}
}

func TestDaemonOpenAIComputerExecutorSettlesOnceWithoutHiddenObservation(
	t *testing.T,
) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	var events []string
	probe.afterRun = func(name string) {
		events = append(events, name)
	}
	executor.postBatchSettle = func(_ context.Context, _ int) error {
		events = append(events, "settle")
		return nil
	}

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if result.ToolResult.IsError || len(result.ToolResult.Images) != 1 {
		t.Fatalf("result = %+v", result.ToolResult)
	}
	if got := strings.Join(events, ","); got !=
		"click,settle,final_screenshot" {
		t.Fatalf("post-batch settle order = %q", got)
	}
}

func TestDaemonOpenAIComputerExecutorCancelledSettleDoesNotObserveOrReplay(
	t *testing.T,
) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	executor.postBatchSettle = func(context.Context, int) error {
		return context.Canceled
	}

	result, err := adapter.ExecuteBatchV1(
		context.Background(),
		openAIComputerDaemonCall(
			`{"type":"click","button":"left","x":10,"y":20}`,
		),
	)
	if err != nil {
		t.Fatalf("ExecuteBatchV1: %v", err)
	}
	if !result.ToolResult.IsError ||
		result.ActionEffect != agent.ComputerUseCommitKnown ||
		result.ToolResult.GUIOutcome == nil ||
		result.ToolResult.GUIOutcome.Result != agent.GUIActionResultCancelled {
		t.Fatalf("cancelled settle result = %+v", result)
	}
	if got := strings.Join(probe.runNames(), ","); got != "click" {
		t.Fatalf("cancelled settle executed more work = %q", got)
	}
}

func TestDaemonOpenAIComputerBatchRunnerBridgesAgentLoopToGuardedWorkflow(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(coordinator, "session-openai-runner", "turn-openai-runner")
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()
	mustOpenAIComputerDaemonLease(t, workflow, "com.apple.Notes", "Notes")

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.apple.Notes",
		targetAppName:  "Notes",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "click committed without a declared postcondition",
				GUIOutcome: &agent.GUIActionOutcome{
					Result:                          agent.GUIActionResultCompletedUnverified,
					Phase:                           agent.GUIActionPhaseVerifying,
					FailureCode:                     "click_postcondition_not_declared",
					SameObservationContinuationSafe: true,
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
				GUIObservation: &agent.GUIObservationOutcome{
					CoordinateActionable: true,
					SemanticActionable:   true,
				},
			},
		},
	}
	runner, err := newDaemonOpenAIComputerBatchRunnerV1(
		workflow,
		&openAIComputerDaemonRuntimeProbe{tool: probe},
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
		"click,type,final_screenshot" {
		t.Fatalf("execution order = %v", got)
	}
	if handler.approvals != 0 {
		t.Fatalf("ordinary per-action approvals = %d, want 0", handler.approvals)
	}
	if active := coordinator.Snapshot().Active; active == nil {
		t.Fatal("one batch lease was not retained through the runner")
	}
}

func TestOpenAIComputerTaskReplansAfterPreflightNotCommittedActionWithEarlierCommit(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-current-action-outcome",
		"turn-openai-current-action-outcome",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	notCommitted := agent.BusinessError(
		"the current target changed before text input",
	)
	notCommitted.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultFailed,
		Phase:       agent.GUIActionPhaseObserving,
		FailureCode: "frontmost_window_mismatch",
	}
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
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "ZnJlc2gtc3RhdGU=",
				}},
			},
		},
		resultQueues: map[string][]agent.ToolResult{
			"type": {
				notCommitted,
				{
					Content: "typed after replanning",
					GUIOutcome: &agent.GUIActionOutcome{
						Result: agent.GUIActionResultVerified,
						Phase:  agent.GUIActionPhaseVerifying,
					},
				},
			},
		},
	}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	firstCall := openAIComputerDaemonCall(
		`{"type":"click","button":"left","x":10,"y":20},` +
			`{"type":"type","text":"draft"}`,
	)
	secondResponseID := "shct_pOIBMOn2gmZdU7TJZm93xdhEM1SNRTRle-n9A0mz76h"
	secondCall := openAIComputerDaemonCallForResponse(
		secondResponseID,
		"call_daemon_current_action_replan",
		`{"type":"type","text":"draft"}`,
	)
	llm := &openAIComputerDaemonLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			openAIComputerDaemonContinuationToken,
			string(firstCall),
			"",
		),
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			secondResponseID,
			string(secondCall),
			"",
		),
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			"resp_daemon_current_action_done",
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"Draft completed after replanning.\"}"}`,
			`{"status":"completed","summary":"Draft completed after replanning."}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     &openAIComputerDaemonRuntimeProbe{tool: probe},
		modelTier:   "large",
		maxIter:     5,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Complete the draft in Notes","controlled_apps":["Notes"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if result.IsError ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskCompleted ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitKnown {
		t.Fatalf("task result = %+v", result)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,type,final_screenshot,type,final_screenshot" {
		t.Fatalf("desktop run order = %q", got)
	}
	if len(llm.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(llm.requests))
	}
	blocks := llm.requests[1].Messages[len(llm.requests[1].Messages)-1].Content.Blocks()
	if len(blocks) != 1 || blocks[0].Type != "tool_result" ||
		!blocks[0].IsError {
		t.Fatalf("first continuation result block = %#v", blocks)
	}
	nested, ok := blocks[0].ToolContent.([]client.ContentBlock)
	if !ok || len(nested) != 2 ||
		nested[0].Type != "image" ||
		nested[1].Type != "text" ||
		nested[1].Text !=
			`kocoro.computer_action_outcome.v1:{"schema_version":1,"effect":"committed","gui_outcome":{"result":"failed","phase":"observing","failure_code":"frontmost_window_mismatch"}}` {
		t.Fatalf("first continuation tool content = %#v", blocks[0].ToolContent)
	}
}

func TestOpenAIComputerTaskReobservesAfterPrecommitPhysicalInterference(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-physical-interference",
		"turn-openai-physical-interference",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	interference := agent.BusinessError(
		"the user moved the physical pointer before the click committed",
	)
	interference.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultUserInterference,
		Phase:       agent.GUIActionPhaseActing,
		FailureCode: "physical_input_interference",
	}
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "ZnJlc2gtc3RhdGU=",
				}},
			},
		},
		resultQueues: map[string][]agent.ToolResult{
			"click": {
				interference,
				{
					Content: "clicked after fresh observation",
					GUIOutcome: &agent.GUIActionOutcome{
						Result: agent.GUIActionResultVerified,
						Phase:  agent.GUIActionPhaseVerifying,
					},
				},
			},
		},
	}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	secondResponseID := "shct_pOIBMOn2gmZdU7TJZm93xdhEM1SNRTRle-n9A0mz76i"
	llm := &openAIComputerDaemonLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			openAIComputerDaemonContinuationToken,
			string(openAIComputerDaemonCall(
				`{"type":"click","button":"left","x":10,"y":20}`,
			)),
			"",
		),
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			secondResponseID,
			string(openAIComputerDaemonCallForResponse(
				secondResponseID,
				"call_daemon_interference_replan",
				`{"type":"click","button":"left","x":10,"y":20}`,
			)),
			"",
		),
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			"resp_daemon_interference_done",
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"Calculation completed after re-observing.\"}"}`,
			`{"status":"completed","summary":"Calculation completed after re-observing."}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     &openAIComputerDaemonRuntimeProbe{tool: probe},
		modelTier:   "large",
		maxIter:     5,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Click Calculator 7","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil || result.IsError ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskCompleted ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitKnown {
		t.Fatalf("task result=%+v err=%v", result, err)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,final_screenshot,click,final_screenshot" {
		t.Fatalf("physical interference run order = %q", got)
	}
	if len(llm.requests) != 3 {
		t.Fatalf("provider requests = %d, want 3", len(llm.requests))
	}
	blocks := llm.requests[1].Messages[len(llm.requests[1].Messages)-1].Content.Blocks()
	nested, ok := blocks[0].ToolContent.([]client.ContentBlock)
	if !ok || len(nested) != 2 ||
		nested[1].Text !=
			`kocoro.computer_action_outcome.v1:{"schema_version":1,"effect":"none","gui_outcome":{"result":"user_interference","phase":"acting","failure_code":"physical_input_interference"}}` {
		t.Fatalf("interference continuation content = %#v", blocks)
	}
}

func TestOpenAIComputerTaskDoesNotTreatObservedPhysicalInputAsUserTakeover(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-observed-physical-input",
		"turn-openai-observed-physical-input",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	interference := agent.BusinessError(
		"the physical pointer moved before the click committed",
	)
	interference.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultUserInterference,
		Phase:       agent.GUIActionPhaseActing,
		FailureCode: "physical_input_interference",
	}
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"click": interference,
			"final_screenshot": {
				Content: "fresh state observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "ZnJlc2gtc3RhdGU=",
				}},
			},
		},
	}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	providerFailure := errors.New("provider continuation failed")
	llm := &openAIComputerDaemonLoopLLM{
		responses: []*client.CompletionResponse{
			openAIComputerDaemonLoopResponse(
				t,
				profile,
				openAIComputerDaemonContinuationToken,
				string(openAIComputerDaemonCall(
					`{"type":"click","button":"left","x":10,"y":20}`,
				)),
				"",
			),
		},
		errors: []error{nil, providerFailure},
	}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     &openAIComputerDaemonRuntimeProbe{tool: probe},
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Click Calculator 7","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskUnverified ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitNone ||
		result.ComputerUseOutcome.FailureCode != "outcome_unverified" ||
		!strings.Contains(result.Content, "reason: outcome_unverified") ||
		strings.Contains(result.Content, "user_intervened") ||
		strings.Contains(result.Content, "user_interference") {
		t.Fatalf("observed physical input result=%+v err=%v", result, err)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,final_screenshot" {
		t.Fatalf("observed physical input replayed input: %q", got)
	}
}

func TestOpenAIComputerTaskLetsFreshObservationResolvePostcommitInterference(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-postcommit-interference",
		"turn-openai-postcommit-interference",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	interference := agent.BusinessError(
		"the user moved the physical pointer after the click was posted",
	)
	interference.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultUserInterference,
		Phase:       agent.GUIActionPhaseInputCommitted,
		FailureCode: "physical_input_interference",
	}
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"click": interference,
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "dmlzaWJsZS1yZXN1bHQ=",
				}},
			},
		},
	}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	llm := &openAIComputerDaemonLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			openAIComputerDaemonContinuationToken,
			string(openAIComputerDaemonCall(
				`{"type":"click","button":"left","x":10,"y":20}`,
			)),
			"",
		),
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			"resp_daemon_postcommit_interference_done",
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"The requested calculator result is visible after re-observing.\"}"}`,
			`{"status":"completed","summary":"The requested calculator result is visible after re-observing."}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     &openAIComputerDaemonRuntimeProbe{tool: probe},
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Click Calculator 7","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil || result.IsError ||
		result.Content !=
			"The requested calculator result is visible after re-observing." {
		t.Fatalf("task result=%+v err=%v", result, err)
	}
	if result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskCompleted ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitUnknown {
		t.Fatalf("postcommit observation outcome = %+v", result.ComputerUseOutcome)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,final_screenshot" {
		t.Fatalf("postcommit interference replayed input: %q", got)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("provider requests = %d, want action + observation decision",
			len(llm.requests))
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
				Content: "click committed without a declared postcondition",
				GUIOutcome: &agent.GUIActionOutcome{
					Result:                          agent.GUIActionResultCompletedUnverified,
					Phase:                           agent.GUIActionPhaseVerifying,
					FailureCode:                     "click_postcondition_not_declared",
					SameObservationContinuationSafe: true,
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
	completedSummary := "The screen shows Slack with hello in the requested composer, " +
		"then Calculator in front with 7 visibly entered, so the full cross-app task is complete."
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
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"The screen shows Slack with hello in the requested composer, then Calculator in front with 7 visibly entered, so the full cross-app task is complete.\"}"}`,
			`{"status":"completed","summary":"The screen shows Slack with hello in the requested composer, then Calculator in front with 7 visibly entered, so the full cross-app task is complete."}`,
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
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		handler:     handler,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Open Slack, type hello, then switch to Calculator and click 7",`+
			`"controlled_apps":["Slack","Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if result.IsError || result.Content != completedSummary {
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
	if got := strings.Join(runtime.initialObservationApps, ","); got != "Slack" {
		t.Fatalf("initial observation apps = %q", got)
	}
	if workflow.lease == nil ||
		strings.Join(workflow.lease.AllowedAppBundleIDs, ",") !=
			"com.example.slack" {
		t.Fatalf("task lease targets = %+v", workflow.lease)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,type,final_screenshot" {
		t.Fatalf("private execution order = %q", got)
	}
	if len(llm.requests) != 2 {
		t.Fatalf("child completion requests = %d, want one batch + one continuation", len(llm.requests))
	}
	childUsage := handler.usage.Snapshot().LLM
	if childUsage.LLMCalls != 2 || childUsage.InputTokens != 20 ||
		childUsage.OutputTokens != 10 || childUsage.TotalTokens != 30 {
		t.Fatalf("parent-forwarded child usage = %+v, want 2 calls and 20/10/30 tokens", childUsage)
	}
	if llm.requests[0].ToolChoice != "any" {
		t.Fatalf("initial child tool_choice = %#v, want any", llm.requests[0].ToolChoice)
	}
	if llm.requests[1].ToolChoice != nil {
		t.Fatalf("continuation child tool_choice = %#v, want auto", llm.requests[1].ToolChoice)
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
		hasOutcomeContract := false
		hasConsequentialPathInstructions := false
		hasFreshObservationInstructions := false
		hasStopBeforeExtraActionInstructions := false
		hasSafeNavigationRecoveryInstructions := false
		hasSelfContainedInspectionResultInstructions := false
		hasControlledAppSwitchInstructions := false
		for _, message := range request.Messages {
			if strings.Contains(
				message.Content.Text(),
				"execution_role=private_openai_native_computer",
			) {
				hasExecutorInstructions = true
			}
			if strings.Contains(
				message.Content.Text(),
				`{"status":"completed","summary":"self-contained result for the parent"}`,
			) {
				hasOutcomeContract = true
			}
			if strings.Contains(
				message.Content.Text(),
				"the summary must contain the requested facts, extracted text, or synthesis itself",
			) && strings.Contains(
				message.Content.Text(),
				"Never return only a completion claim such as content was viewed, recorded, or summarized",
			) {
				hasSelfContainedInspectionResultInstructions = true
			}
			if strings.Contains(
				message.Content.Text(),
				"click the exact visible action button and wait for Kocoro's one local confirmation",
			) {
				hasConsequentialPathInstructions = true
			}
			if strings.Contains(
				message.Content.Text(),
				"derive the next action only from the latest returned screenshot",
			) && strings.Contains(
				message.Content.Text(),
				"Do not add routine fixed waits",
			) && strings.Contains(
				message.Content.Text(),
				"Kocoro applies one short bounded settle",
			) && !strings.Contains(
				message.Content.Text(),
				"bounded adaptive settle",
			) && strings.Contains(
				message.Content.Text(),
				"make that first batch perform the first useful goal action",
			) && strings.Contains(
				message.Content.Text(),
				"Batch adjacent deterministic actions",
			) {
				hasFreshObservationInstructions = true
			}
			if strings.Contains(
				message.Content.Text(),
				"Before every new mutating action, compare the latest screenshot",
			) && strings.Contains(
				message.Content.Text(),
				"your next response must be the completed JSON object, not another computer call",
			) && strings.Contains(
				message.Content.Text(),
				"Never move or drag merely to park the cursor",
			) && strings.Contains(
				message.Content.Text(),
				"do not drag-select text unless the user explicitly requested selection or dragging",
			) {
				hasStopBeforeExtraActionInstructions = true
			}
			if strings.Contains(
				message.Content.Text(),
				"use Command-L, type the exact URL, then Return",
			) && strings.Contains(
				message.Content.Text(),
				"If keyboard focus is unavailable or Return is rejected, do not repeat it",
			) && strings.Contains(
				message.Content.Text(),
				"click the exact visible Go, URL suggestion, or navigation target instead",
			) {
				hasSafeNavigationRecoveryInstructions = true
			}
			if strings.Contains(
				message.Content.Text(),
				"switch between already prepared controlled apps with Command-Tab",
			) && strings.Contains(
				message.Content.Text(),
				"never use Spotlight search plus Return to launch a controlled app",
			) {
				hasControlledAppSwitchInstructions = true
			}
		}
		if !hasExecutorInstructions {
			t.Fatalf("child request %d lacks private executor instructions", index)
		}
		if !hasOutcomeContract {
			t.Fatalf("child request %d lacks terminal outcome contract", index)
		}
		if !hasSelfContainedInspectionResultInstructions {
			t.Fatalf("child request %d lacks self-contained inspection result instructions", index)
		}
		if !hasConsequentialPathInstructions {
			t.Fatalf("child request %d lacks consequential action path instructions", index)
		}
		if !hasFreshObservationInstructions {
			t.Fatalf("child request %d lacks fresh-observation instructions", index)
		}
		if !hasStopBeforeExtraActionInstructions {
			t.Fatalf("child request %d lacks stop-before-extra-action instructions", index)
		}
		if !hasSafeNavigationRecoveryInstructions {
			t.Fatalf("child request %d lacks safe browser navigation recovery instructions", index)
		}
		if !hasControlledAppSwitchInstructions {
			t.Fatalf("child request %d lacks controlled-app switch instructions", index)
		}
	}
	if blocks := llm.requests[0].Messages[len(llm.requests[0].Messages)-1].Content.Blocks(); len(blocks) != 2 || blocks[1].Type != "image" {
		t.Fatalf("child initial user content = %+v", blocks)
	}
}

func TestOpenAIComputerChildGoalInputKeepsOriginalCrossAppRequest(t *testing.T) {
	ctx := agent.ContextWithToolInvocation(
		context.Background(),
		agent.ToolInvocation{
			ToolName:    "computer_use",
			ToolUseID:   "toolu_cross_app_goal",
			UserRequest: "Open TextEdit, type hello, then open Calculator and enter 7+8",
		},
	)
	encoded := openAIComputerChildGoalInputV1(
		ctx,
		"Open TextEdit and type hello",
		[]string{"TextEdit", "Calculator"},
		openAIComputerForegroundAllowedV1,
	)
	var goal openAIComputerChildGoalV1
	if err := json.Unmarshal([]byte(encoded), &goal); err != nil {
		t.Fatalf("decode child goal: %v", err)
	}
	if goal.OriginalUserRequest !=
		"Open TextEdit, type hello, then open Calculator and enter 7+8" {
		t.Fatalf("original user request = %q", goal.OriginalUserRequest)
	}
	if goal.ParentDesktopPlan != "Open TextEdit and type hello" {
		t.Fatalf("parent desktop plan = %q", goal.ParentDesktopPlan)
	}
	if got := strings.Join(goal.ControlledApps, ","); got !=
		"TextEdit,Calculator" {
		t.Fatalf("controlled apps = %q", got)
	}
	if goal.ForegroundPolicy != openAIComputerForegroundAllowedV1 {
		t.Fatalf("foreground policy = %q", goal.ForegroundPolicy)
	}
}

func TestOpenAIComputerChildGoalInputFallsBackForDirectInvocation(t *testing.T) {
	const plan = "Open Calculator and enter 7+8"
	if got := openAIComputerChildGoalInputV1(
		context.Background(),
		plan,
		[]string{"Calculator"},
		openAIComputerForegroundAllowedV1,
	); got != plan {
		t.Fatalf("direct child goal = %q, want %q", got, plan)
	}
}

func TestOpenAIComputerChildGoalInputCarriesRequiredBackgroundContract(
	t *testing.T,
) {
	const plan = "Press Calculator 7 without activating Calculator"
	encoded := openAIComputerChildGoalInputV1(
		context.Background(),
		plan,
		[]string{"Calculator"},
		openAIComputerPreserveFrontmostV1,
	)
	var goal openAIComputerChildGoalV1
	if err := json.Unmarshal([]byte(encoded), &goal); err != nil {
		t.Fatalf("decode background child goal: %v", err)
	}
	if goal.OriginalUserRequest != plan ||
		goal.ParentDesktopPlan != plan ||
		strings.Join(goal.ControlledApps, ",") != "Calculator" ||
		goal.ForegroundPolicy != openAIComputerPreserveFrontmostV1 {
		t.Fatalf("background child goal = %+v", goal)
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
		runtime: runtime,
	}

	for attempt := 0; attempt < 2; attempt++ {
		result, err := taskTool.Run(
			context.Background(),
			`{"task":"Open Slack and type hello","controlled_apps":["Slack"],`+
				`"foreground_policy":"foreground_allowed",`+
				`"description":"Complete the desktop task"}`,
		)
		if err != nil {
			t.Fatalf("task Run %d: %v", attempt, err)
		}
		if !result.IsError ||
			!strings.Contains(result.Content, "another appropriate non-computer_use control path") ||
			!strings.Contains(result.Content, "no desktop action was attempted") ||
			result.ComputerUseOutcome == nil ||
			result.ComputerUseOutcome.Recovery !=
				agent.ComputerUseRecoveryAlternateControl {
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

func TestOpenAIComputerTaskToolInitialObservationFailureDoesNotLeakStaleState(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-initial-failure",
		"turn-openai-initial-failure",
	)
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"final_screenshot": {
				Content: "state_id: stale-secret\nref=e17\nscreenshot_warning: " +
					"[transient error] computer_use_error: window_changed\n" +
					"message: Calculator's window changed during capture\n" +
					"recovery: stop moving or resizing the window, then retry once",
				IsError:     true,
				IsRetryable: true,
			},
		},
	}
	taskTool := &openAIComputerTaskToolV1{
		gateway:    &openAIComputerDaemonLoopLLM{},
		profile:    trustedOpenAIComputerProfileForDaemon(t),
		childTools: agent.NewToolRegistry(),
		workflow:   workflow,
		runtime:    &openAIComputerDaemonRuntimeProbe{tool: probe},
		observationRetry: func(context.Context, int) error {
			return nil
		},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Inspect Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed","description":"Inspect the app"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "initial_observation_unavailable") ||
		!strings.Contains(result.Content, "another appropriate non-computer_use control path") ||
		!strings.Contains(result.Content, "app launch or focus may already have occurred") ||
		strings.Contains(result.Content, "no desktop action was attempted") ||
		!strings.Contains(result.Content, "window_changed") ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Recovery !=
			agent.ComputerUseRecoveryAlternateControl {
		t.Fatalf("task result = %+v", result)
	}
	if strings.Contains(result.Content, "state_id") ||
		strings.Contains(result.Content, "ref=e17") {
		t.Fatalf("initial failure leaked stale observation state: %q", result.Content)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,final_screenshot,final_screenshot,final_screenshot,final_screenshot" {
		t.Fatalf("initial observation attempts = %q", got)
	}
}

func TestOpenAIComputerTaskToolPersistentDisplayTopologyReconfigurationStopsAfterBoundedRetries(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-display-reconfiguration",
		"turn-openai-display-reconfiguration",
	)
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.apple.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"final_screenshot": {
				Content:     "[transient error] read display topology",
				IsError:     true,
				IsRetryable: true,
				GUIOutcome: &agent.GUIActionOutcome{
					Result:      agent.GUIActionResultFailed,
					Phase:       agent.GUIActionPhaseObserving,
					FailureCode: tools.ComputerUseFailureDisplayTopologyReconfiguringV1,
				},
			},
		},
	}
	taskTool := &openAIComputerTaskToolV1{
		gateway:    &openAIComputerDaemonLoopLLM{},
		profile:    trustedOpenAIComputerProfileForDaemon(t),
		childTools: agent.NewToolRegistry(),
		workflow:   workflow,
		runtime:    &openAIComputerDaemonRuntimeProbe{tool: probe},
		observationRetry: func(context.Context, int) error {
			return nil
		},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Inspect Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed","description":"Inspect the app"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(
			result.Content,
			tools.ComputerUseFailureDisplayTopologyReconfiguringV1,
		) ||
		!strings.Contains(result.Content, "did not stabilize after bounded observation retries") ||
		!strings.Contains(result.Content, "do not retry computer_use automatically") ||
		strings.Contains(result.Content, "retry computer_use in a new turn") ||
		!strings.Contains(result.Content, "do not switch to another desktop-control tool") ||
		strings.Contains(result.Content, "another appropriate non-computer_use control path") ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Recovery != agent.ComputerUseRecoveryNone {
		t.Fatalf("task result = %+v", result)
	}
	if got := len(probe.runNames()); got != maxOpenAIComputerInitialObservationsV1 {
		t.Fatalf("initial observation attempts = %d, want %d", got, maxOpenAIComputerInitialObservationsV1)
	}
}

func TestOpenAIComputerTaskToolAppResolutionFailureRequestsCorrectedTargets(
	t *testing.T,
) {
	runtime := &openAIComputerDaemonRuntimeProbe{
		tool:       &openAIComputerDaemonProbeTool{},
		resolveErr: errors.New("requested app is not installed"),
	}
	taskTool := &openAIComputerTaskToolV1{
		gateway:    &openAIComputerDaemonLoopLLM{},
		profile:    trustedOpenAIComputerProfileForDaemon(t),
		childTools: agent.NewToolRegistry(),
		workflow: testGUIWorkflow(
			guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{}),
			"session-openai-app-resolution-failure",
			"turn-openai-app-resolution-failure",
		),
		runtime: runtime,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Open Missing App","controlled_apps":["Missing App"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Open the app"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "app_resolution_failed") ||
		!strings.Contains(result.Content, "corrected canonical installed app names") ||
		!strings.Contains(
			result.Content,
			"do not switch to another desktop-control tool",
		) ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Recovery !=
			agent.ComputerUseRecoveryRetryWithApps {
		t.Fatalf("task result = %+v", result)
	}
	if len(runtime.resolvedApps) != 1 ||
		len(runtime.launchedApps) != 0 {
		t.Fatalf(
			"resolution failure touched launch: resolved=%v launched=%v",
			runtime.resolvedApps,
			runtime.launchedApps,
		)
	}
}

func TestOpenAIComputerTaskToolLaunchFailureReportsPossiblePreparation(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-launch-failure",
		"turn-openai-launch-failure",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	runtime := &openAIComputerDaemonRuntimeProbe{
		tool:      &openAIComputerDaemonProbeTool{},
		launchErr: errors.New("frontmost target did not stabilize"),
	}
	taskTool := &openAIComputerTaskToolV1{
		gateway:    &openAIComputerDaemonLoopLLM{},
		profile:    trustedOpenAIComputerProfileForDaemon(t),
		childTools: agent.NewToolRegistry(),
		workflow:   workflow,
		runtime:    runtime,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Open Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Open the app"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "app_launch_focus_failed") ||
		!strings.Contains(result.Content, "app launch or focus may already have occurred") ||
		strings.Contains(result.Content, "no desktop action was attempted") ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Recovery !=
			agent.ComputerUseRecoveryAlternateControl {
		t.Fatalf("task result = %+v", result)
	}
	if len(runtime.launchedApps) != 1 ||
		runtime.launchedApps[0].App != "Calculator" {
		t.Fatalf("launch attempts = %+v", runtime.launchedApps)
	}
}

func TestOpenAIComputerTaskToolProtectedFrontmostRequestsAppHints(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-protected-frontmost",
		"turn-openai-protected-frontmost",
	)
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "run.shannon.shanclaw.dev",
		targetAppName:  "Kocoro Desktop",
		results: map[string]agent.ToolResult{
			"final_screenshot": agent.BusinessError(
				"computer_use_error: app_policy_blocked\n" +
					"target_app: Kocoro Desktop\n" +
					"policy_source: built_in\n" +
					"message: Kocoro Desktop is a protected app and cannot be controlled by Computer Use",
			),
		},
	}
	taskTool := &openAIComputerTaskToolV1{
		gateway:    &openAIComputerDaemonLoopLLM{},
		profile:    trustedOpenAIComputerProfileForDaemon(t),
		childTools: agent.NewToolRegistry(),
		workflow:   workflow,
		runtime:    &openAIComputerDaemonRuntimeProbe{tool: probe},
		observationRetry: func(context.Context, int) error {
			return nil
		},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Open the browser","foreground_policy":"foreground_allowed",`+
			`"description":"Open the browser"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "initial_target_required") ||
		!strings.Contains(
			result.Content,
			"controlled app names in controlled_apps",
		) {
		t.Fatalf("task result = %+v", result)
	}
	if result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskNotCompleted ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitNone ||
		result.ComputerUseOutcome.FailureCode != "initial_target_required" ||
		result.ComputerUseOutcome.Recovery !=
			agent.ComputerUseRecoveryRetryWithApps {
		t.Fatalf("task outcome = %+v", result.ComputerUseOutcome)
	}
	if got := strings.Join(probe.runNames(), ","); got != "final_screenshot" {
		t.Fatalf("protected initial observation retried: %q", got)
	}
}

func TestOpenAIComputerTaskToolWaitsForColdAppInitialWindow(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-cold-app",
		"turn-openai-cold-app",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	initialFailure := agent.ToolResult{
		Content: "state_id: must-not-escape\nscreenshot_warning: " +
			"[business error] computer_use_error: window_not_found\n" +
			"message: Calculator has no unique capturable window\n" +
			"recovery: open one normal app window, bring it forward, and retry",
		IsRetryable: true,
	}
	initialSuccess := agent.ToolResult{
		Content: "observed",
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "Y29sZC1hcHAtcmVhZHk=",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			CoordinateActionable: true,
			SemanticActionable:   true,
		},
	}
	initialVisualOnly := initialSuccess
	initialVisualOnly.GUIObservation = &agent.GUIObservationOutcome{
		CoordinateActionable:     false,
		ActionabilityFailureCode: "image_dimensions_mismatch",
	}
	initialVisualOnly.IsRetryable = true
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "clicked",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"final_screenshot": initialFailure,
		},
	}
	initialRuns := 0
	probe.afterRun = func(name string) {
		if name != "final_screenshot" {
			return
		}
		initialRuns++
		if initialRuns == 1 {
			probe.mu.Lock()
			probe.results["final_screenshot"] = initialVisualOnly
			probe.mu.Unlock()
		} else if initialRuns == 2 {
			probe.mu.Lock()
			probe.results["final_screenshot"] = initialSuccess
			probe.mu.Unlock()
		}
	}

	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	call := openAIComputerDaemonCall(
		`{"type":"click","button":"left","x":10,"y":20}`,
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
			"resp_daemon_cold_app_final",
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"Calculator visibly shows the requested result.\"}"}`,
			`{"status":"completed","summary":"Calculator visibly shows the requested result."}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	var waits []int
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
		observationRetry: func(_ context.Context, attempt int) error {
			waits = append(waits, attempt)
			return nil
		},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Compute 3-10 in Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if result.IsError ||
		result.Content != "Calculator visibly shows the requested result." {
		t.Fatalf("task result = %+v", result)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,final_screenshot,final_screenshot,click,final_screenshot" {
		t.Fatalf("desktop runs = %q", got)
	}
	if !reflect.DeepEqual(waits, []int{1, 2}) {
		t.Fatalf("initial observation waits = %v", waits)
	}
}

func TestOpenAIComputerTaskToolRecoversInitialDisplayBeforeProviderAction(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-display-recovery",
		"turn-openai-display-recovery",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	displayFailure := agent.ToolResult{
		Content:     "observed visual-only display geometry",
		IsRetryable: true,
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "ZGlzcGxheS1mYWlsdXJl",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			SemanticActionable:       true,
			ActionabilityFailureCode: "display_not_actionable",
		},
	}
	actionable := agent.ToolResult{
		Content: "observed actionable window",
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "YWN0aW9uYWJsZQ==",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			CoordinateActionable: true,
			SemanticActionable:   true,
		},
	}
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "clicked",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"final_screenshot": displayFailure,
		},
	}
	initialRuns := 0
	probe.afterRun = func(name string) {
		if name != "final_screenshot" {
			return
		}
		initialRuns++
		// A display unplug can leave AppKit and WindowServer disagreeing for
		// more than one poll while macOS migrates the task window.
		if initialRuns == 3 {
			probe.mu.Lock()
			probe.results["final_screenshot"] = actionable
			probe.mu.Unlock()
		}
	}

	profile := trustedOpenAIComputerProfileForDaemon(t)
	call := openAIComputerDaemonCall(
		`{"type":"click","button":"left","x":10,"y":20}`,
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
			"resp_daemon_display_recovery_final",
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"Calculator visibly shows the requested result.\"}"}`,
			`{"status":"completed","summary":"Calculator visibly shows the requested result."}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	var waits []int
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     &openAIComputerDaemonRuntimeProbe{tool: probe},
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
		observationRetry: func(_ context.Context, attempt int) error {
			waits = append(waits, attempt)
			return nil
		},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Compute 3-10 in Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if result.IsError ||
		result.Content != "Calculator visibly shows the requested result." {
		t.Fatalf("task result = %+v", result)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,final_screenshot,final_screenshot,"+
			"final_screenshot,click,final_screenshot" {
		t.Fatalf("desktop runs = %q", got)
	}
	if !reflect.DeepEqual(waits, []int{1, 2, 3}) {
		t.Fatalf("display observation waits = %v", waits)
	}
}

func TestOpenAIComputerTaskToolStopsAfterBoundedInitialDisplayRecovery(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-display-terminal",
		"turn-openai-display-terminal",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	displayFailure := agent.ToolResult{
		Content:     "observed visual-only display geometry",
		IsRetryable: true,
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "ZGlzcGxheS1mYWlsdXJl",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			ActionabilityFailureCode: "display_not_actionable",
		},
	}
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"final_screenshot": displayFailure,
		},
	}
	taskTool := &openAIComputerTaskToolV1{
		gateway:     &openAIComputerDaemonLoopLLM{},
		profile:     trustedOpenAIComputerProfileForDaemon(t),
		childTools:  agent.NewToolRegistry(),
		workflow:    workflow,
		runtime:     &openAIComputerDaemonRuntimeProbe{tool: probe},
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
		observationRetry: func(context.Context, int) error {
			return nil
		},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Compute 3-10 in Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "computer_use_error: display_not_actionable") ||
		!strings.Contains(
			result.Content,
			"fully contained in one active, online, awake, unmirrored, unrotated display",
		) {
		t.Fatalf("terminal display result = %+v", result)
	}
	if result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskNotCompleted ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitNone ||
		result.ComputerUseOutcome.FailureCode != "display_not_actionable" ||
		result.ComputerUseOutcome.Recovery != agent.ComputerUseRecoveryNone {
		t.Fatalf("terminal display outcome = %+v", result.ComputerUseOutcome)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,final_screenshot,final_screenshot,"+
			"final_screenshot,final_screenshot" {
		t.Fatalf("terminal display runs = %q", got)
	}
}

func TestOpenAIComputerObservationAcceptsSemanticAuthorityOnlyForBackgroundLane(
	t *testing.T,
) {
	image := agent.ToolResult{
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "c2VtYW50aWMtaW1hZ2U=",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			SemanticActionable:       true,
			ActionabilityFailureCode: "display_not_actionable",
		},
	}
	for _, test := range []struct {
		name          string
		allowSemantic bool
		wantAttempts  int
	}{
		{name: "foreground", allowSemantic: false, wantAttempts: 1},
		{name: "background semantic", allowSemantic: true, wantAttempts: 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			attempts := 0
			_, err := runOpenAIComputerInitialObservationV1(
				context.Background(),
				2,
				true,
				test.allowSemantic,
				func(context.Context, int) error { return nil },
				func(context.Context, int) (agent.ToolResult, error) {
					attempts++
					return image, nil
				},
				nil,
			)
			if err != nil || attempts != test.wantAttempts {
				t.Fatalf("semantic observation attempts=%d err=%v, want %d",
					attempts, err, test.wantAttempts)
			}
		})
	}
}

func TestOpenAIComputerInitialDisplayDoesNotOverrideNonRetryableHelperResult(
	t *testing.T,
) {
	displayFailure := agent.ToolResult{
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "ZGlzcGxheS1mYWlsdXJl",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			ActionabilityFailureCode: "display_not_actionable",
		},
	}
	attempts := 0
	result, err := runOpenAIComputerInitialObservationV1(
		context.Background(),
		maxOpenAIComputerInitialObservationsV1,
		true,
		false,
		func(context.Context, int) error { return nil },
		func(context.Context, int) (agent.ToolResult, error) {
			attempts++
			return displayFailure, nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("initial observation: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("display observation attempts = %d, want 1", attempts)
	}
	if got := openAIComputerTraceFailureCodeV1(result, nil); got !=
		"display_not_actionable" {
		t.Fatalf("terminal failure code = %q", got)
	}
	if got := openAIComputerObservationResultDetailV1(result); !strings.Contains(
		got,
		"display_not_actionable",
	) {
		t.Fatalf("terminal failure detail = %q", got)
	}
}

func TestOpenAIComputerInitialDisplayRecoveryContinuesAfterActionableSecondObservation(
	t *testing.T,
) {
	displayFailure := agent.ToolResult{
		IsRetryable: true,
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "ZGlzcGxheS1mYWlsdXJl",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			ActionabilityFailureCode: "display_not_actionable",
		},
	}
	actionable := agent.ToolResult{
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "YWN0aW9uYWJsZQ==",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			CoordinateActionable: true,
			SemanticActionable:   true,
		},
	}
	attempts := 0
	result, err := runOpenAIComputerInitialObservationV1(
		context.Background(),
		maxOpenAIComputerInitialObservationsV1,
		true,
		false,
		func(context.Context, int) error { return nil },
		func(context.Context, int) (agent.ToolResult, error) {
			attempts++
			if attempts == 1 {
				return displayFailure, nil
			}
			return actionable, nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("initial observation: %v", err)
	}
	if attempts != 2 || !result.GUIObservation.CoordinateActionable {
		t.Fatalf("actionable recovery result=%+v attempts=%d", result, attempts)
	}
}

func TestOpenAIComputerInitialDisplayRecoveryAllowsHotPlugWindowSettle(
	t *testing.T,
) {
	displayFailure := agent.ToolResult{
		IsRetryable: true,
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "ZGlzcGxheS1mYWlsdXJl",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			ActionabilityFailureCode: "display_not_actionable",
		},
	}
	actionable := agent.ToolResult{
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "YWN0aW9uYWJsZQ==",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			CoordinateActionable: true,
			SemanticActionable:   true,
		},
	}
	attempts := 0
	var waits []int
	result, err := runOpenAIComputerInitialObservationV1(
		context.Background(),
		maxOpenAIComputerInitialObservationsV1,
		true,
		false,
		func(_ context.Context, attempt int) error {
			waits = append(waits, attempt)
			return nil
		},
		func(context.Context, int) (agent.ToolResult, error) {
			attempts++
			if attempts < 4 {
				return displayFailure, nil
			}
			return actionable, nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("initial hot-plug observation: %v", err)
	}
	if attempts != 4 || !result.GUIObservation.CoordinateActionable {
		t.Fatalf("hot-plug recovery result=%+v attempts=%d", result, attempts)
	}
	if !reflect.DeepEqual(waits, []int{1, 2, 3}) {
		t.Fatalf("hot-plug observation waits = %v", waits)
	}
}

func TestOpenAIComputerInitialActionableObservationDoesNotRetry(
	t *testing.T,
) {
	actionable := agent.ToolResult{
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "YWN0aW9uYWJsZQ==",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			CoordinateActionable: true,
			SemanticActionable:   true,
		},
	}
	attempts := 0
	result, err := runOpenAIComputerInitialObservationV1(
		context.Background(),
		maxOpenAIComputerInitialObservationsV1,
		true,
		false,
		func(context.Context, int) error {
			t.Fatal("an actionable initial observation was delayed for retry")
			return nil
		},
		func(context.Context, int) (agent.ToolResult, error) {
			attempts++
			return actionable, nil
		},
		nil,
	)
	if err != nil || attempts != 1 ||
		result.GUIObservation == nil ||
		!result.GUIObservation.CoordinateActionable {
		t.Fatalf(
			"actionable initial observation result=%+v attempts=%d err=%v",
			result,
			attempts,
			err,
		)
	}
}

func TestOpenAIComputerInitialIdentityMismatchDoesNotRetry(
	t *testing.T,
) {
	identityFailure := agent.BusinessError(
		"computer_use_error: process_identity_mismatch",
	)
	identityFailure.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultFailed,
		Phase:       agent.GUIActionPhaseObserving,
		FailureCode: "process_identity_mismatch",
	}
	attempts := 0
	_, err := runOpenAIComputerInitialObservationV1(
		context.Background(),
		maxOpenAIComputerInitialObservationsV1,
		true,
		false,
		func(context.Context, int) error { return nil },
		func(context.Context, int) (agent.ToolResult, error) {
			attempts++
			return identityFailure, nil
		},
		nil,
	)
	if err != nil {
		t.Fatalf("initial observation: %v", err)
	}
	if attempts != 1 {
		t.Fatalf("process identity observation attempts = %d, want 1", attempts)
	}
}

func TestOpenAIComputerObservationRequiresExplicitActionabilityProof(
	t *testing.T,
) {
	imageWithoutProof := agent.ToolResult{
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "bm8tYWN0aW9uYWJpbGl0eS1wcm9vZg==",
		}},
	}
	if openAIComputerObservationMeetsActionRequirementV1(
		imageWithoutProof,
		true,
		false,
	) {
		t.Fatal("coordinate action observation accepted without local actionability proof")
	}
	if !openAIComputerObservationMeetsActionRequirementV1(
		imageWithoutProof,
		false,
		false,
	) {
		t.Fatal("visual-only final observation unexpectedly required actionability proof")
	}
}

func TestOpenAIComputerObservationRetryPolicySeparatesIdentityDrift(
	t *testing.T,
) {
	identityDrift := agent.BusinessError("window identity changed")
	identityDrift.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultFailed,
		Phase:       agent.GUIActionPhaseObserving,
		FailureCode: "window_identity_mismatch",
	}
	if retryOpenAIComputerObservationV1(identityDrift, nil) {
		t.Fatal("daemon must not override helper retry_safe=false by failure code")
	}
	transientCapture := identityDrift
	transientCapture.IsRetryable = true
	if !retryOpenAIComputerObservationV1(transientCapture, nil) {
		t.Fatal("helper retry_safe=true must remain observation-retryable")
	}
}

func TestOpenAIComputerTaskBackgroundLaneStartsFromSemanticOnlyImage(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-background-semantic-image",
		"turn-openai-background-semantic-image",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	semanticImage := agent.ToolResult{
		Content: "observed",
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "c2VtYW50aWMtaW1hZ2U=",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			SemanticActionable:       true,
			ActionabilityFailureCode: "display_not_actionable",
		},
	}
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "pressed",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"final_screenshot": semanticImage,
		},
	}
	baseRuntime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	runtime := &backgroundSemanticDaemonRuntimeProbe{
		openAIComputerDaemonRuntimeProbe: baseRuntime,
	}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	call := openAIComputerDaemonCall(
		`{"type":"click","button":"left","x":10,"y":20}`,
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
			"resp_daemon_background_semantic_final",
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"Calculator shows the requested result while remaining in the background.\"}"}`,
			`{"status":"completed","summary":"Calculator shows the requested result while remaining in the background."}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
		observationRetry: func(context.Context, int) error {
			t.Fatal("semantic-only background image was retried")
			return nil
		},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Press Calculator 7 in the background",`+
			`"controlled_apps":["Calculator"],`+
			`"foreground_policy":"preserve_frontmost",`+
			`"description":"Complete the background desktop task"}`,
	)
	if err != nil || result.IsError ||
		!strings.Contains(
			result.Content,
			"Calculator shows the requested result while remaining in the background.",
		) ||
		!strings.Contains(
			result.Content,
			"execution: background_semantic; foreground activation and fallback were disabled",
		) {
		t.Fatalf("background semantic task result=%+v err=%v", result, err)
	}
	if got := strings.Join(runtime.resolvedApps, ","); got != "Calculator" {
		t.Fatalf("background controlled apps = %q", got)
	}
	if len(runtime.preparationOptions) != 1 ||
		!runtime.preparationOptions[0].RequireBackground {
		t.Fatalf("background preparation options = %+v", runtime.preparationOptions)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,final_screenshot" {
		t.Fatalf("background semantic desktop runs=%q", got)
	}
}

func TestOpenAIComputerTaskToolAcceptsVisualSuccessAfterDynamicWindowCaptureSettles(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-final-retry",
		"turn-openai-final-retry",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	initial := agent.ToolResult{
		Content: "observed",
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "aW5pdGlhbC1pbWFnZQ==",
		}},
	}
	transient := agent.ToolResult{
		Content: "state_id: must-not-escape\nscreenshot_warning: " +
			"[transient error] computer_use_error: image_dimensions_mismatch\n" +
			"message: the exact target window capture was rejected",
		IsError:     true,
		IsRetryable: true,
	}
	final := agent.ToolResult{
		Content: "observed",
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "ZnJlc2gtZmluYWwtaW1hZ2U=",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			CoordinateActionable: false,
		},
	}
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "clicked",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
		},
		resultQueues: map[string][]agent.ToolResult{
			"final_screenshot": {
				initial,
				transient,
				transient,
				transient,
				transient,
				final,
			},
		},
	}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	llm := &openAIComputerDaemonLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			openAIComputerDaemonContinuationToken,
			string(openAIComputerDaemonCall(
				`{"type":"click","button":"left","x":10,"y":20}`,
			)),
			"",
		),
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			"resp_daemon_final_retry_completed",
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"Calculator visibly shows the requested result.\"}"}`,
			`{"status":"completed","summary":"Calculator visibly shows the requested result."}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
		observationRetry: func(context.Context, int) error {
			return nil
		},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Compute 3-10 in Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil || result.IsError ||
		result.Content != "Calculator visibly shows the requested result." {
		t.Fatalf("task result=%+v err=%v", result, err)
	}
	if result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskCompleted ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitKnown {
		t.Fatalf("completed task lost structured outcome: %+v",
			result.ComputerUseOutcome)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,final_screenshot,final_screenshot,"+
			"final_screenshot,final_screenshot,final_screenshot" {
		t.Fatalf("transient final observation replayed or reordered actions: %q", got)
	}
}

func TestOpenAIComputerTaskToolReturnsUnverifiedInsteadOfFailureWhenFinalObservationExhausts(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-final-unverified",
		"turn-openai-final-unverified",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	initial := agent.ToolResult{
		Content: "observed",
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "aW5pdGlhbC1pbWFnZQ==",
		}},
	}
	transient := agent.ToolResult{
		Content: "state_id: must-not-escape\nref=e17\nscreenshot_warning: " +
			"[transient error] computer_use_error: capture_timeout\n" +
			"message: the exact window capture timed out",
		IsRetryable: true,
	}
	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "clicked",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"final_screenshot": transient,
		},
		resultQueues: map[string][]agent.ToolResult{
			"final_screenshot": {initial},
		},
	}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	llm := &openAIComputerDaemonLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			openAIComputerDaemonContinuationToken,
			string(openAIComputerDaemonCall(
				`{"type":"click","button":"left","x":10,"y":20}`,
			)),
			"",
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
		observationRetry: func(context.Context, int) error {
			return nil
		},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Compute 3-10 in Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil || result.IsError ||
		!strings.Contains(result.Content, "computer_use_result: unverified") ||
		!strings.Contains(result.Content, "final_observation_unavailable") ||
		!strings.Contains(result.Content, "action_effect: committed") ||
		!strings.Contains(result.Content, "capture_timeout") {
		t.Fatalf("task result=%+v err=%v", result, err)
	}
	if strings.Contains(result.Content, "state_id") ||
		strings.Contains(result.Content, "ref=e17") {
		t.Fatalf("task result leaked stale observation authority: %q", result.Content)
	}
	if result.GUIOutcome == nil ||
		result.GUIOutcome.Result != agent.GUIActionResultCompletedUnverified ||
		result.GUIOutcome.Phase != agent.GUIActionPhaseVerifying ||
		result.GUIOutcome.FailureCode != "final_observation_unavailable" {
		t.Fatalf("task result lost unverified outcome: %+v", result.GUIOutcome)
	}
	if result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskUnverified ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitKnown {
		t.Fatalf("task result lost structured outcome: %+v",
			result.ComputerUseOutcome)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,final_screenshot,final_screenshot,"+
			"final_screenshot,final_screenshot,final_screenshot" {
		t.Fatalf("final observation exhaustion replayed an action: %q", got)
	}
}

func TestOpenAIComputerTaskToolExecutorFailureBeforeActionDoesNotClaimUnknownSideEffect(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-executor-failure",
		"turn-openai-executor-failure",
	)
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "aW5pdGlhbC1pbWFnZQ==",
				}},
			},
		},
	}
	taskTool := &openAIComputerTaskToolV1{
		gateway: &openAIComputerDaemonLoopLLM{
			err: errors.New("API returned 400: malformed provider response"),
		},
		profile:    trustedOpenAIComputerProfileForDaemon(t),
		childTools: agent.NewToolRegistry(),
		workflow:   workflow,
		runtime:    &openAIComputerDaemonRuntimeProbe{tool: probe},
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Inspect Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed","description":"Inspect the app"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "executor_failed_before_action") ||
		!strings.Contains(result.Content, "app launch or focus may already have occurred") ||
		strings.Contains(result.Content, "no desktop action was attempted") ||
		strings.Contains(result.Content, "alternate desktop-control tools") ||
		!strings.Contains(result.Content, "malformed provider response") {
		t.Fatalf("task result = %+v", result)
	}
}

func TestOpenAIComputerTaskToolPreservesEarlierCommitWhenLaterProviderCallFails(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-provider-failure-after-commit",
		"turn-openai-provider-failure-after-commit",
	)
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.apple.TextEdit",
		targetAppName:  "TextEdit",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "click committed",
				GUIOutcome: &agent.GUIActionOutcome{
					Result:                          agent.GUIActionResultCompletedUnverified,
					Phase:                           agent.GUIActionPhaseVerifying,
					FailureCode:                     "click_postcondition_not_declared",
					SameObservationContinuationSafe: true,
				},
			},
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "aW1hZ2U=",
				}},
			},
		},
	}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	llm := &openAIComputerDaemonLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			openAIComputerDaemonContinuationToken,
			string(openAIComputerDaemonCall(
				`{"type":"click","button":"left","x":10,"y":20}`,
			)),
			"",
		),
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			openAIComputerDaemonContinuationToken,
			string(openAIComputerDaemonCall(
				`{"type":"keypress","keys":["F13"]}`,
			)),
			"",
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     &openAIComputerDaemonRuntimeProbe{tool: probe},
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Click the visible control","controlled_apps":["TextEdit"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if result.IsError ||
		!strings.Contains(result.Content, "computer_use_result: unverified") ||
		!strings.Contains(result.Content, "executor_failed_after_commit") ||
		!strings.Contains(result.Content, "action_effect: committed") ||
		!strings.Contains(result.Content, `unsupported key "F13"`) {
		t.Fatalf("task result = %+v", result)
	}
	if result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskUnverified ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitKnown {
		t.Fatalf("task outcome = %+v", result.ComputerUseOutcome)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,final_screenshot" {
		t.Fatalf("desktop run order = %q", got)
	}
}

func TestOpenAIComputerTaskToolBoundsPrivateExecutorDuration(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-timeout",
		"turn-openai-timeout",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.google.Chrome",
		targetAppName:  "Google Chrome",
		results: map[string]agent.ToolResult{
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "aW5pdGlhbC1icm93c2Vy",
				}},
			},
		},
	}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	taskTool := &openAIComputerTaskToolV1{
		gateway:     openAIComputerDaemonBlockingLLM{},
		profile:     trustedOpenAIComputerProfileForDaemon(t),
		childTools:  agent.NewToolRegistry(),
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
		taskTimeout: 20 * time.Millisecond,
	}

	started := time.Now()
	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Open example.com in Chrome","controlled_apps":["Google Chrome"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Open the browser page"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("private executor deadline took %s", elapsed)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "executor_timeout_before_action") ||
		!strings.Contains(result.Content, "exceeded 20ms") ||
		!strings.Contains(result.Content, "app launch or focus may already have occurred") ||
		strings.Contains(result.Content, "no desktop action was attempted") ||
		strings.Contains(result.Content, "alternate desktop-control tools") {
		t.Fatalf("task result = %+v", result)
	}
	if got := strings.Join(runtime.initialObservationApps, ","); got !=
		"Google Chrome" {
		t.Fatalf("initial observation apps = %q", got)
	}
	if got := strings.Join(probe.runNames(), ","); got != "final_screenshot" {
		t.Fatalf("timeout desktop runs = %q", got)
	}
}

func TestOpenAIComputerInitialResponseClientRetriesOneBoundedStall(
	t *testing.T,
) {
	success := &client.CompletionResponse{}
	delegate := &openAIComputerDaemonInitialResponseLLM{
		succeedOn:  2,
		successful: success,
	}
	bounded := newOpenAIComputerInitialResponseClientV1(
		delegate,
		10*time.Millisecond,
		2,
	)
	response, err := bounded.Complete(
		context.Background(),
		client.CompletionRequest{},
	)
	if err != nil || response != success {
		t.Fatalf("bounded initial response=%p err=%v", response, err)
	}
	if calls := delegate.callCount(); calls != 2 {
		t.Fatalf("initial provider calls=%d, want exactly one retry", calls)
	}
	if stats := bounded.StatsV1(); stats.ModelCalls != 2 ||
		stats.ModelTimeouts != 1 {
		t.Fatalf("initial provider stats = %+v", stats)
	}

	response, err = bounded.Complete(
		context.Background(),
		client.CompletionRequest{},
	)
	if err != nil || response != success {
		t.Fatalf("unbounded continuation response=%p err=%v", response, err)
	}
	if calls := delegate.callCount(); calls != 3 {
		t.Fatalf("continuation provider calls=%d, want direct delegation", calls)
	}
	if stats := bounded.StatsV1(); stats.ModelCalls != 3 ||
		stats.ModelTimeouts != 1 {
		t.Fatalf("continuation provider stats = %+v", stats)
	}
}

func TestOpenAIComputerTaskToolBoundsAndReportsInitialProviderStall(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-initial-provider-stall",
		"turn-openai-initial-provider-stall",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.apple.TextEdit",
		targetAppName:  "TextEdit",
		results: map[string]agent.ToolResult{
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "aW5pdGlhbC10ZXh0ZWRpdA==",
				}},
			},
		},
	}
	blocking := &openAIComputerDaemonInitialResponseLLM{}
	taskTool := &openAIComputerTaskToolV1{
		gateway:                 blocking,
		profile:                 trustedOpenAIComputerProfileForDaemon(t),
		childTools:              agent.NewToolRegistry(),
		workflow:                workflow,
		runtime:                 &openAIComputerDaemonRuntimeProbe{tool: probe},
		modelTier:               "large",
		maxIter:                 4,
		resultTrunc:             2000,
		argsTrunc:               200,
		taskTimeout:             time.Second,
		initialResponseTimeout:  10 * time.Millisecond,
		initialResponseAttempts: 2,
	}

	started := time.Now()
	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Type hello in TextEdit","controlled_apps":["TextEdit"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Edit the document"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("bounded initial response took %s", elapsed)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "executor_timeout_before_action") ||
		!strings.Contains(result.Content, "after 2 bounded attempts of 10ms") ||
		!strings.Contains(result.Content, "app launch or focus may already have occurred") ||
		strings.Contains(result.Content, "no desktop action was attempted") ||
		strings.Contains(result.Content, "alternate desktop-control tools") {
		t.Fatalf("task result = %+v", result)
	}
	if calls := blocking.callCount(); calls != 2 {
		t.Fatalf("initial provider calls=%d, want exactly 2", calls)
	}
	if got := strings.Join(probe.runNames(), ","); got != "final_screenshot" {
		t.Fatalf("provider stall desktop runs = %q", got)
	}
}

func TestOpenAIComputerTaskToolRejectsCompletionClaimWithoutAnyDesktopAction(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-no-action",
		"turn-openai-no-action",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "aW5pdGlhbA==",
				}},
			},
		},
	}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	llm := &openAIComputerDaemonLoopLLM{responses: []*client.CompletionResponse{
		openAIComputerDaemonLoopResponse(
			t,
			profile,
			"resp_daemon_no_action",
			`{"type":"text","text":"I finished 250*0.01 in Calculator."}`,
			"I finished 250*0.01 in Calculator.",
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Compute 250*0.01 in Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "no_desktop_action") ||
		!strings.Contains(result.Content, "another appropriate non-computer_use control path") ||
		!strings.Contains(result.Content, "app launch or focus may already have occurred") ||
		strings.Contains(result.Content, "no desktop action was attempted") ||
		!strings.Contains(result.Content, "I finished 250*0.01 in Calculator.") ||
		result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Recovery !=
			agent.ComputerUseRecoveryAlternateControl {
		t.Fatalf("task result = %+v", result)
	}
	if got := strings.Join(probe.runNames(), ","); got != "final_screenshot" {
		t.Fatalf("desktop runs = %q, want only the initial observation", got)
	}
}

func TestOpenAIComputerTaskToolRejectsScreenshotOnlyCompletionAfterFailedMutation(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-failed-batch",
		"turn-openai-failed-batch",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "click target vanished before the press",
				IsError: true,
				GUIOutcome: &agent.GUIActionOutcome{
					Result:      agent.GUIActionResultFailed,
					Phase:       agent.GUIActionPhaseActing,
					FailureCode: "target_not_found",
				},
			},
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "ZmFpbGVkLWJhdGNo",
				}},
			},
		},
	}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	call := openAIComputerDaemonCall(
		`{"type":"click","button":"left","x":10,"y":20}`,
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
			"resp_daemon_failed_batch_final",
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"Calculator visibly shows 6.\"}"}`,
			`{"status":"completed","summary":"Calculator visibly shows 6."}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Compute 3+3 in Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if result.IsError || result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskUnverified ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitNone ||
		!strings.Contains(result.Content, "target_not_found") {
		t.Fatalf("task result = %+v", result)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,final_screenshot" {
		t.Fatalf("desktop runs = %q", got)
	}
}

func TestOpenAIComputerTaskToolReportsFailedOutcomeAfterSuccessfulBatch(
	t *testing.T,
) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-failed-outcome",
		"turn-openai-failed-outcome",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"click": {
				Content: "clicked",
				GUIOutcome: &agent.GUIActionOutcome{
					Result: agent.GUIActionResultVerified,
					Phase:  agent.GUIActionPhaseVerifying,
				},
			},
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "dW5jaGFuZ2VkLWNhbGN1bGF0b3I=",
				}},
			},
		},
	}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	call := openAIComputerDaemonCall(
		`{"type":"click","button":"left","x":10,"y":20}`,
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
			"resp_daemon_failed_outcome_final",
			`{"type":"text","text":"{\"status\":\"not_completed\",\"summary\":\"Calculator still shows 7.56; the requested result is not visible.\"}"}`,
			`{"status":"not_completed","summary":"Calculator still shows 7.56; the requested result is not visible."}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Compute 3+3 in Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "task_not_completed") ||
		!strings.Contains(
			result.Content,
			"Calculator still shows 7.56; the requested result is not visible.",
		) ||
		!strings.Contains(result.Content, "do not repeat committed desktop actions") {
		t.Fatalf("task result = %+v", result)
	}
	if result.ComputerUseOutcome == nil ||
		result.ComputerUseOutcome.Status != agent.ComputerUseTaskNotCompleted ||
		result.ComputerUseOutcome.Effect != agent.ComputerUseCommitKnown {
		t.Fatalf("not-completed task lost structured outcome: %+v",
			result.ComputerUseOutcome)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,click,final_screenshot" {
		t.Fatalf("current-app execution order = %q", got)
	}
}

func TestOpenAIComputerTaskToolRejectsMalformedTerminalOutcome(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-malformed-outcome",
		"turn-openai-malformed-outcome",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.calculator",
		targetAppName:  "Calculator",
		results: map[string]agent.ToolResult{
			"wait": {Content: "waited"},
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "bWFsZm9ybWVkLW91dGNvbWU=",
				}},
			},
		},
	}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	call := openAIComputerDaemonCall(`{"type":"wait"}`)
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
			"resp_daemon_malformed_outcome_final",
			`{"type":"text","text":"done"}`,
			"done",
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Wait for Calculator","controlled_apps":["Calculator"],`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil {
		t.Fatalf("task Run: %v", err)
	}
	if !result.IsError ||
		!strings.Contains(result.Content, "outcome_unverified") ||
		!strings.Contains(result.Content, "decode private Computer Use outcome") {
		t.Fatalf("task result = %+v", result)
	}
}

func TestOpenAIComputerTaskToolSchemaSeparatesControlledAppsAndForegroundPolicy(
	t *testing.T,
) {
	info := (&openAIComputerTaskToolV1{}).Info()
	if got := strings.Join(info.Required, ","); got !=
		"task,foreground_policy,description" {
		t.Fatalf("required fields = %q", got)
	}
	properties := info.Parameters["properties"].(map[string]any)
	controlledApps, ok := properties["controlled_apps"].(map[string]any)
	if !ok {
		t.Fatal("controlled app targets are missing")
	}
	if description, _ := controlledApps["description"].(string); !strings.Contains(description, "Do not translate app names") {
		t.Fatalf("controlled app guidance = %q", description)
	}
	if _, ok := properties["apps"]; ok {
		t.Fatal("ambiguous legacy app hints remain model-visible")
	}
	policy, ok := properties["foreground_policy"].(map[string]any)
	if !ok || !reflect.DeepEqual(
		policy["enum"],
		[]string{
			openAIComputerForegroundAllowedV1,
			openAIComputerPreserveFrontmostV1,
		},
	) {
		t.Fatalf("foreground policy schema = %+v", policy)
	}
}

func TestOpenAIComputerTaskArgsRequireOneControlledAppWhenPreservingFrontmost(
	t *testing.T,
) {
	for _, test := range []struct {
		name string
		args openAIComputerTaskArgsV1
		want string
	}{
		{
			name: "one controlled app",
			args: openAIComputerTaskArgsV1{
				ControlledApps:   []string{" Calculator ", "calculator"},
				ForegroundPolicy: openAIComputerPreserveFrontmostV1,
			},
			want: "Calculator",
		},
		{
			name: "preserved app is not another controlled app",
			args: openAIComputerTaskArgsV1{
				ControlledApps:   []string{"Calculator", "TextEdit"},
				ForegroundPolicy: openAIComputerPreserveFrontmostV1,
			},
		},
		{
			name: "policy is explicit",
			args: openAIComputerTaskArgsV1{
				ControlledApps: []string{"Calculator"},
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			apps, err := normalizeOpenAIComputerTaskArgsV1(&test.args)
			if test.want == "" {
				if err == nil {
					t.Fatalf("normalized apps = %+v, want error", apps)
				}
				return
			}
			if err != nil || strings.Join(apps, ",") != test.want {
				t.Fatalf("normalized apps = %+v, err=%v", apps, err)
			}
		})
	}
}

func TestOpenAIComputerTaskToolRunsAgainstCurrentAppWithoutAppHints(t *testing.T) {
	coordinator := guicontrol.NewCoordinator(guicontrol.CoordinatorOptions{})
	workflow := testGUIWorkflow(
		coordinator,
		"session-openai-current-app",
		"turn-openai-current-app",
	)
	workflow.invocationFromContext = agent.ToolInvocationFromContext
	autoAcknowledgeOpenAIComputerController(t, coordinator)
	defer workflow.EndTurn()

	probe := &openAIComputerDaemonProbeTool{
		targetBundleID: "com.example.slack",
		targetAppName:  "Slack",
		results: map[string]agent.ToolResult{
			"final_screenshot": {
				Content: "observed",
				Images: []agent.ImageBlock{{
					MediaType: "image/png",
					Data:      "Y3VycmVudC1hcHA=",
				}},
			},
		},
	}
	runtime := &openAIComputerDaemonRuntimeProbe{tool: probe}
	profile := trustedOpenAIComputerProfileForDaemon(t)
	call := openAIComputerDaemonCall(`{"type":"wait"}`)
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
			"resp_daemon_current_app_final",
			`{"type":"text","text":"{\"status\":\"completed\",\"summary\":\"done\"}"}`,
			`{"status":"completed","summary":"done"}`,
		),
	}}
	childTools := agent.NewToolRegistry()
	childTools.Register(tools.NewOpenAIComputerAdapterV1(nil))
	taskTool := &openAIComputerTaskToolV1{
		gateway:     llm,
		profile:     profile,
		childTools:  childTools,
		workflow:    workflow,
		runtime:     runtime,
		modelTier:   "large",
		maxIter:     4,
		resultTrunc: 2000,
		argsTrunc:   200,
	}

	result, err := taskTool.Run(
		context.Background(),
		`{"task":"Wait for the current app to settle",`+
			`"foreground_policy":"foreground_allowed",`+
			`"description":"Complete the desktop task"}`,
	)
	if err != nil || result.IsError || result.Content != "done" {
		t.Fatalf("current-app task result = %+v / %v", result, err)
	}
	if len(runtime.resolvedApps) != 0 || len(runtime.launchedApps) != 0 {
		t.Fatalf(
			"current-app task resolved/launched hints: %v / %v",
			runtime.resolvedApps,
			runtime.launchedApps,
		)
	}
	if workflow.lease == nil ||
		workflow.lease.RequestedAppBundleID != "com.example.slack" {
		t.Fatalf("current-app task lease = %+v", workflow.lease)
	}
	if got := strings.Join(probe.runNames(), ","); got !=
		"final_screenshot,wait,final_screenshot" {
		t.Fatalf("current-app execution order = %q", got)
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
	if private == nil || private.runtime == nil {
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
	if registry.Has("computer_use") {
		t.Fatal("workflow wrapping re-registered private computer_use")
	}
	runner, err := newDaemonOpenAIComputerBatchRunnerV1(
		workflow,
		private.runtime,
	)
	if err != nil {
		t.Fatalf("construct post-wrap runner: %v", err)
	}
	if runner.runtime != private.runtime {
		t.Fatal("runner did not retain the detached runtime")
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
	executor, adapter, probe, coordinator := newOpenAIComputerDaemonExecutorFixture(t)
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
	executor, adapter, probe, coordinator := newOpenAIComputerDaemonExecutorFixture(t)
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
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
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
	if result.ActionEffect != agent.ComputerUseCommitUnknown {
		t.Fatalf("unknown action effect = %q", result.ActionEffect)
	}
	if result.ToolResult.GUIOutcome == nil ||
		result.ToolResult.GUIOutcome.FailureCode != "commit_unknown" {
		t.Fatalf("unknown action lost typed outcome: %+v",
			result.ToolResult.GUIOutcome)
	}
	if got := probe.runNames(); strings.Join(got, ",") != "click,final_screenshot" {
		t.Fatalf("uncertain action continued or retried: %v", got)
	}
}

func TestDaemonOpenAIComputerExecutorPartialPixelScrollCommitNeverContinuesOrRetries(t *testing.T) {
	executor, adapter, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
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
	)
	if err != nil {
		t.Fatal(err)
	}
	mustOpenAIComputerDaemonLease(t, workflow, "com.apple.Notes", "Notes")
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
	executor, _, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
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
	executor, _, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
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
			executor, _, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
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
			executor, _, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
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
	executor, _, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
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
	executor, _, probe, _ := newOpenAIComputerDaemonExecutorFixture(t)
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

func TestDaemonOpenAIComputerExecutorRuntimeErrorsAreRedacted(t *testing.T) {
	executor, _, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
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
	if err != nil || authority.LeaseID == "" {
		t.Fatalf("acquire authority: authority=%+v err=%v", authority, err)
	}
	scope := tools.OpenAIComputerActionScopeV1{
		ResponseID:   call.ResponseID,
		CallID:       call.CallID,
		Provider:     call.Provider,
		APISurface:   call.APISurface,
		ToolContract: call.ToolContract,
		ActionID:     call.CallID + "/action/1",
		ActionIndex:  0,
		ActionCount:  1,
	}
	execution, err := executor.ExecuteAuthorizedOpenAIComputerActionV1(
		context.Background(),
		authority,
		scope,
		call.Actions[0],
	)
	if err == nil || strings.Contains(err.Error(), "secret typed text") ||
		strings.Contains(execution.Result.Content, "secret typed text") ||
		execution.CommitState != tools.OpenAIComputerNotCommittedV1 ||
		execution.Result.GUIOutcome == nil ||
		execution.Result.GUIOutcome.FailureCode !=
			"action_projection_failed" {
		t.Fatalf("runtime error leaked or committed: execution=%+v err=%v", execution, err)
	}
}

func TestDaemonOpenAIComputerExecutorPreservesTypedPlanFailureCode(t *testing.T) {
	executor, _, _, _ := newOpenAIComputerDaemonExecutorFixture(t)
	defer executor.EndBatchV1()
	executor.runtime = openAIComputerRuntimeErrorProbe{
		err: &tools.OpenAIComputerActionPlanErrorV1{
			FailureCode: "keyboard_plan_focused_ref_unavailable",
			Detail:      "no unique focused AX element exists",
		},
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
	if err != nil {
		t.Fatal(err)
	}
	scope := tools.OpenAIComputerActionScopeV1{
		ResponseID: call.ResponseID, CallID: call.CallID,
		Provider: call.Provider, APISurface: call.APISurface,
		ToolContract: call.ToolContract,
		ActionID:     call.CallID + "/action/1",
		ActionIndex:  0, ActionCount: 1,
	}
	execution, err := executor.ExecuteAuthorizedOpenAIComputerActionV1(
		context.Background(),
		authority,
		scope,
		call.Actions[0],
	)
	if err == nil ||
		execution.CommitState != tools.OpenAIComputerNotCommittedV1 ||
		execution.Result.GUIOutcome == nil ||
		execution.Result.GUIOutcome.FailureCode !=
			"keyboard_plan_focused_ref_unavailable" ||
		!strings.Contains(
			execution.Result.Content,
			"no unique focused AX element exists",
		) ||
		strings.Contains(execution.Result.Content, "private") {
		t.Fatalf("typed plan failure = %+v / %v", execution, err)
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

func (p openAIComputerRuntimeErrorProbe) AuthorizeOpenAIComputerTypeAfterKeypressV1(
	tools.OpenAIComputerActionV1,
) error {
	return p.err
}
