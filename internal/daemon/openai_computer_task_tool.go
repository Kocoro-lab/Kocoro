package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/hooks"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

type openAIComputerTaskArgsV1 struct {
	Task        string   `json:"task"`
	Apps        []string `json:"apps,omitempty"`
	Description string   `json:"description,omitempty"`
}

const (
	openAIComputerTaskCompletedV1 = "completed"
	openAIComputerTaskFailedV1    = "failed"
)

// openAIComputerTaskOutcomeV1 is the private executor's terminal contract.
// Batch transport status cannot represent goal completion: an action can report
// an error even though the final screenshot proves success, or report success
// while the screenshot proves that the UI did not change.
type openAIComputerTaskOutcomeV1 struct {
	Status  string `json:"status"`
	Summary string `json:"summary"`
}

func parseOpenAIComputerTaskOutcomeV1(raw string) (openAIComputerTaskOutcomeV1, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	var outcome openAIComputerTaskOutcomeV1
	if err := decoder.Decode(&outcome); err != nil {
		return openAIComputerTaskOutcomeV1{},
			fmt.Errorf("decode private Computer Use outcome: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = fmt.Errorf("multiple JSON values")
		}
		return openAIComputerTaskOutcomeV1{},
			fmt.Errorf("decode private Computer Use outcome: %w", err)
	}
	outcome.Status = strings.TrimSpace(outcome.Status)
	outcome.Summary = strings.TrimSpace(outcome.Summary)
	switch outcome.Status {
	case openAIComputerTaskCompletedV1, openAIComputerTaskFailedV1:
	default:
		return openAIComputerTaskOutcomeV1{},
			fmt.Errorf("private Computer Use outcome status %q is invalid", outcome.Status)
	}
	if outcome.Summary == "" {
		return openAIComputerTaskOutcomeV1{},
			fmt.Errorf("private Computer Use outcome summary is required")
	}
	return outcome, nil
}

type openAIComputerTaskRuntimeV1 interface {
	openAIComputerActionRuntimeV1
	ResolveTaskAppV1(
		context.Context,
		string,
	) (tools.OpenAIComputerTaskAppV1, error)
	LaunchAndFocusTaskAppsV1(
		context.Context,
		[]tools.OpenAIComputerTaskAppV1,
	) error
	PlanOpenAIComputerTaskInitialObservationV1(
		*tools.OpenAIComputerTaskAppV1,
		string,
		bool,
	) (tools.OpenAIComputerActionPlanV1, error)
}

// openAIComputerTaskToolV1 is the only model-visible desktop-control surface.
// The parent model delegates one complete goal; a private OpenAI Responses loop
// owns screenshots, ordered action batches, and continuation state.
type openAIComputerTaskToolV1 struct {
	gateway         client.LLMClient
	profile         *client.ExecutionProfile
	resolveProfile  func(context.Context) (*client.ExecutionProfile, error)
	profileOnce     sync.Once
	resolvedProfile *client.ExecutionProfile
	profileErr      error
	childTools      *agent.ToolRegistry
	workflow        *daemonGUIWorkflow
	runtime         openAIComputerTaskRuntimeV1
	appPolicy       *ComputerUseAppPolicyStore
	handler         agent.EventHandler

	modelTier   string
	shannonDir  string
	maxIter     int
	maxTokens   int
	resultTrunc int
	argsTrunc   int
	taskTimeout time.Duration
	// Tests replace this seam so cold-app readiness retries never sleep.
	// The argument is the one-based failed attempt that precedes the wait.
	initialObservationRetry func(context.Context, int) error
	permissions             *permissions.PermissionsConfig
	auditor                 *audit.AuditLogger
	hookRunner              *hooks.HookRunner
}

const (
	defaultOpenAIComputerTaskTimeoutV1        = 2 * time.Minute
	maxOpenAIComputerInitialObservationsV1    = 5
	maxOpenAIComputerObservationDetailRunesV1 = 500
)

var openAIComputerInitialObservationRetryDelaysV1 = [...]time.Duration{
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	1200 * time.Millisecond,
}

