package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type preambleTestTool struct {
	name     string
	source   ToolSource
	required []string
}

func (t preambleTestTool) Info() ToolInfo {
	properties := map[string]any{}
	for _, field := range t.required {
		properties[field] = map[string]any{"type": "string"}
	}
	return ToolInfo{
		Name:       t.name,
		Parameters: map[string]any{"type": "object", "properties": properties},
		Required:   t.required,
	}
}

func (preambleTestTool) Run(context.Context, string) (ToolResult, error) {
	return ToolResult{}, nil
}

func (preambleTestTool) RequiresApproval() bool { return false }
func (t preambleTestTool) ToolSource() ToolSource {
	return t.source
}

func TestFallbackPreambleFromToolCalls(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(preambleTestTool{name: "file_read", source: SourceLocal, required: []string{"description"}})
	registry.Register(preambleTestTool{name: "grep", source: SourceLocal, required: []string{"description"}})
	registry.Register(preambleTestTool{name: "legacy_tool", source: SourceLocal, required: []string{"purpose"}})
	registry.Register(preambleTestTool{name: "optional_description", source: SourceLocal})
	registry.Register(preambleTestTool{name: "create_issue", source: SourceMCP, required: []string{"description"}})

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
			name: "purpose is not an approval description",
			calls: []client.FunctionCall{{
				Name:      "legacy_tool",
				Arguments: json.RawMessage(`{"purpose":"Inspect the current state!"}`),
			}},
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
			name: "mixed language punctuation follows trailing script",
			calls: []client.FunctionCall{{
				Name:      "grep",
				Arguments: json.RawMessage(`{"description":"Read 中文 config file"}`),
			}},
			want: "Read 中文 config file.",
		},
		{
			name: "later call supplies description",
			calls: []client.FunctionCall{
				{
					Name:      "tool_search",
					Arguments: json.RawMessage(`{"query":"calendar"}`),
				},
				{
					Name:      "file_read",
					Arguments: json.RawMessage(`{"description":"Read the relevant file"}`),
				},
			},
			want: "Read the relevant file.",
		},
		{
			name: "external semantic description stays silent",
			calls: []client.FunctionCall{{
				Name:      "create_issue",
				Arguments: json.RawMessage(`{"description":"A long issue body controlled by a third-party schema"}`),
			}},
		},
		{
			name: "optional local description stays silent",
			calls: []client.FunctionCall{{
				Name:      "optional_description",
				Arguments: json.RawMessage(`{"description":"Semantic content, not an approval summary"}`),
			}},
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
			if got := fallbackPreambleFromToolCalls(tt.calls, registry); got != tt.want {
				t.Fatalf("fallbackPreambleFromToolCalls() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLooksLikeToolContinuationPreamble(t *testing.T) {
	tests := []struct {
		name string
		text string
		want bool
	}{
		{name: "review example", text: "Now let me read the file with the tool I just loaded.", want: true},
		{name: "search intent", text: "I'll now search the official documentation.", want: true},
		{name: "Chinese intent", text: "接下来我会读取官方页面。", want: true},
		{name: "Japanese intent", text: "次に公式ページを開く。", want: true},
		{name: "complete answer", text: "The official documentation says the feature is available.", want: false},
		{name: "explanation opener without tool action", text: "Now let me explain the result.", want: false},
		{name: "action substring in already", text: "Let me explain the already final result.", want: false},
		{name: "action substring in official", text: "I will now explain the official result.", want: false},
		{name: "answer with next-step paragraph", text: "The answer is 42.\n\nNext, let me check another source.", want: false},
		{name: "oversized narration", text: "Let me read " + strings.Repeat("x", 400), want: false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := looksLikeToolContinuationPreamble(tt.text); got != tt.want {
				t.Fatalf("looksLikeToolContinuationPreamble(%q) = %v, want %v", tt.text, got, tt.want)
			}
		})
	}
}

func TestFallbackPreambleFromToolCallsCapsLongDescription(t *testing.T) {
	registry := NewToolRegistry()
	registry.Register(preambleTestTool{name: "file_read", source: SourceLocal, required: []string{"description"}})
	description := strings.Repeat("x", 300)
	got := fallbackPreambleFromToolCalls([]client.FunctionCall{{
		Name:      "file_read",
		Arguments: json.RawMessage(`{"description":` + preambleJSON(description) + `}`),
	}}, registry)
	if count := utf8.RuneCountInString(got); count > 200 {
		t.Fatalf("fallback rune count = %d, want <= 200", count)
	}
	if !strings.HasSuffix(got, "…") {
		t.Fatalf("fallback = %q, want truncation marker", got)
	}
}

func preambleJSON(value string) string {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
