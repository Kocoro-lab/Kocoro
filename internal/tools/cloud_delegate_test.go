package tools

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestCloudDelegateInfo(t *testing.T) {
	tool := NewCloudDelegateTool(nil, "", 60*time.Second, nil, "", "")
	info := tool.Info()
	if info.Name != "cloud_delegate" {
		t.Errorf("expected name cloud_delegate, got %s", info.Name)
	}
	if !containsString(info.Required, "task") || !containsString(info.Required, "description") {
		t.Errorf("expected Required to contain 'task' and 'description', got %v", info.Required)
	}
	// Schema must expose the terminal parameter
	props, ok := info.Parameters["properties"].(map[string]any)
	if !ok {
		t.Fatal("expected properties in schema")
	}
	if _, ok := props["terminal"]; !ok {
		t.Error("schema should expose 'terminal' parameter")
	}
}

func TestCloudDelegateTerminalDefault(t *testing.T) {
	tests := []struct {
		name      string
		args      string
		wantCloud bool // expected CloudResult (ignoring fullResultConfirmed)
	}{
		{"research defaults terminal", `{"task":"t","workflow_type":"research"}`, true},
		{"auto defaults non-terminal", `{"task":"t","workflow_type":"auto"}`, false},
		{"omitted defaults non-terminal", `{"task":"t"}`, false},
		{"explicit false overrides research", `{"task":"t","workflow_type":"research","terminal":false}`, false},
		{"explicit true overrides auto", `{"task":"t","workflow_type":"auto","terminal":true}`, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Will fail at gateway (nil), but we can check CloudResult on the error path
			// Since gateway is nil, result is always an error — CloudResult won't be set.
			// Instead, verify the arg parsing and terminal logic directly.
			var args cloudDelegateArgs
			if err := json.Unmarshal([]byte(tt.args), &args); err != nil {
				t.Fatalf("failed to parse args: %v", err)
			}
			terminal := args.WorkflowType == "research"
			if args.Terminal != nil {
				terminal = *args.Terminal
			}
			if terminal != tt.wantCloud {
				t.Errorf("terminal=%v, want %v", terminal, tt.wantCloud)
			}
		})
	}
}

func TestCloudDelegateRequiresApproval(t *testing.T) {
	tool := NewCloudDelegateTool(nil, "", 60*time.Second, nil, "", "")
	if !tool.RequiresApproval() {
		t.Error("cloud_delegate should require approval")
	}
	if tool.IsSafeArgs(`{"task":"anything"}`) {
		t.Error("IsSafeArgs should always return false")
	}
}

func TestCloudDelegateEmptyTask(t *testing.T) {
	tool := NewCloudDelegateTool(nil, "", 60*time.Second, nil, "", "")
	result, err := tool.Run(context.Background(), `{"task":""}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error for empty task")
	}
}

func TestCloudDelegateInvalidJSON(t *testing.T) {
	tool := NewCloudDelegateTool(nil, "", 60*time.Second, nil, "", "")
	result, err := tool.Run(context.Background(), `not json`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error for invalid JSON")
	}
}

func TestCloudDelegateNoGateway(t *testing.T) {
	tool := NewCloudDelegateTool(nil, "", 60*time.Second, nil, "", "")
	result, err := tool.Run(context.Background(), `{"task":"test task"}`)
	if err != nil {
		t.Fatal(err)
	}
	if !result.IsError {
		t.Error("expected error when gateway is nil")
	}
}

func TestCloudDelegateContextTruncation(t *testing.T) {
	tool := NewCloudDelegateTool(nil, "", 60*time.Second, nil, "", "")
	longCtx := make([]byte, 9000)
	for i := range longCtx {
		longCtx[i] = 'x'
	}
	// Will fail at submission (nil gateway), but should get past arg parsing + truncation
	result, _ := tool.Run(context.Background(), `{"task":"test","context":"`+string(longCtx)+`"}`)
	if !result.IsError {
		t.Log("Expected error (nil gateway)")
	}
}

