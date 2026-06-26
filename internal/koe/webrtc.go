package koe

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	openAIMintURL  = "https://api.openai.com/v1/realtime/client_secrets"
	openAICallsURL = "https://api.openai.com/v1/realtime/calls"
)

// mintEphemeral mints an OpenAI Realtime ephemeral client secret with a dev key.
// DEV-KEY: replaced by the deferred daemon mint relay (→ Plan D Cloud mint) (the key never lives here in prod).
func mintEphemeral(ctx context.Context, apiKey, model string) (string, error) {
	return mintEphemeralAt(ctx, openAIMintURL, apiKey, model)
}

// mintEphemeralAt is mintEphemeral with an injectable URL (for tests).
func mintEphemeralAt(ctx context.Context, url, apiKey, model string) (string, error) {
	body, _ := json.Marshal(map[string]any{
		"session": map[string]any{"type": "realtime", "model": model},
	})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+apiKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return "", fmt.Errorf("mint failed: HTTP %d: %s", resp.StatusCode, string(raw))
	}
	var mint struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &mint); err != nil || mint.Value == "" {
		return "", fmt.Errorf("mint parse failed: %v body=%s", err, string(raw))
	}
	return mint.Value, nil
}

// RealtimeConn is one connected WebRTC session to OpenAI Realtime.
type RealtimeConn struct {
	pc        *webrtc.PeerConnection
	sendTrack *webrtc.TrackLocalStaticSample
	dc        *webrtc.DataChannel
	audio     *AudioIO
}

// newPeerConnection builds the pion PC with a send track + recvonly transceiver +
// the oai-events data channel. Grounded in spike stage2-webrtc + stage2b-duplex.
func newPeerConnection(audio *AudioIO) (*RealtimeConn, error) {
	pc, err := webrtc.NewPeerConnection(webrtc.Configuration{
		ICEServers: []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}},
	})
	if err != nil {
		return nil, err
	}
	// Send track: Opus capability MUST be opus/48000/2 (Channels:2) even for mono
	// content, or pion rejects SetRemoteDescription (proven in the spike).
	track, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{MimeType: webrtc.MimeTypeOpus, ClockRate: audioSampleRate, Channels: 2},
		"audio", "koe")
	if err != nil {
		pc.Close()
		return nil, err
	}
	if _, err := pc.AddTrack(track); err != nil {
		pc.Close()
		return nil, err
	}
	rc := &RealtimeConn{pc: pc, sendTrack: track, audio: audio}

	// Inbound audio: decode Opus → playback.
	pc.OnTrack(func(tr *webrtc.TrackRemote, _ *webrtc.RTPReceiver) {
		for {
			pkt, _, err := tr.ReadRTP()
			if err != nil {
				return
			}
			pcm, derr := audio.DecodeFrame(pkt.Payload)
			if derr == nil {
				audio.Play(pcm)
			}
		}
	})

	dc, err := pc.CreateDataChannel("oai-events", nil)
	if err != nil {
		pc.Close()
		return nil, err
	}
	rc.dc = dc
	return rc, nil
}

// dialOpenAI does the non-trickle SDP exchange: gather all candidates, POST raw
// offer SDP, set the answer. Grounded in the spike.
func (rc *RealtimeConn) dialOpenAI(ctx context.Context, ek string) error {
	offer, err := rc.pc.CreateOffer(nil)
	if err != nil {
		return err
	}
	if err := rc.pc.SetLocalDescription(offer); err != nil {
		return err
	}
	<-webrtc.GatheringCompletePromise(rc.pc)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, openAICallsURL,
		bytes.NewReader([]byte(rc.pc.LocalDescription().SDP)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+ek)
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	answer, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 && resp.StatusCode != 201 {
		return fmt.Errorf("sdp exchange failed: HTTP %d: %s", resp.StatusCode, string(answer))
	}
	return rc.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: string(answer),
	})
}

// pumpSendTrack Opus-encodes captured frames and writes them to the send track.
func (rc *RealtimeConn) pumpSendTrack(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-rc.audio.Frames():
			enc, err := rc.audio.EncodeFrame(frame)
			if err != nil {
				continue
			}
			_ = rc.sendTrack.WriteSample(media.Sample{
				Data: enc, Duration: audioFrameMs * time.Millisecond, // 20 ms frame
			})
		}
	}
}

// Close tears down the peer connection.
func (rc *RealtimeConn) Close() { _ = rc.pc.Close() }

// MintEphemeral is the exported dev-key mint (C-minimal). The deferred daemon mint relay swaps the body
// for a via-daemon call; the signature stays so cmd/koe.go is unchanged.
// DEV-KEY: replaced by the deferred daemon mint relay (→ Plan D Cloud mint).
func MintEphemeral(ctx context.Context, apiKey, model string) (string, error) {
	return mintEphemeral(ctx, apiKey, model)
}

// Connect builds the peer connection, dials OpenAI, configures the session, and
// starts the send-pump + event-dispatch loops. Returns once connected. onVoiceState
// (nil-safe) receives listening/thinking/speaking transitions for the Desktop
// control channel (G2 → Kocoro Island sprite).
func Connect(ctx context.Context, audio *AudioIO, ek, persona string, state *CallState, disp *Dispatcher, onVoiceState func(string)) (*RealtimeConn, error) {
	rc, err := newPeerConnection(audio)
	if err != nil {
		return nil, err
	}
	h := newEventHandler(disp, state, audio, func(v any) error {
		b, _ := json.Marshal(v)
		return rc.dc.SendText(string(b))
	})
	h.onVoiceState = onVoiceState
	// configured closes when OpenAI acks our session.update. The send pump waits
	// on it: if mic audio reaches the server before the tools/voice config lands,
	// the VAD-triggered auto response snapshots the default config (no tools) and
	// the first turn can't delegate. Verified against the live API in e2e_test.go.
	configured := make(chan struct{})
	var cfgOnce sync.Once
	rc.dc.OnOpen(func() {
		b, _ := json.Marshal(sessionConfig(persona, "marin"))
		_ = rc.dc.SendText(string(b))
	})
	rc.dc.OnMessage(func(m webrtc.DataChannelMessage) {
		var ev struct {
			Type string `json:"type"`
		}
		_ = json.Unmarshal(m.Data, &ev)
		if ev.Type == "session.updated" {
			cfgOnce.Do(func() { close(configured) })
		}
		h.handleEvent(ctx, m.Data)
	})
	if err := rc.dialOpenAI(ctx, ek); err != nil {
		rc.Close()
		return nil, err
	}
	go func() {
		select {
		case <-configured:
		case <-ctx.Done():
			return
		}
		rc.pumpSendTrack(ctx)
	}()
	return rc, nil
}
