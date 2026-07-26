package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

type openAINativeComputerActionContextKeyV1 struct{}

// ContextWithOpenAINativeComputerActionV1 is a process-local route marker.
// It carries no lease or approval authority; the daemon mints it only after
// provider provenance, batch scope, lease, and per-action admission succeed.
// Its sole purpose is to keep daemon-private action names out of the generic
// computer_use surface while native execution remains bound to the guarded
// daemon runner.
func ContextWithOpenAINativeComputerActionV1(ctx context.Context) context.Context {
	return context.WithValue(ctx, openAINativeComputerActionContextKeyV1{}, true)
}

func hasOpenAINativeComputerActionV1(ctx context.Context) bool {
	authorized, _ := ctx.Value(openAINativeComputerActionContextKeyV1{}).(bool)
	return authorized
}

// OpenAIComputerActionRuntimeV1 projects one normalized OpenAI action at a
// time onto the same guarded, clone-local Accessibility-first ComputerUseTool
// used by the generic and Anthropic adapters. It never executes by itself:
// daemonGUIWorkflow must mint the lease/action capability around every plan.
type OpenAIComputerActionRuntimeV1 struct {
	public agent.Tool
	raw    *ComputerUseTool
}

type OpenAIComputerTaskAppV1 struct {
	App      string
	BundleID string
	PID      int
}

// NewOpenAIComputerActionRuntimeV1 accepts only a registry entry carrying
// Kocoro's final GUI execution gate. A raw ComputerUseTool would let a future
// caller accidentally bypass the daemon coordinator, so it is rejected even
// though focused package tests can construct the runtime directly.
func NewOpenAIComputerActionRuntimeV1(
	public agent.Tool,
) (*OpenAIComputerActionRuntimeV1, error) {
	guarded, ok := public.(guiExecutionGuarded)
	if !ok {
		return nil, fmt.Errorf("OpenAI computer runtime requires a guarded computer_use tool")
	}
	raw, ok := unwrapGUIExecutionGate(guarded).(*ComputerUseTool)
	if !ok || raw == nil || guarded.Info().Name != "computer_use" {
		return nil, fmt.Errorf("OpenAI computer runtime requires clone-local ComputerUseTool")
	}
	return &OpenAIComputerActionRuntimeV1{public: public, raw: raw}, nil
}

func (r *OpenAIComputerActionRuntimeV1) ResolveTaskAppV1(
	ctx context.Context,
	app string,
) (OpenAIComputerTaskAppV1, error) {
	app = strings.TrimSpace(app)
	if r == nil || r.raw == nil || r.raw.client == nil {
		return OpenAIComputerTaskAppV1{},
			fmt.Errorf("OpenAI computer task app resolver is unavailable")
	}
	if app == "" || !ValidAppNamePattern.MatchString(app) {
		return OpenAIComputerTaskAppV1{}, fmt.Errorf("invalid app name")
	}
	raw, err := r.raw.client.Call(
		ctx,
		"resolve_app_identity",
		map[string]any{"app_name": app},
	)
	if err != nil {
		return OpenAIComputerTaskAppV1{}, fmt.Errorf("resolve app %q: %w", app, err)
	}
	var identity struct {
		App      string `json:"app"`
		BundleID string `json:"bundle_id"`
		PID      *int   `json:"pid"`
	}
	if err := json.Unmarshal(raw, &identity); err != nil ||
		strings.TrimSpace(identity.App) == "" ||
		strings.TrimSpace(identity.BundleID) == "" {
		return OpenAIComputerTaskAppV1{},
			fmt.Errorf("resolve app %q returned invalid identity", app)
	}
	result := OpenAIComputerTaskAppV1{
		App:      strings.TrimSpace(identity.App),
		BundleID: strings.TrimSpace(identity.BundleID),
	}
	if identity.PID != nil {
		result.PID = *identity.PID
	}
	return result, nil
}

