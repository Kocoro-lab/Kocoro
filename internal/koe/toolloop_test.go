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
	first := loop.claimAction("initial", "call-1", "do_task", []byte(`{"task":"weather"}`))
	second := loop.claimAction("initial", "call-2", "do_task", []byte(`{"task":"news"}`))
	if !first.allowed || !second.allowed || first.sameResponseDoTaskCall || !second.sameResponseDoTaskCall {
		t.Fatalf("same-response do_task accounting wrong: first=%+v second=%+v", first, second)
	}
	if decision, turnID := loop.finishResponse("initial"); decision != toolLoopContinue || turnID != 1 {
		t.Fatalf("initial finish=(%v,%d), want continuation for turn 1", decision, turnID)
	}

	loop.bindResponse("continued", responsePurposeContinuation, 1)
	if claim := loop.claimAction("continued", "call-3", "cancel", nil); !claim.allowed {
		t.Fatalf("third action denied: %+v", claim)
	}
	if claim := loop.claimAction("continued", "call-4", "do_task", nil); !claim.allowed {
		t.Fatalf("fourth action denied: %+v", claim)
	}
	if claim := loop.claimAction("continued", "call-5", "get_status", nil); claim.allowed || claim.reason != "turn_action_budget_exhausted" {
		t.Fatalf("fifth action must be rejected: %+v", claim)
	}
	if decision, _ := loop.finishResponse("continued"); decision != toolLoopClose {
		t.Fatalf("budget exhaustion decision=%v, want tools-disabled closure", decision)
	}
}

func TestToolLoopSkipsContinuationForDeferredDoTasks(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	loop.bindResponse("initial", responsePurposeUser, 1)
	for i, callID := range []string{"call-1", "call-2"} {
		args := []byte(`{"task":"task-` + string(rune('a'+i)) + `"}`)
		if claim := loop.claimAction("initial", callID, "do_task", args); !claim.allowed {
			t.Fatalf("do_task %s denied: %+v", callID, claim)
		}
		loop.noteDeferredDoTask("initial")
	}
	if decision, turnID := loop.finishResponse("initial"); decision != toolLoopNone || turnID != 1 {
		t.Fatalf("deferred-only finish=(%v,%d), want no continuation for turn 1", decision, turnID)
	}
	if claim := loop.claimAction("initial", "call-3", "do_task", nil); claim.allowed || claim.reason != "turn_preempted" {
		t.Fatalf("deferred-only response did not close turn: %+v", claim)
	}
}

func TestToolLoopNewUserTurnPreemptsContinuation(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	loop.bindResponse("old", responsePurposeUser, 1)
	if claim := loop.claimAction("old", "call-1", "do_task", nil); !claim.allowed {
		t.Fatalf("initial action denied: %+v", claim)
	}
	loop.noteUserCommit(2)
	if decision, _ := loop.finishResponse("old"); decision != toolLoopNone {
		t.Fatalf("preempted response scheduled decision=%v", decision)
	}
	if claim := loop.claimAction("old", "call-2", "cancel", nil); claim.allowed || claim.reason != "turn_preempted" {
		t.Fatalf("old response kept authority after preemption: %+v", claim)
	}
}

func TestToolLoopSyntheticResponsesCannotCallTools(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	loop.bindResponse("result", responsePurposeTaskResult, 1)
	claim := loop.claimAction("result", "call-1", "do_task", nil)
	if claim.allowed || claim.reason != "response_has_no_tool_capability" {
		t.Fatalf("task result response acquired tool authority: %+v", claim)
	}
}

func TestToolLoopDeduplicatesCallIDWithoutSpendingBudget(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	loop.bindResponse("response", responsePurposeUser, 1)
	first := loop.claimAction("response", "same-call", "control_app", []byte(`{"action":"show"}`))
	duplicate := loop.claimAction("response", "same-call", "control_app", []byte(`{"action":"hide"}`))
	if !first.allowed || !duplicate.duplicate || duplicate.allowed || duplicate.reason != "duplicate_tool_event" {
		t.Fatalf("dedup claims first=%+v duplicate=%+v", first, duplicate)
	}
	for i := 0; i < maxTurnTaskActions-1; i++ {
		claim := loop.claimAction("response", "extra-"+string(rune('a'+i)), "get_status", nil)
		if !claim.allowed {
			t.Fatalf("duplicate consumed action budget at extra %d: %+v", i, claim)
		}
	}
}

