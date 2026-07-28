package tools

import (
	"context"
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

// guiExecutionGuarded marks production registry entries whose final Tool.Run
// seam enforces daemon-minted GUI execution authority. The marker is private
// to prevent another package from claiming a raw tool is guarded.
type guiExecutionGuarded interface {
	agent.Tool
	guiExecutionInner() agent.Tool
}

type guiExecutionGate struct {
	inner     agent.Tool
	describer agent.GUIActionDescriber
}

func (g *guiExecutionGate) Info() agent.ToolInfo          { return g.inner.Info() }
func (g *guiExecutionGate) RequiresApproval() bool        { return g.inner.RequiresApproval() }
func (g *guiExecutionGate) guiExecutionInner() agent.Tool { return g.inner }
func (g *guiExecutionGate) DescribeGUIAction(ctx context.Context, args string) (agent.GUIActionDescriptor, error) {
	return g.describer.DescribeGUIAction(ctx, args)
}

func (g *guiExecutionGate) PreflightConsequentialRiskV1(ctx context.Context, args, requestID string) (ConsequentialRiskPreflightResultV1, error) {
	preflighter, ok := g.inner.(ConsequentialRiskPreflighterV1)
	if !ok {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightNoneV1}, nil
	}
	return preflighter.PreflightConsequentialRiskV1(ctx, args, requestID)
}

func (g *guiExecutionGate) RestoreGUIActionTargetV1(
	ctx context.Context,
	descriptor agent.GUIActionDescriptor,
) error {
	restorer, ok := g.inner.(GUIActionTargetRestorerV1)
	if !ok {
		return nil
	}
	return restorer.RestoreGUIActionTargetV1(ctx, descriptor)
}

func (g *guiExecutionGate) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	// Validate required fields BEFORE classification. A malformed call cannot be
	// classified, so without this the gate would mask every missing/empty field
	// behind the generic "could not be safely classified" message — losing the
	// field name the model needs to self-correct, and for some shapes downgrading
	// a validation error to a business error, which costs the loop detector its
	// 3-strike validation fast path. Nothing executes on this path either way:
	// every gated GUI tool declares only string required fields, so the strict
	// zero-value check is exact here.
	if invalid, ok := agent.ValidateToolArguments(g.inner.Info(), args); !ok {
		return invalid, nil
	}
	descriptor, err := g.describer.DescribeGUIAction(ctx, args)
	if err != nil {
		// The gate is the last seam before the real implementation. A classifier
		// failure here must not fall through and turn malformed/future actions
		// into an uncoordinated mutation.
		return agent.ValidationError("GUI action could not be safely classified"), nil
	}
	if !descriptor.Participates {
		return g.inner.Run(ctx, args)
	}
	if descriptor.Effect != agent.GUIActionObservation && descriptor.Effect != agent.GUIActionMutation {
		return agent.BusinessError("computer-use policy denied an unclassified GUI action"), nil
	}
	invocation, ok := agent.ToolInvocationFromContext(ctx)
	riskIntentID := ""
	riskTargetDigest := ""
	if preflighter, supportsRisk := g.inner.(ConsequentialRiskPreflighterV1); supportsRisk {
		preflight, preflightErr := preflighter.PreflightConsequentialRiskV1(ctx, args, invocation.ToolUseID)
		if preflightErr != nil || preflight.Status == ConsequentialRiskPreflightBlockedV1 {
			return agent.BusinessError("computer-use consequential action was blocked by trusted preflight"), nil
		}
		riskExecution, riskErr := validateConsequentialRiskExecutionV1(ctx, preflight)
		if riskErr != nil {
			return agent.BusinessError("computer-use consequential action lacks exact local confirmation"), nil
		}
		if preflight.Status == ConsequentialRiskPreflightRequiredV1 {
			riskIntentID = riskExecution.IntentID
			riskTargetDigest = riskExecution.TargetDigest
		}
	} else if _, stray := consequentialRiskExecutionFromContextV1(ctx); stray {
		return agent.BusinessError("computer-use consequential confirmation cannot authorize this action"), nil
	}
	scope := guicontrol.ExecutionScope{
		ToolName: invocation.ToolName, ToolUseID: invocation.ToolUseID,
		ActionKind: descriptor.ActionKind, Effect: string(descriptor.Effect),
		TargetBundleID: descriptor.TargetBundleID, ExecutionPath: descriptor.ExecutionPath,
		ExecutionLane:      descriptor.ExecutionLane,
		ForegroundFallback: descriptor.ForegroundFallback,
		RiskIntentID:       riskIntentID, RiskTargetDigest: riskTargetDigest,
	}
	if descriptor.Effect == agent.GUIActionObservation && !guicontrol.ExecutionAuthorityPresent(ctx) {
		return g.inner.Run(ctx, args)
	}
	if !ok || invocation.ToolName != g.inner.Info().Name ||
		!guicontrol.ExecutionAuthorized(ctx, scope) {
		return agent.BusinessError("computer-use policy denied GUI action without exact daemon execution authority"), nil
	}
	return g.inner.Run(ctx, args)
}

