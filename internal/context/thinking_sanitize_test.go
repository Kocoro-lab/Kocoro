package context

import (
	"reflect"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func block(t string, fields map[string]string) client.ContentBlock {
	b := client.ContentBlock{Type: t}
	if v, ok := fields["text"]; ok {
		b.Text = v
	}
	if v, ok := fields["thinking"]; ok {
		b.Thinking = v
	}
	if v, ok := fields["signature"]; ok {
		b.Signature = v
	}
	if v, ok := fields["data"]; ok {
		b.Data = v
	}
	if v, ok := fields["id"]; ok {
		b.ID = v
	}
	if v, ok := fields["name"]; ok {
		b.Name = v
	}
	return b
}

func assistantMsg(blocks ...client.ContentBlock) client.Message {
	return client.Message{Role: "assistant", Content: client.NewBlockContent(blocks)}
}

func userMsg(blocks ...client.ContentBlock) client.Message {
	return client.Message{Role: "user", Content: client.NewBlockContent(blocks)}
}

func TestDropMalformedThinking_DropsEmptyThinkingPreservesSibling(t *testing.T) {
	in := []client.Message{
		userMsg(block("text", map[string]string{"text": "hi"})),
		assistantMsg(
			block("thinking", map[string]string{"signature": "sig-only"}),
			block("text", map[string]string{"text": "hello"}),
		),
	}
	out := DropMalformedThinking(in)
	if len(out) != 2 {
		t.Fatalf("want 2 messages, got %d", len(out))
	}
	got := out[1].Content.Blocks()
	if len(got) != 1 {
		t.Fatalf("assistant should retain 1 block, got %d: %#v", len(got), got)
	}
	if got[0].Type != "text" || got[0].Text != "hello" {
		t.Fatalf("surviving block wrong: %#v", got[0])
	}
}

func TestDropMalformedThinking_DropsEmptyRedactedThinking(t *testing.T) {
	in := []client.Message{
		assistantMsg(
			block("redacted_thinking", nil),
			block("text", map[string]string{"text": "ok"}),
		),
	}
	out := DropMalformedThinking(in)
	got := out[0].Content.Blocks()
	if len(got) != 1 || got[0].Type != "text" {
		t.Fatalf("redacted_thinking with empty Data should be dropped: %#v", got)
	}
}

func TestDropMalformedThinking_PreservesValidThinking(t *testing.T) {
	in := []client.Message{
		assistantMsg(
			block("thinking", map[string]string{"thinking": "I should call grep", "signature": "sig123"}),
			block("text", map[string]string{"text": "Let me check."}),
		),
	}
	in2 := []client.Message{
		assistantMsg(
			block("redacted_thinking", map[string]string{"data": "ENCRYPTED_BLOB"}),
		),
	}
	out := DropMalformedThinking(in)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("valid thinking should be unchanged")
	}
	out2 := DropMalformedThinking(in2)
	if !reflect.DeepEqual(out2, in2) {
		t.Fatalf("valid redacted_thinking should be unchanged")
	}
}

func TestDropMalformedThinking_AllBlocksDropped_InsertsPlaceholder(t *testing.T) {
	in := []client.Message{
		assistantMsg(
			block("thinking", map[string]string{"signature": "sig1"}),
			block("redacted_thinking", nil),
		),
	}
	out := DropMalformedThinking(in)
	got := out[0].Content.Blocks()
	if len(got) != 1 {
		t.Fatalf("expected 1 placeholder block, got %d", len(got))
	}
	if got[0].Type != "text" || got[0].Text != "" {
		t.Fatalf("placeholder should be empty text block, got %#v", got[0])
	}
}

func TestDropMalformedThinking_DropsAcrossAllRoles(t *testing.T) {
	in := []client.Message{
		assistantMsg(block("thinking", map[string]string{"signature": "sig"})),
		userMsg(block("text", map[string]string{"text": "u1"})),
		assistantMsg(block("thinking", map[string]string{"signature": "sig"})),
	}
	out := DropMalformedThinking(in)
	if len(out) != 3 {
		t.Fatalf("message count must not change, got %d", len(out))
	}
	if out[0].Content.Blocks()[0].Type != "text" {
		t.Fatalf("msg[0] expected placeholder text, got %#v", out[0].Content.Blocks())
	}
	if out[2].Content.Blocks()[0].Type != "text" {
		t.Fatalf("msg[2] expected placeholder text, got %#v", out[2].Content.Blocks())
	}
}

func TestDropMalformedThinking_NilAndEmptySafe(t *testing.T) {
	if got := DropMalformedThinking(nil); got != nil {
		t.Fatalf("nil input should return nil, got %#v", got)
	}
	empty := []client.Message{}
	if got := DropMalformedThinking(empty); len(got) != 0 {
		t.Fatalf("empty input should return empty, got %#v", got)
	}
}

func TestDropMalformedThinking_StringContentUntouched(t *testing.T) {
	in := []client.Message{
		{Role: "user", Content: client.NewTextContent("plain text")},
		{Role: "assistant", Content: client.NewTextContent("plain reply")},
	}
	out := DropMalformedThinking(in)
	if !reflect.DeepEqual(out, in) {
		t.Fatalf("string-content messages should pass through, got %#v", out)
	}
}

func TestDropMalformedThinking_FastPathNoAlloc(t *testing.T) {
	// Clean history: helper must return the input slice header unchanged
	// (no allocation). Verified by pointer-equality on the slice's backing
	// array via &in[0] vs &out[0].
	in := []client.Message{
		assistantMsg(
			block("thinking", map[string]string{"thinking": "reasoning", "signature": "sig"}),
			block("text", map[string]string{"text": "answer"}),
		),
	}
	out := DropMalformedThinking(in)
	if &in[0] != &out[0] {
		t.Fatalf("clean-history fast path should return input slice unchanged")
	}
}

func TestDropMalformedThinking_Idempotent(t *testing.T) {
	in := []client.Message{
		assistantMsg(
			block("thinking", map[string]string{"signature": "sig"}),
			block("text", map[string]string{"text": "ok"}),
		),
	}
	first := DropMalformedThinking(in)
	snapshot := make([]client.ContentBlock, len(first[0].Content.Blocks()))
	copy(snapshot, first[0].Content.Blocks())
	second := DropMalformedThinking(first)
	if !reflect.DeepEqual(snapshot, second[0].Content.Blocks()) {
		t.Fatalf("second pass mutated already-clean history")
	}
}

// TestDropMalformedThinking_WireRejectShape pins the exact precondition that
// motivated this helper: serializing a thinking block with empty Thinking text
// yields a JSON object missing the "thinking" key. If a future refactor
// removes `,omitempty` from the field tag, this test breaks and signals that
// the helper's whole reason-for-being needs revisiting.
func TestDropMalformedThinking_WireRejectShape(t *testing.T) {
	b := client.ContentBlock{Type: "thinking", Signature: "sig"}
	out, err := b.MarshalJSON()
	if err != nil {
		t.Fatalf("marshal failed: %v", err)
	}
	got := string(out)
	if got != `{"signature":"sig","type":"thinking"}` {
		t.Fatalf("wire shape changed; reconfirm helper is still required.\nwant: %s\ngot:  %s",
			`{"signature":"sig","type":"thinking"}`, got)
	}
}
