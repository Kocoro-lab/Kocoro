package koe

import (
	"encoding/json"
	"io"
	"net/http"
	"sync"
)

// controlEvent is one Koe→Desktop SSE payload, discriminated by Type. Wire shapes
// are pinned in Plan E (the Desktop client) — keep them byte-identical:
//
//	{"type":"voice_state","state":"idle"|"listening"|"thinking"|"speaking"[,"level":0..1]}
//	{"type":"control_app","action":"show"|"hide"|"new_conversation"|"open_settings"}
//	{"type":"call_state","state":"connecting"|"on_call"|"ended"}
//
// level (D3w) is an additive, omitempty field carrying the reactive RMS amplitude
// while listening (input) / speaking (output) so the Desktop Island sprite tracks
// the real signal instead of a canned animation; absent on transition events and
// for thinking/idle. The koe↔Desktop control channel has no handshake, so this is
// additive-only (old Desktop ignores the field) — no capability token applies here.
type controlEvent struct {
	Type   string  `json:"type"`
	State  string  `json:"state,omitempty"`  // voice_state / call_state
	Action string  `json:"action,omitempty"` // control_app
	Level  float64 `json:"level,omitempty"`  // voice_state reactive RMS amplitude (0..1)
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
	mu          sync.Mutex
	subscribers map[chan controlEvent]struct{}
	onStart     func(StartCallRequest) // Desktop pressed talk: start a call
	onEnd       func()                 // Desktop ended: tear the call down
}

// NewControlServer wires the Desktop-driven start/end callbacks (either may be nil).
func NewControlServer(onStart func(StartCallRequest), onEnd func()) *ControlServer {
	return &ControlServer{
		subscribers: make(map[chan controlEvent]struct{}),
		onStart:     onStart,
		onEnd:       onEnd,
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
			s.onStart(req)
		}
		writeControlOK(w)
	})
	mux.HandleFunc("POST /call/end", func(w http.ResponseWriter, r *http.Request) {
		if s.onEnd != nil {
			s.onEnd()
		}
		writeControlOK(w)
	})
	mux.HandleFunc("GET /events", s.handleEvents)
	return mux
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

// EmitVoiceState pushes the ambient voice state to Desktop (drives the Island sprite).
func (s *ControlServer) EmitVoiceState(state string) {
	s.broadcast(controlEvent{Type: "voice_state", State: state})
}

// EmitVoiceLevel pushes a voice_state with the reactive RMS amplitude (D3w): the
// level pump calls this at animation cadence while listening/speaking so the sprite
// tracks the real signal. Same event type as EmitVoiceState — just with level set.
func (s *ControlServer) EmitVoiceLevel(state string, level float64) {
	s.broadcast(controlEvent{Type: "voice_state", State: state, Level: level})
}

// EmitControlApp asks Desktop to perform a window action (the control_app tool).
func (s *ControlServer) EmitControlApp(action string) {
	s.broadcast(controlEvent{Type: "control_app", Action: action})
}

// EmitCallState reports the call lifecycle to Desktop.
func (s *ControlServer) EmitCallState(state string) {
	s.broadcast(controlEvent{Type: "call_state", State: state})
}