type guiExecutionReadOnlyGate struct {
	*guiExecutionGate
	readOnly agent.ReadOnlyChecker
}

func (g *guiExecutionReadOnlyGate) IsReadOnlyCall(args string) bool {
	return g.readOnly.IsReadOnlyCall(args)
}

type guiExecutionSafeReadOnlyGate struct {
	*guiExecutionReadOnlyGate
	safe agent.SafeChecker
}

func (g *guiExecutionSafeReadOnlyGate) IsSafeArgs(args string) bool {
	return g.safe.IsSafeArgs(args)
}

type guiExecutionConcurrencyReadOnlyGate struct {
	*guiExecutionReadOnlyGate
	concurrency agent.ConcurrencySafeChecker
}

func (g *guiExecutionConcurrencyReadOnlyGate) IsConcurrencySafeCall(args string) bool {
	return g.concurrency.IsConcurrencySafeCall(args)
}

type guiExecutionSafeConcurrencyReadOnlyGate struct {
	*guiExecutionSafeReadOnlyGate
	concurrency agent.ConcurrencySafeChecker
}

func (g *guiExecutionSafeConcurrencyReadOnlyGate) IsConcurrencySafeCall(args string) bool {
	return g.concurrency.IsConcurrencySafeCall(args)
}

type guiExecutionNativeReadOnlyGate struct {
	*guiExecutionReadOnlyGate
	native agent.NativeToolProvider
}

func (g *guiExecutionNativeReadOnlyGate) NativeToolDef() *client.NativeToolDef {
	return g.native.NativeToolDef()
}

type guiExecutionNativeSafeReadOnlyGate struct {
	*guiExecutionSafeReadOnlyGate
	native agent.NativeToolProvider
}

func (g *guiExecutionNativeSafeReadOnlyGate) NativeToolDef() *client.NativeToolDef {
	return g.native.NativeToolDef()
}

type guiExecutionNativeConcurrencyReadOnlyGate struct {
	*guiExecutionConcurrencyReadOnlyGate
	native agent.NativeToolProvider
}

func (g *guiExecutionNativeConcurrencyReadOnlyGate) NativeToolDef() *client.NativeToolDef {
	return g.native.NativeToolDef()
}

type guiExecutionNativeSafeConcurrencyReadOnlyGate struct {
	*guiExecutionSafeConcurrencyReadOnlyGate
	native agent.NativeToolProvider
}

func (g *guiExecutionNativeSafeConcurrencyReadOnlyGate) NativeToolDef() *client.NativeToolDef {
	return g.native.NativeToolDef()
}

type guiExecutionNativePreparingReadOnlyGate struct {
	*guiExecutionNativeReadOnlyGate
	preparer agent.NativeToolRequestPreparer
}

