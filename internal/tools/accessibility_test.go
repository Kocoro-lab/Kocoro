package tools

import (
	"context"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestAccessibility_Info(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	info := tool.Info()
	if info.Name != "accessibility" {
		t.Errorf("expected name 'accessibility', got %q", info.Name)
	}
	for _, required := range []string{"action", "description"} {
		if !containsString(info.Required, required) {
			t.Errorf("expected required %q in %v", required, info.Required)
		}
	}
	props, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties map in parameters")
	}
	for _, key := range []string{"action", "description", "app", "max_depth", "filter", "ref", "value"} {
		if _, exists := props[key]; !exists {
			t.Errorf("expected property %q in schema", key)
		}
	}
}

func TestAccessibility_RequiresApproval(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	if !tool.RequiresApproval() {
		t.Error("accessibility mutations must participate in the approval path")
	}
}

func TestAccessibility_SafetyAndSerialization(t *testing.T) {
	tool := &AccessibilityTool{}
	for _, action := range []string{"read_tree", "annotate", "find", "get_value"} {
		args := `{"action":"` + action + `"}`
		if !tool.IsSafeArgs(args) {
			t.Errorf("%s should skip approval", action)
		}
		if tool.IsConcurrencySafeCall(args) {
			t.Errorf("%s must serialize because refs are mutable", action)
		}
	}
	for _, action := range []string{"click", "press", "set_value", "scroll"} {
		if tool.IsSafeArgs(`{"action":"` + action + `"}`) {
			t.Errorf("%s must require approval", action)
		}
	}
}

func TestAccessibility_InvalidJSON(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `not valid json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid JSON")
	}
	if result.ErrorCategory != agent.ErrCategoryValidation {
		t.Errorf("expected validation category, got %q", result.ErrorCategory)
	}
}

func TestAccessibility_MissingAction(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing action")
	}
	if !strings.Contains(result.Content, "missing required parameter: action") {
		t.Errorf("expected missing action error, got: %s", result.Content)
	}
}

func TestAccessibility_UnknownAction(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "fly", "description":"Fly app"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for unknown action")
	}
	if !strings.Contains(result.Content, "unknown action") {
		t.Errorf("expected 'unknown action' in error, got: %s", result.Content)
	}
}

func TestAccessibility_ClickMissingRef(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	result, err := tool.Run(context.Background(), `{"action": "click", "description":"Click control"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for click without ref")
	}
}

func TestAccessibility_ClickUnknownRef(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	tool.refs = map[string]refEntry{"e1": {path: "window[0]", pid: 1}}
	result, err := tool.Run(context.Background(), `{"action": "click", "ref": "e99", "description":"Click control"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for unknown ref")
	}
	if !strings.Contains(result.Content, "unknown ref") {
		t.Errorf("expected 'unknown ref' error, got: %s", result.Content)
	}
}

func TestAccessibility_SetValueMissingValue(t *testing.T) {
	tool := &AccessibilityTool{client: &AXClient{}}
	tool.refs = map[string]refEntry{"e1": {path: "window[0]/AXTextField[0]", role: "AXTextField", pid: 1}}
	result, err := tool.Run(context.Background(), `{"action": "set_value", "ref": "e1", "description":"Set field"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for set_value without value")
	}
}

func TestAccessibility_NilClient(t *testing.T) {
	tool := &AccessibilityTool{} // no client
	result, err := tool.Run(context.Background(), `{"action": "read_tree", "description":"Inspect app"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for nil client")
	}
}
