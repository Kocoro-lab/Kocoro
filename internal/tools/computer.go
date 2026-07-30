package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

type ComputerTool struct {
	client             *AXClient
	screenW            int
	screenH            int
	toolW              int
	toolH              int
	readScreenGeometry func() (screenGeometry, error)
	captureScreen      func(context.Context, int) (string, agent.ImageBlock, error)
}

func (t *ComputerTool) ensureScreenDims() error {
	if t.screenW > 0 && t.screenH > 0 && t.toolW > 0 && t.toolH > 0 {
		return nil
	}
	read := t.readScreenGeometry
	if read == nil {
		read = GetMainScreenGeometry
	}
	geometry, err := read()
	if err != nil {
		return fmt.Errorf("screen geometry unavailable: %w", err)
	}
	if geometry.LogicalWidth <= 0 || geometry.LogicalHeight <= 0 ||
		geometry.CaptureWidth <= 0 || geometry.CaptureHeight <= 0 {
		return fmt.Errorf("screen geometry unavailable: invalid logical or capture dimensions")
	}
	toolW, toolH := resizeDimensions(
		geometry.CaptureWidth, geometry.CaptureHeight, DefaultAPIWidth)
	if toolW <= 0 || toolH <= 0 {
		return fmt.Errorf("screen geometry unavailable: invalid final tool dimensions")
	}
	t.screenW = geometry.LogicalWidth
	t.screenH = geometry.LogicalHeight
	t.toolW = toolW
	t.toolH = toolH
	return nil
}

func (t *ComputerTool) scaleXY(apiX, apiY int) (int, int, error) {
	if err := t.ensureScreenDims(); err != nil {
		return 0, 0, err
	}
	x, y := ScaleCoordinates(apiX, apiY, t.toolW, t.toolH, t.screenW, t.screenH)
	x, y = ClampCoordinates(x, y, t.screenW, t.screenH)
	return x, y, nil
}

func (t *ComputerTool) captureAfterAction(ctx context.Context, result agent.ToolResult) agent.ToolResult {
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return result
	case <-timer.C:
	}
	path, block, err := t.captureExactToolImage(ctx)
	if path != "" {
		defer os.Remove(path)
	}
	if err != nil {
		return result // Non-fatal
	}
	result.Images = []agent.ImageBlock{block}
	return result
}

func (t *ComputerTool) captureExactToolImage(ctx context.Context) (string, agent.ImageBlock, error) {
	if err := t.ensureScreenDims(); err != nil {
		return "", agent.ImageBlock{}, err
	}
	capture := t.captureScreen
	if capture == nil {
		capture = CaptureMainDisplayAndEncodeContext
	}
	path, block, err := capture(ctx, DefaultAPIWidth)
	if err != nil {
		return path, block, err
	}
	width, height, err := imageFileDimensions(path)
	if err != nil {
		return path, agent.ImageBlock{}, fmt.Errorf("read legacy computer screenshot dimensions: %w", err)
	}
	adoptCapturedDimensions := false
	if width != t.toolW || height != t.toolH {
		if !compatibleCapturedToolDimensions(
			width, height, t.screenW, t.screenH,
		) {
			return path, agent.ImageBlock{}, fmt.Errorf(
				"legacy computer screenshot dimensions %dx%d do not match declared tool space %dx%d",
				width, height, t.toolW, t.toolH)
		}
		adoptCapturedDimensions = true
	}
	finalWidth, finalHeight, err := imageBlockDimensions(block)
	if err != nil {
		return path, agent.ImageBlock{}, fmt.Errorf("read final legacy computer image dimensions: %w", err)
	}
	expectedWidth, expectedHeight := t.toolW, t.toolH
	if adoptCapturedDimensions {
		expectedWidth, expectedHeight = width, height
	}
	if finalWidth != expectedWidth || finalHeight != expectedHeight {
		return path, agent.ImageBlock{}, fmt.Errorf(
			"final legacy computer image dimensions %dx%d do not match declared tool space %dx%d",
			finalWidth, finalHeight, expectedWidth, expectedHeight)
	}
	if adoptCapturedDimensions {
		t.toolW, t.toolH = width, height
	}
	return path, block, nil
}

// compatibleCapturedToolDimensions admits display-scale differences only.
// macOS virtual displays can report 1024x768 through every geometry API while
// screencapture emits 1280x960. The actual image is the tool's coordinate
// space, but it may replace the metadata-derived size only when it preserves
// the logical display aspect (within one pixel of resize rounding) and the
// existing capture bound. Crops, wrong-display images, and oversized output
// still fail closed.
func compatibleCapturedToolDimensions(
	width, height, logicalWidth, logicalHeight int,
) bool {
	if width <= 0 || height <= 0 ||
		logicalWidth <= 0 || logicalHeight <= 0 ||
		width > DefaultAPIWidth || height > DefaultAPIWidth {
		return false
	}
	delta := int64(width)*int64(logicalHeight) -
		int64(height)*int64(logicalWidth)
	if delta < 0 {
		delta = -delta
	}
	return delta <= int64(max(logicalWidth, logicalHeight))
}

