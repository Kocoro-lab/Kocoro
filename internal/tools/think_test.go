package tools

import (
	"context"
	"strings"
	"testing"
)

// TestThinkTool_AckNotEcho verifies the tool returns a short ack instead of
// echoing the thought back. The thought lives in the assistant message's
// tool_use.input.thought field — echoing into tool_result was double-counting
// it against cache. Build A of plan #8.
func TestThinkTool_AckNotEcho(t *testing.T) {
	tool := &ThinkTool{}

	longThought := "I should read the file first. Then check the imports. " +
		"Then look for the bug in the parser. Specifically, the case where " +
		"input is empty might trigger the panic we saw earlier."
	result, err := tool.Run(context.Background(), `{"thought":"`+longThought+`"}`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.IsError {
		t.Fatalf("unexpected tool error: %s", result.Content)
	}
	if result.Content == longThought {
		t.Errorf("tool_result must NOT echo the thought (would double-count vs assistant tool_use.input)")
	}
	if result.Content != "thought logged" {
		t.Errorf("expected ack 'thought logged', got %q", result.Content)
	}
	if len(result.Content) > 50 {
		t.Errorf("ack must be short (~15B), got %d bytes", len(result.Content))
	}
}

// TestThinkTool_EmptyThoughtSoftHint verifies that empty/whitespace `thought`
// values return a non-error hint instead of a hard tool error.
//
// Why: Sonnet 4.6 / Opus 4.7 with native extended thinking occasionally emit
// ritual `think({})` calls because the actual reasoning lives in the native
// thinking content block. Hard-erroring on those calls caused the agent loop
// to spin until force-stop (14-min hang in production). The soft hint lets
// the model immediately re-orient without burning the loop-detector budget.
// See plan 2026-05-14-thinking-blocks-cc-alignment.md Phase 0.1.
func TestThinkTool_EmptyThoughtSoftHint(t *testing.T) {
	tool := &ThinkTool{}

	cases := []struct {
		name     string
		argsJSON string
	}{
		{"empty object", `{}`},
		{"empty string", `{"thought":""}`},
		{"whitespace only", `{"thought":"   "}`},
		{"tab and newline", `{"thought":"\t\n  "}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := tool.Run(context.Background(), tc.argsJSON)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.IsError {
				t.Errorf("IsError = true, want false (soft hint instead of hard error); content=%q", result.Content)
			}
			lc := strings.ToLower(result.Content)
			if !strings.Contains(lc, "empty thought") {
				t.Errorf("expected content to mention 'empty thought' explicitly, got %q", result.Content)
			}
			if !strings.Contains(lc, "proceed") && !strings.Contains(lc, "do not retry") {
				t.Errorf("expected imperative steering (proceed / do not retry), got %q", result.Content)
			}
		})
	}
}

func TestThinkTool_InvalidJSON(t *testing.T) {
	tool := &ThinkTool{}

	result, err := tool.Run(context.Background(), `not json`)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.IsError {
		t.Error("expected error for invalid JSON")
	}
}

func TestThinkTool_Info(t *testing.T) {
	tool := &ThinkTool{}
	info := tool.Info()

	if info.Name != "think" {
		t.Errorf("expected name 'think', got %q", info.Name)
	}
	if tool.RequiresApproval() {
		t.Error("think tool should not require approval")
	}
}
