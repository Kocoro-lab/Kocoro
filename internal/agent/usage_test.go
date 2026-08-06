package agent

import (
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestLLMUsageDelta_NormalizesSplitCacheCreation(t *testing.T) {
	delta := LLMUsageDelta(client.Usage{
		InputTokens:           120,
		OutputTokens:          30,
		WebSearchCalls:        2,
		CacheReadTokens:       40,
		CacheCreation5mTokens: 100,
		CacheCreation1hTokens: 200,
	}, "claude-test")

	if delta.TotalTokens != 150 {
		t.Fatalf("expected total tokens 150, got %d", delta.TotalTokens)
	}
	if delta.CacheCreationTokens != 300 {
		t.Fatalf("expected legacy cache creation total 300, got %d", delta.CacheCreationTokens)
	}
	if delta.CacheCreation5mTokens != 100 || delta.CacheCreation1hTokens != 200 {
		t.Fatalf("expected split cache creation 100/200, got %d/%d", delta.CacheCreation5mTokens, delta.CacheCreation1hTokens)
	}
	if delta.Model != "claude-test" {
		t.Fatalf("expected model claude-test, got %q", delta.Model)
	}
	if delta.LLMCalls != 1 {
		t.Fatalf("expected 1 LLM call, got %d", delta.LLMCalls)
	}
	if delta.WebSearchCalls != 2 {
		t.Fatalf("expected 2 web search calls, got %d", delta.WebSearchCalls)
	}
}

func TestUsageAccumulator_AccumulatesSplitCacheCreation(t *testing.T) {
	var acc UsageAccumulator
	acc.Add(LLMUsageDelta(client.Usage{
		InputTokens:           90,
		OutputTokens:          10,
		WebSearchCalls:        1,
		CacheCreation5mTokens: 25,
		CacheCreation1hTokens: 75,
	}, "claude-test"))

	snap := acc.Snapshot()
	if snap.LLM.CacheCreationTokens != 100 {
		t.Fatalf("expected legacy cache creation total 100, got %d", snap.LLM.CacheCreationTokens)
	}
	if snap.LLM.CacheCreation5mTokens != 25 || snap.LLM.CacheCreation1hTokens != 75 {
		t.Fatalf("expected split cache creation 25/75, got %d/%d", snap.LLM.CacheCreation5mTokens, snap.LLM.CacheCreation1hTokens)
	}
	if snap.LLM.TotalTokens != 100 {
		t.Fatalf("expected total tokens 100, got %d", snap.LLM.TotalTokens)
	}
	if snap.LLM.WebSearchCalls != 1 {
		t.Fatalf("expected 1 web search call, got %d", snap.LLM.WebSearchCalls)
	}
}

func TestTotalPromptTokens(t *testing.T) {
	tests := []struct {
		name string
		u    client.Usage
		want int
	}{
		{
			name: "only non-cached input",
			u:    client.Usage{InputTokens: 1200},
			want: 1200,
		},
		{
			name: "warm cache: small input, large cache read",
			u:    client.Usage{InputTokens: 500, CacheReadTokens: 90000},
			want: 90500,
		},
		{
			name: "cache miss filled a new cache: small input, large cache creation",
			u:    client.Usage{InputTokens: 300, CacheCreationTokens: 45000},
			want: 45300,
		},
		{
			name: "mixed: all three populated",
			u:    client.Usage{InputTokens: 800, CacheReadTokens: 60000, CacheCreationTokens: 5000},
			want: 65800,
		},
		{
			name: "zero usage",
			u:    client.Usage{},
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := totalPromptTokens(tt.u)
			if got != tt.want {
				t.Errorf("totalPromptTokens(%+v) = %d, want %d", tt.u, got, tt.want)
			}
		})
	}
}
