//go:build darwin && cgo

package koe

// Live Realtime regression for staggered parallel results. The first task result
// is claimed before the second lands, exactly like two independent daemon runs
// completing at slightly different times. Each response must describe only its
// newly delivered batch: absence from the first batch is not evidence that the
// other task failed, is missing, or is still running.

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestKoeStaggeredParallelDeliveryE2E(t *testing.T) {
	if os.Getenv("KOE_E2E") != "1" {
		t.Skip("staggered full-path E2E: set KOE_E2E=1 (uses OPENAI_API_KEY or the running daemon mint relay)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Second)
	defer cancel()

	ek, err := mintE2EEphemeral(ctx)
	if err != nil {
		t.Fatalf("mint Realtime token: %v", err)
	}

	weatherRelease := make(chan struct{})
	newsRelease := make(chan struct{})
	var weatherOnce, newsOnce sync.Once
	defer weatherOnce.Do(func() { close(weatherRelease) })
	defer newsOnce.Do(func() { close(newsRelease) })
	requests := make(chan DoTaskRequest, 2)
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/message" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		body, _ := io.ReadAll(r.Body)
		var req DoTaskRequest
		_ = json.Unmarshal(body, &req)
		requests <- req
		lower := strings.ToLower(req.Text)
		switch {
		case strings.Contains(req.Text, "天气") || strings.Contains(lower, "weather"):
			select {
			case <-weatherRelease:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"reply": "东京今天晴，最高气温36摄氏度，午后西部山区有短时雷阵雨风险。",
			})
		case strings.Contains(req.Text, "新闻") || strings.Contains(lower, "news"):
			select {
			case <-newsRelease:
			case <-r.Context().Done():
				return
			}
			_ = json.NewEncoder(w).Encode(map[string]any{
				"reply": "今天的重要科技新闻是 Helio 发布 Atlas-7 推理芯片，官方称同等吞吐下能耗降低37%。",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{
				"status": "failed", "failure_code": "unexpected_task", "reason": req.Text,
			})
		}
	}))
	defer mock.Close()

	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("burst-staggered-e2e", "")
	disp := NewDispatcher(NewDaemonClient(mock.URL), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)
	rc, err := newPeerConnection(audio)
	if err != nil {
		t.Fatalf("newPeerConnection: %v", err)
	}
	defer rc.Close()

	firstBatchInjected := make(chan struct{}, 1)
	var resultInjections atomic.Int32
	var h *eventHandler
	send := func(v any) error {
		body, _ := json.Marshal(v)
		if strings.Contains(string(body), "kocoro.task_results.v1") {
			if resultInjections.Add(1) == 1 {
				signalNonBlocking(firstBatchInjected)
			}
		}
		return rc.dc.SendText(string(body))
	}
	h = newEventHandler(disp, state, audio, send)
	go h.runResponseSender(ctx)

	connected := make(chan struct{})
	configured := make(chan struct{})
	var connOnce, cfgOnce sync.Once
	var eventMu sync.Mutex
	var transcripts []string
	var apiErrors []string
	rc.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			connOnce.Do(func() { close(connected) })
		}
	})
	const persona = "You are Kocoro, a concise Chinese voice assistant. For current weather and news, call do_task. If the user asks for both weather and news, emit exactly two distinct parallel do_task calls in the same response. Before the calls say only 我查一下 and nothing else."
	rc.dc.OnOpen(func() { _ = send(sessionConfig(persona, "marin", false)) })
	rc.dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var ev struct {
			Type       string `json:"type"`
			Transcript string `json:"transcript"`
		}
		_ = json.Unmarshal(m.Data, &ev)
		h.handleEvent(ctx, m.Data)
		switch ev.Type {
		case "session.updated":
			cfgOnce.Do(func() { close(configured) })
		case "response.output_audio_transcript.done":
			eventMu.Lock()
			transcripts = append(transcripts, ev.Transcript)
			index := len(transcripts)
			eventMu.Unlock()
			t.Logf("[full-path speech %d] %q", index, ev.Transcript)
		case "error", "response.failed":
			eventMu.Lock()
			apiErrors = append(apiErrors, string(m.Data))
			eventMu.Unlock()
		}
	})
	if err := rc.dialOpenAI(ctx, ek); err != nil {
		t.Fatalf("dial OpenAI: %v", err)
	}
	go rc.pumpSendTrack(ctx)
	waitIncrementalSignal(t, ctx, connected, "peer connection did not connect")
	waitIncrementalSignal(t, ctx, configured, "session did not configure")

	turnID := h.inputCommitSeq.Add(1)
	h.toolLoop.noteUserCommit(turnID)
	if err := send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
		"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": "同时查询今天东京的天气和今天的重要新闻。"}},
	}}); err != nil {
		t.Fatalf("send user turn: %v", err)
	}
	h.queueLoopResponse(responseCreateRequest{
		purpose: responsePurposeUser, turnID: turnID,
		toolMode: responseToolsEnabled, dropIfPreempted: true,
	})

	seen := make(map[string]DoTaskRequest)
	for len(seen) < 2 {
		select {
		case req := <-requests:
			lower := strings.ToLower(req.Text)
			switch {
			case strings.Contains(req.Text, "天气") || strings.Contains(lower, "weather"):
				seen["weather"] = req
			case strings.Contains(req.Text, "新闻") || strings.Contains(lower, "news"):
				seen["news"] = req
			default:
				t.Fatalf("model dispatched an unexpected task: %+v", req)
			}
		case <-ctx.Done():
			t.Fatalf("model did not dispatch both tasks: %+v", seen)
		}
	}
	if seen["weather"].ThreadID == seen["news"].ThreadID {
		t.Fatalf("parallel tasks shared a daemon lane: %+v", seen)
	}
	waitCompoundIdle(t, ctx, h, "initial parallel-tool response did not settle")

	weatherOnce.Do(func() { close(weatherRelease) })
	waitIncrementalSignal(t, ctx, firstBatchInjected, "weather result was not claimed")
	time.Sleep(250 * time.Millisecond)
	newsOnce.Do(func() { close(newsRelease) })

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		eventMu.Lock()
		allSpeech := append([]string(nil), transcripts...)
		errs := append([]string(nil), apiErrors...)
		eventMu.Unlock()
		if len(errs) > 0 {
			t.Fatalf("Realtime error: %s", strings.Join(errs, "\n"))
		}
		var resultSpeech []string
		for _, speech := range allSpeech {
			if strings.Contains(speech, "36") || strings.Contains(speech, "37") {
				resultSpeech = append(resultSpeech, speech)
			}
		}
		if len(resultSpeech) >= 2 && h.resultMailbox.pending() == 0 {
			assertIncrementalResultSpeech(t, resultSpeech[:2])
			if got := resultInjections.Load(); got != 2 {
				t.Fatalf("result injections=%d, want 2", got)
			}
			t.Logf("VERDICT: one live Response dispatched two daemon tasks on distinct lanes; staggered results produced two scoped updates")
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	eventMu.Lock()
	defer eventMu.Unlock()
	t.Fatalf("full-path staggered delivery timed out: transcripts=%v errors=%v tasks=%+v pending=%d", transcripts, apiErrors, seen, h.resultMailbox.pending())
}

