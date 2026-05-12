package agent

import (
	"context"
	"errors"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// fakeLLM is a test double implementing client.LLMClient. It records the
// CompletionRequest it most recently received (so tests can assert on
// SkipCacheWrite, CacheSource, etc.) and returns a canned response or error.
type fakeLLM struct {
	resp        string
	gotReq      client.CompletionRequest
	completeErr error
}

func (f *fakeLLM) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	f.gotReq = req
	if f.completeErr != nil {
		return nil, f.completeErr
	}
	return &client.CompletionResponse{
		Provider:   "anthropic",
		Model:      "claude-sonnet-4-6",
		OutputText: f.resp,
		Usage:      client.Usage{InputTokens: 100, CacheReadTokens: 95},
	}, nil
}

func (f *fakeLLM) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return f.Complete(ctx, req)
}

func TestGenerateSuggestion_HappyPath(t *testing.T) {
	main := client.CompletionRequest{
		Messages:    []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier:   "medium",
		CacheSource: "channel",
	}
	llm := &fakeLLM{resp: "rerun the failing test"}

	got, err := GenerateSuggestion(context.Background(), llm, main)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "rerun the failing test" {
		t.Errorf("got %q, want %q", got, "rerun the failing test")
	}

	// Verify cache-safety contract on the request sent
	if !llm.gotReq.SkipCacheWrite {
		t.Error("request sent without SkipCacheWrite=true")
	}
	if llm.gotReq.CacheSource != "channel" {
		t.Errorf("CacheSource = %q, want channel (must match main)", llm.gotReq.CacheSource)
	}
	if len(llm.gotReq.Messages) != 2 {
		t.Errorf("messages len = %d, want 2 (main + SUGGESTION)", len(llm.gotReq.Messages))
	}
	if llm.gotReq.ForkedKind != "suggestion" {
		t.Errorf("ForkedKind = %q, want %q", llm.gotReq.ForkedKind, "suggestion")
	}
}

func TestGenerateSuggestion_FilterRejection(t *testing.T) {
	main := client.CompletionRequest{
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier: "medium",
	}
	llm := &fakeLLM{resp: "skip"} // FilterSuggestion rejects the literal meta-marker "skip"

	got, err := GenerateSuggestion(context.Background(), llm, main)
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty string from filtered response, got %q", got)
	}
}

func TestGenerateSuggestion_GatewayError(t *testing.T) {
	main := client.CompletionRequest{
		Messages:  []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier: "medium",
	}
	llm := &fakeLLM{completeErr: errors.New("rate limited")}

	got, err := GenerateSuggestion(context.Background(), llm, main)
	if err == nil {
		t.Error("expected error from gateway failure")
	}
	if got != "" {
		t.Errorf("expected empty string on error, got %q", got)
	}
}