func (g *guiExecutionNativePreparingReadOnlyGate) PrepareNativeToolRequest(ctx context.Context) error {
	return g.preparer.PrepareNativeToolRequest(ctx)
}
func (g *guiExecutionNativePreparingReadOnlyGate) DescribeNativeToolRequestPreparation(
	ctx context.Context,
) (agent.GUIActionDescriptor, error) {
	return describeGUIExecutionNativeToolRequestPreparation(ctx, g.native)
}

type guiExecutionNativePreparingSafeReadOnlyGate struct {
	*guiExecutionNativeSafeReadOnlyGate
	preparer agent.NativeToolRequestPreparer
}

func (g *guiExecutionNativePreparingSafeReadOnlyGate) PrepareNativeToolRequest(ctx context.Context) error {
	return g.preparer.PrepareNativeToolRequest(ctx)
}
func (g *guiExecutionNativePreparingSafeReadOnlyGate) DescribeNativeToolRequestPreparation(
	ctx context.Context,
) (agent.GUIActionDescriptor, error) {
	return describeGUIExecutionNativeToolRequestPreparation(ctx, g.native)
}

type guiExecutionNativePreparingConcurrencyReadOnlyGate struct {
	*guiExecutionNativeConcurrencyReadOnlyGate
	preparer agent.NativeToolRequestPreparer
}

func (g *guiExecutionNativePreparingConcurrencyReadOnlyGate) PrepareNativeToolRequest(ctx context.Context) error {
	return g.preparer.PrepareNativeToolRequest(ctx)
}
func (g *guiExecutionNativePreparingConcurrencyReadOnlyGate) DescribeNativeToolRequestPreparation(
	ctx context.Context,
) (agent.GUIActionDescriptor, error) {
	return describeGUIExecutionNativeToolRequestPreparation(ctx, g.native)
}

type guiExecutionNativePreparingSafeConcurrencyReadOnlyGate struct {
	*guiExecutionNativeSafeConcurrencyReadOnlyGate
	preparer agent.NativeToolRequestPreparer
}

func (g *guiExecutionNativePreparingSafeConcurrencyReadOnlyGate) PrepareNativeToolRequest(ctx context.Context) error {
	return g.preparer.PrepareNativeToolRequest(ctx)
}
func (g *guiExecutionNativePreparingSafeConcurrencyReadOnlyGate) DescribeNativeToolRequestPreparation(
	ctx context.Context,
) (agent.GUIActionDescriptor, error) {
	return describeGUIExecutionNativeToolRequestPreparation(ctx, g.native)
}

type nativeToolRequestPreparationDescriber interface {
	DescribeNativeToolRequestPreparation(context.Context) (agent.GUIActionDescriptor, error)
}

func describeGUIExecutionNativeToolRequestPreparation(
	ctx context.Context,
	native agent.NativeToolProvider,
) (agent.GUIActionDescriptor, error) {
	describer, ok := native.(nativeToolRequestPreparationDescriber)
	if !ok {
		return agent.GUIActionDescriptor{}, fmt.Errorf(
			"provider-native GUI tool lacks a preparation descriptor",
		)
	}
	return describer.DescribeNativeToolRequestPreparation(ctx)
}

