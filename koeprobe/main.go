// Command koeprobe is a standalone un-darwin smoke test for the Koe audio stack
// on Reachy Mini Wireless (CM4 / Debian arm64). It exercises the production Koe
// audio cgo deps WITHOUT the full internal/koe package, so U1 can de-risk the
// arm64 cgo toolchain and — critically — the REAL ALSA device binding before the
// whole package is un-darwined:
//
//	capture  — malgo (miniaudio, ALSA) binds card 0 "Reachy Mini Audio", asserts rms>0
//	codec    — gopkg.in/hraban/opus.v2 (libopus) encodes one 20ms frame and decodes it back
//	playback — malgo binds card 0, plays a 440Hz tone; ear-judge for sound
//
// EXPLICIT device binding is load-bearing: the CM4's ALSA DEFAULT card is HDMI
// (card 1 vc4hdmi0), so binding "default" captures silence (no mic) and plays to
// a monitor, not the robot — the ALSA form of the §02 fail-loud lesson. This probe
// therefore enumerates devices, binds the one whose name matches -capture-device /
// -playback-device (default "reachy"), and FAILS LOUD if it is absent rather than
// falling back to the default. oto/v3 (the production PLAYBACK backend) cannot
// select a device — it only opens the ALSA default — so production playback needs
// an .asoundrc routing default→card 0 (§21); the probe uses malgo for playback so
// it can target card 0 directly and prove the hardware path.
//
// Same malgo/opus versions as internal/koe (go.mod is shared): a green run means
// the production audio deps link AND bind the real robot ALSA device.
package main

import (
	"bytes"
	"encoding/binary"
	"flag"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/gen2brain/malgo"
	opus "gopkg.in/hraban/opus.v2"
)

const (
	sampleRate = 48000
	channels   = 1
	frame20ms  = sampleRate / 50 // 960 samples, Opus's largest low-latency frame at 48k
)

func main() {
	captureMs := flag.Int("capture-ms", 200, "microphone capture window")
	toneMs := flag.Int("tone-ms", 300, "playback tone duration")
	captureMatch := flag.String("capture-device", "reachy", "substring (case-insensitive) of the capture device name to bind")
	playbackMatch := flag.String("playback-device", "reachy", "substring (case-insensitive) of the playback device name to bind")
	noPlayback := flag.Bool("no-playback", false, "skip the audible tone (quiet testing)")
	backend := flag.String("backend", "alsa", "malgo backend: alsa | pulse | auto")
	flag.Parse()

	fmt.Println("koeprobe: un-darwin audio smoke test (malgo capture+playback on card 0 + opus codec)")

	// Force the ALSA backend by default: on this robot PipeWire-pulse presents a
	// "Dummy Output" (the daemon owns card 0 at the ALSA-hw level), so miniaudio's
	// default pulse-first order never sees the real card. Direct ALSA is required —
	// after /api/media/release frees card 0, the ALSA backend binds it directly.
	var backends []malgo.Backend
	switch *backend {
	case "alsa":
		backends = []malgo.Backend{malgo.BackendAlsa}
	case "pulse":
		backends = []malgo.Backend{malgo.BackendPulseaudio}
	case "auto":
		backends = nil
	default:
		fatal("backend", fmt.Errorf("unknown -backend %q (want alsa, pulse, or auto)", *backend))
	}
	fmt.Printf("malgo backend: %s\n", *backend)

	ctx, err := malgo.InitContext(backends, malgo.ContextConfig{}, func(string) {})
	if err != nil {
		fatal("init context", err)
	}
	defer func() {
		_ = ctx.Uninit()
		ctx.Free()
	}()

	// Enumerate + print every device so the evidence shows card 0 vs the HDMI default.
	capID := pickDevice(ctx, malgo.Capture, *captureMatch)
	playID := pickDevice(ctx, malgo.Playback, *playbackMatch)

	pcm, err := capture(ctx, capID, *captureMs)
	if err != nil {
		fatal("capture", err)
	}
	rms := rmsOf(pcm)
	fmt.Printf("capture: %d samples @ %dHz, rms=%.2f\n", len(pcm), sampleRate, rms)
	if rms <= 0 {
		fatal("capture", fmt.Errorf("rms=%.2f, expected >0 — mic gain 0, wrong device, or silence", rms))
	}

	if err := codecRoundtrip(pcm); err != nil {
		fatal("codec", err)
	}
	fmt.Println("codec: opus encode+decode roundtrip OK (libopus links & runs)")

	if *noPlayback {
		fmt.Println("playback: skipped (--no-playback)")
	} else {
		if err := playTone(ctx, playID, *toneMs); err != nil {
			fatal("playback", err)
		}
		fmt.Println("playback: 440Hz tone played on card 0 (ear-judge: sound from the robot)")
	}
	fmt.Println("koeprobe: PASS")
}

