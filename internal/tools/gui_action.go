package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

const ghosttyBundleID = "com.mitchellh.ghostty"

type guiAXTarget struct {
	BundleID string
	AppName  string
}

// GUIActionTargetRestorerV1 restores a previously observed app only after the
// daemon has minted exact execution authority for an admitted mutation.
type GUIActionTargetRestorerV1 interface {
	RestoreGUIActionTargetV1(context.Context, agent.GUIActionDescriptor) error
}

// resolveGUIAXTarget performs a narrow, read-only AX lookup used only for
// target-policy admission. Tool.Run still performs its own authoritative
// validation and action; this lookup never creates refs or coordinate state.
func resolveGUIAXTarget(ctx context.Context, client axCallClient, appName string, fallbackPID int) (guiAXTarget, error) {
	if client == nil {
		return guiAXTarget{}, fmt.Errorf("AX target resolver is unavailable")
	}
	pid := fallbackPID
	if appName != "" {
		if !ValidAppNamePattern.MatchString(appName) {
			return guiAXTarget{}, fmt.Errorf("invalid app name")
		}
		raw, err := client.Call(ctx, "resolve_pid", map[string]any{"app_name": appName})
		if err != nil {
			return guiAXTarget{}, err
		}
		var resolved struct {
			PID int `json:"pid"`
		}
		if json.Unmarshal(raw, &resolved) != nil || resolved.PID <= 0 {
			return guiAXTarget{}, fmt.Errorf("AX target resolver returned no pid")
		}
		pid = resolved.PID
	}
	params := map[string]any{"filter": "interactive", "semantic_budget": 1}
	if pid > 0 {
		params["pid"] = pid
	}
	raw, err := client.Call(ctx, "read_tree", params)
	if err != nil {
		raw, err = client.Call(ctx, "read_window_target", params)
		if err != nil {
			return guiAXTarget{}, err
		}
	}
	var tree struct {
		App      string `json:"app"`
		AppName  string `json:"app_name"`
		BundleID string `json:"bundle_id"`
	}
	if json.Unmarshal(raw, &tree) != nil || strings.TrimSpace(tree.BundleID) == "" {
		return guiAXTarget{}, fmt.Errorf("AX target resolver returned no bundle")
	}
	name := strings.TrimSpace(tree.AppName)
	if name == "" {
		name = strings.TrimSpace(tree.App)
	}
	return guiAXTarget{BundleID: strings.TrimSpace(tree.BundleID), AppName: name}, nil
}

func guiDescriptor(action string, effect agent.GUIActionEffect, path string) agent.GUIActionDescriptor {
	phase := action
	if phase == "" {
		phase = "unknown"
	}
	return agent.GUIActionDescriptor{
		Participates:  true,
		ActionKind:    phase,
		Effect:        effect,
		ExecutionPath: path,
	}
}

func attachGUITarget(descriptor agent.GUIActionDescriptor, target guiAXTarget) agent.GUIActionDescriptor {
	descriptor.TargetBundleID = target.BundleID
	descriptor.TargetAppName = target.AppName
	return descriptor
}

func validComputerUseAction(action string) bool {
	switch action {
	case "get_app_state", "click", "press", "get_value",
		"scroll", "pixel_scroll", "type", "hotkey", "keypress", "move", "drag",
		"select_text", "wait", "screenshot":
		return true
	default:
		return false
	}
}

func computerUseExecutionPresentationV1(
	ctx context.Context,
	args computerUseArgs,
) (string, bool, error) {
	lane := strings.TrimSpace(args.ExecutionLane)
	if lane == "" {
		if args.ForegroundFallback {
			return "", false, fmt.Errorf(
				"foreground fallback requires an explicit foreground execution lane",
			)
		}
		return string(guicontrol.ComputerUseExecutionForeground), false, nil
	}
	if !hasOpenAINativeComputerActionV1(ctx) {
		return "", false, fmt.Errorf(
			"execution lane is restricted to an admitted OpenAI native computer action",
		)
	}
	switch lane {
	case string(guicontrol.ComputerUseExecutionForeground):
		return lane, args.ForegroundFallback, nil
	case string(guicontrol.ComputerUseExecutionBackgroundSemantic):
		if args.ForegroundFallback {
			return "", false, fmt.Errorf(
				"background semantic execution cannot claim foreground fallback",
			)
		}
		switch args.Action {
		case "get_app_state", "screenshot", "get_value", "wait", "press":
			return lane, false, nil
		default:
			return "", false, fmt.Errorf(
				"action %q is not supported by background semantic execution",
				args.Action,
			)
		}
	case string(guicontrol.ComputerUseExecutionBackgroundKeyboard):
		if args.ForegroundFallback {
			return "", false, fmt.Errorf(
				"background keyboard execution cannot claim foreground fallback",
			)
		}
		switch args.Action {
		case "type", "keypress":
			return lane, false, nil
		default:
			return "", false, fmt.Errorf(
				"action %q is not supported by background keyboard execution",
				args.Action,
			)
		}
	default:
		return "", false, fmt.Errorf("invalid computer-use execution lane")
	}
}

