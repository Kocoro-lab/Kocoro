// Package koebind exposes the Koe front brain to iOS through gomobile.
//
// Everything exported here must be gomobile-safe: signed ints, floats, string,
// bool, []byte, error, and interfaces/structs declared in THIS package. No maps,
// no struct slices, no multi-return. gobind does not skip unsupported exported
// API — it crashes with a nil dereference — which is why the brain package is
// never bound directly and this wrapper stays deliberately narrow.
package koebind

import (
	"github.com/Kocoro-lab/ShanClaw/internal/koe"
)

// AudioSink is implemented in Swift by the app's audio layer. gomobile turns it
// into an Objective-C protocol.
//
// These are exactly the calls the brain makes into audio, and every one of them
// is boolean state — no audio samples cross this boundary, which is what keeps
// the bridge off the realtime path.
type AudioSink interface {
	SetSpeaking(s bool)
	SetPlaybackEnabled(s bool)
	SetPlaybackPaused(paused bool)
	DropCapture() bool
	UserMicOff() bool
	SetUserMicOff(off bool)
	UserMicSticky() bool
	PlaybackIdle() bool
}

// EventSender is implemented in Swift: it puts one Realtime client event onto
// the WebRTC data channel. The brain never owns a socket.
type EventSender interface {
	Send(payloadJSON string) error
}

// ControlHost is implemented in Swift: the brain asking the app's UI to act.
type ControlHost interface {
	ControlApp(action string)
}

// sinkAdapter bridges the Swift-facing interface to the brain's own. They differ
// only in that koe keeps dropCapture unexported on its internal interface.
type sinkAdapter struct{ s AudioSink }

func (a sinkAdapter) SetSpeaking(v bool)        { a.s.SetSpeaking(v) }
func (a sinkAdapter) SetPlaybackEnabled(v bool) { a.s.SetPlaybackEnabled(v) }
func (a sinkAdapter) SetPlaybackPaused(v bool)  { a.s.SetPlaybackPaused(v) }
func (a sinkAdapter) DropCapture() bool         { return a.s.DropCapture() }
func (a sinkAdapter) UserMicOff() bool          { return a.s.UserMicOff() }
func (a sinkAdapter) SetUserMicOff(v bool)      { a.s.SetUserMicOff(v) }
func (a sinkAdapter) UserMicSticky() bool       { return a.s.UserMicSticky() }
func (a sinkAdapter) PlaybackIdle() bool        { return a.s.PlaybackIdle() }

// Bridge is the single bound object; the app holds one per call.
type Bridge struct {
	session *koe.Session
}

// NewBridge starts a call's front brain against Swift-supplied audio and
// transport. backendURL is Shannon Cloud on iOS, where there is no local daemon.
func NewBridge(
	burstID string,
	boundAgent string,
	backendURL string,
	sink AudioSink,
	sender EventSender,
	host ControlHost,
) *Bridge {
	cfg := koe.SessionConfig{
		BurstID:    burstID,
		BoundAgent: boundAgent,
		BackendURL: backendURL,
	}
	// gomobile hands over a non-nil interface wrapping a nil Objective-C object
	// when Swift passes nil, so each hook is guarded rather than assumed present.
	if sink != nil {
		cfg.Audio = sinkAdapter{s: sink}
	}
	if sender != nil {
		cfg.Send = sender.Send
	}
	if host != nil {
		cfg.ControlApp = host.ControlApp
	}
	return &Bridge{session: koe.NewSession(cfg)}
}

// HandleEvent feeds one Realtime server event, raw JSON straight off the data
// channel. This is the front brain's only input.
func (b *Bridge) HandleEvent(raw []byte) { b.session.HandleEvent(raw) }

// BurstID identifies this call's task lineage.
func (b *Bridge) BurstID() string { return b.session.BurstID() }

// SentEventCount is how many client events the brain has emitted — the cheapest
// way for the app to assert the brain reacted at all.
func (b *Bridge) SentEventCount() int64 { return b.session.SentEventCount() }

// SendSessionUpdate configures the realtime session — instructions, the seven
// voice tools, turn detection, voice. Call it the moment the event channel
// opens; a call that skips it connects and then does nothing at all.
func (b *Bridge) SendSessionUpdate(persona string, voice string, fullDuplexAEC bool) error {
	return b.session.SendSessionUpdate(persona, voice, fullDuplexAEC)
}
