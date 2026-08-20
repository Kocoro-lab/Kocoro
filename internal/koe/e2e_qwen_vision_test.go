//go:build darwin && cgo

package koe

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestKoeQwenLiveVisionE2E sends a caller-supplied H264 keyframe over Qwen's
// real WebRTC video track and checks the model's answer against caller-supplied
// expected terms. It is opt-in because it uses the running daemon's Qwen relay.
func TestKoeQwenLiveVisionE2E(t *testing.T) {
	if os.Getenv("KOE_QWEN_VISION_E2E") != "1" {
		t.Skip("live Qwen vision E2E: set KOE_QWEN_VISION_E2E=1 and KOE_QWEN_VISION_H264")
	}
	framePath := strings.TrimSpace(os.Getenv("KOE_QWEN_VISION_H264"))
	if framePath == "" {
		t.Fatal("KOE_QWEN_VISION_H264 is required")
	}
	frame, err := os.ReadFile(framePath)
	if err != nil {
		t.Fatalf("read H264 frame: %v", err)
	}
	if !bytes.Contains(frame, []byte{0, 0, 1}) {
		t.Fatal("H264 frame is not Annex-B")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	audio, err := NewAudioIO()
	if err != nil {
		t.Fatalf("NewAudioIO: %v", err)
	}
	state := NewCallState("qwen-live-vision-e2e", "")
	disp := NewDispatcher(
		NewDaemonClient("http://127.0.0.1:1"),
		NewAgentResolver(nil, NoopSemanticMatcher{}),
		state,
		nil,
	)

	var framesRead atomic.Int32
	transcripts := make(chan string, 4)
	closed := make(chan error, 1)
	model := strings.TrimSpace(os.Getenv("KOE_QWEN_VISION_MODEL"))
	if model == "" {
		model = DefaultQwenRealtimeModel
	}
	relayURL := strings.TrimSpace(os.Getenv("KOE_E2E_DAEMON_URL"))
	if relayURL == "" {
		relayURL = "http://127.0.0.1:7533"
	}
	relay := NewDaemonClient(relayURL)
	rc, err := ConnectQwen(
		ctx,
		audio,
		func(exchangeCtx context.Context, offer string) (string, error) {
			return relay.ExchangeSDPViaDaemon(exchangeCtx, string(ProviderQwen), model, offer)
		},
		"You are Kocoro. Answer visual questions from the live camera image directly, briefly, and without guessing.",
		state,
		disp,
		ConnectOptions{
			Model:      model,
			Voice:      DefaultQwenRealtimeVoice,
			CallActive: func() bool { return true },
			VideoSource: &RealtimeVideoSource{
				Codec:         VideoCodecH264,
				FrameInterval: time.Second,
				ReadFrame: func(context.Context) ([]byte, error) {
					framesRead.Add(1)
					return append([]byte(nil), frame...), nil
				},
			},
			OnAssistantTranscript: func(text string) {
				select {
				case transcripts <- text:
				default:
				}
			},
			OnClosed: func(err error) {
				select {
				case closed <- err:
				default:
				}
			},
		},
	)
	if err != nil {
		t.Fatalf("connect Qwen: %v", err)
	}
	defer rc.Close()

	deadline := time.Now().Add(6 * time.Second)
	for framesRead.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(100 * time.Millisecond)
	}
	if framesRead.Load() < 2 {
		t.Fatalf("video pump read %d frames, want at least 2", framesRead.Load())
	}
	prompt := strings.TrimSpace(os.Getenv("KOE_QWEN_VISION_PROMPT"))
	if prompt == "" {
		prompt = "画面中央桌子上最高、最显眼的物体是什么？只回答物体。"
	}
	item, _ := json.Marshal(map[string]any{
		"type": "conversation.item.create",
		"item": map[string]any{
			"type": "message", "role": "user",
			"content": []map[string]any{{"type": "input_text", "text": prompt}},
		},
	})
	if err := rc.sendText(string(item)); err != nil {
		t.Fatalf("send vision question: %v", err)
	}
	response, _ := json.Marshal(map[string]any{"type": "response.create"})
	if err := rc.sendText(string(response)); err != nil {
		t.Fatalf("request vision response: %v", err)
	}

	expected := strings.TrimSpace(os.Getenv("KOE_QWEN_VISION_EXPECT"))
	var transcriptsSeen []string
	for {
		select {
		case transcript := <-transcripts:
			transcriptsSeen = append(transcriptsSeen, transcript)
			t.Logf("Qwen live-vision transcript: %q (frames=%d)", transcript, framesRead.Load())
			if expected == "" || transcriptMatchesAny(transcript, expected) {
				return
			}
		case err := <-closed:
			t.Fatalf("Qwen session closed before a matching answer: %v (transcripts=%q)", err, transcriptsSeen)
		case <-ctx.Done():
			t.Fatalf("Qwen vision response timed out: %v (expected=%q transcripts=%q)", ctx.Err(), expected, transcriptsSeen)
		}
	}
}

func transcriptMatchesAny(transcript, expected string) bool {
	lower := strings.ToLower(transcript)
	for _, term := range strings.Split(expected, ",") {
		term = strings.TrimSpace(term)
		if term != "" && strings.Contains(lower, strings.ToLower(term)) {
			return true
		}
	}
	return false
}

func TestTranscriptMatchesAny(t *testing.T) {
	if !transcriptMatchesAny("椅子", "chair, 椅子") {
		t.Fatal("matching transcript was rejected")
	}
	if transcriptMatchesAny("桌子", "chair, 椅子") {
		t.Fatal("non-matching transcript was accepted")
	}
}
