package tools

import (
	"context"
	"testing"
)

func TestComputer_Info(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	info := tool.Info()
	if info.Name != "computer" {
		t.Errorf("expected name 'computer', got %q", info.Name)
	}
	// computer is a native Anthropic tool (NativeToolDef) — its
	// Parameters/Description are dropped on the wire by buildToolSchema, so
	// no `description` field is added (see description_field_test.go exemption).
	if !containsString(info.Required, "action") {
		t.Errorf("expected Required to contain 'action', got %v", info.Required)
	}
	props, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map in parameters")
	}
	for _, key := range []string{"action", "x", "y", "text", "keys", "button", "clicks"} {
		if _, exists := props[key]; !exists {
			t.Errorf("expected property %q in schema", key)
		}
	}
}

func TestComputer_RequiresApproval(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	if !tool.RequiresApproval() {
		t.Error("expected RequiresApproval to return true")
	}
}

func TestComputer_InvalidArgs(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `not valid json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid JSON")
	}
}

func TestComputer_MissingAction(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing action")
	}
	if !contains(result.Content, "missing required `action` parameter") {
		t.Errorf("expected 'missing required parameter: action' in error, got: %s", result.Content)
	}
}

func TestComputer_UnknownAction(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "fly"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for unknown action")
	}
	if !contains(result.Content, "unknown action") {
		t.Errorf("expected 'unknown action' in error, got: %s", result.Content)
	}
}

func TestComputer_TypeMissingText(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "type"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for type without text")
	}
	if !contains(result.Content, "type action requires 'text' parameter") {
		t.Errorf("expected text parameter error, got: %s", result.Content)
	}
}

func TestComputer_HotkeyMissingKeys(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "hotkey"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for hotkey without keys")
	}
	if !contains(result.Content, "hotkey action requires 'keys' parameter") {
		t.Errorf("expected keys parameter error, got: %s", result.Content)
	}
}

func TestComputer_EscapeAppleScript(t *testing.T) {
	tests := []struct {
		input    string
		expected string
	}{
		{`hello`, `hello`},
		{`say "hi"`, `say \"hi\"`},
		{"line1\nline2", `line1\nline2`},
		{`back\slash`, `back\\slash`},
	}
	for _, tc := range tests {
		got := escapeAppleScript(tc.input)
		if got != tc.expected {
			t.Errorf("escapeAppleScript(%q) = %q, want %q", tc.input, got, tc.expected)
		}
	}
}

func TestComputer_NormalizeArgs_LeftClick(t *testing.T) {
	args := &computerArgs{Action: "left_click", Coordinate: []int{640, 400}}
	normalizeArgs(args)
	if args.Action != "click" {
		t.Errorf("expected action 'click', got %q", args.Action)
	}
	if args.X != 640 || args.Y != 400 {
		t.Errorf("expected (640, 400), got (%d, %d)", args.X, args.Y)
	}
	if args.Button != "left" {
		t.Errorf("expected button 'left', got %q", args.Button)
	}
}

func TestComputer_NormalizeArgs_RightClick(t *testing.T) {
	args := &computerArgs{Action: "right_click", Coordinate: []int{100, 200}}
	normalizeArgs(args)
	if args.Action != "click" || args.Button != "right" {
		t.Errorf("expected click/right, got %s/%s", args.Action, args.Button)
	}
}

func TestComputer_NormalizeArgs_DoubleClick(t *testing.T) {
	args := &computerArgs{Action: "double_click", Coordinate: []int{50, 50}}
	normalizeArgs(args)
	if args.Action != "click" || args.Clicks != 2 {
		t.Errorf("expected click with 2 clicks, got %s/%d", args.Action, args.Clicks)
	}
}

func TestComputer_NormalizeArgs_MouseMove(t *testing.T) {
	args := &computerArgs{Action: "mouse_move", Coordinate: []int{300, 400}}
	normalizeArgs(args)
	if args.Action != "move" {
		t.Errorf("expected 'move', got %q", args.Action)
	}
	if args.X != 300 || args.Y != 400 {
		t.Errorf("expected (300, 400), got (%d, %d)", args.X, args.Y)
	}
}

func TestComputer_NormalizeArgs_Key(t *testing.T) {
	args := &computerArgs{Action: "key", Text: "Return"}
	normalizeArgs(args)
	if args.Action != "hotkey" {
		t.Errorf("expected 'hotkey', got %q", args.Action)
	}
	if args.Keys != "Return" {
		t.Errorf("expected keys 'Return', got %q", args.Keys)
	}
}

func TestComputer_NormalizeArgs_Screenshot(t *testing.T) {
	args := &computerArgs{Action: "screenshot"}
	normalizeArgs(args)
	if args.Action != "screenshot" {
		t.Errorf("expected 'screenshot', got %q", args.Action)
	}
}

func TestComputer_NormalizeArgs_NoOp(t *testing.T) {
	// Our custom actions pass through unchanged
	args := &computerArgs{Action: "click", X: 100, Y: 200}
	normalizeArgs(args)
	if args.Action != "click" || args.X != 100 || args.Y != 200 {
		t.Errorf("expected unchanged, got %s (%d, %d)", args.Action, args.X, args.Y)
	}
}

func TestComputer_ScaleXY(t *testing.T) {
	tool := &ComputerTool{client: &AXClient{}, screenW: 1440, screenH: 900}
	x, y := tool.scaleXY(640, 400)
	if x != 720 || y != 450 {
		t.Errorf("expected (720, 450), got (%d, %d)", x, y)
	}
}

func TestComputer_ScaleXY_DefaultFallback(t *testing.T) {
	// When screen dims match API dims, no scaling
	tool := &ComputerTool{client: &AXClient{}, screenW: 1280, screenH: 800}
	x, y := tool.scaleXY(100, 200)
	if x != 100 || y != 200 {
		t.Errorf("expected (100, 200), got (%d, %d)", x, y)
	}
}