func TestToolLoopDeduplicatesCanonicalActionAcrossCallIDs(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	loop.bindResponse("response", responsePurposeUser, 1)
	first := loop.claimAction("response", "call-1", "do_task", []byte(`{"task":"weather and news","relationship":"new"}`))
	duplicate := loop.claimAction("response", "call-2", "do_task", []byte(`{"relationship":"new","task":"weather and news"}`))
	distinct := loop.claimAction("response", "call-3", "do_task", []byte(`{"task":"weather only","relationship":"new"}`))
	if !first.allowed || !distinct.allowed {
		t.Fatalf("distinct actions were denied: first=%+v distinct=%+v", first, distinct)
	}
	if duplicate.allowed || !duplicate.duplicate || !duplicate.duplicateAction || duplicate.reason != "duplicate_tool_action" {
		t.Fatalf("canonical duplicate was not rejected: %+v", duplicate)
	}
}

func TestToolLoopUnknownResponseHasNoAuthority(t *testing.T) {
	loop := newToolLoopLedger()
	loop.noteUserCommit(1)
	claim := loop.claimAction("unbound", "call-1", "do_task", nil)
	if claim.allowed || claim.known || claim.reason != "unknown_response" {
		t.Fatalf("unbound response acquired authority: %+v", claim)
	}
}

func TestToolLoopDuplicateWireEventExecutesSideEffectOnce(t *testing.T) {
	state := NewCallState("burst-dedup", "")
	controlCalls := 0
	dispatcher := NewDispatcher(NewDaemonClient(""), NewAgentResolver(nil, NoopSemanticMatcher{}), state, func(context.Context, string) error {
		controlCalls++
		return nil
	})
	functionOutputs := 0
	h := newEventHandler(dispatcher, state, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		var frame struct {
			Type string `json:"type"`
			Item struct {
				Type string `json:"type"`
			} `json:"item"`
		}
		_ = json.Unmarshal(payload, &frame)
		if frame.Type == "conversation.item.create" && frame.Item.Type == "function_call_output" {
			functionOutputs++
		}
		return nil
	})
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.committed"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"dedup"}}`))
	event := []byte(`{"type":"response.function_call_arguments.done","response_id":"dedup","call_id":"same-call","name":"control_app","arguments":"{\"action\":\"show\"}"}`)
	h.handleEvent(context.Background(), event)
	h.handleEvent(context.Background(), event)
	if controlCalls != 1 || functionOutputs != 1 {
		t.Fatalf("duplicate event replayed: side_effects=%d function_outputs=%d", controlCalls, functionOutputs)
	}
}

func TestToolLoopDuplicateDoTaskActionCreatesOneTask(t *testing.T) {
	posts := make(chan struct{}, 2)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		posts <- struct{}{}
		_ = json.NewEncoder(w).Encode(map[string]any{"reply": "done", "spoken_summary": "done"})
	}))
	defer mock.Close()

	state := NewCallState("burst-action-dedup", "")
	dispatcher := NewDispatcher(NewDaemonClient(mock.URL), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)
	var mu sync.Mutex
	var outputs []map[string]any
	h := newEventHandler(dispatcher, state, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		var frame struct {
			Type string `json:"type"`
			Item struct {
				Type   string `json:"type"`
				Output string `json:"output"`
			} `json:"item"`
		}
		_ = json.Unmarshal(payload, &frame)
		if frame.Type == "conversation.item.create" && frame.Item.Type == "function_call_output" {
			var output map[string]any
			_ = json.Unmarshal([]byte(frame.Item.Output), &output)
			mu.Lock()
			outputs = append(outputs, output)
			mu.Unlock()
		}
		return nil
	})
	h.handleEvent(context.Background(), []byte(`{"type":"input_audio_buffer.committed"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.created","response":{"id":"action-dedup"}}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.function_call_arguments.done","response_id":"action-dedup","call_id":"call-1","name":"do_task","arguments":"{\"task\":\"weather and news\",\"relationship\":\"new\"}"}`))
	h.handleEvent(context.Background(), []byte(`{"type":"response.function_call_arguments.done","response_id":"action-dedup","call_id":"call-2","name":"do_task","arguments":"{\"relationship\":\"new\",\"task\":\"weather and news\"}"}`))

	select {
	case <-posts:
	case <-time.After(time.Second):
		t.Fatal("accepted do_task never reached backend")
	}
	select {
	case <-posts:
		t.Fatal("canonical duplicate reached backend")
	case <-time.After(100 * time.Millisecond):
	}
	if tasks := state.AllTasks(); len(tasks) != 1 {
		t.Fatalf("duplicate action created %d tasks, want 1: %+v", len(tasks), tasks)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(outputs) != 2 || outputs[0]["status"] != "running" || outputs[1]["error_code"] != "duplicate_tool_action" {
		t.Fatalf("function outputs=%+v, want running then duplicate_tool_action", outputs)
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
			h.handleEvent(context.Background(), responseCreatedForRequest("continued", v))
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
	if response["tool_choice"] != "auto" || response["parallel_tool_calls"] != true {
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
			h.handleEvent(context.Background(), responseCreatedForRequest("closure", v))
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
