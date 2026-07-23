package tools

import (
	"context"
	"runtime"
	"testing"
)

func TestProcess_Info(t *testing.T) {
	tool := &ProcessTool{}
	info := tool.Info()
	if info.Name != "process" {
		t.Errorf("expected name 'process', got %q", info.Name)
	}
	if !containsString(info.Required, "action") || !containsString(info.Required, "description") {
		t.Errorf("expected Required to contain 'action' and 'description', got %v", info.Required)
	}
}

func TestProcess_InvalidArgs(t *testing.T) {
	tool := &ProcessTool{}
	result, err := tool.Run(context.Background(), `not valid json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for invalid JSON")
	}
}

func TestProcess_UnknownAction(t *testing.T) {
	tool := &ProcessTool{}
	result, err := tool.Run(context.Background(), `{"action": "restart","description":"test invalid action"}`)
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

func TestProcess_KillNoPID(t *testing.T) {
	tool := &ProcessTool{}
	result, err := tool.Run(context.Background(), `{"action": "kill","description":"test missing pid"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error result for kill without PID")
	}
	if !contains(result.Content, "pid is required") {
		t.Errorf("expected 'pid is required' in error, got: %s", result.Content)
	}
}

func TestProcess_List(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process list test not supported on Windows")
	}
	tool := &ProcessTool{}
	result, err := tool.Run(context.Background(), `{"action": "list","description":"test process list"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !contains(result.Content, "PID") {
		t.Errorf("expected PID header in ps output, got: %s", result.Content)
	}
}

func TestProcess_RequiresApproval(t *testing.T) {
	tool := &ProcessTool{}
	if !tool.RequiresApproval() {
		t.Error("expected RequiresApproval to return true")
	}
}

func TestProcess_IsSafeArgs(t *testing.T) {
	tool := &ProcessTool{}
	tests := []struct {
		argsJSON string
		safe     bool
	}{
		{`{"action": "list"}`, true},
		{`{"action": "ports"}`, true},
		{`{"action": "ports", "port": 8080}`, true},
		{`{"action": "kill", "pid": 1234}`, false},
		{`not valid json`, false},
	}
	for _, tt := range tests {
		if tool.IsSafeArgs(tt.argsJSON) != tt.safe {
			t.Errorf("IsSafeArgs(%q) = %v, want %v", tt.argsJSON, !tt.safe, tt.safe)
		}
	}
}
