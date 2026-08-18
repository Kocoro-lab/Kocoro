//go:build darwin && cgo

package koe

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
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
		return "", &RealtimeBootstrapError{StatusCode: resp.StatusCode, Body: string(raw)}
	}
	var mint struct {
		Value string `json:"value"`
	}
	if err := json.Unmarshal(raw, &mint); err != nil || mint.Value == "" {
		return "", fmt.Errorf("mint parse failed: %v (body %d bytes)", err, len(raw))
	}
	return mint.Value, nil
}

// RealtimeConn is one connected WebRTC session to OpenAI Realtime.
type RealtimeConn struct {
	pc                   *webrtc.PeerConnection
	sendTrack            *webrtc.TrackLocalStaticSample
	dc                   *webrtc.DataChannel
	dcMu                 sync.RWMutex
	audio                *AudioIO
	interruptOutput      func()
	onLocalSpeechStarted func()
	onLocalSpeechEnded   func()
	onRemoteAudio        func()
	// callActive (nil-safe) gates mic capture: when set and it returns false, the
	// send pump drops mic audio so Koe is NOT listening (Desktop press-to-talk —
	// a call must be started via the control channel). nil = always send (the
	// standalone CLI / E2E always-listen behaviour).
	callActive func() bool
	// fullDuplexAEC means the capture stream has already passed through VPIO's
	// voice processing, so the local mic gate can use a lower post-AEC floor.
	fullDuplexAEC bool
}

// newPeerConnection builds the pion PC with a send track + recvonly transceiver +
// the oai-events data channel. Grounded in spike stage2-webrtc + stage2b-duplex.
func newPeerConnection(audio *AudioIO) (*RealtimeConn, error) {
	return newPeerConnectionForProvider(audio, ProviderOpenAI)
}

func newPeerConnectionForProvider(audio *AudioIO, provider RealtimeProvider) (*RealtimeConn, error) {
	configuration := webrtc.Configuration{}
	if provider == ProviderOpenAI {
		configuration.ICEServers = []webrtc.ICEServer{{URLs: []string{"stun:stun.l.google.com:19302"}}}
	}
	pc, err := webrtc.NewPeerConnection(configuration)
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
				if rc.onRemoteAudio != nil {
					rc.onRemoteAudio()
				}
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

func (rc *RealtimeConn) setDataChannel(dc *webrtc.DataChannel) {
	rc.dcMu.Lock()
	rc.dc = dc
	rc.dcMu.Unlock()
}

func (rc *RealtimeConn) sendText(text string) error {
	rc.dcMu.RLock()
	dc := rc.dc
	rc.dcMu.RUnlock()
	if dc == nil {
		return fmt.Errorf("realtime data channel is not ready")
	}
	return dc.SendText(text)
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
	select {
	case <-webrtc.GatheringCompletePromise(rc.pc):
	case <-ctx.Done():
		return connectError(ProviderOpenAI, "ice", ctx.Err())
	}

	answer, err := exchangeSDP(ctx, openAICallsURL, ek, []byte(rc.pc.LocalDescription().SDP))
	if err != nil {
		return connectError(ProviderOpenAI, "sdp_exchange", err)
	}
	if err := rc.pc.SetRemoteDescription(webrtc.SessionDescription{
		Type: webrtc.SDPTypeAnswer, SDP: answer,
	}); err != nil {
		return connectError(ProviderOpenAI, "remote_description", err)
	}
	return nil
}

func (rc *RealtimeConn) dialQwen(ctx context.Context, exchange func(context.Context, string) (string, error)) error {
	offer, err := rc.pc.CreateOffer(nil)
	if err != nil {
		return connectError(ProviderQwen, "local_setup", err)
	}
	if err := rc.pc.SetLocalDescription(offer); err != nil {
		return connectError(ProviderQwen, "local_setup", err)
	}
	select {
	case <-webrtc.GatheringCompletePromise(rc.pc):
	case <-ctx.Done():
		return connectError(ProviderQwen, "sdp_exchange", ctx.Err())
	}
	answer, err := exchange(ctx, rc.pc.LocalDescription().SDP)
	if err != nil {
		return connectError(ProviderQwen, "sdp_exchange", err)
	}
	if err := rc.pc.SetRemoteDescription(webrtc.SessionDescription{Type: webrtc.SDPTypeAnswer, SDP: answer}); err != nil {
		return connectError(ProviderQwen, "remote_description", err)
	}
	return nil
}

// exchangeSDP sends one create-call request. It deliberately does not replay a
// failed POST: the Realtime calls API does not expose an idempotency contract, so
// an error response cannot prove the provider did not already create a session.
func exchangeSDP(ctx context.Context, url, ek string, offer []byte) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(offer))
	if err != nil {
		return "", err
	}
	req.Header.Set("Authorization", "Bearer "+ek)
	req.Header.Set("Content-Type", "application/sdp")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	answer, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()
	if readErr != nil {
		return "", fmt.Errorf("read SDP exchange response: %w", readErr)
	}
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated {
		return string(answer), nil
	}
	body := string(answer)
	if requestID := resp.Header.Get("x-request-id"); requestID != "" {
		body = fmt.Sprintf("request_id=%s: %s", requestID, body)
	}
	return "", &RealtimeBootstrapError{StatusCode: resp.StatusCode, Body: body}
}

