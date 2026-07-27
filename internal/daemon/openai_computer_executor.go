package daemon

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

type openAIComputerActionRuntimeV1 interface {
	PlanOpenAIComputerActionV1(
		context.Context,
		tools.OpenAIComputerActionV1,
	) (tools.OpenAIComputerActionPlanV1, error)
	PlanOpenAIComputerObservationV1(
		string,
		bool,
	) (tools.OpenAIComputerActionPlanV1, error)
	AuthorizeOpenAIComputerTypeAfterKeypressV1(
		tools.OpenAIComputerActionV1,
	) error
}

type daemonOpenAIComputerPrivateRuntimeV1 struct {
	runtime *tools.OpenAIComputerActionRuntimeV1
}

// daemonOpenAIComputerBatchRunnerV1 is the daemon-owned AgentLoop callback.
// It mints a fresh response-bound executor for every provider computer_call,
// while daemonGUIWorkflow retains the single turn lease across Responses
// continuations.
type daemonOpenAIComputerBatchRunnerV1 struct {
	workflow         *daemonGUIWorkflow
	runtime          openAIComputerActionRuntimeV1
	trace            *openAIComputerTraceV1
	observationRetry func(context.Context, int) error

	statsMu       sync.Mutex
	stats         openAIComputerBatchStatsV1
	batchSequence int
}

// openAIComputerBatchStatsV1 records execution evidence for diagnostics. It
// deliberately does not decide whether the user's goal completed: only the
// private model can make that judgment from the latest screenshot, and it
// reports the judgment through openAIComputerTaskOutcomeV1.
type openAIComputerBatchStatsV1 struct {
	Batches                         int
	LastFailureDetail               string
	LastFailureCode                 string
	LastGUIResult                   agent.GUIActionResult
	LastFinalObservationUnavailable bool
	TaskEffect                      agent.ComputerUseCommitEffect
	LastBatchEffect                 agent.ComputerUseCommitEffect
	LastBatchHadFreshObservation    bool
}

func (r *daemonOpenAIComputerBatchRunnerV1) recordBatchV1(
	execution agent.OpenAIComputerBatchExecution,
	err error,
) {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	r.stats.Batches++
	r.stats.LastFailureCode =
		openAIComputerTraceFailureCodeV1(execution.Result, err)
	r.stats.LastGUIResult = ""
	if execution.Result.GUIOutcome != nil {
		r.stats.LastGUIResult = execution.Result.GUIOutcome.Result
	}
	r.stats.LastFinalObservationUnavailable = false
	batchEffect := execution.ActionEffect
	if batchEffect == "" {
		batchEffect = agent.ComputerUseCommitNone
	}
	r.stats.LastBatchEffect = batchEffect
	r.stats.TaskEffect = agent.MergeComputerUseCommitEffect(
		r.stats.TaskEffect,
		batchEffect,
	)
	r.stats.LastBatchHadFreshObservation =
		execution.ContinuationAllowed &&
			len(execution.Result.Images) == 1
	if execution.Result.GUIOutcome != nil &&
		execution.Result.GUIOutcome.FailureCode ==
			"final_observation_unavailable" {
		r.stats.LastFinalObservationUnavailable = true
	}
	if err == nil && !execution.Result.IsError {
		return
	}
	if err != nil {
		r.stats.LastFailureDetail = err.Error()
		return
	}
	r.stats.LastFailureDetail = strings.TrimSpace(execution.Result.Content)
}

func (r *daemonOpenAIComputerBatchRunnerV1) BatchStatsV1() openAIComputerBatchStatsV1 {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	return r.stats
}

func (r *daemonOpenAIComputerBatchRunnerV1) nextBatchIndexV1() int {
	r.statsMu.Lock()
	defer r.statsMu.Unlock()
	r.batchSequence++
	return r.batchSequence
}

func newDaemonOpenAIComputerBatchRunnerV1(
	workflow *daemonGUIWorkflow,
	runtime openAIComputerActionRuntimeV1,
) (*daemonOpenAIComputerBatchRunnerV1, error) {
	if workflow == nil || workflow.coordinator == nil {
		return nil, fmt.Errorf("OpenAI computer daemon workflow is unavailable")
	}
	if runtime == nil {
		return nil, fmt.Errorf("OpenAI computer action runtime is unavailable")
	}
	return &daemonOpenAIComputerBatchRunnerV1{
		workflow: workflow,
		runtime:  runtime,
	}, nil
}

