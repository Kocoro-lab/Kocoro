package daemon

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestConversationReplyPromptSeparatesModelContextFromVisibleText(t *testing.T) {
	raw := "<kocoro_replies>\n<reply><quote>answer</quote><comment>clarify</comment></reply>\n</kocoro_replies>\n\n/inspect target"
	visible, envelope := splitConversationReplyPrompt(raw)
	if visible != "/inspect target" {
		t.Fatalf("visible = %q", visible)
	}
	if got := restoreConversationReplyPrompt("expanded command", envelope); got != envelope+"\n\nexpanded command" {
		t.Fatalf("restored = %q", got)
	}
}

func TestConversationReplyAnnotationsDecodeEscapingAndBounds(t *testing.T) {
	envelope := `<kocoro_replies><reply><quote>price &lt; budget</quote><comment>check &amp; compare</comment></reply></kocoro_replies>`
	got := conversationReplyAnnotations(envelope)
	want := []session.ConversationAnnotation{{SelectedText: "price < budget", Comment: "check & compare"}}
	if len(got) != len(want) || got[0] != want[0] {
		t.Fatalf("annotations = %#v, want %#v", got, want)
	}
	if malformed := conversationReplyAnnotations("<kocoro_replies><reply>"); malformed != nil {
		t.Fatalf("malformed annotations = %#v", malformed)
	}
}

func TestVisibleConversationReplyTextCleansInjectedMessages(t *testing.T) {
	raw := "queued\n<kocoro_replies><reply>secret context</reply></kocoro_replies>\nnext"
	if got := visibleConversationReplyText(raw); got != "queued\n\nnext" {
		t.Fatalf("visible = %q", got)
	}
	message := client.Message{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
		{Type: "text", Text: raw},
		{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "abc"}},
	})}
	clean := visibleConversationReplyMessage(message)
	if clean.Content.Blocks()[0].Text != "queued\n\nnext" || clean.Content.Blocks()[1].Type != "image" {
		t.Fatalf("cleaned blocks = %#v", clean.Content.Blocks())
	}
}

func TestConversationReplyTagsInsideOrdinaryTextAreNotSplit(t *testing.T) {
	raw := "Explain <kocoro_replies> literally"
	visible, envelope := splitConversationReplyPrompt(raw)
	if visible != raw || envelope != "" {
		t.Fatalf("ordinary text changed: visible=%q envelope=%q", visible, envelope)
	}
	if got := visibleConversationReplyText("  ordinary text  "); got != "  ordinary text  " {
		t.Fatalf("ordinary visible text changed: %q", got)
	}
}

func TestConversationReplyContextReachesModelButNotPersistedSession(t *testing.T) {
	gateway := &fakeGatewayBackend{reply: "done"}
	server := httptest.NewServer(gateway.handler())
	defer server.Close()
	deps := runAgentContractTestDeps(t, server.URL)
	defer deps.SessionCache.CloseAll()

	raw := "<kocoro_replies><reply><quote>answer</quote><comment>clarify</comment></reply></kocoro_replies>\n\nvisible request"
	result, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:    raw,
		Source:  "desktop",
		Channel: "kocoro",
	}, nullEventHandler{})
	if err != nil {
		t.Fatal(err)
	}
	requests := gateway.requests()
	var modelInput strings.Builder
	for _, request := range requests {
		for _, message := range request.Messages {
			modelInput.WriteString(message.Content.Text())
		}
	}
	if !strings.Contains(modelInput.String(), "<comment>clarify</comment>") {
		t.Fatalf("model input omitted reply context: %s", modelInput.String())
	}

	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	persisted, err := mgr.Load(result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(persisted.Title, conversationRepliesOpen) {
		t.Fatalf("persisted title = %q", persisted.Title)
	}
	if len(persisted.Messages) == 0 || persisted.Messages[0].Content.Text() != "visible request" {
		t.Fatalf("persisted visible request = %#v", persisted.Messages)
	}
	if len(persisted.MessageMeta) == 0 {
		t.Fatal("persisted message metadata is empty")
	}
	wantAnnotations := []session.ConversationAnnotation{{SelectedText: "answer", Comment: "clarify"}}
	if got := persisted.MessageMeta[0].ConversationAnnotations; len(got) != 1 || got[0] != wantAnnotations[0] {
		t.Fatalf("persisted annotations = %#v, want %#v", got, wantAnnotations)
	}
	for _, message := range persisted.Messages {
		if strings.Contains(message.Content.Text(), conversationRepliesOpen) {
			t.Fatalf("persisted message leaked reply envelope: %q", message.Content.Text())
		}
	}
}
