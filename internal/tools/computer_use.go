package tools

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

// axCallClient is the narrow ax_server seam used by computer_use. Keeping the
// interface local makes the state/approval contract testable without starting
// the macOS helper; AXClient is the production implementation.
type axCallClient interface {
	Call(context.Context, string, any) (json.RawMessage, error)
}

type computerUseSnapshot struct {
	id                     string
	status                 string
	app                    string
	bundleID               string
	pid                    int
	window                 string
	windowID               *int
	expectedWindowAXBounds *CoordinateQuartzRectV1
	focusedRef             string
	filter                 string
	budget                 int
	elements               []computerUseElement
	signatures             map[string]string
	typed                  bool
}

// computerUseSemanticImageArtifactV1 binds an exact window image to the AX
// snapshot that produced it without granting physical coordinate authority.
// It exists for the background semantic lane only: a window may be partly
// outside an active display yet still support a safe, exact AXPress. Physical
// mouse, drag, scroll, and keyboard paths never consume this artifact.
type computerUseSemanticImageArtifactV1 struct {
	stateID      string
	pid          int
	bundleID     string
	windowID     int
	windowBounds CoordinateQuartzRectV1
	widthPX      int
	heightPX     int
	createdAt    time.Time
	expiresAt    time.Time
}

func newComputerUseSemanticImageArtifactV1(
	tree computerUseTree,
	stateID string,
	capture exactVisualOnlyWindowCapture,
	now time.Time,
) (*computerUseSemanticImageArtifactV1, error) {
	if tree.SchemaVersion != 1 || tree.PID <= 0 ||
		strings.TrimSpace(tree.BundleID) == "" ||
		tree.WindowID == nil || *tree.WindowID <= 0 ||
		tree.WindowFrame == nil ||
		strings.TrimSpace(stateID) == "" ||
		capture.WidthPX <= 0 || capture.HeightPX <= 0 ||
		now.IsZero() {
		return nil, fmt.Errorf(
			"semantic image requires exact app, window, state, and image identity",
		)
	}
	bounds := CoordinateQuartzRectV1{
		X: tree.WindowFrame.X, Y: tree.WindowFrame.Y,
		Width: tree.WindowFrame.Width, Height: tree.WindowFrame.Height,
	}
	if err := validateCoordinateQuartzRect("semantic image window", bounds); err != nil {
		return nil, err
	}
	now = now.UTC()
	return &computerUseSemanticImageArtifactV1{
		stateID: stateID, pid: tree.PID, bundleID: tree.BundleID,
		windowID: *tree.WindowID, windowBounds: bounds,
		widthPX: capture.WidthPX, heightPX: capture.HeightPX,
		createdAt: now, expiresAt: now.Add(computerUseCoordinateFrameTTLV1),
	}, nil
}

func (artifact *computerUseSemanticImageArtifactV1) mapPointV1(
	snapshot *computerUseSnapshot,
	now time.Time,
	x int,
	y int,
) (CoordinateMappedPointV1, error) {
	if artifact == nil || snapshot == nil || !snapshot.typed ||
		snapshot.id == "" || snapshot.id != artifact.stateID ||
		snapshot.pid != artifact.pid ||
		snapshot.bundleID != artifact.bundleID ||
		snapshot.windowID == nil || *snapshot.windowID != artifact.windowID ||
		snapshot.expectedWindowAXBounds == nil ||
		*snapshot.expectedWindowAXBounds != artifact.windowBounds {
		return CoordinateMappedPointV1{},
			fmt.Errorf("semantic image does not match the current exact AX target")
	}
	if now.IsZero() || now.Before(artifact.createdAt) ||
		!now.Before(artifact.expiresAt) {
		return CoordinateMappedPointV1{},
			fmt.Errorf("semantic image authority is stale or expired")
	}
	if x < 0 || y < 0 || x >= artifact.widthPX || y >= artifact.heightPX {
		return CoordinateMappedPointV1{},
			fmt.Errorf("semantic image coordinate is outside the exact window image")
	}
	pixelCenterX := float64(x) + 0.5
	pixelCenterY := float64(y) + 0.5
	return CoordinateMappedPointV1{
		X: artifact.windowBounds.X +
			pixelCenterX*artifact.windowBounds.Width/float64(artifact.widthPX),
		Y: artifact.windowBounds.Y +
			pixelCenterY*artifact.windowBounds.Height/float64(artifact.heightPX),
	}, nil
}

type computerUseCoordinateExecutorV1 func(
	context.Context,
	CoordinateMouseEventRequestV1,
) (CoordinateMouseEventResultV1, error)

type computerUseCoordinateDragExecutorV1 func(
	context.Context,
	CoordinateDragRequestV1,
) (CoordinateDragResultV1, error)

type computerUseCoordinatePixelScrollExecutorV1 func(
	context.Context,
	CoordinatePixelScrollRequestV1,
) (CoordinatePixelScrollResultV1, error)

type computerUseSemanticTextSelectionExecutorV2 func(
	context.Context,
	SemanticTextSelectionRequestV2,
) (SemanticTextSelectionResultV2, error)

type computerUseSemanticPressExecutorV2 func(
	context.Context,
	SemanticPressRequestV2,
) (SemanticPressResultV2, error)

type computerUseSemanticScrollExecutorV1 func(
	context.Context,
	SemanticScrollRequestV1,
) (SemanticScrollResultV1, error)

type computerUseTargetBoundInputExecutorV1 func(
	context.Context,
	TargetBoundInputRequestV1,
) (TargetBoundInputResultV1, error)

type computerUseBackgroundTargetedInputExecutorV1 func(
	context.Context,
	BackgroundTargetedInputRequestV1,
) (TargetBoundInputResultV1, error)

type computerUseBackgroundInputAuthorityV1 struct {
	targetLaunchDate             string
	preservedFrontmostPID        int
	preservedFrontmostBundleID   string
	preservedFrontmostLaunchDate string
}

const (
	computerUseCoordinateMaxRawCaptureBytesV1    = 16 * 1024 * 1024
	computerUseCoordinateMaxCaptureNDJSONBytesV1 = 22 * 1024 * 1024
	computerUseCoordinateMaxCapturePixelsV1      = 16_000_000
	computerUseCoordinateFrameTTLV1              = CoordinateFrameMaxTTLV1
	computerUseDefaultDragDurationV1             = 350
	computerUseMutationDeadlineV1                = 2 * time.Second
	computerUseDragDeadlineOverheadV1            = 500 * time.Millisecond
)

func defaultComputerUseCoordinateImageProfileV1() CoordinateImageProfileV1 {
	return CoordinateImageProfileV1{
		SchemaVersion:                     1,
		ID:                                "provider_balanced_png",
		Version:                           1,
		MediaType:                         "image/png",
		FallbackMediaType:                 "image/jpeg",
		TargetLongEdgePX:                  1280,
		MaxLongEdgePX:                     1568,
		MaxTotalPixels:                    1_600_000,
		MaxEncodedBytes:                   CoordinateMaxRawImageBytesV1,
		JPEGQualityLadder:                 []int{90, 82, 74},
		PaddingMode:                       "none",
		RequiresExactCoordinateDimensions: true,
	}
}

func defaultComputerUseCoordinateCaptureLimitsV1() CaptureCoordinateWindowLimitsV1 {
	return CaptureCoordinateWindowLimitsV1{
		MaxRawBytes:    computerUseCoordinateMaxRawCaptureBytesV1,
		MaxNDJSONBytes: computerUseCoordinateMaxCaptureNDJSONBytesV1,
		MaxPixels:      computerUseCoordinateMaxCapturePixelsV1,
	}
}

// computerUseInt accepts both a JSON integer and its decimal string form.
// Tool schemas still advertise integers, but model providers occasionally
// serialize coordinates as "154". Treating that harmless representation
// difference as a hard validation failure causes expensive identical retries.
type computerUseInt int

func (v *computerUseInt) UnmarshalJSON(data []byte) error {
	var integer int
	if err := json.Unmarshal(data, &integer); err == nil {
		*v = computerUseInt(integer)
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err != nil {
		return fmt.Errorf("expected an integer or decimal integer string")
	}
	parsed, err := strconv.Atoi(strings.TrimSpace(text))
	if err != nil {
		return fmt.Errorf("expected an integer or decimal integer string")
	}
	*v = computerUseInt(parsed)
	return nil
}

type computerUseArgs struct {
	Action             string               `json:"action"`
	Description        string               `json:"description"`
	StateID            string               `json:"state_id,omitempty"`
	App                string               `json:"app,omitempty"`
	Window             string               `json:"window,omitempty"`
	Ref                string               `json:"ref,omitempty"`
	Value              *string              `json:"value,omitempty"`
	X                  *computerUseInt      `json:"x,omitempty"`
	Y                  *computerUseInt      `json:"y,omitempty"`
	ScrollX            *computerUseInt      `json:"scroll_x,omitempty"`
	ScrollY            *computerUseInt      `json:"scroll_y,omitempty"`
	StartX             *computerUseInt      `json:"start_x,omitempty"`
	StartY             *computerUseInt      `json:"start_y,omitempty"`
	EndX               *computerUseInt      `json:"end_x,omitempty"`
	EndY               *computerUseInt      `json:"end_y,omitempty"`
	Path               []computerUsePoint   `json:"path,omitempty"`
	DurationMS         computerUseInt       `json:"duration_ms,omitempty"`
	Range              *SemanticTextRangeV2 `json:"range,omitempty"`
	Text               *string              `json:"text,omitempty"`
	Keys               string               `json:"keys,omitempty"`
	KeySequence        []string             `json:"key_sequence,omitempty"`
	Modifiers          []string             `json:"modifiers,omitempty"`
	Button             string               `json:"button,omitempty"`
	Clicks             computerUseInt       `json:"clicks,omitempty"`
	DX                 computerUseInt       `json:"dx,omitempty"`
	DY                 computerUseInt       `json:"dy,omitempty"`
	Condition          string               `json:"condition,omitempty"`
	Query              string               `json:"query,omitempty"`
	Role               string               `json:"role,omitempty"`
	Timeout            float64              `json:"timeout,omitempty"`
	Interval           float64              `json:"interval,omitempty"`
	Filter             string               `json:"filter,omitempty"`
	SemanticBudget     computerUseInt       `json:"semantic_budget,omitempty"`
	IncludeScreenshot  bool                 `json:"include_screenshot,omitempty"`
	ExecutionLane      string               `json:"execution_lane,omitempty"`
	ForegroundFallback bool                 `json:"foreground_fallback,omitempty"`
	FollowFrontmost    bool                 `json:"follow_frontmost,omitempty"`
}

type computerUsePoint struct {
	X computerUseInt `json:"x"`
	Y computerUseInt `json:"y"`
}

// ComputerUseInitialTargetV1 is the exact app identity captured before the
// Desktop surface takes focus. It is bound only to a run-local tool clone.
type ComputerUseInitialTargetV1 struct {
	PID      int
	AppName  string
	BundleID string
}

type computerUseTargetScopeV1 uint8

const (
	computerUseTargetScopeForegroundV1 computerUseTargetScopeV1 = iota
	computerUseTargetScopeExplicitV1
)

type computerUseCoordinateFocusV1 struct {
	stateID                string
	pid                    int
	bundleID               string
	app                    string
	windowID               uint32
	expectedWindowAXBounds CoordinateQuartzRectV1
	filter                 string
	budget                 int
	locationNavigation     bool
}

type computerUseNavigationCommitV1 struct {
	pid      int
	bundleID string
	windowID uint32
	path     string
	role     string
}

func (focus *computerUseCoordinateFocusV1) treeMismatchCode(
	tree computerUseTree,
) string {
	switch {
	case focus == nil:
		return "keyboard_focus_witness_missing"
	case tree.SchemaVersion != 1:
		return "keyboard_target_schema_changed"
	case tree.PID != focus.pid:
		return "keyboard_target_process_changed"
	case tree.BundleID != focus.bundleID:
		return "keyboard_target_bundle_changed"
	case tree.WindowID == nil || *tree.WindowID <= 0:
		return "keyboard_target_window_missing"
	case uint64(*tree.WindowID) != uint64(focus.windowID):
		return "keyboard_target_window_changed"
	case tree.WindowFrame == nil:
		return "keyboard_target_frame_missing"
	case !captureCoordinateWindowRectsCorrelate(
		focus.expectedWindowAXBounds,
		CoordinateQuartzRectV1{
			X: tree.WindowFrame.X, Y: tree.WindowFrame.Y,
			Width: tree.WindowFrame.Width, Height: tree.WindowFrame.Height,
		},
	):
		return "keyboard_target_frame_drift"
	default:
		return ""
	}
}

func (focus *computerUseCoordinateFocusV1) matchesTree(
	tree computerUseTree,
) bool {
	return focus.treeMismatchCode(tree) == ""
}

// ComputerUseTool is the provider-neutral macOS GUI tool. It deliberately
// keeps only one current observation per agent run: refs are meaningful only
// for that state_id and every ref action re-observes before touching the GUI.
type ComputerUseTool struct {
	client                           axCallClient
	targetScope                      computerUseTargetScopeV1
	initialTarget                    *ComputerUseInitialTargetV1
	snapshot                         *computerUseSnapshot
	refs                             map[string]refEntry
	coordinateArtifact               *CoordinateWindowArtifactV1
	semanticImageArtifact            *computerUseSemanticImageArtifactV1
	coordinateFocus                  *computerUseCoordinateFocusV1
	navigationCommit                 *computerUseNavigationCommitV1
	coordinateExecutor               computerUseCoordinateExecutorV1
	coordinateDragExecutor           computerUseCoordinateDragExecutorV1
	coordinatePixelScrollExecutor    computerUseCoordinatePixelScrollExecutorV1
	semanticTextSelectionExecutor    computerUseSemanticTextSelectionExecutorV2
	semanticPressExecutor            computerUseSemanticPressExecutorV2
	semanticScrollExecutor           computerUseSemanticScrollExecutorV1
	targetBoundInputExecutor         computerUseTargetBoundInputExecutorV1
	backgroundTargetedInputExecutor  computerUseBackgroundTargetedInputExecutorV1
	backgroundInputAuthority         *computerUseBackgroundInputAuthorityV1
	foregroundFallbackRestorePending bool
	coordinateProfile                CoordinateImageProfileV1
	coordinateCaptureLimits          CaptureCoordinateWindowLimitsV1
	coordinateNow                    func() time.Time
	coordinateFrameID                func() (string, error)
}

// A Mac has one frontmost app, pointer, keyboard focus, and AX server. Keep a
// whole computer_use call atomic across independently cloned daemon runs so a
// Slack action cannot slip between another route's stale-state preflight and
// click. This cannot prevent the human from moving the UI, which is why the
// state_id preflight remains necessary as a second, optimistic guard.
//
// Shared by EVERY GUI-touching tool in this package — computer_use,
// accessibility, computer, and applescript all acquire it for their whole
// Run — because a legacy-tool mutation from one route interleaving with a
// computer_use preflight+action from another is exactly the race the lock
// exists to prevent. A long computer_use `wait` (up to 120s) or a slow
// osascript (30s timeout) therefore stalls other routes' GUI calls; that is
// intentional — there is only one screen.
var computerUseGUIOperationMu sync.Mutex

func (t *ComputerUseTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "computer_use",
		Description: "Observe and operate existing macOS app windows through a stateful Accessibility-first workflow. " +
			"Start with get_app_state, or screenshot when a window image is needed, then use its state_id and element refs. Coordinates are valid only for the latest actionable target-window image; an observation marked visual-only supports verification but never coordinates. " +
			"Prefer select_text for AX text ranges; if AX reports fallback_required, re-observe before deciding whether an explicit drag is appropriate. " +
			"Pointer actions visibly move the real cursor. Use browser tools for web-page DOM interactions." + agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":      map[string]any{"type": "string", "description": "Action: get_app_state, screenshot, click, press, get_value, scroll, type, hotkey, move, drag, select_text, wait. screenshot observes and captures the exact target app window. Focus, launch, and direct value mutation are temporarily unavailable until they have target-bound execution and action-specific postcondition verification."},
				"description": agent.DescriptionFieldSpec,
				"state_id":    map[string]any{"type": "string", "description": "Latest state_id from get_app_state; required with ref, keyboard, and coordinate actions"},
				"app":         map[string]any{"type": "string", "description": "Target running macOS app name. Required on the first observation in unattended runs; an attended Quick Panel run may already bind its original foreground app"},
				"ref":         map[string]any{"type": "string", "description": "Element ref from the matching state_id. Type prefers the focused ref; after one verified left coordinate click, one same-window type may omit ref. Hotkey rejects ref"},
				"value":       map[string]any{"type": "string", "description": "Value for wait matching"},
				"x":           map[string]any{"type": "integer", "description": "X pixel index in the latest actionable get_app_state image; unavailable for visual-only images"},
				"y":           map[string]any{"type": "integer", "description": "Y pixel index in the latest actionable get_app_state image; unavailable for visual-only images"},
				"start_x":     map[string]any{"type": "integer", "description": "Drag start X pixel index in the latest actionable get_app_state image"},
				"start_y":     map[string]any{"type": "integer", "description": "Drag start Y pixel index in the latest actionable get_app_state image"},
				"end_x":       map[string]any{"type": "integer", "description": "Drag end X pixel index in the latest actionable get_app_state image"},
				"end_y":       map[string]any{"type": "integer", "description": "Drag end Y pixel index in the latest actionable get_app_state image"},
				"duration_ms": map[string]any{"type": "integer", "description": "Drag duration in milliseconds, 120-800 (default 350)"},
				"range": map[string]any{
					"type": "object", "description": "AX UTF-16 text range for select_text",
					"properties": map[string]any{
						"location": map[string]any{"type": "integer", "description": "Zero-based UTF-16 range location"},
						"length":   map[string]any{"type": "integer", "description": "Positive UTF-16 range length"},
					},
					"required": []string{"location", "length"},
				},
				"text":               map[string]any{"type": "string", "description": "Text for type; requires a focused ref or the one-shot focus from a verified coordinate click"},
				"keys":               map[string]any{"type": "string", "description": "Window-bound hotkey such as command+shift+p; requires latest state_id and no ref"},
				"button":             map[string]any{"type": "string", "enum": []string{"left", "right", "wheel", "back", "forward"}, "description": "Mouse button: left (default), right, wheel, back, or forward"},
				"clicks":             map[string]any{"type": "integer", "description": "Click count (default 1)"},
				"dx":                 map[string]any{"type": "integer", "description": "Horizontal semantic AX scroll steps from -10 to 10: dx > 0 uses AXIncrement to scroll right; dx < 0 uses AXDecrement to scroll left. Scroll requires exactly one non-zero axis plus latest state_id and ref"},
				"dy":                 map[string]any{"type": "integer", "description": "Vertical semantic AX scroll steps from -10 to 10: dy > 0 uses AXIncrement to scroll down; dy < 0 uses AXDecrement to scroll up. Scroll requires exactly one non-zero axis plus latest state_id and ref"},
				"condition":          map[string]any{"type": "string", "description": "Optional wait condition: elementExists, elementGone, titleContains, urlContains, titleChanged, urlChanged; omit with timeout for a simple bounded delay"},
				"query":              map[string]any{"type": "string", "description": "Element text query for wait"},
				"role":               map[string]any{"type": "string", "description": "AX role filter for wait"},
				"timeout":            map[string]any{"type": "number", "description": "Wait timeout seconds (default 10)"},
				"interval":           map[string]any{"type": "number", "description": "Wait poll interval seconds (default 0.5)"},
				"filter":             map[string]any{"type": "string", "description": "Observation filter: interactive (default) or all"},
				"semantic_budget":    map[string]any{"type": "integer", "description": "Accessibility-tree semantic budget (default 25)"},
				"include_screenshot": map[string]any{"type": "boolean", "description": "Attach the target window image to get_app_state"},
			},
		},
		Required: []string{"action", "description"},
	}
}