// sendTrackStats reconciles "gate passed N frames" with "track actually wrote M
// frames": when the server goes deaf mid-call the counters rule silent
// WriteSample/encode failures in or out (they are swallowed on the hot path).
type sendTrackStats struct {
	written    uint64
	writeErrs  uint64
	encodeErrs uint64

	segStartPassed     uint64
	segStartWritten    uint64
	segStartWriteErrs  uint64
	segStartEncodeErrs uint64
}

func (s *sendTrackStats) noteWrite(err error) {
	if err != nil {
		s.writeErrs++
		return
	}
	s.written++
}

func (s *sendTrackStats) noteEncodeErr() { s.encodeErrs++ }

// beginSegment snapshots the counters when the mic gate opens; gatePassed is the
// gate's cumulative PassedFrames at that moment.
func (s *sendTrackStats) beginSegment(gatePassed uint64) {
	s.segStartPassed = gatePassed
	s.segStartWritten = s.written
	s.segStartWriteErrs = s.writeErrs
	s.segStartEncodeErrs = s.encodeErrs
}

// segmentLine renders the per-utterance deltas when the gate closes.
func (s *sendTrackStats) segmentLine(gatePassed uint64) string {
	return fmt.Sprintf("gate_passed=%d written=%d write_err=%d encode_err=%d",
		gatePassed-s.segStartPassed,
		s.written-s.segStartWritten,
		s.writeErrs-s.segStartWriteErrs,
		s.encodeErrs-s.segStartEncodeErrs)
}

func (s *sendTrackStats) totalsLine() string {
	return fmt.Sprintf("written=%d write_err=%d encode_err=%d", s.written, s.writeErrs, s.encodeErrs)
}

// pumpSendTrack Opus-encodes captured frames and writes them to the send track.
func (rc *RealtimeConn) pumpSendTrack(ctx context.Context) {
	rc.audio.markSendReady() // unblock the file backend's feedFrames — the session is configured
	gate := newMicNoiseGate()
	if rc.fullDuplexAEC {
		gate = newVPIOMicNoiseGate()
	}
	defer gate.logStats()
	stats := &sendTrackStats{}
	defer func() {
		if eventLogEnabled() || os.Getenv("KOE_AUDIO_LOG") == "1" {
			log.Printf("koe[audio]: send track stats: %s", stats.totalsLine())
		}
	}()
	pacer := time.NewTicker(audioFrameMs * time.Millisecond)
	defer pacer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case frame := <-rc.audio.Frames():
			// Press-to-talk gate: while no call is active, drop captured mic audio so
			// OpenAI never hears the room (Koe stays idle). Drain the frame either way
			// to keep the capture pipeline from backing up.
			if rc.callActive != nil && !rc.callActive() {
				gate.resetState()
				continue
			}
			wasOpen := gate.open
			for _, out := range gate.process(frame) {
				enc, err := rc.audio.EncodeFrame(out)
				if err != nil {
					stats.noteEncodeErr()
					continue
				}
				select {
				case <-ctx.Done():
					return
				case <-pacer.C:
				}
				stats.noteWrite(rc.sendTrack.WriteSample(media.Sample{
					Data: enc, Duration: audioFrameMs * time.Millisecond, // 20 ms frame
				}))
			}
			if !wasOpen && gate.open {
				stats.beginSegment(gate.stats.PassedFrames)
				if rc.onLocalSpeechStarted != nil {
					rc.onLocalSpeechStarted()
				}
			}
			if wasOpen && !gate.open {
				if eventLogEnabled() || os.Getenv("KOE_AUDIO_LOG") == "1" {
					log.Printf("koe[audio]: send segment: %s", stats.segmentLine(gate.stats.PassedFrames))
				}
				if rc.onLocalSpeechEnded != nil {
					rc.onLocalSpeechEnded()
				}
			}
		}
	}
}