func TestKoeIncrementalParallelResultsE2E(t *testing.T) {
	if os.Getenv("KOE_E2E") != "1" {
		t.Skip("incremental parallel-result E2E: set KOE_E2E=1 (uses OPENAI_API_KEY or the running daemon mint relay)")
	}
	trials := koeEnvInt("KOE_E2E_TRIALS", 3)
	if trials < 1 {
		trials = 1
	}
	for trial := 1; trial <= trials; trial++ {
		t.Run(fmt.Sprintf("trial_%02d", trial), func(t *testing.T) {
			runIncrementalParallelResultTrial(t, trial)
		})
	}
}

func runIncrementalParallelResultTrial(t *testing.T, trial int) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	ek, err := mintE2EEphemeral(ctx)
	if err != nil {
		t.Fatalf("mint Realtime token: %v", err)
	}
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	rc, err := newPeerConnection(audio)
	if err != nil {
		t.Fatalf("newPeerConnection: %v", err)
	}
	defer rc.Close()

	firstBatchInjected := make(chan struct{}, 1)
	var injectionMu sync.Mutex
	injections := 0
	send := func(v any) error {
		body, _ := json.Marshal(v)
		if strings.Contains(string(body), "kocoro.task_results.v1") {
			injectionMu.Lock()
			injections++
			if injections == 1 {
				signalNonBlocking(firstBatchInjected)
			}
			injectionMu.Unlock()
		}
		return rc.dc.SendText(string(body))
	}

	mailbox := NewResultMailbox()
	h := newEventHandlerWithMailbox(nil, NewCallState(fmt.Sprintf("burst-incremental-%d", trial), ""), audio, send, mailbox, nil)
	go h.runResponseSender(ctx)

	connected := make(chan struct{})
	configured := make(chan struct{})
	var connOnce, cfgOnce sync.Once
	var eventMu sync.Mutex
	var transcripts []string
	var apiErrors []string
	rc.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			connOnce.Do(func() { close(connected) })
		}
	})
	rc.dc.OnOpen(func() {
		_ = send(sessionConfig("You are Kocoro, a concise native Chinese voice assistant.", "marin", false))
	})
	rc.dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var ev struct {
			Type       string `json:"type"`
			Transcript string `json:"transcript"`
		}
		_ = json.Unmarshal(m.Data, &ev)
		h.handleEvent(ctx, m.Data)
		switch ev.Type {
		case "session.updated":
			cfgOnce.Do(func() { close(configured) })
		case "response.output_audio_transcript.done":
			eventMu.Lock()
			transcripts = append(transcripts, ev.Transcript)
			index := len(transcripts)
			eventMu.Unlock()
			t.Logf("[incremental speech %d] %q", index, ev.Transcript)
		case "error", "response.failed":
			eventMu.Lock()
			apiErrors = append(apiErrors, string(m.Data))
			eventMu.Unlock()
		}
	})
	if err := rc.dialOpenAI(ctx, ek); err != nil {
		t.Fatalf("dial OpenAI: %v", err)
	}
	go rc.pumpSendTrack(ctx)
	waitIncrementalSignal(t, ctx, connected, "peer connection did not connect")
	waitIncrementalSignal(t, ctx, configured, "session did not configure")

	if err := send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
		"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": "同时查询今天东京的天气和今天的重要新闻，结果可以分别告诉我。"}},
	}}); err != nil {
		t.Fatalf("inject user context: %v", err)
	}

	mailbox.Enqueue(SayResult{
		TaskID: "weather", Task: "查询今天东京的天气", Revision: 1, Status: "ok",
		Reply: "东京今天晴，最高气温36摄氏度，午后西部山区有短时雷阵雨风险。",
	}, false)
	waitIncrementalSignal(t, ctx, firstBatchInjected, "weather result was not claimed")
	// Reproduce the observed race: the second daemon task lands shortly after the
	// first result has already become an in-flight Realtime response.
	time.Sleep(250 * time.Millisecond)
	mailbox.Enqueue(SayResult{
		TaskID: "news", Task: "查询今天的重要新闻", Revision: 1, Status: "ok",
		Reply: "今天的重要科技新闻是 Helio 发布 Atlas-7 推理芯片，官方称同等吞吐下能耗降低37%。",
	}, false)

	deadline := time.Now().Add(70 * time.Second)
	for time.Now().Before(deadline) {
		eventMu.Lock()
		got := append([]string(nil), transcripts...)
		errs := append([]string(nil), apiErrors...)
		eventMu.Unlock()
		if len(errs) > 0 {
			t.Fatalf("Realtime error: %s", strings.Join(errs, "\n"))
		}
		if len(got) >= 2 && mailbox.pending() == 0 {
			assertIncrementalResultSpeech(t, got[:2])
			injectionMu.Lock()
			gotInjections := injections
			injectionMu.Unlock()
			if gotInjections != 2 {
				t.Fatalf("result injections=%d, want 2 independent incremental batches", gotInjections)
			}
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	eventMu.Lock()
	defer eventMu.Unlock()
	t.Fatalf("incremental results timed out: transcripts=%v errors=%v pending=%d", transcripts, apiErrors, mailbox.pending())
}

func waitIncrementalSignal(t *testing.T, ctx context.Context, signal <-chan struct{}, failure string) {
	t.Helper()
	select {
	case <-signal:
	case <-ctx.Done():
		t.Fatal(failure)
	}
}

func assertIncrementalResultSpeech(t *testing.T, transcripts []string) {
	t.Helper()
	first := strings.ToLower(transcripts[0])
	second := strings.ToLower(transcripts[1])
	if !strings.Contains(first, "东京") || !strings.Contains(first, "36") {
		t.Fatalf("first update lost weather facts: %q", transcripts[0])
	}
	if strings.Contains(first, "新闻") {
		t.Fatalf("first update commented on an omitted concurrent news task: %q", transcripts[0])
	}
	if !strings.Contains(second, "37") || (!strings.Contains(second, "helio") && !strings.Contains(second, "芯片")) {
		t.Fatalf("second update lost news facts: %q", transcripts[1])
	}
	if strings.Contains(second, "36") {
		t.Fatalf("second update repeated the earlier weather batch: %q", transcripts[1])
	}
}
