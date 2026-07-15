package koe

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
)

// Sentinel rejections for POST /call/mic — the body of the 409 response, so
// Desktop can distinguish "no call" from "task already drained" races.
var (
	ErrNoActiveCall  = errors.New("no_active_call")
	ErrNoTaskPending = errors.New("no_task_pending")
)

// controlEvent is one Koe→Desktop SSE payload, discriminated by Type. Wire shapes
// are pinned in Plan E (the Desktop client) — keep them byte-identical:
//
//	{"type":"voice_state","state":"idle"|"listening"|"thinking"|"speaking"[,"level":0..1][,"task_pending":true][,"mic":"off"]}
//	{"type":"control_app","action":"show"|"hide"|"new_conversation"|"open_settings"}
//	{"type":"call_state","state":"connecting"|"on_call"|"ended"}
//	{"type":"bridge_status","state":"disabled"|"connecting"|"connected"|"degraded"}
//
// level (D3w) is an additive, omitempty field carrying the reactive RMS amplitude
// while listening (input) / speaking (output) so the Desktop Island sprite tracks
// the real signal instead of a canned animation; absent on transition events and
// for thinking/idle. task_pending and mic are additive, omit-when-default snapshot
// fields stamped on every voice_state (koe-mic-off): absent task_pending means
// false (no do_task in flight), absent mic means "on". The koe↔Desktop control
// channel is localhost-only and may be protected by a Desktop-generated Bearer
// token; these fields remain additive-only.
type controlEvent struct {
	Type        string  `json:"type"`
	State       string  `json:"state,omitempty"`        // voice_state / call_state
	Action      string  `json:"action,omitempty"`       // control_app
	Level       float64 `json:"level,omitempty"`        // voice_state reactive RMS amplitude (0..1)
	TaskPending bool    `json:"task_pending,omitempty"` // voice_state: a do_task is in flight (koe-mic-off)
	Mic         string  `json:"mic,omitempty"`          // voice_state: "off" while user mic-off; absent = on
}

// StartCallRequest is the optional Desktop→Koe context payload for POST
// /call/start. Older Desktop builds send only {"command":"start_call"}; that
// still decodes with zero values.
type StartCallRequest struct {
	CWD            string          `json:"cwd,omitempty"`
	ForegroundHint *ForegroundHint `json:"foreground_hint,omitempty"`
}

// ControlServer is the Koe-side HTTP+SSE control channel for Kocoro Desktop: it
// accepts POST /call/start|end and broadcasts GET /events (voice_state,
// control_app, call_state). This is the SERVER half of the Desktop↔Koe contract
// (Desktop is the client, same shape as Desktop→daemon). It satisfies Plan B's
// ControlAppFunc seam: when the model calls control_app, the dispatcher's hook
// calls EmitControlApp and Desktop performs the actual window action.
type ControlServer struct {
	mu           sync.Mutex
	subscribers  map[chan controlEvent]struct{}
	onStart      func(StartCallRequest) // Desktop pressed talk: start a call
	onEnd        func()                 // Desktop ended: tear the call down
	onInterrupt  func()                 // Desktop explicitly interrupted playback
	onMic        func(off bool) error   // POST /call/mic (nil until SetMicHandler)
	taskPending  func() bool            // nil-safe snapshot providers, stamped on every voice_state
	micOff       func() bool
	token        string       // optional Bearer token for Desktop-owned requests
	lastVoice    atomic.Value // string: last voice_state, replayed by ReemitVoiceState
	lastBridge   atomic.Value // string: latest bridge_status, exposed by /carrier/status
	lastCall     atomic.Value // string: current call snapshot; ended normalizes back to idle
	lastRealtime atomic.Value // string: disconnected | connecting | connected

	carrier                        *CarrierProfile // resolved carrier identity for GET /carrier/status (nil = not injected)
	carrierBound                   func() bool     // reports audio.bound; nil → false
	wirelessAudioSocketConfigured  atomic.Bool
	wirelessAudioVerified          atomic.Bool
	wirelessCameraSocketConfigured atomic.Bool
	wirelessCameraVerified         atomic.Bool
	bridgeDetails                  func() (proto, bridgeVersion string)
}

