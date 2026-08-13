package tools

import (
	"context"
	"runtime"
	"testing"
)

func TestSystemInfo_Info(t *testing.T) {
	tool := &SystemInfoTool{}
	info := tool.Info()
	if info.Name != "system_info" {
		t.Errorf("expected name 'system_info', got %q", info.Name)
	}
}

func TestSystemInfo_Run(t *testing.T) {
	tool := &SystemInfoTool{}
	result, err := tool.Run(context.Background(), `{}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected error result: %s", result.Content)
	}
	if !contains(result.Content, runtime.GOOS) {
		t.Errorf("expected OS %q in output, got: %s", runtime.GOOS, result.Content)
	}
	if !contains(result.Content, runtime.GOARCH) {
		t.Errorf("expected arch %q in output, got: %s", runtime.GOARCH, result.Content)
	}
	if !contains(result.Content, "CPUs:") {
		t.Errorf("expected CPU count in output, got: %s", result.Content)
	}
}