func (r *OpenAIComputerActionRuntimeV1) LaunchAndFocusTaskAppsV1(
	ctx context.Context,
	apps []OpenAIComputerTaskAppV1,
) error {
	if r == nil || r.raw == nil || r.raw.client == nil {
		return fmt.Errorf("OpenAI computer task launcher is unavailable")
	}
	if len(apps) == 0 {
		return nil
	}
	for _, app := range apps {
		if strings.TrimSpace(app.App) == "" ||
			strings.TrimSpace(app.BundleID) == "" {
			return fmt.Errorf("OpenAI computer task app identity is invalid")
		}
		if _, err := r.raw.client.Call(
			ctx,
			"launch_app",
			map[string]any{"app_name": app.App},
		); err != nil {
			return fmt.Errorf("launch app %q: %w", app.App, err)
		}
	}
	if _, err := r.raw.client.Call(
		ctx,
		"focus_app",
		map[string]any{"app_name": apps[0].App, "verify": true},
	); err != nil {
		return fmt.Errorf("focus app %q: %w", apps[0].App, err)
	}
	// The explicit task app list supersedes a Quick Panel foreground hint.
	// Clear that one-shot bootstrap target so every post-action refresh follows
	// the actual frontmost window, including an OpenAI-native app switch.
	r.raw.initialTarget = nil
	r.raw.invalidateState()
	return nil
}

func (r *OpenAIComputerActionRuntimeV1) plan(
	args computerUseArgs,
	mutation bool,
) (OpenAIComputerActionPlanV1, error) {
	if r == nil || r.public == nil || r.raw == nil {
		return OpenAIComputerActionPlanV1{},
			fmt.Errorf("OpenAI computer action runtime is unavailable")
	}
	payload, err := json.Marshal(args)
	if err != nil {
		return OpenAIComputerActionPlanV1{},
			fmt.Errorf("marshal internal OpenAI computer action")
	}
	return OpenAIComputerActionPlanV1{
		Tool: r.public, Args: string(payload), Mutation: mutation,
	}, nil
}

// PlanOpenAIComputerObservationV1 creates either an AX-only state refresh or
// the one exact final screenshot. Internal refreshes intentionally set
// include_screenshot=false so an action batch performs image capture only at
// its terminal observation boundary.
func (r *OpenAIComputerActionRuntimeV1) PlanOpenAIComputerObservationV1(
	description string,
	includeScreenshot bool,
) (OpenAIComputerActionPlanV1, error) {
	if strings.TrimSpace(description) == "" {
		return OpenAIComputerActionPlanV1{},
			fmt.Errorf("OpenAI computer observation description is required")
	}
	return r.plan(computerUseArgs{
		Action:            "get_app_state",
		Description:       description,
		IncludeScreenshot: includeScreenshot,
	}, false)
}