// detachDaemonOpenAIComputerPrivateRuntimeV1 runs before every named-agent or
// MCP filter. It captures the guarded clone-local computer_use core, removes
// that private function identity from the provider-facing registry, and keeps
// the detached approval identity beside the action runtime.
func detachDaemonOpenAIComputerPrivateRuntimeV1(
	registry *agent.ToolRegistry,
	profile *client.ExecutionProfile,
) (*daemonOpenAIComputerPrivateRuntimeV1, error) {
	if profile == nil ||
		profile.Provider() != client.OpenAIComputerProvider ||
		profile.APISurface() != client.APISurfaceOpenAIResponses ||
		profile.ExecutionMode() != client.ExecutionModeNativeComputer ||
		profile.ToolContract() != client.ToolContractOpenAIComputerV1 {
		return nil, nil
	}
	if !profile.IsTrustedResolution() ||
		!profile.SupportsImageInput() ||
		!profile.SupportsToolResultImages() ||
		profile.SupportsFunctionTools() ||
		!profile.SupportsBatchedActions() {
		return nil, fmt.Errorf("OpenAI computer run profile is unsupported")
	}
	if !tools.OpenAINativeComputerBatchExecutionAvailable() {
		return nil, nil
	}
	return detachDaemonOpenAIComputerPrivateRuntimeCoreV1(registry)
}

// detachDaemonOpenAIComputerPrivateRuntimeCoreV1 prepares the local executor
// without resolving or pinning a provider model. The high-level dispatcher
// resolves its OpenAI profile lazily only when the model actually delegates a
// desktop task, so ordinary turns never pay Computer Use preparation latency.
func detachDaemonOpenAIComputerPrivateRuntimeCoreV1(
	registry *agent.ToolRegistry,
) (*daemonOpenAIComputerPrivateRuntimeV1, error) {
	if !tools.OpenAINativeComputerBatchExecutionAvailable() {
		return nil, nil
	}
	if registry == nil {
		return nil, fmt.Errorf("OpenAI computer run registry is unavailable")
	}
	core, ok := registry.Get("computer_use")
	if !ok {
		return nil, fmt.Errorf("OpenAI computer runtime requires computer_use")
	}
	runtime, err := tools.NewOpenAIComputerActionRuntimeV1(core)
	if err != nil {
		return nil, err
	}
	registry.Remove("computer_use")
	return &daemonOpenAIComputerPrivateRuntimeV1{
		runtime: runtime,
	}, nil
}

// retainDaemonOpenAIComputerPrivateRuntimeV1 reconciles the detached bundle
// with the final public filter result. Removing the native marker disables the
// profile and destroys the private runner; asking for computer_use alone can
// never reintroduce the removed function identity.
func retainDaemonOpenAIComputerPrivateRuntimeV1(
	registry *agent.ToolRegistry,
	profile *client.ExecutionProfile,
	private *daemonOpenAIComputerPrivateRuntimeV1,
) (*client.ExecutionProfile, *daemonOpenAIComputerPrivateRuntimeV1) {
	if private == nil {
		return profile, nil
	}
	if registry == nil || registry.Has("computer_use") ||
		!registry.Has(client.NativeComputerToolName) ||
		profile == nil ||
		profile.Provider() != client.OpenAIComputerProvider ||
		profile.ExecutionMode() != client.ExecutionModeNativeComputer ||
		profile.ToolContract() != client.ToolContractOpenAIComputerV1 {
		return nil, nil
	}
	return profile, private
}

