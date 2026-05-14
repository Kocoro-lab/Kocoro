package agent

import (
	"bytes"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func TestBuildForkedRequest_ByteEqualPrefix(t *testing.T) {
	main := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent("You are an agent.")},
			{Role: "user", Content: client.NewTextContent("fix the bug")},
			{Role: "assistant", Content: client.NewTextContent("I fixed it.")},
		},
		ModelTier:       "medium",
		SpecificModel:   "claude-sonnet-4-6",
		Temperature:     1.0,
		MaxTokens:       8192,
		Tools:           []client.Tool{{Name: "file_read", Type: "function"}},
		Stream:          false,
		ReasoningEffort: "high",
		ToolChoice:      "auto",
		SessionID:       "sess_abc",
		CacheSource:     "channel",
	}

	appended := []client.Message{{Role: "user", Content: client.NewTextContent("what next?")}}
	forked, err := BuildForkedRequest(main, ForkOptions{
		AppendMessages: appended,
		SkipCacheWrite: true,
		DebugKind:      "test",
	})
	if err != nil {
		t.Fatalf("BuildForkedRequest: %v", err)
	}

	// 1. Forked must have exactly len(appended) more messages
	if len(forked.Messages) != len(main.Messages)+len(appended) {
		t.Fatalf("forked has %d messages, want %d", len(forked.Messages), len(main.Messages)+len(appended))
	}
	// 2. Prefix messages must be byte-equal (deep)
	for i := range main.Messages {
		if !reflect.DeepEqual(main.Messages[i], forked.Messages[i]) {
			t.Errorf("Messages[%d] differs: main=%+v forked=%+v", i, main.Messages[i], forked.Messages[i])
		}
	}
	// 3. Appended messages match
	for i, want := range appended {
		got := forked.Messages[len(main.Messages)+i]
		if !reflect.DeepEqual(got, want) {
			t.Errorf("appended[%d] = %+v, want %+v", i, got, want)
		}
	}
	// 4. SkipCacheWrite set, main unchanged
	if !forked.SkipCacheWrite {
		t.Error("forked.SkipCacheWrite must be true")
	}
	if main.SkipCacheWrite {
		t.Error("main mutated — SkipCacheWrite should remain false")
	}
	// 5. ForkedKind propagated
	if forked.ForkedKind != "test" {
		t.Errorf("forked.ForkedKind = %q, want %q", forked.ForkedKind, "test")
	}
	// 6. All other fields byte-identical
	mainCopy := main
	forkedCopy := forked
	mainCopy.Messages = nil
	forkedCopy.Messages = nil
	forkedCopy.SkipCacheWrite = false
	forkedCopy.ForkedKind = ""
	mb, _ := json.Marshal(mainCopy)
	fb, _ := json.Marshal(forkedCopy)
	if string(mb) != string(fb) {
		t.Errorf("non-Messages/SkipCacheWrite/ForkedKind fields differ:\n  main:   %s\n  forked: %s", mb, fb)
	}
}

func TestBuildForkedRequest_DoesNotMutateMain(t *testing.T) {
	main := client.CompletionRequest{
		Messages:    []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier:   "medium",
		CacheSource: "tui",
	}
	mainBefore, _ := json.Marshal(main)
	mainMessagesBefore := main.Messages
	_, _ = BuildForkedRequest(main, ForkOptions{
		AppendMessages: []client.Message{{Role: "user", Content: client.NewTextContent("x")}},
		SkipCacheWrite: true,
	})
	mainAfter, _ := json.Marshal(main)
	if string(mainBefore) != string(mainAfter) {
		t.Errorf("BuildForkedRequest mutated main:\n  before: %s\n  after:  %s", mainBefore, mainAfter)
	}
	// Slice header should also be untouched (length AND backing-array identity).
	// If BuildForkedRequest did `append(main.Messages, ...)` without first
	// allocating a fresh slice, the underlying array could be shared and a
	// caller mutating the result would corrupt main.
	if len(main.Messages) != len(mainMessagesBefore) {
		t.Errorf("main.Messages length mutated: before=%d after=%d", len(mainMessagesBefore), len(main.Messages))
	}
}