// NewControlServer wires the Desktop-driven start/end callbacks (either may be nil).
func NewControlServer(onStart func(StartCallRequest), onEnd func(), onInterrupt func()) *ControlServer {
	s := &ControlServer{
		subscribers: make(map[chan controlEvent]struct{}),
		onStart:     onStart,
		onEnd:       onEnd,
		onInterrupt: onInterrupt,
	}
	s.lastBridge.Store(bridgeStateDisabled)
	s.lastCall.Store("idle")
	s.lastRealtime.Store("disconnected")
	return s
}

// SetMicHandler wires POST /call/mic. Called once at startup, before Handler()
// serves — no locking needed.
func (s *ControlServer) SetMicHandler(h func(off bool) error) { s.onMic = h }

// SetToken enables Bearer-token auth for every control request. Empty preserves
// older Desktop builds and test harnesses.
func (s *ControlServer) SetToken(token string) { s.token = token }

// SetSnapshotProviders wires the task_pending / mic snapshot stamped on every
// voice_state event. Providers must be sessMu-free (EmitVoiceState is called
// with cmd/koe.go's session mutex held) — cmd/koe.go passes atomic-pointer
// reads over CallState/AudioIO, whose own locks are safe here.
func (s *ControlServer) SetSnapshotProviders(taskPending, micOff func() bool) {
	s.taskPending = taskPending
	s.micOff = micOff
}

// SetCarrierProfile wires GET /carrier/status. Called once at startup before
// Handler() serves — no locking needed. audioBound reports whether Koe is bound
// to explicit device UIDs rather than the system default (§03 audio.bound); nil
// reports false.
func (s *ControlServer) SetCarrierProfile(p CarrierProfile, audioBound func() bool) {
	s.carrier = &p
	s.carrierBound = audioBound
}

// SetWirelessAudioStatus records the startup carrier hello result without
// retaining the probe connection. A verified=true snapshot means Koe completed
// the v0.2 hello against the configured UDS during startup; it does not mean an
// idle media session is being held open.
func (s *ControlServer) SetWirelessAudioStatus(socketConfigured, verified bool) {
	s.wirelessAudioSocketConfigured.Store(socketConfigured)
	s.wirelessAudioVerified.Store(verified)
}

// SetWirelessCameraStatus records the no-capture v0.1 startup probe result.
func (s *ControlServer) SetWirelessCameraStatus(socketConfigured, verified bool) {
	s.wirelessCameraSocketConfigured.Store(socketConfigured)
	s.wirelessCameraVerified.Store(verified)
}

// SetBridgeDetailsProvider exposes the current motion hello metadata. The
// provider is consulted only for a connected bridge snapshot.
func (s *ControlServer) SetBridgeDetailsProvider(provider func() (proto, bridgeVersion string)) {
	s.bridgeDetails = provider
}

// SetRealtimeState updates the runtime snapshot reported by /carrier/status.
// Callers drive it from the actual mint/connect/close state machine.
func (s *ControlServer) SetRealtimeState(state string) { s.lastRealtime.Store(state) }

func (s *ControlServer) stampVoice(ev controlEvent) controlEvent {
	if s.taskPending != nil && s.taskPending() {
		ev.TaskPending = true
	}
	if s.micOff != nil && s.micOff() {
		ev.Mic = "off"
	}
	return ev
}

// ReemitVoiceState replays the last voice state with a fresh task/mic snapshot —
// the /call/mic flow calls it so Desktop sees the flip immediately instead of
// waiting for the next natural voice_state transition.
func (s *ControlServer) ReemitVoiceState() {
	if v, ok := s.lastVoice.Load().(string); ok && v != "" {
		s.broadcast(s.stampVoice(controlEvent{Type: "voice_state", State: v}))
	}
}

