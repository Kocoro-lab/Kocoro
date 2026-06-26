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

	// Plan B wiring: link + resolver + per-call state.
	client := koe.NewDaemonClient(cfg.daemonURL)
	agents, err := client.ListAgents(ctx)
	if err != nil {
		log.Printf("koe: list agents failed (continuing with empty registry): %v", err)
	}
	resolver := koe.NewAgentResolver(agents, koe.NoopSemanticMatcher{})
	state := koe.NewCallState(newBurstID(), cfg.agent)

	// G3: relay each turn's token usage via the daemon to Cloud (fire-and-forget; a
	// usage failure never interrupts the call, and Koe never sees pricing).
	onUsage := func(usage json.RawMessage) {
		go func() {
			if uerr := client.SendRealtimeUsage(context.Background(), usage); uerr != nil {
				log.Printf("koe: usage relay failed: %v", uerr)
			}
		}()
	}
	// mintEK mints a fresh ephemeral secret (ephemeral keys are short-lived, so this
	// runs per call): a dev key (--openai-key/OPENAI_API_KEY) takes the direct mint,
	// else the via-daemon relay (Koe holds no long-lived credential).
	mintEK := func(mctx context.Context) (string, error) {
		if cfg.openAIKey != "" {
			return koe.MintEphemeral(mctx, cfg.openAIKey, cfg.model)
		}
		return client.MintViaDaemon(mctx, cfg.model)
	}
	// persona-profile: the daemon's small-tier-distilled user context appended to the
	// base persona (who the user is, how to address them), fetched once — bounded to
	// 3s, best-effort — and reused across calls.
	persona := koePersona
	pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
	if extra, perr := client.FetchPersona(pctx); perr == nil && extra != "" {
		persona = koePersona + " " + extra
	}
	pcancel()

	// ── Kocoro Desktop (control-port) mode: per-call device + session lifecycle ──
	// The mic device and the OpenAI session are opened on /call/start and torn down
	// on /call/end, so the macOS mic indicator is transient and the mic is never held
	// open while idle (B1 / F1). koe stays resident but idle between calls.
	if cfg.controlPort != "" {
		return runDesktopCall(ctx, cfg, client, resolver, state, persona, mintEK, onUsage)
	}

	// ── Standalone / headless mode: always-on (CLI + E2E + --say/--audio-in) ──
	disp := koe.NewDispatcher(client, resolver, state, nil)
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

	ek, err := mintEK(ctx)
	if err != nil {
		return fmt.Errorf("mint: %v", err)
	}

	// Debug harness: --once exits a short grace after the reply finishes (→
	// "listening"), pausing the timer while thinking/speaking so do_task latency
	// doesn't trip it; --timeout is a hard fallback.
	var onVoiceState func(string)
	if cfg.once {
		var graceMu sync.Mutex
		var graceTimer *time.Timer
		onVoiceState = func(s string) {
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
		Model:        cfg.model,
		OnUsage:      onUsage,
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

// runDesktopCall is the resident control-port loop. It stands up the Desktop
// control server and opens the mic device + OpenAI session ONLY for the duration of
// a call (/call/start → /call/end). Between calls koe holds no device and no
// network session, so the macOS mic indicator is transient (B1 / F1). A fresh
// AudioIO + RealtimeConn + ephemeral key is minted per call; the process-wide oto
// playback context is reused across calls.
func runDesktopCall(ctx context.Context, cfg koeConfig, client *koe.DaemonClient,
	resolver *koe.AgentResolver, state *koe.CallState, persona string,
	mintEK func(context.Context) (string, error), onUsage func(json.RawMessage)) error {

	var ctrl *koe.ControlServer
	disp := koe.NewDispatcher(client, resolver, state, func(_ context.Context, action string) error {
		ctrl.EmitControlApp(action)
		return nil
	})

	// The AudioIO + RealtimeConn live only between start and end; callCancel stops
	// the conn's send pump (which runs on callCtx, not the process ctx, so a hang-up
	// doesn't leak it across calls).
	var sessMu sync.Mutex
	var curAudio *koe.AudioIO
	var curConn *koe.RealtimeConn
	var callCancel context.CancelFunc

	startCall := func() {
		sessMu.Lock()
		defer sessMu.Unlock()
		if curConn != nil {
			return // already in a call
		}
		audio, aerr := koe.NewAudioIO()
		if aerr != nil {
			log.Printf("koe: audio init failed: %v", aerr)
			return
		}
		start := audio.Start
		if cfg.aec == "vpio" {
			start = audio.StartVPIO
		}
		if serr := start(); serr != nil {
			log.Printf("koe: audio start failed: %v", serr)
			audio.Stop()
			return
		}
		mctx, mcancel := context.WithTimeout(ctx, 15*time.Second)
		ek, merr := mintEK(mctx)
		mcancel()
		if merr != nil {
			log.Printf("koe: mint failed: %v", merr)
			audio.Stop()
			return
		}
		callCtx, cancel := context.WithCancel(ctx)
		conn, cerr := koe.Connect(callCtx, audio, ek, persona, state, disp, koe.ConnectOptions{
			OnVoiceState: ctrl.EmitVoiceState,
			OnCallState:  ctrl.EmitCallState,
			OnVoiceLevel: ctrl.EmitVoiceLevel,
			Model:        cfg.model,
			OnUsage:      onUsage,
		})
		if cerr != nil {
			log.Printf("koe: connect failed: %v", cerr)
			cancel()
			audio.Stop()
			return
		}
		curAudio, curConn, callCancel = audio, conn, cancel
		ctrl.EmitVoiceState("listening")
	}

	endCall := func() {
		sessMu.Lock()
		defer sessMu.Unlock()
		if curConn == nil {
			return
		}
		callCancel() // stop the send pump + any in-flight do_task
		curConn.Close()
		curAudio.Stop() // closes the mic device → the macOS indicator goes away
		curAudio, curConn, callCancel = nil, nil, nil
		ctrl.EmitVoiceState("idle")
		ctrl.EmitCallState("ended")
	}

	ctrl = koe.NewControlServer(startCall, endCall)
	go func() {
		addr := "127.0.0.1:" + cfg.controlPort
		if err := http.ListenAndServe(addr, ctrl.Handler()); err != nil {
			log.Printf("koe: control server on %s exited: %v", addr, err)
		}
	}()

	<-ctx.Done()
	endCall() // tear down any in-flight call on process shutdown
	return nil
}
