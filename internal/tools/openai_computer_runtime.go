package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
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

// IsOpenAINativeComputerActionV1 lets the daemon coordinator preserve the
// provider-visible screenshot across one ordered Responses action batch.
func IsOpenAINativeComputerActionV1(ctx context.Context) bool {
	return hasOpenAINativeComputerActionV1(ctx)
}

// OpenAIComputerActionRuntimeV1 projects one normalized OpenAI action at a
// time onto the same guarded, clone-local Accessibility-first ComputerUseTool
// used by the generic and Anthropic adapters. It never executes by itself:
// daemonGUIWorkflow must mint the lease/action capability around every plan.
type OpenAIComputerActionRuntimeV1 struct {
	public agent.Tool
	raw    *ComputerUseTool

	// Kocoro-owned hidden GUI processes are not user task targets. The
	// selection contract is app-agnostic; this provider supplies the current
	// process identities owned by internal automation services.
	taskAppExcludedPIDs func() []int

	executionLane      OpenAIComputerExecutionLaneV1
	foregroundFallback bool
	backgroundTarget   *OpenAIComputerTaskAppV1
	backgroundRequired bool
}

type OpenAIComputerExecutionLaneV1 string

const (
	OpenAIComputerExecutionForegroundV1         OpenAIComputerExecutionLaneV1 = "foreground"
	OpenAIComputerExecutionBackgroundSemanticV1 OpenAIComputerExecutionLaneV1 = "background_semantic"
	OpenAIComputerExecutionBackgroundKeyboardV1 OpenAIComputerExecutionLaneV1 = "background_keyboard"
)

type OpenAIComputerTaskAppV1 struct {
	App        string
	BundleID   string
	PID        int
	LaunchDate string
}

type openAIComputerBackgroundBindingV1 struct {
	target                       OpenAIComputerTaskAppV1
	preservedFrontmostPID        int
	preservedFrontmostBundleID   string
	preservedFrontmostLaunchDate string
}

type OpenAIComputerTaskPreparationOptionsV1 struct {
	RequireBackground bool
}

type OpenAIComputerActionPlanErrorV1 struct {
	FailureCode string
	Detail      string
}

func (e *OpenAIComputerActionPlanErrorV1) Error() string {
	if e == nil {
		return "OpenAI computer action plan failed"
	}
	return e.Detail
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
	return &OpenAIComputerActionRuntimeV1{
		public:              public,
		raw:                 raw,
		taskAppExcludedPIDs: managedComputerUseTaskAppPIDsV1,
		executionLane:       OpenAIComputerExecutionForegroundV1,
	}, nil
}

func managedComputerUseTaskAppPIDsV1() []int {
	pid, err := strconv.Atoi(strings.TrimSpace(mcp.CDPChromePID()))
	if err != nil || pid <= 0 {
		return nil
	}
	return []int{pid}
}

func (r *OpenAIComputerActionRuntimeV1) excludedTaskAppPIDsV1() []int {
	if r == nil || r.taskAppExcludedPIDs == nil {
		return nil
	}
	seen := make(map[int]struct{})
	result := make([]int, 0)
	for _, pid := range r.taskAppExcludedPIDs() {
		if pid <= 0 {
			continue
		}
		if _, duplicate := seen[pid]; duplicate {
			continue
		}
		seen[pid] = struct{}{}
		result = append(result, pid)
	}
	return result
}

func openAIComputerTaskAppParamsV1(
	app OpenAIComputerTaskAppV1,
	excludedPIDs []int,
) map[string]any {
	params := map[string]any{
		"app_name":  app.App,
		"bundle_id": app.BundleID,
	}
	if app.PID > 0 {
		params["pid"] = app.PID
	}
	if len(excludedPIDs) > 0 {
		params["excluded_pids"] = excludedPIDs
	}
	return params
}

