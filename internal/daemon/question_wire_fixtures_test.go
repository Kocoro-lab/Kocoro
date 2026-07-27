package daemon

// Wire-contract tests for the ask_user_question interaction, in the same style
// as wire_fixtures_test.go: each payload is produced through the REAL producer
// path (the question bus emitters / the full Handler() router / the per-request
// SSE sendFn), semantically compared to the committed fixture, and decoded into
// a consumer-shaped struct mirroring what Kocoro Desktop decodes.

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// questionFixtureQuestions is the single Question the question fixtures share.
// Driving the broker directly (rather than through the tool→adapter path) means
// the test assigns the per-question id the adapter would normally mint.
func questionFixtureQuestions() []Question {
	return []Question{{
		ID:          "q0",
		Header:      "Auth method",
		Question:    "Which authentication method should I wire up?",
		MultiSelect: false,
		AllowOther:  true,
		Options: []QuestionOption{
			{Label: "OAuth 2.0", Description: "Delegated login via the provider's consent screen.", Recommended: true},
			{Label: "API key", Description: "A static secret stored in config."},
		},
	}}
}

// TestWireFixture_QuestionRequestAndResolved_Bus drives a pending question on
// the server broker and resolves it through the REAL router (POST /question),
// asserting both bus payloads against their fixtures.
func TestWireFixture_QuestionRequestAndResolved_Bus(t *testing.T) {
	reqFixture := loadWireFixture(t, "bus_event.question_request.json")
	resFixture := loadWireFixture(t, "bus_event.question_resolved.json")

	srv := NewServer(0, nil, nil, "test")
	handler := srv.Handler()
	sub := srv.EventBus().Subscribe()
	defer srv.EventBus().Unsubscribe(sub)

	meta := ApprovalRequestMeta{
		MessageID: "m-1",
		SessionID: reqFixture["session_id"].(string),
		Source:    reqFixture["source"].(string),
		Agent:     reqFixture["agent"].(string),
	}
	dq := &QuestionRequest{
		Questions:        questionFixtureQuestions(),
		AutoResolutionMs: int(reqFixture["auto_resolution_ms"].(float64)),
	}

	resCh := make(chan questionResolution, 1)
	go func() {
		resCh <- srv.questionBroker.Request(t.Context(), meta, dq)
	}()

	evt := waitBusEvent(t, sub, EventQuestionRequest)
	produced := parseJSONMap(t, evt.Payload)
	realID, _ := produced["request_id"].(string)
	normalizePrefixedID(t, produced, reqFixture, "request_id", "qst_")
	normalizeRFC3339(t, produced, reqFixture, "ts")
	assertSemanticEqual(t, reqFixture, produced)

	// Consumer-shaped decode of the producer bytes (mirrors the Desktop
	// question-card decoder fields).
	var card struct {
		RequestID string `json:"request_id"`
		SessionID string `json:"session_id"`
		Agent     string `json:"agent"`
		Source    string `json:"source"`
		Channel   string `json:"channel"`
		Questions []struct {
			ID       string `json:"id"`
			Header   string `json:"header"`
			Question string `json:"question"`
			Options  []struct {
				Label       string `json:"label"`
				Recommended bool   `json:"recommended"`
			} `json:"options"`
		} `json:"questions"`
		AutoResolutionMs int    `json:"auto_resolution_ms"`
		TS               string `json:"ts"`
	}
	if err := json.Unmarshal(evt.Payload, &card); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if card.RequestID != realID || len(card.Questions) != 1 || card.Questions[0].ID != "q0" ||
		len(card.Questions[0].Options) != 2 || !card.Questions[0].Options[0].Recommended {
		t.Fatalf("consumer decode lost fields: %+v", card)
	}

	// Resolve through the real HTTP seam Desktop calls.
	body := strings.NewReader(fmt.Sprintf(`{"request_id":%q,"action":"answer","answers":[{"id":"q0","values":["OAuth 2.0"]}]}`, realID))
	httpReq := httptest.NewRequest(http.MethodPost, "/question", body)
	httpReq.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httpReq)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /question = %d, body %s", rec.Code, rec.Body.String())
	}

	resolvedEvt := waitBusEvent(t, sub, EventQuestionResolved)
	resolvedProduced := parseJSONMap(t, resolvedEvt.Payload)
	normalizePrefixedID(t, resolvedProduced, resFixture, "request_id", "qst_")
	normalizeRFC3339(t, resolvedProduced, resFixture, "ts")
	assertSemanticEqual(t, resFixture, resolvedProduced)

	res := <-resCh
	if res.Action != QuestionActionAnswer || len(res.Answers) != 1 || res.Answers[0].Values[0] != "OAuth 2.0" {
		t.Fatalf("resolution = %+v, want answer OAuth 2.0", res)
	}
}

