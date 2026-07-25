package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/guicontrol"
)

// AnthropicComputerAdapter is an experimental provider-native shell around
// the same Accessibility-first ComputerUseTool used by the function schema.
// It is intentionally not registered by RegisterLocalTools: production may
// opt in only after the provider/history integration gates are complete.
//
// The adapter owns no second execution implementation. Provider-native input
// is translated into ComputerUseTool arguments while state_id, refs,
// CoordinateFrame, topology, and image digests remain daemon-private.
type AnthropicComputerAdapter struct {
	raw                    *ComputerUseTool
	initialDisplayWidthPX  int
	initialDisplayHeightPX int
	pendingBootstrap       *anthropicComputerBootstrap
	visibleImage           *anthropicComputerImageIdentity
	hasExposedImage        bool
}

type anthropicComputerImageIdentity struct {
	stateID   string
	frameID   string
	imageSHA  string
	widthPX   int
	heightPX  int
	expiresAt time.Time
}

type anthropicComputerBootstrap struct {
	snapshot *computerUseSnapshot
	refs     map[string]refEntry
	artifact *CoordinateWindowArtifactV1
	identity anthropicComputerImageIdentity
}

func NewAnthropicComputerAdapter(
	raw *ComputerUseTool,
	initialDisplayWidthPX int,
	initialDisplayHeightPX int,
) *AnthropicComputerAdapter {
	return &AnthropicComputerAdapter{
		raw:                    raw,
		initialDisplayWidthPX:  initialDisplayWidthPX,
		initialDisplayHeightPX: initialDisplayHeightPX,
	}
}

func (a *AnthropicComputerAdapter) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: client.NativeComputerToolName,
		Description: "Anthropic provider-native computer use backed by Kocoro's " +
			"Accessibility-first strict observation and mutation core.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":           map[string]any{"type": "string"},
				"text":             map[string]any{"type": "string"},
				"coordinate":       map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
				"start_coordinate": map[string]any{"type": "array", "items": map[string]any{"type": "integer"}},
				"scroll_direction": map[string]any{"type": "string"},
				"scroll_amount":    map[string]any{"type": "integer"},
				"duration":         map[string]any{"type": "number"},
				"key":              map[string]any{"type": "string"},
			},
		},
		Required: []string{"action"},
	}
}

func (a *AnthropicComputerAdapter) RequiresApproval() bool { return true }

func (a *AnthropicComputerAdapter) NativeToolDef() *client.NativeToolDef {
	if a == nil {
		return nil
	}
	width, height := a.initialDisplayWidthPX, a.initialDisplayHeightPX
	if a.pendingBootstrap != nil {
		width, height = a.pendingBootstrap.identity.widthPX, a.pendingBootstrap.identity.heightPX
	} else if a.raw != nil && a.raw.coordinateArtifact != nil {
		frame := a.raw.coordinateArtifact.Frame()
		if frame.FinalImage.WidthPX > 0 && frame.FinalImage.HeightPX > 0 {
			width, height = frame.FinalImage.WidthPX, frame.FinalImage.HeightPX
		}
	}
	return &client.NativeToolDef{
		Type: client.NativeComputerToolType, Name: client.NativeComputerToolName,
		DisplayWidthPx: width, DisplayHeightPx: height,
	}
}

func anthropicComputerImageIdentityFrom(
	raw *ComputerUseTool,
	snapshot *computerUseSnapshot,
	artifact *CoordinateWindowArtifactV1,
) (anthropicComputerImageIdentity, error) {
	if raw == nil || snapshot == nil || artifact == nil {
		return anthropicComputerImageIdentity{}, fmt.Errorf("strict provider screenshot authority is unavailable")
	}
	frame := artifact.Frame()
	if !snapshot.typed || snapshot.id == "" || snapshot.bundleID == "" ||
		snapshot.pid <= 0 || snapshot.windowID == nil || *snapshot.windowID <= 0 ||
		!frame.Actionable || frame.StateID != snapshot.id ||
		frame.TargetPID == nil || *frame.TargetPID != snapshot.pid ||
		frame.TargetBundleID == nil || *frame.TargetBundleID != snapshot.bundleID ||
		frame.TargetWindowID == nil || *frame.TargetWindowID != *snapshot.windowID {
		return anthropicComputerImageIdentity{}, fmt.Errorf("strict provider screenshot lacks exact typed app/window authority")
	}
	if err := frame.Validate(); err != nil {
		return anthropicComputerImageIdentity{}, fmt.Errorf("strict provider screenshot authority is invalid: %w", err)
	}
	createdAt, createdErr := time.Parse(time.RFC3339Nano, frame.CreatedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, frame.ExpiresAt)
	now := raw.computerUseCoordinateNowV1()
	if createdErr != nil || expiresErr != nil || now.Before(createdAt) || !now.Before(expiresAt) {
		return anthropicComputerImageIdentity{}, fmt.Errorf("strict provider screenshot authority is stale or expired")
	}
	return anthropicComputerImageIdentity{
		stateID: snapshot.id, frameID: frame.FrameID, imageSHA: frame.FinalImage.SHA256,
		widthPX: frame.FinalImage.WidthPX, heightPX: frame.FinalImage.HeightPX,
		expiresAt: expiresAt,
	}, nil
}