func TestFilterSuggestion(t *testing.T) {
	cases := []struct {
		name     string
		in       string
		wantOK   bool
		wantText string
	}{
		{"valid short", "fix the test", true, "fix the test"},
		{"valid 12 words exact",
			"please run the test suite and commit the fix once it passes ok",
			true, "please run the test suite and commit the fix once it passes ok"},
		{"too short 1 word", "yes", false, ""},
		{"single word in allowlist", "commit", true, "commit"},
		{"too long 13 words",
			"please run the test suite and commit the fix once it passes thanks much",
			false, ""},
		{"too long char count",
			"please run the entire test suite which is huge and then commit changes",
			false, ""},
		{"multi-sentence", "fix it. then commit.", false, ""},
		{"evaluative word great", "great idea, run tests", false, ""},
		{"claude voice", "I'll fix the test", false, ""},
		{"format chars",
			"fix the test\n\nthen commit", false, ""},
		{"meta wrap done", "done", false, ""},
		{"empty", "", false, ""},
		{"whitespace only", "   ", false, ""},
		// CJK cases — must accept short Chinese/Japanese suggestions that
		// strings.Fields would otherwise count as 1 word
		{"chinese valid", "运行测试套件", true, "运行测试套件"},
		{"chinese longer valid", "帮我重构这个 loop 函数", true, "帮我重构这个 loop 函数"},
		{"chinese too short 3 runes", "去吧", false, ""},
		{"chinese too long >30 runes",
			"请帮我把整个项目的所有 Go 文件全部按照 gofmt 格式化一遍然后做测试覆盖率分析", false, ""},
		{"chinese meta skip", "跳过", false, ""},
		{"japanese valid", "次のステップに進む", true, "次のステップに進む"},
		{"japanese meta skip", "スキップ", false, ""},
		{"chinese multi-sentence", "运行测试。然后提交。", false, ""},
		{"mixed cjk english latin-dominant",
			"run gofmt now", true, "run gofmt now"},
		{"mixed cjk english cjk-dominant",
			"运行 gofmt 然后看结果", true, "运行 gofmt 然后看结果"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := FilterSuggestion(tc.in)
			if ok != tc.wantOK {
				t.Errorf("ok = %v, want %v (input %q, got %q)", ok, tc.wantOK, tc.in, got)
			}
			if ok && got != tc.wantText {
				t.Errorf("text = %q, want %q", got, tc.wantText)
			}
		})
	}
}

func TestSuggestionPromptIsStable(t *testing.T) {
	// SUGGESTION_PROMPT must not change between releases — changes invalidate
	// the cache benefit since the suggestion message itself becomes part of
	// the forked request's tail. (The cache benefit comes from the *prefix*
	// being identical to main; the appended message is uncached.)
	// This test exists to make accidental edits visible in code review.
	if len(SuggestionPrompt) < 50 {
		t.Errorf("SuggestionPrompt suspiciously short: %d chars", len(SuggestionPrompt))
	}
	if len(SuggestionPrompt) > 2000 {
		t.Errorf("SuggestionPrompt suspiciously long (%d chars) — every char is paid input on each turn",
			len(SuggestionPrompt))
	}
}

func TestShouldGenerateSuggestion(t *testing.T) {
	cases := []struct {
		name string
		args ShouldGenerateArgs
		want bool
	}{
		{"happy path",
			ShouldGenerateArgs{
				Enabled:                  true,
				CompletedTurns:           3,
				MinTurns:                 2,
				LastTurnUncachedTokens:   500,
				CacheColdThresholdTokens: 10000,
				LastTurnHadError:         false,
				PlanMode:                 false,
			}, true},
		{"feature disabled",
			ShouldGenerateArgs{Enabled: false, CompletedTurns: 3, MinTurns: 2}, false},
		{"too few turns",
			ShouldGenerateArgs{Enabled: true, CompletedTurns: 1, MinTurns: 2}, false},
		{"exact MinTurns",
			ShouldGenerateArgs{Enabled: true, CompletedTurns: 2, MinTurns: 2,
				LastTurnUncachedTokens: 100, CacheColdThresholdTokens: 10000}, true},
		{"cache cold above threshold",
			ShouldGenerateArgs{Enabled: true, CompletedTurns: 3, MinTurns: 2,
				LastTurnUncachedTokens: 20000, CacheColdThresholdTokens: 10000}, false},
		{"cache cold disabled (threshold=0)",
			ShouldGenerateArgs{Enabled: true, CompletedTurns: 3, MinTurns: 2,
				LastTurnUncachedTokens: 99999, CacheColdThresholdTokens: 0}, true},
		{"last turn errored",
			ShouldGenerateArgs{Enabled: true, CompletedTurns: 3, MinTurns: 2,
				LastTurnHadError: true}, false},
		{"plan mode",
			ShouldGenerateArgs{Enabled: true, CompletedTurns: 3, MinTurns: 2,
				PlanMode: true}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ShouldGenerateSuggestion(tc.args)
			if got != tc.want {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}
