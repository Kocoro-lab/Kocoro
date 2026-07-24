package daemon

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"time"

	agentpkg "github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// questionResolution is the decision type the QuestionBroker's pendingCore
// buffers: the user's answer/decline, or a daemon-originated cancel (timeout /
// disconnect). Answers is populated only when Action == QuestionActionAnswer.
type questionResolution struct {
	Action  string
	Answers []QuestionAnswer
}

// QuestionBroker mediates the structured ask-user interaction between the
// ask_user_question tool and a UI client. It shares the request/resolve
// lifecycle with ApprovalBroker via pendingCore (see pending.go); it adds only
// the question-specific wire face — the QuestionRequest payload and the
// non-interactive channel gate (a question has no safe auto-answer, so those
// channels decline rather than auto-approve).
type QuestionBroker struct {
	*pendingCore[questionResolution]
	sendFn    func(req QuestionRequest) error
	onRequest func(req QuestionRequest)
}

// NewQuestionBroker creates a broker. sendFn publishes a QuestionRequest to the
// transport (SSE `question` frame, or a no-op for the server-level broker whose
// per-request SSE brokers inherit the bus hooks). It must be reconnect-safe.
func NewQuestionBroker(sendFn func(req QuestionRequest) error) *QuestionBroker {
	return &QuestionBroker{
		pendingCore: newPendingCore[questionResolution](),
		sendFn:      sendFn,
	}
}

// SetOnRequest sets the callback invoked after a QuestionRequest is published
// to the transport (used to emit EventQuestionRequest to the bus).
func (b *QuestionBroker) SetOnRequest(fn func(req QuestionRequest)) { b.onRequest = fn }

// SetOnCleanup sets the callback invoked when the daemon abandons a pending
// question (timeout / ctx cancel / CancelAll) so a synthetic
// EventQuestionResolved dismisses the card.
func (b *QuestionBroker) SetOnCleanup(fn func(requestID string)) { b.onCleanup = fn }

// Request publishes a question and blocks until the user answers/declines via
// POST /question, the auto-resolution window elapses, ctx is cancelled, or
// CancelAll fires. meta.MessageID/SessionID/Source/Channel/ThreadID/Agent fill
// the transport identity fields; the caller supplies req.Questions and
// req.AutoResolutionMs (already clamped).
func (b *QuestionBroker) Request(ctx context.Context, meta ApprovalRequestMeta, req *QuestionRequest) questionResolution {
	cancel := questionResolution{Action: QuestionActionCancel}

	// Non-interactive IM/voice channels have no selection UI and a question
	// cannot be auto-answered — unlike approvals there is no safe "yes". Decline
	// so the agent falls back to its own best judgment instead of blocking.
	if IsNonInteractiveApprovalChannel(meta.Source) {
		log.Printf("question: declining for non-interactive channel %q (no selection UI)", meta.Source)
		return questionResolution{Action: QuestionActionDecline}
	}

	reqID := generateQuestionRequestID()
	req.MessageID = meta.MessageID
	req.SessionID = meta.SessionID
	req.Source = meta.Source
	req.Channel = meta.Channel
	req.ThreadID = meta.ThreadID
	req.Agent = meta.Agent
	req.RequestID = reqID

	// A positive (already-clamped) AutoResolutionMs shortens the window; else
	// fall back to the shared ApprovalTimeout so a question can never pin the
	// tool call open indefinitely.
	timeout := ApprovalTimeout
	if req.AutoResolutionMs > 0 {
		timeout = time.Duration(req.AutoResolutionMs) * time.Millisecond
	}

	sent := *req
	return b.run(ctx, reqID, timeout, cancel,
		func() error { return b.sendFn(sent) },
		func() {
			if b.onRequest != nil {
				b.onRequest(sent)
			}
		},
	)
}

// CancelAll cancels all pending questions (daemon-originated), firing
// onCleanup (question.resolved / resolved_by=daemon) for every emitted request.
func (b *QuestionBroker) CancelAll() { b.cancelAll(questionResolution{Action: QuestionActionCancel}) }

// WireQuestionBusHooks installs the standard EventBus emitters on b so
// question.request / question.resolved flow through the same code path
// regardless of which broker created them (the server-level broker or the SSE
// per-request brokers that inherit these hooks). notify, when non-nil, is fired
// on every daemon-originated cleanup so a future Cloud transport can clear a
// channel question card; pass nil for the local-only path (no Cloud question
// transport exists today).
func WireQuestionBusHooks(b *QuestionBroker, bus *EventBus, notify func(QuestionResolvedPayload) error) {
	if b == nil {
		return
	}
	b.SetOnRequest(makeQuestionRequestEmitter(bus))
	b.SetOnCleanup(makeQuestionCleanupEmitter(bus, notify))
}

