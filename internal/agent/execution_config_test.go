package agent

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestExecutionConfigSnapshotAndApplyAreDeepCopies(t *testing.T) {
	loop := NewAgentLoop(nil, NewToolRegistry(), "large", "", 13, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-5")
	loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096})
	loop.SetReasoningEffort("high")
	loop.SetEffortTier("xhigh")
	loop.SetServiceTier("fast")
	loop.SetResponseLanguage("日本語")
	loop.SetTemperature(0.27)
	loop.SetMaxTokens(7777)
	loop.SetContextWindowExplicit(200_000)

	want := ExecutionConfig{
		SpecificModel:         "claude-sonnet-5",
		ModelTier:             "large",
		Thinking:              &client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096},
		ReasoningEffort:       "high",
		EffortTier:            "xhigh",
		ServiceTier:           "fast",
		ResponseLanguage:      "日本語",
		Temperature:           0.27,
		MaxTokens:             7777,
		ContextWindow:         200_000,
		ContextWindowExplicit: true,
		MaxIterations:         13,
	}
	got := loop.ExecutionConfig()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("ExecutionConfig() = %+v, want %+v", got, want)
	}

	got.Thinking.BudgetTokens = 1
	if loop.Thinking().BudgetTokens != 4096 {
		t.Fatal("snapshot retained the loop-owned thinking pointer")
	}

	cloned := CloneExecutionConfig(&want)
	cloned.Thinking.BudgetTokens = 2
	if want.Thinking.BudgetTokens != 4096 {
		t.Fatal("CloneExecutionConfig retained the source thinking pointer")
	}

	target := NewAgentLoop(nil, NewToolRegistry(), "small", "", 1, 1000, 100, nil, nil, nil)
	target.ApplyExecutionConfig(want)
	want.Thinking.BudgetTokens = 3
	if target.Thinking().BudgetTokens != 4096 {
		t.Fatal("ApplyExecutionConfig retained the caller-owned thinking pointer")
	}
}

func TestExecutionConfigApplyPreservesZeroAndNil(t *testing.T) {
	loop := NewAgentLoop(nil, NewToolRegistry(), "large", "", 9, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-5")
	loop.SetThinking(&client.ThinkingConfig{Type: "adaptive"})
	loop.SetReasoningEffort("high")
	loop.SetEffortTier("xhigh")
	loop.SetServiceTier("fast")
	loop.SetResponseLanguage("中文")
	loop.SetTemperature(0.8)
	loop.SetMaxTokens(32_000)
	loop.SetContextWindowExplicit(200_000)

	loop.ApplyExecutionConfig(ExecutionConfig{})
	if got := loop.ExecutionConfig(); !reflect.DeepEqual(got, ExecutionConfig{}) {
		t.Fatalf("zero/nil config drifted: %+v", got)
	}
	if CloneExecutionConfig(nil) != nil {
		t.Fatal("nil legacy snapshot must remain nil")
	}
}

func TestExecutionConfigExcludesFastProfileAuthority(t *testing.T) {
	loop := NewAgentLoop(nil, NewToolRegistry(), "large", "", 9, 1000, 100, nil, nil, nil)
	loop.SetSpecificModel("claude-sonnet-5")
	loop.SetThinking(&client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096})
	loop.SetReasoningEffort("high")
	loop.SetEffortTier("xhigh")
	loop.SetServiceTier("fast")

	baseline := loop.ExecutionConfig()
	loop.SetKoeExecutionProfile(fastProfileForAgentTest())
	afterFast := loop.ExecutionConfig()
	if !reflect.DeepEqual(afterFast, baseline) {
		t.Fatalf("Fast profile polluted Agent baseline: before=%+v after=%+v", baseline, afterFast)
	}

	raw, err := json.Marshal(baseline)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{
		"gpt-5.6-terra",
		`"reasoning_effort":"none"`,
		"execution_profile",
		"profile_id",
		"provider",
		"tools",
		"prompt",
		"secret",
		"response_id",
	} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("baseline snapshot leaked %q: %s", forbidden, raw)
		}
	}
}
