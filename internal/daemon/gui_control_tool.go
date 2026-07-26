package daemon

import (
	"context"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

type daemonGUIWorkflowRequest struct {
	SessionID   string
	TurnID      string
	SourceKind  string
	SourceLabel string
}

// daemonGUIWorkflow owns one lazy, per-Run GUI lease. It is deliberately
// constructed only by daemon RunAgent. CLI and TUI do not require a Desktop
// heartbeat for observations, but their shared registry's final execution gate
// rejects GUI mutations because those paths cannot mint this workflow's opaque
// action authority.
type daemonGUIWorkflow struct {
	mu          sync.Mutex
	coordinator *guicontrol.Coordinator
	request     daemonGUIWorkflowRequest
	lease       *guicontrol.WorkflowLease
	lastResult  guicontrol.ComputerUseActionResult
	appPolicy   *ComputerUseAppPolicyStore
	riskBroker  *ConsequentialRiskBroker
	riskIntents map[string]struct{}

	nativePreparationSequence uint64

	invocationFromContext func(context.Context) (agent.ToolInvocation, bool)
}

func newDaemonGUIWorkflow(coordinator *guicontrol.Coordinator, request daemonGUIWorkflowRequest) *daemonGUIWorkflow {
	return &daemonGUIWorkflow{
		coordinator:           coordinator,
		request:               request,
		lastResult:            guicontrol.ComputerUseResultFailed,
		riskIntents:           make(map[string]struct{}),
		invocationFromContext: agent.ToolInvocationFromContext,
	}
}

func (w *daemonGUIWorkflow) ensureLease(ctx context.Context, descriptor agent.GUIActionDescriptor) (guicontrol.WorkflowLease, error) {
	w.mu.Lock()
	if w.lease != nil {
		lease := *w.lease
		w.mu.Unlock()
		return lease, nil
	}
	if w.coordinator == nil {
		w.mu.Unlock()
		return guicontrol.WorkflowLease{}, fmt.Errorf("computer-use coordinator is unavailable")
	}
	allowed := []string(nil)
	if descriptor.Effect == agent.GUIActionMutation &&
		descriptor.TargetBundleID != "" {
		// A first mutation carries its own exact execution authority. The
		// first verified observation is admitted by FinishAction instead.
		allowed = []string{descriptor.TargetBundleID}
	}
	lease, err := w.coordinator.BeginWorkflow(guicontrol.WorkflowRequest{
		SessionID:            w.request.SessionID,
		TurnID:               w.request.TurnID,
		SourceKind:           w.request.SourceKind,
		SourceLabel:          w.request.SourceLabel,
		RequestedAppBundleID: descriptor.TargetBundleID,
		RequestedAppName:     descriptor.TargetAppName,
		AllowedAppBundleIDs:  allowed,
		PolicySnapshotID:     "gui-policy/" + w.request.TurnID,
	})
	if err != nil {
		w.mu.Unlock()
		return guicontrol.WorkflowLease{}, err
	}
	// BeginWorkflow only publishes the requesting workflow. A real Desktop
	// heartbeat must acknowledge it; this wait is event-driven inside the
	// coordinator and never polls or masquerades BeginWorkflow as liveness.
	if err := w.coordinator.AwaitController(ctx, lease.LeaseID, lease.TurnID); err != nil {
		// Await cancellation occurs before w.lease is published. Release the
		// just-created workflow immediately so a disconnected caller cannot
		// strand every other route as busy until TTL. Stop/expiry may already
		// have terminalized it; EndTurn is nil-safe for that state.
		_ = w.coordinator.EndTurn(lease.TurnID, guicontrol.ComputerUseResultCancelled)
		w.mu.Unlock()
		return guicontrol.WorkflowLease{}, err
	}
	w.lease = &lease
	w.mu.Unlock()
	return lease, nil
}

func (w *daemonGUIWorkflow) currentLease() (guicontrol.WorkflowLease, bool) {
	if w == nil {
		return guicontrol.WorkflowLease{}, false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.lease == nil {
		return guicontrol.WorkflowLease{}, false
	}
	return *w.lease, true
}

func (w *daemonGUIWorkflow) runTool(ctx context.Context, tool agent.Tool, argsJSON string) (result agent.ToolResult, runErr error) {
	describer, ok := tool.(agent.GUIActionDescriber)
	if !ok {
		return tool.Run(ctx, argsJSON)
	}
	descriptor, err := describer.DescribeGUIAction(ctx, argsJSON)
	if err != nil {
		// A descriptor failure must never fall through to Tool.Run: doing so
		// would turn any future parser/resolver regression into a legacy bypass.
		return agent.ValidationError("GUI action could not be safely classified"), nil
	}
	if !descriptor.Participates {
		// Explicit non-GUI reads such as Ghostty list_tabs keep their original
		// behavior and do not create a misleading workflow lease.
		return tool.Run(ctx, argsJSON)
	}
	if descriptor.Effect != agent.GUIActionObservation && descriptor.Effect != agent.GUIActionMutation {
		return agent.BusinessError("computer-use policy denied an unclassified GUI action"), nil
	}
	// Re-evaluate the final descriptor on every action, after approval and
	// before lease/action admission. ApprovalAdmission classified an earlier
	// descriptor; the foreground app or resolved target may have changed while
	// the user was deciding. The lease allow-list is not a substitute for this
	// per-action check, especially when a workflow already owns a lease.
	if descriptor.TargetBundleID != "" && w.appPolicy != nil {
		entry := w.appPolicy.DecisionFor(descriptor.TargetBundleID)
		if entry.Decision == ComputerUseAppPolicyBlocked {
			return computerUseAppPolicyBlockedResult(descriptor, entry), nil
		}
	}
	if descriptor.Effect == agent.GUIActionMutation && descriptor.TargetBundleID == "" {
		if tool.Info().Name == "computer_use" && descriptor.ActionKind == "launch_app" {
			return agent.BusinessError("computer_use launch_app cannot safely resolve the target bundle before launch; launch the app manually first, then observe it before continuing"), nil
		}
		switch descriptor.ActionKind {
		case "type":
			return agent.BusinessError(
				"computer_use_error: keyboard_target_unavailable\n" +
					"message: no current text target is available from an AX focused ref or one verified coordinate click\n" +
					"recovery: observe the intended window with a screenshot, click the editor once, then type once; do not retry automatically",
			), nil
		case "hotkey", "scroll":
			return agent.BusinessError("computer-use target-bound execution is unavailable for " + descriptor.ActionKind + "; use an accessibility action tied to an observed element"), nil
		}
	}
	invocation, ok := w.invocationFromContext(ctx)
	if !ok || invocation.ToolUseID == "" || invocation.ToolName != "" && invocation.ToolName != tool.Info().Name {
		return agent.BusinessError("computer-use policy denied GUI action without exact tool invocation identity"), nil
	}
	lease, err := w.ensureLease(ctx, descriptor)
	if err != nil {
		return guiCoordinatorToolError(err), nil
	}
	effect := guicontrol.ComputerUseActionObservation
	if descriptor.Effect == agent.GUIActionMutation {
		effect = guicontrol.ComputerUseActionMutation
	}
	phase := guicontrol.ComputerUsePhaseActing
	if descriptor.Effect == agent.GUIActionObservation {
		phase = guicontrol.ComputerUsePhaseObserving
	} else if descriptor.ActionKind == "move" {
		phase = guicontrol.ComputerUsePhaseMoving
	}
	executionPath := guiExecutionPath(descriptor.ExecutionPath)
	orderedBatchAction := tools.IsOpenAINativeComputerActionV1(ctx)
	actionRequest := guicontrol.ActionRequest{
		LeaseID:            lease.LeaseID,
		TurnID:             lease.TurnID,
		ToolName:           tool.Info().Name,
		ToolUseID:          invocation.ToolUseID,
		ActionKind:         descriptor.ActionKind,
		ActionPhase:        phase,
		TargetBundleID:     descriptor.TargetBundleID,
		TargetAppName:      descriptor.TargetAppName,
		ExecutionPath:      executionPath,
		Effect:             effect,
		OrderedBatchAction: orderedBatchAction,
	}
	var approvedRisk *tools.ConsequentialRiskDraftV1
	var riskIntentID string
	if preflighter, supportsRisk := tool.(tools.ConsequentialRiskPreflighterV1); supportsRisk {
		preflight, preflightErr := preflighter.PreflightConsequentialRiskV1(ctx, argsJSON, invocation.ToolUseID)
		if preflightErr != nil {
			return consequentialRiskDaemonFailure(tools.ConsequentialRiskCodeUntrustedMetadataV1), nil
		}
		switch preflight.Status {
		case tools.ConsequentialRiskPreflightNoneV1:
		case tools.ConsequentialRiskPreflightBlockedV1:
			return consequentialRiskDaemonFailure(preflight.FailureCode), nil
		case tools.ConsequentialRiskPreflightRequiredV1:
			if preflight.Draft == nil || w.riskBroker == nil {
				return consequentialRiskDaemonFailure(tools.ConsequentialRiskCodeMissingGrantV1), nil
			}
			if preflight.Draft.Target.BundleID != actionRequest.TargetBundleID ||
				preflight.Draft.Target.AppName != actionRequest.TargetAppName ||
				preflight.Draft.Target.ActionKind != actionRequest.ActionKind ||
				preflight.Draft.Target.ExecutionPath != authorityPathForActionRequest(actionRequest) {
				return consequentialRiskDaemonFailure(tools.ConsequentialRiskCodeGrantMismatchV1), nil
			}
			intent, marker, registerErr := w.riskBroker.Register(*preflight.Draft)
			if registerErr != nil {
				return consequentialRiskDaemonFailure(tools.ConsequentialRiskCodeUntrustedMetadataV1), nil
			}
			riskIntentID = intent.IntentID
			w.trackRiskIntent(riskIntentID)
			defer func() {
				w.riskBroker.InvalidateIntent(riskIntentID)
				w.untrackRiskIntent(riskIntentID)
			}()
			actionRequest.RiskIntentID = intent.IntentID
			actionRequest.RiskTargetDigest = intent.Target.TargetDigest
			stage, stageErr := w.coordinator.StageConsequentialRisk(ctx, actionRequest, guicontrol.ConsequentialRiskMarkerV1{
				SchemaVersion: marker.SchemaVersion, Required: marker.Required, Kind: marker.Kind,
				IntentID: marker.IntentID, ExpiresAt: marker.ExpiresAt,
			})
			if stageErr != nil {
				return guiCoordinatorToolError(stageErr), nil
			}
			decision, decisionErr := w.riskBroker.AwaitDecision(stage.Context, intent.IntentID)
			if decisionErr != nil {
				_ = w.coordinator.CancelConsequentialRisk(lease.LeaseID, intent.IntentID)
				return consequentialRiskDaemonFailure("consequential_risk_confirmation_cancelled"), nil
			}
			if decision.Decision != ConsequentialRiskDecisionAllowed {
				_ = w.coordinator.CancelConsequentialRisk(lease.LeaseID, intent.IntentID)
				return consequentialRiskDaemonFailure("consequential_risk_confirmation_denied"), nil
			}
			approved := tools.ConsequentialRiskDraftV1{
				RequestID: intent.RequestID, Kind: intent.Kind, Target: intent.Target,
				Send: intent.Send, Delete: intent.Delete, Purchase: intent.Purchase,
			}
			approvedRisk = &approved
		default:
			return consequentialRiskDaemonFailure(tools.ConsequentialRiskCodeUntrustedMetadataV1), nil
		}
	}
	handle, err := w.coordinator.BeginAction(ctx, actionRequest)
	if err != nil {
		return guiCoordinatorToolError(err), nil
	}

	// Every successful BeginAction has exactly one acknowledgement, including
	// panic, ordinary error, Stop/Take Over, and heartbeat-expiry cancellation.
	// Coordinator quiescence depends on this defer before Resume can admit work.
	defer func() {
		outcome := guicontrol.ComputerUseResultVerified
		finishPhase := guicontrol.ComputerUsePhaseIdle
		var finishPointer *guicontrol.ComputerUsePointer
		var failureCode *string
		// A versioned mutation may observe controller cancellation, quiesce, and
		// return a precise non-verified typed acknowledgement. Preserve that
		// result below; context cancellation alone is used only when no such ack
		// exists (or a racy success still claims verified after cancellation).
		if handle.Context.Err() != nil &&
			(result.GUIOutcome == nil || result.GUIOutcome.Result == agent.GUIActionResultVerified) {
			outcome = guicontrol.ComputerUseResultCancelled
			code := "control_cancelled"
			message := "computer-use observation cancelled by controller"
			if descriptor.Effect == agent.GUIActionMutation {
				// Context cancellation only proves the executor returned; it does
				// not prove that AX/CGEvent/Apple Events failed to commit first.
				// Never invite an automatic retry of a possibly committed input.
				code = "commit_status_unknown_after_cancel"
				message = "computer-use mutation was interrupted; commit status is unknown; do not retry automatically; re-observe the app"
			}
			failureCode = &code
			result = agent.BusinessError(message)
			runErr = nil
		} else if recovered := recover(); recovered != nil {
			outcome = guicontrol.ComputerUseResultFailed
			code := "tool_panicked"
			failureCode = &code
			finishErr := w.coordinator.FinishAction(guicontrol.ActionFinish{
				LeaseID: lease.LeaseID, ActionID: handle.ActionID,
				Phase: guicontrol.ComputerUsePhaseIdle, Result: &outcome,
				ExecutionPath: executionPath, FailureCode: failureCode,
			})
			w.recordResult(outcome)
			if finishErr != nil {
				log.Printf("daemon: computer-use panic acknowledgement failed: %v", finishErr)
			}
			panic(recovered)
		} else if result.GUIOutcome != nil {
			if err := result.GUIOutcome.Validate(); err != nil {
				outcome = guicontrol.ComputerUseResultFailed
				code := "invalid_tool_outcome"
				failureCode = &code
				result = agent.BusinessError("computer-use executor returned an invalid typed acknowledgement")
				runErr = nil
			} else {
				outcome = guiTypedResult(result.GUIOutcome.Result)
				finishPhase = guiTypedPhase(result.GUIOutcome.Phase)
				if result.GUIOutcome.FailureCode != "" {
					code := result.GUIOutcome.FailureCode
					failureCode = &code
				}
				if pointer := result.GUIOutcome.Pointer; pointer != nil {
					finishPointer = &guicontrol.ComputerUsePointer{
						DisplayID: pointer.DisplayID, TopologyID: pointer.TopologyID,
						TopologyGeneration: pointer.TopologyGeneration,
						X:                  pointer.X, Y: pointer.Y,
						CoordinateSpace: guicontrol.ComputerUseCoordinateQuartzGlobalPoints,
					}
				}
			}
		} else if runErr != nil || result.IsError {
			outcome = guicontrol.ComputerUseResultFailed
			code := "tool_failed"
			if runErr != nil {
				code = "execution_error"
			}
			failureCode = &code
		} else if descriptor.Effect == agent.GUIActionMutation {
			outcome = guicontrol.ComputerUseResultCompletedUnverified
		}
		finishErr := w.coordinator.FinishAction(guicontrol.ActionFinish{
			LeaseID: lease.LeaseID, ActionID: handle.ActionID,
			Phase: finishPhase, Result: &outcome,
			ExecutionPath: executionPath, Pointer: finishPointer, FailureCode: failureCode,
		})
		w.recordResult(outcome)
		if finishErr != nil {
			log.Printf("daemon: computer-use action acknowledgement failed: %v", finishErr)
			if runErr == nil && !result.IsError {
				result = guiCoordinatorToolError(finishErr)
			}
		}
	}()

	authorityPath := ""
	if actionRequest.ExecutionPath != nil {
		authorityPath = string(*actionRequest.ExecutionPath)
	}
	executionCtx := handle.AuthorizeExecution(guicontrol.ExecutionScope{
		ToolName: tool.Info().Name, ToolUseID: invocation.ToolUseID,
		ActionKind: actionRequest.ActionKind, Effect: string(actionRequest.Effect),
		TargetBundleID: actionRequest.TargetBundleID, ExecutionPath: authorityPath,
		RiskIntentID: actionRequest.RiskIntentID, RiskTargetDigest: actionRequest.RiskTargetDigest,
	})
	if approvedRisk != nil {
		executionCtx = tools.ContextWithConsequentialRiskExecutionV1(
			executionCtx, riskIntentID, *approvedRisk,
			func(rederived tools.ConsequentialRiskDraftV1) error {
				return w.riskBroker.ConsumeGrant(ConsequentialRiskGrantClaimV1{
					IntentID: riskIntentID, RequestID: rederived.RequestID,
					TargetDigest: rederived.Target.TargetDigest, Kind: rederived.Kind,
					Send: rederived.Send, Delete: rederived.Delete, Purchase: rederived.Purchase,
				})
			},
		)
	}
	// Re-bind the exact identity verified above so the final registry gate does
	// not depend on which dispatcher/test context the daemon wrapper received.
	executionCtx = agent.ContextWithToolInvocation(executionCtx, agent.ToolInvocation{
		ToolName: tool.Info().Name, ToolUseID: invocation.ToolUseID,
	})
	if descriptor.Effect == agent.GUIActionMutation && !orderedBatchAction {
		if restorer, ok := tool.(tools.GUIActionTargetRestorerV1); ok {
			if err := restorer.RestoreGUIActionTargetV1(executionCtx, descriptor); err != nil {
				return agent.BusinessError(
					"computer-use target could not be restored safely; re-observe the app before retrying",
				), nil
			}
		}
	}
	return tool.Run(executionCtx, argsJSON)
}

type nativeToolRequestPreparationDescriber interface {
	DescribeNativeToolRequestPreparation(context.Context) (agent.GUIActionDescriptor, error)
}

func (w *daemonGUIWorkflow) nextNativePreparationToolUseID() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.nativePreparationSequence++
	return fmt.Sprintf(
		"native_prepare/%s/%d",
		w.request.TurnID,
		w.nativePreparationSequence,
	)
}

// prepareNativeToolRequest admits provider-native bootstrap observation through
// the same controller authority as ordinary GUI tool calls. Preparation occurs
// before a provider request exists, so there is no model tool_use_id to bind;
// the workflow mints a deterministic turn-local identity for the exact pending
// request instead.
//
// Preparation is observation-only. A future native preparer that needs to
// focus, launch, or otherwise mutate an app must use an attended approval seam;
// until one exists at this pre-provider boundary, mutation is rejected rather
// than silently bypassing ordinary tool-call approval.
func (w *daemonGUIWorkflow) prepareNativeToolRequest(
	ctx context.Context,
	tool agent.Tool,
	native agent.NativeToolProvider,
) (prepareErr error) {
	preparer, ok := native.(agent.NativeToolRequestPreparer)
	if !ok {
		return nil
	}
	describer, ok := native.(nativeToolRequestPreparationDescriber)
	if !ok {
		return fmt.Errorf("computer-use native preparation lacks an exact GUI action descriptor")
	}
	descriptor, err := describer.DescribeNativeToolRequestPreparation(ctx)
	if err != nil {
		return fmt.Errorf("computer-use native preparation could not be safely classified: %w", err)
	}
	if !descriptor.Participates || descriptor.ActionKind == "" {
		return fmt.Errorf("computer-use native preparation lacks an exact GUI observation")
	}
	if descriptor.Effect != agent.GUIActionObservation {
		return fmt.Errorf(
			"computer-use native preparation is observation-only; %s requires attended approval",
			descriptor.ActionKind,
		)
	}
	if descriptor.TargetBundleID == "" {
		return fmt.Errorf("computer-use native preparation could not resolve an exact app target")
	}
	if w.appPolicy != nil {
		entry := w.appPolicy.DecisionFor(descriptor.TargetBundleID)
		if entry.Decision == ComputerUseAppPolicyBlocked {
			return errors.New(computerUseAppPolicyBlockedResult(descriptor, entry).Content)
		}
	}

	lease, err := w.ensureLease(ctx, descriptor)
	if err != nil {
		return err
	}
	toolUseID := w.nextNativePreparationToolUseID()
	executionPath := guiExecutionPath(descriptor.ExecutionPath)
	handle, err := w.coordinator.BeginAction(ctx, guicontrol.ActionRequest{
		LeaseID:        lease.LeaseID,
		TurnID:         lease.TurnID,
		ToolName:       tool.Info().Name,
		ToolUseID:      toolUseID,
		ActionKind:     descriptor.ActionKind,
		ActionPhase:    guicontrol.ComputerUsePhaseObserving,
		TargetBundleID: descriptor.TargetBundleID,
		TargetAppName:  descriptor.TargetAppName,
		ExecutionPath:  executionPath,
		Effect:         guicontrol.ComputerUseActionObservation,
	})
	if err != nil {
		return err
	}

	defer func() {
		outcome := guicontrol.ComputerUseResultVerified
		var failureCode *string
		if handle.Context.Err() != nil {
			outcome = guicontrol.ComputerUseResultCancelled
			code := "control_cancelled"
			failureCode = &code
			prepareErr = fmt.Errorf(
				"computer-use native preparation cancelled: %w",
				handle.Context.Err(),
			)
		} else if recovered := recover(); recovered != nil {
			outcome = guicontrol.ComputerUseResultFailed
			code := "native_preparation_panicked"
			failureCode = &code
			finishErr := w.coordinator.FinishAction(guicontrol.ActionFinish{
				LeaseID: lease.LeaseID, ActionID: handle.ActionID,
				Phase: guicontrol.ComputerUsePhaseIdle, Result: &outcome,
				ExecutionPath: executionPath, FailureCode: failureCode,
			})
			w.recordResult(outcome)
			if finishErr != nil {
				log.Printf("daemon: native computer preparation panic acknowledgement failed: %v", finishErr)
			}
			panic(recovered)
		} else if prepareErr != nil {
			outcome = guicontrol.ComputerUseResultFailed
			code := "native_preparation_failed"
			failureCode = &code
		}
		finishErr := w.coordinator.FinishAction(guicontrol.ActionFinish{
			LeaseID: lease.LeaseID, ActionID: handle.ActionID,
			Phase: guicontrol.ComputerUsePhaseIdle, Result: &outcome,
			ExecutionPath: executionPath, FailureCode: failureCode,
		})
		w.recordResult(outcome)
		if finishErr != nil {
			log.Printf("daemon: native computer preparation acknowledgement failed: %v", finishErr)
			if prepareErr == nil {
				prepareErr = finishErr
			}
		}
	}()

	authorityPath := ""
	if executionPath != nil {
		authorityPath = string(*executionPath)
	}
	executionCtx := handle.AuthorizeExecution(guicontrol.ExecutionScope{
		ToolName:       tool.Info().Name,
		ToolUseID:      toolUseID,
		ActionKind:     descriptor.ActionKind,
		Effect:         string(guicontrol.ComputerUseActionObservation),
		TargetBundleID: descriptor.TargetBundleID,
		ExecutionPath:  authorityPath,
	})
	executionCtx = agent.ContextWithToolInvocation(executionCtx, agent.ToolInvocation{
		ToolName: tool.Info().Name, ToolUseID: toolUseID,
	})
	return preparer.PrepareNativeToolRequest(executionCtx)
}

func authorityPathForActionRequest(request guicontrol.ActionRequest) string {
	if request.ExecutionPath == nil {
		return ""
	}
	return string(*request.ExecutionPath)
}

func consequentialRiskDaemonFailure(code string) agent.ToolResult {
	if code == "" {
		code = tools.ConsequentialRiskCodeUntrustedMetadataV1
	}
	return agent.BusinessError("computer-use consequential action blocked: " + code)
}

func (w *daemonGUIWorkflow) trackRiskIntent(intentID string) {
	w.mu.Lock()
	w.riskIntents[intentID] = struct{}{}
	w.mu.Unlock()
}

func (w *daemonGUIWorkflow) untrackRiskIntent(intentID string) {
	w.mu.Lock()
	delete(w.riskIntents, intentID)
	w.mu.Unlock()
}

func guiTypedResult(result agent.GUIActionResult) guicontrol.ComputerUseActionResult {
	switch result {
	case agent.GUIActionResultVerified:
		return guicontrol.ComputerUseResultVerified
	case agent.GUIActionResultCompletedUnverified:
		return guicontrol.ComputerUseResultCompletedUnverified
	case agent.GUIActionResultCancelled:
		return guicontrol.ComputerUseResultCancelled
	case agent.GUIActionResultUserInterference:
		return guicontrol.ComputerUseResultUserInterference
	default:
		return guicontrol.ComputerUseResultFailed
	}
}

func guiTypedPhase(phase agent.GUIActionPhase) guicontrol.ComputerUseActionPhase {
	switch phase {
	case agent.GUIActionPhaseObserving:
		return guicontrol.ComputerUsePhaseObserving
	case agent.GUIActionPhaseMoving:
		return guicontrol.ComputerUsePhaseMoving
	case agent.GUIActionPhaseActing:
		return guicontrol.ComputerUsePhaseActing
	case agent.GUIActionPhaseInputCommitted:
		return guicontrol.ComputerUsePhaseInputCommitted
	case agent.GUIActionPhaseVerifying:
		return guicontrol.ComputerUsePhaseVerifying
	default:
		return guicontrol.ComputerUsePhaseIdle
	}
}

func (w *daemonGUIWorkflow) recordResult(result guicontrol.ComputerUseActionResult) {
	w.mu.Lock()
	w.lastResult = result
	w.mu.Unlock()
}

// EndTurn releases a live lease after AgentLoop.Run returns. Stopped and
// expired workflows may already have become terminal through FinishAction;
// Coordinator.EndTurn is intentionally nil-safe in that case.
func (w *daemonGUIWorkflow) EndTurn() {
	w.mu.Lock()
	lease := w.lease
	result := w.lastResult
	w.lease = nil
	intents := make([]string, 0, len(w.riskIntents))
	for intentID := range w.riskIntents {
		intents = append(intents, intentID)
	}
	clear(w.riskIntents)
	w.mu.Unlock()
	if w.riskBroker != nil {
		for _, intentID := range intents {
			w.riskBroker.InvalidateIntent(intentID)
		}
	}
	if lease == nil || w.coordinator == nil {
		return
	}
	if err := w.coordinator.EndTurn(lease.TurnID, result); err != nil {
		var stale *guicontrol.StaleLeaseError
		var stopped *guicontrol.StoppedTurnError
		var expired *guicontrol.LeaseExpiredError
		if errors.As(err, &stale) || errors.As(err, &stopped) || errors.As(err, &expired) {
			return
		}
		log.Printf("daemon: computer-use EndTurn failed: %v", err)
	}
}

func guiExecutionPath(value string) *guicontrol.ComputerUseExecutionPath {
	var path guicontrol.ComputerUseExecutionPath
	switch value {
	case string(guicontrol.ComputerUseExecutionAccessibility):
		path = guicontrol.ComputerUseExecutionAccessibility
	case string(guicontrol.ComputerUseExecutionSyntheticCoordinate):
		path = guicontrol.ComputerUseExecutionSyntheticCoordinate
	default:
		return nil
	}
	return &path
}

func guiCoordinatorToolError(err error) agent.ToolResult {
	if err == nil {
		return agent.BusinessError("computer-use control failed")
	}
	// Coordinator errors are already redacted and typed. Never append tool
	// arguments or underlying script/text content here.
	return agent.BusinessError(err.Error())
}

func computerUseAppPolicyBlockedResult(
	descriptor agent.GUIActionDescriptor,
	entry ComputerUseAppPolicyEntry,
) agent.ToolResult {
	target := strings.TrimSpace(descriptor.TargetAppName)
	if target == "" {
		target = strings.TrimSpace(descriptor.TargetBundleID)
	}
	if target == "" {
		target = "the requested app"
	}
	source := string(entry.Source)
	message := target + " is blocked by Computer Use app policy"
	recovery := "choose another app"
	switch entry.Source {
	case ComputerUseAppPolicySourceBuiltIn:
		message = target + " is a protected app and cannot be controlled by Computer Use"
	case ComputerUseAppPolicySourceUser:
		recovery = "remove this saved app block in Computer Use settings, or choose another app"
	case ComputerUseAppPolicySourceInvalidStore:
		message = "the Computer Use app policy could not be loaded, so " + target + " is blocked for safety"
		recovery = "repair or reset the saved app policy, then retry"
	case ComputerUseAppPolicySourceInvalidTarget:
		message = "the requested app identity is invalid and was blocked for safety"
		recovery = "retry with an exact running app name"
	}
	return agent.BusinessError(fmt.Sprintf(
		"computer_use_error: app_policy_blocked\ntarget_app: %s\npolicy_source: %s\nmessage: %s\nrecovery: %s",
		target,
		source,
		message,
		recovery,
	))
}

type daemonGUIToolBase struct {
	inner    agent.Tool
	workflow *daemonGUIWorkflow
}

func (t *daemonGUIToolBase) Info() agent.ToolInfo   { return t.inner.Info() }
func (t *daemonGUIToolBase) RequiresApproval() bool { return t.inner.RequiresApproval() }
func (t *daemonGUIToolBase) ApprovalAdmission(ctx context.Context, args string) agent.ApprovalAdmissionDecision {
	describer, ok := t.inner.(agent.GUIActionDescriber)
	if !ok {
		return agent.ApprovalAdmissionDeny
	}
	descriptor, err := describer.DescribeGUIAction(ctx, args)
	if err != nil || descriptor.Participates && descriptor.Effect != agent.GUIActionObservation && descriptor.Effect != agent.GUIActionMutation {
		return agent.ApprovalAdmissionDeny
	}
	if !descriptor.Participates {
		return agent.ApprovalAdmissionInherit
	}
	if descriptor.TargetBundleID != "" {
		if t.workflow != nil && t.workflow.appPolicy != nil {
			entry := t.workflow.appPolicy.DecisionFor(descriptor.TargetBundleID)
			if entry.Decision == ComputerUseAppPolicyBlocked {
				return agent.ApprovalAdmissionDeny
			}
			// Ask is now the ordinary default and cannot override the single
			// global Computer Use permission. Older Desktop builds may have
			// persisted explicit Ask rows; retaining those bytes for wire
			// compatibility must not resurrect a hidden per-app permission model.
		}
	}
	if descriptor.Effect == agent.GUIActionObservation {
		return agent.ApprovalAdmissionInherit
	}
	// An unresolved target remains Ask: never infer a bundle from a window title,
	// AX value, process label, or model-provided prose.
	if descriptor.TargetBundleID == "" {
		return agent.ApprovalAdmissionRequireFresh
	}
	return agent.ApprovalAdmissionInherit
}
func (t *daemonGUIToolBase) ApprovalAdmissionDenialResult(
	ctx context.Context,
	args string,
) (agent.ToolResult, bool) {
	describer, ok := t.inner.(agent.GUIActionDescriber)
	if !ok || t.workflow == nil || t.workflow.appPolicy == nil {
		return agent.ToolResult{}, false
	}
	descriptor, err := describer.DescribeGUIAction(ctx, args)
	if err != nil || descriptor.TargetBundleID == "" {
		return agent.ToolResult{}, false
	}
	entry := t.workflow.appPolicy.DecisionFor(descriptor.TargetBundleID)
	if entry.Decision != ComputerUseAppPolicyBlocked {
		return agent.ToolResult{}, false
	}
	return computerUseAppPolicyBlockedResult(descriptor, entry), true
}
func (t *daemonGUIToolBase) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	return t.workflow.runTool(ctx, t.inner, args)
}