// makeQuestionRequestEmitter publishes EventQuestionRequest with the payload
// Desktop needs to render the question card. Mirrors makeApprovalRequestEmitter;
// auto_resolution_ms is omitted when zero (matching the wire omitempty) so a
// naive client never sees a null.
func makeQuestionRequestEmitter(bus *EventBus) func(req QuestionRequest) {
	return func(req QuestionRequest) {
		payload := map[string]any{
			"request_id": req.RequestID,
			"session_id": req.SessionID,
			"agent":      req.Agent,
			"source":     req.Source,
			"channel":    req.Channel,
			"questions":  req.Questions,
			"ts":         nowISO(),
		}
		if req.AutoResolutionMs > 0 {
			payload["auto_resolution_ms"] = req.AutoResolutionMs
		}
		emitBusJSON(bus, EventQuestionRequest, payload)
	}
}

// makeQuestionCleanupEmitter publishes a synthetic EventQuestionResolved
// (action=cancel, resolved_by=daemon) when the daemon abandons a pending
// question, and optionally notifies a Cloud sink. Mirrors
// makeApprovalCleanupEmitter, including running notify on its own goroutine
// because onCleanup is invoked under the broker mutex by CancelAll.
func makeQuestionCleanupEmitter(bus *EventBus, notify func(QuestionResolvedPayload) error) func(requestID string) {
	return func(requestID string) {
		emitBusJSON(bus, EventQuestionResolved, map[string]any{
			"request_id":  requestID,
			"action":      QuestionActionCancel,
			"resolved_by": "daemon",
			"ts":          nowISO(),
		})
		if notify != nil {
			go func() {
				_ = notify(QuestionResolvedPayload{
					RequestID:  requestID,
					Action:     QuestionActionCancel,
					ResolvedBy: "daemon",
				})
			}()
		}
	}
}

// brokerQuestionAsker adapts a QuestionBroker to the agent.QuestionAsker
// interface the ask_user_question tool calls through context injection. It
// converts the tool's neutral UIQuestionRequest into the wire QuestionRequest
// (minting per-question ids q0.. and clamping the auto-resolution window),
// blocks on the broker, then maps the resolution back to neutral answers keyed
// by question text. metaFn is read lazily so the session id resolved mid-run
// (after RunAgent creates the session) is captured at ask time, mirroring how
// sseEventHandler.OnApprovalNeeded reads h.sessionID.
type brokerQuestionAsker struct {
	broker *QuestionBroker
	metaFn func() ApprovalRequestMeta
}

func (a *brokerQuestionAsker) AskUserQuestion(ctx context.Context, ureq agentpkg.UIQuestionRequest) agentpkg.UIQuestionResult {
	dq := &QuestionRequest{
		AutoResolutionMs: clampQuestionAutoResolutionMs(ureq.AutoResolutionMs),
	}
	for i, q := range ureq.Questions {
		wq := Question{
			ID:          "q" + strconv.Itoa(i),
			Header:      q.Header,
			Question:    q.Question,
			MultiSelect: q.MultiSelect,
			AllowOther:  q.AllowOther,
		}
		for _, o := range q.Options {
			wq.Options = append(wq.Options, QuestionOption{
				Label:       o.Label,
				Description: o.Description,
				Preview:     o.Preview,
				Recommended: o.Recommended,
			})
		}
		dq.Questions = append(dq.Questions, wq)
	}

	res := a.broker.Request(ctx, a.metaFn(), dq)

	out := agentpkg.UIQuestionResult{Action: res.Action}
	for _, ans := range res.Answers {
		qtext := ""
		if idx := questionIndexFromID(ans.ID); idx >= 0 && idx < len(ureq.Questions) {
			qtext = ureq.Questions[idx].Question
		}
		out.Answers = append(out.Answers, agentpkg.UIQuestionAnswer{Question: qtext, Values: ans.Values})
	}
	return out
}

// questionIndexFromID parses a "q<N>" wire id back to its 0-based index, or -1.
func questionIndexFromID(id string) int {
	rest, ok := trimQPrefix(id)
	if !ok {
		return -1
	}
	n, err := strconv.Atoi(rest)
	if err != nil || n < 0 {
		return -1
	}
	return n
}

func trimQPrefix(id string) (string, bool) {
	if len(id) < 2 || id[0] != 'q' {
		return "", false
	}
	return id[1:], true
}

// newSSEQuestionSendFn builds the per-request broker sendFn that frames a
// QuestionRequest as the `event: question` SSE frame. Named (rather than an
// inline closure) so the wire-fixture test exercises the real framing — event
// name included — mirroring newSSEApprovalSendFn.
func newSSEQuestionSendFn(w io.Writer, flusher http.Flusher) func(QuestionRequest) error {
	return func(qreq QuestionRequest) error {
		_, err := fmt.Fprintf(w, "event: question\ndata: %s\n\n", mustJSON(qreq))
		flusher.Flush()
		return err
	}
}
