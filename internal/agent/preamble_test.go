package agent

import (
	"encoding/json"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestFallbackPreambleFromToolCalls(t *testing.T) {
	tests := []struct {
		name  string
		calls []client.FunctionCall
		want  string
	}{
		{
			name: "description",
			calls: []client.FunctionCall{{
				Name:      "file_read",
				Arguments: json.RawMessage(`{"description":"Read the relevant files"}`),
			}},
			want: "Read the relevant files.",
		},
		{
			name: "purpose fallback",
			calls: []client.FunctionCall{{
				Name:      "legacy_tool",
				Arguments: json.RawMessage(`{"purpose":"Inspect the current state!"}`),
			}},
			want: "Inspect the current state!",
		},
		{
			name: "CJK punctuation",
			calls: []client.FunctionCall{{
				Name:      "grep",
				Arguments: json.RawMessage(`{"description":"检查相关实现"}`),
			}},
			want: "检查相关实现。",
		},
		{
			name: "infrastructure call stays silent",
			calls: []client.FunctionCall{{
				Name:      "tool_search",
				Arguments: json.RawMessage(`{"query":"calendar"}`),
			}},
		},
		{
			name: "malformed arguments stay silent",
			calls: []client.FunctionCall{{
				Name:      "file_read",
				Arguments: json.RawMessage(`not-json`),
			}},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := fallbackPreambleFromToolCalls(tt.calls); got != tt.want {
				t.Fatalf("fallbackPreambleFromToolCalls() = %q, want %q", got, tt.want)
			}
		})
	}
}