func (identity anthropicComputerImageIdentity) matches(
	raw *ComputerUseTool,
	snapshot *computerUseSnapshot,
	artifact *CoordinateWindowArtifactV1,
) bool {
	current, err := anthropicComputerImageIdentityFrom(raw, snapshot, artifact)
	return err == nil && current == identity
}

func (a *AnthropicComputerAdapter) providerImageIsCurrent() bool {
	return a != nil && a.hasExposedImage && a.visibleImage != nil && a.raw != nil &&
		a.visibleImage.matches(a.raw, a.raw.snapshot, a.raw.coordinateArtifact)
}

func (a *AnthropicComputerAdapter) clearVisibleImage() {
	if a != nil {
		a.visibleImage = nil
	}
}

func (a *AnthropicComputerAdapter) markCurrentImageVisible() error {
	if a == nil || a.raw == nil {
		return fmt.Errorf("provider-native computer adapter is unavailable")
	}
	identity, err := anthropicComputerImageIdentityFrom(
		a.raw, a.raw.snapshot, a.raw.coordinateArtifact)
	if err != nil {
		a.clearVisibleImage()
		return err
	}
	a.visibleImage = &identity
	a.hasExposedImage = true
	return nil
}

func (a *AnthropicComputerAdapter) discardUnexposedAuthority() {
	if a == nil || a.raw == nil {
		return
	}
	a.raw.snapshot = nil
	a.raw.refs = nil
	a.raw.coordinateArtifact = nil
}

func (a *AnthropicComputerAdapter) DescribeNativeToolRequestPreparation(
	ctx context.Context,
) (agent.GUIActionDescriptor, error) {
	if a == nil || a.raw == nil {
		return agent.GUIActionDescriptor{}, fmt.Errorf("provider-native computer adapter is unavailable")
	}
	payload, err := marshalAnthropicRawArgs(computerUseArgs{
		Action: "get_app_state", Description: "Prepare first native computer screenshot",
		IncludeScreenshot: true,
	})
	if err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	return a.raw.DescribeGUIAction(ctx, payload)
}

func (a *AnthropicComputerAdapter) validateNativePreparationAuthority(
	ctx context.Context,
) error {
	if !guicontrol.ExecutionAuthorityPresent(ctx) {
		// Direct unit/CLI observation remains available. Daemon execution is
		// distinguishable because its wrapper always installs a genuine claim.
		return nil
	}
	descriptor, err := a.DescribeNativeToolRequestPreparation(ctx)
	if err != nil {
		return fmt.Errorf("provider-native computer preparation authority target is unavailable: %w", err)
	}
	invocation, ok := agent.ToolInvocationFromContext(ctx)
	scope := guicontrol.ExecutionScope{
		ToolName:       client.NativeComputerToolName,
		ToolUseID:      invocation.ToolUseID,
		ActionKind:     descriptor.ActionKind,
		Effect:         string(guicontrol.ComputerUseActionObservation),
		TargetBundleID: descriptor.TargetBundleID,
		ExecutionPath:  descriptor.ExecutionPath,
	}
	if !ok || invocation.ToolName != client.NativeComputerToolName ||
		descriptor.Effect != agent.GUIActionObservation ||
		!guicontrol.ExecutionAuthorized(ctx, scope) {
		return fmt.Errorf("provider-native computer preparation lacks exact daemon execution authority")
	}
	return nil
}

// PrepareNativeToolRequest creates the exact first provider image immediately
// before request construction. The observation is Accessibility-first and uses
// the normal strict tree → topology → exact-window capture → tree transaction.
//
// Its action authority is moved out of the raw ComputerUseTool until the
// provider actually receives the image. This prevents a model from mutating
// against an internal screenshot it has never seen. The first screenshot call
// consumes the same finalized artifact byte-for-byte, so its dimensions cannot
// drift after the native schema has been serialized.
func (a *AnthropicComputerAdapter) PrepareNativeToolRequest(ctx context.Context) error {
	if a == nil || a.raw == nil {
		return fmt.Errorf("provider-native computer adapter is unavailable")
	}
	if err := a.validateNativePreparationAuthority(ctx); err != nil {
		return err
	}
	if a.hasExposedImage {
		return nil
	}

	computerUseGUIOperationMu.Lock()
	defer computerUseGUIOperationMu.Unlock()
	if a.hasExposedImage {
		return nil
	}
	if a.pendingBootstrap != nil {
		if a.pendingBootstrap.identity.matches(
			a.raw,
			a.pendingBootstrap.snapshot,
			a.pendingBootstrap.artifact,
		) {
			return nil
		}
		a.pendingBootstrap = nil
	}
	a.discardUnexposedAuthority()

	payload, err := marshalAnthropicRawArgs(computerUseArgs{
		Action: "get_app_state", Description: "Prepare first native computer screenshot",
		IncludeScreenshot: true,
	})
	if err != nil {
		return fmt.Errorf("prepare first native computer screenshot: %w", err)
	}
	result, runErr := a.raw.runWithGUIOperationLockHeld(ctx, payload)
	if runErr != nil || result.IsError || len(result.Images) != 1 ||
		a.raw.snapshot == nil || a.raw.coordinateArtifact == nil {
		a.discardUnexposedAuthority()
		if runErr != nil {
			return fmt.Errorf("prepare first native computer screenshot: %w", runErr)
		}
		return fmt.Errorf("prepare first native computer screenshot: strict exact image unavailable")
	}
	identity, err := anthropicComputerImageIdentityFrom(
		a.raw, a.raw.snapshot, a.raw.coordinateArtifact)
	if err != nil || !reflectImageBlockV1(
		result.Images[0], a.raw.coordinateArtifact.ImageBlock()) {
		a.discardUnexposedAuthority()
		if err != nil {
			return fmt.Errorf("prepare first native computer screenshot: %w", err)
		}
		return fmt.Errorf("prepare first native computer screenshot: image does not match coordinate authority")
	}
	// Re-resolve after capture. If the foreground target changed between daemon
	// admission and the strict screenshot transaction, the original action
	// capability no longer matches and the image must never reach the provider.
	if err := a.validateNativePreparationAuthority(ctx); err != nil {
		a.discardUnexposedAuthority()
		return err
	}
	a.pendingBootstrap = &anthropicComputerBootstrap{
		snapshot: a.raw.snapshot,
		refs:     a.raw.refs,
		artifact: a.raw.coordinateArtifact,
		identity: identity,
	}
	a.discardUnexposedAuthority()
	return nil
}