type daemonGUIReadOnlyTool struct {
	*daemonGUIToolBase
	readOnly agent.ReadOnlyChecker
}

func (t *daemonGUIReadOnlyTool) IsReadOnlyCall(args string) bool {
	return t.readOnly.IsReadOnlyCall(args)
}

type daemonGUISafeReadOnlyTool struct {
	*daemonGUIReadOnlyTool
	safe agent.SafeChecker
}

func (t *daemonGUISafeReadOnlyTool) IsSafeArgs(args string) bool { return t.safe.IsSafeArgs(args) }

type daemonGUIConcurrencyReadOnlyTool struct {
	*daemonGUIReadOnlyTool
	concurrency agent.ConcurrencySafeChecker
}

func (t *daemonGUIConcurrencyReadOnlyTool) IsConcurrencySafeCall(args string) bool {
	return t.concurrency.IsConcurrencySafeCall(args)
}

type daemonGUISafeConcurrencyReadOnlyTool struct {
	*daemonGUISafeReadOnlyTool
	concurrency agent.ConcurrencySafeChecker
}

func (t *daemonGUISafeConcurrencyReadOnlyTool) IsConcurrencySafeCall(args string) bool {
	return t.concurrency.IsConcurrencySafeCall(args)
}

type daemonGUINativeReadOnlyTool struct {
	*daemonGUIReadOnlyTool
	native agent.NativeToolProvider
}

