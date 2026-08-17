package tools

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

func TestXPreparePostToolBuildsOfficialIntentWithoutPublishing(t *testing.T) {
	tests := []struct {
		name string
		text string
		want string
	}{
		{name: "ASCII", text: "Hello X", want: "https://x.com/intent/tweet?text=Hello+X"},
		{name: "Unicode", text: "你好、世界", want: "https://x.com/intent/tweet?text=%E4%BD%A0%E5%A5%BD%E3%80%81%E4%B8%96%E7%95%8C"},
		{name: "newline and reserved", text: "one\ntwo #x & why?", want: "https://x.com/intent/tweet?text=one%0Atwo+%23x+%26+why%3F"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, _ := json.Marshal(map[string]string{"text": tt.text})
			result, err := (&XPreparePostTool{}).Run(context.Background(), string(raw))
			if err != nil || result.IsError {
				t.Fatalf("Run = (%#v, %v)", result, err)
			}
			if !result.StopAgentLoop || !strings.Contains(result.TerminalUserMessage, tt.want) {
				t.Fatalf("terminal result = %#v", result)
			}
			var payload struct {
				Published bool   `json:"published"`
				Message   string `json:"message"`
				URL       string `json:"url"`
				Markdown  string `json:"markdown"`
			}
			if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
				t.Fatalf("invalid JSON result %q: %v", result.Content, err)
			}
			if payload.Published || !strings.Contains(payload.Message, "Nothing was posted") ||
				payload.URL != tt.want || payload.Markdown != "[Review and post on X]("+tt.want+")" {
				t.Fatalf("result = %s", result.Content)
			}
		})
	}
}

func TestXPreparePostToolRejectsInvalidTextWithoutTruncation(t *testing.T) {
	tool := &XPreparePostTool{}
	result, err := tool.Run(context.Background(), `{"text":""}`)
	if err != nil || !result.IsError || !strings.Contains(result.Content, "missing required parameter") {
		t.Fatalf("empty Run = (%#v, %v)", result, err)
	}
	result, err = tool.Run(context.Background(), `{"text":"  \n"}`)
	if err != nil || !result.IsError || !strings.Contains(result.Content, "missing required parameter") {
		t.Fatalf("whitespace Run = (%#v, %v)", result, err)
	}
	over := strings.Repeat("界", xPreparePostMaxRunes+1)
	raw, _ := json.Marshal(map[string]string{"text": over})
	result, err = tool.Run(context.Background(), string(raw))
	if err != nil || !result.IsError || !strings.Contains(result.Content, "was not truncated") {
		t.Fatalf("oversize Run = (%#v, %v)", result, err)
	}
}

func TestXPreparePostToolPolicyAndAuditAreContentFree(t *testing.T) {
	tool := &XPreparePostTool{}
	if tool.RequiresApproval() {
		t.Fatal("local URL construction must not require approval")
	}
	if !tool.IsReadOnlyCall(`{}`) || !tool.IsConcurrencySafeCall(`{}`) {
		t.Fatal("local URL construction must be read-only and concurrency-safe")
	}
	if !tool.StopsAgentLoop() {
		t.Fatal("prepared composer handoff must end the turn before another tool can run")
	}
	if got := agent.EffectiveToolExposure(tool); got != agent.ToolExposureDeferred {
		t.Fatalf("exposure = %q, want deferred", got)
	}
	if description := tool.Info().Description; !strings.Contains(description, "never publishes") ||
		!strings.Contains(description, "never use browser or computer automation") ||
		!strings.Contains(description, "Nothing was posted") {
		t.Fatalf("unsafe description: %q", description)
	}
	secretDraft := "private unreleased launch draft"
	in, out := tool.AuditSummaries(`{"text":"`+secretDraft+`"}`, "https://x.com/intent/tweet?text="+secretDraft)
	if strings.Contains(in, secretDraft) || strings.Contains(out, secretDraft) || strings.Contains(out, "intent/tweet") {
		t.Fatalf("audit summaries leaked draft or URL: %q / %q", in, out)
	}
}

func TestRegisterLocalToolsIncludesXPreparePost(t *testing.T) {
	registry, _, cleanup := RegisterLocalTools(nil, nil)
	defer cleanup()
	tool, ok := registry.Get("x_prepare_post")
	if !ok {
		t.Fatal("x_prepare_post was not registered")
	}
	if agent.EffectiveToolExposure(tool) != agent.ToolExposureDeferred {
		t.Fatal("registered x_prepare_post must remain deferred")
	}
}
