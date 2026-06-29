package koe

import (
	"fmt"
	"log"
	"math"
	"os"
	"sync"
	"sync/atomic"

	"github.com/ebitengine/oto/v3"
	"github.com/gen2brain/malgo"
	opus "gopkg.in/hraban/opus.v2"
)

const (
	audioSampleRate = 48000                                 // WebRTC/Opus path (NOT the 24k WS path)
	audioChannels   = 1                                     // mono capture/playback
	audioFrameMs    = 20                                    // 20 ms frames
	audioFrameSize  = audioSampleRate / 1000 * audioFrameMs // 960 samples
)

// AudioIO owns the malgo duplex device and the Opus codec. Capture frames are
// published on Frames(); playback PCM is enqueued via Play(). While SetSpeaking
// is true, capture is gated (half-duplex echo control).
type AudioIO struct {
	ctx     *malgo.AllocatedContext
	dev     *malgo.Device
	enc     *opus.Encoder
	dec     *opus.Decoder
	frames  chan []int16
	playBuf chan []int16
	// otoPlayer is the PRODUCTION playback path (audio_oto.go): oto drains playBuf
	// through macOS AudioToolbox. nil in the file/playback-only debug backends,
	// which keep the malgo renderInto path. Set by Start(), closed by Stop().
	otoPlayer *oto.Player
	speaking  atomic.Bool
	encMu     sync.Mutex
	decMu     sync.Mutex
	stopOnce  sync.Once
	// file is the headless debug backend (audio_file.go), non-nil only under
	// `shan koe --audio-in`/`--say`. When set, Stop() tears it down.
	file   *fileBackend
	primed atomic.Bool // file-backend renderInto pre-roll gate (audio_file.go)
	// inLevel / outLevel hold the most recent captured / played frame RMS (0..1, as
	// float64 bits) for the D3w reactive Island sprite. Updated on the audio threads,
	// read by the level pump (webrtc.go).
	inLevel  atomic.Uint64
	outLevel atomic.Uint64
}

// rmsLevel returns the RMS amplitude of a PCM frame normalized to 0..1.
func rmsLevel(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sumSq float64
	for _, s := range pcm {
		v := float64(s)
		sumSq += v * v
	}
	return math.Sqrt(sumSq/float64(len(pcm))) / 32768.0
}

func (a *AudioIO) setInputLevel(l float64)  { a.inLevel.Store(math.Float64bits(l)) }
func (a *AudioIO) setOutputLevel(l float64) { a.outLevel.Store(math.Float64bits(l)) }

// InputLevel / OutputLevel report the latest captured / played frame RMS (0..1).
func (a *AudioIO) InputLevel() float64  { return math.Float64frombits(a.inLevel.Load()) }
func (a *AudioIO) OutputLevel() float64 { return math.Float64frombits(a.outLevel.Load()) }

// NewAudioIO builds the codec (no device opened yet — Start() opens it, so unit
// tests can exercise Encode/Decode/gate without audio hardware).
func NewAudioIO() (*AudioIO, error) {
	enc, err := opus.NewEncoder(audioSampleRate, audioChannels, opus.AppVoIP)
	if err != nil {
		return nil, err
	}
	dec, err := opus.NewDecoder(audioSampleRate, audioChannels)
	if err != nil {
		return nil, err
	}
	return &AudioIO{
		enc:     enc,
		dec:     dec,
		frames:  make(chan []int16, 64),
		playBuf: make(chan []int16, 256),
	}, nil
}

// SetSpeaking gates the mic during playback (v1 half-duplex echo control).
func (a *AudioIO) SetSpeaking(s bool) { a.speaking.Store(s) }
func (a *AudioIO) dropCapture() bool  { return a.speaking.Load() }

// Frames yields captured 48 kHz mono 20 ms frames (gated while speaking).
func (a *AudioIO) Frames() <-chan []int16 { return a.frames }

// Play enqueues a decoded PCM frame for playback. It takes ownership of pcm
// without copying — the slice is read later on the audio callback thread, so the
// caller must NOT reuse or mutate it after this call. (Safe today: DecodeFrame
// returns a fresh slice per call.)
func (a *AudioIO) Play(pcm []int16) {
	select {
	case a.playBuf <- pcm:
	default: // drop on overflow rather than block the decode path
	}
}

// EncodeFrame Opus-encodes one 960-sample frame.
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

// prerollFrames is the playback jitter cushion. WORKLOAD: OpenAI streams reply
// audio over WebRTC in network-paced bursts, but the real CoreAudio device drains
// at a strict 48k hardware clock. SYMPTOM when it binds: with no cushion the first
// frames drain before the next burst lands → constant underrun → the "电流杂音"
// the user heard (clean in the lenient software-ticker file backend, garbled on
// the strict hardware clock). 8 frames = ~160 ms, the low end of typical voice
// jitter buffers. OVERRIDE: raise this const if a slow link still underruns.
const prerollFrames = 8