// Handler returns the localhost mux for `shan koe --control-port N`.
func (s *ControlServer) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /call/start", func(w http.ResponseWriter, r *http.Request) {
		var req StartCallRequest
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil && err != io.EOF {
			http.Error(w, "invalid start payload", http.StatusBadRequest)
			return
		}
		if s.onStart != nil {
			go s.onStart(req)
		}
		writeControlOK(w)
	})
	mux.HandleFunc("POST /call/end", func(w http.ResponseWriter, r *http.Request) {
		if s.onEnd != nil {
			s.onEnd()
		}
		writeControlOK(w)
	})
	mux.HandleFunc("POST /call/interrupt", func(w http.ResponseWriter, r *http.Request) {
		if s.onInterrupt != nil {
			s.onInterrupt()
		}
		writeControlOK(w)
	})
	mux.HandleFunc("POST /call/mic", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Mic string `json:"mic"`
		}
		if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&req); err != nil || (req.Mic != "on" && req.Mic != "off") {
			http.Error(w, `{"error":"invalid_mic"}`, http.StatusBadRequest)
			return
		}
		if s.onMic == nil {
			writeControlOK(w)
			return
		}
		if err := s.onMic(req.Mic == "off"); err != nil {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusConflict)
			_, _ = w.Write([]byte(`{"error":"` + err.Error() + `"}`))
			return
		}
		writeControlOK(w)
	})
	mux.HandleFunc("GET /events", s.handleEvents)
	mux.HandleFunc("GET /carrier/status", func(w http.ResponseWriter, r *http.Request) {
		s.writeCarrierStatus(w)
	})
	if s.token == "" {
		return mux
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorizedControlRequest(r, s.token) {
			writeControlUnauthorized(w)
			return
		}
		mux.ServeHTTP(w, r)
	})
}

func authorizedControlRequest(r *http.Request, token string) bool {
	got, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || got == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(got), []byte(token)) == 1
}

func writeControlUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_, _ = w.Write([]byte(`{"error":"unauthorized"}`))
}

func writeControlOK(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte(`{"status":"ok"}`))
}

func (s *ControlServer) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	ch := make(chan controlEvent, 16)
	s.mu.Lock()
	s.subscribers[ch] = struct{}{}
	s.mu.Unlock()
	defer func() {
		s.mu.Lock()
		delete(s.subscribers, ch)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	flusher.Flush()

	for {
		select {
		case <-r.Context().Done():
			return
		case ev := <-ch:
			b, _ := json.Marshal(ev)
			_, _ = w.Write([]byte("data: " + string(b) + "\n\n"))
			flusher.Flush()
		}
	}
}

func (s *ControlServer) broadcast(ev controlEvent) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch := range s.subscribers {
		select {
		case ch <- ev:
		default: // drop on a wedged subscriber rather than block the call loop
		}
	}
}

// EmitVoiceState pushes the ambient voice state to Desktop (drives the Island
// sprite), stamped with the task_pending/mic snapshot (koe-mic-off).
func (s *ControlServer) EmitVoiceState(state string) {
	s.lastVoice.Store(state)
	s.broadcast(s.stampVoice(controlEvent{Type: "voice_state", State: state}))
}

// EmitVoiceLevel pushes a voice_state with the reactive RMS amplitude (D3w): the
// level pump calls this at animation cadence while listening/speaking so the sprite
// tracks the real signal. Same event type as EmitVoiceState — just with level set,
// stamped like EmitVoiceState.
func (s *ControlServer) EmitVoiceLevel(state string, level float64) {
	s.lastVoice.Store(state)
	s.broadcast(s.stampVoice(controlEvent{Type: "voice_state", State: state, Level: level}))
}

// EmitControlApp asks Desktop to perform a window action (the control_app tool).
func (s *ControlServer) EmitControlApp(action string) {
	s.broadcast(controlEvent{Type: "control_app", Action: action})
}

// EmitCallState reports the call lifecycle to Desktop.
func (s *ControlServer) EmitCallState(state string) {
	snapshot := state
	if state == "ended" {
		snapshot = "idle"
	}
	s.lastCall.Store(snapshot)
	s.broadcast(controlEvent{Type: "call_state", State: state})
}

// EmitMicStatus reports microphone health to Desktop. "silent" = the bound input
// produced no signal for the watchdog window while capture was expected (e.g. a
// clamshell built-in mic with the lid shut); "ok" = input recovered. Additive
// event — older Desktop builds skip the unknown type, so it is safe to emit
// unconditionally.
func (s *ControlServer) EmitMicStatus(status string) {
	s.broadcast(controlEvent{Type: "mic_status", State: status})
}

// EmitBridgeStatus reports the motion-bridge connection state to Desktop (the
// Kocoro Robot card "motion" light). Additive flat event; M1 always emits
// "disabled" (no bridge yet), M3 drives connecting/connected/degraded.
func (s *ControlServer) EmitBridgeStatus(state string) {
	s.lastBridge.Store(state)
	s.broadcast(controlEvent{Type: "bridge_status", State: state})
}

