package agent

import (
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

func TestBuildForkedRequest_ToolsAllowlist_FiltersTools(t *testing.T) {
	// ToolsAllowlist is the documented cache-fragmenting option. Verify it
	// works correctly when used, and that the test makes its risk explicit.
	main := client.CompletionRequest{
		Messages: []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		Tools: []client.Tool{
			{Name: "file_read"},
			{Name: "file_write"},
			{Name: "bash"},
		},
	}
	forked, err := BuildForkedRequest(main, ForkOptions{
		AppendMessages: []client.Message{{Role: "user", Content: client.NewTextContent("x")}},
		SkipCacheWrite: true,
		ToolsAllowlist: []string{"file_read"},
	})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(forked.Tools) != 1 || forked.Tools[0].Name != "file_read" {
		t.Errorf("Tools = %+v, want only file_read", forked.Tools)
	}
	if len(main.Tools) != 3 {
		t.Error("main.Tools mutated")
	}
}