func (t *ComputerUseTool) RequiresApproval() bool { return true }

func computerUseObservationAction(action string) bool {
	switch action {
	case "get_app_state", "get_value", "screenshot", "wait":
		return true
	default:
		return false
	}
}

func computerUseTemporarilyUnavailableMutation(action string) bool {
	switch action {
	case "focus_app", "launch_app", "set_value":
		return true
	default:
		return false
	}
}

func parseComputerUseAction(argsJSON string) (string, bool) {
	var args struct {
		Action string `json:"action"`
	}
	if json.Unmarshal([]byte(argsJSON), &args) != nil || args.Action == "" {
		return "", false
	}
	return args.Action, true
}

func (t *ComputerUseTool) IsSafeArgs(argsJSON string) bool {
	action, ok := parseComputerUseAction(argsJSON)
	return ok && computerUseObservationAction(action)
}

func (t *ComputerUseTool) IsReadOnlyCall(argsJSON string) bool {
	action, ok := parseComputerUseAction(argsJSON)
	return ok && computerUseObservationAction(action)
}

// Even observations serialize within a turn. Each one replaces the single
// latest state_id/ref table, so concurrent "read-only" calls could otherwise
// make a sibling ref action stale nondeterministically.
func (t *ComputerUseTool) IsConcurrencySafeCall(string) bool { return false }

// requiresExplicitFirstTargetV1 prevents unattended workflows from resolving
// whichever app happens to be frontmost when their first observation runs.
// A bound Quick Panel target and a prior exact observation are already scoped.
func (t *ComputerUseTool) requiresExplicitFirstTargetV1(
	ctx context.Context,
	args computerUseArgs,
) bool {
	if t == nil || t.targetScope != computerUseTargetScopeExplicitV1 ||
		hasOpenAINativeComputerActionV1(ctx) ||
		strings.TrimSpace(args.App) != "" || t.initialTarget != nil || t.snapshot != nil ||
		t.coordinateFocus != nil {
		return false
	}
	switch args.Action {
	case "get_app_state", "screenshot":
		return true
	case "wait":
		return strings.TrimSpace(args.Condition) != ""
	default:
		return false
	}
}

func (t *ComputerUseTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	computerUseGUIOperationMu.Lock()
	defer computerUseGUIOperationMu.Unlock()
	return t.runWithGUIOperationLockHeld(ctx, argsJSON)
}

// runWithGUIOperationLockHeld is the one raw execution seam used by both the
// provider-neutral function tool and provider-native adapters. Callers must
// hold computerUseGUIOperationMu. Keeping mutation and its required strict
// post-observation under one lock prevents another route from changing the
// frontmost app between those two operations.
func (t *ComputerUseTool) runWithGUIOperationLockHeld(
	ctx context.Context,
	argsJSON string,
) (agent.ToolResult, error) {
	if runtime.GOOS != "darwin" || t.client == nil {
		return agent.BusinessError("computer_use is only available on macOS with ax_server"), nil
	}

	var args computerUseArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Action == "" {
		return agent.ValidationError("missing required parameter: action"), nil
	}
	if (args.Action == "pixel_scroll" || args.Action == "keypress" ||
		len(args.Modifiers) > 0 || len(args.KeySequence) > 0) &&
		!hasOpenAINativeComputerActionV1(ctx) {
		return agent.ValidationError(
			"native key/modifier actions are restricted to an admitted OpenAI native computer action"), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("missing required parameter: description"), nil
	}
	if _, _, err := computerUseExecutionPresentationV1(ctx, args); err != nil {
		return agent.ValidationError(err.Error()), nil
	}
	if t.requiresExplicitFirstTargetV1(ctx, args) {
		return agent.BusinessError(
			"computer_use requires an explicit app for the first unattended observation; retry with the app field",
		), nil
	}
	if _, present := consequentialRiskExecutionFromContextV1(ctx); present &&
		!computerUseObservationAction(args.Action) &&
		args.Action != "press" && args.Action != "click" {
		// A one-shot consequential grant is scoped to the only two paths that
		// rederive exact target authority at the commit boundary. Drag and
		// keyboard actions must fail before any GUI side effect rather than
		// silently ignoring or borrowing a click/press confirmation. Read-only
		// actions remain available so a confirmed mutation can perform its
		// mandatory strict post-observation in the same context.
		return consequentialRiskToolFailureV1(ConsequentialRiskCodeGrantMismatchV1), nil
	}
	if computerUseTemporarilyUnavailableMutation(args.Action) {
		return agent.BusinessError(fmt.Sprintf(
			"computer_use action %q is temporarily unavailable until target-bound execution and action-specific verification are implemented",
			args.Action,
		)), nil
	}
	if args.SemanticBudget < 0 || args.SemanticBudget > 100 {
		return agent.ValidationError("semantic_budget must be between 0 and 100 (0 or omitted uses the default of 25)"), nil
	}
	if args.Timeout > 120 {
		return agent.ValidationError("timeout must not exceed 120 seconds"), nil
	}
	if args.Interval > 10 {
		return agent.ValidationError("interval must not exceed 10 seconds"), nil
	}
	if args.Clicks < 0 || args.Clicks > 3 {
		return agent.ValidationError("clicks must be between 0 and 3 (0 or omitted means a single click)"), nil
	}
	if t.coordinateFocus != nil && args.Action != "type" &&
		args.Action != "get_app_state" && args.Action != "screenshot" {
		t.coordinateFocus = nil
	}
	if t.navigationCommit != nil &&
		args.Action != "get_app_state" &&
		args.Action != "screenshot" &&
		!computerUsePlainReturnKeypressV1(args) {
		t.navigationCommit = nil
	}

	switch args.Action {
	case "get_app_state":
		return t.getAppState(ctx, args)
	case "click":
		return t.click(ctx, args)
	case "press":
		return t.semanticPress(ctx, args)
	case "get_value":
		return t.refAction(ctx, args, "get_value", true)
	case "scroll":
		return t.scroll(ctx, args)
	case "type":
		return t.typeText(ctx, args)
	case "hotkey":
		return t.hotkey(ctx, args)
	case "keypress":
		return t.keypress(ctx, args)
	case "move":
		return t.move(ctx, args)
	case "drag":
		return t.drag(ctx, args)
	case "pixel_scroll":
		return t.pixelScroll(ctx, args)
	case "select_text":
		return t.selectText(ctx, args)
	case "wait":
		return t.wait(ctx, args)
	case "screenshot":
		return t.screenshot(ctx, args)
	default:
		return agent.ValidationError(fmt.Sprintf("unknown action: %q", args.Action)), nil
	}
}

func (t *ComputerUseTool) resolvePID(ctx context.Context, app string) (int, agent.ToolResult, bool) {
	if app == "" {
		return 0, agent.ToolResult{}, true
	}
	if !ValidAppNamePattern.MatchString(app) {
		return 0, agent.ValidationError(fmt.Sprintf("invalid app name %q", app)), false
	}
	raw, err := t.client.Call(ctx, "resolve_pid", map[string]any{"app_name": app})
	if err != nil {
		return 0, computerUseCallError(fmt.Sprintf("resolve app %q", app), err), false
	}
	var result struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(raw, &result); err != nil || result.PID <= 0 {
		return 0, agent.BusinessError(fmt.Sprintf("could not resolve running app %q", app)), false
	}
	return result.PID, agent.ToolResult{}, true
}

func (t *ComputerUseTool) readTree(ctx context.Context, pid int, filter string, budget int) (computerUseTree, agent.ToolResult, bool) {
	if filter == "" {
		filter = "interactive"
	}
	if filter != "interactive" && filter != "all" {
		return computerUseTree{}, agent.ValidationError("filter must be 'interactive' or 'all'"), false
	}
	if budget <= 0 {
		budget = 25
	}
	params := map[string]any{"filter": filter, "semantic_budget": budget}
	if pid > 0 {
		params["pid"] = pid
	}
	raw, err := t.client.Call(ctx, "read_tree", params)
	if err != nil {
		windowRaw, windowErr := t.client.Call(ctx, "read_window_target", params)
		if windowErr != nil {
			return computerUseTree{}, computerUseCallError("observe app", err), false
		}
		raw = windowRaw
	}
	tree, err := decodeComputerUseTree(raw)
	if err != nil {
		return computerUseTree{}, agent.BusinessError(fmt.Sprintf("parse accessibility state: %v", err)), false
	}
	// A successful AX read can still omit the exact CGWindow identity needed
	// for screenshots and coordinate actions. In that case use the independent
	// CGWindow observation as the whole target rather than mixing AX refs from
	// one window with coordinate authority from another.
	if !computerUseTreeHasCoordinateTargetV1(tree) {
		windowParams := params
		if tree.PID > 0 {
			windowParams = map[string]any{
				"filter":          filter,
				"semantic_budget": budget,
				"pid":             tree.PID,
			}
		}
		if windowRaw, windowErr := t.client.Call(
			ctx,
			"read_window_target",
			windowParams,
		); windowErr == nil {
			if windowTree, decodeErr := decodeComputerUseTree(windowRaw); decodeErr == nil &&
				computerUseTreeHasCoordinateTargetV1(windowTree) {
				tree = windowTree
			}
		}
	}
	if tree.PID <= 0 {
		return computerUseTree{}, agent.BusinessError("accessibility state did not include a valid pid"), false
	}
	return tree, agent.ToolResult{}, true
}

func computerUseTreeHasCoordinateTargetV1(tree computerUseTree) bool {
	return tree.SchemaVersion == 1 &&
		tree.PID > 0 &&
		strings.TrimSpace(tree.BundleID) != "" &&
		tree.WindowID != nil && *tree.WindowID > 0 &&
		tree.WindowFrame != nil &&
		tree.WindowFrame.Width > 0 &&
		tree.WindowFrame.Height > 0
}

func computerUseStateID(tree computerUseTree) string {
	canonical, _ := json.Marshal(tree)
	sum := sha256.Sum256(canonical)
	return "s_" + hex.EncodeToString(sum[:8])
}

func computerUseSignatures(elements []computerUseElement) map[string]string {
	result := make(map[string]string)
	var walk func(computerUseElement)
	walk = func(node computerUseElement) {
		if node.Ref != "" {
			copyNode := node
			copyNode.Children = nil
			encoded, _ := json.Marshal(copyNode)
			result[node.Ref] = string(encoded)
		}
		for _, child := range node.Children {
			walk(child)
		}
	}
	for _, element := range elements {
		walk(element)
	}
	return result
}

func computerUseDiff(before, after map[string]string) (added, removed, changed int) {
	for ref, signature := range after {
		old, ok := before[ref]
		if !ok {
			added++
		} else if old != signature {
			changed++
		}
	}
	for ref := range before {
		if _, ok := after[ref]; !ok {
			removed++
		}
	}
	return added, removed, changed
}

func (t *ComputerUseTool) getAppState(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	// Any new observation invalidates coordinate authority immediately. A new
	// artifact is installed only after tree A -> topology -> capture ->
	// finalizer -> tree B succeeds as one exact-window transaction.
	t.coordinateArtifact = nil
	t.semanticImageArtifact = nil
	pendingFocus := t.coordinateFocus
	t.coordinateFocus = nil

	pid := 0
	var expectedPID int
	var expectedBundleID string
	if args.App == "" {
		if t.snapshot != nil {
			pid = t.snapshot.pid
			expectedPID = t.snapshot.pid
			expectedBundleID = t.snapshot.bundleID
		} else if pendingFocus != nil && !args.FollowFrontmost {
			pid = pendingFocus.pid
			expectedPID = pendingFocus.pid
			expectedBundleID = pendingFocus.bundleID
		} else if t.initialTarget != nil {
			pid = t.initialTarget.PID
			expectedPID = t.initialTarget.PID
			expectedBundleID = t.initialTarget.BundleID
		}
	} else {
		var failure agent.ToolResult
		var ok bool
		pid, failure, ok = t.resolvePID(ctx, args.App)
		if !ok {
			return failure, nil
		}
	}
	filter := args.Filter
	if filter == "" {
		filter = "interactive"
	}
	budget := int(args.SemanticBudget)
	if budget <= 0 {
		budget = 25
	}
	tree, failure, ok := t.readTree(ctx, pid, filter, budget)
	if !ok {
		return failure, nil
	}
	if expectedPID > 0 && (tree.PID != expectedPID ||
		strings.TrimSpace(tree.BundleID) != expectedBundleID) {
		return agent.BusinessError(
			"computer_use target identity changed before observation; re-observe the intended app",
		), nil
	}
	result, stateID := t.publishComputerUseObservation(tree, filter, budget)
	if pendingFocus != nil && pendingFocus.matchesTree(tree) {
		pendingFocus.stateID = stateID
		pendingFocus.filter = filter
		pendingFocus.budget = budget
		t.coordinateFocus = pendingFocus
	}
	if !args.IncludeScreenshot {
		return result, nil
	}

	artifact, failure, ok := t.captureCoordinateObservationV1(
		ctx,
		tree,
		stateID,
		filter,
		budget,
	)
	if !ok {
		if computerUseAllowsVisualOnlyFallbackV1(failure) {
			capture, visualErr := t.captureVisualOnlyObservationV1(ctx, tree)
			if visualErr == nil {
				result.Images = []agent.ImageBlock{capture.Image}
				result.GUIObservation.CoordinateActionable = false
				if failure.GUIOutcome != nil {
					result.GUIObservation.ActionabilityFailureCode =
						failure.GUIOutcome.FailureCode
				}
				if failure.GUIOutcome != nil &&
					failure.GUIOutcome.FailureCode == "display_not_actionable" {
					currentTree, _, currentOK := t.readTree(
						ctx,
						tree.PID,
						filter,
						budget,
					)
					if currentOK &&
						computerUseCoordinateTreesStableV1(tree, currentTree) {
						semanticArtifact, semanticErr :=
							newComputerUseSemanticImageArtifactV1(
								tree,
								stateID,
								capture,
								t.computerUseCoordinateNowV1(),
							)
						if semanticErr == nil {
							t.semanticImageArtifact = semanticArtifact
							result.GUIObservation.SemanticActionable = true
						}
					}
				}
				result.Content += "\ncoordinate_notice: exact window screenshot is visual-only; coordinate actions require a new fully actionable observation"
				result.GUICaptureDiagnostics = failure.GUICaptureDiagnostics
				return result, nil
			}
		}
		if args.Action == "screenshot" {
			return failure, nil
		}
		result.Content += "\nscreenshot_warning: " + failure.Content
		result.GUICaptureDiagnostics = failure.GUICaptureDiagnostics
		return result, nil
	}
	t.coordinateArtifact = &artifact
	result.Images = []agent.ImageBlock{artifact.ImageBlock()}
	result.GUIObservation.CoordinateActionable = true
	result.GUIObservation.SemanticActionable = true
	return result, nil
}

func computerUseAllowsVisualOnlyFallbackV1(
	failure agent.ToolResult,
) bool {
	if failure.GUIOutcome == nil {
		return false
	}
	switch failure.GUIOutcome.FailureCode {
	case "display_not_actionable", "image_dimensions_mismatch":
		return true
	default:
		return false
	}
}