func (r *daemonOpenAIComputerBatchRunnerV1) ExecuteOpenAIComputerBatch(
	ctx context.Context,
	profile *client.ExecutionProfile,
	responseID string,
	payload json.RawMessage,
	safetyAcknowledgement *agent.OpenAIComputerSafetyAcknowledgement,
) (execution agent.OpenAIComputerBatchExecution, err error) {
	if r == nil || r.workflow == nil || r.runtime == nil ||
		safetyAcknowledgement == nil {
		return agent.OpenAIComputerBatchExecution{},
			fmt.Errorf("OpenAI computer daemon batch runner is unavailable")
	}
	batchIndex := r.nextBatchIndexV1()
	started := time.Now()
	defer func() {
		r.recordBatchV1(execution, err)
		status := openAIComputerTraceStatusV1(execution.Result, err)
		failureCode := openAIComputerTraceFailureCodeV1(execution.Result, err)
		if err == nil && !execution.Result.IsError &&
			len(execution.Result.Images) != 1 {
			status = "failed"
			if failureCode == "" {
				failureCode = "final_image_unavailable"
			}
		}
		r.trace.record(openAIComputerTraceEventV1{
			Phase:       "batch",
			Status:      status,
			BatchIndex:  batchIndex,
			FailureCode: failureCode,
			DurationMS:  time.Since(started).Milliseconds(),
		})
	}()
	var normalizedCall client.OpenAIComputerCall
	if err := json.Unmarshal(payload, &normalizedCall); err != nil ||
		normalizedCall.ResponseID != responseID {
		return agent.OpenAIComputerBatchExecution{},
			fmt.Errorf("OpenAI computer daemon batch safety provenance is invalid")
	}
	if !safetyAcknowledgement.ConsumeForExecution(
		profile,
		responseID,
		normalizedCall,
	) {
		return agent.OpenAIComputerBatchExecution{},
			fmt.Errorf("OpenAI computer daemon batch safety acknowledgement is invalid")
	}
	provenance, err := tools.NewOpenAIComputerExecutionProvenanceV1(
		profile,
		responseID,
	)
	if err != nil {
		return agent.OpenAIComputerBatchExecution{},
			fmt.Errorf("OpenAI computer daemon batch provenance is invalid")
	}
	executor, err := newDaemonOpenAIComputerExecutorV1(
		r.workflow,
		r.runtime,
		provenance,
	)
	if err != nil {
		return agent.OpenAIComputerBatchExecution{}, err
	}
	executor.trace = r.trace
	executor.batchIndex = batchIndex
	executor.observationRetry = r.observationRetry
	defer executor.EndBatchV1()

	result, executeErr := tools.NewOpenAIComputerAdapterV1(executor).
		ExecuteBatchV1(ctx, payload)
	return agent.OpenAIComputerBatchExecution{
		CallID: result.CallID,
		ContinuationAllowed: executeErr == nil &&
			len(result.ToolResult.Images) == 1 &&
			result.ActionEffect != agent.ComputerUseCommitUnknown,
		ActionEffect: result.ActionEffect,
		Result:       result.ToolResult,
	}, executeErr
}

// daemonOpenAIComputerExecutorV1 binds one trusted provider response to one
// daemon GUI workflow. It is never registered as a public function tool; the
// execution path uses the same lease, Pause/Take Over/Stop, app-policy,
// risk-broker, and final capability checks as computer_use.
type daemonOpenAIComputerExecutorV1 struct {
	// operationMu serializes the whole provider action/final-observation
	// boundary. Coordinator Stop/Pause never takes this lock and can still
	// cancel an in-flight action immediately.
	operationMu sync.Mutex
	mu          sync.Mutex

	workflow         *daemonGUIWorkflow
	runtime          openAIComputerActionRuntimeV1
	provenance       tools.OpenAIComputerExecutionProvenanceV1
	trace            *openAIComputerTraceV1
	batchIndex       int
	observationRetry func(context.Context, int) error

	authority       tools.OpenAIComputerBatchAuthorityV1
	call            *tools.OpenAIComputerCallV1
	nextActionIndex int
	finalCaptures   int
	closed          bool
}

func newDaemonOpenAIComputerExecutorV1(
	workflow *daemonGUIWorkflow,
	runtime openAIComputerActionRuntimeV1,
	provenance tools.OpenAIComputerExecutionProvenanceV1,
) (*daemonOpenAIComputerExecutorV1, error) {
	if workflow == nil || workflow.coordinator == nil {
		return nil, fmt.Errorf("OpenAI computer daemon workflow is unavailable")
	}
	if runtime == nil {
		return nil, fmt.Errorf("OpenAI computer action runtime is unavailable")
	}
	if !provenance.IsTrusted() {
		return nil, fmt.Errorf("OpenAI computer execution provenance is untrusted")
	}
	return &daemonOpenAIComputerExecutorV1{
		workflow: workflow, runtime: runtime,
		provenance: provenance,
	}, nil
}