func (a *AnthropicComputerAdapter) consumePreparedBootstrap() agent.ToolResult {
	if a == nil || a.raw == nil || a.pendingBootstrap == nil {
		return agent.BusinessError("native computer request was not prepared; retry the request before calling screenshot")
	}
	pending := a.pendingBootstrap
	if a.raw.snapshot != nil || a.raw.refs != nil || a.raw.coordinateArtifact != nil ||
		!pending.identity.matches(a.raw, pending.snapshot, pending.artifact) {
		a.pendingBootstrap = nil
		a.discardUnexposedAuthority()
		return agent.BusinessError("prepared native computer screenshot authority changed; retry the request")
	}
	a.raw.snapshot = pending.snapshot
	a.raw.refs = pending.refs
	a.raw.coordinateArtifact = pending.artifact
	a.pendingBootstrap = nil
	a.visibleImage = &pending.identity
	a.hasExposedImage = true
	return agent.ToolResult{
		Content: "Screenshot captured from the current exact Accessibility target.",
		Images:  []agent.ImageBlock{pending.artifact.ImageBlock()},
	}
}

type anthropicComputerArgs struct {
	Action          string   `json:"action"`
	Text            *string  `json:"text,omitempty"`
	Coordinate      []int    `json:"coordinate,omitempty"`
	StartCoordinate []int    `json:"start_coordinate,omitempty"`
	ScrollDirection *string  `json:"scroll_direction,omitempty"`
	ScrollAmount    *int     `json:"scroll_amount,omitempty"`
	Duration        *float64 `json:"duration,omitempty"`
	Key             *string  `json:"key,omitempty"`
}

type anthropicComputerValidationError struct{ message string }

func (e *anthropicComputerValidationError) Error() string { return e.message }

func anthropicValidationError(format string, args ...any) error {
	return &anthropicComputerValidationError{message: fmt.Sprintf(format, args...)}
}

func decodeAnthropicComputerArgs(payload string) (anthropicComputerArgs, error) {
	var args anthropicComputerArgs
	if err := decodeStrictCoordinateJSON([]byte(payload), &args); err != nil {
		return args, anthropicValidationError("invalid provider-native computer arguments: %v", err)
	}
	if strings.TrimSpace(args.Action) == "" {
		return args, anthropicValidationError("missing required parameter: action")
	}
	return args, nil
}

func noAnthropicCoordinate(values []int) bool { return len(values) == 0 }