type computerArgs struct {
	Action      string `json:"action"`
	X           int    `json:"x,omitempty"`
	Y           int    `json:"y,omitempty"`
	Text        string `json:"text,omitempty"`
	Keys        string `json:"keys,omitempty"`
	Button      string `json:"button,omitempty"`
	Clicks      int    `json:"clicks,omitempty"`
	Coordinate  []int  `json:"coordinate,omitempty"` // Historical provider-shaped compatibility: [x, y]
	Description string `json:"description,omitempty"`
}

// normalizeArgs keeps historical provider-shaped calls replay-compatible.
// New provider-native calls must use AnthropicComputerAdapter instead.
func normalizeArgs(args *computerArgs) {
	// Map Anthropic coordinate array to x, y
	if len(args.Coordinate) == 2 {
		args.X = args.Coordinate[0]
		args.Y = args.Coordinate[1]
	}

	// Map historical provider action names to legacy internal actions.
	switch args.Action {
	case "left_click":
		args.Action = "click"
		args.Button = "left"
		args.Clicks = 1
	case "right_click":
		args.Action = "click"
		args.Button = "right"
		args.Clicks = 1
	case "double_click":
		args.Action = "click"
		args.Button = "left"
		args.Clicks = 2
	case "middle_click":
		args.Action = "click"
		args.Button = "left" // fallback — no middle click support
		args.Clicks = 1
	case "triple_click":
		args.Action = "click"
		args.Button = "left"
		args.Clicks = 3
	case "mouse_move":
		args.Action = "move"
	case "key":
		args.Action = "hotkey"
		if args.Text != "" && args.Keys == "" {
			args.Keys = args.Text // Anthropic sends key combo in "text" field
		}
	case "screenshot":
		args.Action = "screenshot"
	}
}

func (t *ComputerTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name: "computer",
		// This is the rollback-compatible legacy function tool. Anthropic's
		// provider-native computer identity is reserved for the separately
		// attested Accessibility-first adapter.
		Description: "OS-level mouse and keyboard control for macOS. Use for coordinate-based clicks, typing text (CJK/emoji safe), and keyboard shortcuts. For clicking UI elements, prefer computer_use or accessibility (ref-based) over coordinate clicks. Actions: click, type, hotkey, move, screenshot." +
			agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":      map[string]any{"type": "string", "description": "Action to perform: click, type, hotkey, move, screenshot"},
				"x":           map[string]any{"type": "integer", "description": "Screen X coordinate (for click/move)"},
				"y":           map[string]any{"type": "integer", "description": "Screen Y coordinate (for click/move)"},
				"text":        map[string]any{"type": "string", "description": "Text to type (for type action)"},
				"keys":        map[string]any{"type": "string", "description": "Key combination like command+c, command+shift+4 (for hotkey action)"},
				"button":      map[string]any{"type": "string", "description": "Mouse button: left (default), right (for click action)"},
				"clicks":      map[string]any{"type": "integer", "description": "Number of clicks: 1 (default), 2 for double-click (for click action)"},
				"description": agent.DescriptionFieldSpec,
			},
		},
		Required: []string{"action", "description"},
	}
}

func (t *ComputerTool) RequiresApproval() bool { return true }

func (t *ComputerTool) IsReadOnlyCall(string) bool { return false }

func (t *ComputerTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
	if runtime.GOOS != "darwin" {
		return agent.ToolResult{Content: "computer tool is only available on macOS", IsError: true}, nil
	}
	var args computerArgs
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}

	if strings.TrimSpace(args.Action) == "" {
		return agent.ValidationError("computer: missing required `action` parameter"), nil
	}
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("computer: missing required `description` parameter"), nil
	}

	normalizeArgs(&args)

	// Share the process-wide GUI-operation lock with computer_use so a native
	// computer action from one route cannot interleave with another route's
	// stale-state preflight + action. See computerUseGUIOperationMu.
	computerUseGUIOperationMu.Lock()
	defer computerUseGUIOperationMu.Unlock()

	switch args.Action {
	case "screenshot":
		return t.screenshot(ctx)
	case "click":
		if t.client == nil {
			return agent.ToolResult{Content: "computer tool requires macOS with ax_server", IsError: true}, nil
		}
		return t.click(ctx, args)
	case "type":
		if t.client == nil {
			return agent.ToolResult{Content: "computer tool requires macOS with ax_server", IsError: true}, nil
		}
		return t.typeText(ctx, args)
	case "hotkey":
		if t.client == nil {
			return agent.ToolResult{Content: "computer tool requires macOS with ax_server", IsError: true}, nil
		}
		return t.hotkey(ctx, args)
	case "move":
		if t.client == nil {
			return agent.ToolResult{Content: "computer tool requires macOS with ax_server", IsError: true}, nil
		}
		return t.move(ctx, args)
	default:
		return agent.ToolResult{
			Content: fmt.Sprintf("unknown action: %q (valid: click, type, hotkey, move, screenshot)", args.Action),
			IsError: true,
		}, nil
	}
}

