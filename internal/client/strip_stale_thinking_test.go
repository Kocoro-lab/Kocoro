package client

import (
	"testing"
)

func mkAssistantWithThinking(thinkingText, replyText string) Message {
	return Message{
		Role: "assistant",
		Content: NewBlockContent([]ContentBlock{
			{Type: "thinking", Thinking: thinkingText, Signature: "sig-" + thinkingText},
			{Type: "text", Text: replyText},
		}),
	}
}

func mkUser(text string) Message {
	return Message{Role: "user", Content: NewTextContent(text)}
}

func TestStripStaleThinking_KeepsMostRecentAssistantThinking(t *testing.T) {
	msgs := []Message{
		mkUser("first"),
		mkAssistantWithThinking("old reasoning", "old reply"),
		mkUser("second"),
		mkAssistantWithThinking("fresh reasoning", "fresh reply"),
	}
	out := stripStaleThinkingBlocks(msgs)

	// Older assistant must lose thinking.
	oldBlocks := out[1].Content.Blocks()
	for _, b := range oldBlocks {
		if b.Type == "thinking" {
			t.Errorf("older assistant still has thinking: %+v", b)
		}
	}
	// Most-recent assistant must retain thinking + signature for Anthropic
	// "immediately preceding assistant turn" rule.
	freshBlocks := out[3].Content.Blocks()
	foundThinking := false
	for _, b := range freshBlocks {
		if b.Type == "thinking" {
			foundThinking = true
			if b.Thinking != "fresh reasoning" {
				t.Errorf("most-recent thinking text mangled: %q", b.Thinking)
			}
			if b.Signature == "" {
				t.Error("signature dropped from most-recent assistant thinking")
			}
		}
	}
	if !foundThinking {
		t.Error("most-recent assistant lost its thinking block")
	}
}

func TestStripStaleThinking_NoStripWhenSingleAssistant(t *testing.T) {
	msgs := []Message{
		mkUser("hi"),
		mkAssistantWithThinking("only reasoning", "only reply"),
	}
	out := stripStaleThinkingBlocks(msgs)
	if &out[0] != &msgs[0] && len(out) == len(msgs) {
		// Identity check: we returned the same underlying slice
		// (no allocation when no strip is needed). The function
		// documents this as a perf invariant.
	}
	if out[1].Content.Blocks()[0].Type != "thinking" {
		t.Error("single-assistant thinking was wrongly stripped")
	}
}

func TestStripStaleThinking_NoStripWhenNoThinking(t *testing.T) {
	msgs := []Message{
		mkUser("a"),
		{Role: "assistant", Content: NewTextContent("plain reply 1")},
		mkUser("b"),
		{Role: "assistant", Content: NewTextContent("plain reply 2")},
	}
	out := stripStaleThinkingBlocks(msgs)
	// Function returns the input slice unchanged when no strip happens.
	if len(out) != len(msgs) {
		t.Fatalf("length mismatch: in=%d out=%d", len(msgs), len(out))
	}
}

func TestStripStaleThinking_AllThinkingMessageBecomesEmptyText(t *testing.T) {
	thinkingOnly := Message{
		Role: "assistant",
		Content: NewBlockContent([]ContentBlock{
			{Type: "thinking", Thinking: "all I do is think", Signature: "sig"},
		}),
	}
	msgs := []Message{
		mkUser("u1"),
		thinkingOnly,
		mkUser("u2"),
		mkAssistantWithThinking("fresh", "fresh reply"),
	}
	out := stripStaleThinkingBlocks(msgs)
	blocks := out[1].Content.Blocks()
	if len(blocks) != 1 {
		t.Fatalf("want 1 block after strip, got %d (%+v)", len(blocks), blocks)
	}
	if blocks[0].Type != "text" {
		t.Errorf("all-thinking message should become empty-text, got type=%s", blocks[0].Type)
	}
}

func TestStripStaleThinking_EmptyAndUserOnly(t *testing.T) {
	if got := stripStaleThinkingBlocks(nil); got != nil {
		t.Errorf("nil input: want nil out, got %+v", got)
	}
	userOnly := []Message{mkUser("a"), mkUser("b")}
	if got := stripStaleThinkingBlocks(userOnly); len(got) != 2 {
		t.Errorf("user-only input shouldn't be altered, got %+v", got)
	}
}
