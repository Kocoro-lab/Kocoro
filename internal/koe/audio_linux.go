//go:build linux

package koe

import (
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/koe/audiobridge"
	opus "gopkg.in/hraban/opus.v2"
)

// inputBufferFrames / playbackIdleLevelEps mirror the darwin audio.go values (they
// live there behind the darwin tag; defined here for the linux backend). Behaviour
// is identical — the cold-start capture buffer depth and the playback-idle RMS floor.
const (
	inputBufferFrames    = 256
	playbackIdleLevelEps = 0.005
)

// AudioIO is the linux (Wireless / W-中, direction A) audio backend. Unlike the
// darwin backend (malgo capture + oto playback + VPIO, audio.go), the daemon owns
// the CM4 sound card, so audio reaches Koe over a local UDS from the Python audio
// carrier that bridges the daemon's WebRTC (koe-audio-carrier-spec.md). The neutral
// half — the Opus codec, the half-duplex gate, the frames/playBuf channels, the
// level meters — mirrors the darwin core exactly; only the device layer (Start/Stop
// + the UDS pumps) differs. VPIO is macOS-only, so its knobs are inert no-ops here.
type AudioIO struct {
	enc     *opus.Encoder
	dec     *opus.Decoder
	frames  chan []int16
	playBuf chan []int16

	speaking      atomic.Bool
	userMicOff    atomic.Bool
	userMicSticky atomic.Bool
	playback      atomic.Bool
	encMu         sync.Mutex
	decMu         sync.Mutex
	stopOnce      sync.Once
	sendReady     chan struct{}
	sendReadyOnce sync.Once

	inLevel  atomic.Uint64
	outLevel atomic.Uint64

	// UDS device: the carrier link. socketPath is injected (never derived, §2).
	socketPath string
	conn       net.Conn
	done       chan struct{}
	wg         sync.WaitGroup
	upSeq      atomic.Uint32 // spk uplink frame seq
}