func waitOpenAIComputerInitialObservationRetryV1(
	ctx context.Context,
	failedAttempt int,
) error {
	index := failedAttempt - 1
	if index < 0 || index >= len(openAIComputerInitialObservationRetryDelaysV1) {
		return nil
	}
	timer := time.NewTimer(openAIComputerInitialObservationRetryDelaysV1[index])
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func boundOpenAIComputerObservationDetailV1(value string) string {
	value = strings.TrimSpace(value)
	runes := []rune(value)
	if len(runes) <= maxOpenAIComputerObservationDetailRunesV1 {
		return value
	}
	return string(runes[:maxOpenAIComputerObservationDetailRunesV1]) + "..."
}

// initialObservationResultDetailV1 returns only the capture failure envelope,
// never the successful AX state that preceded it. A get_app_state result may
// contain refs, values, and state IDs before its screenshot_warning; those are
// private executor authority and must not escape into the parent transcript.
func initialObservationResultDetailV1(result agent.ToolResult) string {
	content := result.Content
	if marker := strings.LastIndex(content, "screenshot_warning:"); marker >= 0 {
		content = strings.TrimSpace(content[marker+len("screenshot_warning:"):])
	}
	lines := strings.Split(content, "\n")
	code := ""
	message := ""
	for _, line := range lines {
		line = strings.TrimSpace(line)
		switch {
		case strings.Contains(line, "computer_use_error:"):
			marker := strings.Index(line, "computer_use_error:")
			code = strings.TrimSpace(line[marker+len("computer_use_error:"):])
		case strings.HasPrefix(line, "message:"):
			message = strings.TrimSpace(strings.TrimPrefix(line, "message:"))
		}
	}
	if code != "" {
		if message != "" {
			return boundOpenAIComputerObservationDetailV1(code + ": " + message)
		}
		return boundOpenAIComputerObservationDetailV1(code)
	}
	if marker := strings.LastIndex(result.Content, "screenshot_warning:"); marker >= 0 {
		warning := strings.TrimSpace(
			result.Content[marker+len("screenshot_warning:"):],
		)
		if line, _, found := strings.Cut(warning, "\n"); found {
			warning = line
		}
		if warning != "" {
			return boundOpenAIComputerObservationDetailV1(
				"screenshot capture warning: " + warning,
			)
		}
	}
	if result.GUIOutcome != nil &&
		strings.TrimSpace(result.GUIOutcome.FailureCode) != "" {
		return boundOpenAIComputerObservationDetailV1(
			strings.TrimSpace(result.GUIOutcome.FailureCode),
		)
	}
	if result.IsError {
		return "the desktop observation tool returned an error without a verified image"
	}
	return "the desktop observation completed without a verified image"
}

func retryOpenAIComputerInitialObservationV1(
	result agent.ToolResult,
	runErr error,
	planErr error,
) bool {
	if planErr != nil || runErr != nil {
		return false
	}
	if result.IsRetryable {
		return true
	}
	switch openAIComputerTraceFailureCodeV1(result, nil) {
	case "window_not_found",
		"window_not_actionable",
		"display_not_actionable",
		"window_bounds_mismatch",
		"window_changed",
		"topology_changed",
		"stale_topology",
		"capture_timeout",
		"capture_failed",
		"topology_unavailable",
		"process_identity_mismatch",
		"window_identity_mismatch":
		return true
	}
	// get_app_state deliberately preserves a useful AX observation when its
	// optional image capture fails, so this shape has IsError=false. Retrying
	// the observation is safe and is the cold-launch readiness path.
	return !result.IsError && len(result.Images) != 1
}

func (t *openAIComputerTaskToolV1) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "computer_use",
		Description: "Operate native macOS desktop apps to complete one full user goal. " +
			"Give the complete task once; the computer executor launches/focuses apps, " +
			"observes the current UI, performs the needed actions, and verifies the result internally. " +
			"Do not split clicks, typing, screenshots, or app switches into separate calls. " +
			"List app names in apps when known; the executor may switch apps itself.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"task": map[string]any{
					"type":        "string",
					"description": "The complete desktop task and desired end state.",
				},
				"apps": map[string]any{
					"type":        "array",
					"items":       map[string]any{"type": "string"},
					"description": "Optional installed macOS app names to launch before starting.",
				},
				"description": agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"task", "description"},
	}
}

