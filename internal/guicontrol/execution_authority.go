package guicontrol

import (
	"context"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync/atomic"
)

// executionAuthority is a process-local capability minted only after
// Coordinator.BeginAction admits an exact lease action. Its fields and
// context key remain private so strings, environment variables, and a
// zero-value ActionHandle cannot manufacture permission to execute GUI input.
type executionAuthority struct {
	seal             *executionAuthoritySeal
	leaseID          string
	actionID         string
	toolName         string
	toolUseID        string
	actionKind       string
	effect           string
	targetBundleID   string
	executionPath    string
	riskIntentID     string
	riskTargetDigest string
	active           atomic.Bool
}

type executionAuthoritySeal struct{}

type executionAuthorityClaim struct {
	authority *executionAuthority
	scope     ExecutionScope
}

type executionAuthorityContextKey struct{}

// ExecutionScope is the redacted, exact execution identity sealed into an
// admitted action. It intentionally excludes arguments, typed text, scripts,
// and AX values.
type ExecutionScope struct {
	ToolName       string
	ToolUseID      string
	ActionKind     string
	Effect         string
	TargetBundleID string
	ExecutionPath  string
	// Consequential-risk authority is deliberately process-local. Both fields
	// must be present together and are never activity/event payload fields.
	RiskIntentID     string
	RiskTargetDigest string
}

func newExecutionAuthority(request ActionRequest, actionID string) *executionAuthority {
	executionPath := ""
	if request.ExecutionPath != nil {
		executionPath = string(*request.ExecutionPath)
	}
	authority := &executionAuthority{
		seal:             &executionAuthoritySeal{},
		leaseID:          request.LeaseID,
		actionID:         actionID,
		toolName:         request.ToolName,
		toolUseID:        request.ToolUseID,
		actionKind:       request.ActionKind,
		effect:           string(request.Effect),
		targetBundleID:   request.TargetBundleID,
		executionPath:    executionPath,
		riskIntentID:     request.RiskIntentID,
		riskTargetDigest: request.RiskTargetDigest,
	}
	authority.active.Store(true)
	return authority
}

// AuthorizeExecution binds this admitted action's opaque capability to the
// exact tool and tool_use_id that is about to enter Tool.Run. A genuine live
// handle adds a claim even when the requested scope is mismatched, allowing
// the final gate to distinguish a denied daemon attempt from a direct CLI/TUI
// observation. A zero-value, revoked, or malformed handle adds nothing.
func (h ActionHandle) AuthorizeExecution(scope ExecutionScope) context.Context {
	ctx := h.Context
	if ctx == nil {
		ctx = context.Background()
	}
	authority := h.executionAuthority
	if authority == nil || authority.seal == nil || !validExecutionScope(scope) ||
		authority.leaseID != h.LeaseID || authority.actionID != h.ActionID ||
		!authority.active.Load() {
		return ctx
	}
	return context.WithValue(ctx, executionAuthorityContextKey{}, executionAuthorityClaim{
		authority: authority,
		scope:     scope,
	})
}

// ExecutionAuthorized verifies that ctx carries a still-live capability for
// this exact tool invocation. Callers cannot construct a valid claim because
// both the context key and the authority seal are private to this package.
func ExecutionAuthorized(ctx context.Context, scope ExecutionScope) bool {
	// Backstop for every revocation path. The coordinator revokes the capability
	// explicitly on Stop / Take Over / expiry, but each of those also cancels the
	// action context, so honoring cancellation here keeps the gate closed even if
	// a future cancellation path forgets to revoke.
	if ctx == nil || ctx.Err() != nil || !validExecutionScope(scope) {
		return false
	}
	claim, ok := ctx.Value(executionAuthorityContextKey{}).(executionAuthorityClaim)
	if !ok || claim.scope != scope {
		return false
	}
	authority := claim.authority
	return authority != nil && authority.seal != nil && authority.leaseID != "" &&
		authority.actionID != "" && authority.matches(scope) && authority.active.Load()
}

// ExecutionAuthorityPresent reports whether ctx carries a genuine daemon
// action claim, independently of whether its scope is still live and exact.
// The final execution gate uses this distinction to keep direct CLI/TUI reads
// available while refusing a daemon observation whose descriptor drifted
// after admission.
func ExecutionAuthorityPresent(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	claim, ok := ctx.Value(executionAuthorityContextKey{}).(executionAuthorityClaim)
	return ok && claim.authority != nil && claim.authority.seal != nil
}

func validExecutionScope(scope ExecutionScope) bool {
	return scope.ToolName != "" && scope.ToolUseID != "" && scope.ActionKind != "" && scope.Effect != "" &&
		validConsequentialRiskExecutionScope(
			scope.ToolName, scope.ActionKind, scope.Effect, scope.TargetBundleID,
			scope.ExecutionPath, scope.RiskIntentID, scope.RiskTargetDigest)
}

func (a *executionAuthority) matches(scope ExecutionScope) bool {
	return a != nil && a.toolName == scope.ToolName && a.toolUseID == scope.ToolUseID && a.actionKind == scope.ActionKind &&
		a.effect == scope.Effect && a.targetBundleID == scope.TargetBundleID &&
		a.executionPath == scope.ExecutionPath && a.riskIntentID == scope.RiskIntentID &&
		a.riskTargetDigest == scope.RiskTargetDigest
}

func executionPathString(path *ComputerUseExecutionPath) string {
	if path == nil {
		return ""
	}
	return string(*path)
}

// validConsequentialRiskExecutionScope keeps consequential grants on the two
// commit-adjacent paths that can prove exact target authority: semantic AX
// press and framed synthetic single-click. Other coordinate gestures, legacy
// tools, observations, and partially populated claims fail closed.
func validConsequentialRiskExecutionScope(
	toolName, actionKind, effect, targetBundleID, executionPath,
	intentID, targetDigest string,
) bool {
	if intentID == "" && targetDigest == "" {
		return true
	}
	if !validConsequentialRiskIdentity(intentID, targetDigest) {
		return false
	}
	if toolName != "computer_use" || effect != string(ComputerUseActionMutation) || targetBundleID == "" {
		return false
	}
	return actionKind == "press" && executionPath == string(ComputerUseExecutionAccessibility) ||
		actionKind == "click" && executionPath == string(ComputerUseExecutionSyntheticCoordinate)
}

func validConsequentialRiskIdentity(intentID, targetDigest string) bool {
	if !strings.HasPrefix(intentID, "cri_") || !strings.HasPrefix(targetDigest, "tdv1_") {
		return false
	}
	rawID, err := base64.RawURLEncoding.Strict().DecodeString(strings.TrimPrefix(intentID, "cri_"))
	if err != nil || len(rawID) != 16 {
		return false
	}
	digestText := strings.TrimPrefix(targetDigest, "tdv1_")
	if len(digestText) != 64 {
		return false
	}
	rawDigest, err := hex.DecodeString(digestText)
	return err == nil && len(rawDigest) == 32 && hex.EncodeToString(rawDigest) == digestText
}