func (t *daemonGUINativeReadOnlyTool) NativeToolDef() *client.NativeToolDef {
	return t.native.NativeToolDef()
}

type daemonGUINativeSafeReadOnlyTool struct {
	*daemonGUISafeReadOnlyTool
	native agent.NativeToolProvider
}

func (t *daemonGUINativeSafeReadOnlyTool) NativeToolDef() *client.NativeToolDef {
	return t.native.NativeToolDef()
}

type daemonGUINativeConcurrencyReadOnlyTool struct {
	*daemonGUIConcurrencyReadOnlyTool
	native agent.NativeToolProvider
}

func (t *daemonGUINativeConcurrencyReadOnlyTool) NativeToolDef() *client.NativeToolDef {
	return t.native.NativeToolDef()
}

type daemonGUINativeSafeConcurrencyReadOnlyTool struct {
	*daemonGUISafeConcurrencyReadOnlyTool
	native agent.NativeToolProvider
}

func (t *daemonGUINativeSafeConcurrencyReadOnlyTool) NativeToolDef() *client.NativeToolDef {
	return t.native.NativeToolDef()
}

type daemonGUINativePreparingReadOnlyTool struct {
	*daemonGUINativeReadOnlyTool
}

func (t *daemonGUINativePreparingReadOnlyTool) PrepareNativeToolRequest(ctx context.Context) error {
	return prepareDaemonNativeToolRequest(ctx, t.daemonGUIToolBase, t.native)
}

