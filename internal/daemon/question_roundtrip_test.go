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

	// Retrying the same POST after a lost HTTP response is idempotent: return
	// success, but never deliver a second answer or emit a second terminal
	// event for the already-resolved request.
	duplicateBody := strings.NewReader(`{"request_id":"` + requestID + `","action":"answer","answers":[{"id":"q0","values":["Clean first"]}]}`)
	duplicateRec := httptest.NewRecorder()
	srv.handleQuestion(duplicateRec, httptest.NewRequest(http.MethodPost, "/question", duplicateBody))
	if duplicateRec.Code != http.StatusOK {
		t.Fatalf("duplicate POST /question status = %d, want 200", duplicateRec.Code)
	}
	select {
	case evt := <-bus:
		if evt.Type == EventQuestionResolved {
			t.Fatalf("duplicate POST emitted a second terminal event: %s", evt.Payload)
		}
	case <-time.After(150 * time.Millisecond):
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

func TestQuestionRoundTripMultipleQuestionsPreservesIDsAndFullLabels(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	bus := srv.eventBus.Subscribe()
	defer srv.eventBus.Unsubscribe(bus)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resCh := make(chan questionResolution, 1)
	go func() {
		resCh <- srv.questionBroker.Request(ctx, ApprovalRequestMeta{
			SessionID: "sess-multi",
			Source:    "desktop",
		}, &QuestionRequest{Questions: []Question{
			{
				ID:       "q0",
				Question: "Deployment target?",
				Options: []QuestionOption{
					{Label: "Staging (recommended)"},
					{Label: "Production"},
				},
			},
			{
				ID:          "q1",
				Question:    "Required checks?",
				MultiSelect: true,
				Options: []QuestionOption{
					{Label: "Unit tests"},
					{Label: "Race detector"},
					{Label: "Manual QA"},
				},
			},
		}})
	}()

	request := waitForEvent(t, bus, EventQuestionRequest)
	var payload struct {
		RequestID string     `json:"request_id"`
		Questions []Question `json:"questions"`
	}
	if err := json.Unmarshal(request.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question.request: %v", err)
	}
	if len(payload.Questions) != 2 || payload.Questions[0].ID != "q0" || payload.Questions[1].ID != "q1" {
		t.Fatalf("question IDs changed on the wire: %+v", payload.Questions)
	}

	body := strings.NewReader(`{"request_id":"` + payload.RequestID + `","action":"answer","answers":[` +
		`{"id":"q0","values":["Staging (recommended)"]},` +
		`{"id":"q1","values":["Unit tests","Race detector"]}]}`)
	rec := httptest.NewRecorder()
	srv.handleQuestion(rec, httptest.NewRequest(http.MethodPost, "/question", body))
	if rec.Code != http.StatusOK {
		t.Fatalf("POST /question status = %d, body %q", rec.Code, rec.Body.String())
	}
	res := <-resCh
	if len(res.Answers) != 2 ||
		res.Answers[0].ID != "q0" || strings.Join(res.Answers[0].Values, "|") != "Staging (recommended)" ||
		res.Answers[1].ID != "q1" || strings.Join(res.Answers[1].Values, "|") != "Unit tests|Race detector" {
		t.Fatalf("answers lost IDs or full labels: %+v", res.Answers)
	}
}

func TestQuestionContextCancelEmitsSingleDaemonCleanup(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	bus := srv.eventBus.Subscribe()
	defer srv.eventBus.Unsubscribe(bus)

	ctx, cancel := context.WithCancel(context.Background())
	resCh := make(chan questionResolution, 1)
	go func() {
		resCh <- srv.questionBroker.Request(ctx, ApprovalRequestMeta{Source: "desktop"}, &QuestionRequest{
			Questions: []Question{{ID: "q0", Question: "Pick", Options: []QuestionOption{{Label: "A"}, {Label: "B"}}}},
		})
	}()
	requestID := waitForQuestionRequest(t, bus)
	cancel()

	if res := <-resCh; res.Action != QuestionActionCancel {
		t.Fatalf("context cancel action = %q, want cancel", res.Action)
	}
	resolved := waitForEvent(t, bus, EventQuestionResolved)
	var payload QuestionResolvedPayload
	if err := json.Unmarshal(resolved.Payload, &payload); err != nil {
		t.Fatalf("unmarshal question.resolved: %v", err)
	}
	if payload.RequestID != requestID || payload.Action != QuestionActionCancel || payload.ResolvedBy != "daemon" {
		t.Fatalf("cleanup payload = %+v", payload)
	}
	if srv.questionBroker.Resolve(requestID, questionResolution{Action: QuestionActionAnswer}, nil) {
		t.Fatal("a cancelled question must not accept a late answer")
	}
}

func TestQuestionRejectsUngroundedAnswerAndRemainsPending(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	bus := srv.eventBus.Subscribe()
	defer srv.eventBus.Unsubscribe(bus)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resCh := make(chan questionResolution, 1)
	go func() {
		resCh <- srv.questionBroker.Request(ctx, ApprovalRequestMeta{Source: "desktop"}, &QuestionRequest{
			Questions: []Question{{
				ID:         "q0",
				Question:   "Pick a color",
				AllowOther: false,
				Options:    []QuestionOption{{Label: "Red"}, {Label: "Blue"}},
			}},
		})
	}()
	requestID := waitForQuestionRequest(t, bus)

	invalid := httptest.NewRecorder()
	srv.handleQuestion(invalid, httptest.NewRequest(
		http.MethodPost,
		"/question",
		strings.NewReader(`{"request_id":"`+requestID+`","action":"answer","answers":[{"id":"q0","values":["opt_b"]}]}`),
	))
	if invalid.Code != http.StatusBadRequest {
		t.Fatalf("ungrounded answer status = %d, want 400; body=%q", invalid.Code, invalid.Body.String())
	}
	select {
	case res := <-resCh:
		t.Fatalf("invalid answer unexpectedly resolved the question: %+v", res)
	default:
	}

	valid := httptest.NewRecorder()
	srv.handleQuestion(valid, httptest.NewRequest(
		http.MethodPost,
		"/question",
		strings.NewReader(`{"request_id":"`+requestID+`","action":"answer","answers":[{"id":"q0","values":["Blue"]}]}`),
	))
	if valid.Code != http.StatusOK {
		t.Fatalf("grounded answer status = %d, want 200; body=%q", valid.Code, valid.Body.String())
	}
	res := <-resCh
	if len(res.Answers) != 1 || len(res.Answers[0].Values) != 1 || res.Answers[0].Values[0] != "Blue" {
		t.Fatalf("valid full label not preserved: %+v", res.Answers)
	}
}

func TestQuestionRejectsIncompleteMultiQuestionAnswer(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	bus := srv.eventBus.Subscribe()
	defer srv.eventBus.Unsubscribe(bus)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	resCh := make(chan questionResolution, 1)
	go func() {
		resCh <- srv.questionBroker.Request(ctx, ApprovalRequestMeta{Source: "desktop"}, &QuestionRequest{
			Questions: []Question{
				{ID: "q0", Question: "Target?", Options: []QuestionOption{{Label: "Staging"}, {Label: "Production"}}},
				{ID: "q1", Question: "Checks?", MultiSelect: true, Options: []QuestionOption{{Label: "Unit"}, {Label: "Race"}}},
			},
		})
	}()
	requestID := waitForQuestionRequest(t, bus)

	rec := httptest.NewRecorder()
	srv.handleQuestion(rec, httptest.NewRequest(
		http.MethodPost,
		"/question",
		strings.NewReader(`{"request_id":"`+requestID+`","action":"answer","answers":[{"id":"q0","values":["Staging"]}]}`),
	))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("incomplete answer status = %d, want 400; body=%q", rec.Code, rec.Body.String())
	}
	select {
	case res := <-resCh:
		t.Fatalf("incomplete answer unexpectedly resolved the question: %+v", res)
	default:
	}
	cancel()
	if res := <-resCh; res.Action != QuestionActionCancel {
		t.Fatalf("cleanup action = %q, want cancel", res.Action)
	}
}

