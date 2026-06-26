package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kocoro-lab/ShanClaw/internal/koe"
)

// koeConfig holds the resolved settings for one `shan koe` voice session.
type koeConfig struct {
	openAIKey   string // DEV-KEY: replaced by the deferred daemon mint relay (→ Plan D Cloud mint)
	daemonURL   string
	agent       string
	model       string
	language    string
	controlPort string // Desktop↔Koe control server port (Kocoro Desktop passes it); empty = no control channel
	aec         string // echo control: "" / "gate" = v1 half-duplex gate (default), "vpio" = Apple VoiceProcessingIO full-duplex AEC
	// Debug harness (workstream A): headless file-backed audio so a run needs no
	// mic/ears. All empty/zero = normal mic+speaker device.
	sayText     string // --say: synthesize this text (macOS say) as the mic input
	audioIn     string // --audio-in: WAV file to feed as the mic input
	audioOut    string // --audio-out: capture the reply audio to this WAV
	audioPeriod int    // --audio-period: renderInto pull size in samples (480 reproduces the framing bug; 0=960)
	once        bool   // --once: exit shortly after the reply finishes
	timeoutSec  int    // --timeout: hard exit after N seconds (0 = none)
}

func defaultKoeConfig() koeConfig {
	return koeConfig{
		daemonURL: "http://127.0.0.1:7533", // must match the daemon's listen addr; Desktop (Plan E) passes the real one
		model:     "gpt-realtime-mini-2025-12-15",
	}
}

var koeCmd = &cobra.Command{
	Use:   "koe",
	Short: "Voice front-brain: a realtime voice agent that delegates to the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := defaultKoeConfig()
		if v, _ := cmd.Flags().GetString("openai-key"); v != "" {
			cfg.openAIKey = v
		}
		if cfg.openAIKey == "" {
			cfg.openAIKey = os.Getenv("OPENAI_API_KEY") // DEV-KEY: replaced by the deferred daemon mint relay (→ Plan D Cloud mint)
		}
		if v, _ := cmd.Flags().GetString("daemon-url"); v != "" {
			cfg.daemonURL = v
		}
		cfg.agent, _ = cmd.Flags().GetString("agent")
		if v, _ := cmd.Flags().GetString("model"); v != "" {
			cfg.model = v
		}
		cfg.language, _ = cmd.Flags().GetString("language")
		cfg.controlPort, _ = cmd.Flags().GetString("control-port")
		cfg.aec, _ = cmd.Flags().GetString("aec")
		cfg.sayText, _ = cmd.Flags().GetString("say")
		cfg.audioIn, _ = cmd.Flags().GetString("audio-in")
		cfg.audioOut, _ = cmd.Flags().GetString("audio-out")
		cfg.audioPeriod, _ = cmd.Flags().GetInt("audio-period")
		cfg.once, _ = cmd.Flags().GetBool("once")
		cfg.timeoutSec, _ = cmd.Flags().GetInt("timeout")

		// No key check: with no --openai-key/OPENAI_API_KEY, runKoeCall mints via
		// the daemon (production path — Koe holds no credential). A dev key, if set,
		// takes the direct mint path instead.
		return runKoeCall(cmd.Context(), cfg)
	},
}

func init() {
	koeCmd.Flags().String("openai-key", "", "OpenAI API key (dev; C-minimal only)")
	koeCmd.Flags().String("daemon-url", "", "daemon base URL (default http://127.0.0.1:7533)")
	koeCmd.Flags().String("agent", "", "bound back-brain agent slug (empty = daemon default)")
	koeCmd.Flags().String("model", "", "realtime model (default gpt-realtime-mini-2025-12-15)")
	koeCmd.Flags().String("language", "", "conversation language hint")
	koeCmd.Flags().String("control-port", "", "Desktop↔Koe control server port (Kocoro Desktop passes it)")
	koeCmd.Flags().String("aec", "", "echo control: gate (default, half-duplex) | vpio (Apple VoiceProcessingIO full-duplex AEC)")
	koeCmd.Flags().String("say", "", "debug: synthesize this text as the mic input (macOS say) — headless file mode")
	koeCmd.Flags().String("audio-in", "", "debug: WAV file to feed as the mic input — headless file mode")
	koeCmd.Flags().String("audio-out", "", "debug: capture the reply audio to this WAV")
	koeCmd.Flags().Int("audio-period", 0, "debug: renderInto pull size in samples (480 reproduces the framing bug; 0=960)")
	koeCmd.Flags().Bool("once", false, "debug: exit shortly after the reply finishes")
	koeCmd.Flags().Int("timeout", 0, "debug: hard exit after N seconds (0=none)")
	rootCmd.AddCommand(koeCmd)
}

