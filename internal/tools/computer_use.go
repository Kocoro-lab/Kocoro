package tools

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// axCallClient is the narrow ax_server seam used by computer_use. Keeping the
// interface local makes the state/approval contract testable without starting
// the macOS helper; AXClient is the production implementation.
type axCallClient interface {
	Call(context.Context, string, any) (json.RawMessage, error)
}

type computerUseTree struct {
	App      string                       `json:"app"`
	PID      int                          `json:"pid"`
	Window   string                       `json:"window"`
	Elements []any                        `json:"elements"`
	RefPaths map[string]map[string]string `json:"ref_paths"`
}

type computerUseSnapshot struct {
	id         string
	status     string
	app        string
	pid        int
	window     string
	filter     string
	budget     int
	elements   []any
	signatures map[string]string
}

type computerUseArgs struct {
	Action            string  `json:"action"`
	Description       string  `json:"description"`
	StateID           string  `json:"state_id,omitempty"`
	App               string  `json:"app,omitempty"`
	Window            string  `json:"window,omitempty"`
	Ref               string  `json:"ref,omitempty"`
	Value             *string `json:"value,omitempty"`
	X                 *int    `json:"x,omitempty"`
	Y                 *int    `json:"y,omitempty"`
	Text              *string `json:"text,omitempty"`
	Keys              string  `json:"keys,omitempty"`
	Button            string  `json:"button,omitempty"`
	Clicks            int     `json:"clicks,omitempty"`
	DX                int     `json:"dx,omitempty"`
	DY                int     `json:"dy,omitempty"`
	Condition         string  `json:"condition,omitempty"`
	Query             string  `json:"query,omitempty"`
	Role              string  `json:"role,omitempty"`
	Timeout           float64 `json:"timeout,omitempty"`
	Interval          float64 `json:"interval,omitempty"`
	Filter            string  `json:"filter,omitempty"`
	SemanticBudget    int     `json:"semantic_budget,omitempty"`
	IncludeScreenshot bool    `json:"include_screenshot,omitempty"`
}