// captureVisualOnlyObservationV1 captures the same exact PID/window/bounds
// without minting a CoordinateWindowArtifact. It is used only when the pixels
// remain useful for visual verification but the coordinate transform cannot be
// admitted safely (for example, a window spanning displays). Future coordinate
// actions therefore still fail closed until a new actionable observation.
func (t *ComputerUseTool) captureVisualOnlyObservationV1(
	ctx context.Context,
	tree computerUseTree,
) (exactVisualOnlyWindowCapture, error) {
	if t == nil || t.client == nil || tree.PID <= 0 ||
		tree.WindowID == nil || *tree.WindowID <= 0 ||
		tree.WindowFrame == nil {
		return exactVisualOnlyWindowCapture{},
			fmt.Errorf("visual-only capture requires exact window identity")
	}
	raw, err := t.client.Call(ctx, "capture_window", map[string]any{
		"pid":       tree.PID,
		"window_id": *tree.WindowID,
		"expected_quartz_bounds": CoordinateQuartzRectV1{
			X: tree.WindowFrame.X, Y: tree.WindowFrame.Y,
			Width: tree.WindowFrame.Width, Height: tree.WindowFrame.Height,
		},
		// Zero explicitly disables annotation/content-signature work in the
		// helper. Exact identity and bounds are still checked both before and
		// after screencapture.
		"max_labels": 0,
	})
	if err != nil {
		return exactVisualOnlyWindowCapture{}, err
	}
	var result exactAccessibilityWindowResult
	if err := json.Unmarshal(raw, &result); err != nil {
		return exactVisualOnlyWindowCapture{},
			fmt.Errorf("decode visual-only exact window: %w", err)
	}
	return decodeExactVisualOnlyWindowCapture(result)
}

func (t *ComputerUseTool) publishComputerUseObservation(
	tree computerUseTree,
	filter string,
	budget int,
) (agent.ToolResult, string) {
	id := computerUseStateID(tree)
	signatures := computerUseSignatures(tree.Elements)
	status := "initial"
	added, removed, changed := len(signatures), 0, 0
	if t.snapshot != nil {
		if t.snapshot.id == id {
			status = "unchanged"
			added = 0
		} else if t.snapshot.pid == tree.PID && t.snapshot.filter == filter && t.snapshot.budget == budget {
			status = "changed"
			added, removed, changed = computerUseDiff(t.snapshot.signatures, signatures)
		}
		// Different app/filter/budget: the previous snapshot observed another
		// scope, so a ref-level diff would report meaningless added/removed
		// counts. Treat the observation as a fresh "initial" baseline.
	}

	var expectedWindowAXBounds *CoordinateQuartzRectV1
	if tree.WindowFrame != nil {
		bounds := CoordinateQuartzRectV1{
			X: tree.WindowFrame.X, Y: tree.WindowFrame.Y,
			Width: tree.WindowFrame.Width, Height: tree.WindowFrame.Height,
		}
		expectedWindowAXBounds = &bounds
	}
	focusedRef := ""
	if tree.FocusedRef != nil {
		focusedRef = *tree.FocusedRef
	}
	t.snapshot = &computerUseSnapshot{
		id:                     id,
		status:                 status,
		app:                    tree.App,
		bundleID:               tree.BundleID,
		pid:                    tree.PID,
		window:                 tree.Window,
		windowID:               tree.WindowID,
		expectedWindowAXBounds: expectedWindowAXBounds,
		focusedRef:             focusedRef,
		filter:                 filter,
		budget:                 budget,
		elements:               tree.Elements,
		signatures:             signatures,
		typed:                  tree.SchemaVersion == 1,
	}
	t.refs = make(map[string]refEntry, len(tree.RefPaths))
	for ref, entry := range tree.RefPaths {
		t.refs[ref] = refEntry{
			path: entry.Path, role: entry.Role, fingerprint: entry.Fingerprint, pid: tree.PID,
		}
	}

	lines := []string{
		"state_id: " + id,
		"app: " + tree.App,
		fmt.Sprintf("pid: %d", tree.PID),
		"window: " + tree.Window,
		"status: " + status,
		fmt.Sprintf("diff: added=%d removed=%d changed=%d", added, removed, changed),
	}
	lines = append(lines, "elements:")
	lines = append(lines, formatComputerUseElements(tree.Elements)...)

	return agent.ToolResult{
		Content:        strings.Join(lines, "\n"),
		GUIObservation: &agent.GUIObservationOutcome{},
	}, id
}

func (t *ComputerUseTool) captureCoordinateObservationV1(
	ctx context.Context,
	treeA computerUseTree,
	stateID string,
	filter string,
	budget int,
) (CoordinateWindowArtifactV1, agent.ToolResult, bool) {
	captureRequest, failure, ok := computerUseCoordinateCaptureRequestV1(treeA)
	if !ok {
		return CoordinateWindowArtifactV1{}, failure, false
	}
	topology, err := ReadDisplayTopologyV1(ctx, t.client)
	if err != nil {
		return CoordinateWindowArtifactV1{}, computerUseCallError("read display topology", err), false
	}
	captureRequest.TopologyRef = CoordinateTopologyRefV1{
		TopologyID: topology.TopologyID,
		Generation: topology.Generation,
	}
	rawCapture, err := t.client.Call(ctx, "capture_coordinate_window", captureRequest)
	if err != nil {
		return CoordinateWindowArtifactV1{}, computerUseCallError("capture exact window", err), false
	}
	if failure, failed := computerUseCaptureFailureV1(rawCapture, treeA); failed {
		return CoordinateWindowArtifactV1{}, failure, false
	}
	frameID, err := t.computerUseCoordinateFrameIDV1()
	if err != nil {
		return CoordinateWindowArtifactV1{}, agent.BusinessError("create coordinate frame identity: " + err.Error()), false
	}
	artifact, err := FinalizeCoordinateWindowV1(CoordinateWindowFinalizeInputV1{
		CapturePayload:  rawCapture,
		CaptureRequest:  captureRequest,
		CurrentTopology: topology,
		CaptureLimits:   t.computerUseCoordinateCaptureLimitsV1(),
		StateID:         stateID,
		Profile:         t.computerUseCoordinateProfileV1(),
		Now:             t.computerUseCoordinateNowV1(),
		TTL:             computerUseCoordinateFrameTTLV1,
		FrameID:         frameID,
	})
	if err != nil {
		return CoordinateWindowArtifactV1{}, agent.BusinessError("admit coordinate screenshot: " + err.Error()), false
	}
	treeB, treeFailure, ok := t.readTree(ctx, treeA.PID, filter, budget)
	if !ok {
		return CoordinateWindowArtifactV1{}, treeFailure, false
	}
	if !computerUseCoordinateTreesStableV1(treeA, treeB) {
		return CoordinateWindowArtifactV1{}, agent.BusinessError(
			"window identity changed during screenshot capture; coordinate image was discarded"), false
	}
	frame := artifact.Frame()
	createdAt, createdErr := time.Parse(time.RFC3339Nano, frame.CreatedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, frame.ExpiresAt)
	publishedAt := t.computerUseCoordinateNowV1()
	if createdErr != nil || expiresErr != nil || publishedAt.Before(createdAt) || !publishedAt.Before(expiresAt) {
		return CoordinateWindowArtifactV1{}, agent.BusinessError(
			"coordinate screenshot authority was not current after window identity verification"), false
	}
	return artifact, agent.ToolResult{}, true
}

func computerUseCoordinateCaptureRequestV1(
	tree computerUseTree,
) (CaptureCoordinateWindowRequestV1, agent.ToolResult, bool) {
	app := strings.TrimSpace(tree.AppName)
	if app == "" {
		app = strings.TrimSpace(tree.App)
	}
	if app == "" {
		app = "the target app"
	}
	if tree.SchemaVersion != 1 || tree.PID <= 0 || tree.BundleID == "" ||
		tree.BundleID != strings.TrimSpace(tree.BundleID) {
		return CaptureCoordinateWindowRequestV1{}, agent.BusinessError(
			"computer_use_error: target_identity_unavailable\nmessage: the target app did not provide stable Accessibility identity\nrecovery: re-open the app and retry the observation"), false
	}
	if tree.WindowID == nil || *tree.WindowID <= 0 ||
		uint64(*tree.WindowID) > uint64(^uint32(0)) || tree.WindowFrame == nil {
		return CaptureCoordinateWindowRequestV1{}, agent.BusinessError(fmt.Sprintf(
			"computer_use_error: window_not_found\nmessage: %s has no unique capturable window\nrecovery: open one normal app window, bring it forward, and retry",
			app,
		)), false
	}
	bounds := CoordinateQuartzRectV1{
		X: tree.WindowFrame.X, Y: tree.WindowFrame.Y,
		Width: tree.WindowFrame.Width, Height: tree.WindowFrame.Height,
	}
	if err := validateCoordinateQuartzRect("AX window_frame", bounds); err != nil {
		return CaptureCoordinateWindowRequestV1{}, agent.BusinessError(err.Error()), false
	}
	return CaptureCoordinateWindowRequestV1{
		SchemaVersion: 1,
		PID:           tree.PID, BundleID: tree.BundleID, WindowID: uint32(*tree.WindowID),
		ExpectedQuartzBounds: bounds,
	}, agent.ToolResult{}, true
}

func computerUseCaptureFailureV1(
	payload []byte,
	tree computerUseTree,
) (agent.ToolResult, bool) {
	result, err := DecodeCaptureCoordinateWindowResultV1(payload)
	if err != nil || result.Status != "failed" || result.FailureCode == nil {
		return agent.ToolResult{}, false
	}
	code := *result.FailureCode
	app := strings.TrimSpace(tree.AppName)
	if app == "" {
		app = strings.TrimSpace(tree.App)
	}
	if app == "" {
		app = "the target app"
	}
	var message, recovery string
	switch code {
	case "window_not_found":
		message = app + " has no capturable window"
		recovery = "open one normal app window, bring it forward, and retry"
	case "window_not_actionable":
		message = app + "'s target window is hidden, minimized, or transient"
		recovery = "make one normal window fully visible on an active display and retry"
	case "display_not_actionable":
		message = app + "'s target window is not fully contained in one supported active display"
		recovery = "make one normal window fully visible on an active display and retry"
	case "window_bounds_mismatch", "window_changed", "topology_changed", "stale_topology":
		message = app + "'s window or display changed during capture"
		recovery = "stop moving or resizing the window, then retry once"
	case "capture_timeout", "capture_failed", "topology_unavailable":
		message = "the exact target window could not be captured"
		recovery = "keep the window visible and retry once"
	case "process_identity_mismatch", "window_identity_mismatch":
		message = app + "'s process or window identity changed before capture"
		recovery = "re-observe the app before retrying"
	default:
		message = "the exact target window capture was rejected"
		recovery = "re-observe the app and use Accessibility refs if the window still cannot be captured"
	}
	content := fmt.Sprintf(
		"computer_use_error: %s\nmessage: %s\nrecovery: %s",
		code,
		message,
		recovery,
	)
	var diagnostics *agent.GUICaptureDiagnostics
	if result.FailureDiagnostics != nil {
		value := result.FailureDiagnostics
		diagnostics = &agent.GUICaptureDiagnostics{
			Stage:              value.Stage,
			PID:                value.PID,
			BundleID:           value.BundleID,
			WindowID:           value.WindowID,
			PreWindowBounds:    guiCaptureRectV1(value.PreWindowQuartzBounds),
			PostWindowBounds:   guiCaptureRectV1(value.PostWindowQuartzBounds),
			DisplayID:          value.DisplayID,
			BackingScaleFactor: value.BackingScaleFactor,
			ExpectedWidthPX:    value.ExpectedWidthPX,
			ExpectedHeightPX:   value.ExpectedHeightPX,
		}
		if value.MetadataWidthPX != nil {
			diagnostics.MetadataWidthPX = *value.MetadataWidthPX
		}
		if value.MetadataHeightPX != nil {
			diagnostics.MetadataHeightPX = *value.MetadataHeightPX
		}
		if value.DecodedWidthPX != nil {
			diagnostics.DecodedWidthPX = *value.DecodedWidthPX
		}
		if value.DecodedHeightPX != nil {
			diagnostics.DecodedHeightPX = *value.DecodedHeightPX
		}
	}
	if result.DisplayDiagnostics != nil {
		value := result.DisplayDiagnostics
		diagnostics = &agent.GUICaptureDiagnostics{
			Stage:                  "display_actionability",
			PID:                    value.PID,
			BundleID:               value.BundleID,
			WindowID:               value.WindowID,
			PreWindowBounds:        guiCaptureRectV1(value.WindowQuartzBounds),
			PostWindowBounds:       guiCaptureRectV1(value.WindowQuartzBounds),
			ActionableDisplayCount: value.ActionableDisplayCount,
			DisplayCandidates: make(
				[]agent.GUIDisplayActionabilityDiagnostics,
				0,
				len(value.Displays),
			),
		}
		for _, display := range value.Displays {
			diagnostics.DisplayCandidates = append(
				diagnostics.DisplayCandidates,
				agent.GUIDisplayActionabilityDiagnostics{
					DisplayID:           display.DisplayID,
					QuartzBounds:        guiCaptureRectV1(display.QuartzBounds),
					IsActive:            display.IsActive,
					IsOnline:            display.IsOnline,
					IsAsleep:            display.IsAsleep,
					IsMirrorFollower:    display.IsMirrorFollower,
					RotationDegrees:     display.RotationDegrees,
					FullyContainsWindow: display.FullyContainsWindow,
					FailedPredicates: append(
						[]string(nil),
						display.FailedPredicates...,
					),
				},
			)
		}
	}
	if result.RetrySafe {
		failure := agent.TransientError(content)
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result:      agent.GUIActionResultFailed,
			Phase:       agent.GUIActionPhaseObserving,
			FailureCode: code,
		}
		failure.GUICaptureDiagnostics = diagnostics
		return failure, true
	}
	failure := agent.BusinessError(content)
	failure.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultFailed,
		Phase:       agent.GUIActionPhaseObserving,
		FailureCode: code,
	}
	failure.GUICaptureDiagnostics = diagnostics
	return failure, true
}

func guiCaptureRectV1(rect CoordinateQuartzRectV1) agent.GUICaptureRect {
	return agent.GUICaptureRect{
		X: rect.X, Y: rect.Y, Width: rect.Width, Height: rect.Height,
	}
}

func computerUseCoordinateTreesStableV1(before, after computerUseTree) bool {
	if before.PID != after.PID || before.BundleID != after.BundleID ||
		before.WindowFrame == nil || after.WindowFrame == nil ||
		*before.WindowFrame != *after.WindowFrame ||
		before.WindowID == nil || after.WindowID == nil || *before.WindowID != *after.WindowID {
		return false
	}
	return true
}

func (t *ComputerUseTool) computerUseCoordinateProfileV1() CoordinateImageProfileV1 {
	if t.coordinateProfile.SchemaVersion == 0 {
		return defaultComputerUseCoordinateImageProfileV1()
	}
	return t.coordinateProfile
}

func (t *ComputerUseTool) computerUseCoordinateCaptureLimitsV1() CaptureCoordinateWindowLimitsV1 {
	if t.coordinateCaptureLimits.MaxRawBytes <= 0 ||
		t.coordinateCaptureLimits.MaxNDJSONBytes <= 0 ||
		t.coordinateCaptureLimits.MaxPixels <= 0 {
		return defaultComputerUseCoordinateCaptureLimitsV1()
	}
	return t.coordinateCaptureLimits
}

func (t *ComputerUseTool) computerUseCoordinateNowV1() time.Time {
	if t.coordinateNow != nil {
		return t.coordinateNow().UTC()
	}
	return time.Now().UTC()
}

func (t *ComputerUseTool) computerUseCoordinateFrameIDV1() (string, error) {
	if t.coordinateFrameID != nil {
		return t.coordinateFrameID()
	}
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		return "", err
	}
	return "frame_" + hex.EncodeToString(bytes), nil
}

func formatComputerUseElements(elements []computerUseElement) []string {
	lines := make([]string, 0, len(elements))
	var walk func(computerUseElement, int)
	walk = func(node computerUseElement, depth int) {
		fields := make([]string, 0, 8)
		fields = append(fields, fmt.Sprintf("ref=%q", node.Ref), fmt.Sprintf("role=%q", node.Role))
		for _, item := range []struct {
			key   string
			field *string
		}{
			{key: "subrole", field: node.Subrole},
			{key: "title", field: node.Title},
			{key: "desc", field: firstComputerUseString(node.Description, node.Desc)},
			{key: "value", field: node.Value},
		} {
			key, field := item.key, item.field
			if field != nil && *field != "" {
				fields = append(fields, fmt.Sprintf("%s=%q", key, *field))
			}
		}
		if node.ValueRedacted {
			fields = append(fields, "value_redacted=true")
		}
		// Preserve the legacy presentation contract: enabled is the ordinary
		// state and stays quiet; an explicitly disabled control is surfaced.
		// A nil pointer means the schema-v0 producer omitted the field, not that
		// the control was disabled.
		if node.Enabled != nil && !*node.Enabled {
			fields = append(fields, "enabled=false")
		}
		if node.Focused {
			fields = append(fields, "focused=true")
		}
		if node.Selected {
			fields = append(fields, "selected=true")
		}
		if len(fields) > 0 {
			lines = append(lines, strings.Repeat("  ", depth)+"- "+strings.Join(fields, " "))
		}
		for _, child := range node.Children {
			walk(child, depth+1)
		}
	}
	for _, element := range elements {
		walk(element, 0)
	}
	if len(lines) == 0 {
		return []string{"- (no accessible elements)"}
	}
	return lines
}