// TestWireFixture_QuestionResolvedDaemonCleanup_Bus exercises the synthetic
// terminal event emitted when the daemon abandons a pending question (CancelAll
// on disconnect; same emitter as timeout / ctx-cancel).
func TestWireFixture_QuestionResolvedDaemonCleanup_Bus(t *testing.T) {
	fixture := loadWireFixture(t, "bus_event.question_resolved.daemon_cleanup.json")

	bus := NewEventBus()
	sub := bus.Subscribe()
	defer bus.Unsubscribe(sub)
	broker := NewQuestionBroker(func(QuestionRequest) error { return nil })
	WireQuestionBusHooks(broker, bus, nil)

	resCh := make(chan questionResolution, 1)
	go func() {
		resCh <- broker.Request(t.Context(), ApprovalRequestMeta{MessageID: "m-1"}, &QuestionRequest{Questions: questionFixtureQuestions()})
	}()
	waitBusEvent(t, sub, EventQuestionRequest) // emitted=true is now set
	broker.CancelAll()

	evt := waitBusEvent(t, sub, EventQuestionResolved)
	produced := parseJSONMap(t, evt.Payload)
	normalizePrefixedID(t, produced, fixture, "request_id", "qst_")
	normalizeRFC3339(t, produced, fixture, "ts")
	assertSemanticEqual(t, fixture, produced)

	if res := <-resCh; res.Action != QuestionActionCancel {
		t.Fatalf("resolution = %+v, want cancel", res)
	}
}

// TestWireFixture_Question_PerRequestSSE asserts the per-request stream's
// `event: question` data payload (the full QuestionRequest struct). The sendFn
// mirrors handleMessageSSE's wiring.
func TestWireFixture_Question_PerRequestSSE(t *testing.T) {
	fixture := loadWireFixture(t, "sse_event.question.json")

	rec := httptest.NewRecorder()
	sent := make(chan string, 1)
	productionSendFn := newSSEQuestionSendFn(rec, rec)
	reqBroker := NewQuestionBroker(func(qreq QuestionRequest) error {
		if err := productionSendFn(qreq); err != nil {
			return err
		}
		sent <- qreq.RequestID
		return nil
	})

	meta := ApprovalRequestMeta{
		SessionID: fixture["session_id"].(string),
		Source:    fixture["source"].(string),
	}
	dq := &QuestionRequest{
		Questions:        questionFixtureQuestions(),
		AutoResolutionMs: int(fixture["auto_resolution_ms"].(float64)),
	}
	resCh := make(chan questionResolution, 1)
	go func() {
		resCh <- reqBroker.Request(t.Context(), meta, dq)
	}()
	realID := <-sent
	if !reqBroker.Resolve(realID, questionResolution{Action: QuestionActionAnswer, Answers: []QuestionAnswer{{ID: "q0", Values: []string{"OAuth 2.0"}}}}, nil) {
		t.Fatal("Resolve did not claim the pending request")
	}
	if res := <-resCh; res.Action != QuestionActionAnswer {
		t.Fatalf("resolution = %+v, want answer", res)
	}

	frames := parseSSEFrames(t, rec.Body.String())
	if len(frames) != 1 || frames[0][0] != "question" {
		t.Fatalf("frames = %v, want one question frame", frames)
	}
	produced := parseJSONMap(t, []byte(frames[0][1]))
	normalizePrefixedID(t, produced, fixture, "request_id", "qst_")
	assertSemanticEqual(t, fixture, produced)
}
