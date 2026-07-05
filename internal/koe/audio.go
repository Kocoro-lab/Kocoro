//go:build darwin && cgo

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
	// inputBufferFrames covers the cold-start window before session.updated starts
	// the send pump. Desktop normally uses a warm session, but keeping ~5s here
	// prevents first words from being dropped during network stalls or startup.
	inputBufferFrames = 256
)

// AudioIO owns the selected realtime audio backend and the Opus codec. The
// default backend is oto playback + malgo capture with a half-duplex gate. The
// opt-in VPIO backend uses Apple's VoiceProcessingIO for full-duplex AEC. Local
// tests need the opus/opusfile/pkg-config cgo deps installed and discoverable.
type AudioIO struct {
	ctx     *malgo.AllocatedContext
	dev     *malgo.Device
	enc     *opus.Encoder
	dec     *opus.Decoder
	frames  chan []int16
	playBuf chan []int16
	// otoPlayer is the default playback path (audio_oto.go): oto drains playBuf
	// through macOS AudioToolbox. nil in the file/VPIO/playback-only debug
	// backends, which keep their own render paths. Set by Start(), closed by Stop().
	otoPlayer *oto.Player
	speaking  atomic.Bool
	// userMicOff is the user-initiated "mic off while a do_task runs" gate
	// (Desktop double-tap / mic button via POST /call/mic). Independent of the
	// speaking gate on purpose: the response lifecycle flips SetSpeaking
	// (response.done → false), which would silently undo a user's mute.
	// Capture keeps flowing as silent keepalive frames (resolveCaptureFrame),
	// so the send-track RTP timeline stays continuous during long mutes.
	userMicOff atomic.Bool
	// preferredMicUID / preferredSpeakerUID are CoreAudio device UIDs bound in
	// Desktop settings (koe.mic_device / koe.speaker_device → --mic-device /
	// --speaker-device). Empty = system default. Only the VPIO backend honors
	// them (voice-settings wave §W4); the gate backend logs and ignores —
	// Desktop, the only device-binding UI, always runs vpio.
	preferredMicUID     string
	preferredSpeakerUID string
	playback            atomic.Bool
	encMu               sync.Mutex
	decMu               sync.Mutex
	stopOnce            sync.Once
	// vpioActive / vpioDone track the opt-in VoiceProcessingIO backend
	// (audio_vpio.go). VPIO supplies native echo cancellation, but the product
	// keeps Desktop audio call-scoped so macOS does not hold the mic while idle.
	// Explicit barge-in experiments can opt into a stricter local energy gate.
	vpioActive      atomic.Bool
	vpioDone        chan struct{}
	vpioWG          sync.WaitGroup
	vpioBargeFrames int
	vpioNoiseFloor  float64
	vpioForwarded   atomic.Uint64
	vpioGateDropped atomic.Uint64
	vpioBargePassed atomic.Uint64
	vpioMaxInput    atomic.Uint64
	vpioMaxOutput   atomic.Uint64
	vpioStatsMu     sync.Mutex
	vpioStatsBase   vpioDebugStats
	// sendReady is closed (once) when pumpSendTrack starts draining — i.e. the OpenAI
	// session is configured. The file backend's feedFrames waits on it so a one-shot
	// --say/--audio-in utterance is streamed in sync with the send pump, never fed
	// into the 64-frame buffer before the pump is ready to drain it (that overflow
	// dropped/bursted the synthesized speech → the silent/truncated --say runs). The
	// real-mic path streams continuously and never waits; closing it is harmless.
	sendReady     chan struct{}
	sendReadyOnce sync.Once
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

func (a *AudioIO) setInputLevel(l float64) { a.inLevel.Store(math.Float64bits(l)) }
func (a *AudioIO) setOutputLevel(l float64) {
	a.outLevel.Store(math.Float64bits(l))
	a.trackVPIOMaxOutput(l)
}

// InputLevel reports the latest captured frame RMS (0..1). While the user
// mic-off gate is active it reports 0 — the wire hears zeros and the Desktop
// sprite must visibly agree (trust: "it can't hear us").
func (a *AudioIO) InputLevel() float64 {
	if a.userMicOff.Load() {
		return 0
	}
	return math.Float64frombits(a.inLevel.Load())
}

// OutputLevel reports the latest played frame RMS (0..1).
func (a *AudioIO) OutputLevel() float64 { return math.Float64frombits(a.outLevel.Load()) }

// playbackIdleLevelEps separates "reply audio audibly playing" from silence /
// warm-session comfort noise for PlaybackIdle. WORKLOAD: TTS speech RMS runs
// well above 0.01; decoded silent RTP and drained pipelines sit near 0. SYMPTOM
// if too high: the speaking watchdog releases (and cuts) mid-speech; if too low:
// residual noise keeps the watchdog waiting until its hard cap. OVERRIDE: none —
// revisit alongside the playback paths' level reporting.
const playbackIdleLevelEps = 0.005

// PlaybackIdle reports whether reply audio is audibly playing. All playback
// paths (oto, VPIO, renderInto file/debug) zero the output level when their
// pipeline drains, so a sustained-idle poll over this is the speaking watchdog's
// drain signal.
func (a *AudioIO) PlaybackIdle() bool { return a.OutputLevel() < playbackIdleLevelEps }

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
	a := &AudioIO{
		enc:       enc,
		dec:       dec,
		frames:    make(chan []int16, inputBufferFrames),
		playBuf:   make(chan []int16, 256),
		sendReady: make(chan struct{}),
	}
	a.playback.Store(true)
	return a, nil
}