func validateAnthropicComputerArgs(args anthropicComputerArgs) error {
	noText := args.Text == nil
	noCoordinate := noAnthropicCoordinate(args.Coordinate)
	noStart := noAnthropicCoordinate(args.StartCoordinate)
	noDirection := args.ScrollDirection == nil
	noAmount := args.ScrollAmount == nil
	noDuration := args.Duration == nil
	noKey := args.Key == nil
	noExtras := func(allowed ...string) bool {
		allow := make(map[string]bool, len(allowed))
		for _, field := range allowed {
			allow[field] = true
		}
		return (allow["text"] || noText) &&
			(allow["coordinate"] || noCoordinate) &&
			(allow["start_coordinate"] || noStart) &&
			(allow["scroll_direction"] || noDirection) &&
			(allow["scroll_amount"] || noAmount) &&
			(allow["duration"] || noDuration) &&
			(allow["key"] || noKey)
	}
	requireCoordinate := func(name string, value []int) error {
		if len(value) != 2 {
			return anthropicValidationError("%s requires exactly two integer coordinates", name)
		}
		return nil
	}

	switch args.Action {
	case "screenshot":
		if !noExtras() {
			return anthropicValidationError("screenshot does not accept action-specific fields")
		}
	case "left_click", "right_click", "double_click", "triple_click", "mouse_move":
		if !noExtras("coordinate") {
			return anthropicValidationError("%s accepts only coordinate", args.Action)
		}
		return requireCoordinate(args.Action, args.Coordinate)
	case "left_click_drag":
		if !noExtras("coordinate", "start_coordinate", "duration") {
			return anthropicValidationError("left_click_drag accepts only coordinate, start_coordinate, and duration")
		}
		if err := requireCoordinate("left_click_drag coordinate", args.Coordinate); err != nil {
			return err
		}
		if err := requireCoordinate("left_click_drag start_coordinate", args.StartCoordinate); err != nil {
			return err
		}
		if args.Duration != nil && (!finiteCoordinate(*args.Duration) || *args.Duration < 0.12 || *args.Duration > 0.8) {
			return anthropicValidationError("left_click_drag duration must be between 0.12 and 0.8 seconds")
		}
	case "type":
		if !noExtras("text") || args.Text == nil || *args.Text == "" {
			return anthropicValidationError("type requires non-empty text and no unrelated fields")
		}
	case "key":
		if !noExtras("text") || args.Text == nil || strings.TrimSpace(*args.Text) == "" {
			return anthropicValidationError("key requires non-empty text and no unrelated fields")
		}
	case "scroll":
		if !noExtras("scroll_direction", "scroll_amount") || args.ScrollDirection == nil || args.ScrollAmount == nil {
			return anthropicValidationError("scroll requires scroll_direction and scroll_amount")
		}
		if *args.ScrollAmount < 1 || *args.ScrollAmount > 10 {
			return anthropicValidationError("scroll_amount must be between 1 and 10")
		}
		switch *args.ScrollDirection {
		case "up", "down", "left", "right":
		default:
			return anthropicValidationError("scroll_direction must be up, down, left, or right")
		}
	case "wait":
		if !noExtras("duration") {
			return anthropicValidationError("wait accepts only duration")
		}
		if args.Duration != nil && (!finiteCoordinate(*args.Duration) || *args.Duration <= 0 || *args.Duration > 120) {
			return anthropicValidationError("wait duration must be in (0, 120] seconds")
		}
	case "middle_click", "left_mouse_down", "left_mouse_up", "hold_key", "zoom", "cursor_position":
		return anthropicValidationError("provider-native computer action %q is not safely supported", args.Action)
	default:
		return anthropicValidationError("unknown provider-native computer action %q", args.Action)
	}
	return nil
}

type anthropicActionableObservation struct {
	snapshot *computerUseSnapshot
	frame    CoordinateFrameV1
	topology DisplayTopologyV1
}

func (a *AnthropicComputerAdapter) actionableObservation(
	ctx context.Context,
) (anthropicActionableObservation, error) {
	if a == nil || a.raw == nil || a.raw.snapshot == nil || a.raw.coordinateArtifact == nil {
		return anthropicActionableObservation{}, fmt.Errorf("no actionable provider screenshot; call screenshot first")
	}
	snapshot := a.raw.snapshot
	frame := a.raw.coordinateArtifact.Frame()
	if !snapshot.typed || snapshot.id == "" || snapshot.bundleID == "" || snapshot.pid <= 0 ||
		snapshot.windowID == nil || *snapshot.windowID <= 0 || !frame.Actionable ||
		frame.StateID != snapshot.id || frame.TargetPID == nil || *frame.TargetPID != snapshot.pid ||
		frame.TargetBundleID == nil || *frame.TargetBundleID != snapshot.bundleID ||
		frame.TargetWindowID == nil || *frame.TargetWindowID != *snapshot.windowID {
		return anthropicActionableObservation{}, fmt.Errorf("provider screenshot lacks exact typed app/window authority; call screenshot again")
	}
	if err := frame.Validate(); err != nil {
		return anthropicActionableObservation{}, fmt.Errorf("provider screenshot authority is invalid; call screenshot again")
	}
	now := a.raw.computerUseCoordinateNowV1()
	createdAt, createdErr := time.Parse(time.RFC3339Nano, frame.CreatedAt)
	expiresAt, expiresErr := time.Parse(time.RFC3339Nano, frame.ExpiresAt)
	if createdErr != nil || expiresErr != nil || now.Before(createdAt) || !now.Before(expiresAt) {
		return anthropicActionableObservation{}, fmt.Errorf("provider screenshot authority is stale or expired; call screenshot again")
	}
	topology, err := ReadDisplayTopologyV1(ctx, a.raw.client)
	if err != nil {
		return anthropicActionableObservation{}, fmt.Errorf("current display topology is unavailable; call screenshot again")
	}
	if frame.TopologyRef != (CoordinateTopologyRefV1{
		TopologyID: topology.TopologyID, Generation: topology.Generation,
	}) {
		return anthropicActionableObservation{}, fmt.Errorf("provider screenshot display topology is stale; call screenshot again")
	}
	return anthropicActionableObservation{snapshot: snapshot, frame: frame, topology: topology}, nil
}