// bridgeStateDisabled is the only bridge state Koe reports in M1 (no motion
// bridge yet); M3 drives connecting/connected/degraded.
const bridgeStateDisabled = "disabled"

// carrierStatusResponse is the GET /carrier/status body (desktop-koe-carrier-
// control-spec §3). Additive + field-presence-gated on the Desktop side: an old
// Koe with no route 404s, which Desktop reads as "feature absent".
type carrierStatusResponse struct {
	Carrier       string               `json:"carrier"`
	Caps          []string             `json:"caps"`
	Audio         carrierAudioStatus   `json:"audio"`
	Camera        *carrierCameraStatus `json:"camera,omitempty"`
	Bridge        carrierBridgeStatus  `json:"bridge"`
	Model         string               `json:"model"`
	Agent         string               `json:"agent"`
	CallState     string               `json:"call_state,omitempty"`
	RealtimeState string               `json:"realtime_state,omitempty"`
}

type carrierAudioStatus struct {
	Backend          string `json:"backend"`
	MicUID           string `json:"mic_uid"`
	SpeakerUID       string `json:"speaker_uid"`
	Bound            bool   `json:"bound"`
	Transport        string `json:"transport,omitempty"`
	State            string `json:"state,omitempty"`
	WireRateHz       int    `json:"wire_rate_hz,omitempty"`
	SocketConfigured *bool  `json:"socket_configured,omitempty"`
}

type carrierBridgeStatus struct {
	State         string `json:"state"`
	Proto         string `json:"proto,omitempty"`
	BridgeVersion string `json:"bridge_version,omitempty"`
}

type carrierCameraStatus struct {
	State            string `json:"state"`
	Transport        string `json:"transport"`
	Proto            string `json:"proto,omitempty"`
	SocketConfigured bool   `json:"socket_configured"`
}

func (s *ControlServer) writeCarrierStatus(w http.ResponseWriter) {
	var p CarrierProfile
	if s.carrier != nil {
		p = *s.carrier
	}
	caps := p.Caps
	if caps == nil {
		caps = []string{} // never serialize null — Desktop gates on membership
	}
	bound := false
	if s.carrierBound != nil {
		bound = s.carrierBound()
	}
	bridgeState := bridgeStateDisabled
	if state, ok := s.lastBridge.Load().(string); ok && state != "" {
		bridgeState = state
	}
	audioStatus := carrierAudioStatus{
		Backend:    p.AudioBackend,
		MicUID:     p.MicUID,
		SpeakerUID: p.SpeakerUID,
		Bound:      bound,
	}
	bridgeStatus := carrierBridgeStatus{State: bridgeState}
	callState := ""
	realtimeState := ""
	var cameraStatus *carrierCameraStatus
	if p.Carrier == CarrierReachyWireless {
		socketConfigured := s.wirelessAudioSocketConfigured.Load()
		audioState := "unavailable"
		if s.wirelessAudioVerified.Load() {
			audioState = "connected"
		}
		audioStatus = carrierAudioStatus{
			Backend:          "carrier_uds",
			Bound:            false,
			Transport:        "uds",
			State:            audioState,
			WireRateHz:       wirelessCarrierWireRate,
			SocketConfigured: &socketConfigured,
		}
		cameraConfigured := s.wirelessCameraSocketConfigured.Load()
		cameraState := "unavailable"
		cameraProto := ""
		if s.wirelessCameraVerified.Load() {
			cameraState = "ready"
			cameraProto = cameraSnapshotProto
		}
		cameraStatus = &carrierCameraStatus{
			State: cameraState, Transport: "uds", Proto: cameraProto, SocketConfigured: cameraConfigured,
		}
		if value, ok := s.lastCall.Load().(string); ok {
			callState = value
		}
		if value, ok := s.lastRealtime.Load().(string); ok {
			realtimeState = value
		}
	}
	if bridgeState == bridgeStateConnected && s.bridgeDetails != nil {
		bridgeStatus.Proto, bridgeStatus.BridgeVersion = s.bridgeDetails()
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(carrierStatusResponse{
		Carrier:       p.Carrier,
		Caps:          caps,
		Audio:         audioStatus,
		Camera:        cameraStatus,
		Bridge:        bridgeStatus,
		Model:         p.Model,
		Agent:         p.Agent,
		CallState:     callState,
		RealtimeState: realtimeState,
	})
}