// Close tears down the peer connection.
func (rc *RealtimeConn) Close() { _ = rc.pc.Close() }

// InterruptOutput stops any local assistant playback and asks Realtime to cancel
// the active response / clear buffered output. It is an explicit user action, not
// automatic barge-in.
func (rc *RealtimeConn) InterruptOutput() bool {
	if rc == nil || rc.interruptOutput == nil {
		return false
	}
	rc.interruptOutput()
	return true
}

// MintEphemeral is the exported dev-key mint (C-minimal). The deferred daemon mint relay swaps the body
// for a via-daemon call; the signature stays so cmd/koe.go is unchanged.
// DEV-KEY: replaced by the deferred daemon mint relay (→ Plan D Cloud mint).
func MintEphemeral(ctx context.Context, apiKey, model string) (string, error) {
	return mintEphemeral(ctx, apiKey, model)
}

// ConnectOptions carries the optional Desktop/billing hooks (all nil/zero-safe).
type ConnectOptions struct {
	OnVoiceState func(string)          // G2: Desktop control channel voice state (listening/thinking/speaking)
	Model        string                // G3: realtime model id stamped into usage reports
	Voice        string                // realtime output voice (marin/cedar/shimmer/…); empty → "marin" fallback
	OnUsage      func(json.RawMessage) // G3: per-turn usage relay (→ daemon → Cloud)
	// OnEndCall (nil-safe) is invoked when the model calls the end_call voice tool
	// (dismiss / hang up). In the Desktop path it is the endCall closure that plays
	// the goodbye earcon and tears the call down; the standalone/CLI path wires it to
	// a goodbye earcon + process exit; nil only in unit tests.
	OnEndCall func()
	// Language is the user-pinned koe reply language ("en"/"ja"/"zh"; "" = follow the
	// utterance). It selects the language of the mechanical spoken fallbacks (transport
	// failure / busy / misheard / agent clarify); empty defers to per-utterance inference.
	Language string
	// CallActive (nil-safe) gates mic capture for Desktop press-to-talk: when set
	// and it returns false, the send pump drops mic audio (Koe is idle, not
	// listening) until the double-tap (or the menu / settings-configured trigger)
	// starts a call. nil = always send (standalone CLI / E2E always-listen).
	CallActive func() bool
	// ResultMailbox carries completed do_task speech across warm Realtime session
	// replacement. nil creates a connection-local mailbox for standalone callers.
	ResultMailbox *ResultMailbox
	// OnCallState (nil-safe) reports the call lifecycle to Desktop (Q2b feedback so
	// the user knows it's working): "connecting" while the WebRTC/session setup runs
	// (~2s), "on_call" once OpenAI acks the session. "ended" is emitted by the
	// control server on hang-up.
	OnCallState func(string)
	// OnVoiceLevel (nil-safe, D3w) receives (state, rms) at animation cadence while
	// listening/speaking so the Desktop Island sprite tracks the real signal level.
	OnVoiceLevel func(string, float64)
	// FullDuplexAEC selects the VPIO/AEC audio path. Server-side interruption still
	// stays off by default; explicit barge-in experiments opt in via environment.
	FullDuplexAEC bool
	// OnClosed is called when the underlying WebRTC/DataChannel path closes or fails.
	// Desktop warm sessions use this to retire stale idle sessions before the next
	// double-tap can land on a dead connection.
	OnClosed func(error)
}