func wrapGUIExecutionGate(tool agent.Tool) agent.Tool {
	if tool == nil {
		return nil
	}
	if _, ok := tool.(guiExecutionGuarded); ok {
		return tool
	}
	describer, ok := tool.(agent.GUIActionDescriber)
	if !ok {
		return tool
	}
	base := &guiExecutionGate{inner: tool, describer: describer}
	readOnly, hasReadOnly := tool.(agent.ReadOnlyChecker)
	if !hasReadOnly {
		return base
	}
	ro := &guiExecutionReadOnlyGate{guiExecutionGate: base, readOnly: readOnly}
	safe, hasSafe := tool.(agent.SafeChecker)
	concurrency, hasConcurrency := tool.(agent.ConcurrencySafeChecker)
	if native, ok := tool.(agent.NativeToolProvider); ok {
		preparer, hasPreparation := tool.(agent.NativeToolRequestPreparer)
		switch {
		case hasSafe && hasConcurrency:
			wrapped := &guiExecutionNativeSafeConcurrencyReadOnlyGate{
				guiExecutionSafeConcurrencyReadOnlyGate: &guiExecutionSafeConcurrencyReadOnlyGate{
					guiExecutionSafeReadOnlyGate: &guiExecutionSafeReadOnlyGate{
						guiExecutionReadOnlyGate: ro, safe: safe,
					},
					concurrency: concurrency,
				},
				native: native,
			}
			if hasPreparation {
				return &guiExecutionNativePreparingSafeConcurrencyReadOnlyGate{
					guiExecutionNativeSafeConcurrencyReadOnlyGate: wrapped,
					preparer: preparer,
				}
			}
			return wrapped
		case hasSafe:
			wrapped := &guiExecutionNativeSafeReadOnlyGate{
				guiExecutionSafeReadOnlyGate: &guiExecutionSafeReadOnlyGate{
					guiExecutionReadOnlyGate: ro, safe: safe,
				},
				native: native,
			}
			if hasPreparation {
				return &guiExecutionNativePreparingSafeReadOnlyGate{
					guiExecutionNativeSafeReadOnlyGate: wrapped,
					preparer:                           preparer,
				}
			}
			return wrapped
		case hasConcurrency:
			wrapped := &guiExecutionNativeConcurrencyReadOnlyGate{
				guiExecutionConcurrencyReadOnlyGate: &guiExecutionConcurrencyReadOnlyGate{
					guiExecutionReadOnlyGate: ro, concurrency: concurrency,
				},
				native: native,
			}
			if hasPreparation {
				return &guiExecutionNativePreparingConcurrencyReadOnlyGate{
					guiExecutionNativeConcurrencyReadOnlyGate: wrapped,
					preparer: preparer,
				}
			}
			return wrapped
		default:
			wrapped := &guiExecutionNativeReadOnlyGate{guiExecutionReadOnlyGate: ro, native: native}
			if hasPreparation {
				return &guiExecutionNativePreparingReadOnlyGate{
					guiExecutionNativeReadOnlyGate: wrapped,
					preparer:                       preparer,
				}
			}
			return wrapped
		}
	}
	switch {
	case hasSafe && hasConcurrency:
		return &guiExecutionSafeConcurrencyReadOnlyGate{
			guiExecutionSafeReadOnlyGate: &guiExecutionSafeReadOnlyGate{
				guiExecutionReadOnlyGate: ro, safe: safe,
			},
			concurrency: concurrency,
		}
	case hasSafe:
		return &guiExecutionSafeReadOnlyGate{guiExecutionReadOnlyGate: ro, safe: safe}
	case hasConcurrency:
		return &guiExecutionConcurrencyReadOnlyGate{guiExecutionReadOnlyGate: ro, concurrency: concurrency}
	default:
		return ro
	}
}

func unwrapGUIExecutionGate(tool agent.Tool) agent.Tool {
	if guarded, ok := tool.(guiExecutionGuarded); ok {
		return guarded.guiExecutionInner()
	}
	return tool
}

// guardRegisteredGUIExecution installs the final execution gate for every
// local GUIActionDescriber, including legacy names. Keeping this descriptor-
// driven means a future GUI tool cannot accidentally bypass the gate merely
// because somebody forgot to add its string name to a second list.
func guardRegisteredGUIExecution(registry *agent.ToolRegistry) {
	if registry == nil {
		return
	}
	for _, tool := range registry.All() {
		if _, ok := tool.(agent.GUIActionDescriber); ok {
			registry.Register(wrapGUIExecutionGate(tool))
		}
	}
}
