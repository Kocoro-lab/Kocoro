//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestToolLoopBudgetContinuationAndClosure(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	loop.bindResponse("initial", responsePurposeUser, 1)
	first := loop.claimAction("initial", "do_task")
	second := loop.claimAction("initial", "do_task")
	if !first.allowed || !second.allowed || first.sameResponseDoTaskCall || !second.sameResponseDoTaskCall {
		t.Fatalf("same-response do_task accounting wrong: first=%+v second=%+v", first, second)
	}
	if decision, turnID := loop.finishResponse("initial"); decision != toolLoopContinue || turnID != 1 {
		t.Fatalf("initial finish=(%v,%d), want continuation for turn 1", decision, turnID)
	}

	loop.bindResponse("continued", responsePurposeContinuation, 1)
	if claim := loop.claimAction("continued", "cancel"); !claim.allowed {
		t.Fatalf("third action denied: %+v", claim)
	}
	if claim := loop.claimAction("continued", "do_task"); !claim.allowed {
		t.Fatalf("fourth action denied: %+v", claim)
	}
	if claim := loop.claimAction("continued", "get_status"); claim.allowed || claim.reason != "turn_action_budget_exhausted" {
		t.Fatalf("fifth action must be rejected: %+v", claim)
	}
	if decision, _ := loop.finishResponse("continued"); decision != toolLoopClose {
		t.Fatalf("budget exhaustion decision=%v, want tools-disabled closure", decision)
	}
}

func TestToolLoopNewUserTurnPreemptsContinuation(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	loop.bindResponse("old", responsePurposeUser, 1)
	if claim := loop.claimAction("old", "do_task"); !claim.allowed {
		t.Fatalf("initial action denied: %+v", claim)
	}
	loop.noteUserCommit(2)
	if decision, _ := loop.finishResponse("old"); decision != toolLoopNone {
		t.Fatalf("preempted response scheduled decision=%v", decision)
	}
	if claim := loop.claimAction("old", "cancel"); claim.allowed || claim.reason != "turn_preempted" {
		t.Fatalf("old response kept authority after preemption: %+v", claim)
	}
}

func TestToolLoopSyntheticResponsesCannotCallTools(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	loop.bindResponse("result", responsePurposeTaskResult, 1)
	claim := loop.claimAction("result", "do_task")
	if claim.allowed || claim.reason != "response_has_no_tool_capability" {
		t.Fatalf("task result response acquired tool authority: %+v", claim)
	}
}

func TestToolLoopOneMessageFuse(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	loop.bindResponse("response", responsePurposeUser, 1)
	if loop.noteMessageItem("response", "message") {
		t.Fatal("first message item tripped fuse")
	}
	if !loop.noteMessageItem("response", "message") {
		t.Fatal("second message item must trip fuse")
	}
	if loop.noteMessageItem("response", "message") {
		t.Fatal("fuse must trigger at most once")
	}
}