// markSendReady signals (once) that the send pump has started draining captured
// frames — the OpenAI session is configured. The file backend's feedFrames waits on
// this before streaming a one-shot utterance so it lands in sync with the pump.
func (a *AudioIO) markSendReady() { a.sendReadyOnce.Do(func() { close(a.sendReady) }) }

// SetSpeaking marks playback as active. Production treats it as a hard mute while
// Kocoro speaks unless experimental VPIO barge-in is explicitly enabled.
func (a *AudioIO) SetSpeaking(s bool) { a.speaking.Store(s) }
func (a *AudioIO) dropCapture() bool  { return a.speaking.Load() }

// SetUserMicOff toggles the user mic-off gate (koe-mic-off design). Task-window
// enforcement lives in the /call/mic handler; auto-restore in maybeRestoreUserMic.
func (a *AudioIO) SetUserMicOff(off bool) { a.userMicOff.Store(off) }
func (a *AudioIO) UserMicOff() bool       { return a.userMicOff.Load() }

// SetPreferredDevices records the CoreAudio device UIDs StartVPIO binds
// (empty = system default). Call after NewAudioIO, before Start/StartVPIO.
func (a *AudioIO) SetPreferredDevices(micUID, speakerUID string) {
	a.preferredMicUID = micUID
	a.preferredSpeakerUID = speakerUID
}

// captureSuppressed is the capture-path gate: speaking gate OR user mic off.
// Both resolve to silent keepalive frames downstream.
func (a *AudioIO) captureSuppressed() bool { return a.dropCapture() || a.userMicOff.Load() }

// CaptureExpected reports whether the mic is currently expected to be capturing
// REAL input — i.e. not user-muted and not gated by Kocoro speaking. The
// silent-input watchdog only advances its silence timer while this is true, so a
// legitimately-suppressed mic never trips the "can't hear you" warning.
func (a *AudioIO) CaptureExpected() bool { return !a.captureSuppressed() }

// VPIOActive reports whether the VoiceProcessingIO capture backend is live.
func (a *AudioIO) VPIOActive() bool { return a.vpioActive.Load() }

// captureSilenceFrame is the shared 20 ms zero frame forwarded while the
// speak-gate suppresses capture. Read-only downstream (the gate copies frames it
// buffers; the encoder only reads), so sharing one slice is safe.
var captureSilenceFrame = make([]int16, audioFrameSize)