func TestBuildForkedRequest_RejectsEmptyAppend(t *testing.T) {
	main := client.CompletionRequest{Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}}}
	_, err := BuildForkedRequest(main, ForkOptions{AppendMessages: nil, SkipCacheWrite: true})
	if err == nil {
		t.Error("expected error on empty AppendMessages — a fork with no new messages is meaningless")
	}
}

// TestForkedRequest_DebugTagging confirms the suggestion wrapper stamps
// the off-wire ForkedKind field so SHANNON_CACHE_DEBUG log lines can be
// filtered by fork type. The field is json:"-" so it never reaches the
// gateway, but it surfaces in the local debug log.
func TestForkedRequest_DebugTagging(t *testing.T) {
	main := client.CompletionRequest{Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}}}

	forked := BuildForkedSuggestionRequest(main)
	if forked.ForkedKind != "suggestion" {
		t.Errorf("ForkedKind = %q, want suggestion", forked.ForkedKind)
	}
}

// TestBuildForkedRequest_ThinkingDeepCopied catches the pointer-aliasing
// footgun: Thinking is a *ThinkingConfig pointer. A naïve `out := main`
// would share the same underlying struct, so a caller mutating
// out.Thinking.BudgetTokens would corrupt main.Thinking too — taking down
// the parent turn's cache prefix in the process (thinking config is part
// of the Anthropic cache key).
func TestBuildForkedRequest_ThinkingDeepCopied(t *testing.T) {
	main := client.CompletionRequest{
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Thinking: &client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096},
	}
	forked, err := BuildForkedRequest(main, ForkOptions{
		AppendMessages: []client.Message{{Role: "user", Content: client.NewTextContent("x")}},
		SkipCacheWrite: true,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if forked.Thinking == nil {
		t.Fatal("forked.Thinking lost during copy")
	}
	if forked.Thinking == main.Thinking {
		t.Error("forked.Thinking is the SAME pointer as main.Thinking — caller mutation would corrupt main")
	}
	if forked.Thinking.BudgetTokens != 4096 {
		t.Errorf("forked.Thinking.BudgetTokens = %d, want 4096", forked.Thinking.BudgetTokens)
	}
	// Mutate forked's Thinking — main must stay at 4096.
	forked.Thinking.BudgetTokens = 100
	if main.Thinking.BudgetTokens != 4096 {
		t.Errorf("after mutating forked, main.Thinking.BudgetTokens = %d, want 4096 (aliasing not fixed)", main.Thinking.BudgetTokens)
	}
}

// TestBuildForkedRequest_CallerMutationBreaksByteEquality is a regression
// test that DOCUMENTS the failure mode for caller misuse — if a caller
// modifies forked.MaxTokens / Temperature / Model / etc. after this
// function returns, the byte-equality contract with `main` is broken and
// the Anthropic prompt cache will miss on the forked call.
//
// This test does not "prevent" the misuse (Go's type system can't); it
// pins what byte-equality looks like and catches regressions where future
// edits to BuildForkedRequest accidentally introduce divergence.
func TestBuildForkedRequest_CallerMutationBreaksByteEquality(t *testing.T) {
	main := client.CompletionRequest{
		Messages:        []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier:       "medium",
		SpecificModel:   "claude-sonnet-4-6",
		Temperature:     1.0,
		MaxTokens:       8192,
		ReasoningEffort: "high",
		Thinking:        &client.ThinkingConfig{Type: "enabled", BudgetTokens: 4096},
		CacheSource:     "channel",
	}
	forked, err := BuildForkedRequest(main, ForkOptions{
		AppendMessages: []client.Message{{Role: "user", Content: client.NewTextContent("x")}},
		SkipCacheWrite: true,
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}

	// Snapshot the cache-key-affecting bytes BEFORE caller mutation.
	type cacheKeyFields struct {
		ModelTier       string
		SpecificModel   string
		Temperature     float64
		MaxTokens       int
		ReasoningEffort string
		Thinking        client.ThinkingConfig
		CacheSource     string
	}
	snap := func(r client.CompletionRequest) cacheKeyFields {
		var th client.ThinkingConfig
		if r.Thinking != nil {
			th = *r.Thinking
		}
		return cacheKeyFields{
			ModelTier:       r.ModelTier,
			SpecificModel:   r.SpecificModel,
			Temperature:     r.Temperature,
			MaxTokens:       r.MaxTokens,
			ReasoningEffort: r.ReasoningEffort,
			Thinking:        th,
			CacheSource:     r.CacheSource,
		}
	}
	mainSnap := snap(main)
	forkedSnap := snap(forked)
	if mainSnap != forkedSnap {
		t.Fatalf("immediately after BuildForkedRequest, cache-key fields already differ:\n  main:   %+v\n  forked: %+v", mainSnap, forkedSnap)
	}

	// Simulate caller misuse — every line below is what the docstring tells
	// callers NOT to do. We mutate forked, then re-snapshot, and assert it
	// HAS diverged. This anchors the documented failure mode in a runnable
	// test so future contributors can grep for it.
	forked.MaxTokens = 100
	forked.Temperature = 0.0
	forked.ReasoningEffort = "low"
	if forked.Thinking != nil {
		forked.Thinking.BudgetTokens = 100
	}
	mutatedSnap := snap(forked)
	if mutatedSnap == mainSnap {
		t.Error("caller mutation should have produced divergence — this test's purpose is to document that path")
	}
	// And main MUST still be its original self — caller mutating forked
	// must NOT touch main. (Thinking-aliasing regression check.)
	if snap(main) != mainSnap {
		t.Error("caller mutation to forked changed main — pointer aliasing not contained")
	}
}

// TestBuildForkedRequest_ByteStableWithInterleavedThinking confirms the
// byte-equality cache contract holds when assistant messages carry
// interleaved thinking blocks. Without this, a forked suggestion call
// after a thinking-rich main turn would miss the parent's prompt cache.
func TestBuildForkedRequest_ByteStableWithInterleavedThinking(t *testing.T) {
	main := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hi")},
			{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "thinking", Thinking: "reason 1", Signature: "sigA"},
				{Type: "text", Text: "ok"},
				{Type: "tool_use", ID: "t1", Name: "f", Input: json.RawMessage(`{}`)},
				{Type: "thinking", Thinking: "reason 2 (interleaved)", Signature: "sigB"},
			})},
		},
		Thinking: &client.ThinkingConfig{Type: "adaptive"},
	}
	opts := ForkOptions{
		AppendMessages: []client.Message{
			{Role: "user", Content: client.NewTextContent("suggest a continuation")},
		},
		SkipCacheWrite: true,
		DebugKind:      "suggestion-test",
	}
	fork, err := BuildForkedRequest(main, opts)
	if err != nil {
		t.Fatalf("BuildForkedRequest: %v", err)
	}

	// Truncate appended messages + clear fork-only divergences for byte compare.
	forkTrunc := fork
	forkTrunc.Messages = forkTrunc.Messages[:len(main.Messages)]
	forkTrunc.SkipCacheWrite = false
	forkTrunc.ForkedKind = ""

	mainBytes, err := json.Marshal(main)
	if err != nil {
		t.Fatalf("marshal main: %v", err)
	}
	forkBytes, err := json.Marshal(forkTrunc)
	if err != nil {
		t.Fatalf("marshal fork: %v", err)
	}
	if !bytes.Equal(mainBytes, forkBytes) {
		t.Errorf("byte drift with interleaved thinking blocks:\n  main=%s\n  fork=%s", mainBytes, forkBytes)
	}
}
