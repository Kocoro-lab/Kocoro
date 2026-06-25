package koe

import (
	"sync"
	"sync/atomic"

	"github.com/gen2brain/malgo"
	opus "gopkg.in/hraban/opus.v2"
)

const (
	audioSampleRate = 48000                                  // WebRTC/Opus path (NOT the 24k WS path)
	audioChannels   = 1                                      // mono capture/playback
	audioFrameMs    = 20                                     // 20 ms frames
	audioFrameSize  = audioSampleRate / 1000 * audioFrameMs  // 960 samples
)

// AudioIO owns the malgo duplex device and the Opus codec. Capture frames are
// published on Frames(); playback PCM is enqueued via Play(). While SetSpeaking
// is true, capture is gated (half-duplex echo control).
type AudioIO struct {
	ctx      *malgo.AllocatedContext
	dev      *malgo.Device
	enc      *opus.Encoder
	dec      *opus.Decoder
	frames   chan []int16
	playBuf  chan []int16
	speaking atomic.Bool
	encMu    sync.Mutex
	decMu    sync.Mutex
}

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

// Play enqueues a decoded PCM frame for playback.
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

// Start opens the malgo duplex device and pumps capture→frames / playBuf→speaker.
func (a *AudioIO) Start() error {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, func(string) {})
	if err != nil {
		return err
	}
	a.ctx = ctx

	cfg := malgo.DefaultDeviceConfig(malgo.Duplex)
	cfg.SampleRate = audioSampleRate
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = audioChannels
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = audioChannels

	onData := func(out, in []byte, n uint32) {
		// Capture: bytes → []int16, publish unless gated.
		if !a.dropCapture() {
			frame := bytesToS16(in)
			select {
			case a.frames <- frame:
			default: // drop if the send path is behind
			}
		}
		// Playback: pull the next decoded frame (or silence).
		select {
		case pcm := <-a.playBuf:
			s16ToBytes(pcm, out)
		default:
			for i := range out {
				out[i] = 0
			}
		}
	}

	dev, err := malgo.InitDevice(ctx.Context, cfg, malgo.DeviceCallbacks{Data: onData})
	if err != nil {
		return err
	}
	a.dev = dev
	return dev.Start()
}

// Stop tears the device down.
func (a *AudioIO) Stop() {
	if a.dev != nil {
		_ = a.dev.Stop()
		a.dev.Uninit()
	}
	if a.ctx != nil {
		_ = a.ctx.Uninit()
		a.ctx.Free()
	}
}

func bytesToS16(b []byte) []int16 {
	out := make([]int16, len(b)/2)
	for i := range out {
		out[i] = int16(uint16(b[2*i]) | uint16(b[2*i+1])<<8)
	}
	return out
}

func s16ToBytes(pcm []int16, out []byte) {
	for i := 0; i < len(pcm) && 2*i+1 < len(out); i++ {
		out[2*i] = byte(pcm[i])
		out[2*i+1] = byte(pcm[i] >> 8)
	}
}