type daemonGUINativePreparingSafeReadOnlyTool struct {
	*daemonGUINativeSafeReadOnlyTool
}

func (t *daemonGUINativePreparingSafeReadOnlyTool) PrepareNativeToolRequest(ctx context.Context) error {
	return prepareDaemonNativeToolRequest(ctx, t.daemonGUIToolBase, t.native)
}

type daemonGUINativePreparingConcurrencyReadOnlyTool struct {
	*daemonGUINativeConcurrencyReadOnlyTool
}

func (t *daemonGUINativePreparingConcurrencyReadOnlyTool) PrepareNativeToolRequest(ctx context.Context) error {
	return prepareDaemonNativeToolRequest(ctx, t.daemonGUIToolBase, t.native)
}

type daemonGUINativePreparingSafeConcurrencyReadOnlyTool struct {
	*daemonGUINativeSafeConcurrencyReadOnlyTool
}

func (t *daemonGUINativePreparingSafeConcurrencyReadOnlyTool) PrepareNativeToolRequest(ctx context.Context) error {
	return prepareDaemonNativeToolRequest(ctx, t.daemonGUIToolBase, t.native)
}

func prepareDaemonNativeToolRequest(
	ctx context.Context,
	base *daemonGUIToolBase,
	native agent.NativeToolProvider,
) error {
	if base == nil || base.workflow == nil {
		return fmt.Errorf("computer-use native preparation workflow is unavailable")
	}
	if _, ok := native.(agent.NativeToolRequestPreparer); !ok {
		return nil
	}
	return base.workflow.prepareNativeToolRequest(ctx, base.inner, native)
}