// renderInto fills out with the next playback bytes, behind a pre-roll jitter
// buffer: hold (silence) until prerollFrames have accumulated, then drain FIFO;
// re-arm on underrun so playback never resumes starved. Shared by the malgo
// callback AND the file debug backend so both exercise this exact path — the fix
// is unit-tested headlessly (render_test.go), not in a parallel copy.
func (a *AudioIO) renderInto(out []byte) {
	if !a.primed.Load() {
		if len(a.playBuf) < prerollFrames {
			zeroBytes(out)
			return
		}
		a.primed.Store(true)
	}
	select {
	case pcm := <-a.playBuf:
		n := s16ToBytes(pcm, out)
		for i := n; i < len(out); i++ { // zero any tail a >1-frame device buffer leaves
			out[i] = 0
		}
	default:
		a.primed.Store(false) // underran → re-prime before resuming
		zeroBytes(out)
	}
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

// Start opens oto for playback (the production path — malgo's low-level playback
// is static on this hardware, see audio_oto.go) and a malgo CAPTURE-ONLY device
// for the mic.
func (a *AudioIO) Start() error {
	// Playback: oto (macOS AudioToolbox, high-level). Reuse the process-wide context,
	// then a fresh player draining playBuf via otoSource.
	octx, err := ensureOtoContext()
	if err != nil {
		return fmt.Errorf("oto playback init: %w", err)
	}
	a.otoPlayer = octx.NewPlayer(&otoSource{a: a})
	a.otoPlayer.Play()

	// Capture: malgo CAPTURE-ONLY (not Duplex). A duplex device whose native rate
	// differs from ours forces two-way resampling and trips miniaudio's ring-buffer
	// bug #191 (the Bluetooth static); capturing alone sidesteps it. The half-duplex
	// gate (SetSpeaking → dropCapture) still mutes the mic while Kocoro speaks.
	probe := os.Getenv("KOE_AUDIO_PROBE") == "1"
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(msg string) {
		if probe {
			log.Printf("miniaudio: %s", msg)
		}
	})
	if err != nil {
		a.closeOtoPlayer()
		return err
	}
	a.ctx = ctx

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.SampleRate = audioSampleRate
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = audioChannels
	cfg.PeriodSizeInFrames = audioFrameSize // one 20 ms Opus frame per callback

	onData := func(out, in []byte, n uint32) {
		// Capture only: publish the mic frame unless the half-duplex gate is muting it
		// while Kocoro speaks. (out is the empty playback half of a capture device —
		// playback is oto's job now.)
		if !a.dropCapture() {
			frame := bytesToS16(in)
			a.setInputLevel(rmsLevel(frame)) // D3w: reactive listening amplitude
			select {
			case a.frames <- frame:
			default: // drop if the send path is behind
			}
		}
	}

	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: onData})
	if err != nil {
		// InitContext succeeded but the device did not — free the context (and the
		// oto player) here rather than leaking until a caller invokes Stop().
		_ = ctx.Uninit()
		ctx.Free()
		a.ctx = nil
		a.closeOtoPlayer()
		return err
	}
	a.dev = dev
	return dev.Start()
}

// closeOtoPlayer stops the production playback player (idempotent). oto v3.4
// deprecated Player.Close (the context manages the player), so Pause stops
// playback and dropping the reference lets it be reclaimed. The oto Context is
// process-wide and intentionally kept (oto allows one per process, reused across
// Start/Stop cycles).
func (a *AudioIO) closeOtoPlayer() {
	if a.otoPlayer != nil {
		a.otoPlayer.Pause()
		a.otoPlayer = nil
	}
}

// Stop tears the device down. Guarded by stopOnce so a second call (e.g. a
// caller's `defer Stop()` plus an explicit Stop on an error path) does not
// re-run Uninit/Free on already-freed C memory (use-after-free).
func (a *AudioIO) Stop() {
	a.stopOnce.Do(func() {
		if a.file != nil {
			a.stopFile() // audio_file.go: stop feed+capture goroutines, flush the WAV
			return
		}
		a.closeOtoPlayer() // production playback (nil-safe for the playback-only debug path)
		if a.dev != nil {
			_ = a.dev.Stop()
			a.dev.Uninit()
		}
		if a.ctx != nil {
			_ = a.ctx.Uninit()
			a.ctx.Free()
		}
	})
}

func bytesToS16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
	}
	return out
}

// s16ToBytes writes pcm into out as little-endian S16, returning the number of
// bytes written (≤ len(out)); the caller zeroes any remaining tail.
func s16ToBytes(pcm []int16, out []byte) int {
	n := 0
	for i := 0; i < len(pcm) && 2*i+1 < len(out); i++ {
		out[2*i] = byte(pcm[i])
		out[2*i+1] = byte(pcm[i] >> 8)
		n = 2*i + 2
	}
	return n
}