func firstComputerUseString(values ...*string) *string {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func (t *ComputerUseTool) preflightRef(ctx context.Context, args computerUseArgs) (refEntry, agent.ToolResult, bool) {
	if args.Ref == "" {
		return refEntry{}, agent.ValidationError(fmt.Sprintf("%s requires 'ref'", args.Action)), false
	}
	if args.StateID == "" {
		return refEntry{}, agent.ValidationError(fmt.Sprintf("%s requires 'state_id' with ref", args.Action)), false
	}
	if t.snapshot == nil || args.StateID != t.snapshot.id {
		return refEntry{}, agent.BusinessError("stale state_id or no active state; call get_app_state again"), false
	}
	entry, exists := t.refs[args.Ref]
	if !exists {
		return refEntry{}, agent.BusinessError(fmt.Sprintf("unknown ref %q for state_id %s; call get_app_state again", args.Ref, args.StateID)), false
	}

	current, failure, ok := t.readTree(ctx, t.snapshot.pid, t.snapshot.filter, t.snapshot.budget)
	if !ok {
		return refEntry{}, failure, false
	}
	if computerUseStateID(current) != t.snapshot.id {
		t.invalidateState()
		return refEntry{}, agent.BusinessError("stale state detected before GUI action; call get_app_state again"), false
	}
	return entry, agent.ToolResult{}, true
}

func (t *ComputerUseTool) refAction(ctx context.Context, args computerUseArgs, method string, keepState bool) (agent.ToolResult, error) {
	entry, failure, ok := t.preflightRef(ctx, args)
	if !ok {
		return failure, nil
	}
	params := map[string]any{"pid": entry.pid, "path": entry.path}
	if entry.role != "" && method != "get_value" {
		params["expected_role"] = entry.role
	}
	if args.Value != nil {
		params["value"] = *args.Value
	}
	raw, err := t.client.Call(ctx, method, params)
	if err != nil {
		return computerUseCallError(method, err), nil
	}
	if !keepState {
		t.invalidateState()
	}
	return agent.ToolResult{Content: computerUseActionMessage(raw, method+" completed")}, nil
}

func (t *ComputerUseTool) click(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	if args.Ref != "" {
		return t.semanticPress(ctx, args)
	}
	return t.coordinatePointerActionV1(ctx, args)
}

type computerUseSemanticPressResult struct {
	Status         string  `json:"status"`
	PressCommitted *bool   `json:"press_committed"`
	Phase          string  `json:"phase"`
	FailureCode    *string `json:"failure_code"`
	Postcondition  *string `json:"postcondition"`
	RetrySafe      *bool   `json:"retry_safe"`
}

func validateComputerUseSemanticPressResult(result computerUseSemanticPressResult) error {
	if result.PressCommitted == nil || result.RetrySafe == nil {
		return errors.New("semantic_press omitted required boolean fields")
	}
	if *result.RetrySafe {
		return errors.New("semantic_press must never claim an automatic retry is safe")
	}
	switch result.Status {
	case "completed_unverified":
		if !*result.PressCommitted || result.Phase != "post_observation" || result.FailureCode == nil ||
			(*result.FailureCode != "postcondition_not_observed" && *result.FailureCode != "postcondition_not_declared") ||
			result.Postcondition != nil {
			return errors.New("incoherent completed_unverified semantic_press result")
		}
	case "failed":
		if *result.PressCommitted || (result.Phase != "preflight" && result.Phase != "action") ||
			result.FailureCode == nil || *result.FailureCode == "" || result.Postcondition != nil {
			return errors.New("incoherent failed semantic_press result")
		}
	default:
		return fmt.Errorf("unknown semantic_press status %q", result.Status)
	}
	return nil
}

func decodeComputerUseSemanticPressResult(payload []byte) (computerUseSemanticPressResult, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(payload, &fields); err != nil {
		return computerUseSemanticPressResult{}, err
	}
	for _, required := range []string{
		"status", "press_committed", "phase", "failure_code", "postcondition", "retry_safe",
	} {
		if _, exists := fields[required]; !exists {
			return computerUseSemanticPressResult{}, fmt.Errorf("semantic_press omitted required field %q", required)
		}
	}
	var result computerUseSemanticPressResult
	if err := json.Unmarshal(payload, &result); err != nil {
		return computerUseSemanticPressResult{}, err
	}
	if err := validateComputerUseSemanticPressResult(result); err != nil {
		return computerUseSemanticPressResult{}, err
	}
	return result, nil
}

// legacySemanticPressV1ParamsForFixture keeps the historical loose decoder
// fixture testable. ComputerUse never sends this shape; production mutations
// use SemanticPressRequestV2 and AXClient.SemanticPressV2.
func legacySemanticPressV1ParamsForFixture(windowID int, entry refEntry) map[string]any {
	return map[string]any{
		"pid":                  entry.pid,
		"window_id":            windowID,
		"path":                 entry.path,
		"expected_role":        entry.role,
		"expected_fingerprint": entry.fingerprint,
		"fallback_policy":      "none",
	}
}

func (t *ComputerUseTool) semanticPress(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	// Generic calls consume their observation. A trusted provider batch keeps
	// its source observation for later ordered actions; every semantic target
	// is still re-resolved and revalidated at its own commit boundary.
	defer t.invalidateStateAfterMutationV1(ctx)

	entry, failure, ok := t.preflightRef(ctx, args)
	if !ok {
		return failure, nil
	}
	if t.snapshot == nil || t.snapshot.windowID == nil || *t.snapshot.windowID <= 0 {
		return agent.BusinessError("semantic press requires a unique window identity; call get_app_state again"), nil
	}
	if entry.fingerprint == "" {
		return agent.BusinessError("semantic press requires a typed element fingerprint; call get_app_state again"), nil
	}
	if t.snapshot.bundleID == "" || t.snapshot.bundleID != strings.TrimSpace(t.snapshot.bundleID) ||
		uint64(*t.snapshot.windowID) > uint64(^uint32(0)) {
		return agent.BusinessError(
			"semantic press requires typed bundle and window authority; call get_app_state again"), nil
	}
	request := SemanticPressRequestV2{
		SchemaVersion:       2,
		PID:                 entry.pid,
		BundleID:            t.snapshot.bundleID,
		WindowID:            uint32(*t.snapshot.windowID),
		Ref:                 args.Ref,
		Path:                entry.path,
		ExpectedRole:        entry.role,
		ExpectedFingerprint: entry.fingerprint,
		FallbackPolicy:      "none",
		InterferencePolicy:  "global_physical",
		CommitDeadlineAt:    t.computerUseCoordinateNowV1().Add(time.Second).UTC().Format(time.RFC3339Nano),
	}
	if args.ExecutionLane ==
		string(guicontrol.ComputerUseExecutionBackgroundSemantic) {
		request.InterferencePolicy = "target_foreground"
	}
	if err := request.Validate(); err != nil {
		return agent.BusinessError(
			"semantic press authority is invalid; call get_app_state again"), nil
	}
	riskRequestID := ""
	if execution, present := consequentialRiskExecutionFromContextV1(ctx); present {
		riskRequestID = execution.RequestID
	} else if invocation, present := agent.ToolInvocationFromContext(ctx); present {
		riskRequestID = invocation.ToolUseID
	}
	riskPreflight, err := t.PreflightConsequentialRiskV1(ctx, mustMarshalComputerUseArgsV1(args), riskRequestID)
	if err != nil || riskPreflight.Status == ConsequentialRiskPreflightBlockedV1 {
		return consequentialRiskToolFailureV1(ConsequentialRiskCodeGrantMismatchV1), nil
	}
	riskExecution, riskErr := validateConsequentialRiskExecutionV1(ctx, riskPreflight)
	if riskErr != nil {
		code := ConsequentialRiskCodeGrantMismatchV1
		if errors.Is(riskErr, ErrConsequentialRiskExecutionMissingV1) {
			code = ConsequentialRiskCodeMissingGrantV1
		}
		return consequentialRiskToolFailureV1(code), nil
	}
	if riskPreflight.Status == ConsequentialRiskPreflightRequiredV1 {
		request.RiskDestinationAssertion = &SemanticPressRiskDestinationAssertionV2{
			Kind: "exact_window_title", ExpectedWindowTitle: riskPreflight.Draft.Send.DestinationLabel,
		}
		if err := request.Validate(); err != nil {
			return consequentialRiskToolFailureV1(ConsequentialRiskCodeGrantMismatchV1), nil
		}
	}
	executor := t.semanticPressExecutor
	if executor == nil {
		if client, ok := t.client.(*AXClient); ok {
			executor = client.SemanticPressV2
		} else {
			return agent.BusinessError(
				"semantic press executor is unavailable; call get_app_state again"), nil
		}
	}
	// Burn the one-shot grant only after every local preflight and immediately
	// before the helper call. The callback compares the full canonical intent,
	// including destination detail; drift and replay therefore stop here.
	if riskPreflight.Status == ConsequentialRiskPreflightRequiredV1 {
		if riskPreflight.Draft == nil || riskExecution.consume(*riskPreflight.Draft) != nil {
			return consequentialRiskToolFailureV1(ConsequentialRiskCodeGrantMismatchV1), nil
		}
	}
	result, err := executor(ctx, request)
	if err != nil {
		var commitUnknown *SemanticPressCommitUnknownErrorV2
		if errors.As(err, &commitUnknown) {
			failure := agent.BusinessError(
				"semantic press commit status is unknown; do not retry automatically; re-observe the app")
			failure.GUIOutcome = &agent.GUIActionOutcome{
				Result: agent.GUIActionResultCompletedUnverified,
				Phase:  agent.GUIActionPhaseInputCommitted, FailureCode: "commit_unknown",
			}
			return failure, nil
		}
		return computerUseCallError("semantic press", err), nil
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		failure := agent.BusinessError(
			"invalid semantic press acknowledgement; do not retry automatically; re-observe the app")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCompletedUnverified,
			Phase:  agent.GUIActionPhaseInputCommitted, FailureCode: "invalid_helper_result",
		}
		return failure, nil
	}
	outcome := computerUseSemanticPressGUIOutcomeV2(result)

	switch result.Status {
	case "completed_unverified":
		content := "semantic_press_v2 completed_unverified: press was committed but no declared causal postcondition was verified; do not retry automatically"
		if result.CommitState == "unknown" {
			content = "semantic_press_v2 completed_unverified: press may have committed; do not retry automatically; re-observe the app"
		}
		return agent.ToolResult{
			Content:    content,
			GUIOutcome: outcome,
		}, nil
	case "user_interference":
		failure := agent.BusinessError(
			"semantic_press_v2 stopped after user interference; do not retry automatically; re-observe the app")
		failure.GUIOutcome = outcome
		return failure, nil
	default:
		failure := agent.BusinessError(fmt.Sprintf(
			"semantic_press_v2 failed during %s: %s; re-observe before retrying",
			result.Phase, *result.FailureCode))
		failure.GUIOutcome = outcome
		return failure, nil
	}
}

func computerUseSemanticPressGUIOutcomeV2(
	result SemanticPressResultV2,
) *agent.GUIActionOutcome {
	outcome := &agent.GUIActionOutcome{
		Result: agent.GUIActionResultCompletedUnverified,
		Phase:  computerUseGUIPhaseV1(result.Phase),
	}
	if result.FailureCode != nil {
		outcome.FailureCode = *result.FailureCode
	}
	switch result.Status {
	case "user_interference":
		outcome.Result = agent.GUIActionResultUserInterference
		if result.FailureCode != nil &&
			*result.FailureCode == "target_foreground_interference" {
			outcome.Result = agent.GUIActionResultFailed
		}
		if result.CommitState != "not_committed" {
			outcome.Phase = agent.GUIActionPhaseInputCommitted
		}
	case "failed":
		outcome.Result = agent.GUIActionResultFailed
	}
	if result.Status == "completed_unverified" && result.CommitState == "unknown" {
		outcome.Phase = agent.GUIActionPhaseInputCommitted
	}
	outcome.SameObservationContinuationSafe =
		result.Status == "completed_unverified" &&
			result.CommitState == "committed" &&
			result.FailureCode != nil &&
			*result.FailureCode == "postcondition_not_declared"
	return outcome
}

func (t *ComputerUseTool) scroll(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	axis, direction, steps, valid := computerUseSemanticScrollDeltaV1(int(args.DX), int(args.DY))
	if !valid {
		return agent.ValidationError(
			"scroll requires exactly one non-zero dx or dy semantic step count between -10 and 10"), nil
	}
	// Match coordinate, drag, and keyboard batch semantics: generic calls
	// consume their observation, while one trusted provider batch keeps its
	// source observation until the executor-owned final screenshot.
	defer t.invalidateStateAfterMutationV1(ctx)
	entry, failure, ok := t.preflightRef(ctx, args)
	if !ok {
		return failure, nil
	}
	if t.snapshot == nil || !t.snapshot.typed || t.snapshot.bundleID == "" ||
		t.snapshot.bundleID != strings.TrimSpace(t.snapshot.bundleID) ||
		t.snapshot.windowID == nil || *t.snapshot.windowID <= 0 ||
		uint64(*t.snapshot.windowID) > uint64(^uint32(0)) ||
		entry.pid != t.snapshot.pid || entry.path == "" || entry.role == "" ||
		entry.fingerprint == "" {
		return agent.BusinessError(
			"scroll requires typed app, window, and element authority; call get_app_state again"), nil
	}
	request := SemanticScrollRequestV1{
		SchemaVersion: 1, PID: entry.pid, BundleID: t.snapshot.bundleID,
		WindowID: uint32(*t.snapshot.windowID), Ref: args.Ref, Path: entry.path,
		ExpectedRole: entry.role, ExpectedFingerprint: entry.fingerprint,
		Axis: axis, Direction: direction, Steps: steps,
		FallbackPolicy:     "report_unsupported",
		InterferencePolicy: "global_physical",
		CommitDeadlineAt:   t.computerUseCoordinateNowV1().Add(computerUseMutationDeadlineV1).UTC().Format(time.RFC3339Nano),
	}
	if args.ExecutionLane ==
		string(guicontrol.ComputerUseExecutionBackgroundSemantic) {
		request.InterferencePolicy = "target_foreground"
	}
	if err := request.Validate(); err != nil {
		return agent.BusinessError("scroll authority is invalid; call get_app_state again"), nil
	}
	executor := t.semanticScrollExecutor
	if executor == nil {
		if client, ok := t.client.(*AXClient); ok {
			executor = client.SemanticScrollV1
		} else {
			return agent.BusinessError(
				"typed semantic scroll executor is unavailable; call get_app_state again"), nil
		}
	}
	result, err := executor(ctx, request)
	if err != nil {
		var commitUnknown *SemanticScrollCommitUnknownErrorV1
		if errors.As(err, &commitUnknown) {
			failure := agent.BusinessError(
				"semantic scroll commit status is unknown; do not retry automatically; re-observe the app")
			failure.GUIOutcome = &agent.GUIActionOutcome{
				Result: agent.GUIActionResultCompletedUnverified,
				Phase:  agent.GUIActionPhaseInputCommitted, FailureCode: "commit_unknown",
			}
			return failure, nil
		}
		if errors.Is(err, context.Canceled) {
			failure := agent.BusinessError(
				"semantic scroll was cancelled before helper commit; re-observe the app")
			failure.GUIOutcome = &agent.GUIActionOutcome{
				Result: agent.GUIActionResultCancelled,
				Phase:  agent.GUIActionPhaseActing, FailureCode: "cancelled",
			}
			return failure, nil
		}
		failure := computerUseCallError("semantic scroll", err)
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultFailed,
			Phase:  agent.GUIActionPhaseActing, FailureCode: "transport_error",
		}
		return failure, nil
	}
	if err := result.ValidateTaggedUnion(); err != nil || result.ExpectedSteps != steps {
		failure := agent.BusinessError(
			"invalid semantic scroll acknowledgement; commit status is unknown; do not retry automatically; re-observe the app")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCompletedUnverified,
			Phase:  agent.GUIActionPhaseInputCommitted, FailureCode: "invalid_helper_result",
		}
		return failure, nil
	}
	outcome := computerUseSemanticScrollGUIOutcomeV1(result)
	switch result.Status {
	case "verified":
		return agent.ToolResult{
			Content: fmt.Sprintf(
				"semantic_scroll_v1 verified %d of %d requested AX steps; re-observe before the next action",
				result.StepsCompleted, result.ExpectedSteps),
			GUIOutcome: outcome,
		}, nil
	case "completed_unverified":
		content := fmt.Sprintf(
			"semantic_scroll_v1 completed_unverified after %d of %d requested AX steps: %s; do not retry automatically; re-observe",
			result.StepsCompleted, result.ExpectedSteps, *result.FailureCode)
		if result.CommitState == "unknown" {
			content = "semantic_scroll_v1 may have committed an additional step; do not retry automatically; re-observe"
		}
		return agent.ToolResult{Content: content, GUIOutcome: outcome}, nil
	case "user_interference":
		failure := agent.BusinessError(
			"semantic scroll stopped because physical user input was detected; do not retry automatically; re-observe")
		failure.GUIOutcome = outcome
		return failure, nil
	case "cancelled":
		failure := agent.BusinessError(fmt.Sprintf(
			"semantic scroll cancelled after %d of %d verified AX steps; re-observe before any retry",
			result.StepsCompleted, result.ExpectedSteps))
		failure.GUIOutcome = outcome
		return failure, nil
	case "fallback_required":
		failure := agent.BusinessError(
			"semantic scroll fallback_required: the exact target exposes no reliable verifiable AX scroll metric; no global scroll event was attempted; re-observe")
		failure.GUIOutcome = outcome
		return failure, nil
	default:
		failure := agent.BusinessError(fmt.Sprintf(
			"semantic scroll failed during %s: %s; re-observe before retrying",
			result.Phase, *result.FailureCode))
		failure.GUIOutcome = outcome
		return failure, nil
	}
}