func (e *daemonOpenAIComputerExecutorV1) AcquireOpenAIComputerBatchAuthorityV1(
	ctx context.Context,
	call tools.OpenAIComputerCallV1,
) (tools.OpenAIComputerBatchAuthorityV1, error) {
	if e == nil {
		return tools.OpenAIComputerBatchAuthorityV1{},
			fmt.Errorf("OpenAI computer daemon executor is unavailable")
	}
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	if err := tools.ValidateOpenAIComputerCallV1(call); err != nil {
		return tools.OpenAIComputerBatchAuthorityV1{},
			fmt.Errorf("OpenAI computer call is invalid")
	}
	e.mu.Lock()
	if e.closed || e.call != nil || !e.provenance.IsTrusted() {
		e.mu.Unlock()
		return tools.OpenAIComputerBatchAuthorityV1{},
			fmt.Errorf("OpenAI computer batch authority is unavailable")
	}
	e.mu.Unlock()
	if call.ResponseID != e.provenance.ResponseID() {
		return tools.OpenAIComputerBatchAuthorityV1{},
			fmt.Errorf("OpenAI computer response provenance does not match")
	}

	// The task entry point already resolved every declared app, checked policy,
	// and opened one turn lease before the initial screenshot.
	lease, ok := e.workflow.currentLease()
	if !ok {
		return tools.OpenAIComputerBatchAuthorityV1{},
			fmt.Errorf("OpenAI computer batch lease is unavailable")
	}
	authority := tools.OpenAIComputerBatchAuthorityV1{
		LeaseID:      lease.LeaseID,
		ResponseID:   call.ResponseID,
		CallID:       call.CallID,
		Provider:     call.Provider,
		APISurface:   call.APISurface,
		ToolContract: call.ToolContract,
	}

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || e.call != nil {
		return tools.OpenAIComputerBatchAuthorityV1{},
			fmt.Errorf("OpenAI computer batch authority raced another call")
	}
	callCopy := cloneOpenAIComputerCallV1(call)
	e.call = &callCopy
	e.authority = authority
	e.nextActionIndex = 0
	return authority, nil
}

func cloneOpenAIComputerCallV1(
	call tools.OpenAIComputerCallV1,
) tools.OpenAIComputerCallV1 {
	cloned := call
	cloned.PendingSafetyChecks = client.CloneOpenAIComputerSafetyChecks(
		call.PendingSafetyChecks,
	)
	cloned.Actions = make([]tools.OpenAIComputerActionV1, len(call.Actions))
	cloneInt := func(value *int) *int {
		if value == nil {
			return nil
		}
		exact := *value
		return &exact
	}
	for index, action := range call.Actions {
		copy := action
		copy.X = cloneInt(action.X)
		copy.Y = cloneInt(action.Y)
		copy.ScrollX = cloneInt(action.ScrollX)
		copy.ScrollY = cloneInt(action.ScrollY)
		if action.Keys != nil {
			copy.Keys = append([]string{}, action.Keys...)
		}
		copy.Path = append([]tools.OpenAIComputerPointV1(nil), action.Path...)
		cloned.Actions[index] = copy
	}
	return cloned
}

