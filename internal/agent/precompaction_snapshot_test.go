package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestAgentLoop_ProactiveCompactionSnapshotsHistory pins the gap #6 hook: an
// APPLIED proactive compaction must hand the full pre-replacement history to
// the injected snapshotter before ShapeHistory's result is swapped in — the
// snapshot is the only rollback material once the middle is dropped.
func TestAgentLoop_ProactiveCompactionSnapshotsHistory(t *testing.T) {
	history := make([]client.Message, 0, 60)
	for i := 0; i < 30; i++ {
		history = append(history,
			client.Message{Role: "user", Content: client.NewTextContent(fmt.Sprintf("step request %d %s", i, strings.Repeat("filler words to fatten the turn ", 20)))},
			client.Message{Role: "assistant", Content: client.NewTextContent(fmt.Sprintf("step reply %d %s", i, strings.Repeat("acknowledged and completed nicely ", 20)))},
		)
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()
		var req struct {
			ModelTier string `json:"model_tier"`
		}
		json.Unmarshal(raw, &req)
		if req.ModelTier == "small" {
			json.NewEncoder(w).Encode(nativeResponse(
				"<summary>## Current task & next steps\ncontinue the steps</summary>", "end_turn", nil, 50, 30))
			return
		}
		json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 100, 10))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindowExplicit(60_000)
	loop.SetMemoryDir(t.TempDir())
	loop.SetHandler(&mockHandler{approveResult: true})
	loop.SetSessionID("snapshot-proactive")
	fp := loop.ToolsFingerprint()
	loop.SetEstOverheadState(43_000, "test-model", fp)

	var mu sync.Mutex
	var phases []string
	var capturedLen int
	capturedMiddle := false
	loop.SetPreCompactionSnapshot(func(phase string, msgs []client.Message) {
		mu.Lock()
		defer mu.Unlock()
		phases = append(phases, phase)
		capturedLen = len(msgs)
		for _, m := range msgs {
			if strings.Contains(m.Content.Text(), "step request 5 ") {
				capturedMiddle = true
			}
		}
	})

	if _, _, err := loop.Run(context.Background(), "continue the work", nil, history); err != nil {
		t.Logf("Run error (tolerated): %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(phases) != 1 || phases[0] != "proactive" {
		t.Fatalf("expected exactly one proactive snapshot, got %v", phases)
	}
	if !capturedMiddle {
		t.Error("snapshot must contain the droppable middle turns")
	}
	if capturedLen < len(history) {
		t.Errorf("snapshot should carry the full pre-compaction history: got %d < %d", capturedLen, len(history))
	}
}

// TestAgentLoop_ReactiveCompactionSnapshotsHistory: the reactive (post-400)
// path replaces history too — it must snapshot with its own phase tag.
func TestAgentLoop_ReactiveCompactionSnapshotsHistory(t *testing.T) {
	var mu sync.Mutex
	firstErrorMsgCount := 0

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := readBody(r.Body)
		defer r.Body.Close()
		var req struct {
			ModelTier string `json:"model_tier"`
			Messages  []struct {
				Role string `json:"role"`
			} `json:"messages"`
		}
		json.Unmarshal(raw, &req)

		if req.ModelTier == "small" {
			json.NewEncoder(w).Encode(nativeResponse(
				"## Current task & next steps\nsummary of prior steps", "end_turn", nil, 50, 30))
			return
		}

		mu.Lock()
		msgCount := len(req.Messages)
		if msgCount < 12 && firstErrorMsgCount == 0 {
			mu.Unlock()
			json.NewEncoder(w).Encode(nativeResponse(
				"", "tool_use",
				toolCall("think", fmt.Sprintf(`{"thought":"step %d"}`, msgCount)),
				50, 20))
			return
		}
		if firstErrorMsgCount == 0 || msgCount >= firstErrorMsgCount {
			if firstErrorMsgCount == 0 {
				firstErrorMsgCount = msgCount
			}
			mu.Unlock()
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(`{"error":{"type":"invalid_request_error","message":"prompt is too long"}}`))
			return
		}
		mu.Unlock()
		json.NewEncoder(w).Encode(nativeResponse("done after shaped retry", "end_turn", nil, 100, 20))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindowExplicit(200_000)
	loop.SetMemoryDir(t.TempDir())
	loop.SetHandler(&mockHandler{approveResult: true})

	var pmu sync.Mutex
	var phases []string
	loop.SetPreCompactionSnapshot(func(phase string, msgs []client.Message) {
		pmu.Lock()
		phases = append(phases, phase)
		pmu.Unlock()
	})

	if _, _, err := loop.Run(context.Background(), "run the steps", nil, nil); err != nil {
		t.Errorf("Run should recover through the shaped retry: %v", err)
	}

	pmu.Lock()
	defer pmu.Unlock()
	if len(phases) != 1 || phases[0] != "reactive" {
		t.Fatalf("expected exactly one reactive snapshot, got %v", phases)
	}
}

// TestAgentLoop_SwitchAgentResetsSnapshotter: a snapshotter closure captures a
// session id at wiring time; agent/session switches must drop it so a stale
// closure can never write snapshots under the wrong session.
func TestAgentLoop_SwitchAgentResetsSnapshotter(t *testing.T) {
	reg := NewToolRegistry()
	loop := NewAgentLoop(nil, reg, "medium", "", 20, 2000, 200, nil, nil, nil)

	loop.SetPreCompactionSnapshot(func(string, []client.Message) {})
	if loop.preCompactionSnapshot == nil {
		t.Fatal("setter did not store the snapshotter")
	}
	loop.SwitchAgent("prompt", t.TempDir(), reg, "", nil)
	if loop.preCompactionSnapshot != nil {
		t.Error("SwitchAgent must reset the snapshotter")
	}

	loop.SetPreCompactionSnapshot(func(string, []client.Message) {})
	loop.SetSessionID("other-session")
	if loop.preCompactionSnapshot != nil {
		t.Error("SetSessionID must reset the snapshotter")
	}
}
