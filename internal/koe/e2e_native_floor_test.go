//go:build darwin && cgo

package koe

// Live raw-audio E2E for reversible turn-taking. Each trial starts a long native
// audio response, injects synthesized speech while it is playing, and runs every
// server event through the production handler. A backchannel must call only
// resume_playback and produce no reply; a real interruption must call accept_turn
// and the subsequent tools-enabled native Response must answer the raw utterance.

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/pion/webrtc/v4"
)

func TestKoeNativeFloorE2E(t *testing.T) {
	if os.Getenv("KOE_E2E") != "1" {
		t.Skip("native floor E2E: set KOE_E2E=1 (mints via the running daemon)")
	}
	enableNativeFloorForTest(t)
	t.Run("backchannel resumes exact response without reply", func(t *testing.T) {
		tools, postDecision := runNativeFloorTrial(t, "Mm-hmm.", "resume_playback")
		if len(tools) != 1 || tools[0] != "resume_playback" {
			t.Fatalf("backchannel floor tools=%v, want only resume_playback", tools)
		}
		if len(postDecision) != 0 {
			t.Fatalf("backchannel produced a new spoken reply: %v", postDecision)
		}
	})
	t.Run("real interruption is accepted and answered", func(t *testing.T) {
		tools, postDecision := runNativeFloorTrial(t, "Stop that. What is the capital of France?", "accept_turn")
		if len(tools) != 1 || tools[0] != "accept_turn" {
			t.Fatalf("real interruption floor tools=%v, want only accept_turn", tools)
		}
		joined := strings.ToLower(strings.Join(postDecision, " "))
		if !strings.Contains(joined, "paris") {
			t.Fatalf("accepted raw-audio turn was not answered from its content: %v", postDecision)
		}
	})
}