func (t *ComputerUseTool) DescribeGUIAction(ctx context.Context, argsJSON string) (agent.GUIActionDescriptor, error) {
	var args computerUseArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	if !validComputerUseAction(args.Action) {
		if computerUseTemporarilyUnavailableMutation(args.Action) {
			return agent.GUIActionDescriptor{}, fmt.Errorf("computer_use action %q is temporarily unavailable", args.Action)
		}
		return agent.GUIActionDescriptor{}, fmt.Errorf("invalid computer_use action")
	}
	if args.Action == "pixel_scroll" && !hasOpenAINativeComputerActionV1(ctx) {
		return agent.GUIActionDescriptor{}, fmt.Errorf(
			"pixel_scroll is restricted to an admitted OpenAI native computer action")
	}
	executionLane, foregroundFallback, err :=
		computerUseExecutionPresentationV1(ctx, args)
	if err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	// A pure delay has no GUI target. Likewise, an unattended first
	// observation without an explicit app is intentionally classified as
	// non-participating so admission does not resolve the frontmost app merely
	// to return an actionable validation result from Tool.Run.
	if args.Action == "wait" && strings.TrimSpace(args.Condition) == "" {
		return agent.GUIActionDescriptor{}, nil
	}
	if t.requiresExplicitFirstTargetV1(ctx, args) {
		return agent.GUIActionDescriptor{}, nil
	}
	effect := agent.GUIActionMutation
	if computerUseObservationAction(args.Action) {
		effect = agent.GUIActionObservation
	}
	path := "accessibility"
	if (args.Action == "click" && args.Ref == "") || args.Action == "move" ||
		args.Action == "drag" || args.Action == "pixel_scroll" {
		path = "synthetic_coordinate"
	}
	descriptor := guiDescriptor(args.Action, effect, path)
	descriptor.ExecutionLane = executionLane
	descriptor.ForegroundFallback = foregroundFallback

	var target guiAXTarget
	pid := 0
	usesInitialTarget := false
	if t.snapshot != nil {
		target = guiAXTarget{BundleID: t.snapshot.bundleID, AppName: t.snapshot.app}
		pid = t.snapshot.pid
	} else if t.coordinateFocus != nil {
		target = guiAXTarget{
			BundleID: t.coordinateFocus.bundleID,
			AppName:  t.coordinateFocus.app,
		}
		pid = t.coordinateFocus.pid
	} else if args.App == "" && t.initialTarget != nil {
		pid = t.initialTarget.PID
		usesInitialTarget = true
	}
	// Explicit app-changing and app-observation actions must resolve their own
	// target rather than inheriting a prior snapshot from another app.
	resolveApp := ""
	if args.App != "" || t.snapshot == nil {
		resolveApp = args.App
		if args.App != "" {
			pid = 0
		}
	}
	if target.BundleID == "" || args.App != "" && (args.Action == "get_app_state" || args.Action == "wait") {
		resolved, err := resolveGUIAXTarget(ctx, t.client, resolveApp, pid)
		if err != nil {
			if usesInitialTarget {
				return agent.GUIActionDescriptor{}, fmt.Errorf("resolve initial computer-use target: %w", err)
			}
		} else {
			if usesInitialTarget && resolved.BundleID != t.initialTarget.BundleID {
				return agent.GUIActionDescriptor{}, fmt.Errorf("initial computer-use target identity changed")
			}
			target = resolved
		}
	}
	if target.AppName == "" {
		target.AppName = args.App
	}
	// Type/hotkey/scroll may claim the frozen observed bundle only when
	// Tool.Run can enforce that same state + unique window through the dedicated
	// strict RPC immediately before event commit.
	targetBoundInputInvalid := false
	if args.Action == "hotkey" || args.Action == "keypress" {
		targetBoundInputInvalid = t.snapshot == nil || args.StateID == "" ||
			args.StateID != t.snapshot.id || t.snapshot.bundleID == "" ||
			t.snapshot.windowID == nil || *t.snapshot.windowID <= 0
	}
	if args.Action == "type" {
		if args.Ref == "" {
			focus := t.coordinateFocus
			targetBoundInputInvalid = focus == nil || args.StateID == "" ||
				focus != nil && args.StateID != focus.stateID
		} else {
			entry, exists := t.refs[args.Ref]
			targetBoundInputInvalid = t.snapshot == nil || args.StateID == "" ||
				args.StateID != t.snapshot.id || t.snapshot.bundleID == "" ||
				t.snapshot.windowID == nil || *t.snapshot.windowID <= 0 ||
				!exists || entry.path == "" || entry.role == "" ||
				entry.fingerprint == ""
		}
	}
	if args.Action == "hotkey" && args.Ref != "" {
		targetBoundInputInvalid = true
	}
	targetBoundScrollInvalid := false
	if args.Action == "scroll" {
		_, _, _, validDelta := computerUseSemanticScrollDeltaV1(int(args.DX), int(args.DY))
		entry, exists := t.refs[args.Ref]
		targetBoundScrollInvalid = !validDelta || t.snapshot == nil || !t.snapshot.typed ||
			args.StateID == "" || args.StateID != t.snapshot.id ||
			t.snapshot.bundleID == "" || t.snapshot.bundleID != strings.TrimSpace(t.snapshot.bundleID) ||
			t.snapshot.windowID == nil || *t.snapshot.windowID <= 0 ||
			uint64(*t.snapshot.windowID) > uint64(^uint32(0)) || args.Ref == "" || !exists ||
			entry.pid != t.snapshot.pid || entry.path == "" || entry.role == "" ||
			entry.fingerprint == ""
	}
	targetBoundPixelScrollInvalid := false
	if args.Action == "pixel_scroll" {
		validDelta := false
		if args.ScrollX != nil && args.ScrollY != nil {
			validDelta = coordinatePixelScrollProviderDeltasV1(
				int64(*args.ScrollX), int64(*args.ScrollY))
		}
		targetBoundPixelScrollInvalid = !validDelta || args.X == nil || args.Y == nil ||
			t.snapshot == nil || !t.snapshot.typed || args.StateID == "" ||
			args.StateID != t.snapshot.id || t.snapshot.bundleID == "" ||
			t.snapshot.bundleID != strings.TrimSpace(t.snapshot.bundleID) ||
			t.snapshot.windowID == nil || *t.snapshot.windowID <= 0 ||
			uint64(*t.snapshot.windowID) > uint64(^uint32(0)) ||
			t.coordinateArtifact == nil ||
			t.coordinateArtifact.Frame().StateID != args.StateID
	}
	if targetBoundScrollInvalid || targetBoundInputInvalid || targetBoundPixelScrollInvalid {
		target.BundleID = ""
	}
	return attachGUITarget(descriptor, target), nil
}