func TestHandleQuestionRejectsMalformedIngress(t *testing.T) {
	srv := NewServer(0, nil, nil, "test")
	tests := map[string]string{
		"missing request id": `{"action":"answer","answers":[]}`,
		"invalid action":     `{"request_id":"qst_missing","action":"cancel"}`,
		"malformed json":     `{`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			srv.handleQuestion(rec, httptest.NewRequest(http.MethodPost, "/question", strings.NewReader(body)))
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400; body=%q", rec.Code, rec.Body.String())
			}
		})
	}
}

func TestClampQuestionAutoResolutionMs(t *testing.T) {
	tests := map[string]struct {
		input int
		want  int
	}{
		"omitted":       {input: 0, want: 0},
		"negative":      {input: -1, want: 0},
		"below minimum": {input: 1, want: questionAutoResolutionMinMs},
		"minimum":       {input: questionAutoResolutionMinMs, want: questionAutoResolutionMinMs},
		"inside range":  {input: 90000, want: 90000},
		"maximum":       {input: questionAutoResolutionMaxMs, want: questionAutoResolutionMaxMs},
		"above maximum": {input: 999999, want: questionAutoResolutionMaxMs},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if got := clampQuestionAutoResolutionMs(tt.input); got != tt.want {
				t.Fatalf("clampQuestionAutoResolutionMs(%d) = %d, want %d", tt.input, got, tt.want)
			}
		})
	}
}

func TestQuestionIndexFromID(t *testing.T) {
	tests := map[string]int{
		"q0":  0,
		"q3":  3,
		"":    -1,
		"q":   -1,
		"x1":  -1,
		"q-1": -1,
		"qA":  -1,
	}
	for id, want := range tests {
		if got := questionIndexFromID(id); got != want {
			t.Errorf("questionIndexFromID(%q) = %d, want %d", id, got, want)
		}
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
	if p.RequestID == "" || len(p.Questions) == 0 || p.Questions[0].Question == "" {
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
