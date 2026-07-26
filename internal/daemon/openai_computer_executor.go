package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

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
}

type openAIComputerFreshApprovalV1 func(
	context.Context,
	string,
	string,
) bool

type daemonOpenAIComputerPrivateRuntimeV1 struct {
	runtime      *tools.OpenAIComputerActionRuntimeV1
	approvalCore agent.Tool
}

// daemonOpenAIComputerBatchRunnerV1 is the daemon-owned AgentLoop callback.
// It mints a fresh response-bound executor for every provider computer_call,
// while daemonGUIWorkflow retains the single turn lease across Responses
// continuations.
type daemonOpenAIComputerBatchRunnerV1 struct {
	workflow     *daemonGUIWorkflow
	runtime      openAIComputerActionRuntimeV1
	approvalTool agent.Tool
}

func newDaemonOpenAIComputerBatchRunnerV1(
	workflow *daemonGUIWorkflow,
	runtime openAIComputerActionRuntimeV1,
	approvalTool agent.Tool,
) (*daemonOpenAIComputerBatchRunnerV1, error) {
	if workflow == nil || workflow.coordinator == nil {
		return nil, fmt.Errorf("OpenAI computer daemon workflow is unavailable")
	}
	if runtime == nil {
		return nil, fmt.Errorf("OpenAI computer action runtime is unavailable")
	}
	if approvalTool == nil || approvalTool.Info().Name != "computer_use" {
		return nil, fmt.Errorf("OpenAI computer approval tool is unavailable")
	}
	return &daemonOpenAIComputerBatchRunnerV1{
		workflow:     workflow,
		runtime:      runtime,
		approvalTool: approvalTool,
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
		runtime:      runtime,
		approvalCore: core,
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

func installDaemonOpenAIComputerBatchRunnerV1(
	loop *agent.AgentLoop,
	workflow *daemonGUIWorkflow,
	runtime *tools.OpenAIComputerActionRuntimeV1,
	approvalTool agent.Tool,
) error {
	if runtime == nil {
		return nil
	}
	if loop == nil || workflow == nil {
		return fmt.Errorf("OpenAI computer AgentLoop installation is unavailable")
	}
	runner, err := newDaemonOpenAIComputerBatchRunnerV1(
		workflow,
		runtime,
		approvalTool,
	)
	if err != nil {
		return err
	}
	loop.SetOpenAIComputerBatchExecutor(runner)
	return nil
}

func (r *daemonOpenAIComputerBatchRunnerV1) ExecuteOpenAIComputerBatch(
	ctx context.Context,
	profile *client.ExecutionProfile,
	responseID string,
	payload json.RawMessage,
	safetyAcknowledgement *agent.OpenAIComputerSafetyAcknowledgement,
	approve agent.OpenAIComputerFreshApproval,
) (agent.OpenAIComputerBatchExecution, error) {
	if r == nil || r.workflow == nil || r.runtime == nil ||
		r.approvalTool == nil || safetyAcknowledgement == nil || approve == nil {
		return agent.OpenAIComputerBatchExecution{},
			fmt.Errorf("OpenAI computer daemon batch runner is unavailable")
	}
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
		func(approvalCtx context.Context, _ string, args string) bool {
			return approve(approvalCtx, r.approvalTool, args)
		},
	)
	if err != nil {
		return agent.OpenAIComputerBatchExecution{}, err
	}
	defer executor.EndBatchV1()

	result, executeErr := tools.NewOpenAIComputerAdapterV1(executor).
		ExecuteBatchV1(ctx, payload)
	return agent.OpenAIComputerBatchExecution{
		CallID:              result.CallID,
		ContinuationAllowed: executeErr == nil && !result.ToolResult.IsError,
		Result:              result.ToolResult,
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

	workflow   *daemonGUIWorkflow
	runtime    openAIComputerActionRuntimeV1
	provenance tools.OpenAIComputerExecutionProvenanceV1
	approve    openAIComputerFreshApprovalV1

	authority        tools.OpenAIComputerBatchAuthorityV1
	call             *tools.OpenAIComputerCallV1
	nextActionIndex  int
	needsObservation bool
	finalCaptures    int
	closed           bool
}

func newDaemonOpenAIComputerExecutorV1(
	workflow *daemonGUIWorkflow,
	runtime openAIComputerActionRuntimeV1,
	provenance tools.OpenAIComputerExecutionProvenanceV1,
	approve openAIComputerFreshApprovalV1,
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
	if approve == nil {
		return nil, fmt.Errorf("OpenAI computer fresh approval seam is unavailable")
	}
	return &daemonOpenAIComputerExecutorV1{
		workflow: workflow, runtime: runtime,
		provenance: provenance, approve: approve,
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

	// Classify the exact current target without committing GUI input. The
	// resulting descriptor freezes the batch allowlist into one workflow lease.
	plan, err := e.runtime.PlanOpenAIComputerObservationV1(
		"Bind OpenAI native computer batch target",
		false,
	)
	if err != nil {
		return tools.OpenAIComputerBatchAuthorityV1{},
			fmt.Errorf("OpenAI computer batch target could not be safely classified")
	}
	descriptor, err := describeOpenAIComputerPlanV1(ctx, plan, false)
	if err != nil || descriptor.TargetBundleID == "" {
		return tools.OpenAIComputerBatchAuthorityV1{},
			fmt.Errorf("OpenAI computer batch target could not be safely classified")
	}
	if e.workflow.appPolicy != nil &&
		e.workflow.appPolicy.DecisionFor(descriptor.TargetBundleID).Decision ==
			ComputerUseAppPolicyBlocked {
		return tools.OpenAIComputerBatchAuthorityV1{},
			fmt.Errorf("OpenAI computer batch target is blocked by app policy")
	}
	// The runtime plan describes the exact current observation that was sent to
	// the provider. Admit that already-observed target without pretending the
	// classification step itself is another GUI observation.
	lease, err := e.workflow.ensureLeaseWithObservedTarget(ctx, descriptor, true)
	if err != nil {
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
	e.needsObservation = false
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
) (tools.OpenAIComputerActionExecutionV1, error) {
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

	e.mu.Lock()
	needsObservation := e.needsObservation
	e.mu.Unlock()
	if needsObservation {
		if err := e.runRequiredObservationV1(ctx, scope); err != nil {
			return tools.OpenAIComputerActionExecutionV1{
				CommitState: tools.OpenAIComputerNotCommittedV1,
			}, err
		}
	}

	plan, err := e.runtime.PlanOpenAIComputerActionV1(ctx, action)
	if err != nil {
		return tools.OpenAIComputerActionExecutionV1{
			CommitState: tools.OpenAIComputerNotCommittedV1,
		}, fmt.Errorf("OpenAI computer action could not be safely projected")
	}
	if plan.Mutation != openAIComputerActionMutatesInDaemonV1(action) {
		return tools.OpenAIComputerActionExecutionV1{
			CommitState: tools.OpenAIComputerNotCommittedV1,
		}, fmt.Errorf("OpenAI computer action projection changed its effect")
	}
	nativeActionCtx := tools.ContextWithOpenAINativeComputerActionV1(ctx)
	if _, err := describeOpenAIComputerPlanV1(
		nativeActionCtx, plan, plan.Mutation); err != nil {
		return tools.OpenAIComputerActionExecutionV1{
			CommitState: tools.OpenAIComputerNotCommittedV1,
		}, fmt.Errorf("OpenAI computer action projection is not executable")
	}
	if plan.Mutation && !e.approve(ctx, plan.Tool.Info().Name, plan.Args) {
		return tools.OpenAIComputerActionExecutionV1{
			CommitState: tools.OpenAIComputerNotCommittedV1,
			Result:      agent.BusinessError("fresh per-action approval denied"),
		}, fmt.Errorf("OpenAI computer fresh per-action approval denied")
	}

	result, runErr := e.runPlanV1(nativeActionCtx, plan, scope.ActionID)
	execution := tools.OpenAIComputerActionExecutionV1{
		CommitState: tools.OpenAIComputerNotCommittedV1,
		Result:      result,
	}
	// Provider-visible batches receive no intermediate images, including error
	// paths. CaptureFinalOpenAIComputerObservationV1 is the sole image seam.
	execution.Result.Images = nil
	if plan.Mutation {
		execution.CommitState = openAIComputerCommitStateV1(result, runErr)
	}

	e.mu.Lock()
	e.nextActionIndex++
	e.needsObservation =
		plan.Mutation &&
			execution.CommitState != tools.OpenAIComputerNotCommittedV1
	e.mu.Unlock()
	if runErr != nil {
		return execution, fmt.Errorf("OpenAI computer action executor failed")
	}
	if execution.Result.IsError {
		return execution, nil
	}
	return execution, nil
}

func (e *daemonOpenAIComputerExecutorV1) CaptureFinalOpenAIComputerObservationV1(
	ctx context.Context,
	authority tools.OpenAIComputerBatchAuthorityV1,
	call tools.OpenAIComputerCallV1,
) (agent.ToolResult, error) {
	if e == nil {
		return agent.ToolResult{},
			fmt.Errorf("OpenAI computer executor is unavailable")
	}
	e.operationMu.Lock()
	defer e.operationMu.Unlock()
	if err := e.validateFinalBoundary(authority, call); err != nil {
		return agent.ToolResult{}, err
	}
	if err := ctx.Err(); err != nil {
		return agent.ToolResult{}, fmt.Errorf("OpenAI computer final observation cancelled")
	}
	plan, err := e.runtime.PlanOpenAIComputerObservationV1(
		"Capture final OpenAI native computer screenshot",
		true,
	)
	if err != nil {
		return agent.ToolResult{},
			fmt.Errorf("OpenAI computer final observation could not be safely projected")
	}
	if _, err := describeOpenAIComputerPlanV1(ctx, plan, false); err != nil {
		return agent.ToolResult{},
			fmt.Errorf("OpenAI computer final observation is not executable")
	}
	result, runErr := e.runPlanV1(
		ctx,
		plan,
		call.CallID+"/final-observation",
	)
	if runErr != nil || result.IsError || len(result.Images) != 1 {
		return agent.ToolResult{},
			fmt.Errorf("OpenAI computer final exact screenshot is unavailable")
	}
	result.GUIOutcome = nil
	return result, nil
}

func (e *daemonOpenAIComputerExecutorV1) runRequiredObservationV1(
	ctx context.Context,
	scope tools.OpenAIComputerActionScopeV1,
) error {
	plan, err := e.runtime.PlanOpenAIComputerObservationV1(
		"Refresh exact AX state between OpenAI computer actions",
		false,
	)
	if err != nil {
		return fmt.Errorf("OpenAI computer required re-observation is unavailable")
	}
	if _, err := describeOpenAIComputerPlanV1(ctx, plan, false); err != nil {
		return fmt.Errorf("OpenAI computer required re-observation is invalid")
	}
	result, runErr := e.runPlanV1(
		ctx,
		plan,
		scope.ActionID+"/reobserve",
	)
	if runErr != nil || result.IsError || len(result.Images) != 0 {
		return fmt.Errorf("OpenAI computer required re-observation failed")
	}
	e.mu.Lock()
	e.needsObservation = false
	e.mu.Unlock()
	return nil
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

func describeOpenAIComputerPlanV1(
	ctx context.Context,
	plan tools.OpenAIComputerActionPlanV1,
	mutation bool,
) (agent.GUIActionDescriptor, error) {
	if plan.Tool == nil || plan.Tool.Info().Name != "computer_use" ||
		plan.Args == "" || plan.Mutation != mutation {
		return agent.GUIActionDescriptor{}, fmt.Errorf("invalid OpenAI computer action plan")
	}
	describer, ok := plan.Tool.(agent.GUIActionDescriber)
	if !ok {
		return agent.GUIActionDescriptor{},
			fmt.Errorf("OpenAI computer action plan lacks a GUI descriptor")
	}
	descriptor, err := describer.DescribeGUIAction(ctx, plan.Args)
	if err != nil || !descriptor.Participates ||
		descriptor.ActionKind == "" ||
		descriptor.TargetBundleID == "" {
		return agent.GUIActionDescriptor{},
			fmt.Errorf("OpenAI computer action target is unavailable")
	}
	wantEffect := agent.GUIActionObservation
	if mutation {
		wantEffect = agent.GUIActionMutation
	}
	if descriptor.Effect != wantEffect {
		return agent.GUIActionDescriptor{},
			fmt.Errorf("OpenAI computer action effect is inconsistent")
	}
	return descriptor, nil
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