func computerUseSemanticScrollDeltaV1(dx, dy int) (axis, direction string, steps int, ok bool) {
	if (dx == 0) == (dy == 0) {
		return "", "", 0, false
	}
	delta := dy
	axis = "vertical"
	if dx != 0 {
		delta = dx
		axis = "horizontal"
	}
	if delta < -10 || delta > 10 {
		return "", "", 0, false
	}
	direction = "increment"
	if delta < 0 {
		direction = "decrement"
		delta = -delta
	}
	return axis, direction, delta, true
}

func computerUseSemanticScrollGUIOutcomeV1(
	result SemanticScrollResultV1,
) *agent.GUIActionOutcome {
	outcome := &agent.GUIActionOutcome{
		Result: agent.GUIActionResultCompletedUnverified,
		Phase:  computerUseGUIPhaseV1(result.Phase),
	}
	if result.FailureCode != nil {
		outcome.FailureCode = *result.FailureCode
	}
	switch result.Status {
	case "verified":
		outcome.Result = agent.GUIActionResultVerified
	case "user_interference":
		outcome.Result = agent.GUIActionResultUserInterference
		if result.FailureCode != nil &&
			*result.FailureCode == "target_foreground_interference" {
			outcome.Result = agent.GUIActionResultFailed
		}
	case "cancelled":
		outcome.Result = agent.GUIActionResultCancelled
	case "fallback_required", "failed":
		outcome.Result = agent.GUIActionResultFailed
	}
	if result.CommitState == "unknown" ||
		(result.Status == "user_interference" || result.Status == "cancelled") &&
			result.CommitState != "not_committed" {
		outcome.Phase = agent.GUIActionPhaseInputCommitted
	}
	return outcome
}

func (t *ComputerUseTool) focusOrLaunch(ctx context.Context, args computerUseArgs, method string) (agent.ToolResult, error) {
	if args.App == "" {
		return agent.ValidationError(fmt.Sprintf("%s requires 'app'", args.Action)), nil
	}
	if !ValidAppNamePattern.MatchString(args.App) {
		return agent.ValidationError(fmt.Sprintf("invalid app name %q", args.App)), nil
	}
	params := map[string]any{"app_name": args.App}
	if args.Window != "" {
		params["window_title"] = args.Window
	}
	if method == "focus" {
		params["verify"] = true
	}
	raw, err := t.client.Call(ctx, method, params)
	if err != nil {
		return computerUseCallError(args.Action, err), nil
	}
	t.invalidateState()
	return agent.ToolResult{Content: computerUseActionMessage(raw, args.Action+" completed")}, nil
}

func (t *ComputerUseTool) typeText(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	if args.Text == nil || *args.Text == "" {
		return agent.ValidationError("type requires non-empty 'text'"), nil
	}
	if args.Ref == "" {
		return t.coordinateFocusedType(ctx, args, *args.Text)
	}
	return t.targetBoundInput(ctx, args, *args.Text, "", nil)
}

func (t *ComputerUseTool) coordinateFocusedType(
	ctx context.Context,
	args computerUseArgs,
	text string,
) (agent.ToolResult, error) {
	defer t.invalidateStateAfterMutationV1(ctx)
	focus := t.coordinateFocus
	if focus == nil || args.StateID == "" || args.StateID != focus.stateID {
		return computerUseKeyboardTargetUnavailableV1(), nil
	}
	current, failure, ok := t.readTree(ctx, focus.pid, focus.filter, focus.budget)
	if !ok {
		return failure, nil
	}
	if code := focus.treeMismatchCode(current); code != "" {
		return computerUseKeyboardTargetUnavailableReasonV1(code), nil
	}
	ref, entry, element, hasFocusedElement :=
		computerUseFocusedElementAuthorityV1(current)
	deadline := t.computerUseCoordinateNowV1().Add(
		computerUseMutationDeadlineV1,
	)
	request := TargetBoundInputRequestV1{
		SchemaVersion: 1,
		PID:           current.PID,
		BundleID:      current.BundleID,
		WindowID:      uint32(*current.WindowID),
		ExpectedWindowAXBounds: CoordinateQuartzRectV1{
			X: current.WindowFrame.X, Y: current.WindowFrame.Y,
			Width: current.WindowFrame.Width, Height: current.WindowFrame.Height,
		},
		Action:           "type",
		Text:             &text,
		CommitDeadlineAt: deadline.UTC().Format(time.RFC3339Nano),
	}
	if hasFocusedElement {
		request.Ref = &ref
		path, role, fingerprint := entry.Path, entry.Role, entry.Fingerprint
		request.Path = &path
		request.ExpectedRole = &role
		request.ExpectedFingerprint = &fingerprint
	}
	result, err := t.executeTargetBoundInput(ctx, args.Action, request)
	locationTarget := focus.locationNavigation
	if hasFocusedElement {
		locationTarget = locationTarget ||
			computerUseLocationFieldEvidenceV1(element)
	}
	t.recordLocationNavigationCommitV1(
		result,
		err,
		text,
		current,
		entry,
		locationTarget,
	)
	return result, err
}

func computerUseEditableFocusEvidenceV1(element computerUseElement) bool {
	switch element.Role {
	case "AXTextField", "AXTextArea", "AXComboBox":
		return element.Enabled == nil || *element.Enabled
	default:
		return false
	}
}

func computerUseFocusedElementAuthorityV1(
	tree computerUseTree,
) (string, computerUseRefPath, computerUseElement, bool) {
	if tree.FocusedRef == nil || !validComputerUseRef(*tree.FocusedRef) {
		return "", computerUseRefPath{}, computerUseElement{}, false
	}
	ref := *tree.FocusedRef
	entry, exists := tree.RefPaths[ref]
	if !exists || entry.Path == "" || entry.Role == "" ||
		entry.Fingerprint == "" {
		return "", computerUseRefPath{}, computerUseElement{}, false
	}
	element, err := resolveComputerUseFingerprint(
		tree.Elements,
		entry.Fingerprint,
	)
	if err != nil || element.Ref != ref ||
		element.Path != entry.Path ||
		element.Role != entry.Role ||
		element.Fingerprint != entry.Fingerprint {
		return "", computerUseRefPath{}, computerUseElement{}, false
	}
	return ref, entry, element, true
}

func (t *ComputerUseTool) recordLocationNavigationCommitV1(
	result agent.ToolResult,
	err error,
	text string,
	tree computerUseTree,
	entry computerUseRefPath,
	locationTarget bool,
) {
	knownCommit := result.GUIOutcome != nil &&
		(result.GUIOutcome.Result == agent.GUIActionResultVerified ||
			result.GUIOutcome.Result == agent.GUIActionResultCompletedUnverified) &&
		result.GUIOutcome.FailureCode != "commit_unknown" &&
		result.GUIOutcome.FailureCode != "commit_status_unknown" &&
		result.GUIOutcome.FailureCode != "action_commit_unknown" &&
		result.GUIOutcome.FailureCode != "invalid_helper_result"
	if err != nil || result.IsError || !knownCommit || !locationTarget ||
		!computerUseLocationNavigationTextV1(text) ||
		tree.WindowID == nil || *tree.WindowID <= 0 ||
		uint64(*tree.WindowID) > uint64(^uint32(0)) {
		return
	}
	t.navigationCommit = &computerUseNavigationCommitV1{
		pid:      tree.PID,
		bundleID: tree.BundleID,
		windowID: uint32(*tree.WindowID),
		path:     entry.Path,
		role:     entry.Role,
	}
}

func computerUseKeyboardTargetUnavailableV1() agent.ToolResult {
	return computerUseKeyboardTargetUnavailableReasonV1(
		"keyboard_target_unavailable",
	)
}

func computerUseKeyboardTargetUnavailableReasonV1(
	failureCode string,
) agent.ToolResult {
	return computerUsePrecommitBusinessErrorV1(
		failureCode,
		"computer_use_error: keyboard_target_unavailable\n"+
			"message: no current exact AX focus or one-shot verified same-window focus handoff is available for this keyboard action\n"+
			"gate: "+failureCode+"\n"+
			"recovery: re-observe the intended window and establish one focused text target before typing; do not retry automatically",
	)
}

// computerUsePrecommitFailureV1 is the single classification seam for an input
// action rejected before executeTargetBoundInput invokes the helper. These
// failures may require a fresh observation, but they can never make the input
// commit status unknown because no input request crossed the mutation boundary.
func computerUsePrecommitFailureV1(
	result agent.ToolResult,
	failureCode string,
) agent.ToolResult {
	result.GUIOutcome = &agent.GUIActionOutcome{
		Result:      agent.GUIActionResultFailed,
		Phase:       agent.GUIActionPhaseActing,
		FailureCode: failureCode,
	}
	return result
}

func computerUsePrecommitBusinessErrorV1(
	failureCode string,
	message string,
) agent.ToolResult {
	return computerUsePrecommitFailureV1(
		agent.BusinessError(message),
		failureCode,
	)
}

func (t *ComputerUseTool) hotkey(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	if args.Keys == "" {
		return agent.ValidationError("hotkey requires 'keys'"), nil
	}
	key, modifiers, ok := parseComputerUseHotkeyV1(args.Keys)
	if !ok {
		return agent.ValidationError("hotkey requires a final key"), nil
	}
	key = canonicalComputerUseHotkeyTokenV1(key)
	for index := range modifiers {
		modifiers[index] = canonicalComputerUseHotkeyTokenV1(modifiers[index])
	}
	if args.Ref != "" {
		return agent.ValidationError("hotkey is window-bound and does not accept 'ref'"), nil
	}
	if computerUseKeyboardNeedsExactIntentV1(args) {
		if _, ok := t.ordinaryKeyboardFocusWitnessV1(args); !ok {
			t.invalidateState()
			return consequentialRiskToolFailureV1(
				ConsequentialRiskCodeUnsupportedPathV1,
			), nil
		}
	}
	return t.targetBoundInput(ctx, args, "", key, modifiers)
}

func (t *ComputerUseTool) keypress(
	ctx context.Context,
	args computerUseArgs,
) (agent.ToolResult, error) {
	if len(args.KeySequence) == 0 {
		return agent.ValidationError("keypress requires non-empty 'key_sequence'"), nil
	}
	backgroundKeyboard := args.ExecutionLane ==
		string(guicontrol.ComputerUseExecutionBackgroundKeyboard)
	if args.Keys != "" || (args.Ref != "" && !backgroundKeyboard) {
		return agent.ValidationError(
			"keypress accepts a focused ref only in background keyboard execution"), nil
	}
	if backgroundKeyboard && args.Ref == "" {
		return agent.ValidationError(
			"background keyboard keypress requires one exact focused ref"), nil
	}
	if len(args.KeySequence) > 64 || len(args.Modifiers) > 4 {
		return agent.ValidationError(
			"keypress exceeds the admitted key/modifier sequence"), nil
	}
	if backgroundKeyboard &&
		backgroundTargetedInputConsequentialKeyV1(
			args.KeySequence,
			args.Modifiers,
		) {
		return consequentialRiskToolFailureV1(
			ConsequentialRiskCodeUnsupportedPathV1,
		), nil
	}
	if computerUseKeyboardNeedsExactIntentV1(args) &&
		!backgroundKeyboard &&
		!t.allowsLocationNavigationCommitV1(args) {
		if _, ok := t.ordinaryKeyboardFocusWitnessV1(args); !ok {
			t.invalidateState()
			return consequentialRiskToolFailureV1(
				ConsequentialRiskCodeUnsupportedPathV1,
			), nil
		}
	}
	return t.targetBoundInput(ctx, args, "", "", args.Modifiers)
}

func (t *ComputerUseTool) targetBoundInput(
	ctx context.Context,
	args computerUseArgs,
	text string,
	key string,
	modifiers []string,
) (agent.ToolResult, error) {
	// Generic single-action calls consume their observation. One trusted
	// OpenAI Responses action batch instead keeps its original screenshot and
	// exact window frame until the batch's required final screenshot.
	defer t.invalidateStateAfterMutationV1(ctx)
	if args.StateID == "" {
		return agent.ValidationError(args.Action + " requires the latest state_id"), nil
	}
	if t.snapshot == nil || args.StateID != t.snapshot.id {
		return computerUsePrecommitBusinessErrorV1(
			"stale_state",
			"stale state_id or no active state; call get_app_state again",
		), nil
	}
	if t.snapshot.bundleID == "" || t.snapshot.windowID == nil || *t.snapshot.windowID <= 0 {
		return computerUsePrecommitBusinessErrorV1(
			"target_identity_unavailable",
			"target-bound input requires typed bundle and unique window authority; call get_app_state again",
		), nil
	}
	backgroundKeyboard := args.ExecutionLane ==
		string(guicontrol.ComputerUseExecutionBackgroundKeyboard)
	if backgroundKeyboard &&
		(args.Action != "type" && args.Action != "keypress") {
		return computerUsePrecommitBusinessErrorV1(
			"background_keyboard_action_invalid",
			"background keyboard execution supports type or keypress only",
		), nil
	}
	var typeEntry refEntry
	var navigationCommit *computerUseNavigationCommitV1
	var keyboardFocusWitness *computerUseKeyboardFocusWitnessV1
	if args.Action == "type" || backgroundKeyboard {
		var exists bool
		typeEntry, exists = t.refs[args.Ref]
		if !exists {
			return computerUsePrecommitBusinessErrorV1(
				"target_ref_unavailable",
				"unknown ref for latest state_id; call get_app_state again",
			), nil
		}
		if typeEntry.path == "" || typeEntry.role == "" || typeEntry.fingerprint == "" {
			return computerUsePrecommitBusinessErrorV1(
				"target_element_unavailable",
				"target-bound type requires typed element authority; call get_app_state again",
			), nil
		}
	} else if computerUseKeyboardNeedsExactIntentV1(args) {
		if t.allowsLocationNavigationCommitV1(args) {
			navigationCommit = t.navigationCommit
			t.navigationCommit = nil
		} else if witness, ok := t.ordinaryKeyboardFocusWitnessV1(args); ok {
			keyboardFocusWitness = &witness
		} else {
			return consequentialRiskToolFailureV1(
				ConsequentialRiskCodeUnsupportedPathV1,
			), nil
		}
	}

	current, failure, ok := t.readTree(
		ctx, t.snapshot.pid, t.snapshot.filter, t.snapshot.budget)
	if !ok {
		return computerUsePrecommitFailureV1(
			failure,
			"target_observation_unavailable",
		), nil
	}
	if args.Action == "type" && !hasOpenAINativeComputerActionV1(ctx) &&
		computerUseStateID(current) != args.StateID {
		return computerUsePrecommitBusinessErrorV1(
			"stale_state",
			"stale state detected before target-bound input; call get_app_state again",
		), nil
	}
	if current.BundleID != t.snapshot.bundleID || current.WindowID == nil ||
		*current.WindowID != *t.snapshot.windowID || current.WindowFrame == nil {
		return computerUsePrecommitBusinessErrorV1(
			"target_authority_changed",
			"target-bound input authority changed; call get_app_state again",
		), nil
	}
	if uint64(*current.WindowID) > uint64(^uint32(0)) {
		return computerUsePrecommitBusinessErrorV1(
			"window_identity_invalid",
			"target-bound input window authority is invalid; call get_app_state again",
		), nil
	}
	if args.Action == "type" || backgroundKeyboard {
		if current.FocusedRef == nil || *current.FocusedRef != args.Ref {
			return computerUseKeyboardTargetUnavailableV1(), nil
		}
		if backgroundKeyboard {
			ref, entry, element, focused :=
				computerUseFocusedElementAuthorityV1(current)
			if !focused || ref != args.Ref ||
				entry.Path != typeEntry.path ||
				entry.Role != typeEntry.role ||
				entry.Fingerprint != typeEntry.fingerprint ||
				!computerUseEditableFocusEvidenceV1(element) {
				return computerUseKeyboardTargetUnavailableReasonV1(
					"background_keyboard_focused_element_changed",
				), nil
			}
		}
	} else if navigationCommit != nil && navigationCommit.path != "" {
		_, entry, _, focused := computerUseFocusedElementAuthorityV1(current)
		// Typing intentionally changes the value-derived AX fingerprint of
		// some editable location fields (Safari is one example). Keep the
		// one-shot Return bound to the same window, focused AX path, and role,
		// but do not require the pre-type value fingerprint to survive its own
		// mutation.
		if !focused ||
			entry.Path != navigationCommit.path ||
			entry.Role != navigationCommit.role {
			return computerUseKeyboardTargetUnavailableV1(), nil
		}
	} else if keyboardFocusWitness != nil {
		_, entry, element, focused :=
			computerUseFocusedElementAuthorityV1(current)
		if !focused ||
			entry.Path != keyboardFocusWitness.path ||
			entry.Role != keyboardFocusWitness.role {
			return computerUseKeyboardTargetUnavailableReasonV1(
				"keyboard_focused_element_changed",
			), nil
		}
		if !computerUseKeyboardFocusRoleAllowedV1(args, element) ||
			computerUseFocusedKeyboardElementConsequentialV1(element) {
			return consequentialRiskToolFailureV1(
				ConsequentialRiskCodeUnsupportedPathV1,
			), nil
		}
	}

	deadline := t.computerUseCoordinateNowV1().
		Add(computerUseMutationDeadlineV1).
		UTC().Format(time.RFC3339Nano)
	request := TargetBoundInputRequestV1{
		SchemaVersion: 1,
		PID:           current.PID,
		BundleID:      current.BundleID,
		WindowID:      uint32(*current.WindowID),
		ExpectedWindowAXBounds: CoordinateQuartzRectV1{
			X: current.WindowFrame.X, Y: current.WindowFrame.Y,
			Width: current.WindowFrame.Width, Height: current.WindowFrame.Height,
		},
		Action: args.Action, CommitDeadlineAt: deadline,
	}
	if args.Action == "type" {
		request.Ref = &args.Ref
		path, role, fingerprint := typeEntry.path, typeEntry.role, typeEntry.fingerprint
		request.Path = &path
		request.ExpectedRole = &role
		request.ExpectedFingerprint = &fingerprint
		request.Text = &text
	} else if args.Action == "hotkey" {
		// computerUseArgs uses omitempty, so a provider keypress without
		// modifiers arrives here as a nil slice. The strict cross-language wire
		// contract requires an explicit JSON array: Swift intentionally rejects
		// null because null and "no modifiers" are different protocol states.
		explicitModifiers := append([]string{}, modifiers...)
		request.Key = &key
		request.Modifiers = &explicitModifiers
	} else {
		explicitModifiers := append([]string{}, modifiers...)
		keys := append([]string(nil), args.KeySequence...)
		request.Keys = &keys
		request.Modifiers = &explicitModifiers
	}
	var result agent.ToolResult
	var err error
	if backgroundKeyboard {
		authority := t.backgroundInputAuthority
		if authority == nil {
			return computerUsePrecommitBusinessErrorV1(
				"background_keyboard_authority_unavailable",
				"background keyboard execution has no initial foreground witness",
			), nil
		}
		backgroundRequest := BackgroundTargetedInputRequestV1{
			SchemaVersion:                1,
			Input:                        request,
			FocusedRef:                   args.Ref,
			FocusedPath:                  typeEntry.path,
			ExpectedFocusedRole:          typeEntry.role,
			ExpectedFocusedFingerprint:   typeEntry.fingerprint,
			TargetLaunchDate:             authority.targetLaunchDate,
			PreservedFrontmostPID:        authority.preservedFrontmostPID,
			PreservedFrontmostBundleID:   authority.preservedFrontmostBundleID,
			PreservedFrontmostLaunchDate: authority.preservedFrontmostLaunchDate,
		}
		result, err = t.executeBackgroundTargetedInput(
			ctx,
			args.Action,
			backgroundRequest,
		)
	} else {
		result, err = t.executeTargetBoundInput(ctx, args.Action, request)
	}
	if args.Action == "type" && !backgroundKeyboard {
		ref, entry, element, focused := computerUseFocusedElementAuthorityV1(current)
		if focused && ref == args.Ref {
			t.recordLocationNavigationCommitV1(
				result,
				err,
				text,
				current,
				entry,
				computerUseLocationFieldEvidenceV1(element),
			)
		}
	}
	return result, err
}

