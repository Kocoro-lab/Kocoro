package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
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

func TestConversationReplyPersistedMessageStripsHeadEnvelopeOnly(t *testing.T) {
	head := "<kocoro_replies><reply><quote>context</quote><comment>note</comment></reply></kocoro_replies>\n\nvisible"
	message := client.Message{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
		{Type: "text", Text: head},
		{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "abc"}},
	})}
	clean, annotations := conversationReplyPersistedMessage(message)
	if clean.Content.Blocks()[0].Text != "visible" || clean.Content.Blocks()[1].Type != "image" {
		t.Fatalf("cleaned blocks = %#v", clean.Content.Blocks())
	}
	if len(annotations) != 1 || annotations[0].SelectedText != "context" {
		t.Fatalf("annotations = %#v", annotations)
	}

	// A well-formed pair PAST the head is the user's own text — e.g. pasted
	// code discussing this feature — and must persist byte-for-byte.
	midText := "queued\n<kocoro_replies><reply><quote>q</quote></reply></kocoro_replies>\nnext"
	kept, midAnnotations := conversationReplyPersistedMessage(client.Message{
		Role: "user", Content: client.NewTextContent(midText),
	})
	if kept.Content.Text() != midText || midAnnotations != nil {
		t.Fatalf("mid-text envelope was rewritten: %q %#v", kept.Content.Text(), midAnnotations)
	}
}

func TestInjectedTurnStripsOnlyThePrefixAdjacentEnvelope(t *testing.T) {
	prefix := agent.InjectedUserMessagePrefix
	env := "<kocoro_replies><reply><quote>q1</quote><comment>c1</comment></reply></kocoro_replies>"

	// The envelope immediately after the inject prefix is a follow-up head by
	// construction and is stripped.
	head, annotations := conversationReplyPersistedMessage(client.Message{
		Role: "user", Content: client.NewTextContent(prefix + env + "\n\nfollow-up text"),
	})
	if head.Content.Text() != prefix+"follow-up text" || len(annotations) != 1 {
		t.Fatalf("prefix-adjacent envelope not stripped: %q %#v", head.Content.Text(), annotations)
	}

	// A well-formed pair at a "\n\n" boundary deeper into the joined text is
	// indistinguishable from the user's own prose paragraph — it must stay
	// verbatim (the old blank-line heuristic ate it).
	prose := prefix + "please explain this snippet:\n\n" + env + "\n\nthanks"
	kept, keptAnnotations := conversationReplyPersistedMessage(client.Message{
		Role: "user", Content: client.NewTextContent(prose),
	})
	if kept.Content.Text() != prose || keptAnnotations != nil {
		t.Fatalf("mid-join envelope was eaten: %q %#v", kept.Content.Text(), keptAnnotations)
	}

	// Block-path injected batches (files present) carry no prefix; the head
	// envelope of the first follow-up gets the identical treatment.
	blockMsg, blockAnnotations := conversationReplyPersistedMessage(client.Message{
		Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "text", Text: env + "\n\nsee attachment"},
			{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "abc"}},
		}),
	})
	if blockMsg.Content.Blocks()[0].Text != "see attachment" || len(blockAnnotations) != 1 {
		t.Fatalf("block-path head envelope not stripped: %#v", blockMsg.Content.Blocks())
	}
}

func TestConversationReplyPersistedMessageKeepsMalformedEnvelopeVerbatim(t *testing.T) {
	malformed := "<kocoro_replies><reply><quote>broken</kocoro_replies>\n\nquery"
	kept, annotations := conversationReplyPersistedMessage(client.Message{
		Role: "user", Content: client.NewTextContent(malformed),
	})
	if kept.Content.Text() != malformed || annotations != nil {
		t.Fatalf("malformed envelope was dropped: %q %#v", kept.Content.Text(), annotations)
	}
}