func (e *daemonOpenAIComputerExecutorV1) ExecuteAuthorizedOpenAIComputerActionV1(
	ctx context.Context,
	authority tools.OpenAIComputerBatchAuthorityV1,
	scope tools.OpenAIComputerActionScopeV1,
	action tools.OpenAIComputerActionV1,
) (execution tools.OpenAIComputerActionExecutionV1, err error) {
	started := time.Now()
	var trace *openAIComputerTraceV1
	batchIndex := 0
	if e != nil {
		trace = e.trace
		batchIndex = e.batchIndex
	}
	defer func() {
		trace.record(openAIComputerTraceWithCaptureDiagnosticsV1(openAIComputerTraceEventV1{
			Phase:       "action",
			Status:      openAIComputerTraceStatusV1(execution.Result, err),
			BatchIndex:  batchIndex,
			ActionIndex: scope.ActionIndex + 1,
			ActionCount: scope.ActionCount,
			ActionType:  action.Type,
			CommitState: string(execution.CommitState),
			FailureCode: openAIComputerTraceFailureCodeV1(
				execution.Result,
				err,
			),
			DurationMS: time.Since(started).Milliseconds(),
		}, execution.Result))
	}()
	if e == nil {
		return tools.OpenAIComputerActionExecutionV1{
			CommitState: tools.OpenAIComputerNotCommittedV1,
		}, fmt.Errorf("OpenAI computer executor is unavailable")
	}
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	if err := e.validateActionBoundary(authority, scope, action); err != nil {
		return tools.OpenAIComputerActionExecutionV1{
			CommitState: tools.OpenAIComputerNotCommittedV1,
		}, err
	}
	if err := ctx.Err(); err != nil {
		return tools.OpenAIComputerActionExecutionV1{
			CommitState: tools.OpenAIComputerNotCommittedV1,
		}, fmt.Errorf("OpenAI computer action cancelled before admission")
	}

	plan, err := e.runtime.PlanOpenAIComputerActionV1(ctx, action)
	if err != nil {
		failureCode := "action_projection_failed"
		detail := "the action planner rejected the provider action"
		var planErr *tools.OpenAIComputerActionPlanErrorV1
		if errors.As(err, &planErr) &&
			strings.TrimSpace(planErr.FailureCode) != "" {
			failureCode = strings.TrimSpace(planErr.FailureCode)
			detail = strings.TrimSpace(planErr.Detail)
		}
		result := agent.BusinessError(
			"computer_use_error: " + failureCode + "\n" +
				"message: the OpenAI computer action could not be safely projected\n" +
				"detail: " + detail,
		)
		result.GUIOutcome = &agent.GUIActionOutcome{
			Result:      agent.GUIActionResultFailed,
			Phase:       agent.GUIActionPhaseActing,
			FailureCode: failureCode,
		}
		return tools.OpenAIComputerActionExecutionV1{
				CommitState: tools.OpenAIComputerNotCommittedV1,
				Result:      result,
			},
			fmt.Errorf(
				"OpenAI computer action could not be safely projected (%s)",
				failureCode,
			)
	}
	if plan.Mutation != openAIComputerActionMutatesInDaemonV1(action) {
		return tools.OpenAIComputerActionExecutionV1{
			CommitState: tools.OpenAIComputerNotCommittedV1,
		}, fmt.Errorf("OpenAI computer action projection changed its effect")
	}
	nativeActionCtx := tools.ContextWithOpenAINativeComputerActionV1(ctx)
	actionToolUseID := openAIComputerActionToolUseIDV1(scope.ActionID)

	result, runErr := e.runPlanV1(nativeActionCtx, plan, actionToolUseID)
	execution = tools.OpenAIComputerActionExecutionV1{
		CommitState: tools.OpenAIComputerNotCommittedV1,
		Result:      result,
	}
	// Provider-visible batches receive no intermediate images, including error
	// paths. CaptureFinalOpenAIComputerObservationV1 is the sole image seam.
	execution.Result.Images = nil
	if plan.Mutation {
		execution.CommitState = openAIComputerCommitStateV1(result, runErr)
	}

	var targetRefreshErr error
	if runErr == nil &&
		!execution.Result.IsError &&
		(execution.CommitState == tools.OpenAIComputerCommitVerifiedV1 ||
			execution.CommitState == tools.OpenAIComputerCommitUnverifiedV1) &&
		openAIComputerActionNeedsFollowingTargetRefreshV1(action, scope) {
		refreshPlan, refreshPlanErr := e.runtime.PlanOpenAIComputerObservationV1(
			"Refresh the frontmost target after an ordered OpenAI computer keypress",
			false,
		)
		if refreshPlanErr != nil {
			failureCode, detail := openAIComputerTypedPlanFailureV1(
				refreshPlanErr,
				"target_refresh_projection_failed",
				"the post-keypress observation planner rejected the target refresh",
			)
			execution.Result, targetRefreshErr =
				openAIComputerCommittedTargetRefreshFailureV1(
					failureCode,
					"the keyboard action committed, but its next target could not be refreshed",
					detail,
				)
		} else {
			refreshResult, refreshRunErr := e.runPlanV1(
				nativeActionCtx,
				refreshPlan,
				actionToolUseID+"_target_refresh",
			)
			switch {
			case refreshRunErr != nil:
				failureCode := openAIComputerTraceFailureCodeV1(
					refreshResult,
					refreshRunErr,
				)
				if failureCode == "" || failureCode == "executor_error" {
					failureCode = "target_refresh_executor_failed"
				}
				detail := "the post-keypress target refresh executor failed"
				if errors.Is(refreshRunErr, context.Canceled) ||
					errors.Is(refreshRunErr, context.DeadlineExceeded) {
					failureCode = "target_refresh_cancelled"
					detail = "the post-keypress target refresh was cancelled"
				}
				execution.Result, targetRefreshErr =
					openAIComputerCommittedTargetRefreshFailureV1(
						failureCode,
						"the keyboard action committed, but its next target refresh failed",
						detail,
					)
			case refreshResult.IsError:
				failureCode := openAIComputerTraceFailureCodeV1(
					refreshResult,
					nil,
				)
				if failureCode == "" || failureCode == "tool_error" {
					failureCode = "target_refresh_failed"
				}
				execution.Result, targetRefreshErr =
					openAIComputerCommittedTargetRefreshFailureV1(
						failureCode,
						"the keyboard action committed, but its next target refresh was rejected",
						openAIComputerObservationResultDetailV1(refreshResult),
					)
			case len(refreshResult.Images) != 0:
				execution.Result, targetRefreshErr =
					openAIComputerCommittedTargetRefreshFailureV1(
						"target_refresh_contract_invalid",
						"the keyboard action committed, but its next target refresh violated the internal observation contract",
						"an image-free target refresh unexpectedly returned an image",
					)
			case e.nextOpenAIComputerActionIsTypeV1(scope):
				if authorizeErr := e.runtime.
					AuthorizeOpenAIComputerTypeAfterKeypressV1(action); authorizeErr != nil {
					failureCode, detail := openAIComputerTypedPlanFailureV1(
						authorizeErr,
						"keyboard_target_bind_failed",
						"the refreshed post-keypress target could not authorize text input",
					)
					execution.Result, targetRefreshErr =
						openAIComputerCommittedTargetRefreshFailureV1(
							failureCode,
							"the keyboard action committed, but text input could not be bound to its refreshed target",
							detail,
						)
				}
			}
		}
	}

	e.mu.Lock()
	e.nextActionIndex++
	e.mu.Unlock()
	if runErr != nil {
		return execution, fmt.Errorf("OpenAI computer action executor failed: %w", runErr)
	}
	if targetRefreshErr != nil {
		return execution, targetRefreshErr
	}
	if execution.Result.IsError {
		return execution, nil
	}
	return execution, nil
}