func (t *ComputerUseTool) RestoreGUIActionTargetV1(
	ctx context.Context,
	descriptor agent.GUIActionDescriptor,
) error {
	if !guicontrol.ExecutionAuthorityPresent(ctx) {
		return fmt.Errorf("computer-use target restore lacks daemon execution authority")
	}
	if !descriptor.Participates || descriptor.Effect != agent.GUIActionMutation {
		return fmt.Errorf("computer-use target restore requires an admitted mutation")
	}
	orderedNativeAction := hasOpenAINativeComputerActionV1(ctx)
	foregroundFallbackTransition :=
		orderedNativeAction &&
			descriptor.ForegroundFallback &&
			t != nil &&
			t.foregroundFallbackRestorePending
	if orderedNativeAction && !foregroundFallbackTransition {
		// The runtime already activated the first foreground target. Later
		// ordered actions stay bound to their current observation; re-focusing
		// here would collapse popovers, sheets, file panels, or cross-app state.
		return nil
	}
	if descriptor.ActionKind == "type" && t != nil &&
		t.coordinateFocus != nil && !foregroundFallbackTransition {
		if descriptor.TargetBundleID == t.coordinateFocus.bundleID &&
			descriptor.TargetAppName == t.coordinateFocus.app {
			// A coordinate-focused type must use the focus left by the verified
			// click. Do not reactivate the app here; the helper will fail closed
			// unless that exact process/window authority is still live.
			return nil
		}
		return fmt.Errorf("computer-use coordinate focus does not match the admitted target")
	}
	if t == nil || t.client == nil || t.snapshot == nil {
		return fmt.Errorf("computer-use target restore has no observed target")
	}
	if descriptor.TargetBundleID == "" || descriptor.TargetBundleID != t.snapshot.bundleID ||
		descriptor.TargetAppName == "" || descriptor.TargetAppName != t.snapshot.app {
		return fmt.Errorf("computer-use target restore identity does not match the observation")
	}
	params := map[string]any{
		"app_name":  t.snapshot.app,
		"verify":    true,
		"pid":       t.snapshot.pid,
		"bundle_id": t.snapshot.bundleID,
	}
	if window := strings.TrimSpace(t.snapshot.window); window != "" {
		params["window_title"] = window
	}
	if _, err := t.client.Call(ctx, "focus", params); err != nil {
		return fmt.Errorf("restore observed computer-use target: %w", err)
	}
	if foregroundFallbackTransition {
		t.foregroundFallbackRestorePending = false
	}
	return nil
}