// resolveCaptureFrame decides what a capture callback forwards into a.frames.
// forward=true passes the real frame through. When the speak-gate suppresses the
// frame, a SILENT frame is forwarded instead of nothing (KOE_CAPTURE_KEEPALIVE=0
// restores the legacy drop): halting the send track glues the pre/post-speech RTP
// timelines together, and the drift accumulated over a few assistant turns is the
// prime suspect for the 2026-07-02 mid-call server-VAD deafness. The mute itself
// is unchanged — the server hears zeros, never residual speaker echo.
func (a *AudioIO) resolveCaptureFrame(frame []int16, forward bool) []int16 {
	if forward {
		return frame
	}
	if !koeEnvBool("KOE_CAPTURE_KEEPALIVE", true) {
		return nil
	}
	return captureSilenceFrame
}

// Frames yields captured 48 kHz mono 20 ms frames.
func (a *AudioIO) Frames() <-chan []int16 { return a.frames }

const (
	// VPIO already performs native AEC. This extra local guard is deliberately
	// conservative: while Koe is speaking, drop mic frames by default. Experimental
	// barge-in can be enabled with KOE_VPIO_BARGE_IN=1; then frames pass only after
	// sustained post-AEC energy that is much louder than residual speaker bleed.
	// Tune with KOE_VPIO_BARGE_THRESHOLD, KOE_VPIO_BARGE_MS, and
	// KOE_VPIO_BARGE_NOISE_MULTIPLIER.
	defaultVPIOBargeInThreshold       = 0.045
	defaultVPIOBargeInMS              = 500
	defaultVPIOBargeInNoiseMultiplier = 6.0
	vpioNoiseFloorAlpha               = 0.02
)

func (a *AudioIO) shouldForwardVPIOCapture(level float64) bool {
	if a.userMicOff.Load() {
		// User mic-off outranks everything, including KOE_VPIO_BARGE_IN:
		// barge-in may forward frames while Koe speaks, but never while the
		// user asked for silence.
		a.vpioBargeFrames = 0
		a.vpioGateDropped.Add(1)
		return false
	}
	if !a.dropCapture() {
		a.vpioBargeFrames = 0
		a.vpioNoiseFloor = 0
		a.vpioForwarded.Add(1)
		return true
	}
	if !koeEnvBool("KOE_VPIO_BARGE_IN", false) {
		a.vpioBargeFrames = 0
		a.updateVPIONoiseFloor(level)
		a.vpioGateDropped.Add(1)
		return false
	}
	threshold := a.vpioBargeInThreshold()
	if level < threshold {
		a.vpioBargeFrames = 0
		a.updateVPIONoiseFloor(level)
		a.vpioGateDropped.Add(1)
		return false
	}
	a.vpioBargeFrames++
	if a.vpioBargeFrames >= vpioBargeInFrames() {
		a.vpioBargePassed.Add(1)
		a.vpioForwarded.Add(1)
		return true
	}
	a.vpioGateDropped.Add(1)
	return false
}

func (a *AudioIO) vpioBargeInThreshold() float64 {
	base := koeEnvFloat("KOE_VPIO_BARGE_THRESHOLD", defaultVPIOBargeInThreshold)
	adaptive := a.vpioNoiseFloor * koeEnvFloat("KOE_VPIO_BARGE_NOISE_MULTIPLIER", defaultVPIOBargeInNoiseMultiplier)
	return math.Max(base, adaptive)
}

func vpioBargeInFrames() int {
	ms := koeEnvInt("KOE_VPIO_BARGE_MS", defaultVPIOBargeInMS)
	frames := (ms + audioFrameMs - 1) / audioFrameMs
	if frames < 1 {
		return 1
	}
	return frames
}

func (a *AudioIO) updateVPIONoiseFloor(level float64) {
	if level <= 0 {
		return
	}
	if a.vpioNoiseFloor == 0 {
		a.vpioNoiseFloor = level
		return
	}
	a.vpioNoiseFloor = (1-vpioNoiseFloorAlpha)*a.vpioNoiseFloor + vpioNoiseFloorAlpha*level
}

func (a *AudioIO) trackVPIOMaxInput(level float64) {
	for {
		oldBits := a.vpioMaxInput.Load()
		old := math.Float64frombits(oldBits)
		if level <= old {
			return
		}
		if a.vpioMaxInput.CompareAndSwap(oldBits, math.Float64bits(level)) {
			return
		}
	}
}

