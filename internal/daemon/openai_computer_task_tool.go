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

type openAIComputerChildGoalV1 struct {
	OriginalUserRequest string `json:"original_user_request"`
	ParentDesktopPlan   string `json:"parent_desktop_plan"`
}

type openAIComputerInitialResponseUnavailableV1 struct {
	Attempts int
	Window   time.Duration
}

func (e *openAIComputerInitialResponseUnavailableV1) Error() string {
	return fmt.Sprintf(
		"private OpenAI Computer Use initial response unavailable after %d bounded attempts of %s",
		e.Attempts,
		e.Window,
	)
}

// openAIComputerInitialResponseClientV1 bounds only the first private-provider
// response. A dropped initial request previously consumed the whole two-minute
// desktop task without issuing one action. Continuations retain the ordinary
// task deadline because slow page/app transitions can legitimately need more
// time after visible progress has begun.
type openAIComputerInitialResponseClientV1 struct {
	delegate client.LLMClient
	window   time.Duration
	attempts int

	mu          sync.Mutex
	completed   bool
	unavailable *openAIComputerInitialResponseUnavailableV1
}

func newOpenAIComputerInitialResponseClientV1(
	delegate client.LLMClient,
	window time.Duration,
	attempts int,
) *openAIComputerInitialResponseClientV1 {
	if window <= 0 {
		window = defaultOpenAIComputerInitialResponseTimeoutV1
	}
	if attempts <= 0 {
		attempts = defaultOpenAIComputerInitialResponseAttemptsV1
	}
	return &openAIComputerInitialResponseClientV1{
		delegate: delegate,
		window:   window,
		attempts: attempts,
	}
}