type anthropicTrustedElement struct {
	element computerUseElement
	entry   refEntry
	depth   int
}

func walkComputerUseElements(
	elements []computerUseElement,
	depth int,
	visit func(computerUseElement, int),
) {
	for _, element := range elements {
		visit(element, depth)
		walkComputerUseElements(element.Children, depth+1, visit)
	}
}

func trustedAnthropicElement(
	raw *ComputerUseTool,
	element computerUseElement,
) (refEntry, bool) {
	if raw == nil || raw.snapshot == nil || element.Ref == "" || element.Path == "" ||
		element.Role == "" || element.Fingerprint == "" || element.Enabled == nil || !*element.Enabled {
		return refEntry{}, false
	}
	entry, found := raw.refs[element.Ref]
	if !found || entry.pid != raw.snapshot.pid || entry.path != element.Path ||
		entry.role != element.Role || entry.fingerprint != element.Fingerprint {
		return refEntry{}, false
	}
	return entry, true
}

func (a *AnthropicComputerAdapter) uniqueAXPressHit(
	observation anthropicActionableObservation,
	x int,
	y int,
) (string, bool, error) {
	mapped, err := MapCoordinatePixelCenterV1(
		observation.frame,
		observation.frame.TopologyRef,
		observation.snapshot.id,
		observation.frame.FrameID,
		a.raw.computerUseCoordinateNowV1(),
		float64(x), float64(y),
	)
	if err != nil {
		return "", false, fmt.Errorf("provider coordinate is not actionable: %w", err)
	}
	var candidates []anthropicTrustedElement
	walkComputerUseElements(observation.snapshot.elements, 0, func(element computerUseElement, depth int) {
		if element.Frame == nil || mapped.X < element.Frame.X || mapped.Y < element.Frame.Y ||
			mapped.X >= element.Frame.X+element.Frame.Width || mapped.Y >= element.Frame.Y+element.Frame.Height {
			return
		}
		hasPress := false
		for _, action := range element.Actions {
			if action == "AXPress" {
				hasPress = true
			}
		}
		entry, trusted := trustedAnthropicElement(a.raw, element)
		if hasPress && trusted {
			candidates = append(candidates, anthropicTrustedElement{element: element, entry: entry, depth: depth})
		}
	})
	if len(candidates) == 0 {
		return "", false, nil
	}
	deepest := candidates[0].depth
	for _, candidate := range candidates[1:] {
		if candidate.depth > deepest {
			deepest = candidate.depth
		}
	}
	var selected []anthropicTrustedElement
	for _, candidate := range candidates {
		if candidate.depth == deepest {
			selected = append(selected, candidate)
		}
	}
	if len(selected) != 1 {
		return "", false, fmt.Errorf("provider coordinate resolves to ambiguous trusted AXPress targets")
	}
	return selected[0].element.Ref, true, nil
}

func (a *AnthropicComputerAdapter) uniqueFocusedRef() (string, error) {
	var matches []string
	walkComputerUseElements(a.raw.snapshot.elements, 0, func(element computerUseElement, _ int) {
		if !element.Focused {
			return
		}
		if _, trusted := trustedAnthropicElement(a.raw, element); trusted {
			matches = append(matches, element.Ref)
		}
	})
	if len(matches) != 1 {
		return "", fmt.Errorf("provider type requires exactly one trusted focused AX ref; call screenshot again")
	}
	return matches[0], nil
}

func (a *AnthropicComputerAdapter) uniqueScrollRef() (string, error) {
	var matches []string
	walkComputerUseElements(a.raw.snapshot.elements, 0, func(element computerUseElement, _ int) {
		if element.Role != "AXScrollArea" && element.Role != "AXScrollBar" {
			return
		}
		if _, trusted := trustedAnthropicElement(a.raw, element); trusted {
			matches = append(matches, element.Ref)
		}
	})
	if len(matches) != 1 {
		return "", fmt.Errorf("provider scroll requires exactly one trusted AX scroll ref; no global scroll fallback is allowed")
	}
	return matches[0], nil
}

func marshalAnthropicRawArgs(args computerUseArgs) (string, error) {
	payload, err := json.Marshal(args)
	if err != nil {
		return "", fmt.Errorf("marshal internal computer-use arguments: %w", err)
	}
	return string(payload), nil
}