func openAIComputerTypedPlanFailureV1(
	err error,
	fallbackCode string,
	fallbackDetail string,
) (string, string) {
	failureCode := strings.TrimSpace(fallbackCode)
	detail := strings.TrimSpace(fallbackDetail)
	var planErr *tools.OpenAIComputerActionPlanErrorV1
	if errors.As(err, &planErr) {
		if typedCode := strings.TrimSpace(planErr.FailureCode); typedCode != "" {
			failureCode = typedCode
		}
		if typedDetail := strings.TrimSpace(planErr.Detail); typedDetail != "" {
			detail = typedDetail
		}
	}
	return failureCode, boundOpenAIComputerObservationDetailV1(detail)
}

func openAIComputerCommittedTargetRefreshFailureV1(
	failureCode string,
	message string,
	detail string,
) (agent.ToolResult, error) {
	failureCode = strings.TrimSpace(failureCode)
	if failureCode == "" {
		failureCode = "target_refresh_failed"
	}
	message = strings.Join(strings.Fields(message), " ")
	detail = strings.Join(strings.Fields(
		boundOpenAIComputerObservationDetailV1(detail),
	), " ")
	content := "computer_use_error: " + failureCode + "\n" +
		"message: " + message
	if detail != "" {
		content += "\ndetail: " + detail
	}
	result := agent.BusinessError(content)
	result.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultCompletedUnverified,
		Phase:       agent.GUIActionPhaseVerifying,
		FailureCode: failureCode,
	}
	return result, fmt.Errorf(
		"OpenAI computer post-keypress target refresh failed (%s)",
		failureCode,
	)
}

// openAIComputerActionToolUseIDV1 keeps provider call identity opaque while
// deriving a bounded daemon invocation ID that also satisfies the local
// consequential-risk request contract. Provider action IDs contain "/" and
// can exceed that contract's 128-byte limit, so they must not cross the
// confirmation seam verbatim.
func openAIComputerActionToolUseIDV1(actionID string) string {
	digest := sha256.Sum256([]byte(actionID))
	return fmt.Sprintf("openai_action_%x", digest[:])
}

func openAIComputerActionNeedsFollowingTargetRefreshV1(
	action tools.OpenAIComputerActionV1,
	scope tools.OpenAIComputerActionScopeV1,
) bool {
	return action.Type == tools.OpenAIComputerActionKeypressV1 &&
		scope.ActionIndex+1 < scope.ActionCount
}