func (t *ComputerTool) screenshot(ctx context.Context) (agent.ToolResult, error) {
	path, block, err := t.captureExactToolImage(ctx)
	if path != "" {
		defer os.Remove(path)
	}
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("screenshot error: %v", err), IsError: true}, nil
	}
	return agent.ToolResult{
		Content: "Screenshot captured.",
		Images:  []agent.ImageBlock{block},
	}, nil
}

func (t *ComputerTool) click(ctx context.Context, args computerArgs) (agent.ToolResult, error) {
	x, y, err := t.scaleXY(args.X, args.Y)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("click error: %v", err), IsError: true}, nil
	}
	button := args.Button
	if button == "" {
		button = "left"
	}
	clicks := args.Clicks
	if clicks < 1 {
		clicks = 1
	}

	rawResult, err := t.client.Call(ctx, "mouse_event", map[string]any{
		"type":   "click",
		"x":      float64(x),
		"y":      float64(y),
		"button": button,
		"clicks": clicks,
	})
	if err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("click error: %v", err),
			IsError: true,
		}, nil
	}

	msg := fmt.Sprintf("Clicked %s button %d time(s) at (%d, %d)", button, clicks, x, y)
	msg += parseActionContext(rawResult)
	result := agent.ToolResult{Content: msg}
	return t.captureAfterAction(ctx, result), nil
}

func (t *ComputerTool) typeText(ctx context.Context, args computerArgs) (agent.ToolResult, error) {
	if args.Text == "" {
		return agent.ToolResult{Content: "type action requires 'text' parameter", IsError: true}, nil
	}

	// ax_server handles CJK/non-ASCII via clipboard paste automatically
	rawResult, err := t.client.Call(ctx, "type_text", map[string]any{
		"value": args.Text,
	})
	if err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("type error: %v", err),
			IsError: true,
		}, nil
	}

	result := agent.ToolResult{Content: legacyComputerTypeAcknowledgement(rawResult)}
	return t.captureAfterAction(ctx, result), nil
}

func legacyComputerTypeAcknowledgement(rawResult json.RawMessage) string {
	return "Typed text (content redacted)." + parseActionContext(rawResult)
}

func (t *ComputerTool) hotkey(ctx context.Context, args computerArgs) (agent.ToolResult, error) {
	if args.Keys == "" {
		return agent.ToolResult{Content: "hotkey action requires 'keys' parameter", IsError: true}, nil
	}

	parts := strings.Split(strings.ToLower(args.Keys), "+")
	if len(parts) == 0 {
		return agent.ToolResult{Content: fmt.Sprintf("invalid key combination: %q", args.Keys), IsError: true}, nil
	}

	key := strings.TrimSpace(parts[len(parts)-1])
	var modifiers []string
	for _, part := range parts[:len(parts)-1] {
		modifiers = append(modifiers, strings.TrimSpace(part))
	}

	rawResult, err := t.client.Call(ctx, "key_event", map[string]any{
		"key":       key,
		"modifiers": modifiers,
	})
	if err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("hotkey error: %v", err),
			IsError: true,
		}, nil
	}

	msg := fmt.Sprintf("Pressed: %s", args.Keys)
	msg += parseActionContext(rawResult)
	result := agent.ToolResult{Content: msg}
	return t.captureAfterAction(ctx, result), nil
}

func (t *ComputerTool) move(ctx context.Context, args computerArgs) (agent.ToolResult, error) {
	x, y, err := t.scaleXY(args.X, args.Y)
	if err != nil {
		return agent.ToolResult{Content: fmt.Sprintf("move error: %v", err), IsError: true}, nil
	}

	rawResult, err := t.client.Call(ctx, "mouse_event", map[string]any{
		"type": "move",
		"x":    float64(x),
		"y":    float64(y),
	})
	if err != nil {
		return agent.ToolResult{
			Content: fmt.Sprintf("move error: %v", err),
			IsError: true,
		}, nil
	}

	msg := fmt.Sprintf("Moved cursor to (%d, %d)", x, y)
	msg += parseActionContext(rawResult)
	result := agent.ToolResult{Content: msg}
	return t.captureAfterAction(ctx, result), nil
}

// parseActionContext extracts the context field from an ax_server action response
// and formats it as a human-readable string.
func parseActionContext(raw json.RawMessage) string {
	var resp struct {
		Context *appContext `json:"context,omitempty"`
	}
	if err := json.Unmarshal(raw, &resp); err != nil {
		return ""
	}
	return formatContext(resp.Context)
}