func validAccessibilityAction(action string) bool {
	switch action {
	case "read_tree", "click", "press", "set_value", "get_value", "find", "scroll", "annotate":
		return true
	default:
		return false
	}
}

func (t *AccessibilityTool) DescribeGUIAction(ctx context.Context, argsJSON string) (agent.GUIActionDescriptor, error) {
	var args accessibilityArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	if !validAccessibilityAction(args.Action) {
		return agent.GUIActionDescriptor{}, fmt.Errorf("invalid accessibility action")
	}
	effect := agent.GUIActionMutation
	if t.IsReadOnlyCall(argsJSON) {
		effect = agent.GUIActionObservation
	}
	descriptor := guiDescriptor(args.Action, effect, "accessibility")
	target := guiAXTarget{BundleID: t.lastBundleID, AppName: t.lastAppName}
	pid := t.lastPID
	if args.Ref != "" {
		if entry, ok := t.refs[args.Ref]; ok {
			pid = entry.pid
		}
	}
	if args.App != "" || target.BundleID == "" {
		if resolved, err := resolveGUIAXTarget(ctx, t.client, args.App, pid); err == nil {
			target = resolved
		}
	}
	if target.AppName == "" {
		target.AppName = args.App
	}
	// Legacy scroll validates an AX ref, then emits a process-global scroll
	// event. Until execution is atomically bound to the resolved app, leave the
	// bundle empty so daemon policy rejects the action before Tool.Run.
	if args.Action == "scroll" {
		target.BundleID = ""
	}
	return attachGUITarget(descriptor, target), nil
}

func (t *ComputerTool) DescribeGUIAction(ctx context.Context, argsJSON string) (agent.GUIActionDescriptor, error) {
	var args computerArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	if args.Action == "" {
		return agent.GUIActionDescriptor{}, fmt.Errorf("missing computer action")
	}
	normalizeArgs(&args)
	if args.Action != "screenshot" && args.Action != "click" && args.Action != "type" && args.Action != "hotkey" && args.Action != "move" {
		return agent.GUIActionDescriptor{}, fmt.Errorf("invalid computer action")
	}
	effect := agent.GUIActionMutation
	path := "synthetic_coordinate"
	if args.Action == "screenshot" {
		effect = agent.GUIActionObservation
		path = ""
	}
	descriptor := guiDescriptor(args.Action, effect, path)
	// Every legacy mutation uses global input and has neither immutable
	// frame/window authority nor an atomic target binding. Leave the target
	// empty so daemon policy denies it during the compatibility window. The
	// observation-only screenshot can still resolve the exact frontmost app.
	if effect == agent.GUIActionMutation {
		return descriptor, nil
	}
	if target, err := resolveGUIAXTarget(ctx, t.client, "", 0); err == nil {
		descriptor = attachGUITarget(descriptor, target)
	}
	return descriptor, nil
}

func (t *AppleScriptTool) DescribeGUIAction(_ context.Context, argsJSON string) (agent.GUIActionDescriptor, error) {
	var args appleScriptArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	if strings.TrimSpace(args.Script) == "" {
		return agent.GUIActionDescriptor{}, fmt.Errorf("missing AppleScript")
	}
	// AppleScript is intentionally unscoped: one script can activate and mutate
	// multiple apps. The daemon coordinator therefore rejects it before Run
	// rather than falsely attributing the mutation to a guessed frontmost app.
	return guiDescriptor("execute_script", agent.GUIActionMutation, ""), nil
}

func (t *GhosttyTool) DescribeGUIAction(_ context.Context, argsJSON string) (agent.GUIActionDescriptor, error) {
	var args ghosttyArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	if args.Action == "list_tabs" {
		return agent.GUIActionDescriptor{Participates: false, ActionKind: "list_tabs", Effect: agent.GUIActionObservation}, nil
	}
	if args.Action != "new_tab" && args.Action != "new_split" && args.Action != "send_input" {
		return agent.GUIActionDescriptor{}, fmt.Errorf("invalid Ghostty action")
	}
	descriptor := guiDescriptor(args.Action, agent.GUIActionMutation, "")
	descriptor.TargetBundleID = ghosttyBundleID
	descriptor.TargetAppName = "Ghostty"
	return descriptor, nil
}

var _ agent.GUIActionDescriber = (*ComputerUseTool)(nil)
var _ agent.GUIActionDescriber = (*AccessibilityTool)(nil)
var _ agent.GUIActionDescriber = (*ComputerTool)(nil)
var _ agent.GUIActionDescriber = (*AppleScriptTool)(nil)
var _ agent.GUIActionDescriber = (*GhosttyTool)(nil)