func (t *ComputerUseTool) executeTargetBoundInput(
	ctx context.Context,
	action string,
	request TargetBoundInputRequestV1,
) (agent.ToolResult, error) {
	if err := request.Validate(); err != nil {
		return computerUsePrecommitBusinessErrorV1(
			"invalid_request",
			"target-bound input request is invalid; re-observe before retrying",
		), nil
	}
	executor := t.targetBoundInputExecutor
	if executor == nil {
		if client, ok := t.client.(*AXClient); ok {
			executor = client.TargetBoundInputV1
		} else {
			return computerUsePrecommitBusinessErrorV1(
				"input_executor_unavailable",
				"target-bound input executor is unavailable; re-observe before retrying",
			), nil
		}
	}
	result, err := executor(ctx, request)
	return computerUseTargetBoundInputResultV1(action, result, err)
}

func (t *ComputerUseTool) executeBackgroundTargetedInput(
	ctx context.Context,
	action string,
	request BackgroundTargetedInputRequestV1,
) (agent.ToolResult, error) {
	if err := request.Validate(); err != nil {
		return computerUsePrecommitBusinessErrorV1(
			"invalid_request",
			"background targeted input request is invalid; re-observe before retrying",
		), nil
	}
	executor := t.backgroundTargetedInputExecutor
	if executor == nil {
		if client, ok := t.client.(*AXClient); ok {
			executor = client.BackgroundTargetedInputV1
		} else {
			return computerUsePrecommitBusinessErrorV1(
				"input_executor_unavailable",
				"background targeted input executor is unavailable; re-observe before retrying",
			), nil
		}
	}
	result, err := executor(ctx, request)
	return computerUseTargetBoundInputResultV1(action, result, err)
}

func computerUseTargetBoundInputResultV1(
	action string,
	result TargetBoundInputResultV1,
	err error,
) (agent.ToolResult, error) {
	if err != nil {
		var commitUnknown *TargetBoundInputCommitUnknownErrorV1
		if errors.As(err, &commitUnknown) {
			detail := commitUnknown.Error()
			failure := agent.BusinessError(
				"target-bound input commit status is unknown: " + detail +
					"; do not retry automatically; re-observe the app")
			failure.GUIOutcome = &agent.GUIActionOutcome{
				Result: agent.GUIActionResultCompletedUnverified, Phase: agent.GUIActionPhaseInputCommitted,
				FailureCode: "commit_unknown",
			}
			return failure, nil
		}
		return computerUseCallError("target-bound "+action, err), nil
	}
	if result.Action == "unknown" && result.Status == "failed" &&
		!result.InputCommitted && result.FailureCode != nil &&
		*result.FailureCode == "invalid_request" {
		failure := agent.BusinessError(
			"target-bound input helper rejected the request before execution; re-observe before retrying")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result:      agent.GUIActionResultFailed,
			Phase:       agent.GUIActionPhaseActing,
			FailureCode: "invalid_request",
		}
		return failure, nil
	}
	if err := result.ValidateTaggedUnion(); err != nil || result.Action != action {
		failure := agent.BusinessError(
			"invalid target-bound input acknowledgement; do not retry automatically; re-observe the app")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCompletedUnverified, Phase: agent.GUIActionPhaseInputCommitted,
			FailureCode: "invalid_helper_result",
		}
		return failure, nil
	}
	outcome := computerUseTargetBoundInputGUIOutcomeV1(result)
	if result.Status == "verified" {
		return agent.ToolResult{
			Content:    "target-bound type verified; input content is redacted; re-observe before the next action",
			GUIOutcome: outcome,
		}, nil
	}
	if result.Status == "completed_unverified" {
		return agent.ToolResult{
			Content: "target-bound " + action +
				" completed_unverified; input content is redacted; do not retry automatically; re-observe the app",
			GUIOutcome: outcome,
		}, nil
	}
	if result.Status == "user_interference" {
		failure := agent.BusinessError(
			"target-bound " + action + " stopped after user interference: " +
				*result.FailureCode + "; input content is redacted; do not retry automatically; re-observe the app")
		failure.GUIOutcome = outcome
		return failure, nil
	}
	failure := agent.BusinessError(
		"target-bound " + action + " failed before input commit: " +
			*result.FailureCode + "; input content is redacted; re-observe the app")
	failure.GUIOutcome = outcome
	return failure, nil
}

func computerUseTargetBoundInputGUIOutcomeV1(
	result TargetBoundInputResultV1,
) *agent.GUIActionOutcome {
	outcome := &agent.GUIActionOutcome{
		Result: agent.GUIActionResultCompletedUnverified,
		Phase:  computerUseGUIPhaseV1(result.Phase),
	}
	if result.Status == "verified" {
		outcome.Result = agent.GUIActionResultVerified
	}
	if result.Status == "user_interference" {
		outcome.Result = agent.GUIActionResultUserInterference
	}
	if result.FailureCode != nil {
		outcome.FailureCode = *result.FailureCode
	}
	if result.Status == "failed" {
		outcome.Result = agent.GUIActionResultFailed
		switch outcome.FailureCode {
		case "clipboard_ownership_lost_before_input", "physical_input_interference":
			outcome.Result = agent.GUIActionResultUserInterference
		}
	}
	outcome.SameObservationContinuationSafe =
		result.Status == "completed_unverified" &&
			result.InputCommitted &&
			result.FailureCode != nil &&
			(*result.FailureCode == "postcondition_not_declared" ||
				*result.FailureCode == "interference_detection_unavailable")
	return outcome
}

func (t *ComputerUseTool) move(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	return t.coordinatePointerActionV1(ctx, args)
}

func (t *ComputerUseTool) pixelScroll(
	ctx context.Context,
	args computerUseArgs,
) (agent.ToolResult, error) {
	// OpenAI's native scroll union is one ordered two-commit mutation: move the
	// real pointer to the provider point, then emit the provider's exact pixel
	// deltas there. Every path consumes the immutable observation authority and
	// no result is retry-safe.
	defer t.invalidateStateAfterMutationV1(ctx)

	if args.X == nil || args.Y == nil || args.ScrollX == nil || args.ScrollY == nil {
		return agent.ValidationError(
			"pixel_scroll requires x+y+scroll_x+scroll_y"), nil
	}
	providerX, providerY := int64(*args.ScrollX), int64(*args.ScrollY)
	if !coordinatePixelScrollProviderDeltasV1(providerX, providerY) {
		return agent.ValidationError(
			"pixel_scroll requires a non-zero provider delta within the admitted safety bound"), nil
	}
	if args.StateID == "" {
		return agent.ValidationError(
			"pixel_scroll coordinates require the latest state_id"), nil
	}
	if t.snapshot == nil || args.StateID != t.snapshot.id {
		return agent.BusinessError(
			"stale state_id or no active state; call get_app_state with include_screenshot=true again"), nil
	}
	if t.coordinateArtifact == nil {
		return agent.BusinessError(
			"pixel_scroll requires an actionable screenshot from get_app_state with include_screenshot=true"), nil
	}
	frame := t.coordinateArtifact.Frame()
	if frame.StateID != args.StateID {
		return agent.BusinessError(
			"coordinate screenshot does not match the latest state_id; re-observe before scrolling"), nil
	}
	currentTree, failure, ok := t.readTree(
		ctx, t.snapshot.pid, t.snapshot.filter, t.snapshot.budget)
	if !ok {
		return failure, nil
	}
	currentTarget, failure, ok := computerUseCoordinateCaptureRequestV1(currentTree)
	if !ok {
		return failure, nil
	}
	if err := computerUseCoordinateFrameMatchesCurrentTargetV1(frame, currentTarget); err != nil {
		return agent.BusinessError(
			"coordinate frame authority no longer matches current AX target: " +
				err.Error() + "; re-observe before scrolling"), nil
	}
	topology, err := ReadDisplayTopologyV1(ctx, t.client)
	if err != nil {
		return computerUseCallError("read display topology", err), nil
	}
	now := t.computerUseCoordinateNowV1()
	topologyRef := CoordinateTopologyRefV1{
		TopologyID: topology.TopologyID, Generation: topology.Generation,
	}
	mapped, err := MapCoordinatePixelCenterV1(
		frame, topologyRef, args.StateID, frame.FrameID, now,
		float64(*args.X), float64(*args.Y))
	if err != nil {
		return agent.BusinessError(
			"pixel scroll coordinate mapping failed: " + err.Error() +
				"; re-observe before retrying"), nil
	}
	pixelCenterX := float64(*args.X) + 0.5
	pixelCenterY := float64(*args.Y) + 0.5
	var deltaRegion *CoordinateTransformRegionV1
	for index := range frame.TransformRegions {
		region := &frame.TransformRegions[index]
		if coordinatePixelRectContainsPoint(
			region.PixelRect, pixelCenterX, pixelCenterY) {
			if deltaRegion != nil {
				return agent.BusinessError(
					"pixel scroll coordinate belongs to overlapping transforms; re-observe"), nil
			}
			deltaRegion = region
		}
	}
	if deltaRegion == nil || deltaRegion.DisplayID != mapped.DisplayID ||
		deltaRegion.Affine.B != 0 || deltaRegion.Affine.C != 0 ||
		!finiteCoordinate(deltaRegion.Affine.A) ||
		!finiteCoordinate(deltaRegion.Affine.D) ||
		deltaRegion.Affine.A <= 0 || deltaRegion.Affine.D <= 0 {
		return agent.BusinessError(
			"pixel scroll requires one positive axis-aligned immutable frame transform; re-observe"), nil
	}
	cgAxis1, cgAxis2, ok := coordinatePixelScrollCGDeltasV1(
		providerX, providerY, deltaRegion.Affine.A, deltaRegion.Affine.D)
	if !ok {
		return agent.ValidationError(
			"pixel_scroll delta is not representable after immutable frame scaling"), nil
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, frame.ExpiresAt)
	if err != nil {
		return agent.BusinessError(
			"coordinate frame expiry is invalid; re-observe before scrolling"), nil
	}
	deadline := now.Add(computerUseMutationDeadlineV1)
	if expiresAt.Before(deadline) {
		deadline = expiresAt
	}
	request := CoordinatePixelScrollRequestV1{
		SchemaVersion: 1, TopologyRef: topologyRef,
		HelperBootID: topology.HelperBootID,
		PID:          currentTarget.PID, BundleID: currentTarget.BundleID,
		WindowID:                   currentTarget.WindowID,
		ExpectedWindowQuartzBounds: frame.CapturedQuartzRect,
		DisplayID:                  mapped.DisplayID,
		QuartzPoint:                CoordinateMouseEventPointV1{X: mapped.X, Y: mapped.Y},
		Unit:                       "pixel",
		ProviderDeltaX:             providerX,
		ProviderDeltaY:             providerY,
		ProviderToQuartzScaleX:     deltaRegion.Affine.A,
		ProviderToQuartzScaleY:     deltaRegion.Affine.D,
		CGPointDeltaAxis1:          cgAxis1,
		CGPointDeltaAxis2:          cgAxis2,
		Modifiers:                  append([]string{}, args.Modifiers...),
		TargetPolicy:               "same_window",
		CommitDeadlineAt:           deadline.UTC().Format(time.RFC3339Nano),
	}
	if err := request.Validate(); err != nil {
		return agent.BusinessError(
			"pixel scroll request is invalid: " + err.Error() +
				"; re-observe before retrying"), nil
	}
	executor := t.coordinatePixelScrollExecutor
	if executor == nil {
		if client, ok := t.client.(*AXClient); ok {
			executor = client.CoordinatePixelScrollV1
		} else {
			return agent.BusinessError(
				"typed pixel scroll executor is unavailable; re-observe before retrying"), nil
		}
	}
	result, err := executor(ctx, request)
	if err != nil {
		var commitUnknown *CoordinatePixelScrollCommitUnknownErrorV1
		if errors.As(err, &commitUnknown) {
			failure := agent.BusinessError(
				"pixel scroll commit status is unknown; do not retry automatically; re-observe")
			failure.GUIOutcome = &agent.GUIActionOutcome{
				Result:      agent.GUIActionResultCompletedUnverified,
				Phase:       agent.GUIActionPhaseInputCommitted,
				FailureCode: "commit_unknown",
			}
			return failure, nil
		}
		return computerUseCallError("pixel scroll", err), nil
	}
	expected := coordinatePixelScrollAcknowledgementV1(request)
	if err := result.ValidateTaggedUnion(); err != nil ||
		result.Requested == nil || *result.Requested != expected ||
		result.PointerEndpoint != nil &&
			result.PointerEndpoint.Requested != request.QuartzPoint {
		if err == nil {
			err = errors.New("helper result does not match pixel scroll request")
		}
		failure := agent.BusinessError(
			"invalid pixel scroll result: " + err.Error() +
				"; commit status is unknown; do not retry automatically")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result:      agent.GUIActionResultCompletedUnverified,
			Phase:       agent.GUIActionPhaseInputCommitted,
			FailureCode: "invalid_helper_result",
		}
		return failure, nil
	}
	outcome := computerUsePixelScrollGUIOutcomeV1(request, result)
	switch result.Status {
	case "committed_unverified":
		return agent.ToolResult{
			Content: "pixel scroll committed_unverified: " + *result.FailureCode +
				"; do not retry automatically; re-observe",
			GUIOutcome: outcome,
		}, nil
	case "user_interference":
		failure := agent.BusinessError(
			"pixel scroll stopped after user interference: " + *result.FailureCode +
				"; do not retry automatically; re-observe")
		failure.GUIOutcome = outcome
		return failure, nil
	case "commit_unknown":
		failure := agent.BusinessError(
			"pixel scroll commit status is unknown; do not retry automatically; re-observe")
		failure.GUIOutcome = outcome
		return failure, nil
	default:
		failure := agent.BusinessError(
			"pixel scroll failed during " + result.Phase + ": " + *result.FailureCode +
				"; do not retry automatically; re-observe")
		failure.GUIOutcome = outcome
		return failure, nil
	}
}

func computerUsePixelScrollGUIOutcomeV1(
	request CoordinatePixelScrollRequestV1,
	result CoordinatePixelScrollResultV1,
) *agent.GUIActionOutcome {
	outcome := &agent.GUIActionOutcome{Phase: computerUseGUIPhaseV1(result.Phase)}
	switch result.Status {
	case "committed_unverified", "commit_unknown":
		outcome.Result = agent.GUIActionResultCompletedUnverified
	case "user_interference":
		outcome.Result = agent.GUIActionResultUserInterference
	default:
		outcome.Result = agent.GUIActionResultFailed
	}
	if result.FailureCode != nil {
		outcome.FailureCode = *result.FailureCode
	}
	if result.PointerMoveCommitState != "not_committed" &&
		result.Phase != "preflight" && result.Phase != "post_verification" {
		outcome.Phase = agent.GUIActionPhaseInputCommitted
	}
	if result.PointerEndpoint != nil && result.PointerEndpoint.Observed != nil {
		outcome.Pointer = &agent.GUIActionPointer{
			DisplayID:          request.DisplayID,
			TopologyID:         request.TopologyRef.TopologyID,
			TopologyGeneration: request.TopologyRef.Generation,
			X:                  result.PointerEndpoint.Observed.X, Y: result.PointerEndpoint.Observed.Y,
		}
	}
	outcome.SameObservationContinuationSafe =
		result.Status == "committed_unverified" &&
			result.PointerMoveCommitState == "committed" &&
			result.ScrollCommitState == "committed" &&
			result.FailureCode != nil &&
			*result.FailureCode == "scroll_postcondition_not_declared" &&
			result.PointerEndpoint != nil &&
			result.PointerEndpoint.Verified
	return outcome
}

