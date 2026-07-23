package agent

import (
	"strings"
	"testing"
)

func TestValidateToolArguments(t *testing.T) {
	info := ToolInfo{
		Name:     "example",
		Required: []string{"query", "limit", "enabled", "items"},
	}

	tests := []struct {
		name      string
		args      string
		wantValid bool
		wantText  string
	}{
		{name: "valid", args: `{"query":"go","limit":2,"enabled":true,"items":["x"]}`, wantValid: true},
		{name: "empty input", args: "", wantText: "query, limit, enabled, items"},
		{name: "invalid json", args: `{"query":`, wantText: "invalid arguments"},
		{name: "trailing json", args: `{} {}`, wantText: "exactly one JSON value"},
		{name: "non object", args: `[]`, wantText: "JSON object"},
		{name: "whitespace string", args: `{"query":" ","limit":2,"enabled":true,"items":["x"]}`, wantText: "query"},
		{name: "zero number", args: `{"query":"go","limit":0,"enabled":true,"items":["x"]}`, wantText: "limit"},
		{name: "false bool", args: `{"query":"go","limit":2,"enabled":false,"items":["x"]}`, wantText: "enabled"},
		{name: "empty collection", args: `{"query":"go","limit":2,"enabled":true,"items":[]}`, wantText: "items"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, valid := ValidateToolArguments(info, tt.args)
			if valid != tt.wantValid {
				t.Fatalf("valid = %v, want %v; result=%q", valid, tt.wantValid, result.Content)
			}
			if tt.wantValid {
				if result.IsError || result.Content != "" {
					t.Fatalf("valid input returned result %#v", result)
				}
				return
			}
			if !result.IsError || result.ErrorCategory != ErrCategoryValidation {
				t.Fatalf("invalid input returned non-validation result %#v", result)
			}
			if !strings.HasPrefix(result.Content, "[validation error]") || !strings.Contains(result.Content, tt.wantText) {
				t.Fatalf("result %q does not contain %q", result.Content, tt.wantText)
			}
		})
	}
}
