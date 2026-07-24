package daemon

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// TestQuestionRoundTripViaHandler exercises the full daemon-side ask-user path
// end to end without a model or a live daemon: a blocked QuestionBroker.Request
// (what the ask_user_question tool does) is resolved by a real POST /question
// through handleQuestion (what Desktop does), and the answer flows back to the
// caller. It also asserts the two terminal bus events UI clients bind to
// (question.request to render, question.resolved to dismiss).
func TestQuestionRoundTripViaHandler(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")

	// Subscribe BEFORE the request so the question.request emit is observed.
	bus := srv.eventBus.Subscribe()
	defer srv.eventBus.Unsubscribe(bus)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// The tool side: block until the user answers. "desktop" is an attended,
	// interactive source so the non-interactive decline gate does not fire.
	resCh := make(chan questionResolution, 1)
	go func() {
		resCh <- srv.questionBroker.Request(ctx, ApprovalRequestMeta{
			SessionID: "sess-1",
			Source:    "desktop",
			Agent:     "agent-x",
		}, &QuestionRequest{
			Questions: []Question{{
				ID:          "q0",
				Header:      "Approach",
				Question:    "Which approach should I take?",
				MultiSelect: false,
				AllowOther:  true,
				Options: []QuestionOption{
					{Label: "Clean first", Description: "Normalize the data before analysis.", Recommended: true},
					{Label: "Explore first", Description: "Jump straight to EDA."},
				},
			}},
		})
	}()

	// The UI side: wait for question.request, then answer via the real handler.
	requestID := waitForQuestionRequest(t, bus)

	body := strings.NewReader(`{"request_id":"` + requestID + `","action":"answer","answers":[{"id":"q0","values":["Clean first"]}]}`)
	req := httptest.NewRequest(http.MethodPost, "/question", body)
	rec := httptest.NewRecorder()
	srv.handleQuestion(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /question status = %d, want 200 (body %q)", rec.Code, rec.Body.String())
	}

	// The tool call unblocks with the structured answer (full label, not a token).
	select {
	case res := <-resCh:
		if res.Action != QuestionActionAnswer {
			t.Fatalf("resolution action = %q, want %q", res.Action, QuestionActionAnswer)
		}
		if len(res.Answers) != 1 || res.Answers[0].ID != "q0" ||
			len(res.Answers[0].Values) != 1 || res.Answers[0].Values[0] != "Clean first" {
			t.Fatalf("resolution answers = %+v, want q0=[Clean first]", res.Answers)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("QuestionBroker.Request did not return after POST /question")
	}

	// Desktop dismisses the card on the terminal resolved event.
	resolved := waitForEvent(t, bus, EventQuestionResolved)
	var rp struct {
		RequestID  string `json:"request_id"`
		Action     string `json:"action"`
		ResolvedBy string `json:"resolved_by"`
	}
	if err := json.Unmarshal(resolved.Payload, &rp); err != nil {
		t.Fatalf("unmarshal question.resolved: %v", err)
	}
	if rp.RequestID != requestID || rp.Action != QuestionActionAnswer || rp.ResolvedBy != "kocoro" {
		t.Fatalf("question.resolved = %+v, want request_id=%s action=answer resolved_by=kocoro", rp, requestID)
	}
}

// TestQuestionNonInteractiveDeclines confirms a source with no selection UI
// declines immediately instead of blocking (a question has no safe auto-answer).
func TestQuestionNonInteractiveDeclines(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	res := srv.questionBroker.Request(ctx, ApprovalRequestMeta{Source: ChannelWeChat}, &QuestionRequest{
		Questions: []Question{{ID: "q0", Question: "?", Options: []QuestionOption{{Label: "a"}, {Label: "b"}}}},
	})
	if res.Action != QuestionActionDecline {
		t.Fatalf("non-interactive channel action = %q, want %q", res.Action, QuestionActionDecline)
	}
}

func waitForQuestionRequest(t *testing.T, bus <-chan Event) string {
	t.Helper()
	evt := waitForEvent(t, bus, EventQuestionRequest)
	var p struct {
		RequestID string     `json:"request_id"`
		Questions []Question `json:"questions"`
	}
	if err := json.Unmarshal(evt.Payload, &p); err != nil {
		t.Fatalf("unmarshal question.request: %v", err)
	}
	if p.RequestID == "" || len(p.Questions) != 1 || p.Questions[0].Question == "" {
		t.Fatalf("question.request payload malformed: %s", string(evt.Payload))
	}
	return p.RequestID
}

func waitForEvent(t *testing.T, bus <-chan Event, typ string) Event {
	t.Helper()
	deadline := time.After(3 * time.Second)
	for {
		select {
		case evt := <-bus:
			if evt.Type == typ {
				return evt
			}
		case <-deadline:
			t.Fatalf("timed out waiting for %q event", typ)
		}
	}
}