// PlanOpenAIComputerActionV1 translates only actions whose provider semantics
// can be preserved by today's strict Kocoro primitives. Unsupported shapes
// fail before daemon action admission; they never degrade to global input.
func (r *OpenAIComputerActionRuntimeV1) PlanOpenAIComputerActionV1(
	ctx context.Context,
	action OpenAIComputerActionV1,
) (OpenAIComputerActionPlanV1, error) {
	if r == nil || r.public == nil || r.raw == nil {
		return OpenAIComputerActionPlanV1{},
			fmt.Errorf("OpenAI computer action runtime is unavailable")
	}
	description := "OpenAI native " + action.Type
	toInt := func(value int) *computerUseInt {
		converted := computerUseInt(value)
		return &converted
	}

	switch action.Type {
	case OpenAIComputerActionScreenshotV1:
		return r.PlanOpenAIComputerObservationV1(
			"OpenAI native screenshot checkpoint",
			false,
		)
	case OpenAIComputerActionWaitV1:
		return r.plan(computerUseArgs{
			Action: "wait", Description: description, Timeout: 1,
		}, false)
	}

	// Every mutating provider action must remain bound to a typed AX snapshot.
	// Coordinate paths additionally require the immutable provider image/frame.
	if r.raw.snapshot == nil || !r.raw.snapshot.typed ||
		r.raw.snapshot.id == "" || r.raw.snapshot.bundleID == "" ||
		r.raw.snapshot.windowID == nil || *r.raw.snapshot.windowID <= 0 {
		return OpenAIComputerActionPlanV1{},
			fmt.Errorf("OpenAI computer action requires a fresh typed target observation")
	}
	stateID := r.raw.snapshot.id
	args := computerUseArgs{StateID: stateID, Description: description}
	anthropicHelpers := NewAnthropicComputerAdapter(r.raw, 1, 1)

	switch action.Type {
	case OpenAIComputerActionClickV1, OpenAIComputerActionDoubleClickV1:
		if action.X == nil || action.Y == nil {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer click coordinates are unavailable")
		}
		if _, err := anthropicHelpers.actionableObservation(ctx); err != nil {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer coordinate authority is unavailable")
		}
		args.Action = "click"
		args.X, args.Y = toInt(*action.X), toInt(*action.Y)
		args.Button = action.Button
		args.Clicks = 1
		if action.Type == OpenAIComputerActionDoubleClickV1 {
			args.Button = "left"
			args.Clicks = 2
		}
		args.Modifiers = append([]string(nil), action.Keys...)
		return r.plan(args, true)

	case OpenAIComputerActionMoveV1:
		if action.X == nil || action.Y == nil {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer move coordinates are unavailable")
		}
		if _, err := anthropicHelpers.actionableObservation(ctx); err != nil {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer coordinate authority is unavailable")
		}
		args.Action = "move"
		args.X, args.Y = toInt(*action.X), toInt(*action.Y)
		args.Modifiers = append([]string(nil), action.Keys...)
		return r.plan(args, true)

	case OpenAIComputerActionDragV1:
		if len(action.Path) < 2 ||
			len(action.Path) > coordinateDragMaximumWaypointsV1 {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf(
					"OpenAI computer drag requires 2..%d path points",
					coordinateDragMaximumWaypointsV1,
				)
		}
		if _, err := anthropicHelpers.actionableObservation(ctx); err != nil {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer coordinate authority is unavailable")
		}
		args.Action = "drag"
		args.Modifiers = append([]string(nil), action.Keys...)
		args.Path = make([]computerUsePoint, 0, len(action.Path))
		for index, point := range action.Path {
			if index > 0 && point == action.Path[index-1] {
				return OpenAIComputerActionPlanV1{},
					fmt.Errorf("OpenAI computer drag adjacent path points must be distinct")
			}
			args.Path = append(args.Path, computerUsePoint{
				X: computerUseInt(point.X),
				Y: computerUseInt(point.Y),
			})
		}
		return r.plan(args, true)

	case OpenAIComputerActionTypeTextV1:
		ref, err := anthropicHelpers.uniqueFocusedRef()
		if err != nil && (r.raw.coordinateFocus == nil ||
			r.raw.coordinateFocus.stateID != stateID) {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer type target is unavailable")
		}
		text := action.Text
		args.Action, args.Ref, args.Text = "type", ref, &text
		return r.plan(args, true)

	case OpenAIComputerActionKeypressV1:
		modifiers, keys, err := openAIComputerKeySequenceV1(action.Keys)
		if err != nil {
			return OpenAIComputerActionPlanV1{}, err
		}
		args.Action = "keypress"
		args.Modifiers = modifiers
		args.KeySequence = keys
		return r.plan(args, true)

	case OpenAIComputerActionScrollV1:
		if action.X == nil || action.Y == nil ||
			action.ScrollX == nil || action.ScrollY == nil {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer pixel scroll fields are unavailable")
		}
		if !coordinatePixelScrollProviderDeltasV1(
			int64(*action.ScrollX), int64(*action.ScrollY)) {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer pixel scroll deltas are invalid")
		}
		if _, err := anthropicHelpers.actionableObservation(ctx); err != nil {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer coordinate authority is unavailable")
		}
		args.Action = "pixel_scroll"
		args.X, args.Y = toInt(*action.X), toInt(*action.Y)
		args.ScrollX, args.ScrollY = toInt(*action.ScrollX), toInt(*action.ScrollY)
		args.Modifiers = append([]string(nil), action.Keys...)
		return r.plan(args, true)

	default:
		return OpenAIComputerActionPlanV1{},
			fmt.Errorf("OpenAI computer action %q is not safely supported", action.Type)
	}
}

func openAIComputerKeySequenceV1(keys []string) ([]string, []string, error) {
	if len(keys) == 0 {
		return nil, nil, fmt.Errorf("OpenAI computer keypress keys are unavailable")
	}
	modifiers := make([]string, 0, len(keys))
	ordinary := make([]string, 0, len(keys))
	for _, key := range keys {
		switch key {
		case "command", "control", "option", "shift":
			modifiers = append(modifiers, key)
		default:
			ordinary = append(ordinary, key)
		}
	}
	if len(ordinary) == 0 {
		return nil, nil, fmt.Errorf(
			"OpenAI computer keypress requires at least one non-modifier key")
	}
	return modifiers, ordinary, nil
}