func TestToolLoopOneMessageFuseCancelsRepeatedAudio(t *testing.T) {
	state := NewCallState("burst-fuse", "")
	dispatcher := NewDispatcher(NewDaemonClient(""), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)
	var mu sync.Mutex
	var sent []string
	h := newEventHandler(dispatcher, state, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		var frame struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(payload, &frame)
		mu.Lock()
		sent = append(sent, frame.Type)
		mu.Unlock()
		return nil
	})

	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.committed"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"fused"}}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.output_item.added","response_id":"fused","item":{"type":"message"}}`))
	mu.Lock()
	if countEventType(sent, "response.cancel") != 0 {
		mu.Unlock()
		t.Fatal("first assistant message cancelled the response")
	}
	mu.Unlock()

	h.handleEvent(context.Background(), []byte(`{"type":"response.output_item.added","response_id":"fused","item":{"type":"message"}}`))
	mu.Lock()
	defer mu.Unlock()
	if got := countEventType(sent, "response.cancel"); got != 1 {
		t.Fatalf("repeated assistant message response.cancel count=%d, want 1; sent=%v", got, sent)
	}
}

func countEventType(events []string, want string) int {
	count := 0
	for _, event := range events {
		if event == want {
			count++
		}
	}
	return count
}

func TestToolLoopCancelsAndStartsTaskInSameResponse(t *testing.T) {
	t.Setenv("KOE_TASK_LEDGER", "1")
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	cancels := make(chan CancelRequest, 1)
	posts := make(chan DoTaskRequest, 1)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/cancel":
			var req CancelRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			cancels <- req
			w.WriteHeader(http.StatusNoContent)
		case "/message":
			var req DoTaskRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			posts <- req
			<-release
			_ = json.NewEncoder(w).Encode(map[string]any{
				"reply": "new task done", "spoken_summary": "new task done",
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer func() {
		releaseAll()
		mock.Close()
	}()

	state := NewCallState("burst-compound", "")
	old := state.BeginTask("old task", "")
	dispatcher := NewDispatcher(NewDaemonClient(mock.URL), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)
	h := newEventHandlerWithMailbox(dispatcher, state, nil, func(any) error { return nil }, NewResultMailbox(), nil)
	ctx := context.Background()
	h.handleEvent(ctx, []byte(`{"type":"input_audio_buffer.committed"}`))
	h.handleEvent(ctx, []byte(`{"type":"response.created","response":{"id":"compound"}}`))
	h.handleEvent(ctx, []byte(`{"type":"response.function_call_arguments.done","response_id":"compound","call_id":"cancel-old","name":"cancel","arguments":"{\"task_id\":\"`+old.ID+`\"}"}`))
	h.handleEvent(ctx, []byte(`{"type":"response.function_call_arguments.done","response_id":"compound","call_id":"start-new","name":"do_task","arguments":"{\"task\":\"new task\",\"relationship\":\"new\"}"}`))

	select {
	case req := <-cancels:
		if req.RouteKey != routeKeyFor(old.Agent, old.ThreadID) {
			t.Fatalf("cancel route=%q, want %q", req.RouteKey, routeKeyFor(old.Agent, old.ThreadID))
		}
	case <-time.After(time.Second):
		t.Fatal("cancel did not reach daemon")
	}
	started := waitDoTaskPost(t, posts)
	if started.Text != "new task" || started.ThreadID != state.BurstID() {
		t.Fatalf("new task request=%+v", started)
	}
	oldAfter, _ := state.TaskByID(old.ID)
	newTask, ok := state.TaskByID("t02")
	if oldAfter.State != TaskCancelled || !ok || newTask.State != TaskRunning {
		t.Fatalf("compound transition old=%+v new=%+v exists=%t", oldAfter, newTask, ok)
	}
	releaseAll()
}

func TestToolLoopAggregatesCallsIntoOneContinuation(t *testing.T) {
	state := NewCallState("burst-loop", "")
	dispatcher := NewDispatcher(NewDaemonClient(""), NewAgentResolver(fixtureAgents(), NoopSemanticMatcher{}), state, nil)
	var mu sync.Mutex
	creates := 0
	var continuationPayload map[string]any
	var h *eventHandler
	h = newEventHandler(dispatcher, state, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		var frame map[string]any
		_ = json.Unmarshal(payload, &frame)
		if frame["type"] == "response.create" {
			mu.Lock()
			creates++
			continuationPayload = frame
			mu.Unlock()
			h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"continued"}}`))
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)

	h.handleEvent(ctx, []byte(`{"type":"input_audio_buffer.committed"}`))
	h.handleEvent(ctx, []byte(`{"type":"response.created","response":{"id":"initial"}}`))
	h.handleEvent(ctx, []byte(`{"type":"response.function_call_arguments.done","response_id":"initial","call_id":"status","name":"get_status","arguments":"{}"}`))
	h.handleEvent(ctx, []byte(`{"type":"response.function_call_arguments.done","response_id":"initial","call_id":"agent","name":"switch_agent","arguments":"{\"agent\":\"finance\"}"}`))
	time.Sleep(50 * time.Millisecond)
	mu.Lock()
	beforeDone := creates
	mu.Unlock()
	if beforeDone != 0 {
		t.Fatalf("per-tool response.create regression: creates before response.done=%d", beforeDone)
	}
	h.handleEvent(ctx, []byte(`{"type":"response.done","response":{"id":"initial","status":"completed"}}`))

	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		count := creates
		mu.Unlock()
		if count == 1 {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if creates != 1 {
		t.Fatalf("aggregated continuation creates=%d, want 1", creates)
	}
	response, _ := continuationPayload["response"].(map[string]any)
	if response["tool_choice"] != "auto" {
		t.Fatalf("continuation is not tools-enabled: %#v", response)
	}
}

func TestToolLoopBudgetRejectsFifthSideEffectAndClosesWithoutTools(t *testing.T) {
	state := NewCallState("burst-budget", "")
	var controlCalls int
	dispatcher := NewDispatcher(NewDaemonClient(""), NewAgentResolver(nil, NoopSemanticMatcher{}), state, func(context.Context, string) error {
		controlCalls++
		return nil
	})
	closure := make(chan map[string]any, 1)
	var h *eventHandler
	h = newEventHandler(dispatcher, state, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		var frame map[string]any
		_ = json.Unmarshal(payload, &frame)
		if frame["type"] == "response.create" {
			closure <- frame
			h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"closure"}}`))
		}
		return nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runResponseSender(ctx)
	h.handleEvent(ctx, []byte(`{"type":"input_audio_buffer.committed"}`))
	h.handleEvent(ctx, []byte(`{"type":"response.created","response":{"id":"budget"}}`))
	for i := 0; i < 5; i++ {
		event := `{"type":"response.function_call_arguments.done","response_id":"budget","call_id":"c` + string(rune('0'+i)) + `","name":"control_app","arguments":"{\"action\":\"show\"}"}`
		h.handleEvent(ctx, []byte(event))
	}
	if controlCalls != 4 {
		t.Fatalf("executed side effects=%d, want budget cap 4", controlCalls)
	}
	h.handleEvent(ctx, []byte(`{"type":"response.done","response":{"id":"budget","status":"completed"}}`))
	select {
	case frame := <-closure:
		response, _ := frame["response"].(map[string]any)
		if _, exists := response["tool_choice"]; exists || !strings.Contains(response["instructions"].(string), "not executed") {
			t.Fatalf("invalid budget closure: %#v", response)
		}
		if tools, ok := response["tools"].([]any); !ok || len(tools) != 0 {
			t.Fatalf("closure tools must be empty: %#v", response["tools"])
		}
	case <-time.After(time.Second):
		t.Fatal("budget closure was not created")
	}
}
