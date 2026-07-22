package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestRunAgent_IdempotencyKeyReturnsCompletedRunWithoutSecondLLMCall(t *testing.T) {
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(client.CompletionResponse{
			Model:        "test-model",
			FinishReason: "end_turn",
			OutputText:   "report delivered",
		})
	}))
	defer gateway.Close()

	deps := runAgentContractTestDeps(t, gateway.URL)
	defer deps.SessionCache.CloseAll()
	req := RunAgentRequest{
		Text: "write the report",
		// heartbeat (not desktop): this test compares gateway call counts, so
		// the source must fire neither the async smart-title upgrade nor the
		// prompt-suggestion fork — both are detached goroutines that hit the
		// same counted gateway and race the assertions (flaked on CI 2026-07-22).
		// The idempotency path itself is source-agnostic.
		Source:         "heartbeat",
		SessionID:      "task-12345678",
		NewSession:     true,
		IdempotencyKey: "deliverable:12345678",
	}

	first, err := RunAgent(context.Background(), deps, req, nullEventHandler{})
	if err != nil {
		t.Fatalf("first RunAgent: %v", err)
	}
	firstCallCount := calls.Load()
	if firstCallCount == 0 {
		t.Fatal("first RunAgent never reached the gateway")
	}
	second, err := RunAgent(context.Background(), deps, req, nullEventHandler{})
	if err != nil {
		t.Fatalf("deduplicated RunAgent: %v", err)
	}
	if got := calls.Load(); got != firstCallCount {
		t.Fatalf("deduplicated request reached gateway: calls %d -> %d", firstCallCount, got)
	}
	if first.SessionID != req.SessionID || second.SessionID != req.SessionID {
		t.Fatalf("session ids = %q / %q, want %q", first.SessionID, second.SessionID, req.SessionID)
	}
	if second.Reply != first.Reply || second.Reply != "report delivered" {
		t.Fatalf("deduplicated reply = %q, first = %q", second.Reply, first.Reply)
	}

	mgr := deps.SessionCache.GetOrCreateManager(deps.SessionCache.SessionsDir(""))
	sess, err := mgr.Load(req.SessionID)
	if err != nil {
		t.Fatalf("load session: %v", err)
	}
	record, ok := sess.IdempotentRequests[req.IdempotencyKey]
	if !ok || record.State != "completed" || record.Reply != "report delivered" {
		t.Fatalf("persisted idempotency record = %+v, present=%t", record, ok)
	}
}

func TestRunAgent_FailedIdempotentRequestNeverReplaysAutomatically(t *testing.T) {
	var calls atomic.Int32
	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.Error(w, "synthetic upstream failure", http.StatusInternalServerError)
	}))
	defer gateway.Close()

	deps := runAgentContractTestDeps(t, gateway.URL)
	defer deps.SessionCache.CloseAll()
	req := RunAgentRequest{
		Text: "write the report",
		// heartbeat, not desktop — same call-count-race rationale as the
		// dedup test above.
		Source:         "heartbeat",
		SessionID:      "task-87654321",
		NewSession:     true,
		IdempotencyKey: "deliverable:87654321",
	}

	if _, err := RunAgent(context.Background(), deps, req, nullEventHandler{}); err == nil {
		t.Fatal("first RunAgent unexpectedly succeeded")
	}
	firstCallCount := calls.Load()
	if firstCallCount == 0 {
		t.Fatal("first RunAgent never reached the gateway")
	}
	if _, err := RunAgent(context.Background(), deps, req, nullEventHandler{}); !errors.Is(err, ErrIdempotentRequestFailed) {
		t.Fatalf("second RunAgent error = %v, want ErrIdempotentRequestFailed", err)
	}
	if got := calls.Load(); got != firstCallCount {
		t.Fatalf("failed request replayed: gateway calls %d -> %d", firstCallCount, got)
	}
}

func TestRunAgentRequestValidateRequiresSessionForIdempotency(t *testing.T) {
	err := (&RunAgentRequest{
		Text:           "write notes",
		IdempotencyKey: "deliverable:12345678",
	}).Validate()
	if err == nil {
		t.Fatal("Validate accepted idempotency_key without session_id")
	}
}

func TestRunAgentRequestValidateRejectsUnsafeIdempotencyKey(t *testing.T) {
	err := (&RunAgentRequest{
		Text:           "write notes",
		SessionID:      "task-12345678",
		IdempotencyKey: "deliverable/../../secret",
	}).Validate()
	if err == nil {
		t.Fatal("Validate accepted unsafe idempotency_key")
	}
}

func TestRunAgentRejectsUnboundIdempotencyKeyAtDirectCallSeam(t *testing.T) {
	deps := runAgentContractTestDeps(t, "http://127.0.0.1:1")
	defer deps.SessionCache.CloseAll()

	_, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:           "write notes",
		IdempotencyKey: "deliverable:12345678",
	}, nullEventHandler{})
	if err == nil {
		t.Fatal("RunAgent accepted idempotency_key without session_id")
	}
}

func TestTerminalIdempotencyState_SoftFailureWithoutDeliverableFailsClosed(t *testing.T) {
	if got := terminalIdempotencyState(true, context.Canceled, 0); got != idempotentRequestFailed {
		t.Fatalf("terminal state = %q, want %q", got, idempotentRequestFailed)
	}
}

func TestTerminalIdempotencyState_DeliverableIsDurableSuccessEvidence(t *testing.T) {
	if got := terminalIdempotencyState(true, context.Canceled, 1); got != idempotentRequestCompleted {
		t.Fatalf("terminal state = %q, want %q", got, idempotentRequestCompleted)
	}
}

func TestCompletedIdempotentResultReplaysDeliveryReceiptAndStatus(t *testing.T) {
	sess := &session.Session{
		ID: "task-12345678",
		IdempotentRequests: map[string]session.IdempotentRequest{
			"deliverable:12345678": {
				State:       idempotentRequestCompleted,
				Reply:       "",
				Partial:     true,
				FailureCode: string(runstatus.CodeIterationLimit),
				Deliverables: []session.DeliverableReceipt{{
					ID: "dlv_1234", Path: "/tmp/report.md", Filename: "report.md",
					MIME: "text/markdown", ByteSize: 42,
				}},
			},
		},
	}

	result, err, found := completedIdempotentResult(sess, "deliverable:12345678", "")
	if err != nil || !found {
		t.Fatalf("completedIdempotentResult = (%+v, %v, %t)", result, err, found)
	}
	if !result.Partial || result.FailureCode != runstatus.CodeIterationLimit {
		t.Fatalf("status not replayed: partial=%t failure=%q", result.Partial, result.FailureCode)
	}
	if len(result.Deliverables) != 1 || result.Deliverables[0].ByteSize != 42 {
		t.Fatalf("deliverables = %+v", result.Deliverables)
	}
}