func decodeOpenAIComputerTaskAppIdentityV1(
	raw json.RawMessage,
) (OpenAIComputerTaskAppV1, error) {
	var identity struct {
		App        string `json:"app"`
		BundleID   string `json:"bundle_id"`
		PID        *int   `json:"pid"`
		LaunchDate string `json:"launch_date"`
	}
	if err := json.Unmarshal(raw, &identity); err != nil ||
		strings.TrimSpace(identity.App) == "" ||
		strings.TrimSpace(identity.BundleID) == "" ||
		identity.PID == nil || *identity.PID <= 0 {
		return OpenAIComputerTaskAppV1{},
			fmt.Errorf("task app preparation returned invalid identity")
	}
	return OpenAIComputerTaskAppV1{
		App:        strings.TrimSpace(identity.App),
		BundleID:   strings.TrimSpace(identity.BundleID),
		PID:        *identity.PID,
		LaunchDate: strings.TrimSpace(identity.LaunchDate),
	}, nil
}

func decodeOpenAIComputerBackgroundBindingV1(
	raw json.RawMessage,
) (openAIComputerBackgroundBindingV1, error) {
	target, err := decodeOpenAIComputerTaskAppIdentityV1(raw)
	if err != nil {
		return openAIComputerBackgroundBindingV1{}, err
	}
	var identity struct {
		PreservedFrontmostPID        *int   `json:"preserved_frontmost_pid"`
		PreservedFrontmostBundleID   string `json:"preserved_frontmost_bundle_id"`
		PreservedFrontmostLaunchDate string `json:"preserved_frontmost_launch_date"`
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		return openAIComputerBackgroundBindingV1{},
			fmt.Errorf("decode background app binding: %w", err)
	}
	binding := openAIComputerBackgroundBindingV1{target: target}
	if identity.PreservedFrontmostPID == nil &&
		strings.TrimSpace(identity.PreservedFrontmostBundleID) == "" &&
		strings.TrimSpace(identity.PreservedFrontmostLaunchDate) == "" {
		// A pre-keyboard helper can still provide the already-qualified
		// background semantic lane. Keyboard planning will fail closed because
		// no preserved-frontmost authority is installed.
		return binding, nil
	}
	if identity.PreservedFrontmostPID == nil ||
		*identity.PreservedFrontmostPID <= 0 ||
		*identity.PreservedFrontmostPID == target.PID ||
		strings.TrimSpace(identity.PreservedFrontmostBundleID) == "" {
		return openAIComputerBackgroundBindingV1{},
			fmt.Errorf("background app binding returned invalid preserved frontmost identity")
	}
	binding.preservedFrontmostPID = *identity.PreservedFrontmostPID
	binding.preservedFrontmostBundleID =
		strings.TrimSpace(identity.PreservedFrontmostBundleID)
	binding.preservedFrontmostLaunchDate =
		strings.TrimSpace(identity.PreservedFrontmostLaunchDate)
	return binding, nil
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
	params := map[string]any{"app_name": app}
	excludedPIDs := r.excludedTaskAppPIDsV1()
	if len(excludedPIDs) > 0 {
		params["excluded_pids"] = excludedPIDs
	}
	raw, err := r.raw.client.Call(
		ctx,
		"resolve_app_identity",
		params,
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
		for _, excludedPID := range excludedPIDs {
			if result.PID == excludedPID {
				return OpenAIComputerTaskAppV1{},
					fmt.Errorf("resolve app %q returned an excluded process", app)
			}
		}
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
	excludedPIDs := r.excludedTaskAppPIDsV1()
	for index := range apps {
		app := apps[index]
		if strings.TrimSpace(app.App) == "" ||
			strings.TrimSpace(app.BundleID) == "" {
			return fmt.Errorf("OpenAI computer task app identity is invalid")
		}
		raw, err := r.raw.client.Call(
			ctx,
			"prepare_task_app",
			openAIComputerTaskAppParamsV1(app, excludedPIDs),
		)
		if err != nil {
			return fmt.Errorf("prepare app %q: %w", app.App, err)
		}
		prepared, err := decodeOpenAIComputerTaskAppIdentityV1(raw)
		if err != nil {
			return fmt.Errorf("prepare app %q: %w", app.App, err)
		}
		if !strings.EqualFold(prepared.BundleID, app.BundleID) {
			return fmt.Errorf("prepare app %q changed bundle identity", app.App)
		}
		if app.PID > 0 && prepared.PID != app.PID {
			return fmt.Errorf("prepare app %q changed exact pid", app.App)
		}
		for _, excludedPID := range excludedPIDs {
			if prepared.PID == excludedPID {
				return fmt.Errorf("prepare app %q returned an excluded process", app.App)
			}
		}
		apps[index] = prepared
	}
	// Preparing multiple launch hints leaves the last app active. Restore the
	// first task target by its exact prepared identity, never by name.
	if len(apps) > 1 {
		focusParams := openAIComputerTaskAppParamsV1(apps[0], excludedPIDs)
		focusParams["verify"] = true
		if _, err := r.raw.client.Call(
			ctx,
			"focus",
			focusParams,
		); err != nil {
			return fmt.Errorf("focus app %q: %w", apps[0].App, err)
		}
	}
	// The explicit task app list supersedes a Quick Panel foreground hint.
	// Clear that one-shot bootstrap target so every post-action refresh follows
	// the actual frontmost window, including an OpenAI-native app switch.
	r.raw.initialTarget = nil
	r.raw.invalidateState()
	return nil
}

// PrepareTaskAppsV1 keeps ordinary tasks on the established foreground path.
// Background execution is an explicit user constraint, never an opportunistic
// optimization: probing it for foreground-allowed work adds a fragile
// background-to-foreground transition before common keyboard actions.
// A required background task binds one already-running exact process with one
// visible non-frontmost window and fails before activation when unavailable.
func (r *OpenAIComputerActionRuntimeV1) PrepareTaskAppsV1(
	ctx context.Context,
	apps []OpenAIComputerTaskAppV1,
	options OpenAIComputerTaskPreparationOptionsV1,
) (OpenAIComputerExecutionLaneV1, error) {
	if r == nil || r.raw == nil || r.raw.client == nil {
		return "", fmt.Errorf("OpenAI computer task preparer is unavailable")
	}
	r.executionLane = OpenAIComputerExecutionForegroundV1
	r.foregroundFallback = false
	r.backgroundTarget = nil
	r.raw.backgroundInputAuthority = nil
	r.raw.foregroundFallbackRestorePending = false
	r.backgroundRequired = options.RequireBackground

	if !options.RequireBackground {
		if err := r.LaunchAndFocusTaskAppsV1(ctx, apps); err != nil {
			return "", err
		}
		return r.executionLane, nil
	}

	if len(apps) == 1 && apps[0].PID > 0 {
		excludedPIDs := r.excludedTaskAppPIDsV1()
		raw, err := r.raw.client.Call(
			ctx,
			"bind_background_task_app",
			openAIComputerTaskAppParamsV1(apps[0], excludedPIDs),
		)
		if err == nil {
			binding, decodeErr := decodeOpenAIComputerBackgroundBindingV1(raw)
			if decodeErr != nil {
				return "", fmt.Errorf(
					"bind background app %q: %w",
					apps[0].App,
					decodeErr,
				)
			}
			prepared := binding.target
			if !strings.EqualFold(prepared.BundleID, apps[0].BundleID) ||
				prepared.PID != apps[0].PID {
				return "", fmt.Errorf(
					"bind background app %q changed exact identity",
					apps[0].App,
				)
			}
			for _, excludedPID := range excludedPIDs {
				if prepared.PID == excludedPID {
					return "", fmt.Errorf(
						"bind background app %q returned an excluded process",
						apps[0].App,
					)
				}
			}
			apps[0] = prepared
			r.executionLane = OpenAIComputerExecutionBackgroundSemanticV1
			target := prepared
			r.backgroundTarget = &target
			if prepared.LaunchDate != "" &&
				binding.preservedFrontmostPID > 0 &&
				binding.preservedFrontmostBundleID != "" &&
				binding.preservedFrontmostLaunchDate != "" {
				r.raw.backgroundInputAuthority =
					&computerUseBackgroundInputAuthorityV1{
						targetLaunchDate:             prepared.LaunchDate,
						preservedFrontmostPID:        binding.preservedFrontmostPID,
						preservedFrontmostBundleID:   binding.preservedFrontmostBundleID,
						preservedFrontmostLaunchDate: binding.preservedFrontmostLaunchDate,
					}
			}
			r.raw.initialTarget = nil
			r.raw.invalidateState()
			return r.executionLane, nil
		}
		return "", fmt.Errorf(
			"required background bind for %q failed: %w",
			apps[0].App,
			err,
		)
	}

	return "", fmt.Errorf(
		"required background execution needs one already-running exact app target",
	)
}

func (r *OpenAIComputerActionRuntimeV1) applyExecutionLaneV1(
	args *computerUseArgs,
) {
	if args == nil {
		return
	}
	lane := r.executionLane
	if lane == "" {
		lane = OpenAIComputerExecutionForegroundV1
	}
	args.ExecutionLane = string(lane)
	args.ForegroundFallback = r.foregroundFallback
}

func (r *OpenAIComputerActionRuntimeV1) transitionToForegroundV1() {
	if r == nil || r.executionLane != OpenAIComputerExecutionBackgroundSemanticV1 {
		return
	}
	if r.backgroundRequired {
		return
	}
	r.executionLane = OpenAIComputerExecutionForegroundV1
	r.foregroundFallback = true
	r.backgroundTarget = nil
	r.raw.backgroundInputAuthority = nil
	r.raw.foregroundFallbackRestorePending = true
	r.raw.initialTarget = nil
}

func (r *OpenAIComputerActionRuntimeV1) backgroundKeyboardFocusedRefV1(
	helpers *AnthropicComputerAdapter,
) (string, error) {
	if r == nil || r.raw == nil || r.raw.backgroundInputAuthority == nil ||
		helpers == nil {
		return "", fmt.Errorf("preserved-frontmost keyboard authority is unavailable")
	}
	ref, err := helpers.uniqueFocusedRef()
	if err != nil {
		return "", err
	}
	entry, exists := r.raw.refs[ref]
	if !exists || entry.path == "" || entry.role == "" ||
		entry.fingerprint == "" {
		return "", fmt.Errorf("focused AX ref has no exact element authority")
	}
	element, err := resolveComputerUseFingerprint(
		r.raw.snapshot.elements,
		entry.fingerprint,
	)
	if err != nil || element.Ref != ref ||
		element.Path != entry.path ||
		element.Role != entry.role ||
		element.Fingerprint != entry.fingerprint ||
		!computerUseEditableFocusEvidenceV1(element) {
		return "", fmt.Errorf("focused AX ref is not one enabled editable target")
	}
	return ref, nil
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
	return r.planOpenAIComputerObservationV1(
		nil,
		description,
		includeScreenshot,
		false,
	)
}

// AuthorizeOpenAIComputerTypeAfterKeypressV1 installs one window-bound,
// one-shot keyboard target after the daemon has executed a provider keypress
// and refreshed the real frontmost window. This covers ordinary ordered
// batches such as focus-shortcut -> type without trusting a stale pre-keypress
// AX focus or inventing app-specific address-bar rules.
func (r *OpenAIComputerActionRuntimeV1) AuthorizeOpenAIComputerTypeAfterKeypressV1(
	action OpenAIComputerActionV1,
) error {
	if r == nil || r.raw == nil || r.raw.snapshot == nil {
		return &OpenAIComputerActionPlanErrorV1{
			FailureCode: "keyboard_post_keypress_target_unavailable",
			Detail:      "the post-keypress observation did not produce a keyboard target",
		}
	}
	if action.Type != OpenAIComputerActionKeypressV1 {
		return &OpenAIComputerActionPlanErrorV1{
			FailureCode: "keyboard_post_keypress_action_invalid",
			Detail:      "the keyboard target request was not bound to the committed keypress",
		}
	}
	snapshot := r.raw.snapshot
	if !snapshot.typed || snapshot.id == "" || snapshot.pid <= 0 ||
		strings.TrimSpace(snapshot.bundleID) == "" ||
		strings.TrimSpace(snapshot.app) == "" ||
		snapshot.windowID == nil || *snapshot.windowID <= 0 ||
		uint64(*snapshot.windowID) > uint64(^uint32(0)) ||
		snapshot.expectedWindowAXBounds == nil ||
		snapshot.expectedWindowAXBounds.Width <= 0 ||
		snapshot.expectedWindowAXBounds.Height <= 0 {
		return &OpenAIComputerActionPlanErrorV1{
			FailureCode: "keyboard_post_keypress_target_identity_invalid",
			Detail:      "the post-keypress app, window, or frame identity is incomplete",
		}
	}
	r.raw.coordinateFocus = &computerUseCoordinateFocusV1{
		stateID:                snapshot.id,
		pid:                    snapshot.pid,
		bundleID:               snapshot.bundleID,
		app:                    snapshot.app,
		windowID:               uint32(*snapshot.windowID),
		expectedWindowAXBounds: *snapshot.expectedWindowAXBounds,
		filter:                 snapshot.filter,
		budget:                 snapshot.budget,
		locationNavigation:     openAIComputerLocationFocusShortcutV1(action),
		// The target comes from the post-keypress refresh itself. Any later
		// input is still bound to the same verified app, window, and frame.
	}
	return nil
}

// PlanOpenAIComputerTaskInitialObservationV1 binds the first screenshot to the
// first optional task app hint. Later observations deliberately pass nil and
// follow the real frontmost app so one native task can still switch apps.
func (r *OpenAIComputerActionRuntimeV1) PlanOpenAIComputerTaskInitialObservationV1(
	app *OpenAIComputerTaskAppV1,
	description string,
	includeScreenshot bool,
) (OpenAIComputerActionPlanV1, error) {
	return r.planOpenAIComputerObservationV1(
		app,
		description,
		includeScreenshot,
		true,
	)
}

func (r *OpenAIComputerActionRuntimeV1) planOpenAIComputerObservationV1(
	app *OpenAIComputerTaskAppV1,
	description string,
	includeScreenshot bool,
	initial bool,
) (OpenAIComputerActionPlanV1, error) {
	if strings.TrimSpace(description) == "" {
		return OpenAIComputerActionPlanV1{},
			fmt.Errorf("OpenAI computer observation description is required")
	}
	if r.executionLane == OpenAIComputerExecutionBackgroundSemanticV1 &&
		r.backgroundTarget != nil {
		r.raw.initialTarget = &ComputerUseInitialTargetV1{
			PID:      r.backgroundTarget.PID,
			AppName:  strings.TrimSpace(r.backgroundTarget.App),
			BundleID: strings.TrimSpace(r.backgroundTarget.BundleID),
		}
	} else if initial && app != nil {
		if strings.TrimSpace(app.App) == "" ||
			strings.TrimSpace(app.BundleID) == "" ||
			app.PID <= 0 {
			return OpenAIComputerActionPlanV1{},
				fmt.Errorf("OpenAI computer initial observation app identity is invalid")
		}
		r.raw.initialTarget = &ComputerUseInitialTargetV1{
			PID:      app.PID,
			AppName:  strings.TrimSpace(app.App),
			BundleID: strings.TrimSpace(app.BundleID),
		}
	} else if !initial {
		// The prepared first-app identity is a one-observation bootstrap. Once
		// native execution starts, later observations must follow the actual
		// frontmost app so one goal-level task can switch applications.
		r.raw.initialTarget = nil
	}
	// Every provider-requested observation follows the app/window that is
	// actually frontmost now. This is also used for one lightweight,
	// image-free refresh after a keypress when the same ordered batch has a
	// later action: Command-Tab (and shortcuts that open another window) must
	// not leave that later action bound to the previous app's snapshot.
	// Keep the one-shot verified click handoff while the observation follows
	// the real frontmost window. getAppState restores it only when the newly
	// observed PID, bundle, window, and frame still match; an app/window switch
	// drops it. This lets a provider put click and type in adjacent batches
	// without weakening target identity or requiring an AX-focused ref.
	r.raw.invalidateObservationState()
	r.raw.navigationCommit = nil
	args := computerUseArgs{
		Action:            "get_app_state",
		Description:       description,
		IncludeScreenshot: includeScreenshot,
		FollowFrontmost:   true,
	}
	r.applyExecutionLaneV1(&args)
	return r.plan(args, false)
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
		args := computerUseArgs{
			Action: "wait", Description: description, Timeout: 1,
		}
		r.applyExecutionLaneV1(&args)
		return r.plan(args, false)
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

	if r.executionLane == OpenAIComputerExecutionBackgroundSemanticV1 {
		if action.Type == OpenAIComputerActionClickV1 &&
			action.Button == "left" &&
			len(action.Keys) == 0 &&
			action.X != nil && action.Y != nil {
			ref, found, hitErr := anthropicHelpers.
				uniqueAXPressHitFromCurrentImage(
					ctx,
					*action.X,
					*action.Y,
				)
			if hitErr != nil {
				return OpenAIComputerActionPlanV1{},
					&OpenAIComputerActionPlanErrorV1{
						FailureCode: "background_semantic_projection_unavailable",
						Detail: "the background semantic image could not be " +
							"mapped to one exact AX target: " + hitErr.Error(),
					}
			}
			if found {
				args.Action, args.Ref = "press", ref
				r.applyExecutionLaneV1(&args)
				return r.plan(args, true)
			}
		}
		if action.Type == OpenAIComputerActionScrollV1 &&
			action.X != nil && action.Y != nil &&
			action.ScrollX != nil && action.ScrollY != nil {
			dx, dy, valid := openAIComputerSemanticScrollDeltasV1(
				*action.ScrollX,
				*action.ScrollY,
			)
			if !valid {
				return OpenAIComputerActionPlanV1{},
					&OpenAIComputerActionPlanErrorV1{
						FailureCode: "scroll_delta_invalid",
						Detail:      "OpenAI computer scroll deltas are invalid",
					}
			}
			ref, found, hitErr := anthropicHelpers.
				uniqueAXScrollHitFromCurrentImage(
					ctx,
					*action.X,
					*action.Y,
				)
			if hitErr != nil {
				return OpenAIComputerActionPlanV1{},
					&OpenAIComputerActionPlanErrorV1{
						FailureCode: "background_semantic_projection_unavailable",
						Detail: "the background semantic image could not be " +
							"mapped to one exact AX scroll target: " +
							hitErr.Error(),
					}
			}
			if found {
				args.Action, args.Ref = "scroll", ref
				args.DX, args.DY = dx, dy
				r.applyExecutionLaneV1(&args)
				return r.plan(args, true)
			}
		}
		if action.Type == OpenAIComputerActionTypeTextV1 {
			if strings.IndexFunc(action.Text, unicode.IsControl) >= 0 {
				return OpenAIComputerActionPlanV1{},
					&OpenAIComputerActionPlanErrorV1{
						FailureCode: "background_keyboard_control_text_unsupported",
						Detail: "background text cannot contain Return, Tab, " +
							"or another control character; use an explicit " +
							"keypress with its own safety policy",
					}
			}
			ref, focusErr := r.backgroundKeyboardFocusedRefV1(anthropicHelpers)
			if focusErr == nil {
				text := action.Text
				args.Action, args.Ref, args.Text = "type", ref, &text
				args.ExecutionLane =
					string(OpenAIComputerExecutionBackgroundKeyboardV1)
				args.ForegroundFallback = false
				return r.plan(args, true)
			}
			if r.backgroundRequired {
				return OpenAIComputerActionPlanV1{},
					&OpenAIComputerActionPlanErrorV1{
						FailureCode: "background_keyboard_target_unavailable",
						Detail: "the required background keyboard target is " +
							"unavailable: " + focusErr.Error(),
					}
			}
		}
		if action.Type == OpenAIComputerActionKeypressV1 {
			ref, focusErr := r.backgroundKeyboardFocusedRefV1(anthropicHelpers)
			modifiers, keys, sequenceErr :=
				openAIComputerKeySequenceV1(action.Keys)
			if sequenceErr != nil {
				return OpenAIComputerActionPlanV1{}, sequenceErr
			}
			if backgroundTargetedInputConsequentialKeyV1(keys, modifiers) {
				return OpenAIComputerActionPlanV1{},
					&OpenAIComputerActionPlanErrorV1{
						FailureCode: "background_keyboard_consequential_key_unsupported",
						Detail: "the requested background key could submit or " +
							"destructively mutate content and has no exact " +
							"background confirmation target",
					}
			}
			if focusErr == nil {
				args.Action = "keypress"
				args.Ref = ref
				args.Modifiers = modifiers
				args.KeySequence = keys
				args.ExecutionLane =
					string(OpenAIComputerExecutionBackgroundKeyboardV1)
				args.ForegroundFallback = false
				return r.plan(args, true)
			}
			if r.backgroundRequired {
				return OpenAIComputerActionPlanV1{},
					&OpenAIComputerActionPlanErrorV1{
						FailureCode: "background_keyboard_target_unavailable",
						Detail: "the required background keyboard target is " +
							"unavailable: " + focusErr.Error(),
					}
			}
		}
		if r.backgroundRequired {
			return OpenAIComputerActionPlanV1{},
				&OpenAIComputerActionPlanErrorV1{
					FailureCode: "background_action_unsupported",
					Detail: "the requested " + action.Type +
						" action cannot be executed by the required " +
						"background semantic lane without activating the app",
				}
		}
		r.transitionToForegroundV1()
	}
	r.applyExecutionLaneV1(&args)

	switch action.Type {
	case OpenAIComputerActionClickV1, OpenAIComputerActionDoubleClickV1:
		if action.X == nil || action.Y == nil {
			return OpenAIComputerActionPlanV1{},
				&OpenAIComputerActionPlanErrorV1{
					FailureCode: "coordinate_fields_unavailable",
					Detail:      "OpenAI computer click coordinates are unavailable",
				}
		}
		if _, err := anthropicHelpers.actionableObservation(ctx); err != nil {
			return OpenAIComputerActionPlanV1{},
				&OpenAIComputerActionPlanErrorV1{
					FailureCode: "coordinate_authority_unavailable",
					Detail:      "OpenAI computer coordinate authority is unavailable",
				}
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
				&OpenAIComputerActionPlanErrorV1{
					FailureCode: "coordinate_fields_unavailable",
					Detail:      "OpenAI computer move coordinates are unavailable",
				}
		}
		if _, err := anthropicHelpers.actionableObservation(ctx); err != nil {
			return OpenAIComputerActionPlanV1{},
				&OpenAIComputerActionPlanErrorV1{
					FailureCode: "coordinate_authority_unavailable",
					Detail:      "OpenAI computer coordinate authority is unavailable",
				}
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
				&OpenAIComputerActionPlanErrorV1{
					FailureCode: "coordinate_authority_unavailable",
					Detail:      "OpenAI computer coordinate authority is unavailable",
				}
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
		ref := ""
		if r.raw.coordinateFocus == nil ||
			r.raw.coordinateFocus.stateID != stateID {
			var err error
			ref, err = anthropicHelpers.uniqueFocusedRef()
			if err != nil {
				return OpenAIComputerActionPlanV1{},
					&OpenAIComputerActionPlanErrorV1{
						FailureCode: "keyboard_plan_focused_ref_unavailable",
						Detail: "OpenAI computer type target is unavailable: " +
							err.Error(),
					}
			}
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
				&OpenAIComputerActionPlanErrorV1{
					FailureCode: "scroll_fields_unavailable",
					Detail:      "OpenAI computer pixel scroll fields are unavailable",
				}
		}
		if !coordinatePixelScrollProviderDeltasV1(
			int64(*action.ScrollX), int64(*action.ScrollY)) {
			return OpenAIComputerActionPlanV1{},
				&OpenAIComputerActionPlanErrorV1{
					FailureCode: "scroll_delta_invalid",
					Detail:      "OpenAI computer pixel scroll deltas are invalid",
				}
		}
		if _, err := anthropicHelpers.actionableObservation(ctx); err != nil {
			return OpenAIComputerActionPlanV1{},
				&OpenAIComputerActionPlanErrorV1{
					FailureCode: "coordinate_authority_unavailable",
					Detail:      "OpenAI computer coordinate authority is unavailable",
				}
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

func openAIComputerSemanticScrollDeltasV1(
	scrollX int,
	scrollY int,
) (computerUseInt, computerUseInt, bool) {
	if !coordinatePixelScrollProviderDeltasV1(
		int64(scrollX),
		int64(scrollY),
	) {
		return 0, 0, false
	}
	value := scrollY
	vertical := true
	if absIntV1(scrollX) > absIntV1(scrollY) {
		value = scrollX
		vertical = false
	}
	if value == 0 {
		return 0, 0, false
	}
	// Native computer deltas are pixels while AX exposes discrete increments.
	// Preserve direction and coarse magnitude without turning one provider
	// action into an unbounded local loop.
	steps := (absIntV1(value) + 99) / 100
	if steps < 1 {
		steps = 1
	}
	if steps > 10 {
		steps = 10
	}
	if value < 0 {
		steps = -steps
	}
	if vertical {
		return 0, computerUseInt(steps), true
	}
	return computerUseInt(steps), 0, true
}

func absIntV1(value int) int {
	if value < 0 {
		// Provider deltas are constrained to signed 32-bit range before this
		// helper, so negation cannot overflow the host int.
		return -value
	}
	return value
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