func (t *ComputerUseTool) drag(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	// Every drag attempt consumes the observation and immutable image authority,
	// including argument, freshness, cancellation, transport, and helper-result
	// failures. A possible drop side effect must always be followed by a fresh
	// observation rather than an automatic retry.
	defer t.invalidateStateAfterMutationV1(ctx)

	pixelPath := args.Path
	if len(pixelPath) > 0 {
		if args.StartX != nil || args.StartY != nil || args.EndX != nil || args.EndY != nil {
			return agent.ValidationError(
				"drag path cannot be combined with start_x/start_y/end_x/end_y"), nil
		}
	} else {
		if args.StartX == nil || args.StartY == nil || args.EndX == nil || args.EndY == nil {
			return agent.ValidationError(
				"drag requires path or start_x+start_y+end_x+end_y coordinates"), nil
		}
		pixelPath = []computerUsePoint{
			{X: *args.StartX, Y: *args.StartY},
			{X: *args.EndX, Y: *args.EndY},
		}
	}
	if len(pixelPath) < 2 || len(pixelPath) > coordinateDragMaximumWaypointsV1 {
		return agent.ValidationError(fmt.Sprintf(
			"drag path requires 2..%d waypoints",
			coordinateDragMaximumWaypointsV1,
		)), nil
	}
	for index := 1; index < len(pixelPath); index++ {
		if pixelPath[index] == pixelPath[index-1] {
			return agent.ValidationError("drag adjacent waypoints must be distinct"), nil
		}
	}
	durationMS := int(args.DurationMS)
	if durationMS == 0 {
		durationMS = computerUseDefaultDragDurationV1
	}
	if durationMS < 120 || durationMS > 800 {
		return agent.ValidationError("duration_ms must be between 120 and 800"), nil
	}
	if args.StateID == "" {
		return agent.ValidationError("drag requires the latest state_id"), nil
	}
	if t.snapshot == nil || args.StateID != t.snapshot.id {
		return agent.BusinessError("stale state_id or no active state; call get_app_state with include_screenshot=true again"), nil
	}
	if t.coordinateArtifact == nil {
		return agent.BusinessError("drag requires an actionable screenshot from get_app_state with include_screenshot=true"), nil
	}
	frame := t.coordinateArtifact.Frame()
	if frame.StateID != args.StateID {
		return agent.BusinessError("coordinate screenshot does not match the latest state_id; re-observe before dragging"), nil
	}

	currentTree, failure, ok := t.readTree(ctx, t.snapshot.pid, t.snapshot.filter, t.snapshot.budget)
	if !ok {
		return failure, nil
	}
	currentTarget, failure, ok := computerUseCoordinateCaptureRequestV1(currentTree)
	if !ok {
		return failure, nil
	}
	if err := computerUseCoordinateFrameMatchesCurrentTargetV1(frame, currentTarget); err != nil {
		return agent.BusinessError("coordinate frame authority no longer matches current AX target: " + err.Error() +
			"; re-observe before dragging"), nil
	}
	topology, err := ReadDisplayTopologyV1(ctx, t.client)
	if err != nil {
		return computerUseCallError("read display topology", err), nil
	}
	now := t.computerUseCoordinateNowV1()
	topologyRef := CoordinateTopologyRefV1{
		TopologyID: topology.TopologyID,
		Generation: topology.Generation,
	}
	waypoints := make([]CoordinateDragWaypointV1, 0, len(pixelPath))
	for index, point := range pixelPath {
		mapped, mapErr := MapCoordinatePixelCenterV1(
			frame, topologyRef, args.StateID, frame.FrameID, now,
			float64(point.X), float64(point.Y),
		)
		if mapErr != nil {
			return agent.BusinessError(fmt.Sprintf(
				"drag waypoint %d mapping failed: %v; re-observe before retrying",
				index, mapErr,
			)), nil
		}
		waypoints = append(waypoints, CoordinateDragWaypointV1{
			DisplayID: mapped.DisplayID,
			QuartzPoint: CoordinateMouseEventPointV1{
				X: mapped.X,
				Y: mapped.Y,
			},
		})
	}
	start, end := waypoints[0], waypoints[len(waypoints)-1]
	expiresAt, err := time.Parse(time.RFC3339Nano, frame.ExpiresAt)
	if err != nil {
		return agent.BusinessError("coordinate frame expiry is invalid; re-observe before dragging"), nil
	}
	deadline := now.Add(computerUseMutationDeadlineV1)
	if expiresAt.Before(deadline) {
		deadline = expiresAt
	}
	minimumCompletionDeadline := now.Add(
		time.Duration(durationMS)*time.Millisecond + computerUseDragDeadlineOverheadV1)
	if deadline.Before(minimumCompletionDeadline) {
		return agent.BusinessError(
			"coordinate frame expires too soon to complete drag and cleanup; re-observe before retrying"), nil
	}

	request := CoordinateDragRequestV1{
		SchemaVersion:              1,
		TopologyRef:                topologyRef,
		HelperBootID:               topology.HelperBootID,
		PID:                        currentTarget.PID,
		BundleID:                   currentTarget.BundleID,
		WindowID:                   currentTarget.WindowID,
		ExpectedWindowQuartzBounds: frame.CapturedQuartzRect,
		StartDisplayID:             start.DisplayID,
		EndDisplayID:               end.DisplayID,
		StartQuartzPoint:           start.QuartzPoint,
		EndQuartzPoint:             end.QuartzPoint,
		Waypoints:                  waypoints,
		Button:                     "left",
		Modifiers:                  append([]string{}, args.Modifiers...),
		DurationMS:                 durationMS,
		EndTargetPolicy:            "same_window",
		CommitDeadlineAt:           deadline.UTC().Format(time.RFC3339Nano),
	}
	if err := request.Validate(); err != nil {
		return agent.BusinessError("drag request is invalid: " + err.Error() + "; re-observe before retrying"), nil
	}
	executor := t.coordinateDragExecutor
	if executor == nil {
		if client, ok := t.client.(*AXClient); ok {
			executor = client.CoordinateDragV1
		} else {
			return agent.BusinessError("typed drag executor is unavailable; re-observe before retrying"), nil
		}
	}
	result, err := executor(ctx, request)
	if err != nil {
		return computerUseDragExecutorErrorV1(err), nil
	}
	if err := result.ValidateTaggedUnion(); err != nil ||
		(result.PointerEndpoint != nil && result.PointerEndpoint.Requested != request.EndQuartzPoint) {
		if err == nil {
			err = errors.New("helper result does not match drag request")
		}
		failure := agent.BusinessError("invalid drag result: " + err.Error() +
			"; commit status is unknown; do not retry automatically")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCompletedUnverified,
			Phase:  agent.GUIActionPhaseActing, FailureCode: "invalid_helper_result",
		}
		return failure, nil
	}
	outcome := computerUseDragGUIOutcomeV1(request, result)
	switch result.Status {
	case "completed_unverified":
		return agent.ToolResult{
			Content: "drag completed_unverified: " + *result.FailureCode +
				"; a drop side effect is possible; do not retry automatically; re-observe",
			GUIOutcome: outcome,
		}, nil
	case "user_interference":
		failure := agent.BusinessError("drag stopped after user interference: " + *result.FailureCode +
			"; a drop side effect is possible; do not retry automatically; re-observe")
		failure.GUIOutcome = outcome
		return failure, nil
	default:
		failure := agent.BusinessError("drag failed during " + result.Phase + ": " + *result.FailureCode +
			"; do not retry automatically; re-observe")
		failure.GUIOutcome = outcome
		return failure, nil
	}
}

func computerUseDragExecutorErrorV1(err error) agent.ToolResult {
	var commitUnknown *CoordinateDragCommitUnknownErrorV1
	if errors.As(err, &commitUnknown) {
		failure := agent.BusinessError(
			"drag commit status is unknown; a drop side effect is possible; do not retry automatically; re-observe")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCompletedUnverified,
			Phase:  agent.GUIActionPhaseInputCommitted, FailureCode: "commit_unknown",
		}
		return failure
	}
	if errors.Is(err, context.Canceled) {
		failure := agent.BusinessError("drag was cancelled before a typed helper acknowledgement; re-observe")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCancelled,
			Phase:  agent.GUIActionPhaseActing, FailureCode: "cancelled",
		}
		return failure
	}
	failure := computerUseCallError("drag", err)
	failure.GUIOutcome = &agent.GUIActionOutcome{
		Result: agent.GUIActionResultFailed,
		Phase:  agent.GUIActionPhaseActing, FailureCode: "transport_error",
	}
	return failure
}

func computerUseDragGUIOutcomeV1(
	request CoordinateDragRequestV1,
	result CoordinateDragResultV1,
) *agent.GUIActionOutcome {
	outcome := &agent.GUIActionOutcome{Phase: computerUseGUIPhaseV1(result.Phase)}
	switch result.Status {
	case "completed_unverified":
		outcome.Result = agent.GUIActionResultCompletedUnverified
	case "user_interference":
		outcome.Result = agent.GUIActionResultUserInterference
	default:
		outcome.Result = agent.GUIActionResultFailed
	}
	if result.FailureCode != nil {
		outcome.FailureCode = *result.FailureCode
	}
	if result.PointerEndpoint != nil && result.PointerEndpoint.Observed != nil {
		outcome.Pointer = &agent.GUIActionPointer{
			DisplayID:          request.EndDisplayID,
			TopologyID:         request.TopologyRef.TopologyID,
			TopologyGeneration: request.TopologyRef.Generation,
			X:                  result.PointerEndpoint.Observed.X,
			Y:                  result.PointerEndpoint.Observed.Y,
		}
	}
	outcome.SameObservationContinuationSafe =
		result.Status == "completed_unverified" &&
			result.DragCommitted &&
			result.MouseDownCommitted &&
			result.PointerMotionCommitted &&
			result.MouseUpCommitted &&
			result.FailureCode != nil &&
			*result.FailureCode == "drop_postcondition_not_declared" &&
			result.PointerEndpoint != nil &&
			result.PointerEndpoint.Verified
	return outcome
}

func (t *ComputerUseTool) selectText(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	// AX text selection is still a mutation even when the helper reports that
	// an Electron/web target lacks AXSelectedTextRange support. Consume the
	// state on every path and never turn fallback_required into a hidden drag.
	defer t.invalidateState()

	if args.Range == nil || args.Range.Location < 0 || args.Range.Length <= 0 ||
		args.Range.Location > int(^uint(0)>>1)-args.Range.Length {
		return agent.ValidationError("select_text requires a valid range with location >= 0 and length > 0"), nil
	}
	entry, failure, ok := t.preflightRef(ctx, args)
	if !ok {
		return failure, nil
	}
	if t.snapshot == nil || t.snapshot.bundleID == "" ||
		t.snapshot.windowID == nil || *t.snapshot.windowID <= 0 ||
		uint64(*t.snapshot.windowID) > uint64(^uint32(0)) {
		return agent.BusinessError("select_text requires exact app and window identity; call get_app_state again"), nil
	}
	if entry.fingerprint == "" {
		return agent.BusinessError("select_text requires a typed element fingerprint; call get_app_state again"), nil
	}
	now := t.computerUseCoordinateNowV1()
	request := SemanticTextSelectionRequestV2{
		SchemaVersion:       2,
		PID:                 entry.pid,
		BundleID:            t.snapshot.bundleID,
		WindowID:            uint32(*t.snapshot.windowID),
		Ref:                 args.Ref,
		Path:                entry.path,
		ExpectedRole:        entry.role,
		ExpectedFingerprint: entry.fingerprint,
		Range:               *args.Range,
		FallbackPolicy:      "report_unsupported",
		CommitDeadlineAt:    now.Add(computerUseMutationDeadlineV1).UTC().Format(time.RFC3339Nano),
	}
	if err := request.Validate(); err != nil {
		return agent.BusinessError("select_text request is invalid: " + err.Error() + "; re-observe before retrying"), nil
	}
	executor := t.semanticTextSelectionExecutor
	if executor == nil {
		if client, ok := t.client.(*AXClient); ok {
			executor = client.SemanticTextSelectionV2
		} else {
			return agent.BusinessError("typed semantic text selection executor is unavailable; re-observe before retrying"), nil
		}
	}
	result, err := executor(ctx, request)
	if err != nil {
		return computerUseSelectionExecutorErrorV2(err), nil
	}
	if err := result.ValidateTaggedUnion(); err != nil ||
		(result.Status == "verified" &&
			(result.SelectedRange == nil || *result.SelectedRange != request.Range)) {
		if err == nil {
			err = errors.New("helper result range does not match select_text request")
		}
		failure := agent.BusinessError("invalid select_text result: " + err.Error() +
			"; commit status is unknown; do not retry automatically")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCompletedUnverified,
			Phase:  agent.GUIActionPhaseActing, FailureCode: "invalid_helper_result",
		}
		return failure, nil
	}
	outcome := computerUseSelectionGUIOutcomeV2(result)
	switch result.Status {
	case "verified":
		return agent.ToolResult{
			Content:    "semantic_text_selection_v2 verified the exact selected range through Accessibility; re-observe before the next action",
			GUIOutcome: outcome,
		}, nil
	case "completed_unverified":
		content := "semantic_text_selection_v2 completed_unverified: " + *result.FailureCode +
			"; do not retry automatically; re-observe"
		if result.CommitState == "unknown" {
			content = "semantic_text_selection_v2 completed_unverified: selection may have committed; do not retry automatically; re-observe"
		}
		return agent.ToolResult{
			Content:    content,
			GUIOutcome: outcome,
		}, nil
	case "user_interference":
		failure := agent.BusinessError(
			"select_text stopped because physical user input was detected; " +
				"do not retry automatically; re-observe")
		failure.GUIOutcome = outcome
		return failure, nil
	case "fallback_required":
		failure := agent.BusinessError(
			"select_text fallback_required: Accessibility text range is unsupported; " +
				"no coordinate drag was attempted; re-observe before choosing an explicit drag")
		failure.GUIOutcome = outcome
		return failure, nil
	default:
		failure := agent.BusinessError("select_text failed during " + result.Phase + ": " +
			*result.FailureCode + "; do not retry automatically; re-observe")
		failure.GUIOutcome = outcome
		return failure, nil
	}
}

func computerUseSelectionExecutorErrorV2(err error) agent.ToolResult {
	var commitUnknown *SemanticTextSelectionCommitUnknownErrorV2
	if errors.As(err, &commitUnknown) {
		failure := agent.BusinessError(
			"select_text commit status is unknown; do not retry automatically; re-observe")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCompletedUnverified,
			Phase:  agent.GUIActionPhaseInputCommitted, FailureCode: "commit_unknown",
		}
		return failure
	}
	if errors.Is(err, context.Canceled) {
		failure := agent.BusinessError("select_text was cancelled before a typed helper acknowledgement; re-observe")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCancelled,
			Phase:  agent.GUIActionPhaseActing, FailureCode: "cancelled",
		}
		return failure
	}
	failure := computerUseCallError("select_text", err)
	failure.GUIOutcome = &agent.GUIActionOutcome{
		Result: agent.GUIActionResultFailed,
		Phase:  agent.GUIActionPhaseActing, FailureCode: "transport_error",
	}
	return failure
}

func computerUseSelectionGUIOutcomeV2(result SemanticTextSelectionResultV2) *agent.GUIActionOutcome {
	outcome := &agent.GUIActionOutcome{Phase: computerUseGUIPhaseV1(result.Phase)}
	switch result.Status {
	case "verified":
		outcome.Result = agent.GUIActionResultVerified
	case "completed_unverified":
		outcome.Result = agent.GUIActionResultCompletedUnverified
	case "user_interference":
		outcome.Result = agent.GUIActionResultUserInterference
	default:
		outcome.Result = agent.GUIActionResultFailed
	}
	if result.FailureCode != nil {
		outcome.FailureCode = *result.FailureCode
	}
	if result.CommitState == "unknown" ||
		(result.Status == "user_interference" && result.CommitState != "not_committed") {
		outcome.Phase = agent.GUIActionPhaseInputCommitted
	}
	return outcome
}