// translate returns raw ComputerUseTool JSON plus whether the translated call
// is a mutation. Every provider-native mutation is bound to the current exact
// screenshot without exposing that authority in provider arguments.
func (a *AnthropicComputerAdapter) translate(
	ctx context.Context,
	payload string,
) (string, bool, error) {
	args, err := decodeAnthropicComputerArgs(payload)
	if err != nil {
		return "", false, err
	}
	if err := validateAnthropicComputerArgs(args); err != nil {
		return "", false, err
	}
	if a == nil || a.raw == nil {
		return "", false, fmt.Errorf("provider-native computer adapter has no raw ComputerUseTool")
	}
	description := "Anthropic native " + args.Action
	switch args.Action {
	case "screenshot":
		translated, err := marshalAnthropicRawArgs(computerUseArgs{
			Action: "get_app_state", Description: description, IncludeScreenshot: true,
		})
		return translated, false, err
	case "wait":
		duration := 1.0
		if args.Duration != nil {
			duration = *args.Duration
		}
		translated, err := marshalAnthropicRawArgs(computerUseArgs{
			Action: "wait", Description: description, Timeout: duration,
		})
		return translated, false, err
	}

	if !a.providerImageIsCurrent() {
		return "", true, fmt.Errorf("no provider-visible actionable screenshot; call screenshot first")
	}
	observation, err := a.actionableObservation(ctx)
	if err != nil {
		return "", true, err
	}
	stateID := observation.snapshot.id
	toInt := func(value int) *computerUseInt {
		converted := computerUseInt(value)
		return &converted
	}
	translated := computerUseArgs{StateID: stateID, Description: description}
	switch args.Action {
	case "left_click", "right_click", "double_click", "triple_click":
		translated.Action = "click"
		translated.X, translated.Y = toInt(args.Coordinate[0]), toInt(args.Coordinate[1])
		translated.Button = "left"
		translated.Clicks = 1
		switch args.Action {
		case "right_click":
			translated.Button = "right"
		case "double_click":
			translated.Clicks = 2
		case "triple_click":
			translated.Clicks = 3
		case "left_click":
			ref, found, hitErr := a.uniqueAXPressHit(observation, args.Coordinate[0], args.Coordinate[1])
			if hitErr != nil {
				return "", true, hitErr
			}
			if found {
				translated.Action, translated.Ref = "press", ref
				translated.X, translated.Y = nil, nil
				translated.Button, translated.Clicks = "", 0
			}
		}
	case "mouse_move":
		translated.Action = "move"
		translated.X, translated.Y = toInt(args.Coordinate[0]), toInt(args.Coordinate[1])
	case "left_click_drag":
		translated.Action = "drag"
		translated.StartX, translated.StartY = toInt(args.StartCoordinate[0]), toInt(args.StartCoordinate[1])
		translated.EndX, translated.EndY = toInt(args.Coordinate[0]), toInt(args.Coordinate[1])
		if args.Duration != nil {
			translated.DurationMS = computerUseInt(math.Round(*args.Duration * 1000))
		}
	case "type":
		ref, refErr := a.uniqueFocusedRef()
		if refErr != nil {
			return "", true, refErr
		}
		translated.Action, translated.Ref, translated.Text = "type", ref, args.Text
	case "key":
		translated.Action, translated.Keys = "hotkey", *args.Text
	case "scroll":
		ref, refErr := a.uniqueScrollRef()
		if refErr != nil {
			return "", true, refErr
		}
		translated.Action, translated.Ref = "scroll", ref
		switch *args.ScrollDirection {
		case "up":
			translated.DY = computerUseInt(-*args.ScrollAmount)
		case "down":
			translated.DY = computerUseInt(*args.ScrollAmount)
		case "left":
			translated.DX = computerUseInt(-*args.ScrollAmount)
		case "right":
			translated.DX = computerUseInt(*args.ScrollAmount)
		}
	default:
		return "", true, anthropicValidationError("provider-native computer action %q is not safely supported", args.Action)
	}
	rawPayload, err := marshalAnthropicRawArgs(translated)
	return rawPayload, true, err
}

func (a *AnthropicComputerAdapter) classifyObservation(payload string) (string, bool) {
	args, err := decodeAnthropicComputerArgs(payload)
	if err != nil || validateAnthropicComputerArgs(args) != nil {
		return "", false
	}
	description := "Anthropic native " + args.Action
	var translated computerUseArgs
	switch args.Action {
	case "screenshot":
		translated = computerUseArgs{Action: "get_app_state", Description: description, IncludeScreenshot: true}
	case "wait":
		duration := 1.0
		if args.Duration != nil {
			duration = *args.Duration
		}
		translated = computerUseArgs{Action: "wait", Description: description, Timeout: duration}
	default:
		return "", false
	}
	encoded, err := marshalAnthropicRawArgs(translated)
	return encoded, err == nil
}

func (a *AnthropicComputerAdapter) IsSafeArgs(payload string) bool {
	translated, ok := a.classifyObservation(payload)
	return ok && a.raw != nil && a.raw.IsSafeArgs(translated)
}

func (a *AnthropicComputerAdapter) IsReadOnlyCall(payload string) bool {
	translated, ok := a.classifyObservation(payload)
	return ok && a.raw != nil && a.raw.IsReadOnlyCall(translated)
}

func (a *AnthropicComputerAdapter) IsConcurrencySafeCall(payload string) bool {
	if a == nil || a.raw == nil {
		return false
	}
	translated, ok := a.classifyObservation(payload)
	if !ok {
		return false
	}
	return a.raw.IsConcurrencySafeCall(translated)
}

func (a *AnthropicComputerAdapter) DescribeGUIAction(
	ctx context.Context,
	payload string,
) (agent.GUIActionDescriptor, error) {
	translated, _, err := a.translate(ctx, payload)
	if err != nil {
		return agent.GUIActionDescriptor{}, err
	}
	return a.raw.DescribeGUIAction(ctx, translated)
}

