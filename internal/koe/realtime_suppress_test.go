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

// TestShouldVoiceDoTaskResult pins the stale-result suppression decision: the
// function_call_output is always submitted; this gate only decides whether the
// result is spoken. A bare mid-task user turn (correction / topic change) with no
// follow-up do_task suppresses; a follow-up that refines the same task still voices.
func TestShouldVoiceDoTaskResult(t *testing.T) {
	ok := SayResult{Status: "ok", Say: "Done — reminder set."}
	failed := SayResult{Status: "failed", Say: "Sorry, that failed."}
	injected := SayResult{Status: "injected"}
	emptySay := SayResult{Status: "ok", Say: ""}
	cancelled := SayResult{Status: "cancelled"} // MapDoTaskOutcome leaves Say empty

	cases := []struct {
		name        string
		r           SayResult
		newUserTurn bool
		followUp    bool
		want        bool
	}{
		{"normal completed, user waited", ok, false, false, true},
		{"correction: new turn, no follow-up → suppress", ok, true, false, false},
		{"follow-up: new turn + another do_task → voice", ok, true, true, true},
		{"follow-up dispatched, no committed turn → voice", ok, false, true, true},
		{"failed result still voices when user waited", failed, false, false, true},
		{"failed result suppressed after a correction", failed, true, false, false},
		{"injected never voices", injected, false, false, false},
		{"injected never voices even mid-correction", injected, true, false, false},
		{"empty say never voices", emptySay, false, false, false},
		{"cancelled (empty say) never voices", cancelled, false, false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldVoiceDoTaskResult(c.r, c.newUserTurn, c.followUp); got != c.want {
				t.Errorf("shouldVoiceDoTaskResult(%+v, newUserTurn=%v, followUp=%v) = %v, want %v",
					c.r, c.newUserTurn, c.followUp, got, c.want)
			}
		})
	}
}

// TestDoTaskVoicingGateWiring drives the REAL handleFunctionCall/do_task goroutine
// against a mock daemon whose /message result is released on demand, so a mid-task
// user turn can be interleaved deterministically. It asserts the wiring — counter
// capture at dispatch, comparison at land-time — actually suppresses the voicing
// (the response.create), while the function_call_output is still submitted. No live
// API: the gate is production code exercised end-to-end.
func TestDoTaskVoicingGateWiring(t *testing.T) {
	run := func(t *testing.T, midTaskUserTurn bool) (fnOutputs, voiceRequested int) {
		t.Helper()
		release := make(chan struct{})
		mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path == "/message" {
				<-release // hold the result until the test decides the mid-task timing
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"reply": "Done, reminder set.", "spoken_summary": "Done, reminder set."})
		}))
		defer mock.Close()

		audio, err := NewAudioIO()
		if err != nil {
			t.Fatalf("NewAudioIO: %v", err)
		}
		state := NewCallState("burst-gate", "")
		disp := NewDispatcher(NewDaemonClient(mock.URL), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)

		var mu sync.Mutex
		fnOut := 0
		sendFn := func(v any) error {
			b, _ := json.Marshal(v)
			var m struct {
				Type string `json:"type"`
				Item struct {
					Type string `json:"type"`
				} `json:"item"`
			}
			_ = json.Unmarshal(b, &m)
			if m.Type == "conversation.item.create" && m.Item.Type == "function_call_output" {
				mu.Lock()
				fnOut++
				mu.Unlock()
			}
			return nil
		}
		h := newEventHandler(disp, state, audio, sendFn)
		ctx := context.Background()

		// do_task fires (arguments arrive string-encoded, like the live API).
		fc, _ := json.Marshal(map[string]any{
			"type": "response.function_call_arguments.done", "name": "do_task",
			"call_id": "call_gate", "arguments": `{"task":"remind me to call mom at six"}`})
		h.handleEvent(ctx, fc)

		if midTaskUserTurn {
			// The user takes the floor mid-task (e.g. "you got that wrong") → the server
			// commits their turn. This is the stale-making signal.
			committed, _ := json.Marshal(map[string]any{"type": "input_audio_buffer.committed"})
			h.handleEvent(ctx, committed)
		}
		close(release) // result lands now

		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			mu.Lock()
			n := fnOut
			mu.Unlock()
			if n > 0 {
				break
			}
			time.Sleep(10 * time.Millisecond)
		}
		time.Sleep(100 * time.Millisecond) // settle: voice decision runs right after the output
		mu.Lock()
		fnOutputs = fnOut
		mu.Unlock()
		return fnOutputs, len(h.respReq) // respReq holds the queued voicing response.create
	}

	t.Run("user waited → result voiced", func(t *testing.T) {
		out, voice := run(t, false)
		if out == 0 {
			t.Fatalf("function_call_output was never submitted")
		}
		if voice != 1 {
			t.Errorf("expected the result to be voiced (respReq=1), got respReq=%d", voice)
		}
	})
	t.Run("user took the floor mid-task → result NOT voiced", func(t *testing.T) {
		out, voice := run(t, true)
		if out == 0 {
			t.Fatalf("function_call_output must still be submitted even when suppressed")
		}
		if voice != 0 {
			t.Errorf("expected the stale result to be suppressed (respReq=0), got respReq=%d", voice)
		}
	})
}