// ComputerUseTool is the provider-neutral macOS GUI tool. It deliberately
// keeps only one current observation per agent run: refs are meaningful only
// for that state_id and every ref action re-observes before touching the GUI.
type ComputerUseTool struct {
	client        axCallClient
	snapshot      *computerUseSnapshot
	refs          map[string]refEntry
	screenW       int
	screenH       int
	captureScreen func(int) (string, agent.ImageBlock, error)
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
		Description: "Observe and operate native macOS apps through one stateful Accessibility-first workflow. " +
			"Start with get_app_state, then use its state_id and element refs. Screenshots are opt-in; use coordinates only when semantic refs are unavailable. " +
			"Use browser tools for web-page DOM interactions." + agent.DescriptionGuidance,
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"action":             map[string]any{"type": "string", "description": "Action: get_app_state, focus_app, launch_app, click, press, set_value, get_value, scroll, type, hotkey, move, wait, screenshot"},
				"description":        agent.DescriptionFieldSpec,
				"state_id":           map[string]any{"type": "string", "description": "Latest state_id from get_app_state; required with ref actions"},
				"app":                map[string]any{"type": "string", "description": "Target macOS app name; omitted means frontmost app where supported"},
				"window":             map[string]any{"type": "string", "description": "Optional window-title substring"},
				"ref":                map[string]any{"type": "string", "description": "Element ref from the matching state_id"},
				"value":              map[string]any{"type": "string", "description": "Value for set_value or wait matching"},
				"x":                  map[string]any{"type": "integer", "description": "X coordinate in the 1280x800 tool coordinate space"},
				"y":                  map[string]any{"type": "integer", "description": "Y coordinate in the 1280x800 tool coordinate space"},
				"text":               map[string]any{"type": "string", "description": "Text for the type action"},
				"keys":               map[string]any{"type": "string", "description": "Key combination such as command+shift+p"},
				"button":             map[string]any{"type": "string", "description": "Mouse button: left (default) or right"},
				"clicks":             map[string]any{"type": "integer", "description": "Click count (default 1)"},
				"dx":                 map[string]any{"type": "integer", "description": "Horizontal scroll amount in pixels"},
				"dy":                 map[string]any{"type": "integer", "description": "Vertical scroll amount in pixels; positive is down"},
				"condition":          map[string]any{"type": "string", "description": "Wait condition: elementExists, elementGone, titleContains, urlContains, titleChanged, urlChanged"},
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

func (t *ComputerUseTool) Run(ctx context.Context, argsJSON string) (agent.ToolResult, error) {
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
	if strings.TrimSpace(args.Description) == "" {
		return agent.ValidationError("missing required parameter: description"), nil
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

	computerUseGUIOperationMu.Lock()
	defer computerUseGUIOperationMu.Unlock()

	switch args.Action {
	case "get_app_state":
		return t.getAppState(ctx, args)
	case "focus_app":
		return t.focusOrLaunch(ctx, args, "focus")
	case "launch_app":
		return t.focusOrLaunch(ctx, args, "launch_app")
	case "click":
		return t.click(ctx, args)
	case "press":
		return t.refAction(ctx, args, "press", false)
	case "set_value":
		if args.Value == nil {
			return agent.ValidationError("set_value requires 'value' parameter"), nil
		}
		return t.refAction(ctx, args, "set_value", false)
	case "get_value":
		return t.refAction(ctx, args, "get_value", true)
	case "scroll":
		return t.scroll(ctx, args)
	case "type":
		return t.typeText(ctx, args)
	case "hotkey":
		return t.hotkey(ctx, args)
	case "move":
		return t.move(ctx, args)
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
		return computerUseTree{}, computerUseCallError("observe app", err), false
	}
	var tree computerUseTree
	if err := json.Unmarshal(raw, &tree); err != nil {
		return computerUseTree{}, agent.BusinessError(fmt.Sprintf("parse accessibility state: %v", err)), false
	}
	if tree.PID <= 0 {
		return computerUseTree{}, agent.BusinessError("accessibility state did not include a valid pid"), false
	}
	if tree.RefPaths == nil {
		tree.RefPaths = make(map[string]map[string]string)
	}
	return tree, agent.ToolResult{}, true
}

func computerUseStateID(tree computerUseTree) string {
	canonical, _ := json.Marshal(tree)
	sum := sha256.Sum256(canonical)
	return "s_" + hex.EncodeToString(sum[:8])
}

func computerUseSignatures(elements []any) map[string]string {
	result := make(map[string]string)
	var walk func(any)
	walk = func(value any) {
		node, ok := value.(map[string]any)
		if !ok {
			return
		}
		if ref, _ := node["ref"].(string); ref != "" {
			copyNode := make(map[string]any, len(node))
			for key, field := range node {
				if key != "children" {
					copyNode[key] = field
				}
			}
			encoded, _ := json.Marshal(copyNode)
			result[ref] = string(encoded)
		}
		if children, ok := node["children"].([]any); ok {
			for _, child := range children {
				walk(child)
			}
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
	pid, failure, ok := t.resolvePID(ctx, args.App)
	if !ok {
		return failure, nil
	}
	filter := args.Filter
	if filter == "" {
		filter = "interactive"
	}
	budget := args.SemanticBudget
	if budget <= 0 {
		budget = 25
	}
	tree, failure, ok := t.readTree(ctx, pid, filter, budget)
	if !ok {
		return failure, nil
	}

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

	t.snapshot = &computerUseSnapshot{
		id:         id,
		status:     status,
		app:        tree.App,
		pid:        tree.PID,
		window:     tree.Window,
		filter:     filter,
		budget:     budget,
		elements:   tree.Elements,
		signatures: signatures,
	}
	t.refs = make(map[string]refEntry, len(tree.RefPaths))
	for ref, entry := range tree.RefPaths {
		t.refs[ref] = refEntry{path: entry["path"], role: entry["role"], pid: tree.PID}
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

	result := agent.ToolResult{Content: strings.Join(lines, "\n")}
	if args.IncludeScreenshot {
		block, failure, ok := t.captureWindow(ctx, tree.PID, tree.App, args.Window)
		if ok {
			result.Images = []agent.ImageBlock{block}
		} else {
			result.Content += "\nscreenshot_warning: " + failure.Content
		}
	}
	return result, nil
}

func formatComputerUseElements(elements []any) []string {
	lines := make([]string, 0, len(elements))
	var walk func(any, int)
	walk = func(value any, depth int) {
		node, ok := value.(map[string]any)
		if !ok {
			return
		}
		fields := make([]string, 0, 8)
		for _, key := range []string{"ref", "role", "subrole", "title", "desc", "value", "enabled", "selected"} {
			field, exists := node[key]
			if !exists || field == nil || field == "" {
				continue
			}
			if text, ok := field.(string); ok {
				fields = append(fields, fmt.Sprintf("%s=%q", key, text))
			} else {
				fields = append(fields, fmt.Sprintf("%s=%v", key, field))
			}
		}
		if len(fields) > 0 {
			lines = append(lines, strings.Repeat("  ", depth)+"- "+strings.Join(fields, " "))
		}
		if children, ok := node["children"].([]any); ok {
			for _, child := range children {
				walk(child, depth+1)
			}
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
	if method == "scroll" {
		params["dx"] = args.DX
		params["dy"] = args.DY
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
		return t.refAction(ctx, args, "click", false)
	}
	if args.X == nil || args.Y == nil {
		return agent.ValidationError("click requires either ref+state_id or x+y coordinates"), nil
	}
	x, y := t.scaleXY(*args.X, *args.Y)
	button := args.Button
	if button == "" {
		button = "left"
	}
	if button != "left" && button != "right" {
		return agent.ValidationError("button must be 'left' or 'right'"), nil
	}
	clicks := args.Clicks
	if clicks <= 0 {
		clicks = 1
	}
	raw, err := t.client.Call(ctx, "mouse_event", map[string]any{
		"type": "click", "x": float64(x), "y": float64(y), "button": button, "clicks": clicks,
	})
	if err != nil {
		return computerUseCallError("click", err), nil
	}
	t.invalidateState()
	return agent.ToolResult{Content: computerUseActionMessage(raw, fmt.Sprintf("clicked at (%d, %d)", x, y))}, nil
}

func (t *ComputerUseTool) scroll(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	if args.Ref != "" {
		return t.refAction(ctx, args, "scroll", false)
	}
	pid, failure, ok := t.resolvePID(ctx, args.App)
	if !ok {
		return failure, nil
	}
	params := map[string]any{"dx": args.DX, "dy": args.DY}
	if pid > 0 {
		params["pid"] = pid
	}
	raw, err := t.client.Call(ctx, "scroll", params)
	if err != nil {
		return computerUseCallError("scroll", err), nil
	}
	t.invalidateState()
	return agent.ToolResult{Content: computerUseActionMessage(raw, "scrolled")}, nil
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
	raw, err := t.client.Call(ctx, "type_text", map[string]any{"value": *args.Text})
	if err != nil {
		return computerUseCallError("type", err), nil
	}
	t.invalidateState()
	return agent.ToolResult{Content: computerUseActionMessage(raw, "text typed")}, nil
}

func (t *ComputerUseTool) hotkey(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	if args.Keys == "" {
		return agent.ValidationError("hotkey requires 'keys'"), nil
	}
	parts := strings.Split(strings.ToLower(args.Keys), "+")
	key := strings.TrimSpace(parts[len(parts)-1])
	if key == "" {
		return agent.ValidationError("hotkey requires a final key"), nil
	}
	modifiers := make([]string, 0, len(parts)-1)
	for _, part := range parts[:len(parts)-1] {
		if modifier := strings.TrimSpace(part); modifier != "" {
			modifiers = append(modifiers, modifier)
		}
	}
	raw, err := t.client.Call(ctx, "key_event", map[string]any{"key": key, "modifiers": modifiers})
	if err != nil {
		return computerUseCallError("hotkey", err), nil
	}
	t.invalidateState()
	return agent.ToolResult{Content: computerUseActionMessage(raw, "hotkey pressed")}, nil
}

func (t *ComputerUseTool) move(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	if args.X == nil || args.Y == nil {
		return agent.ValidationError("move requires x+y coordinates"), nil
	}
	x, y := t.scaleXY(*args.X, *args.Y)
	raw, err := t.client.Call(ctx, "mouse_event", map[string]any{"type": "move", "x": float64(x), "y": float64(y)})
	if err != nil {
		return computerUseCallError("move", err), nil
	}
	t.invalidateState()
	return agent.ToolResult{Content: computerUseActionMessage(raw, fmt.Sprintf("moved to (%d, %d)", x, y))}, nil
}

func (t *ComputerUseTool) wait(ctx context.Context, args computerUseArgs) (agent.ToolResult, error) {
	if args.Condition == "" {
		return agent.ValidationError("wait requires 'condition'"), nil
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
	if args.App != "" {
		pid, failure, ok := t.resolvePID(ctx, args.App)
		if !ok {
			return failure, nil
		}
		block, failure, ok := t.captureWindow(ctx, pid, args.App, args.Window)
		if !ok {
			return failure, nil
		}
		return agent.ToolResult{Content: "Captured app window", Images: []agent.ImageBlock{block}}, nil
	}
	capture := t.captureScreen
	if capture == nil {
		capture = CaptureAndEncode
	}
	path, block, err := capture(DefaultAPIWidth)
	if path != "" {
		defer os.Remove(path)
	}
	if err != nil {
		return computerUseCallError("screenshot", err), nil
	}
	return agent.ToolResult{Content: "Captured desktop screenshot", Images: []agent.ImageBlock{block}}, nil
}

func (t *ComputerUseTool) captureWindow(ctx context.Context, pid int, app, window string) (agent.ImageBlock, agent.ToolResult, bool) {
	params := map[string]any{"pid": pid}
	if app != "" {
		params["app_name"] = app
	}
	if window != "" {
		params["window_title"] = window
	}
	raw, err := t.client.Call(ctx, "capture_window", params)
	if err != nil {
		return agent.ImageBlock{}, computerUseCallError("capture window", err), false
	}
	var capture struct {
		OK          bool   `json:"ok"`
		Code        string `json:"code"`
		ImageBase64 string `json:"image_base64"`
	}
	if err := json.Unmarshal(raw, &capture); err != nil {
		return agent.ImageBlock{}, agent.BusinessError(fmt.Sprintf("parse window screenshot: %v", err)), false
	}
	if !capture.OK {
		if capture.Code == "screen_recording_denied" {
			return agent.ImageBlock{}, agent.PermissionError("Screen Recording permission is required for window screenshots"), false
		}
		return agent.ImageBlock{}, agent.BusinessError("window screenshot failed: " + capture.Code), false
	}
	bytes, err := base64.StdEncoding.DecodeString(capture.ImageBase64)
	if err != nil {
		return agent.ImageBlock{}, agent.BusinessError(fmt.Sprintf("decode window screenshot: %v", err)), false
	}
	block, err := EncodeImageBytes(bytes, "image/png")
	if err != nil {
		return agent.ImageBlock{}, agent.BusinessError(fmt.Sprintf("encode window screenshot: %v", err)), false
	}
	return block, agent.ToolResult{}, true
}

func (t *ComputerUseTool) ensureScreenDims() {
	if t.screenW > 0 && t.screenH > 0 {
		return
	}
	w, h, err := GetScreenDimensions()
	if err != nil {
		t.screenW, t.screenH = DefaultAPIWidth, DefaultAPIHeight
		return
	}
	t.screenW, t.screenH = w, h
}

func (t *ComputerUseTool) scaleXY(apiX, apiY int) (int, int) {
	t.ensureScreenDims()
	x, y := ScaleCoordinates(apiX, apiY, DefaultAPIWidth, DefaultAPIHeight, t.screenW, t.screenH)
	return ClampCoordinates(x, y, t.screenW, t.screenH)
}

func (t *ComputerUseTool) invalidateState() {
	t.snapshot = nil
	t.refs = nil
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