const koePersona = "You are Kocoro, a calm, professional voice assistant. Speak in the first person as Kocoro. " +
	"When the user asks for real work, call do_task and then say the result in one or two short spoken sentences. " +
	"Never read markdown, code, JSON, URLs, or file paths aloud. Confirm irreversible actions by restating them and waiting for a clear yes. " +
	"Never narrate that you are delegating — just do it and report back as yourself."

// onceGrace is how long after the reply finishes (→ "listening") --once waits
// before exiting, so a quick follow-up (e.g. an async do_task result) still lands.
const onceGrace = 3 * time.Second

func newBurstID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "burst-" + hex.EncodeToString(b[:])
}

func runKoeCall(ctx context.Context, cfg koeConfig) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Plan B wiring: link + resolver + dispatcher + per-call state.
	client := koe.NewDaemonClient(cfg.daemonURL)
	agents, err := client.ListAgents(ctx)
	if err != nil {
		log.Printf("koe: list agents failed (continuing with empty registry): %v", err)
	}
	resolver := koe.NewAgentResolver(agents, koe.NoopSemanticMatcher{})
	state := koe.NewCallState(newBurstID(), cfg.agent)

	// G2: Kocoro Desktop control channel. When Desktop spawns `shan koe
	// --control-port N`, stand up the control server so it receives voice_state /
	// control_app SSE and the control_app tool routes to the Desktop window. No
	// port = standalone CLI (no control channel).
	var onVoiceState func(string)
	var onCallState func(string)
	var controlApp koe.ControlAppFunc
	// callGate is the Desktop press-to-talk switch: mic stays muted until a call is
	// started (double-tap ⌥ / menu / the settings-configured trigger). callActive
	// stays nil in standalone/E2E mode → always-listen.
	var callGate atomic.Bool
	var callActive func() bool
	if cfg.controlPort != "" {
		callActive = callGate.Load
		var ctrl *koe.ControlServer
		ctrl = koe.NewControlServer(
			// onStart: a call began (POST /call/start from the double-tap, menu, or
			// configured trigger) — open the mic gate + show the listening sprite.
			func() { callGate.Store(true); ctrl.EmitVoiceState("listening") },
			// onEnd: the call ended — mute the mic + clear the sprite/popup + report ended.
			func() { callGate.Store(false); ctrl.EmitVoiceState("idle"); ctrl.EmitCallState("ended") },
		)
		onVoiceState = ctrl.EmitVoiceState
		onCallState = ctrl.EmitCallState
		controlApp = func(_ context.Context, action string) error {
			ctrl.EmitControlApp(action)
			return nil
		}
		go func() {
			addr := "127.0.0.1:" + cfg.controlPort
			if err := http.ListenAndServe(addr, ctrl.Handler()); err != nil {
				log.Printf("koe: control server on %s exited: %v", addr, err)
			}
		}()
	}
	disp := koe.NewDispatcher(client, resolver, state, controlApp)

	// Audio + WebRTC.
	audio, err := koe.NewAudioIO()
	if err != nil {
		return fmt.Errorf("audio init: %v", err)
	}
	startAudio := audio.Start
	if cfg.aec == "vpio" {
		startAudio = audio.StartVPIO // Apple VoiceProcessingIO full-duplex AEC (terminal); default is the v1 half-duplex gate
	}
	// Headless debug mode (workstream A): --say/--audio-in replace the mic+speaker
	// with a file backend (feed a WAV, capture the reply to --audio-out) so the
	// whole path runs without a mic or ears.
	fileMode := cfg.sayText != "" || cfg.audioIn != ""
	if fileMode {
		inPCM, lerr := koe.LoadInputPCM(cfg.sayText, cfg.audioIn)
		if lerr != nil {
			return fmt.Errorf("audio input: %v", lerr)
		}
		log.Printf("koe[debug]: file mode — %d input samples (%.2fs) out=%q period=%d",
			len(inPCM), float64(len(inPCM))/48000, cfg.audioOut, cfg.audioPeriod)
		startAudio = func() error { return audio.StartFile(inPCM, cfg.audioOut, cfg.audioPeriod) }
	}
	if err := startAudio(); err != nil {
		return fmt.Errorf("audio start: %v", err)
	}
	defer audio.Stop()

	// Mint the ephemeral secret. Production path is the via-daemon relay (Koe
	// never holds a long-lived credential). A dev key (--openai-key/OPENAI_API_KEY)
	// takes the direct mint instead — the C-minimal escape hatch for running
	// without a signed-in daemon.
	var ek string
	if cfg.openAIKey != "" {
		ek, err = koe.MintEphemeral(ctx, cfg.openAIKey, cfg.model) // DEV-KEY: direct dev mint
	} else {
		ek, err = client.MintViaDaemon(ctx, cfg.model) // via daemon → Cloud
	}
	if err != nil {
		return fmt.Errorf("mint: %v", err)
	}
	// G3: relay each turn's token usage via the daemon to Cloud for server-side
	// cost + quota (fire-and-forget; a usage failure never interrupts the call,
	// and Koe never sees pricing). Active whenever the daemon is reachable.
	onUsage := func(usage json.RawMessage) {
		go func() {
			if err := client.SendRealtimeUsage(context.Background(), usage); err != nil {
				log.Printf("koe: usage relay failed: %v", err)
			}
		}()
	}
	// persona-profile: append the daemon's small-tier-distilled user context (who
	// the user is, how to address them) to the base persona, so Kocoro greets the
	// user as themselves. Bounded to 3s + best-effort — a slow/failed distill must
	// not delay or block the call; Koe then speaks with its base persona only.
	persona := koePersona
	pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
	if extra, perr := client.FetchPersona(pctx); perr == nil && extra != "" {
		persona = koePersona + " " + extra
	}
	pcancel()

	// Debug harness: --once exits a short grace after the reply finishes (→
	// "listening"), pausing the timer while thinking/speaking so do_task latency
	// doesn't trip it; --timeout is a hard fallback. Wired even in standalone file
	// mode (no control channel) by composing onto any existing emitter.
	if cfg.once {
		prev := onVoiceState
		var graceMu sync.Mutex
		var graceTimer *time.Timer
		onVoiceState = func(s string) {
			if prev != nil {
				prev(s)
			}
			graceMu.Lock()
			defer graceMu.Unlock()
			if graceTimer != nil {
				graceTimer.Stop()
			}
			if s == "listening" {
				graceTimer = time.AfterFunc(onceGrace, cancel)
			}
		}
	}
	if cfg.timeoutSec > 0 {
		time.AfterFunc(time.Duration(cfg.timeoutSec)*time.Second, cancel)
	}

	conn, err := koe.Connect(ctx, audio, ek, persona, state, disp, koe.ConnectOptions{
		OnVoiceState: onVoiceState,
		OnCallState:  onCallState,
		Model:        cfg.model,
		OnUsage:      onUsage,
		CallActive:   callActive,
	})
	if err != nil {
		return fmt.Errorf("connect: %v", err)
	}
	defer conn.Close()

	if !fileMode {
		fmt.Println("Kocoro is listening. Speak; Ctrl-C to end.")
	}
	<-ctx.Done()
	if fileMode {
		m := audio.CapturedMetrics()
		log.Printf("koe[debug]: captured %d samples (%.2fs) rms=%.4f peak=%.4f disc=%.4f silence=%.2f clip=%.4f",
			m.Samples, float64(m.Samples)/48000, m.RMS, m.Peak, m.DiscontinuityRatio, m.SilenceRatio, m.ClippingRatio)
	}
	return nil
}