func runNativeFloorTrial(t *testing.T, overlap, expectedTool string) ([]string, []string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	daemonBase := os.Getenv("KOE_DAEMON_URL")
	if daemonBase == "" {
		daemonBase = "http://127.0.0.1:7533"
	}
	ek, err := NewDaemonClient(daemonBase).MintViaDaemon(ctx, e2eModel)
	if err != nil {
		t.Fatalf("mint via daemon: %v", err)
	}
	pcm := synthSpokenWAV(t, overlap)

	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	rc, err := newPeerConnection(audio)
	if err != nil {
		t.Fatalf("newPeerConnection: %v", err)
	}
	defer rc.Close()
	rc.fullDuplexAEC = true
	send := func(v any) error {
		body, _ := json.Marshal(v)
		return rc.dc.SendText(string(body))
	}
	state := NewCallState("burst-floor-e2e", "")
	disp := NewDispatcher(NewDaemonClient("http://127.0.0.1:1"), NewAgentResolver(nil, NoopSemanticMatcher{}), state, nil)
	h := newEventHandler(disp, state, audio, send)
	h.fullDuplexAEC = true
	go h.runResponseSender(ctx)
	go drainHeadlessPlayback(ctx, audio)

	connected := make(chan struct{})
	configured := make(chan struct{})
	outputStarted := make(chan struct{})
	decisionSeen := make(chan struct{})
	acceptedReply := make(chan struct{})
	var connOnce, cfgOnce, outputOnce, decisionOnce, replyOnce sync.Once
	var mu sync.Mutex
	var floorTools []string
	var postDecision []string
	decisionApplied := false
	var errorsSeen []string
	persona := "You are Kocoro, a concise voice assistant. For the initial request, speak a calm story for at least twenty seconds so the user can interrupt. Never call a tool for that story."
	rc.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		if s == webrtc.PeerConnectionStateConnected {
			connOnce.Do(func() { close(connected) })
		}
	})
	rc.dc.OnOpen(func() { _ = send(sessionConfig(persona, "marin", true)) })
	rc.dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var ev struct {
			Type       string `json:"type"`
			Name       string `json:"name"`
			Transcript string `json:"transcript"`
		}
		_ = json.Unmarshal(m.Data, &ev)
		h.handleEvent(ctx, m.Data)
		switch ev.Type {
		case "session.updated":
			cfgOnce.Do(func() { close(configured) })
		case "output_audio_buffer.started":
			outputOnce.Do(func() { close(outputStarted) })
		case "response.function_call_arguments.done":
			if ev.Name == "resume_playback" || ev.Name == "accept_turn" {
				mu.Lock()
				floorTools = append(floorTools, ev.Name)
				decisionApplied = true
				mu.Unlock()
				decisionOnce.Do(func() { close(decisionSeen) })
			}
		case "response.output_audio_transcript.done":
			mu.Lock()
			if decisionApplied && strings.TrimSpace(ev.Transcript) != "" {
				postDecision = append(postDecision, ev.Transcript)
				if expectedTool == "accept_turn" && strings.Contains(strings.ToLower(ev.Transcript), "paris") {
					replyOnce.Do(func() { close(acceptedReply) })
				}
			}
			mu.Unlock()
		case "error", "response.failed":
			mu.Lock()
			errorsSeen = append(errorsSeen, string(m.Data))
			mu.Unlock()
		}
	})
	if err := rc.dialOpenAI(ctx, ek); err != nil {
		t.Fatalf("dial OpenAI: %v", err)
	}
	go rc.pumpSendTrack(ctx)
	select {
	case <-connected:
	case <-ctx.Done():
		t.Fatal("peer connection did not connect")
	}
	select {
	case <-configured:
	case <-ctx.Done():
		t.Fatal("session did not configure")
	}

	turnID := h.inputCommitSeq.Add(1)
	h.toolLoop.noteUserCommit(turnID)
	if err := send(map[string]any{"type": "conversation.item.create", "item": map[string]any{
		"type": "message", "role": "user",
		"content": []map[string]any{{"type": "input_text", "text": "Tell me a calm, continuous story for at least twenty seconds."}},
	}}); err != nil {
		t.Fatalf("send source request: %v", err)
	}
	h.queueLoopResponse(responseCreateRequest{
		purpose: responsePurposeUser, turnID: turnID,
		toolMode: responseToolsEnabled, dropIfPreempted: true,
	})
	select {
	case <-outputStarted:
	case <-ctx.Done():
		t.Fatal("source response never started audio")
	}
	feedWAVWithSilence(ctx, audio, pcm, 100) // 2s tail exceeds the fixed 1500ms endpoint
	select {
	case <-decisionSeen:
	case <-ctx.Done():
		mu.Lock()
		errs := append([]string(nil), errorsSeen...)
		mu.Unlock()
		t.Fatalf("native floor made no decision; errors=%v", errs)
	}
	if expectedTool == "accept_turn" {
		select {
		case <-acceptedReply:
		case <-time.After(30 * time.Second):
			mu.Lock()
			transcripts := append([]string(nil), postDecision...)
			errs := append([]string(nil), errorsSeen...)
			mu.Unlock()
			t.Fatalf("accepted turn did not answer Paris; transcripts=%v errors=%v", transcripts, errs)
		}
	} else {
		// The floor function output itself completes silently. Give a mistaken normal
		// reply a full response window to surface.
		time.Sleep(3 * time.Second)
	}
	mu.Lock()
	defer mu.Unlock()
	return append([]string(nil), floorTools...), append([]string(nil), postDecision...)
}

func feedWAVWithSilence(ctx context.Context, audio *AudioIO, pcm []int16, silenceFrames int) {
	ticker := time.NewTicker(audioFrameMs * time.Millisecond)
	defer ticker.Stop()
	feed := func(frame []int16) bool {
		select {
		case <-ctx.Done():
			return false
		case <-ticker.C:
		}
		audio.frames <- append([]int16(nil), frame...)
		return true
	}
	for off := 0; off+audioFrameSize <= len(pcm); off += audioFrameSize {
		if !feed(pcm[off : off+audioFrameSize]) {
			return
		}
	}
	silence := make([]int16, audioFrameSize)
	for range silenceFrames {
		if !feed(silence) {
			return
		}
	}
}

func drainHeadlessPlayback(ctx context.Context, audio *AudioIO) {
	ticker := time.NewTicker(audioFrameMs * time.Millisecond)
	defer ticker.Stop()
	out := make([]byte, audioFrameSize*2)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			audio.renderInto(out)
		}
	}
}