var daemonControlledGUIToolNames = map[string]struct{}{
	"computer_use":  {},
	"computer":      {},
	"accessibility": {},
	"applescript":   {},
	"ghostty":       {},
}

func wrapOneDaemonGUIToolV1(
	inner agent.Tool,
	workflow *daemonGUIWorkflow,
) (agent.Tool, error) {
	if inner == nil || workflow == nil {
		return nil, fmt.Errorf("daemon GUI tool wrapper is unavailable")
	}
	if _, ok := inner.(agent.GUIActionDescriber); !ok {
		return nil, fmt.Errorf(
			"GUI tool %s lacks an action descriptor",
			inner.Info().Name,
		)
	}
	base := &daemonGUIToolBase{inner: inner, workflow: workflow}
	readOnly, hasReadOnly := inner.(agent.ReadOnlyChecker)
	if !hasReadOnly {
		return base, nil
	}
	ro := &daemonGUIReadOnlyTool{daemonGUIToolBase: base, readOnly: readOnly}
	safe, hasSafe := inner.(agent.SafeChecker)
	concurrency, hasConcurrency := inner.(agent.ConcurrencySafeChecker)
	if native, ok := inner.(agent.NativeToolProvider); ok {
		_, hasPreparation := inner.(agent.NativeToolRequestPreparer)
		switch {
		case hasSafe && hasConcurrency:
			wrapped := &daemonGUINativeSafeConcurrencyReadOnlyTool{
				daemonGUISafeConcurrencyReadOnlyTool: &daemonGUISafeConcurrencyReadOnlyTool{
					daemonGUISafeReadOnlyTool: &daemonGUISafeReadOnlyTool{
						daemonGUIReadOnlyTool: ro, safe: safe,
					},
					concurrency: concurrency,
				},
				native: native,
			}
			if hasPreparation {
				return &daemonGUINativePreparingSafeConcurrencyReadOnlyTool{
					daemonGUINativeSafeConcurrencyReadOnlyTool: wrapped,
				}, nil
			}
			return wrapped, nil
		case hasSafe:
			wrapped := &daemonGUINativeSafeReadOnlyTool{
				daemonGUISafeReadOnlyTool: &daemonGUISafeReadOnlyTool{
					daemonGUIReadOnlyTool: ro, safe: safe,
				},
				native: native,
			}
			if hasPreparation {
				return &daemonGUINativePreparingSafeReadOnlyTool{
					daemonGUINativeSafeReadOnlyTool: wrapped,
				}, nil
			}
			return wrapped, nil
		case hasConcurrency:
			wrapped := &daemonGUINativeConcurrencyReadOnlyTool{
				daemonGUIConcurrencyReadOnlyTool: &daemonGUIConcurrencyReadOnlyTool{
					daemonGUIReadOnlyTool: ro, concurrency: concurrency,
				},
				native: native,
			}
			if hasPreparation {
				return &daemonGUINativePreparingConcurrencyReadOnlyTool{
					daemonGUINativeConcurrencyReadOnlyTool: wrapped,
				}, nil
			}
			return wrapped, nil
		default:
			wrapped := &daemonGUINativeReadOnlyTool{
				daemonGUIReadOnlyTool: ro,
				native:                native,
			}
			if hasPreparation {
				return &daemonGUINativePreparingReadOnlyTool{
					daemonGUINativeReadOnlyTool: wrapped,
				}, nil
			}
			return wrapped, nil
		}
	}
	switch {
	case hasSafe && hasConcurrency:
		return &daemonGUISafeConcurrencyReadOnlyTool{
			daemonGUISafeReadOnlyTool: &daemonGUISafeReadOnlyTool{
				daemonGUIReadOnlyTool: ro,
				safe:                  safe,
			},
			concurrency: concurrency,
		}, nil
	case hasSafe:
		return &daemonGUISafeReadOnlyTool{
			daemonGUIReadOnlyTool: ro,
			safe:                  safe,
		}, nil
	case hasConcurrency:
		return &daemonGUIConcurrencyReadOnlyTool{
			daemonGUIReadOnlyTool: ro,
			concurrency:           concurrency,
		}, nil
	default:
		return ro, nil
	}
}

func wrapDaemonGUITools(registry *agent.ToolRegistry, workflow *daemonGUIWorkflow) {
	if registry == nil || workflow == nil {
		return
	}
	for name := range daemonControlledGUIToolNames {
		inner, ok := registry.Get(name)
		if !ok {
			continue
		}
		wrapped, err := wrapOneDaemonGUIToolV1(inner, workflow)
		if err != nil {
			// Failing closed is safer than leaving a legacy tool name as a bypass.
			log.Printf("daemon: %v; removing it from this run", err)
			registry.Remove(name)
			continue
		}
		registry.Register(wrapped)
	}
}

func daemonGUISourceLabel(source string) string {
	source = strings.TrimSpace(source)
	if source == "" {
		return "Kocoro"
	}
	return source
}