func (c *openAIComputerInitialResponseClientV1) complete(
	ctx context.Context,
	invoke func(context.Context) (*client.CompletionResponse, error),
) (*client.CompletionResponse, error) {
	c.mu.Lock()
	if c.completed {
		c.mu.Unlock()
		return invoke(ctx)
	}
	if c.unavailable != nil {
		unavailable := c.unavailable
		c.mu.Unlock()
		return nil, unavailable
	}
	defer c.mu.Unlock()

	for attempt := 1; attempt <= c.attempts; attempt++ {
		attemptCtx, cancel := context.WithTimeout(ctx, c.window)
		response, err := invoke(attemptCtx)
		attemptExpired := errors.Is(attemptCtx.Err(), context.DeadlineExceeded)
		cancel()
		if err == nil {
			c.completed = true
			return response, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if !attemptExpired {
			return nil, err
		}
	}
	c.unavailable = &openAIComputerInitialResponseUnavailableV1{
		Attempts: c.attempts,
		Window:   c.window,
	}
	return nil, c.unavailable
}

func (c *openAIComputerInitialResponseClientV1) Complete(
	ctx context.Context,
	request client.CompletionRequest,
) (*client.CompletionResponse, error) {
	return c.complete(ctx, func(attemptCtx context.Context) (*client.CompletionResponse, error) {
		return c.delegate.Complete(attemptCtx, request)
	})
}

func (c *openAIComputerInitialResponseClientV1) CompleteStream(
	ctx context.Context,
	request client.CompletionRequest,
	onDelta func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return c.complete(ctx, func(attemptCtx context.Context) (*client.CompletionResponse, error) {
		return c.delegate.CompleteStream(attemptCtx, request, onDelta)
	})
}

const (
	openAIComputerTaskCompletedV1    = "completed"
	openAIComputerTaskNotCompletedV1 = "not_completed"
	openAIComputerTaskUnverifiedV1   = "unverified"
)

func openAIComputerChildGoalInputV1(
	ctx context.Context,
	parentDesktopPlan string,
) string {
	parentDesktopPlan = strings.TrimSpace(parentDesktopPlan)
	invocation, ok := agent.ToolInvocationFromContext(ctx)
	if !ok {
		return parentDesktopPlan
	}
	originalUserRequest := strings.TrimSpace(invocation.UserRequest)
	if originalUserRequest == "" || originalUserRequest == parentDesktopPlan {
		return parentDesktopPlan
	}
	encoded, err := json.Marshal(openAIComputerChildGoalV1{
		OriginalUserRequest: originalUserRequest,
		ParentDesktopPlan:   parentDesktopPlan,
	})
	if err != nil {
		return parentDesktopPlan
	}
	return string(encoded)
}

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
	case openAIComputerTaskCompletedV1,
		openAIComputerTaskNotCompletedV1,
		openAIComputerTaskUnverifiedV1:
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

func openAIComputerTaskUnverifiedResultV1(
	failureCode string,
	detail string,
	effect agent.ComputerUseCommitEffect,
) agent.ToolResult {
	failureCode = strings.TrimSpace(failureCode)
	if failureCode == "" {
		failureCode = "goal_unverified"
	}
	detail = strings.Join(strings.Fields(
		boundOpenAIComputerObservationDetailV1(detail),
	), " ")
	if detail == "" {
		detail = "the final visible state could not be verified"
	}
	message := "the requested final desktop state could not be verified"
	recovery := "obtain a fresh observation before any further mutation"
	if effect == "" {
		effect = agent.ComputerUseCommitNone
	}
	actionEffect := string(effect)
	if effect == agent.ComputerUseCommitKnown {
		message = "desktop actions may have completed, but the requested final state could not be verified"
		recovery = "do not repeat committed actions; obtain a fresh observation before any further mutation"
	} else if effect == agent.ComputerUseCommitUnknown {
		message = "a desktop action has an unresolved commit state, so the requested final state is unverified"
		recovery = "do not repeat the unresolved action or use another desktop-control path in this turn"
	}
	result := agent.ToolResult{
		Content: "computer_use_result: unverified\n" +
			"reason: " + failureCode + "\n" +
			"action_effect: " + actionEffect + "\n" +
			"message: " + message + "\n" +
			"recovery: " + recovery + "\n" +
			"detail: " + detail,
		GUIOutcome: &agent.GUIActionOutcome{
			Result:      agent.GUIActionResultCompletedUnverified,
			Phase:       agent.GUIActionPhaseVerifying,
			FailureCode: failureCode,
		},
		ComputerUseOutcome: &agent.ComputerUseTaskOutcome{
			Status: agent.ComputerUseTaskUnverified,
			Effect: effect,
		},
	}
	return result
}

func withOpenAIComputerTaskOutcomeV1(
	result agent.ToolResult,
	status agent.ComputerUseTaskStatus,
	effect agent.ComputerUseCommitEffect,
) agent.ToolResult {
	if effect == "" {
		effect = agent.ComputerUseCommitNone
	}
	result.ComputerUseOutcome = &agent.ComputerUseTaskOutcome{
		Status: status,
		Effect: effect,
	}
	return result
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
	// Tests may shorten this private first-response window. Production keeps
	// exactly one retry so a provider-side stall cannot consume the whole task.
	initialResponseTimeout  time.Duration
	initialResponseAttempts int
	// Tests replace this seam so initial and final observation retries never sleep.
	// The argument is the one-based failed attempt that precedes the wait.
	observationRetry func(context.Context, int) error
	permissions      *permissions.PermissionsConfig
	auditor          *audit.AuditLogger
	hookRunner       *hooks.HookRunner
}

const (
	defaultOpenAIComputerTaskTimeoutV1             = 2 * time.Minute
	defaultOpenAIComputerInitialResponseTimeoutV1  = 20 * time.Second
	defaultOpenAIComputerInitialResponseAttemptsV1 = 2
	maxOpenAIComputerInitialObservationsV1         = 5
	// Final observations receive the same bounded settle window as initial
	// cold-app capture. These retries repeat only screenshot observation,
	// never a committed provider action.
	maxOpenAIComputerFinalObservationsV1      = maxOpenAIComputerInitialObservationsV1
	maxOpenAIComputerObservationDetailRunesV1 = 500
)

var openAIComputerObservationRetryDelaysV1 = [...]time.Duration{
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
	1200 * time.Millisecond,
}

func waitOpenAIComputerObservationRetryV1(
	ctx context.Context,
	failedAttempt int,
) error {
	index := failedAttempt - 1
	if index < 0 || index >= len(openAIComputerObservationRetryDelaysV1) {
		return nil
	}
	timer := time.NewTimer(openAIComputerObservationRetryDelaysV1[index])
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

// openAIComputerObservationResultDetailV1 returns only the capture failure envelope,
// never the successful AX state that preceded it. A get_app_state result may
// contain refs, values, and state IDs before its screenshot_warning; those are
// private executor authority and must not escape into the parent transcript.
func openAIComputerObservationResultDetailV1(result agent.ToolResult) string {
	if result.GUIObservation != nil &&
		!result.GUIObservation.CoordinateActionable &&
		len(result.Images) == 1 {
		return "the exact target window image was visual-only and could not safely seed desktop actions"
	}
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

func retryOpenAIComputerObservationV1(
	result agent.ToolResult,
	err error,
) bool {
	if err != nil {
		return false
	}
	if result.IsRetryable {
		return true
	}
	if result.GUIObservation != nil &&
		!result.GUIObservation.CoordinateActionable {
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
		"image_dimensions_mismatch",
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

type openAIComputerObservationAttemptV1 func(
	context.Context,
	int,
) (agent.ToolResult, error)

type openAIComputerObservationRecorderV1 func(
	int,
	agent.ToolResult,
	error,
	time.Duration,
)

// runOpenAIComputerObservationV1 is the single retry owner for initial and
// post-batch/final screenshots. Each retry invokes only the observation
// closure; it has no action payload and therefore cannot replay input.
func runOpenAIComputerObservationV1(
	ctx context.Context,
	maxAttempts int,
	requireCoordinateActionable bool,
	retryWait func(context.Context, int) error,
	attempt openAIComputerObservationAttemptV1,
	record openAIComputerObservationRecorderV1,
) (agent.ToolResult, error) {
	if maxAttempts <= 0 || attempt == nil {
		return agent.ToolResult{},
			fmt.Errorf("OpenAI computer observation runner is unavailable")
	}
	if retryWait == nil {
		retryWait = waitOpenAIComputerObservationRetryV1
	}
	var result agent.ToolResult
	var err error
	for attemptIndex := 1; attemptIndex <= maxAttempts; attemptIndex++ {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return result, ctxErr
		}
		started := time.Now()
		result, err = attempt(ctx, attemptIndex)
		if record != nil {
			record(attemptIndex, result, err, time.Since(started))
		}
		actionable := result.GUIObservation == nil ||
			result.GUIObservation.CoordinateActionable
		if err == nil && !result.IsError && len(result.Images) == 1 &&
			(!requireCoordinateActionable || actionable) {
			return result, nil
		}
		if attemptIndex == maxAttempts ||
			!retryOpenAIComputerObservationV1(result, err) {
			return result, err
		}
		if waitErr := retryWait(ctx, attemptIndex); waitErr != nil {
			return result, waitErr
		}
	}
	return result, err
}

func (t *openAIComputerTaskToolV1) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "computer_use",
		Description: "Operate native macOS desktop apps to complete one full user goal. " +
			"Give the complete task once; the computer executor launches/focuses apps, " +
			"observes the current UI, performs the needed actions, and verifies the result internally. " +
			"Do not split clicks, typing, screenshots, or app switches into separate calls. " +
			"Do not omit a later step or app from the user's request. " +
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

	var initial agent.ToolResult
	var observationErr error
	observationDetail := ""
	var initialApp *tools.OpenAIComputerTaskAppV1
	if len(apps) > 0 {
		initialApp = &apps[0]
	}
	retryObservation := t.observationRetry
	if retryObservation == nil {
		retryObservation = waitOpenAIComputerObservationRetryV1
	}
	initial, observationErr = runOpenAIComputerObservationV1(
		ctx,
		maxOpenAIComputerInitialObservationsV1,
		true,
		retryObservation,
		func(
			attemptCtx context.Context,
			attempt int,
		) (agent.ToolResult, error) {
			plan, planErr := t.runtime.PlanOpenAIComputerTaskInitialObservationV1(
				initialApp,
				"Capture the initial desktop task state",
				true,
			)
			if planErr != nil {
				return agent.ToolResult{}, planErr
			}
			invocationCtx := tools.ContextWithOpenAINativeComputerActionV1(
				agent.ContextWithToolInvocation(
					attemptCtx,
					agent.ToolInvocation{
						ToolName: "computer_use",
						ToolUseID: fmt.Sprintf(
							"computer-task/initial-observation/%d",
							attempt,
						),
					},
				),
			)
			return t.workflow.runTool(invocationCtx, plan.Tool, plan.Args)
		},
		func(
			attempt int,
			result agent.ToolResult,
			attemptErr error,
			duration time.Duration,
		) {
			event := openAIComputerTraceEventV1{
				Phase:      "initial_observation",
				Status:     openAIComputerTraceStatusV1(result, attemptErr),
				Attempt:    attempt,
				DurationMS: duration.Milliseconds(),
			}
			if attemptErr == nil && !result.IsError &&
				len(result.Images) == 1 {
				observationDetail = ""
			} else {
				event.Status = "failed"
				event.FailureCode = openAIComputerTraceFailureCodeV1(
					result,
					attemptErr,
				)
				if event.FailureCode == "" {
					event.FailureCode = "initial_image_unavailable"
				}
				if attemptErr != nil {
					observationDetail = boundOpenAIComputerObservationDetailV1(
						attemptErr.Error(),
					)
				} else {
					observationDetail =
						openAIComputerObservationResultDetailV1(result)
				}
			}
			if initialApp != nil {
				event.AppBundleID = initialApp.BundleID
			}
			trace.record(openAIComputerTraceWithCaptureDiagnosticsV1(
				event,
				result,
			))
		},
	)
	if observationErr != nil || initial.IsError || len(initial.Images) != 1 {
		detail := observationDetail
		if detail == "" && observationErr != nil {
			detail = boundOpenAIComputerObservationDetailV1(
				observationErr.Error(),
			)
		}
		if detail == "" {
			detail = "the desktop observation backend returned an error"
		}
		failureCode := openAIComputerTraceFailureCodeV1(
			initial,
			observationErr,
		)
		if len(apps) == 0 && failureCode == "app_policy_blocked" {
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "failed",
				FailureCode: "initial_target_required",
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			result := withOpenAIComputerTaskOutcomeV1(agent.BusinessError(
				"computer_use_error: initial_target_required\n"+
					"message: Computer Use cannot infer a safe initial target while the protected Kocoro Desktop window is frontmost\n"+
					"recovery: retry computer_use once in this turn with the relevant app names in apps; do not switch to another desktop-control tool\n"+
					"detail: no desktop action was attempted",
			), agent.ComputerUseTaskNotCompleted, agent.ComputerUseCommitNone)
			result.ComputerUseOutcome.FailureCode = "initial_target_required"
			result.ComputerUseOutcome.Recovery =
				agent.ComputerUseRecoveryRetryWithApps
			return result, nil
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
	runner.observationRetry = retryObservation
	privateGateway := newOpenAIComputerInitialResponseClientV1(
		t.gateway,
		t.initialResponseTimeout,
		t.initialResponseAttempts,
	)
	child := agent.NewAgentLoop(
		privateGateway,
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
			"When the child input is JSON, original_user_request is authoritative: complete every desktop step it requests, including later steps in other apps. " +
			"parent_desktop_plan is only a planning hint and may accidentally omit a requested step; never treat that omission as permission to stop early. " +
			"The initial image is the current verified app window. " +
			"After every action batch, derive the next action only from the latest returned screenshot; never reuse coordinates from an older app state. " +
			"Use semantic visible targets when they are clear, and use coordinates only against that latest screenshot. " +
			"Before every new mutating action, compare the latest screenshot with every observable requirement in original_user_request. " +
			"If all requested end states are already visible, your next response must be the completed JSON object, not another computer call. " +
			"Never move or drag merely to park the cursor, rearrange windows, clean up the screen, or prepare the final response. " +
			"For inspection or summarization, read from screenshots and use scroll only when more content must be revealed; do not drag-select text unless the user explicitly requested selection or dragging. " +
			"Do not add routine fixed waits: Kocoro already settles and re-observes after actions; use wait only while the latest UI visibly shows loading or an in-progress transition. " +
			"Continue across app switches, " +
			"and stop as soon as the requested end state is verified. " +
			"Keyboard actions are bound to the latest verified target app, window, and focus; re-observe whenever that target is uncertain. " +
			"For browser navigation, use Command-L, type the exact URL, then Return while the latest observation still verifies the same browser window. " +
			"If keyboard focus is unavailable or Return is rejected, do not repeat it; use the latest screenshot to click the exact visible Go, URL suggestion, or navigation target instead. " +
			"Click the intended visible button for harmless dialogs. " +
			"For send, delete, or purchase actions, click the exact visible action button and wait for Kocoro's one local confirmation; " +
			"never use Return, Enter, Space, or another keyboard shortcut to submit or bypass that exact consequential-action confirmation because those activation paths are rejected locally. " +
			"Do not ask the parent to perform clicks, typing, screenshots, or state management. " +
			"Your final response is a machine-readable result: return exactly one compact JSON object " +
			`{"status":"completed","summary":"brief visible result"} only when the latest screenshot visibly proves the requested end state. ` +
			`Return exactly {"status":"not_completed","summary":"brief visible result"} when the latest screenshot proves the requested end state was not reached. ` +
			`Return exactly {"status":"unverified","summary":"brief reason"} when the available observation cannot prove either outcome. ` +
			"Do not use Markdown, add fields, or put any text outside that JSON object. " +
			"If an action may have committed but the latest screenshot does not prove the result, use unverified.",
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
	reply, _, err := child.Run(
		taskCtx,
		openAIComputerChildGoalInputV1(ctx, args.Task),
		content,
		nil,
	)
	stats := runner.BatchStatsV1()
	providerEvent := openAIComputerTraceEventV1{
		Phase:      "private_executor",
		Status:     "completed",
		DurationMS: time.Since(providerStarted).Milliseconds(),
	}
	if err != nil {
		var initialUnavailable *openAIComputerInitialResponseUnavailableV1
		if stats.LastFinalObservationUnavailable {
			providerEvent.Status = "completed_unverified"
			providerEvent.FailureCode = "final_observation_unavailable"
		} else if errors.As(err, &initialUnavailable) {
			providerEvent.Status = "failed"
			providerEvent.FailureCode = "initial_response_unavailable"
		} else {
			providerEvent.Status = "failed"
			providerEvent.FailureCode = openAIComputerTraceFailureCodeV1(
				agent.ToolResult{},
				err,
			)
		}
	}
	trace.record(providerEvent)
	if stats.TaskEffect == agent.ComputerUseCommitUnknown {
		failureCode := stats.LastFailureCode
		if failureCode == "" {
			failureCode = "commit_unknown"
		}
		detail := stats.LastFailureDetail
		if err != nil {
			detail = err.Error()
		}
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "completed_unverified",
			FailureCode: failureCode,
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return openAIComputerTaskUnverifiedResultV1(
			failureCode,
			detail,
			agent.ComputerUseCommitUnknown,
		), nil
	}
	if err != nil {
		var initialUnavailable *openAIComputerInitialResponseUnavailableV1
		if errors.As(err, &initialUnavailable) {
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "failed",
				FailureCode: "executor_timeout_before_action",
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			return agent.BusinessError(
				"computer_use_error: executor_timeout_before_action\n" +
					"message: the private OpenAI Computer Use executor did not return an initial action plan within its bounded response window\n" +
					"recovery: no desktop action was attempted; another observation or control path may still be used if appropriate\n" +
					"detail: " + initialUnavailable.Error(),
			), nil
		}
		// A later provider/protocol/observation failure cannot erase already
		// acknowledged desktop effects from earlier batches. Preserve the task
		// as structured unverified so the parent neither claims that nothing ran
		// nor blindly repeats work that is known to have committed.
		if stats.TaskEffect == agent.ComputerUseCommitKnown {
			failureCode := "executor_failed_after_commit"
			detail := err.Error()
			switch {
			case stats.LastFinalObservationUnavailable:
				failureCode = "final_observation_unavailable"
				if strings.TrimSpace(stats.LastFailureDetail) != "" {
					detail = stats.LastFailureDetail
				}
			case errors.Is(err, context.DeadlineExceeded) ||
				errors.Is(taskCtx.Err(), context.DeadlineExceeded):
				failureCode = "executor_timeout_after_commit"
			case stats.LastGUIResult == agent.GUIActionResultCancelled:
				failureCode = "cancelled"
			case stats.LastGUIResult == agent.GUIActionResultUserInterference:
				failureCode = "user_interference"
			case stats.LastBatchHadFreshObservation:
				failureCode = "outcome_unverified"
			}
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "completed_unverified",
				FailureCode: failureCode,
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			return openAIComputerTaskUnverifiedResultV1(
				failureCode,
				detail,
				agent.ComputerUseCommitKnown,
			), nil
		}
		if errors.Is(err, context.DeadlineExceeded) ||
			errors.Is(taskCtx.Err(), context.DeadlineExceeded) {
			failureCode := "executor_timeout"
			recovery := "do not retry computer_use or attempt alternate desktop-control tools in this turn; report that the desktop task timed out"
			if stats.Batches == 0 {
				failureCode = "executor_timeout_before_action"
				recovery = "no desktop action was attempted; another observation or control path may still be used if appropriate"
			}
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "failed",
				FailureCode: failureCode,
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			return agent.BusinessError(
				"computer_use_error: " + failureCode + "\n" +
					"message: the private OpenAI Computer Use executor exceeded its interactive task deadline\n" +
					"recovery: " + recovery + "\n" +
					"detail: the private executor exceeded " + taskTimeout.String(),
			), nil
		}
		if stats.LastFinalObservationUnavailable {
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "completed_unverified",
				FailureCode: "final_observation_unavailable",
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			return openAIComputerTaskUnverifiedResultV1(
				"final_observation_unavailable",
				stats.LastFailureDetail,
				stats.TaskEffect,
			), nil
		}
		if stats.LastGUIResult == agent.GUIActionResultCancelled ||
			stats.LastGUIResult == agent.GUIActionResultUserInterference {
			failureCode := stats.LastFailureCode
			if stats.LastGUIResult == agent.GUIActionResultCancelled {
				failureCode = "cancelled"
			} else if stats.LastGUIResult ==
				agent.GUIActionResultUserInterference {
				failureCode = "user_interference"
			}
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "failed",
				FailureCode: failureCode,
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			return openAIComputerTaskUnverifiedResultV1(
				failureCode,
				err.Error(),
				agent.ComputerUseCommitUnknown,
			), nil
		}
		if stats.LastBatchHadFreshObservation {
			trace.record(openAIComputerTraceEventV1{
				Phase:       "task",
				Status:      "completed_unverified",
				FailureCode: "outcome_unverified",
				DurationMS:  time.Since(taskStarted).Milliseconds(),
			})
			return openAIComputerTaskUnverifiedResultV1(
				"outcome_unverified",
				err.Error(),
				stats.TaskEffect,
			), nil
		}
		failureCode := "executor_failed"
		recovery := "do not retry computer_use or attempt alternate desktop-control tools in this turn; report the executor failure without guessing that the target app is missing or blocked"
		if stats.Batches == 0 {
			failureCode = "executor_failed_before_action"
			recovery = "no desktop action was attempted; another observation or control path may still be used if appropriate"
		} else if stats.TaskEffect == agent.ComputerUseCommitNone {
			failureCode = "executor_failed_without_mutation"
			recovery = "no desktop mutation committed; another observation or control path may still be used if appropriate"
		}
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "failed",
			FailureCode: failureCode,
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return agent.BusinessError(
			"computer_use_error: " + failureCode + "\n" +
				"message: the private OpenAI Computer Use executor could not complete the task\n" +
				"recovery: " + recovery + "\n" +
				"detail: " + err.Error(),
		), nil
	}
	reply = strings.TrimSpace(reply)
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
		return withOpenAIComputerTaskOutcomeV1(agent.BusinessError(
			"computer_use_error: no_desktop_action\n"+
				"message: the private OpenAI Computer Use executor finished without issuing any native computer batch, so the task was not done\n"+
				"recovery: do not retry computer_use again in this turn; report that the native executor performed no action\n"+
				"detail: "+detail,
		), agent.ComputerUseTaskNotCompleted, agent.ComputerUseCommitNone), nil
	}
	outcome, outcomeErr := parseOpenAIComputerTaskOutcomeV1(reply)
	if outcomeErr != nil {
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "failed",
			FailureCode: "outcome_unverified",
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return withOpenAIComputerTaskOutcomeV1(agent.BusinessError(
			"computer_use_error: outcome_unverified\n"+
				"message: the private OpenAI Computer Use executor did not return a verifiable task outcome\n"+
				"recovery: do not retry computer_use again in this turn; report that the executor outcome could not be verified\n"+
				"detail: "+outcomeErr.Error(),
		), agent.ComputerUseTaskUnverified, stats.TaskEffect), nil
	}
	if outcome.Status == openAIComputerTaskNotCompletedV1 {
		detail := outcome.Summary
		if stats.LastFailureDetail != "" {
			detail += "; last executor failure: " + stats.LastFailureDetail
		}
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "failed",
			FailureCode: "task_not_completed",
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return withOpenAIComputerTaskOutcomeV1(agent.BusinessError(
			"computer_use_error: task_not_completed\n"+
				"message: the private OpenAI Computer Use executor reported that the requested visible end state was not reached\n"+
				"recovery: do not retry computer_use again in this turn; report the executor's visible-state result\n"+
				"detail: "+detail,
		), agent.ComputerUseTaskNotCompleted, stats.TaskEffect), nil
	}
	if outcome.Status == openAIComputerTaskUnverifiedV1 {
		trace.record(openAIComputerTraceEventV1{
			Phase:       "task",
			Status:      "completed_unverified",
			FailureCode: "goal_unverified",
			DurationMS:  time.Since(taskStarted).Milliseconds(),
		})
		return openAIComputerTaskUnverifiedResultV1(
			"goal_unverified",
			outcome.Summary,
			stats.TaskEffect,
		), nil
	}
	trace.record(openAIComputerTraceEventV1{
		Phase:      "task",
		Status:     "completed",
		DurationMS: time.Since(taskStarted).Milliseconds(),
	})
	return withOpenAIComputerTaskOutcomeV1(
		agent.ToolResult{Content: outcome.Summary},
		agent.ComputerUseTaskCompleted,
		stats.TaskEffect,
	), nil
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