// pickDevice enumerates devices of the given kind, prints them, and returns the
// ID of the first whose name contains match (case-insensitive). Fails loud if no
// device matches — never silently falls back to the (HDMI) default.
func pickDevice(ctx *malgo.AllocatedContext, kind malgo.DeviceType, match string) *malgo.DeviceID {
	label := map[malgo.DeviceType]string{malgo.Capture: "capture", malgo.Playback: "playback"}[kind]
	infos, err := ctx.Devices(kind)
	if err != nil {
		fatal("enumerate "+label, err)
	}
	fmt.Printf("%s devices (%d):\n", label, len(infos))
	var chosen *malgo.DeviceID
	for i := range infos {
		name := infos[i].Name()
		def := ""
		if infos[i].IsDefault != 0 {
			def = " [ALSA default]"
		}
		hit := ""
		if chosen == nil && strings.Contains(strings.ToLower(name), strings.ToLower(match)) {
			id := infos[i].ID
			chosen = &id
			hit = " <== bound"
		}
		fmt.Printf("  [%d] %q%s%s\n", i, name, def, hit)
	}
	if chosen == nil {
		fatal(label+" device", fmt.Errorf("no device name contains %q — refusing to fall back to the default (likely HDMI); re-run with -%s-device=<substr>", match, label))
	}
	return chosen
}

// capture records mono S16 from the bound ALSA device via malgo.
func capture(ctx *malgo.AllocatedContext, id *malgo.DeviceID, ms int) ([]int16, error) {
	cfg := malgo.DefaultDeviceConfig(malgo.Capture)
	cfg.Capture.Format = malgo.FormatS16
	cfg.Capture.Channels = channels
	cfg.Capture.DeviceID = id.Pointer()
	cfg.SampleRate = sampleRate
	cfg.Alsa.NoMMap = 1 // ALSA MMAP is flaky on some devices; match malgo's own example

	var buf bytes.Buffer
	cb := malgo.DeviceCallbacks{Data: func(_, in []byte, _ uint32) {
		buf.Write(in)
	}}
	dev, err := malgo.InitDevice(ctx.Context, cfg, cb)
	if err != nil {
		return nil, fmt.Errorf("init device: %w", err)
	}
	if err := dev.Start(); err != nil {
		return nil, fmt.Errorf("start: %w", err)
	}
	time.Sleep(time.Duration(ms) * time.Millisecond)
	dev.Uninit()

	raw := buf.Bytes()
	out := make([]int16, len(raw)/2)
	for i := range out {
		out[i] = int16(binary.LittleEndian.Uint16(raw[i*2:]))
	}
	return out, nil
}

func rmsOf(pcm []int16) float64 {
	if len(pcm) == 0 {
		return 0
	}
	var sum float64
	for _, s := range pcm {
		f := float64(s)
		sum += f * f
	}
	return math.Sqrt(sum / float64(len(pcm)))
}

// codecRoundtrip proves libopus links and runs: encode one 20ms frame of the
// captured audio, decode it back, and confirm the frame size survives.
func codecRoundtrip(pcm []int16) error {
	enc, err := opus.NewEncoder(sampleRate, channels, opus.AppVoIP)
	if err != nil {
		return fmt.Errorf("new encoder: %w", err)
	}
	dec, err := opus.NewDecoder(sampleRate, channels)
	if err != nil {
		return fmt.Errorf("new decoder: %w", err)
	}
	frame := make([]int16, frame20ms) // zero-padded if capture was shorter
	copy(frame, pcm)
	packet := make([]byte, 4000)
	n, err := enc.Encode(frame, packet)
	if err != nil {
		return fmt.Errorf("encode: %w", err)
	}
	decoded := make([]int16, frame20ms)
	got, err := dec.Decode(packet[:n], decoded)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if got != frame20ms {
		return fmt.Errorf("decoded %d samples, want %d", got, frame20ms)
	}
	return nil
}

// playTone plays a 440Hz sine to the bound ALSA device via malgo.
func playTone(ctx *malgo.AllocatedContext, id *malgo.DeviceID, ms int) error {
	total := sampleRate * ms / 1000
	tone := make([]byte, total*2)
	const freq = 440.0
	for i := range total {
		v := int16(0.3 * math.MaxInt16 * math.Sin(2*math.Pi*freq*float64(i)/sampleRate))
		binary.LittleEndian.PutUint16(tone[i*2:], uint16(v))
	}

	cfg := malgo.DefaultDeviceConfig(malgo.Playback)
	cfg.Playback.Format = malgo.FormatS16
	cfg.Playback.Channels = channels
	cfg.Playback.DeviceID = id.Pointer()
	cfg.SampleRate = sampleRate
	cfg.Alsa.NoMMap = 1

	pos := 0
	done := make(chan struct{})
	var closeOnce bool
	cb := malgo.DeviceCallbacks{Data: func(out, _ []byte, _ uint32) {
		n := copy(out, tone[pos:])
		pos += n
		for i := n; i < len(out); i++ {
			out[i] = 0 // silence-pad the tail frame
		}
		if pos >= len(tone) && !closeOnce {
			closeOnce = true
			close(done)
		}
	}}
	dev, err := malgo.InitDevice(ctx.Context, cfg, cb)
	if err != nil {
		return fmt.Errorf("init device: %w", err)
	}
	if err := dev.Start(); err != nil {
		return fmt.Errorf("start: %w", err)
	}
	select {
	case <-done:
	case <-time.After(time.Duration(ms+500) * time.Millisecond): // safety cap
	}
	time.Sleep(120 * time.Millisecond) // let the last buffer drain before teardown
	dev.Uninit()
	return nil
}

func fatal(stage string, err error) {
	fmt.Fprintf(os.Stderr, "koeprobe: %s FAILED: %v\n", stage, err)
	os.Exit(1)
}