// NewAudioIO builds the codec (no UDS opened yet — Start() dials the carrier, so
// unit tests exercise Encode/Decode/gate without a carrier present). Mirror of the
// darwin NewAudioIO so cmd/koe.go is platform-agnostic.
func NewAudioIO() (*AudioIO, error) {
	enc, err := opus.NewEncoder(audioSampleRate, audioChannels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	dec, err := opus.NewDecoder(audioSampleRate, audioChannels)
	if err != nil {
		return nil, err
	}
	a := &AudioIO{
		enc:       enc,
		dec:       dec,
		frames:    make(chan []int16, inputBufferFrames),
		playBuf:   make(chan []int16, 256),
		sendReady: make(chan struct{}),
		done:      make(chan struct{}),
	}
	a.playback.Store(true)
	return a, nil
}

// SetAudioSocket injects the carrier UDS path (from the carrier control plane).
func (a *AudioIO) SetAudioSocket(path string) { a.socketPath = path }

// ---- neutral half (mirrors audio.go; kept behaviourally identical) ----

func (a *AudioIO) setInputLevel(l float64)  { a.inLevel.Store(math.Float64bits(l)) }
func (a *AudioIO) setOutputLevel(l float64) { a.outLevel.Store(math.Float64bits(l)) }

func (a *AudioIO) InputLevel() float64 {
	if a.userMicOff.Load() {
		return 0
	}
	return math.Float64frombits(a.inLevel.Load())
}
func (a *AudioIO) OutputLevel() float64  { return math.Float64frombits(a.outLevel.Load()) }
func (a *AudioIO) PlaybackIdle() bool    { return a.OutputLevel() < playbackIdleLevelEps }
func (a *AudioIO) markSendReady()        { a.sendReadyOnce.Do(func() { close(a.sendReady) }) }
func (a *AudioIO) SetSpeaking(s bool)    { a.speaking.Store(s) }
func (a *AudioIO) dropCapture() bool     { return a.speaking.Load() }
func (a *AudioIO) SetUserMicOff(off bool) { a.userMicOff.Store(off) }
func (a *AudioIO) UserMicOff() bool       { return a.userMicOff.Load() }
func (a *AudioIO) SetUserMicSticky(s bool) { a.userMicSticky.Store(s) }
func (a *AudioIO) UserMicSticky() bool     { return a.userMicSticky.Load() }
func (a *AudioIO) captureSuppressed() bool { return a.dropCapture() || a.userMicOff.Load() }
func (a *AudioIO) CaptureExpected() bool    { return !a.captureSuppressed() }
func (a *AudioIO) Frames() <-chan []int16   { return a.frames }

// SetPreferredDevices is a no-op on linux: CoreAudio device UIDs are darwin/VPIO
// only; the wireless mic/speaker reach Koe through the carrier UDS, not a device UID.
func (a *AudioIO) SetPreferredDevices(micUID, speakerUID string) {
	if micUID != "" || speakerUID != "" {
		log.Printf("koe[audio]: --mic-device/--speaker-device ignored on wireless (audio flows over the carrier UDS)")
	}
}

// VPIO knobs are inert on linux (VoiceProcessingIO is macOS-only). Kept so the
// cross-platform callers (realtime/cmd) compile and behave sensibly (never active).
func (a *AudioIO) VPIOActive() bool                       { return false }
func (a *AudioIO) SetVPIOVoiceProcessingBypassed(_ bool)  {}
func (a *AudioIO) VPIOVoiceProcessingBypassed() bool      { return false }

func (a *AudioIO) resolveCaptureFrame(frame []int16, forward bool) []int16 {
	if forward {
		return frame
	}
	if !koeEnvBool("KOE_CAPTURE_KEEPALIVE", true) {
		return nil
	}
	return captureSilenceFrameLinux
}

var captureSilenceFrameLinux = make([]int16, audioFrameSize)

// Play enqueues a decoded PCM frame for uplink playback (drop on overflow rather
// than block the decode path, same as darwin).
func (a *AudioIO) Play(pcm []int16) {
	if !a.playback.Load() {
		return
	}
	select {
	case a.playBuf <- pcm:
	default:
	}
}

// PrepareForCall clears stale capture/playback queued before a session starts.
func (a *AudioIO) PrepareForCall() {
	a.SetSpeaking(false)
	a.SetPlaybackEnabled(false)
	drain(a.frames)
	drain(a.playBuf)
}

func drain(ch chan []int16) {
	for {
		select {
		case <-ch:
		default:
			return
		}
	}
}

// SetPlaybackEnabled controls whether inbound Realtime audio is accepted.
func (a *AudioIO) SetPlaybackEnabled(s bool) {
	a.playback.Store(s)
	if !s {
		a.setOutputLevel(0)
	}
}

// EncodeFrame Opus-encodes one 960-sample (20 ms @ 48k) frame.
func (a *AudioIO) EncodeFrame(frame []int16) ([]byte, error) {
	a.encMu.Lock()
	defer a.encMu.Unlock()
	out := make([]byte, 4000)
	n, err := a.enc.Encode(frame, out)
	if err != nil {
		return nil, err
	}
	return out[:n], nil
}

// DecodeFrame Opus-decodes to a 960-sample frame.
func (a *AudioIO) DecodeFrame(payload []byte) ([]int16, error) {
	a.decMu.Lock()
	defer a.decMu.Unlock()
	pcm := make([]int16, audioFrameSize)
	n, err := a.dec.Decode(payload, pcm)
	if err != nil {
		return nil, err
	}
	return pcm[:n], nil
}

// ---- device half: the carrier UDS link (koe-audio-carrier-spec §2–§5) ----

// spkRingFrames bounds the speaker uplink jitter buffer (§3): ~300 ms of 20 ms
// frames. WORKLOAD: model reply audio arrives in network-paced bursts while the
// carrier drains at a steady clock. SYMPTOM if too small: audible drop on jitter;
// if too large: added latency. OVERRIDE: KOE_SPK_RING_FRAMES.
const spkRingFrames = 15

type helloMsg struct {
	Type  string `json:"type"`
	Proto string `json:"proto"`
	Role  string `json:"role"`
}

// audioProto is the carrier-link protocol version (koe-audio-carrier-spec §4.1).
const audioProto = "0.2"

// Start dials the carrier UDS, performs the hello handshake, and runs the mic
// (carrier→koe) and speaker (koe→carrier) pumps. Media stays daemon-resident under
// A; this is a WebRTC-consumer-style connect, no ownership handoff.
func (a *AudioIO) Start() error {
	if a.socketPath == "" {
		return fmt.Errorf("koe[audio]: no carrier socket (--audio-socket); wireless audio needs the carrier UDS")
	}
	conn, err := net.Dial("unixpacket", a.socketPath)
	if err != nil {
		// SEQPACKET may be unavailable; fall back to stream framing (audiobridge
		// WriteFrame/ReadFrame length-frame either way).
		conn, err = net.Dial("unix", a.socketPath)
		if err != nil {
			return fmt.Errorf("koe[audio]: dial carrier %q: %w", a.socketPath, err)
		}
	}
	a.conn = conn
	if err := a.handshake(); err != nil {
		_ = conn.Close()
		a.conn = nil
		return err
	}
	a.wg.Add(2)
	go a.micPump()
	go a.spkPump()
	return nil
}

// handshake sends our hello and validates the carrier's, per §4.1 (fail-loud on a
// proto mismatch — a future header v2 must not be silently misparsed).
func (a *AudioIO) handshake() error {
	mine := helloMsg{Type: "hello", Proto: audioProto, Role: "koe"}
	body, _ := json.Marshal(mine)
	h := audiobridge.Header{Magic: audiobridge.MagicControl, Format: audiobridge.FormatS16LE, Channels: 1, NSamples: uint32(len(body)) / 2}
	// control payload is opaque JSON bytes; encode its exact length via a raw write.
	if err := writeControl(a.conn, body); err != nil {
		return fmt.Errorf("koe[audio]: send hello: %w", err)
	}
	peer, err := readControl(a.conn)
	if err != nil {
		return fmt.Errorf("koe[audio]: read carrier hello: %w", err)
	}
	var got helloMsg
	if err := json.Unmarshal(peer, &got); err != nil || got.Type != "hello" {
		return fmt.Errorf("koe[audio]: first frame not hello: %q", string(peer))
	}
	if got.Proto != audioProto {
		return fmt.Errorf("koe[audio]: carrier proto %q != %q — closing", got.Proto, audioProto)
	}
	_ = h
	return nil
}

// micPump reads carrier mic frames, transcodes to the codec's 48k mono S16, applies
// the half-duplex gate, and publishes to a.frames.
func (a *AudioIO) micPump() {
	defer a.wg.Done()
	for {
		select {
		case <-a.done:
			return
		default:
		}
		hdr, payload, err := audiobridge.ReadFrame(a.conn)
		if err != nil {
			return // link closed / error → pump ends; supervisor restarts the carrier
		}
		if hdr.Magic != audiobridge.MagicMic {
			continue // control/spk echoes are ignored on the downlink
		}
		frame := toCodecPCM(hdr, payload) // → 48k mono S16
		forward := !a.captureSuppressed()
		if forward {
			a.setInputLevel(rmsLevel(frame))
		} else {
			frame = nil
		}
		if frame = a.resolveCaptureFrame(frame, forward); frame == nil {
			continue
		}
		select {
		case a.frames <- frame:
		default: // send path behind → drop (latest-wins, §3)
		}
	}
}

// spkPump drains playBuf through a bounded ring (§3: drop-oldest on overflow so
// scheduling jitter never produces an audible glitch) and writes each frame uplink.
func (a *AudioIO) spkPump() {
	defer a.wg.Done()
	ring := make(chan []int16, spkRingFrames)
	go func() {
		for {
			select {
			case <-a.done:
				return
			case pcm := <-a.playBuf:
				a.setOutputLevel(rmsLevel(pcm))
				for {
					select {
					case ring <- pcm:
					default:
						select {
						case <-ring: // drop oldest, then retry
						default:
						}
						continue
					}
					break
				}
			}
		}
	}()
	for {
		select {
		case <-a.done:
			return
		case pcm := <-ring:
			payload := s16PCMToBytes(pcm)
			hdr := audiobridge.Header{
				Magic:      audiobridge.MagicSpk,
				Format:     audiobridge.FormatS16LE,
				Channels:   audioChannels,
				SampleRate: audioSampleRate,
				NSamples:   uint32(len(pcm)),
				Seq:        a.upSeq.Add(1),
			}
			if err := audiobridge.WriteFrame(a.conn, hdr, payload); err != nil {
				return
			}
		}
	}
}

// Stop tears the carrier link down (idempotent — a caller's defer plus an explicit
// error-path Stop must not double-close).
func (a *AudioIO) Stop() {
	a.stopOnce.Do(func() {
		close(a.done)
		if a.conn != nil {
			_ = a.conn.Close()
		}
		a.wg.Wait()
	})
}

// ---- darwin-backend stubs (cmd/koe.go references these; inert on linux) ----

// StartVPIO is a macOS-only backend (Apple VoiceProcessingIO AEC, used by
// reachy_lite). Wireless routes audio through the carrier UDS, so it fails loud
// rather than silently doing nothing when a mis-wired argv selects it.
func (a *AudioIO) StartVPIO() error {
	return fmt.Errorf("koe[audio]: vpio backend is macOS-only; wireless audio uses the carrier UDS")
}

// StartFile is the macOS-only headless WAV debug backend (--say / --audio-in).
func (a *AudioIO) StartFile(inPCM []int16, outWAV string, pullSamples int) error {
	return fmt.Errorf("koe[audio]: --say/--audio-in file backend is macOS-only")
}

// CapturedMetrics returns the WAV debug backend's metrics — always zero on linux.
func (a *AudioIO) CapturedMetrics() WavMetrics { return WavMetrics{} }

// ---- helpers ----

// toCodecPCM converts a carrier mic frame (per its header format/rate/channels) to
// the codec's 48k mono S16. TODO(u2-fidelity): the carrier is thin and sends the
// SDK-native F32LE/16k/2ch (§9-b); this does the format+downmix and a linear
// resample. Replace with a windowed resampler if the naive one aliases audibly.
func toCodecPCM(h audiobridge.Header, payload []byte) []int16 {
	// Decode payload → mono float samples at the source rate.
	mono := decodeToMono(h, payload)
	if int(h.SampleRate) == audioSampleRate || h.SampleRate == 0 {
		return floatToS16(mono)
	}
	return floatToS16(resampleLinear(mono, int(h.SampleRate), audioSampleRate))
}

func decodeToMono(h audiobridge.Header, payload []byte) []float64 {
	ch := int(h.Channels)
	if ch < 1 {
		ch = 1
	}
	var samples []float64
	if h.Format == audiobridge.FormatS16LE {
		n := len(payload) / 2
		samples = make([]float64, 0, n)
		for i := 0; i+1 < len(payload); i += 2 {
			v := int16(uint16(payload[i]) | uint16(payload[i+1])<<8)
			samples = append(samples, float64(v)/32768.0)
		}
	} else { // F32LE
		n := len(payload) / 4
		samples = make([]float64, 0, n)
		for i := 0; i+3 < len(payload); i += 4 {
			bits := uint32(payload[i]) | uint32(payload[i+1])<<8 | uint32(payload[i+2])<<16 | uint32(payload[i+3])<<24
			samples = append(samples, float64(math.Float32frombits(bits)))
		}
	}
	if ch == 1 {
		return samples
	}
	mono := make([]float64, len(samples)/ch)
	for i := range mono {
		var sum float64
		for c := 0; c < ch; c++ {
			sum += samples[i*ch+c]
		}
		mono[i] = sum / float64(ch)
	}
	return mono
}

func resampleLinear(in []float64, srcRate, dstRate int) []float64 {
	if len(in) == 0 || srcRate == dstRate {
		return in
	}
	outLen := len(in) * dstRate / srcRate
	out := make([]float64, outLen)
	ratio := float64(srcRate) / float64(dstRate)
	for i := range out {
		pos := float64(i) * ratio
		j := int(pos)
		frac := pos - float64(j)
		if j+1 < len(in) {
			out[i] = in[j]*(1-frac) + in[j+1]*frac
		} else if j < len(in) {
			out[i] = in[j]
		}
	}
	return out
}

func floatToS16(in []float64) []int16 {
	out := make([]int16, len(in))
	for i, v := range in {
		if v > 1 {
			v = 1
		} else if v < -1 {
			v = -1
		}
		out[i] = int16(v * 32767)
	}
	return out
}

func s16PCMToBytes(pcm []int16) []byte {
	out := make([]byte, len(pcm)*2)
	for i, v := range pcm {
		out[i*2] = byte(uint16(v))
		out[i*2+1] = byte(uint16(v) >> 8)
	}
	return out
}

// writeControl / readControl carry an opaque control JSON payload as a 0xC0 frame.
// The header's NSamples encodes the JSON byte length (channels=1, S16 → *2 undone
// here by carrying raw bytes; we frame the length explicitly via a 4-byte prefix in
// the payload-len field of the header instead).
func writeControl(w net.Conn, body []byte) error {
	h := audiobridge.Header{Magic: audiobridge.MagicControl, Format: audiobridge.FormatS16LE, Channels: 1, NSamples: uint32(len(body)+1) / 2}
	// Pad to the header's declared PayloadLen so WriteFrame's length check passes.
	buf := make([]byte, h.PayloadLen())
	copy(buf, body)
	return audiobridge.WriteFrame(w, h, buf)
}

func readControl(r net.Conn) ([]byte, error) {
	h, payload, err := audiobridge.ReadFrame(r)
	if err != nil {
		return nil, err
	}
	if h.Magic != audiobridge.MagicControl {
		return nil, fmt.Errorf("koe[audio]: expected control frame, got magic %#x", h.Magic)
	}
	// Trim trailing NUL pad added by writeControl.
	end := len(payload)
	for end > 0 && payload[end-1] == 0 {
		end--
	}
	return payload[:end], nil
}

var _ = time.Second // reserved for future read/dial deadlines