// SetPlaybackEnabled controls whether inbound Realtime audio is accepted. Desktop
// keeps a warm WebRTC session while idle, and OpenAI may still send silent RTP;
// dropping it until a response starts keeps the hardware jitter buffer clean.
func (a *AudioIO) SetPlaybackEnabled(s bool) {
	a.playback.Store(s)
	if !s {
		a.setOutputLevel(0)
		a.clearVPIOBuffers()
		for {
			select {
			case <-a.playBuf:
			default:
				return
			}
		}
	}
}

// Play enqueues a decoded PCM frame for playback. It takes ownership of pcm
// without copying — the slice is read later on the audio callback thread, so the
// caller must NOT reuse or mutate it after this call. (Safe today: DecodeFrame
// returns a fresh slice per call.)
func (a *AudioIO) Play(pcm []int16) {
	if !a.playback.Load() {
		return
	}
	select {
	case a.playBuf <- pcm:
	default: // drop on overflow rather than block the decode path
	}
}

// PrepareForCall clears stale capture/playback queued before a session starts
// sending user audio.
func (a *AudioIO) PrepareForCall() {
	a.SetSpeaking(false)
	a.SetPlaybackEnabled(false)
	a.primed.Store(false)
	for {
		select {
		case <-a.frames:
		default:
			goto drainPlay
		}
	}
drainPlay:
	for {
		select {
		case <-a.playBuf:
		default:
			a.resetVPIOCallStats()
			return
		}
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
			a.setOutputLevel(0) // pre-rolling → nothing audible yet
			zeroBytes(out)
			return
		}
		a.primed.Store(true)
	}
	select {
	case pcm := <-a.playBuf:
		a.setOutputLevel(rmsLevel(pcm)) // keep the drain signal honest on this path too
		n := s16ToBytes(pcm, out)
		for i := n; i < len(out); i++ { // zero any tail a >1-frame device buffer leaves
			out[i] = 0
		}
	default:
		a.primed.Store(false) // underran → re-prime before resuming
		a.setOutputLevel(0)
		zeroBytes(out)
	}
}

func zeroBytes(b []byte) {
	for i := range b {
		b[i] = 0
	}
}

func audioProbe(step string) {
	if os.Getenv("KOE_AUDIO_PROBE") == "1" {
		log.Printf("koe[audio-probe]: %s", step)
	}
}