func (t *openAIComputerTaskToolV1) RequiresApproval() bool { return true }

func (t *openAIComputerTaskToolV1) Run(
	ctx context.Context,
	argsJSON string,
) (agent.ToolResult, error) {
	var args openAIComputerTaskArgsV1
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	args.Task = strings.TrimSpace(args.Task)
	args.Description = strings.TrimSpace(args.Description)
	if args.Task == "" {
		return agent.ValidationError("task is required"), nil
	}
	if args.Description == "" {
		return agent.ValidationError("description is required"), nil
	}
	if t == nil || t.gateway == nil ||
		t.childTools == nil || t.workflow == nil || t.runtime == nil {
		return agent.BusinessError(
			"OpenAI native Computer Use is temporarily unavailable; no desktop action was attempted",
		), nil
	}
	taskStarted := time.Now()
	trace := newOpenAIComputerTraceV1(t.auditor, t.workflow.request)
	trace.record(openAIComputerTraceEventV1{
		Phase:  "task",
		Status: "started",
	})
	profile := t.profile
	if profile == nil && t.resolveProfile != nil {
		t.profileOnce.Do(func() {
			t.resolvedProfile, t.profileErr = t.resolveProfile(ctx)
		})
		profile = t.resolvedProfile
		if t.profileErr != nil {
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "failed",
				FailureCode: "backend_contract_unavailable",
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			return agent.BusinessError(
				"computer_use_error: backend_contract_unavailable\n" +
					"message: OpenAI native Computer Use is unavailable because its execution profile could not be resolved\n" +
					"recovery: do not retry computer_use in this turn; no desktop action was attempted\n" +
					"detail: " + t.profileErr.Error(),
			), nil
		}
	}
	if profile == nil ||
		!profile.IsTrustedResolution() ||
		profile.Provider() != client.OpenAIComputerProvider ||
		profile.APISurface() != client.APISurfaceOpenAIResponses ||
		profile.ExecutionMode() != client.ExecutionModeNativeComputer ||
		profile.ToolContract() != client.ToolContractOpenAIComputerV1 ||
		!profile.SupportsImageInput() ||
		!profile.SupportsToolResultImages() ||
		profile.SupportsFunctionTools() ||
		!profile.SupportsBatchedActions() {
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "failed",
			FailureCode: "backend_contract_unsupported",
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return agent.BusinessError(
			"OpenAI native Computer Use is temporarily unavailable; no desktop action was attempted",
		), nil
	}

	seen := make(map[string]struct{}, len(args.Apps))
	apps := make([]tools.OpenAIComputerTaskAppV1, 0, len(args.Apps))
	for _, requested := range args.Apps {
		requested = strings.TrimSpace(requested)
		if requested == "" {
			continue
		}
		key := strings.ToLower(requested)
		if _, duplicate := seen[key]; duplicate {
			continue
		}
		seen[key] = struct{}{}
		resolutionStarted := time.Now()
		identity, err := t.runtime.ResolveTaskAppV1(ctx, requested)
		if err != nil {
			trace.record(openAIComputerTraceEventV1{
				Phase:       "app_resolution",
				Status:      "failed",
				FailureCode: "app_resolution_failed",
				DurationMS:  time.Since(resolutionStarted).Milliseconds(),
			})
			return agent.BusinessError(err.Error()), nil
		}
		trace.record(openAIComputerTraceEventV1{
			Phase:       "app_resolution",
			Status:      "completed",
			AppBundleID: identity.BundleID,
			DurationMS:  time.Since(resolutionStarted).Milliseconds(),
		})
		if t.appPolicy != nil &&
			t.appPolicy.DecisionFor(identity.BundleID).Decision ==
				ComputerUseAppPolicyBlocked {
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "failed",
				AppBundleID: identity.BundleID,
				FailureCode: "saved_app_blocked",
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			return agent.PermissionError(
				fmt.Sprintf("%s is blocked in Saved App Blocks", identity.App),
			), nil
		}
		apps = append(apps, identity)
	}
	preparationStarted := time.Now()
	if len(apps) > 0 {
		if _, err := t.workflow.ensureLease(
			ctx,
			agent.GUIActionDescriptor{
				Participates:   true,
				ActionKind:     "desktop_task",
				Effect:         agent.GUIActionMutation,
				TargetBundleID: apps[0].BundleID,
				TargetAppName:  apps[0].App,
				ExecutionPath:  "openai_native",
			},
		); err != nil {
			trace.record(openAIComputerTraceEventV1{
				Phase:       "app_launch_focus",
				Status:      "failed",
				AppBundleID: apps[0].BundleID,
				FailureCode: "controller_unavailable",
				DurationMS:  time.Since(preparationStarted).Milliseconds(),
			})
			return guiCoordinatorToolError(err), nil
		}
	}
	if err := t.runtime.LaunchAndFocusTaskAppsV1(ctx, apps); err != nil {
		event := openAIComputerTraceEventV1{
			Phase:       "app_launch_focus",
			Status:      "failed",
			FailureCode: "app_launch_focus_failed",
			DurationMS:  time.Since(preparationStarted).Milliseconds(),
		}
		if len(apps) > 0 {
			event.AppBundleID = apps[0].BundleID
		}
		trace.record(event)
		return agent.BusinessError(err.Error()), nil
	}
	preparationEvent := openAIComputerTraceEventV1{
		Phase:      "app_launch_focus",
		Status:     "completed",
		DurationMS: time.Since(preparationStarted).Milliseconds(),
	}
	if len(apps) > 0 {
		preparationEvent.AppBundleID = apps[0].BundleID
	}
	trace.record(preparationEvent)

	invocationCtx := tools.ContextWithOpenAINativeComputerActionV1(
		agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{
			ToolName:  "computer_use",
			ToolUseID: "computer-task/initial-observation",
		}),
	)
	var initial agent.ToolResult
	var runErr error
	var observationErr error
	observationDetail := ""
	var initialApp *tools.OpenAIComputerTaskAppV1
	if len(apps) > 0 {
		initialApp = &apps[0]
	}
	retryInitialObservation := t.initialObservationRetry
	if retryInitialObservation == nil {
		retryInitialObservation = waitOpenAIComputerInitialObservationRetryV1
	}
	for attempt := 1; attempt <= maxOpenAIComputerInitialObservationsV1; attempt++ {
		observationStarted := time.Now()
		initial = agent.ToolResult{}
		runErr = nil
		plan, planErr := t.runtime.PlanOpenAIComputerTaskInitialObservationV1(
			initialApp,
			"Capture the initial desktop task state",
			true,
		)
		if planErr != nil {
			observationErr = planErr
			observationDetail = boundOpenAIComputerObservationDetailV1(planErr.Error())
		} else {
			initial, runErr = t.workflow.runTool(invocationCtx, plan.Tool, plan.Args)
			if runErr == nil && !initial.IsError && len(initial.Images) == 1 {
				event := openAIComputerTraceEventV1{
					Phase:      "initial_observation",
					Status:     "completed",
					Attempt:    attempt,
					DurationMS: time.Since(observationStarted).Milliseconds(),
				}
				if initialApp != nil {
					event.AppBundleID = initialApp.BundleID
				}
				trace.record(event)
				observationErr = nil
				observationDetail = ""
				break
			}
			observationErr = runErr
			if runErr != nil {
				observationDetail = boundOpenAIComputerObservationDetailV1(
					runErr.Error(),
				)
			} else {
				observationDetail = initialObservationResultDetailV1(initial)
			}
		}
		event := openAIComputerTraceEventV1{
			Phase:       "initial_observation",
			Status:      "failed",
			Attempt:     attempt,
			FailureCode: openAIComputerTraceFailureCodeV1(initial, observationErr),
			DurationMS:  time.Since(observationStarted).Milliseconds(),
		}
		if event.FailureCode == "" {
			event.FailureCode = "initial_image_unavailable"
		}
		if initialApp != nil {
			event.AppBundleID = initialApp.BundleID
		}
		trace.record(event)
		if !retryOpenAIComputerInitialObservationV1(
			initial,
			runErr,
			planErr,
		) {
			break
		}
		if attempt < maxOpenAIComputerInitialObservationsV1 {
			if err := retryInitialObservation(ctx, attempt); err != nil {
				observationErr = err
				observationDetail = boundOpenAIComputerObservationDetailV1(
					err.Error(),
				)
				break
			}
		}
	}
	if runErr != nil || initial.IsError || len(initial.Images) != 1 {
		detail := observationDetail
		if detail == "" && observationErr != nil {
			detail = boundOpenAIComputerObservationDetailV1(
				observationErr.Error(),
			)
		}
		if detail == "" {
			detail = "the desktop observation backend returned an error"
		}
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "failed",
			FailureCode: "initial_observation_unavailable",
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return agent.BusinessError(
			"computer_use_error: initial_observation_unavailable\n" +
				"message: Computer Use could not capture the verified initial app window\n" +
				"recovery: do not retry computer_use again in this turn; report that the initial desktop state could not be observed\n" +
				"detail: " + detail,
		), nil
	}

	runner, err := newDaemonOpenAIComputerBatchRunnerV1(
		t.workflow,
		t.runtime,
	)
	if err != nil {
		return agent.BusinessError(err.Error()), nil
	}
	runner.trace = trace
	child := agent.NewAgentLoop(
		t.gateway,
		t.childTools,
		t.modelTier,
		t.shannonDir,
		t.maxIter,
		t.resultTrunc,
		t.argsTrunc,
		t.permissions,
		t.auditor,
		t.hookRunner,
	)
	child.SetSkillDiscovery(false)
	child.SetBypassPermissions(true)
	child.SetSpecificModel(profile.Model())
	child.SetExecutionProfile(profile)
	child.SetOpenAIComputerBatchExecutor(runner)
	child.SetForceInitialToolUse(true)
	child.SetHandler(openAIComputerChildHandlerV1{parent: t.handler})
	child.SetStickyContext(
		"execution_role=private_openai_native_computer\n" +
			"Complete the user's entire desktop goal with the native computer tool. " +
			"The initial image is the current verified app window. " +
			"Use coordinates from the latest returned screenshot, continue across app switches, " +
			"and stop as soon as the requested end state is verified. " +
			"Do not use Return, Enter, Space, or destination-free save, quit, or delete keyboard shortcuts to activate controls. " +
			"For ordinary URL navigation only, use the exact sequence Meta+L, type the URL, then Return; Kocoro admits that one window-bound navigation chain. " +
			"Click the intended visible button for harmless dialogs. " +
			"For send, delete, or purchase actions, click the exact visible action button and wait for Kocoro's one local confirmation; " +
			"never substitute a keyboard shortcut for that confirmed button. " +
			"Do not ask the parent to perform clicks, typing, screenshots, or state management. " +
			"Your final response is a machine-readable result: return exactly one compact JSON object " +
			`{"status":"completed","summary":"brief visible result"} only when the latest screenshot visibly proves the requested end state. ` +
			`Otherwise return exactly {"status":"failed","summary":"brief reason the visible end state was not reached or cannot be verified"}. ` +
			"Do not use Markdown, add fields, or put any text outside that JSON object. " +
			"If an action may have committed but the latest screenshot does not prove the result, use failed.",
	)
	if t.maxTokens > 0 {
		child.SetMaxTokens(t.maxTokens)
	}
	content := []client.ContentBlock{{
		Type: "image",
		Source: &client.ImageSource{
			Type:      "base64",
			MediaType: initial.Images[0].MediaType,
			Data:      initial.Images[0].Data,
		},
	}}
	taskTimeout := t.taskTimeout
	if taskTimeout <= 0 {
		taskTimeout = defaultOpenAIComputerTaskTimeoutV1
	}
	taskCtx, cancelTask := context.WithTimeout(ctx, taskTimeout)
	defer cancelTask()
	providerStarted := time.Now()
	reply, _, err := child.Run(taskCtx, args.Task, content, nil)
	providerEvent := openAIComputerTraceEventV1{
		Phase:      "private_executor",
		Status:     "completed",
		DurationMS: time.Since(providerStarted).Milliseconds(),
	}
	if err != nil {
		providerEvent.Status = "failed"
		providerEvent.FailureCode = openAIComputerTraceFailureCodeV1(
			agent.ToolResult{},
			err,
		)
	}
	trace.record(providerEvent)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "failed",
				FailureCode: "executor_timeout",
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			return agent.BusinessError(
				"computer_use_error: executor_timeout\n" +
					"message: the private OpenAI Computer Use executor exceeded its interactive task deadline\n" +
					"recovery: do not retry computer_use or attempt alternate desktop-control tools in this turn; report that the desktop task timed out\n" +
					"detail: the private executor exceeded " + taskTimeout.String(),
			), nil
		}
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "failed",
			FailureCode: "executor_failed",
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return agent.BusinessError(
			"computer_use_error: executor_failed\n" +
				"message: the private OpenAI Computer Use executor could not complete the task\n" +
				"recovery: do not retry computer_use or attempt alternate desktop-control tools in this turn; report the executor failure without guessing that the target app is missing or blocked\n" +
				"detail: " + err.Error(),
		), nil
	}
	reply = strings.TrimSpace(reply)
	stats := runner.BatchStatsV1()
	if stats.Batches == 0 {
		detail := reply
		if detail == "" {
			detail = "the executor returned no summary"
		}
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "failed",
			FailureCode: "no_desktop_action",
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return agent.BusinessError(
			"computer_use_error: no_desktop_action\n" +
				"message: the private OpenAI Computer Use executor finished without issuing any native computer batch, so the task was not done\n" +
				"recovery: do not retry computer_use again in this turn; report that the native executor performed no action\n" +
				"detail: " + detail,
		), nil
	}
	outcome, outcomeErr := parseOpenAIComputerTaskOutcomeV1(reply)
	if outcomeErr != nil {
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "failed",
			FailureCode: "outcome_unverified",
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return agent.BusinessError(
			"computer_use_error: outcome_unverified\n" +
				"message: the private OpenAI Computer Use executor did not return a verifiable task outcome\n" +
				"recovery: do not retry computer_use again in this turn; report that the executor outcome could not be verified\n" +
				"detail: " + outcomeErr.Error(),
		), nil
	}
	if outcome.Status == openAIComputerTaskFailedV1 {
		detail := outcome.Summary
		if stats.LastFailureDetail != "" {
			detail += "; last executor failure: " + stats.LastFailureDetail
		}
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "failed",
			FailureCode: "task_failed",
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return agent.BusinessError(
			"computer_use_error: task_failed\n" +
				"message: the private OpenAI Computer Use executor reported that the requested visible end state was not reached\n" +
				"recovery: do not retry computer_use again in this turn; report the executor's visible-state result\n" +
				"detail: " + detail,
		), nil
	}
	trace.record(openAIComputerTraceEventV1{
		Phase:      "task",
		Status:     "completed",
		DurationMS: time.Since(taskStarted).Milliseconds(),
	})
	return agent.ToolResult{Content: outcome.Summary}, nil
}

