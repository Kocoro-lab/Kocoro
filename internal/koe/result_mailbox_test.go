//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestResultMailboxRetainsUntilCompleted(t *testing.T) {
	m := NewResultMailbox()
	first := m.Enqueue("task-a", "Tokyo will have rain.", false, false)
	second := m.Enqueue("task-b", "Three messages need replies.", true, false)
	if first == 0 || second <= first {
		t.Fatalf("entry ids must be non-zero and ordered: first=%d second=%d", first, second)
	}

	claimed := m.claim("connection-a")
	if len(claimed) != 2 {
		t.Fatalf("claimed=%d, want 2", len(claimed))
	}
	if got := m.pending(); got != 2 {
		t.Fatalf("response.created must not remove entries: pending=%d, want 2", got)
	}
	if got := m.complete("connection-a"); got != 2 {
		t.Fatalf("completed=%d, want 2", got)
	}
	if got := m.pending(); got != 0 {
		t.Fatalf("pending=%d after response.done, want 0", got)
	}
}

func TestResultMailboxReleasesAcrossConnectionTeardown(t *testing.T) {
	m := NewResultMailbox()
	m.Enqueue("task-a", "Done.", false, false)
	if got := len(m.claim("old-connection")); got != 1 {
		t.Fatalf("old connection claimed=%d, want 1", got)
	}
	if got := m.release("old-connection"); got != 1 {
		t.Fatalf("released=%d, want 1", got)
	}

	claimed := m.claim("new-connection")
	if len(claimed) != 1 || claimed[0].taskID != "task-a" || claimed[0].text != "Done." {
		t.Fatalf("new connection did not recover result: %+v", claimed)
	}
}

func TestResultMailboxWakeCoalescesWithoutDroppingEntries(t *testing.T) {
	m := NewResultMailbox()
	for i := 0; i < 32; i++ {
		m.Enqueue("task", "result", false, false)
	}
	select {
	case <-m.notifications():
	default:
		t.Fatal("enqueue must wake a sender")
	}
	if got := len(m.claim("connection")); got != 32 {
		t.Fatalf("claimed=%d, want 32 despite one coalesced wake", got)
	}
}

func TestResultDeliverySurvivesRealtimeTeardown(t *testing.T) {
	m := NewResultMailbox()
	firstCreate := make(chan struct{}, 1)
	h1 := newEventHandlerWithMailbox(nil, nil, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		if strings.Contains(string(payload), `"type":"response.create"`) {
			signalNonBlocking(firstCreate)
		}
		return nil
	}, m, nil)
	ctx1, cancel1 := context.WithCancel(context.Background())
	go h1.runResponseSender(ctx1)
	m.Enqueue("task-a", "The task is complete.", false, false)

	select {
	case <-firstCreate:
	case <-time.After(time.Second):
		t.Fatal("old connection never attempted result delivery")
	}
	cancel1() // no response.created: the old connection disappears mid-delivery
	waitForMailboxOwner(t, m, "", time.Second)

	secondCreate := make(chan string, 1)
	var h2 *eventHandler
	h2 = newEventHandlerWithMailbox(nil, nil, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		var frame struct {
			Type     string `json:"type"`
			Response struct {
				Instructions string `json:"instructions"`
			} `json:"response"`
		}
		_ = json.Unmarshal(payload, &frame)
		if frame.Type == "response.create" {
			secondCreate <- frame.Response.Instructions
			h2.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"result-response","status":"in_progress"}}`))
		}
		return nil
	}, m, nil)
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	go h2.runResponseSender(ctx2)

	select {
	case instructions := <-secondCreate:
		if !strings.Contains(instructions, "The task is complete.") {
			t.Fatalf("recovered delivery lost result text: %q", instructions)
		}
	case <-time.After(time.Second):
		t.Fatal("new connection did not recover pending result")
	}
	if got := m.pending(); got != 1 {
		t.Fatalf("response.created removed result: pending=%d, want 1", got)
	}
	h2.handleEvent(context.Background(), []byte(`{"type":"response.done","response":{"id":"result-response","status":"completed"}}`))
	if got := m.pending(); got != 0 {
		t.Fatalf("completed response.done did not ack result: pending=%d", got)
	}
}

func TestResultDeliveryWaitsForActiveCallAndUserFloor(t *testing.T) {
	m := NewResultMailbox()
	var active atomic.Bool
	creates := make(chan struct{}, 1)
	var h *eventHandler
	h = newEventHandlerWithMailbox(nil, nil, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		if strings.Contains(string(payload), `"type":"response.create"`) {
			signalNonBlocking(creates)
			h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"result-response"}}`))
		}
		return nil
	}, m, active.Load)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	h.userSpeaking.Store(true)
	m.Enqueue("task-a", "Done.", false, false)
	select {
	case <-creates:
		t.Fatal("inactive call must not announce a pending result")
	case <-time.After(80 * time.Millisecond):
	}

	active.Store(true)
	m.Wake()
	select {
	case <-creates:
		t.Fatal("result must not take the floor while the user is speaking")
	case <-time.After(80 * time.Millisecond):
	}

	h.userSpeaking.Store(false)
	m.Wake()
	select {
	case <-creates:
	case <-time.After(time.Second):
		t.Fatal("result was not announced after the user yielded")
	}
}

func TestDoTaskResultUsesMailboxAfterUserMovesOn(t *testing.T) {
	release := make(chan struct{})
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/message" {
			<-release
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"reply": "Done, reminder set.", "spoken_summary": "Done, reminder set.",
		})
	}))
	defer mock.Close()

	mailbox := NewResultMailbox()
	state := NewCallState("burst-mailbox", "")
	disp := NewDispatcher(NewDaemonClient(mock.URL), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)
	var functionOutputs atomic.Int32
	h := newEventHandlerWithMailbox(disp, state, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		if strings.Contains(string(payload), `"type":"function_call_output"`) {
			functionOutputs.Add(1)
		}
		return nil
	}, mailbox, nil)

	h.handleFunctionCall(context.Background(), "call-mailbox", "do_task", []byte(`{"task":"set a reminder"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.committed"}`))
	close(release)

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && mailbox.pending() == 0 {
		time.Sleep(5 * time.Millisecond)
	}
	if functionOutputs.Load() != 1 {
		t.Fatalf("function_call_output count=%d, want 1", functionOutputs.Load())
	}
	mailbox.mu.Lock()
	defer mailbox.mu.Unlock()
	if len(mailbox.entries) != 1 {
		t.Fatalf("mailbox entries=%d, want 1", len(mailbox.entries))
	}
	entry := mailbox.entries[0]
	if entry.taskID != "call-mailbox" || entry.text != "Done, reminder set." || !entry.resumptive {
		t.Fatalf("unexpected mailbox entry: %+v", entry)
	}
}

func waitForMailboxOwner(t *testing.T, m *ResultMailbox, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		m.mu.Lock()
		owner := ""
		if len(m.entries) > 0 {
			owner = m.entries[0].owner
		}
		m.mu.Unlock()
		if owner == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("mailbox owner did not become %q", want)
}