// Start opens oto for playback (the production path — malgo's low-level playback
// is static on this hardware, see audio_oto.go) and a malgo CAPTURE-ONLY device
// for the mic.
func (a *AudioIO) Start() error {
	if a.preferredMicUID != "" || a.preferredSpeakerUID != "" {
		log.Printf("koe[audio]: device binding is vpio-only; gate backend uses system defaults")
	}
	// Playback: oto (macOS AudioToolbox, high-level). Reuse the process-wide context,
	// then a fresh player draining playBuf via otoSource.
	audioProbe("gate start enter")
	audioProbe("oto context begin")
	octx, err := ensureOtoContext()
	if err != nil {
		return fmt.Errorf("oto playback init: %w", err)
	}
	audioProbe("oto context done")
	audioProbe("oto player begin")
	a.otoPlayer = octx.NewPlayer(&otoSource{a: a})
	audioProbe("oto player done")
	audioProbe("oto play begin")
	a.otoPlayer.Play()
	audioProbe("oto play done")

	// Capture: malgo CAPTURE-ONLY (not Duplex). A duplex device whose native rate
	// differs from ours forces two-way resampling and trips miniaudio's ring-buffer
	// bug #191 (the Bluetooth static); capturing alone sidesteps it. The half-duplex
	// gate (SetSpeaking → dropCapture) still mutes the mic while Kocoro speaks.
	probe := os.Getenv("KOE_AUDIO_PROBE") == "1"
	audioProbe("malgo context begin")
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(msg string) {
		if probe {
			log.Printf("miniaudio: %s", msg)
		}
	})
	if err != nil {
		a.closeOtoPlayer()
		return err
	}
	audioProbe("malgo context done")
	a.ctx = ctx

	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.SampleRate = audioSampleRate
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = audioChannels
	cfg.PeriodSizeInFrames = audioFrameSize // one 20 ms Opus frame per callback

	onData := func(out, in []byte, n uint32) {
		// Capture only: publish the mic frame unless the half-duplex gate is muting it
		// while Kocoro speaks — then a silent keepalive frame keeps the send track's
		// RTP timeline continuous (see resolveCaptureFrame). (out is the empty
		// playback half of a capture device — playback is oto's job now.)
		// User mic off (koe-mic-off) suppresses the same way.
		forward := !a.captureSuppressed()
		var frame []int16
		if forward {
			frame = bytesToS16(in)
			a.setInputLevel(rmsLevel(frame)) // D3w: reactive listening amplitude
		}
		if frame = a.resolveCaptureFrame(frame, forward); frame == nil {
			return
		}
		select {
		case a.frames <- frame:
		default: // drop if the send path is behind
		}
	}

	audioProbe("malgo device init begin")
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
	audioProbe("malgo device init done")
	a.dev = dev
	audioProbe("malgo device start begin")
	if err := dev.Start(); err != nil {
		// The device inited but did not start — uninit it and free the context
		// (and the oto player) here rather than leaking until a caller invokes Stop().
		dev.Uninit()
		a.dev = nil
		_ = ctx.Uninit()
		ctx.Free()
		a.ctx = nil
		a.closeOtoPlayer()
		return err
	}
	audioProbe("malgo device start done")
	return nil
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
		if a.vpioActive.Load() {
			a.LogDebugStats()
			a.stopVPIO()
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

func (a *AudioIO) LogDebugStats() {
	if os.Getenv("KOE_AUDIO_LOG") != "1" && os.Getenv("KOE_EVENT_LOG") != "1" {
		return
	}
	if a.vpioActive.Load() {
		log.Printf("koe[audio]: vpio stats: %+v", a.vpioDebugStatsSinceBase())
	}
}

func (a *AudioIO) resetVPIOCallStats() {
	if !a.vpioActive.Load() {
		return
	}
	a.vpioStatsMu.Lock()
	defer a.vpioStatsMu.Unlock()
	a.vpioStatsBase = a.vpioDebugStats()
	a.vpioMaxInput.Store(0)
	a.vpioMaxOutput.Store(0)
	a.vpioStatsBase.MaxInputLevel = 0
	a.vpioStatsBase.MaxOutputLevel = 0
}

func (a *AudioIO) vpioDebugStatsSinceBase() vpioDebugStats {
	a.vpioStatsMu.Lock()
	base := a.vpioStatsBase
	a.vpioStatsMu.Unlock()
	cur := a.vpioDebugStats()
	return vpioDebugStats{
		InputCallbacks:  subUint64(cur.InputCallbacks, base.InputCallbacks),
		OutputCallbacks: subUint64(cur.OutputCallbacks, base.OutputCallbacks),
		InputFrames:     subUint64(cur.InputFrames, base.InputFrames),
		OutputFrames:    subUint64(cur.OutputFrames, base.OutputFrames),
		PlayUnderruns:   subUint64(cur.PlayUnderruns, base.PlayUnderruns),
		PlayOverwrites:  subUint64(cur.PlayOverwrites, base.PlayOverwrites),
		PlayBuffered:    cur.PlayBuffered,
		PlayCapacity:    cur.PlayCapacity,
		ForwardedFrames: subUint64(cur.ForwardedFrames, base.ForwardedFrames),
		GateDropped:     subUint64(cur.GateDropped, base.GateDropped),
		BargePassed:     subUint64(cur.BargePassed, base.BargePassed),
		MaxInputLevel:   cur.MaxInputLevel,
		MaxOutputLevel:  cur.MaxOutputLevel,
	}
}

func (a *AudioIO) trackVPIOMaxOutput(level float64) {
	if !a.vpioActive.Load() {
		return
	}
	for {
		curBits := a.vpioMaxOutput.Load()
		if level <= math.Float64frombits(curBits) {
			return
		}
		if a.vpioMaxOutput.CompareAndSwap(curBits, math.Float64bits(level)) {
			return
		}
	}
}

func subUint64(cur, base uint64) uint64 {
	if cur < base {
		return 0
	}
	return cur - base
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
