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

func newCalibrationTestLoop(reg *ToolRegistry) *AgentLoop {
	gw := client.NewGatewayClient("http://127.0.0.1:0", "")
	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindowExplicit(200_000)
	return loop
}

func TestEstOverheadState_SnapshotAndRestore(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})

	src := newCalibrationTestLoop(reg)
	src.estOverheadTokens.Store(12345)
	src.estOverheadModel.Store("claude-sonnet-5-20260203")

	tokens, model, fp := src.EstOverheadState()
	if tokens != 12345 || model != "claude-sonnet-5-20260203" {
		t.Fatalf("snapshot = (%d, %q), want (12345, claude-sonnet-5-20260203)", tokens, model)
	}
	if fp == "" {
		t.Fatal("snapshot must include a non-empty registry fingerprint")
	}

	dst := newCalibrationTestLoop(reg)
	dst.SetEstOverheadState(tokens, model, fp)
	if got := dst.estOverhead(); got != 12345 {
		t.Errorf("restore on an identical registry must apply the sample, got %d", got)
	}
	if gotTokens, gotModel, _ := dst.EstOverheadState(); gotTokens != 12345 || gotModel != model {
		t.Errorf("restored snapshot = (%d, %q), want round-trip", gotTokens, gotModel)
	}
}

func TestEstOverheadState_RestoreDiscardsOnRegistryChange(t *testing.T) {
	regA := NewToolRegistry()
	regA.Register(&thinkTool{})
	src := newCalibrationTestLoop(regA)
	src.estOverheadTokens.Store(9000)
	tokens, model, fp := src.EstOverheadState()

	// Destination registry lacks the tool — different schema mass, so the
	// persisted sample no longer describes what this loop will send.
	dst := newCalibrationTestLoop(NewToolRegistry())
	dst.SetEstOverheadState(tokens, model, fp)
	if got := dst.estOverhead(); got != 0 {
		t.Errorf("registry-changed restore must discard the sample, got %d", got)
	}
}

func TestEstOverheadState_RestoreDiscardsInvalidSamples(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})
	src := newCalibrationTestLoop(reg)
	src.estOverheadTokens.Store(5000)
	_, _, fp := src.EstOverheadState()

	dst := newCalibrationTestLoop(reg)
	dst.SetContextWindowExplicit(20000)
	dst.SetEstOverheadState(25000, "", fp) // sample larger than the whole window
	if got := dst.estOverhead(); got != 0 {
		t.Errorf("over-window restore must discard the sample, got %d", got)
	}

	dst.SetEstOverheadState(0, "", fp) // non-positive
	if got := dst.estOverhead(); got != 0 {
		t.Errorf("non-positive restore must be a no-op, got %d", got)
	}

	dst.SetEstOverheadState(5000, "", "") // missing fingerprint = unverifiable
	if got := dst.estOverhead(); got != 0 {
		t.Errorf("fingerprint-less restore must discard the sample, got %d", got)
	}
}

func TestEstOverheadState_RestoreValidatesModelPin(t *testing.T) {
	reg := NewToolRegistry()
	reg.Register(&thinkTool{})
	src := newCalibrationTestLoop(reg)
	src.estOverheadTokens.Store(7000)
	src.estOverheadModel.Store("claude-sonnet-5-20260203")
	tokens, model, fp := src.EstOverheadState()

	// Pin matches (dated variant of the pinned family) → accepted.
	dst := newCalibrationTestLoop(reg)
	dst.SetSpecificModel("claude-sonnet-5")
	dst.SetEstOverheadState(tokens, model, fp)
	if got := dst.estOverhead(); got != 7000 {
		t.Errorf("pin-compatible restore must apply, got %d", got)
	}

	// Pin switched to a different model → tokenizer/schema overhead differ.
	other := newCalibrationTestLoop(reg)
	other.SetSpecificModel("gpt-5.6-luna")
	other.SetEstOverheadState(tokens, model, fp)
	if got := other.estOverhead(); got != 0 {
		t.Errorf("pin-mismatched restore must discard the sample, got %d", got)
	}

	// Sample without a model under an active pin cannot prove compatibility.
	pinned := newCalibrationTestLoop(reg)
	pinned.SetSpecificModel("claude-sonnet-5")
	pinned.SetEstOverheadState(tokens, "", fp)
	if got := pinned.estOverhead(); got != 0 {
		t.Errorf("model-less restore under a pin must discard the sample, got %d", got)
	}
}

