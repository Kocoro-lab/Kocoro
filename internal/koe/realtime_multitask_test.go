//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

func TestDoTaskImmediateAckAndParallelLanes(t *testing.T) {
	t.Setenv("KOE_TASK_LEDGER", "1")
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	posts := make(chan DoTaskRequest, 2)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req DoTaskRequest
		_ = json.NewDecoder(r.Body).Decode(&req)
		posts <- req
		<-release
		_ = json.NewEncoder(w).Encode(map[string]any{
			"reply": "result for " + req.Text, "spoken_summary": "result for " + req.Text,
		})
	}))
	defer func() {
		releaseAll()
		mock.Close()
	}()

	state := NewCallState("burst-m", "")
	dispatcher := NewDispatcher(NewDaemonClient(mock.URL), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)
	mailbox := NewResultMailbox()
	var mu sync.Mutex
	var outputs []SayResult
	h := newEventHandlerWithMailbox(dispatcher, state, nil, func(v any) error {
		payload, _ := json.Marshal(v)
		var frame struct {
			Type string `json:"type"`
			Item struct {
				Type   string `json:"type"`
				Output string `json:"output"`
			} `json:"item"`
		}
		_ = json.Unmarshal(payload, &frame)
		mu.Lock()
		defer mu.Unlock()
		switch {
		case frame.Type == "conversation.item.create" && frame.Item.Type == "function_call_output":
			var result SayResult
			_ = json.Unmarshal([]byte(frame.Item.Output), &result)
			outputs = append(outputs, result)
		}
		return nil
	}, mailbox, nil)

	h.handleFunctionCall(context.Background(), "call-a", "do_task", []byte(`{"task":"check Tokyo weather","relationship":"new"}`))
	h.handleFunctionCall(context.Background(), "call-b", "do_task", []byte(`{"task":"sort unread email","relationship":"new"}`))

	first := waitDoTaskPost(t, posts)
	second := waitDoTaskPost(t, posts)
	if first.ThreadID == second.ThreadID {
		t.Fatalf("parallel independent tasks shared lane %q", first.ThreadID)
	}
	mu.Lock()
	if len(outputs) != 2 || outputs[0].Status != "running" || outputs[1].Status != "running" ||
		outputs[0].TaskID == outputs[1].TaskID {
		mu.Unlock()
		t.Fatalf("immediate running acks wrong: %+v", outputs)
	}
	mu.Unlock()

	releaseAll()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		outputCount := len(outputs)
		mu.Unlock()
		if mailbox.pending() == 2 {
			if outputCount != 2 {
				t.Fatalf("final results must not reuse consumed call ids: outputs=%d", outputCount)
			}
			mailbox.mu.Lock()
			firstReply := mailbox.entries[0].result.Reply
			secondReply := mailbox.entries[1].result.Reply
			mailbox.mu.Unlock()
			if firstReply == "" || secondReply == "" || firstReply == secondReply {
				t.Fatalf("complete parallel results missing: %q / %q", firstReply, secondReply)
			}
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("results did not land: mailbox=%d", mailbox.pending())
}

func waitDoTaskPost(t *testing.T, posts <-chan DoTaskRequest) DoTaskRequest {
	t.Helper()
	select {
	case req := <-posts:
		return req
	case <-time.After(time.Second):
		t.Fatal("do_task did not reach daemon")
		return DoTaskRequest{}
	}
}