func (a *AnthropicComputerAdapter) RestoreGUIActionTargetV1(
	ctx context.Context,
	descriptor agent.GUIActionDescriptor,
) error {
	if a == nil || a.raw == nil {
		return fmt.Errorf("provider-native computer adapter is unavailable")
	}
	return a.raw.RestoreGUIActionTargetV1(ctx, descriptor)
}

func (a *AnthropicComputerAdapter) PreflightConsequentialRiskV1(
	ctx context.Context,
	payload string,
	requestID string,
) (ConsequentialRiskPreflightResultV1, error) {
	translated, mutation, err := a.translate(ctx, payload)
	if err != nil {
		return ConsequentialRiskPreflightResultV1{}, err
	}
	if !mutation {
		return ConsequentialRiskPreflightResultV1{Status: ConsequentialRiskPreflightNoneV1}, nil
	}
	return a.raw.PreflightConsequentialRiskV1(ctx, translated, requestID)
}

type anthropicNativeDisplayDeclaration struct {
	widthPX  int
	heightPX int
}

func currentAnthropicNativeDisplayDeclaration(
	adapter *AnthropicComputerAdapter,
) (anthropicNativeDisplayDeclaration, error) {
	definition := adapter.NativeToolDef()
	if definition == nil {
		return anthropicNativeDisplayDeclaration{}, fmt.Errorf("native display declaration is unavailable")
	}
	if err := definition.Validate(); err != nil {
		return anthropicNativeDisplayDeclaration{}, fmt.Errorf("native display declaration is invalid: %w", err)
	}
	return anthropicNativeDisplayDeclaration{
		widthPX: definition.DisplayWidthPx, heightPX: definition.DisplayHeightPx,
	}, nil
}

func (declaration anthropicNativeDisplayDeclaration) matchesArtifact(raw *ComputerUseTool) bool {
	if raw == nil || raw.coordinateArtifact == nil {
		return false
	}
	frame := raw.coordinateArtifact.Frame()
	return frame.FinalImage.WidthPX == declaration.widthPX &&
		frame.FinalImage.HeightPX == declaration.heightPX
}

func sanitizeAnthropicObservationResult(
	result agent.ToolResult,
	raw *ComputerUseTool,
	declaration anthropicNativeDisplayDeclaration,
) agent.ToolResult {
	if result.IsError || raw == nil || raw.coordinateArtifact == nil || len(result.Images) != 1 {
		failure := agent.BusinessError("strict provider screenshot could not be established")
		failure.GUIOutcome = result.GUIOutcome
		return failure
	}
	if !declaration.matchesArtifact(raw) {
		failure := agent.BusinessError(
			"strict provider screenshot dimensions do not match this request's native display declaration; retry after the next declaration refresh",
		)
		failure.GUIOutcome = result.GUIOutcome
		return failure
	}
	exact := raw.coordinateArtifact.ImageBlock()
	if !reflectImageBlockV1(result.Images[0], exact) {
		failure := agent.BusinessError("strict provider screenshot did not match its internal coordinate authority")
		failure.GUIOutcome = result.GUIOutcome
		return failure
	}
	result.Content = "Screenshot captured from the current exact Accessibility target."
	result.Images = []agent.ImageBlock{exact}
	result.GUIOutcome = nil
	return result
}

func reflectImageBlockV1(left, right agent.ImageBlock) bool {
	return left.MediaType == right.MediaType && left.Data == right.Data
}