func TestSplitConversationReplyPromptConsumesConsecutiveEnvelopes(t *testing.T) {
	first := "<kocoro_replies><reply><quote>a</quote><comment>one</comment></reply></kocoro_replies>"
	second := "<kocoro_replies><reply><quote>b</quote><comment>two</comment></reply></kocoro_replies>"
	visible, envelope := splitConversationReplyPrompt(first + "\n" + second + "\n\ntail")
	if visible != "tail" || envelope != first+"\n"+second {
		t.Fatalf("split = %q / %q", visible, envelope)
	}
	got := conversationReplyAnnotations(envelope)
	if len(got) != 2 || got[0].Comment != "one" || got[1].Comment != "two" {
		t.Fatalf("merged annotations = %#v", got)
	}
}

func TestConversationReplyTagsInsideOrdinaryTextAreNotSplit(t *testing.T) {
	raw := "Explain <kocoro_replies> literally"
	visible, envelope := splitConversationReplyPrompt(raw)
	if visible != raw || envelope != "" {
		t.Fatalf("ordinary text changed: visible=%q envelope=%q", visible, envelope)
	}
}

func TestValidateConversationReplyEnvelope(t *testing.T) {
	long := strings.Repeat("字", maxConversationAnnotationQuoteRunes+1)
	longComment := strings.Repeat("字", maxConversationAnnotationCommentRunes+1)
	var many strings.Builder
	many.WriteString(conversationRepliesOpen)
	for range maxConversationAnnotations + 1 {
		many.WriteString("<reply><quote>q</quote><comment>c</comment></reply>")
	}
	many.WriteString(conversationRepliesClose)

	cases := []struct {
		name, text, code string
	}{
		{"no envelope", "plain text", ""},
		{"valid", "<kocoro_replies><reply><quote>q</quote><comment>c</comment></reply></kocoro_replies>\n\nok", ""},
		{"malformed", "<kocoro_replies><reply></kocoro_replies>x", conversationRepliesMalformedCode},
		{"too many", many.String(), conversationRepliesTooManyCode},
		{"quote too long", "<kocoro_replies><reply><quote>" + long + "</quote></reply></kocoro_replies>", conversationReplyQuoteTooLongCode},
		{"comment too long", "<kocoro_replies><reply><comment>" + longComment + "</comment></reply></kocoro_replies>", conversationReplyCommentTooLongCode},
	}
	for _, tc := range cases {
		if code, _ := validateConversationReplyEnvelope(tc.text); code != tc.code {
			t.Fatalf("%s: code = %q, want %q", tc.name, code, tc.code)
		}
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

func TestConversationReplyIngressValidationRejectsOversizedEnvelope(t *testing.T) {
	shannonDir := t.TempDir()
	deps := &ServerDeps{ShannonDir: shannonDir, SessionCache: NewSessionCache(shannonDir)}
	server := NewServer(0, nil, deps, "test")

	malformed := `{"text":"<kocoro_replies><reply></kocoro_replies>x"}`
	for _, target := range []string{"/message", "/queue"} {
		body := malformed
		if target == "/queue" {
			body = `{"route_key":"session:reply-validate","text":"<kocoro_replies><reply></kocoro_replies>x"}`
		}
		req := httptest.NewRequest(http.MethodPost, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("POST %s = %d body=%s", target, rec.Code, rec.Body.String())
		}
		var payload struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil || payload.Code != conversationRepliesMalformedCode {
			t.Fatalf("POST %s code = %q err=%v", target, payload.Code, err)
		}
	}
}

// TestConversationReplyDrainedMailboxAnnotationsPersist pins the busy-queue
// path: an annotated message enqueued while the route was idle drains into the
// next run's merged user turn, and its annotations must survive persistence
// exactly like the trigger message's do (they previously vanished on reload).
func TestConversationReplyDrainedMailboxAnnotationsPersist(t *testing.T) {
	const sessID = "drained-annotations-persist"
	routeKey := "session:" + sanitizeRouteValue(sessID)

	deps, _ := mailboxOrderingDeps(t)
	defer deps.SessionCache.CloseAll()
	gateway := &fakeGatewayBackend{reply: "ack"}
	ts := httptest.NewServer(gateway.handler())
	defer ts.Close()
	deps.GW = client.NewGatewayClient(ts.URL, "test-key")

	queued := agenttypes.QueuedMessage{
		ID:         "mbx-annotated-1",
		Source:     "desktop",
		Text:       "<kocoro_replies><reply><quote>queued quote</quote><comment>queued comment</comment></reply></kocoro_replies>\n\nqueued visible",
		Priority:   agenttypes.PriorityNext,
		EnqueuedAt: time.Now(),
	}
	if out, err := deps.SessionCache.EnqueueMessage(routeKey, queued); err != nil || out != MailboxQueued {
		t.Fatalf("seed enqueue: out=%v err=%v", out, err)
	}

	trigger := "<kocoro_replies><reply><quote>live quote</quote><comment>live comment</comment></reply></kocoro_replies>\n\nlive visible"
	if _, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:       trigger,
		SessionID:  sessID,
		NewSession: true,
		Source:     "desktop",
	}, nullEventHandler{}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	var modelInput strings.Builder
	for _, request := range gateway.requests() {
		for _, message := range request.Messages {
			modelInput.WriteString(message.Content.Text())
		}
	}
	for _, want := range []string{"queued quote", "live quote", "queued visible", "live visible"} {
		if !strings.Contains(modelInput.String(), want) {
			t.Fatalf("model input omitted %q: %s", want, modelInput.String())
		}
	}

	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	persisted, err := mgr.Load(sessID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.Messages) == 0 || len(persisted.MessageMeta) == 0 {
		t.Fatalf("nothing persisted: %#v", persisted.Messages)
	}
	if got := persisted.Messages[0].Content.Text(); got != "queued visible\nlive visible" {
		t.Fatalf("merged visible text = %q", got)
	}
	annotations := persisted.MessageMeta[0].ConversationAnnotations
	if len(annotations) != 2 ||
		annotations[0] != (session.ConversationAnnotation{SelectedText: "queued quote", Comment: "queued comment"}) ||
		annotations[1] != (session.ConversationAnnotation{SelectedText: "live quote", Comment: "live comment"}) {
		t.Fatalf("merged annotations = %#v", annotations)
	}
	for _, message := range persisted.Messages {
		if strings.Contains(message.Content.Text(), conversationRepliesOpen) {
			t.Fatalf("persisted message leaked envelope: %q", message.Content.Text())
		}
	}
}

// TestApplyTurnMessagesPersistsMidRunInjectedAnnotations pins the other half
// of the busy-queue path: a follow-up injected into a RUNNING loop becomes its
// own user message, whose head envelope must decode into persisted annotations
// instead of evaporating.
func TestApplyTurnMessagesPersistsMidRunInjectedAnnotations(t *testing.T) {
	injectCh := make(chan agent.InjectedMessage, 1)
	var calls int
	var mu sync.Mutex
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		n := calls
		mu.Unlock()
		if n == 1 {
			injectCh <- agent.InjectedMessage{
				Text: "<kocoro_replies><reply><quote>mid quote</quote><comment>mid comment</comment></reply></kocoro_replies>\n\nmid visible",
			}
		}
		_ = json.NewEncoder(w).Encode(client.CompletionResponse{
			Model: "test-model", OutputText: "done", FinishReason: "end_turn",
			Usage: client.Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2}, RequestID: "r",
		})
	}))
	defer ts.Close()

	gw := client.NewGatewayClient(ts.URL, "")
	loop := agent.NewAgentLoop(gw, agent.NewToolRegistry(), "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	if _, _, err := loop.Run(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	sess := &session.Session{ID: "mid-run-annotations"}
	b := captureTurnBaseline(sess, "desktop", false)
	applyTurnMessages(sess, loop, b)

	var injectedIdx = -1
	for i, msg := range sess.Messages {
		if msg.Role == "user" && strings.Contains(msg.Content.Text(), "mid visible") {
			injectedIdx = i
		}
		if strings.Contains(msg.Content.Text(), conversationRepliesOpen) {
			t.Fatalf("persisted message %d leaked envelope: %q", i, msg.Content.Text())
		}
	}
	if injectedIdx < 0 {
		t.Fatalf("injected message not persisted: %#v", sess.Messages)
	}
	annotations := sess.MessageMeta[injectedIdx].ConversationAnnotations
	if len(annotations) != 1 ||
		annotations[0] != (session.ConversationAnnotation{SelectedText: "mid quote", Comment: "mid comment"}) {
		t.Fatalf("injected annotations = %#v", annotations)
	}
}