// TestAgentLoop_CalibrationRecordsResponseModel: the per-response calibration
// site must record which model produced the sample, so a checkpoint can later
// validate the sample against the resumed configuration.
func TestAgentLoop_CalibrationRecordsResponseModel(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(nativeResponse("ok", "end_turn", nil, 50_000, 10))
	}))
	defer server.Close()

	gw := client.NewGatewayClient(server.URL, "")
	reg := NewToolRegistry()
	loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
	loop.SetContextWindowExplicit(200_000)
	if _, _, err := loop.Run(context.Background(), "hello", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}

	tokens, model, fp := loop.EstOverheadState()
	if tokens <= 0 {
		t.Fatalf("calibration should be positive after a response with real usage, got %d", tokens)
	}
	if model != "test-model" {
		t.Errorf("calibration model = %q, want the response model test-model", model)
	}
	if fp == "" {
		t.Error("snapshot fingerprint must be non-empty")
	}
}

// TestAgentLoop_RestoredCalibrationArmsIterationZeroCompaction is the #327
// behavior: a fresh (daemon-style) loop resuming a large history makes its
// iteration-0 proactive decision blind unless the checkpointed calibration is
// restored. With the restored overhead the i==0 heuristic must fire the
// gentle proactive path (summary generated + history shaped); without it the
// same history must NOT trigger.
func TestAgentLoop_RestoredCalibrationArmsIterationZeroCompaction(t *testing.T) {
	history := make([]client.Message, 0, 40)
	for i := 0; i < 20; i++ {
		history = append(history,
			client.Message{Role: "user", Content: client.NewTextContent(fmt.Sprintf("step request %d with some filler text to give the turn a little bit of mass", i))},
			client.Message{Role: "assistant", Content: client.NewTextContent(fmt.Sprintf("step reply %d acknowledged and completed", i))},
		)
	}

	run := func(restore bool) (sawSummary bool) {
		var mu sync.Mutex
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw, _ := readBody(r.Body)
			defer r.Body.Close()
			var req struct {
				ModelTier string `json:"model_tier"`
			}
			json.Unmarshal(raw, &req)
			if req.ModelTier == "small" {
				mu.Lock()
				if strings.Contains(string(raw), "Compress the following conversation") {
					sawSummary = true
				}
				mu.Unlock()
				json.NewEncoder(w).Encode(nativeResponse("summary of the resumed session", "end_turn", nil, 50, 30))
				return
			}
			json.NewEncoder(w).Encode(nativeResponse("done", "end_turn", nil, 100, 10))
		}))
		defer server.Close()

		gw := client.NewGatewayClient(server.URL, "")
		reg := NewToolRegistry()
		reg.Register(&thinkTool{})

		// Snapshot from a previous "run" under the identical registry: overhead
		// large enough that estimate+overhead crosses 90% of the 20K window
		// while the raw estimate stays far below it (the resumed-blindness band).
		src := newCalibrationTestLoop(reg)
		src.estOverheadTokens.Store(17_000)
		src.estOverheadModel.Store("test-model")
		tokens, model, fp := src.EstOverheadState()

		loop := NewAgentLoop(gw, reg, "medium", "", 20, 2000, 200, nil, nil, nil)
		loop.SetContextWindowExplicit(20_000)
		loop.SetMemoryDir(t.TempDir())
		loop.SetHandler(&mockHandler{approveResult: true})
		loop.SetSessionID("resumed-session")
		if restore {
			loop.SetEstOverheadState(tokens, model, fp)
		}
		if _, _, err := loop.Run(context.Background(), "continue the work", nil, history); err != nil {
			t.Logf("Run error (tolerated): %v", err)
		}
		mu.Lock()
		defer mu.Unlock()
		return sawSummary
	}

	if run(false) {
		t.Fatal("control: uncalibrated iteration-0 heuristic must NOT fire on this low-estimate history")
	}
	if !run(true) {
		t.Error("restored calibration must arm the iteration-0 proactive trigger (no summary was generated)")
	}
}