func (a *AnthropicComputerAdapter) Run(
	ctx context.Context,
	payload string,
) (agent.ToolResult, error) {
	if a == nil || a.raw == nil {
		return agent.BusinessError("provider-native computer adapter is unavailable"), nil
	}
	// Decode once before taking the global lock so malformed provider input
	// cannot occupy GUI authority. Translation is repeated under the lock because
	// snapshot/topology authority must be current at the raw execution boundary.
	args, err := decodeAnthropicComputerArgs(payload)
	if err != nil {
		return agent.ValidationError(err.Error()), nil
	}
	if err := validateAnthropicComputerArgs(args); err != nil {
		return agent.ValidationError(err.Error()), nil
	}

	computerUseGUIOperationMu.Lock()
	defer computerUseGUIOperationMu.Unlock()
	if !a.hasExposedImage {
		if args.Action != "screenshot" {
			return agent.BusinessError(
				"no provider-visible actionable screenshot; call screenshot first"), nil
		}
		return a.consumePreparedBootstrap(), nil
	}
	declaration, err := currentAnthropicNativeDisplayDeclaration(a)
	if err != nil {
		return agent.BusinessError(err.Error()), nil
	}
	translated, mutation, err := a.translate(ctx, payload)
	if err != nil {
		var validation *anthropicComputerValidationError
		if errors.As(err, &validation) {
			return agent.ValidationError(validation.Error()), nil
		}
		return agent.BusinessError(err.Error()), nil
	}
	result, runErr := a.raw.runWithGUIOperationLockHeld(ctx, translated)
	if !mutation {
		if args.Action == "screenshot" {
			sanitized := sanitizeAnthropicObservationResult(result, a.raw, declaration)
			if sanitized.IsError {
				a.clearVisibleImage()
				return sanitized, runErr
			}
			if err := a.markCurrentImageVisible(); err != nil {
				return agent.BusinessError(err.Error()), runErr
			}
			return sanitized, runErr
		}
		if runErr != nil || result.IsError {
			result.Content = "Wait did not complete."
			result.Images = nil
			return result, runErr
		}
		postPayload, marshalErr := marshalAnthropicRawArgs(computerUseArgs{
			Action: "get_app_state", Description: "Post-wait strict observation", IncludeScreenshot: true,
		})
		if marshalErr != nil {
			return agent.BusinessError("strict post-wait observation could not be prepared"), nil
		}
		a.clearVisibleImage()
		post, postErr := a.raw.runWithGUIOperationLockHeld(ctx, postPayload)
		if postErr != nil || post.IsError || len(post.Images) != 1 || a.raw.coordinateArtifact == nil {
			return agent.BusinessError("wait completed, but the required fresh exact screenshot is unavailable"), postErr
		}
		if !declaration.matchesArtifact(a.raw) {
			return agent.BusinessError(
				"wait completed, but fresh screenshot dimensions do not match this request's native display declaration; retry only after the next declaration refresh",
			), nil
		}
		exact := a.raw.coordinateArtifact.ImageBlock()
		if !reflectImageBlockV1(post.Images[0], exact) {
			return agent.BusinessError("wait completed, but fresh screenshot authority did not match its image"), nil
		}
		result.Content = "Wait completed; a fresh exact screenshot is attached."
		result.Images = []agent.ImageBlock{exact}
		if err := a.markCurrentImageVisible(); err != nil {
			return agent.BusinessError(err.Error()), nil
		}
		return result, nil
	}

	// Every raw mutation attempt is followed by a strict exact observation while
	// the same GUI lock remains held. This includes typed failures because helper
	// transport loss can make commit state uncertain. The original typed
	// GUIOutcome remains the daemon's acknowledgement of the attempted action.
	postPayload, marshalErr := marshalAnthropicRawArgs(computerUseArgs{
		Action: "get_app_state", Description: "Post-action strict observation", IncludeScreenshot: true,
	})
	if marshalErr != nil {
		failure := agent.BusinessError("strict post-action observation could not be prepared")
		failure.GUIOutcome = result.GUIOutcome
		return failure, runErr
	}
	a.clearVisibleImage()
	post, postErr := a.raw.runWithGUIOperationLockHeld(ctx, postPayload)
	if postErr != nil || post.IsError || len(post.Images) != 1 || a.raw.coordinateArtifact == nil {
		failure := agent.BusinessError("computer action may have completed, but the required fresh exact screenshot is unavailable")
		failure.GUIOutcome = result.GUIOutcome
		if runErr != nil {
			return failure, runErr
		}
		return failure, postErr
	}
	if !declaration.matchesArtifact(a.raw) {
		failure := agent.BusinessError(
			"computer action may have completed, but fresh screenshot dimensions do not match this request's native display declaration; do not retry the action",
		)
		failure.GUIOutcome = result.GUIOutcome
		return failure, runErr
	}
	exact := a.raw.coordinateArtifact.ImageBlock()
	if !reflectImageBlockV1(post.Images[0], exact) {
		failure := agent.BusinessError("computer action may have completed, but fresh screenshot authority did not match its image")
		failure.GUIOutcome = result.GUIOutcome
		return failure, runErr
	}
	switch {
	case result.IsError:
		result.Content = "Computer action did not verify; a fresh exact screenshot is attached."
	case result.GUIOutcome != nil && result.GUIOutcome.Result == agent.GUIActionResultVerified:
		result.Content = "Computer action verified; a fresh exact screenshot is attached."
	default:
		// The provider cannot see GUIOutcome. Keep completed_unverified (and any
		// future non-verified acknowledgement) explicit in provider-visible text
		// so the model neither reports success nor retries a possibly committed
		// side effect automatically.
		result.Content = "Computer action completed but was not verified; do not retry automatically; a fresh exact screenshot is attached."
	}
	result.Images = []agent.ImageBlock{exact}
	if err := a.markCurrentImageVisible(); err != nil {
		failure := agent.BusinessError(err.Error())
		failure.GUIOutcome = result.GUIOutcome
		return failure, runErr
	}
	return result, runErr
}

var _ agent.Tool = (*AnthropicComputerAdapter)(nil)
var _ agent.NativeToolProvider = (*AnthropicComputerAdapter)(nil)
var _ agent.NativeToolRequestPreparer = (*AnthropicComputerAdapter)(nil)
var _ agent.SafeChecker = (*AnthropicComputerAdapter)(nil)
var _ agent.ReadOnlyChecker = (*AnthropicComputerAdapter)(nil)
var _ agent.ConcurrencySafeChecker = (*AnthropicComputerAdapter)(nil)
var _ agent.GUIActionDescriber = (*AnthropicComputerAdapter)(nil)
var _ ConsequentialRiskPreflighterV1 = (*AnthropicComputerAdapter)(nil)