// defaultSessionConfigTimeoutMS bounds how long Connect waits for OpenAI to ack our
// session.update (session.updated) before giving up. WORKLOAD: a Desktop double-tap
// warms a Realtime session; mint+SDP+session.update normally lands in ~2s. SYMPTOM if
// it binds: a rejected or dropped session.update would otherwise wedge the call in
// "connecting" forever; at 10s Connect returns an error and the caller retires the
// warm session + emits call_state ended. OVERRIDE: KOE_SESSION_CONFIG_TIMEOUT_MS.
const defaultSessionConfigTimeoutMS = 10000

var errSessionConfigTimeout = errors.New("session config timeout")

// sessionConfigWatcher tracks the session.update handshake over the oai-events
// stream so a rejected config can't wedge the call in "connecting". session.updated
// is the ack (closes configured, fires onConfigured once); an error arriving BEFORE
// the ack is a rejected session.update — it records the reason and closes
// configFailed. Extracted from Connect's OnMessage so the wedge-on-rejected-config
// logic is unit-testable without a live peer connection.
type sessionConfigWatcher struct {
	configured   chan struct{}
	configFailed chan struct{}
	cfgOnce      sync.Once
	failOnce     sync.Once
	cfgErr       error // set once inside failOnce before close(configFailed); read only after <-configFailed
	onConfigured func()
}

func newSessionConfigWatcher(onConfigured func()) *sessionConfigWatcher {
	return &sessionConfigWatcher{
		configured:   make(chan struct{}),
		configFailed: make(chan struct{}),
		onConfigured: onConfigured,
	}
}

// observe folds one oai-events frame into the handshake state. An error AFTER the ack
// is a mid-call error (handled by handleEvent's retry path), not a config failure, so
// the pre-ack guard (`select { case <-configured: }`) skips it.
func (w *sessionConfigWatcher) observe(raw []byte) {
	var ev struct {
		Type  string `json:"type"`
		Error struct {
			Code    string `json:"code"`
			Type    string `json:"type"`
			Message string `json:"message"`
		} `json:"error"`
	}
	_ = json.Unmarshal(raw, &ev)
	switch ev.Type {
	case "session.updated":
		w.cfgOnce.Do(func() {
			close(w.configured)
			if w.onConfigured != nil {
				w.onConfigured()
			}
		})
	case "error":
		select {
		case <-w.configured:
		default:
			w.failOnce.Do(func() {
				w.cfgErr = fmt.Errorf("session config rejected before ack: code=%q type=%q message=%q", ev.Error.Code, ev.Error.Type, ev.Error.Message)
				close(w.configFailed)
			})
		}
	}
}

