package agent

import (
	"strings"
	"testing"
)

func TestAgentLoop_SetInjectCh(t *testing.T) {
	reg := NewToolRegistry()
	loop := NewAgentLoop(nil, reg, "test", t.TempDir(), 5, 2000, 200, nil, nil, nil)
	ch := make(chan InjectedMessage, 10)
	loop.SetInjectCh(ch)
	if loop.injectCh != ch {
		t.Fatal("expected injectCh to be set")
	}
}

func TestAgentLoop_InjectCh_Nil_NoPanic(t *testing.T) {
	reg := NewToolRegistry()
	loop := NewAgentLoop(nil, reg, "test", t.TempDir(), 1, 2000, 200, nil, nil, nil)
	// injectCh is nil by default — should not panic
	if loop.injectCh != nil {
		t.Fatal("expected injectCh to be nil by default")
	}
}

func TestAgentLoop_MultipleInjections_Batched(t *testing.T) {
	ch := make(chan InjectedMessage, 10)
	ch <- InjectedMessage{Text: "message one"}
	ch <- InjectedMessage{Text: "message two"}
	ch <- InjectedMessage{Text: "message three"}

	// Drain like the loop does
	var injected []string
drain:
	for {
		select {
		case msg := <-ch:
			injected = append(injected, msg.Text)
		default:
			break drain
		}
	}
	if len(injected) != 3 {
		t.Fatalf("expected 3 messages, got %d", len(injected))
	}
	if injected[0] != "message one" || injected[2] != "message three" {
		t.Errorf("unexpected order: %v", injected)
	}
}

func TestBuildInjectedUserMessage_TextOnly(t *testing.T) {
	msgs := []InjectedMessage{
		{Text: "first"},
		{Text: "second"},
	}
	msg, ok := buildInjectedUserMessage(msgs)
	if !ok {
		t.Fatal("expected message produced")
	}
	if msg.Role != "user" {
		t.Errorf("role: got %q want user", msg.Role)
	}
	// Text-only path must preserve the existing "[New message from user]\n" prefix
	// for byte-identical behavior with the pre-refactor code.
	blocks := msg.Content.Blocks()
	if blocks != nil {
		t.Fatalf("text-only path should use NewTextContent, not multi-block; got %d blocks", len(blocks))
	}
	if got := msg.Content.Text(); !strings.Contains(got, "first") || !strings.Contains(got, "second") {
		t.Errorf("text missing inject content: %q", got)
	}
	if !strings.HasPrefix(msg.Content.Text(), "[New message from user]\n") {
		t.Errorf("text-only path lost the legacy prefix: %q", msg.Content.Text())
	}
}

func TestBuildInjectedUserMessage_WithFiles(t *testing.T) {
	msgs := []InjectedMessage{
		{
			Text: "describe this",
			Files: []InjectedFile{
				{Type: "image", MediaType: "image/png", Data: "iVBORw0KGgo="},
			},
		},
	}
	msg, ok := buildInjectedUserMessage(msgs)
	if !ok {
		t.Fatal("expected message produced")
	}
	blocks := msg.Content.Blocks()
	if len(blocks) != 2 {
		t.Fatalf("expected 2 blocks (text + image), got %d", len(blocks))
	}
	if blocks[0].Type != "text" || !strings.Contains(blocks[0].Text, "describe this") {
		t.Errorf("first block wrong: %+v", blocks[0])
	}
	if blocks[1].Type != "image" {
		t.Errorf("second block wrong: type=%q", blocks[1].Type)
	}
	// Image block must use base64 ImageSource (the only Type our gateway accepts).
	if blocks[1].Source == nil || blocks[1].Source.Type != "base64" {
		t.Errorf("image block must have base64 source; got %+v", blocks[1].Source)
	}
}

func TestBuildInjectedUserMessage_Empty(t *testing.T) {
	if _, ok := buildInjectedUserMessage(nil); ok {
		t.Error("empty input should return ok=false")
	}
}