func (e *daemonOpenAIComputerExecutorV1) nextOpenAIComputerActionIsTypeV1(
	scope tools.OpenAIComputerActionScopeV1,
) bool {
	if e == nil {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	next := scope.ActionIndex + 1
	return e.call != nil &&
		next >= 0 &&
		next < len(e.call.Actions) &&
		e.call.Actions[next].Type == tools.OpenAIComputerActionTypeTextV1
}

func (e *daemonOpenAIComputerExecutorV1) CaptureFinalOpenAIComputerObservationV1(
	ctx context.Context,
	authority tools.OpenAIComputerBatchAuthorityV1,
	call tools.OpenAIComputerCallV1,
) (result agent.ToolResult, err error) {
	started := time.Now()
	var trace *openAIComputerTraceV1
	batchIndex := 0
	attemptRecorded := false
	if e != nil {
		trace = e.trace
		batchIndex = e.batchIndex
	}
	defer func() {
		if attemptRecorded {
			return
		}
		failureCode := openAIComputerTraceFailureCodeV1(result, err)
		if failureCode == "" && err != nil {
			failureCode = "executor_error"
		}
		trace.record(openAIComputerTraceWithCaptureDiagnosticsV1(openAIComputerTraceEventV1{
			Phase:       "final_observation",
			Status:      openAIComputerTraceStatusV1(result, err),
			BatchIndex:  batchIndex,
			FailureCode: failureCode,
			DurationMS:  time.Since(started).Milliseconds(),
		}, result))
	}()
	if e == nil {
		return agent.ToolResult{},
			fmt.Errorf("OpenAI computer executor is unavailable")
	}
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	if err := e.validateFinalBoundary(authority, call); err != nil {
		return agent.ToolResult{}, err
	}

	result, err = runOpenAIComputerObservationV1(
		ctx,
		maxOpenAIComputerFinalObservationsV1,
		false,
		e.observationRetry,
		func(
			attemptCtx context.Context,
			attempt int,
		) (agent.ToolResult, error) {
			plan, planErr := e.runtime.PlanOpenAIComputerObservationV1(
				"Capture final OpenAI native computer screenshot",
				true,
			)
			if planErr != nil {
				return agent.ToolResult{},
					fmt.Errorf(
						"OpenAI computer final observation could not be safely projected",
					)
			}
			nativeActionCtx :=
				tools.ContextWithOpenAINativeComputerActionV1(attemptCtx)
			return e.runPlanV1(
				nativeActionCtx,
				plan,
				fmt.Sprintf(
					"%s/final-observation/%d",
					call.CallID,
					attempt,
				),
			)
		},
		func(
			attempt int,
			attemptResult agent.ToolResult,
			attemptErr error,
			duration time.Duration,
		) {
			attemptRecorded = true
			status := openAIComputerTraceStatusV1(attemptResult, attemptErr)
			failureCode := openAIComputerTraceFailureCodeV1(
				attemptResult,
				attemptErr,
			)
			if attemptErr == nil && !attemptResult.IsError &&
				len(attemptResult.Images) != 1 {
				status = "failed"
				if failureCode == "" {
					failureCode = "final_image_unavailable"
				}
			}
			trace.record(openAIComputerTraceWithCaptureDiagnosticsV1(openAIComputerTraceEventV1{
				Phase:       "final_observation",
				Status:      status,
				Attempt:     attempt,
				BatchIndex:  batchIndex,
				FailureCode: failureCode,
				DurationMS:  duration.Milliseconds(),
			}, attemptResult))
		},
	)
	if err == nil && !result.IsError && len(result.Images) == 1 {
		result.GUIOutcome = nil
		return result, nil
	}

	detail := ""
	if err != nil {
		detail = boundOpenAIComputerObservationDetailV1(err.Error())
	} else {
		detail = openAIComputerObservationResultDetailV1(result)
	}
	if detail == "" {
		detail = "the desktop observation completed without a verified image"
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		ctx.Err() != nil {
		failure := agent.BusinessError(
			"computer_use_error: cancelled\n" +
				"message: the final desktop observation was cancelled\n" +
				"detail: " + detail,
		)
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result:      agent.GUIActionResultCancelled,
			Phase:       agent.GUIActionPhaseObserving,
			FailureCode: "cancelled",
		}
		return failure, err
	}
	failure := agent.BusinessError(
		"computer_use_error: final_observation_unavailable\n" +
			"message: the action batch ended, but no fresh final screenshot could be captured\n" +
			"detail: " + detail,
	)
	failure.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultFailed,
		Phase:       agent.GUIActionPhaseObserving,
		FailureCode: "final_observation_unavailable",
	}
	return failure, fmt.Errorf(
		"OpenAI computer final exact screenshot is unavailable: %s",
		detail,
	)
}

func (e *daemonOpenAIComputerExecutorV1) runPlanV1(
	ctx context.Context,
	plan tools.OpenAIComputerActionPlanV1,
	toolUseID string,
) (agent.ToolResult, error) {
	if plan.Tool == nil || plan.Tool.Info().Name != "computer_use" ||
		plan.Args == "" || toolUseID == "" {
		return agent.ToolResult{},
			fmt.Errorf("OpenAI computer action plan identity is invalid")
	}
	invocationCtx := agent.ContextWithToolInvocation(ctx, agent.ToolInvocation{
		ToolName:  plan.Tool.Info().Name,
		ToolUseID: toolUseID,
	})
	return e.workflow.runTool(invocationCtx, plan.Tool, plan.Args)
}

