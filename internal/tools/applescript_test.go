package tools

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestAppleScript_Info(t *testing.T) {
	tool := &AppleScriptTool{}
	info := tool.Info()
	if info.Name != "applescript" {
		t.Errorf("expected name 'applescript', got %q", info.Name)
	}
	if !containsString(info.Required, "script") || !containsString(info.Required, "description") {
		t.Errorf("expected Required to contain 'script' and 'description', got %v", info.Required)
	}
}

func TestAppleScript_InvalidArgs(t *testing.T) {
	tool := &AppleScriptTool{}
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

func TestAppleScript_MissingScript(t *testing.T) {
	tool := &AppleScriptTool{}
	result, err := tool.Run(context.Background(), `{"description":"Do nothing"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing script")
	}
	if !strings.Contains(result.Content, "missing required parameter: script") {
		t.Errorf("expected missing script error, got: %s", result.Content)
	}
}

func TestAppleScript_MissingDescription(t *testing.T) {
	tool := &AppleScriptTool{}
	result, err := tool.Run(context.Background(), `{"script":"return 1"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for missing description")
	}
	if !strings.Contains(result.Content, "missing required parameter: description") {
		t.Errorf("expected missing description error, got: %s", result.Content)
	}
}

func TestAppleScript_SimpleScript(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("applescript tests require macOS")
	}
	tool := &AppleScriptTool{}
	result, err := tool.Run(context.Background(), `{"script": "return 1 + 1", "description": "Add two numbers"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !contains(result.Content, "2") {
		t.Errorf("expected '2' in output, got: %s", result.Content)
	}
}

func TestAppleScript_InvalidScript(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("applescript tests require macOS")
	}
	tool := &AppleScriptTool{}
	result, err := tool.Run(context.Background(), `{"script": "this is not valid applescript code!!!", "description": "Run broken script"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid script")
	}
}

func TestAppleScript_RequiresApproval(t *testing.T) {
	tool := &AppleScriptTool{}
	if !tool.RequiresApproval() {
		t.Error("expected RequiresApproval to return true")
	}
}
