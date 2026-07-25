package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// axOnlyComputerUseTool keeps the standard computer_use function identity
// while removing every path that can emit or depend on a screenshot. It is
// used when Cloud attests function tools but cannot round-trip images inside a
// tool result (currently OpenAI Chat Completions in Phase 1).
type axOnlyComputerUseTool struct {
	inner agent.Tool
}

func (t *axOnlyComputerUseTool) Info() agent.ToolInfo {
	info := t.inner.Info()
	info.Description = "Observe and operate native macOS apps through Accessibility only. " +
		"Start with get_app_state, then use its state_id and element refs. " +
		"Screenshots and coordinate actions are unavailable on this model route. " +
		"Use browser tools for web-page DOM interactions." + agent.DescriptionGuidance

	properties, _ := info.Parameters["properties"].(map[string]any)
	cloned := make(map[string]any, len(properties))
	for name, value := range properties {
		cloned[name] = value
	}
	for _, name := range []string{
		"x", "y", "start_x", "start_y", "end_x", "end_y",
		"duration_ms", "button", "clicks", "include_screenshot",
	} {
		delete(cloned, name)
	}
	cloned["action"] = map[string]any{
		"type": "string",
		"description": "Action: get_app_state, click, press, get_value, scroll, type, hotkey, select_text, wait. " +
			"click requires an Accessibility ref; visual and coordinate actions are unavailable.",
	}
	params := make(map[string]any, len(info.Parameters))
	for name, value := range info.Parameters {
		params[name] = value
	}
	params["properties"] = cloned
	info.Parameters = params
	return info
}

func (t *axOnlyComputerUseTool) RequiresApproval() bool {
	return t.inner.RequiresApproval()
}

func validateAXOnlyComputerUseArgs(argsJSON string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal([]byte(argsJSON), &raw); err != nil {
		return fmt.Errorf("invalid arguments: %w", err)
	}
	var action string
	if err := json.Unmarshal(raw["action"], &action); err != nil || strings.TrimSpace(action) == "" {
		return fmt.Errorf("AX-only computer_use requires action")
	}
	switch action {
	case "screenshot", "move", "drag":
		return fmt.Errorf("AX-only computer_use does not support %s", action)
	}
	for _, coordinate := range []string{
		"x", "y", "start_x", "start_y", "end_x", "end_y",
	} {
		if _, present := raw[coordinate]; present {
			return fmt.Errorf("AX-only computer_use does not accept %s", coordinate)
		}
	}
	if encoded, present := raw["include_screenshot"]; present {
		var include bool
		if err := json.Unmarshal(encoded, &include); err != nil {
			return fmt.Errorf("include_screenshot must be boolean")
		}
		if include {
			return fmt.Errorf("AX-only computer_use cannot include a screenshot")
		}
	}
	if action == "click" {
		var ref string
		if err := json.Unmarshal(raw["ref"], &ref); err != nil || strings.TrimSpace(ref) == "" {
			return fmt.Errorf("AX-only computer_use click requires an Accessibility ref")
		}
	}
	return nil
}

func (t *axOnlyComputerUseTool) Run(ctx context.Context, args string) (agent.ToolResult, error) {
	if err := validateAXOnlyComputerUseArgs(args); err != nil {
		return agent.ValidationError(err.Error()), nil
	}
	result, err := t.inner.Run(ctx, args)
	if len(result.Images) == 0 {
		return result, err
	}
	failure := agent.BusinessError("AX-only computer_use blocked an unexpected screenshot result")
	failure.GUIOutcome = result.GUIOutcome
	return failure, err
}

func (t *axOnlyComputerUseTool) DescribeGUIAction(
	ctx context.Context,
	args string,
) (agent.GUIActionDescriptor, error) {
	if err := validateAXOnlyComputerUseArgs(args); err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	describer, ok := t.inner.(agent.GUIActionDescriber)
	if !ok {
		return agent.GUIActionDescriptor{}, fmt.Errorf("computer_use lacks GUI action descriptor")
	}
	return describer.DescribeGUIAction(ctx, args)
}

func (t *axOnlyComputerUseTool) IsSafeArgs(args string) bool {
	if validateAXOnlyComputerUseArgs(args) != nil {
		return false
	}
	checker, ok := t.inner.(agent.SafeChecker)
	return ok && checker.IsSafeArgs(args)
}

func (t *axOnlyComputerUseTool) IsReadOnlyCall(args string) bool {
	if validateAXOnlyComputerUseArgs(args) != nil {
		return false
	}
	checker, ok := t.inner.(agent.ReadOnlyChecker)
	return ok && checker.IsReadOnlyCall(args)
}

func (t *axOnlyComputerUseTool) IsConcurrencySafeCall(args string) bool {
	if validateAXOnlyComputerUseArgs(args) != nil {
		return false
	}
	checker, ok := t.inner.(agent.ConcurrencySafeChecker)
	return ok && checker.IsConcurrencySafeCall(args)
}

func (t *axOnlyComputerUseTool) PreflightConsequentialRiskV1(
	ctx context.Context,
	args string,
	requestID string,
) (ConsequentialRiskPreflightResultV1, error) {
	if err := validateAXOnlyComputerUseArgs(args); err != nil {
		return ConsequentialRiskPreflightResultV1{}, err
	}
	preflighter, ok := t.inner.(ConsequentialRiskPreflighterV1)
	if !ok {
		return ConsequentialRiskPreflightResultV1{
			Status: ConsequentialRiskPreflightNoneV1,
		}, nil
	}
	return preflighter.PreflightConsequentialRiskV1(ctx, args, requestID)
}

func (t *axOnlyComputerUseTool) RestoreGUIActionTargetV1(
	ctx context.Context,
	descriptor agent.GUIActionDescriptor,
) error {
	restorer, ok := t.inner.(GUIActionTargetRestorerV1)
	if !ok {
		return nil
	}
	return restorer.RestoreGUIActionTargetV1(ctx, descriptor)
}

var _ agent.Tool = (*axOnlyComputerUseTool)(nil)
var _ agent.GUIActionDescriber = (*axOnlyComputerUseTool)(nil)
var _ agent.SafeChecker = (*axOnlyComputerUseTool)(nil)
var _ agent.ReadOnlyChecker = (*axOnlyComputerUseTool)(nil)
var _ agent.ConcurrencySafeChecker = (*axOnlyComputerUseTool)(nil)
var _ ConsequentialRiskPreflighterV1 = (*axOnlyComputerUseTool)(nil)
var _ GUIActionTargetRestorerV1 = (*axOnlyComputerUseTool)(nil)