// wait blocks until the session is configured, a pre-ack error arrives, the timeout
// elapses, or ctx is cancelled. Only a clean ack returns nil; every other exit is an
// error so Connect's caller retires the wedged session and emits call_state ended.
func (w *sessionConfigWatcher) wait(ctx context.Context, timeout time.Duration) error {
	select {
	case <-w.configured:
		return nil
	case <-w.configFailed:
		return w.cfgErr
	case <-time.After(timeout):
		return fmt.Errorf("%w after %s", errSessionConfigTimeout, timeout)
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Connect builds the peer connection, dials OpenAI, configures the session, and
// starts the send-pump + event-dispatch loops. Returns once connected. opts
// carries the optional Desktop (G2) + billing (G3) hooks.
func Connect(ctx context.Context, audio *AudioIO, ek, persona string, state *CallState, disp *Dispatcher, opts ConnectOptions) (*RealtimeConn, error) {
	return connectRealtime(ctx, audio, ProviderOpenAI, persona, state, disp, opts, func(rc *RealtimeConn) error {
		return rc.dialOpenAI(ctx, ek)
	})
}

// ConnectQwen builds a Qwen WebRTC session. The caller supplies a one-shot SDP
// exchange backed by daemon→Cloud; no long-lived Qwen credential enters Koe.
func ConnectQwen(ctx context.Context, audio *AudioIO, exchange func(context.Context, string) (string, error), persona string, state *CallState, disp *Dispatcher, opts ConnectOptions) (*RealtimeConn, error) {
	return connectRealtime(ctx, audio, ProviderQwen, persona, state, disp, opts, func(rc *RealtimeConn) error {
		return rc.dialQwen(ctx, exchange)
	})
}

func connectRealtime(ctx context.Context, audio *AudioIO, provider RealtimeProvider, persona string, state *CallState, disp *Dispatcher, opts ConnectOptions, dial func(*RealtimeConn) error) (*RealtimeConn, error) {
	rc, err := newPeerConnectionForProvider(audio, provider)
	if err != nil {
		return nil, connectError(provider, "local_setup", err)
	}
	h := newEventHandlerWithMailbox(disp, state, audio, func(v any) error {
		b, _ := json.Marshal(v)
		return rc.sendText(string(b))
	}, opts.ResultMailbox, opts.CallActive)
	h.onVoiceState = opts.OnVoiceState
	h.onEndCall = opts.OnEndCall
	h.provider = string(provider)
	h.model = opts.Model
	h.onUsage = opts.OnUsage
	h.language = opts.Language
	h.fullDuplexAEC = opts.FullDuplexAEC
	rc.interruptOutput = h.interruptOutput
	rc.onLocalSpeechStarted = h.observeLocalSpeechStarted
	rc.onLocalSpeechEnded = func() { h.observeLocalSpeechEnded(ctx) }
	rc.callActive = opts.CallActive
	rc.fullDuplexAEC = opts.FullDuplexAEC
	if provider == ProviderQwen {
		rc.onRemoteAudio = func() {
			if !h.outputBufferActive.Swap(true) {
				h.outputStartedAt = time.Now()
			}
			h.markSpeaking()
		}
	}
	var closedOnce sync.Once
	var live atomic.Bool
	transportFailed := make(chan error, 1)
	notifyClosed := func(err error) {
		select {
		case transportFailed <- err:
		default:
		}
		if opts.OnClosed == nil || !live.Load() {
			return
		}
		closedOnce.Do(func() { opts.OnClosed(err) })
	}
	rc.pc.OnConnectionStateChange(func(s webrtc.PeerConnectionState) {
		switch s {
		case webrtc.PeerConnectionStateFailed, webrtc.PeerConnectionStateClosed:
			notifyClosed(fmt.Errorf("peer connection %s", s.String()))
		}
	})
	go h.runResponseSender(ctx) // serialized response.create for tool-result voicing
	if opts.OnCallState != nil {
		opts.OnCallState("connecting") // Q2b: the ~2s mint+SDP+session.update setup
	}
	// watcher.configured closes when OpenAI acks our session.update. The send pump
	// waits on it: if mic audio reaches the server before the tools/voice config
	// lands, the VAD-triggered auto response snapshots the default config (no tools)
	// and the first turn can't delegate. Verified against the live API in e2e_test.go.
	// A REJECTED session.update (bad model/tool schema/auth) sends an error instead of
	// session.updated, which would leave configured un-closed forever and wedge the
	// call in "connecting"; the watcher closes configFailed on that so Connect returns
	// an error (below) and the caller retires the session.
	watcher := newSessionConfigWatcher(func() {
		live.Store(true)
		if opts.OnCallState != nil {
			opts.OnCallState("on_call") // session is live — Koe is ready
		}
	})
	openAIVoice := opts.Voice
	if openAIVoice == "" {
		openAIVoice = DefaultOpenAIRealtimeVoice
	}
	qwenVoice := opts.Voice
	if qwenVoice == "" {
		qwenVoice = DefaultQwenRealtimeVoice
	}
	var sendConfigOnce sync.Once
	sendConfig := func(dc *webrtc.DataChannel) {
		sendConfigOnce.Do(func() {
			rc.setDataChannel(dc)
			var payload map[string]any
			if provider == ProviderQwen {
				payload = qwenSessionConfig(persona, qwenVoice)
			} else {
				payload = sessionConfig(persona, openAIVoice, opts.FullDuplexAEC)
			}
			b, _ := json.Marshal(payload)
			if err := dc.SendText(string(b)); err != nil {
				notifyClosed(fmt.Errorf("send session config: %w", err))
			}
		})
	}
	wireDataChannel := func(dc *webrtc.DataChannel, configureOnOpen bool) {
		dc.OnOpen(func() {
			if configureOnOpen {
				sendConfig(dc)
			}
		})
		dc.OnClose(func() { notifyClosed(fmt.Errorf("data channel %q closed", dc.Label())) })
		dc.OnError(func(err error) { notifyClosed(fmt.Errorf("data channel %q error: %w", dc.Label(), err)) })
		dc.OnMessage(func(m webrtc.DataChannelMessage) {
			if provider == ProviderQwen {
				var envelope struct {
					Type string `json:"type"`
				}
				_ = json.Unmarshal(m.Data, &envelope)
				if envelope.Type == "session.created" {
					sendConfig(dc)
				}
			}
			watcher.observe(m.Data)
			h.handleEvent(ctx, m.Data)
		})
	}
	wireDataChannel(rc.dc, provider == ProviderOpenAI)
	if provider == ProviderQwen {
		rc.pc.OnDataChannel(func(dc *webrtc.DataChannel) {
			if dc.Label() == "txt" {
				rc.setDataChannel(dc)
			}
			wireDataChannel(dc, false)
		})
	}
	if err := dial(rc); err != nil {
		rc.Close()
		return nil, err
	}
	// Block until the session is configured before returning success. Previously
	// Connect returned as soon as the dial succeeded, so a rejected/silent
	// session.update left the send pump parked on `configured` forever and the call
	// wedged in "connecting" (no on_call, no ended). A pre-ack error or the timeout
	// now surfaces as an error; both callers cancel ctx on the error path (standalone
	// runKoeCall via defer cancel, warm path via stopSessionResources), so the send /
	// level pumps below unblock and the session is retired + emits call_state ended.
	timeout := time.Duration(koeEnvInt("KOE_SESSION_CONFIG_TIMEOUT_MS", defaultSessionConfigTimeoutMS)) * time.Millisecond
	waitCtx, waitCancel := context.WithCancel(ctx)
	waitResult := make(chan error, 1)
	go func() { waitResult <- watcher.wait(waitCtx, timeout) }()
	var waitErr error
	select {
	case waitErr = <-waitResult:
	case transportErr := <-transportFailed:
		waitErr = connectError(provider, "data_channel", transportErr)
	}
	waitCancel()
	if waitErr != nil {
		rc.Close()
		var ce *RealtimeConnectError
		if errors.As(waitErr, &ce) {
			return nil, waitErr
		}
		stage := "session_config_rejected"
		if errors.Is(waitErr, errSessionConfigTimeout) {
			stage = "session_ready_timeout"
		}
		return nil, connectError(provider, stage, waitErr)
	}
	go func() {
		select {
		case <-watcher.configured:
		case <-ctx.Done():
			return
		}
		rc.pumpSendTrack(ctx)
	}()
	// Level pump (D3w): emit the reactive RMS amplitude for the Desktop Island sprite
	// at animation cadence while listening/speaking. thinking/idle carry no level
	// (the sprite is self-driven there).
	if opts.OnVoiceLevel != nil {
		go func() {
			ticker := time.NewTicker(levelPumpInterval)
			defer ticker.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-ticker.C:
					// thinking is self-driven (no reactive level). speaking → output
					// amplitude. Everything else (idle before the first event, or
					// listening) means the mic is open → report the input amplitude.
					switch h.voiceState() {
					case "thinking":
					case "speaking":
						opts.OnVoiceLevel("speaking", audio.OutputLevel())
					default:
						opts.OnVoiceLevel("listening", audio.InputLevel())
					}
				}
			}
		}()
	}
	return rc, nil
}

// levelPumpInterval is the cadence of D3w reactive-amplitude updates. WORKLOAD: a
// Desktop sprite animating to the voice level; SYMPTOM if too slow: choppy/laggy
// amplitude; if too fast: needless SSE traffic. ~80 ms (~12 fps) is smooth for a
// small sprite while staying well under the 20 ms audio-frame rate it samples.
const levelPumpInterval = 80 * time.Millisecond