func (e *daemonOpenAIComputerExecutorV1) validateActionBoundary(
	authority tools.OpenAIComputerBatchAuthorityV1,
	scope tools.OpenAIComputerActionScopeV1,
	action tools.OpenAIComputerActionV1,
) error {
	if e == nil {
		return fmt.Errorf("OpenAI computer executor is unavailable")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !e.provenance.IsTrusted() || e.call == nil ||
		e.authority != authority ||
		scope.ResponseID != e.call.ResponseID ||
		scope.CallID != e.call.CallID ||
		scope.Provider != e.call.Provider ||
		scope.APISurface != e.call.APISurface ||
		scope.ToolContract != e.call.ToolContract ||
		scope.ActionIndex != e.nextActionIndex ||
		scope.ActionIndex < 0 ||
		scope.ActionIndex >= len(e.call.Actions) ||
		scope.ActionCount != len(e.call.Actions) ||
		scope.ActionID != e.call.CallID+"/action/"+
			fmt.Sprint(scope.ActionIndex+1) ||
		!reflect.DeepEqual(e.call.Actions[scope.ActionIndex], action) {
		return fmt.Errorf("OpenAI computer action provenance or ordering mismatch")
	}
	return nil
}

func (e *daemonOpenAIComputerExecutorV1) validateFinalBoundary(
	authority tools.OpenAIComputerBatchAuthorityV1,
	call tools.OpenAIComputerCallV1,
) error {
	if e == nil {
		return fmt.Errorf("OpenAI computer executor is unavailable")
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.closed || !e.provenance.IsTrusted() || e.call == nil ||
		e.authority != authority ||
		!reflect.DeepEqual(*e.call, call) ||
		e.finalCaptures != 0 {
		return fmt.Errorf("OpenAI computer final observation provenance mismatch")
	}
	// Reserve the only capture while still under the executor lock. A racing
	// direct interface caller must fail before reaching the runtime/tool.
	e.finalCaptures = 1
	return nil
}

func openAIComputerActionMutatesInDaemonV1(
	action tools.OpenAIComputerActionV1,
) bool {
	return action.Type != tools.OpenAIComputerActionScreenshotV1 &&
		action.Type != tools.OpenAIComputerActionWaitV1
}

func openAIComputerCommitStateV1(
	result agent.ToolResult,
	runErr error,
) tools.OpenAIComputerCommitStateV1 {
	if result.GUIOutcome == nil {
		if runErr != nil || result.IsError {
			return tools.OpenAIComputerCommitUnknownV1
		}
		// A mutation without a typed acknowledgement is not sufficient proof of
		// verification, even if the legacy ToolResult claims success.
		return tools.OpenAIComputerCommitUnverifiedV1
	}
	switch result.GUIOutcome.Result {
	case agent.GUIActionResultVerified:
		return tools.OpenAIComputerCommitVerifiedV1
	case agent.GUIActionResultCompletedUnverified:
		switch result.GUIOutcome.FailureCode {
		case "commit_unknown",
			"commit_status_unknown",
			"action_commit_unknown",
			"invalid_helper_result":
			return tools.OpenAIComputerCommitUnknownV1
		}
		return tools.OpenAIComputerCommitUnverifiedV1
	case agent.GUIActionResultUserInterference, agent.GUIActionResultCancelled:
		return tools.OpenAIComputerCommitUnknownV1
	case agent.GUIActionResultFailed:
		if result.GUIOutcome.Phase == agent.GUIActionPhaseInputCommitted ||
			result.GUIOutcome.Phase == agent.GUIActionPhaseVerifying {
			return tools.OpenAIComputerCommitUnknownV1
		}
		return tools.OpenAIComputerNotCommittedV1
	default:
		return tools.OpenAIComputerCommitUnknownV1
	}
}

// EndBatchV1 invalidates the executor-local provider authority. The enclosing
// daemonGUIWorkflow owns turn-level lease release, so a Responses continuation
// can retain the same one-controller workflow until AgentLoop returns.
func (e *daemonOpenAIComputerExecutorV1) EndBatchV1() {
	if e == nil {
		return
	}
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	e.mu.Lock()
	e.closed = true
	e.call = nil
	e.authority = tools.OpenAIComputerBatchAuthorityV1{}
	e.mu.Unlock()
}

var _ tools.OpenAIComputerBatchActionExecutorV1 = (*daemonOpenAIComputerExecutorV1)(nil)
var _ agent.OpenAIComputerBatchExecutor = (*daemonOpenAIComputerBatchRunnerV1)(nil)