// The child shares approvals and usage reporting with the parent transport,
// while its intermediate narration stays inside the single parent tool card.
type openAIComputerChildHandlerV1 struct {
	parent agent.EventHandler
}

func (h openAIComputerChildHandlerV1) OnToolCall(string, string, string) {}
func (h openAIComputerChildHandlerV1) OnToolResult(
	string, string, string, agent.ToolResult, time.Duration,
) {
}
func (h openAIComputerChildHandlerV1) OnText(string)                       {}
func (h openAIComputerChildHandlerV1) OnPreamble(string)                   {}
func (h openAIComputerChildHandlerV1) OnStreamDelta(string)                {}
func (h openAIComputerChildHandlerV1) OnCloudAgent(string, string, string) {}
func (h openAIComputerChildHandlerV1) OnCloudProgress(int, int)            {}
func (h openAIComputerChildHandlerV1) OnCloudPlan(string, string, bool)    {}
func (h openAIComputerChildHandlerV1) OnUsage(usage agent.TurnUsage) {
	if h.parent != nil {
		h.parent.OnUsage(usage)
	}
}
func (h openAIComputerChildHandlerV1) OnApprovalNeeded(
	tool string,
	args string,
) bool {
	return h.parent != nil && h.parent.OnApprovalNeeded(tool, args)
}

var _ agent.Tool = (*openAIComputerTaskToolV1)(nil)