func (t *ComputerUseTool) coordinatePointerActionV1(
	ctx context.Context,
	args computerUseArgs,
) (agent.ToolResult, error) {
	// Generic coordinate calls consume the screenshot after one action. A
	// trusted OpenAI Responses batch deliberately maps all ordered coordinates
	// from the same provider-visible screenshot, then captures one final image.
	t.coordinateFocus = nil
	defer t.invalidateObservationStateAfterMutationV1(ctx)

	if args.X == nil || args.Y == nil {
		return agent.ValidationError(args.Action + " requires x+y coordinates"), nil
	}
	if args.StateID == "" {
		return agent.ValidationError(args.Action + " coordinates require the latest state_id"), nil
	}
	if t.snapshot == nil || args.StateID != t.snapshot.id {
		return agent.BusinessError("stale state_id or no active state; call get_app_state with include_screenshot=true again"), nil
	}
	if t.coordinateArtifact == nil {
		return agent.BusinessError("coordinate action requires an actionable screenshot from get_app_state with include_screenshot=true"), nil
	}
	frame := t.coordinateArtifact.Frame()
	if frame.StateID != args.StateID {
		return agent.BusinessError("coordinate screenshot does not match the latest state_id; re-observe before acting"), nil
	}

	button := args.Button
	clicks := int(args.Clicks)
	if args.Action == "click" {
		if button == "" {
			button = "left"
		}
		if !validOpenAIComputerClickButtonV1(button) {
			return agent.ValidationError(
				"button must be 'left', 'right', 'wheel', 'back', or 'forward'",
			), nil
		}
		if clicks <= 0 {
			clicks = 1
		}
	} else {
		button = ""
		clicks = 0
	}

	currentTree, failure, ok := t.readTree(
		ctx,
		t.snapshot.pid,
		t.snapshot.filter,
		t.snapshot.budget,
	)
	if !ok {
		return failure, nil
	}
	currentTarget, failure, ok := computerUseCoordinateCaptureRequestV1(currentTree)
	if !ok {
		return failure, nil
	}
	if err := computerUseCoordinateFrameMatchesCurrentTargetV1(frame, currentTarget); err != nil {
		return agent.BusinessError("coordinate frame authority no longer matches current AX target: " + err.Error() +
			"; re-observe before retrying"), nil
	}
	topology, err := ReadDisplayTopologyV1(ctx, t.client)
	if err != nil {
		return computerUseCallError("read display topology", err), nil
	}
	now := t.computerUseCoordinateNowV1()
	mapped, err := MapCoordinatePixelCenterV1(
		frame,
		CoordinateTopologyRefV1{TopologyID: topology.TopologyID, Generation: topology.Generation},
		args.StateID,
		frame.FrameID,
		now,
		float64(*args.X),
		float64(*args.Y),
	)
	if err != nil {
		return agent.BusinessError("coordinate mapping failed: " + err.Error() + "; re-observe before retrying"), nil
	}
	riskRequestID := ""
	if execution, present := consequentialRiskExecutionFromContextV1(ctx); present {
		riskRequestID = execution.RequestID
	} else if invocation, present := agent.ToolInvocationFromContext(ctx); present {
		riskRequestID = invocation.ToolUseID
	}
	riskPreflight, err := t.preflightMappedCoordinateConsequentialRiskV1(
		args, riskRequestID, frame, topology, mapped)
	if err != nil || riskPreflight.Status == ConsequentialRiskPreflightBlockedV1 {
		code := riskPreflight.FailureCode
		if code == "" {
			code = ConsequentialRiskCodeGrantMismatchV1
		}
		return consequentialRiskToolFailureV1(code), nil
	}
	riskExecution, riskErr := validateConsequentialRiskExecutionV1(ctx, riskPreflight)
	if riskErr != nil {
		code := ConsequentialRiskCodeGrantMismatchV1
		if errors.Is(riskErr, ErrConsequentialRiskExecutionMissingV1) {
			code = ConsequentialRiskCodeMissingGrantV1
		}
		return consequentialRiskToolFailureV1(code), nil
	}
	expiresAt, err := time.Parse(time.RFC3339Nano, frame.ExpiresAt)
	if err != nil {
		return agent.BusinessError("coordinate frame expiry is invalid; re-observe before retrying"), nil
	}
	deadline := now.Add(time.Second)
	if expiresAt.Before(deadline) {
		deadline = expiresAt
	}

	request := CoordinateMouseEventRequestV1{
		SchemaVersion: 1,
		TopologyRef: CoordinateTopologyRefV1{
			TopologyID: topology.TopologyID,
			Generation: topology.Generation,
		},
		HelperBootID: topology.HelperBootID,
		PID:          currentTarget.PID, BundleID: currentTarget.BundleID,
		WindowID:                   currentTarget.WindowID,
		ExpectedWindowQuartzBounds: frame.CapturedQuartzRect,
		DisplayID:                  mapped.DisplayID,
		QuartzPoint:                CoordinateMouseEventPointV1{X: mapped.X, Y: mapped.Y},
		Action:                     args.Action,
		Modifiers:                  append([]string{}, args.Modifiers...),
		CommitDeadlineAt:           deadline.UTC().Format(time.RFC3339Nano),
	}
	if args.Action == "click" {
		request.Button = &button
		request.ClickCount = &clicks
	}
	if riskPreflight.Status == ConsequentialRiskPreflightRequiredV1 {
		if riskPreflight.Draft == nil || riskPreflight.Draft.Target.CoordinateAuthority == nil {
			return consequentialRiskToolFailureV1(ConsequentialRiskCodeGrantMismatchV1), nil
		}
		request.RiskAssertion = &CoordinateMouseRiskAssertionV1{
			Kind: "consequential_click_v1", RiskKind: riskPreflight.Draft.Kind,
			ElementRef:          riskPreflight.Draft.Target.ElementRef,
			ExpectedRole:        riskPreflight.Draft.Target.Role,
			ExpectedFingerprint: riskPreflight.Draft.Target.Fingerprint,
			CoordinateAuthority: *riskPreflight.Draft.Target.CoordinateAuthority,
			DestinationAssertion: SemanticPressRiskDestinationAssertionV2{
				Kind: "exact_window_title", ExpectedWindowTitle: t.snapshot.window,
			},
		}
	}
	if err := request.Validate(); err != nil {
		return agent.BusinessError("coordinate action request is invalid: " + err.Error() + "; re-observe before retrying"), nil
	}
	executor := t.coordinateExecutor
	if executor == nil {
		if client, ok := t.client.(*AXClient); ok {
			executor = client.CoordinateMouseEventV1
		} else {
			return agent.BusinessError("typed coordinate executor is unavailable; re-observe before retrying"), nil
		}
	}
	// Burn the exact one-shot grant only after local state/frame/point
	// rederivation and immediately before the helper receives a click-capable
	// request. Move/right/double click never reach this branch with a grant.
	if riskPreflight.Status == ConsequentialRiskPreflightRequiredV1 {
		if riskPreflight.Draft == nil || riskExecution.consume(*riskPreflight.Draft) != nil {
			return consequentialRiskToolFailureV1(ConsequentialRiskCodeGrantMismatchV1), nil
		}
	}
	result, err := executor(ctx, request)
	if err != nil {
		var commitUnknown *CoordinateMouseEventCommitUnknownErrorV1
		if errors.As(err, &commitUnknown) {
			failure := agent.BusinessError(
				"coordinate action commit status is unknown; do not retry automatically; re-observe the app first")
			failure.GUIOutcome = &agent.GUIActionOutcome{
				Result: agent.GUIActionResultCompletedUnverified,
				Phase:  agent.GUIActionPhaseInputCommitted, FailureCode: "commit_unknown",
			}
			return failure, nil
		}
		return computerUseCallError("coordinate "+args.Action, err), nil
	}
	if err := result.ValidateTaggedUnion(); err != nil || result.Action != args.Action ||
		(result.PointerEndpoint != nil && result.PointerEndpoint.Requested != request.QuartzPoint) {
		if err == nil {
			err = errors.New("helper result does not match coordinate request")
		}
		failure := agent.BusinessError(
			"invalid coordinate action result: " + err.Error() + "; do not retry automatically")
		failure.GUIOutcome = &agent.GUIActionOutcome{
			Result: agent.GUIActionResultCompletedUnverified,
			Phase:  agent.GUIActionPhaseInputCommitted, FailureCode: "invalid_helper_result",
		}
		return failure, nil
	}
	clickFocusRecorded := t.recordCoordinateClickFocusV1(
		args,
		request,
		currentTree,
		result,
	)
	switch result.Status {
	case "completed":
		content := fmt.Sprintf(
			"coordinate %s completed at verified target; re-observe before the next action",
			args.Action,
		)
		if clickFocusRecorded {
			content = "coordinate click completed at the verified pointer; " +
				"type may use this state_id once in the same verified window; exact AX focus is used when available"
		}
		return agent.ToolResult{
			Content:    content,
			GUIOutcome: computerUseCoordinateGUIOutcomeV1(request, result),
		}, nil
	case "completed_unverified":
		if clickFocusRecorded {
			return agent.ToolResult{Content: "coordinate click completed_unverified at the verified pointer; " +
				"type may use this state_id once in the same verified window; exact AX focus is used when available; " +
				"otherwise re-observe before the next action; do not retry the click automatically",
				GUIOutcome: computerUseCoordinateGUIOutcomeV1(request, result)}, nil
		}
		return agent.ToolResult{Content: fmt.Sprintf(
			"coordinate %s completed_unverified: %s; do not retry automatically; re-observe the app",
			args.Action, *result.FailureCode), GUIOutcome: computerUseCoordinateGUIOutcomeV1(request, result)}, nil
	case "user_interference":
		failure := agent.BusinessError(fmt.Sprintf(
			"coordinate %s stopped after user interference: %s; do not retry automatically; re-observe the app",
			args.Action, *result.FailureCode))
		failure.GUIOutcome = computerUseCoordinateGUIOutcomeV1(request, result)
		return failure, nil
	default:
		failure := agent.BusinessError(fmt.Sprintf(
			"coordinate %s failed during %s: %s; do not retry automatically; re-observe the app",
			args.Action, result.Phase, *result.FailureCode))
		failure.GUIOutcome = computerUseCoordinateGUIOutcomeV1(request, result)
		return failure, nil
	}
}

func (t *ComputerUseTool) recordCoordinateClickFocusV1(
	args computerUseArgs,
	request CoordinateMouseEventRequestV1,
	currentTree computerUseTree,
	result CoordinateMouseEventResultV1,
) bool {
	if args.Action != "click" ||
		request.Button == nil || *request.Button != "left" ||
		request.ClickCount == nil || *request.ClickCount != 1 ||
		len(request.Modifiers) != 0 || request.RiskAssertion != nil ||
		!result.PrimaryActionCommitted || result.PointerEndpoint == nil ||
		!result.PointerEndpoint.Verified ||
		result.PointerEndpoint.Observed == nil ||
		currentTree.WindowID == nil || *currentTree.WindowID <= 0 ||
		uint64(*currentTree.WindowID) > uint64(^uint32(0)) ||
		currentTree.WindowFrame == nil ||
		result.Status != "completed_unverified" ||
		result.FailureCode == nil ||
		*result.FailureCode != "click_postcondition_not_declared" {
		return false
	}
	app := strings.TrimSpace(currentTree.AppName)
	if app == "" {
		app = strings.TrimSpace(currentTree.App)
	}
	t.coordinateFocus = &computerUseCoordinateFocusV1{
		stateID: args.StateID, pid: currentTree.PID,
		bundleID: currentTree.BundleID, app: app,
		windowID: uint32(*currentTree.WindowID),
		expectedWindowAXBounds: CoordinateQuartzRectV1{
			X: currentTree.WindowFrame.X, Y: currentTree.WindowFrame.Y,
			Width: currentTree.WindowFrame.Width, Height: currentTree.WindowFrame.Height,
		},
		filter: t.snapshot.filter, budget: t.snapshot.budget,
	}
	return true
}

func computerUseCoordinateGUIOutcomeV1(
	request CoordinateMouseEventRequestV1,
	result CoordinateMouseEventResultV1,
) *agent.GUIActionOutcome {
	outcome := &agent.GUIActionOutcome{Phase: computerUseGUIPhaseV1(result.Phase)}
	switch result.Status {
	case "completed":
		outcome.Result = agent.GUIActionResultVerified
	case "completed_unverified":
		outcome.Result = agent.GUIActionResultCompletedUnverified
	case "user_interference":
		outcome.Result = agent.GUIActionResultUserInterference
	default:
		outcome.Result = agent.GUIActionResultFailed
	}
	if result.FailureCode != nil {
		outcome.FailureCode = *result.FailureCode
	}
	if result.PointerEndpoint != nil && result.PointerEndpoint.Observed != nil {
		outcome.Pointer = &agent.GUIActionPointer{
			DisplayID:          request.DisplayID,
			TopologyID:         request.TopologyRef.TopologyID,
			TopologyGeneration: request.TopologyRef.Generation,
			X:                  result.PointerEndpoint.Observed.X,
			Y:                  result.PointerEndpoint.Observed.Y,
		}
	}
	outcome.SameObservationContinuationSafe =
		result.Status == "completed_unverified" &&
			result.Action == "click" &&
			result.PrimaryActionCommitted &&
			result.PointerMotionCommitted &&
			request.Button != nil &&
			*request.Button == "left" &&
			request.ClickCount != nil &&
			*request.ClickCount == 1 &&
			len(request.Modifiers) == 0 &&
			result.FailureCode != nil &&
			*result.FailureCode == "click_postcondition_not_declared" &&
			result.PointerEndpoint != nil &&
			result.PointerEndpoint.Verified
	return outcome
}

func computerUseGUIPhaseV1(phase string) agent.GUIActionPhase {
	switch phase {
	case "post_observation", "post_verification":
		return agent.GUIActionPhaseVerifying
	case "pointer_move", "drag_motion":
		return agent.GUIActionPhaseMoving
	case "input_committed", "mouse_up", "cleanup":
		return agent.GUIActionPhaseInputCommitted
	case "preflight":
		return agent.GUIActionPhaseObserving
	default:
		return agent.GUIActionPhaseActing
	}
}

func computerUseCoordinateFrameMatchesCurrentTargetV1(
	frame CoordinateFrameV1,
	target CaptureCoordinateWindowRequestV1,
) error {
	if frame.TargetPID == nil || *frame.TargetPID != target.PID ||
		frame.TargetBundleID == nil || *frame.TargetBundleID != target.BundleID ||
		frame.TargetWindowID == nil || *frame.TargetWindowID <= 0 ||
		uint64(*frame.TargetWindowID) != uint64(target.WindowID) {
		return errors.New("pid, bundle, or window identity mismatch")
	}
	// The AX frame used to identify the window may differ slightly from the
	// exact CGWindow bounds. Capture admits that calibration tolerance, while
	// the helper's mutation preflight intentionally requires the exact captured
	// CG bounds so it cannot drift to a neighboring geometry.
	if !captureCoordinateWindowRectsCorrelate(frame.CapturedQuartzRect, target.ExpectedQuartzBounds) {
		return errors.New("AX window bounds no longer correlate with captured CG bounds")
	}
	return nil
}

func (t *ComputerUseTool) wait(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	if args.Condition == "" {
		if args.Timeout <= 0 {
			return agent.ValidationError("wait requires either 'condition' or a positive 'timeout' delay"), nil
		}
		timer := time.NewTimer(time.Duration(args.Timeout * float64(time.Second)))
		defer timer.Stop()
		select {
		case <-ctx.Done():
			return agent.TransientError("wait interrupted: " + ctx.Err().Error()), nil
		case <-timer.C:
			return agent.ToolResult{Content: fmt.Sprintf("waited %.1f seconds", args.Timeout)}, nil
		}
	}
	pid, failure, ok := t.resolvePID(ctx, args.App)
	if !ok {
		return failure, nil
	}
	params := map[string]any{"condition": args.Condition}
	if pid > 0 {
		params["pid"] = pid
	}
	if args.Value != nil {
		params["value"] = *args.Value
	}
	if args.Query != "" {
		params["query"] = args.Query
	}
	if args.Role != "" {
		params["role"] = args.Role
	}
	if args.Timeout > 0 {
		params["timeout"] = args.Timeout
	}
	if args.Interval > 0 {
		params["interval"] = args.Interval
	}
	raw, err := t.client.Call(ctx, "wait_for", params)
	if err != nil {
		return computerUseCallError("wait", err), nil
	}
	return agent.ToolResult{Content: computerUseActionMessage(raw, "wait condition satisfied")}, nil
}

func (t *ComputerUseTool) screenshot(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	// Keep screenshot on the same exact-window transaction as an explicit
	// visual get_app_state. This avoids leaking unrelated windows or
	// notifications and publishes one coherent state/image coordinate frame.
	args.IncludeScreenshot = true
	return t.getAppState(ctx, args)
}

func (t *ComputerUseTool) invalidateObservationState() {
	t.snapshot = nil
	t.refs = nil
	t.coordinateArtifact = nil
	t.semanticImageArtifact = nil
}

func (t *ComputerUseTool) invalidateObservationStateAfterMutationV1(
	ctx context.Context,
) {
	if !hasOpenAINativeComputerActionV1(ctx) {
		t.invalidateObservationState()
	}
}

func (t *ComputerUseTool) invalidateStateAfterMutationV1(ctx context.Context) {
	if hasOpenAINativeComputerActionV1(ctx) {
		// Keyboard and compound input can change focus, but the provider's
		// original screenshot/window frame remains the coordinate authority for
		// later actions in this same ordered batch.
		t.coordinateFocus = nil
		return
	}
	t.invalidateState()
}

func (t *ComputerUseTool) invalidateState() {
	t.invalidateObservationState()
	t.coordinateFocus = nil
	t.navigationCommit = nil
}

func computerUseActionMessage(raw json.RawMessage, fallback string) string {
	var response struct {
		Result  string      `json:"result"`
		Role    string      `json:"role"`
		Context *appContext `json:"context,omitempty"`
	}
	if json.Unmarshal(raw, &response) != nil || response.Result == "" {
		return fallback
	}
	message := response.Result
	if response.Role != "" {
		message += " (role: " + response.Role + ")"
	}
	return message + formatContext(response.Context)
}

func computerUseCallError(operation string, err error) agent.ToolResult {
	message := strings.ToLower(err.Error())
	switch {
	case strings.Contains(message, "permission"), strings.Contains(message, "not trusted"), strings.Contains(message, "screen recording"):
		return agent.PermissionError(fmt.Sprintf("%s: %v", operation, err))
	case strings.Contains(message, "timeout"), strings.Contains(message, "deadline"), strings.Contains(message, "unexpected eof"), strings.Contains(message, "read error"):
		return agent.TransientError(fmt.Sprintf("%s: %v", operation, err))
	default:
		return agent.BusinessError(fmt.Sprintf("%s: %v", operation, err))
	}
}
