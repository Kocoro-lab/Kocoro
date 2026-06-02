package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// countingTextHandler records every OnText invocation (final-answer emissions).
type countingTextHandler struct {
	mockHandler
	mu        sync.Mutex
	textCalls []string
}

func (h *countingTextHandler) OnText(text string) {
	h.mu.Lock()
	h.textCalls = append(h.textCalls, text)
	h.mu.Unlock()
}

// TestAgentLoop_RetractedInject_EndTurnDrainRace reproduces the reviewer's claim:
// a SOLE queued follow-up that the client retracts while the model is composing
// its end_turn reply causes the drain-race guard at the end_turn branch
// (len(injectCh) > 0) to continue into a second iteration. That iteration drains
// the retracted message, filters it out (no new user content), and re-issues the
// LLM call — producing a SECOND end_turn and a SECOND OnText(fullText).
func TestAgentLoop_RetractedInject_EndTurnDrainRace(t *testing.T) {
	const retractedID = "local-retract"
	const retractedText = "drop this cancelled follow-up"

	injectCh := make(chan InjectedMessage, 10)
	var mu sync.Mutex
	callCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()

		if n == 1 {
			// Model composes its final "done" reply. WHILE composing, the user
			// enqueues a follow-up (lands in injectCh AFTER iter-0's top-of-loop
			// drain) and then retracts it. The retract is modeled by the checker
			// below returning true for retractedID; the message itself stays in
			// injectCh because RetractInject only tombstones.
			injectCh <- InjectedMessage{ClientMessageID: retractedID, Text: retractedText}
		}
		// Every call returns end_turn (no tool use). The ONLY thing that can
		// cause a second LLM call here is the drain-race guard.
		_ = json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	loop := NewAgentLoop(gw, reg, "medium", "", 10, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	loop.SetInjectRetractedChecker(func(id string) bool { return id == retractedID })
	h := &countingTextHandler{}
	loop.SetHandler(h)

	if _, _, err := loop.Run(context.Background(), "do work", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	mu.Lock()
	gotCalls := callCount
	mu.Unlock()
	h.mu.Lock()
	gotText := len(h.textCalls)
	h.mu.Unlock()

	t.Logf("LLM calls=%d  OnText emissions=%d  texts=%v", gotCalls, gotText, h.textCalls)
	if gotCalls != 1 {
		t.Errorf("REPRO: expected exactly 1 LLM call for a fully-retracted sole inject, got %d (duplicate paid call)", gotCalls)
	}
	if gotText != 1 {
		t.Errorf("REPRO: expected exactly 1 OnText emission, got %d (duplicate final-answer bubble)", gotText)
	}
}

// TestAgentLoop_InjectedSurvivorCommittedBeforeMaxIterBreak verifies the
// end_turn drain-race guard commits a non-retracted follow-up INLINE, so a
// follow-up enqueued just as the run reaches its iteration cap is still recorded
// (OnInjectedCommitted fires) rather than dropped. Steering injects have no
// mailbox backing to replay, so a stash-then-continue that lands on the maxIter
// break would lose them silently and strand the client's queued-draft card.
func TestAgentLoop_InjectedSurvivorCommittedBeforeMaxIterBreak(t *testing.T) {
	const keptID = "local-keep"

	injectCh := make(chan InjectedMessage, 10)
	var mu sync.Mutex
	callCount := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		callCount++
		n := callCount
		mu.Unlock()
		if n == 1 {
			// Enqueue a non-retracted follow-up while the model composes its
			// end_turn reply — it lands in injectCh after iter 0's top-of-loop drain.
			injectCh <- InjectedMessage{ClientMessageID: keptID, Text: "steer at the cap"}
		}
		_ = json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 10, 5))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	// maxIter=1: the end_turn guard's `continue` lands directly on the maxIter
	// break next iteration, so a stashed (non-inline) survivor would be lost.
	loop := NewAgentLoop(gw, reg, "medium", "", 1, 2000, 200, nil, nil, nil)
	loop.SetInjectCh(injectCh)
	h := &injectCommitRecordingHandler{}
	loop.SetHandler(h)

	// A maxIter break may surface as a benign run-status (not a hard error); we
	// only assert the inline commit fired before the loop exited.
	_, _, _ = loop.Run(context.Background(), "do work", nil, nil)

	if len(h.committedIDs) != 1 || h.committedIDs[0] != keptID {
		t.Errorf("survivor must be committed inline before the maxIter break, got committedIDs=%v", h.committedIDs)
	}
}
