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
	"strconv"
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
	voice       string // realtime output voice (marin/cedar/shimmer/…); empty → "marin" fallback in sessionConfig
	language    string
	controlPort string // Desktop↔Koe control server port (Kocoro Desktop passes it); empty = no control channel
	aec         string // echo control: "" / "gate" = oto half-duplex fallback, "vpio" = Apple VoiceProcessingIO full-duplex AEC
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
		cfg.voice, _ = cmd.Flags().GetString("voice")
		cfg.language, _ = cmd.Flags().GetString("language")
		cfg.controlPort, _ = cmd.Flags().GetString("control-port")
		if v, _ := cmd.Flags().GetString("aec"); v != "" {
			cfg.aec = v
		} else {
			cfg.aec = os.Getenv("KOE_AEC")
		}
		cfg.sayText, _ = cmd.Flags().GetString("say")
		cfg.audioIn, _ = cmd.Flags().GetString("audio-in")
		cfg.audioOut, _ = cmd.Flags().GetString("audio-out")
		cfg.audioPeriod, _ = cmd.Flags().GetInt("audio-period")
		cfg.once, _ = cmd.Flags().GetBool("once")
		cfg.timeoutSec, _ = cmd.Flags().GetInt("timeout")

		// No key check: with no --openai-key/OPENAI_API_KEY, runKoeCall mints via
		// the daemon (production path — Koe holds no credential). A dev key, if set,
		// takes the direct mint path instead.
		if cfg.aec != "" && cfg.aec != "gate" && cfg.aec != "vpio" {
			return fmt.Errorf("invalid --aec %q (want gate or vpio)", cfg.aec)
		}
		return runKoeCall(cmd.Context(), cfg)
	},
}

func init() {
	koeCmd.Flags().String("openai-key", "", "OpenAI API key (dev; C-minimal only)")
	koeCmd.Flags().String("daemon-url", "", "daemon base URL (default http://127.0.0.1:7533)")
	koeCmd.Flags().String("agent", "", "bound back-brain agent slug (empty = daemon default)")
	koeCmd.Flags().String("model", "", "realtime model (default gpt-realtime-mini-2025-12-15)")
	koeCmd.Flags().String("voice", "", "realtime output voice (marin/cedar/shimmer/…; empty = marin)")
	koeCmd.Flags().String("language", "", "conversation language hint")
	koeCmd.Flags().String("control-port", "", "Desktop↔Koe control server port (Kocoro Desktop passes it)")
	koeCmd.Flags().String("aec", "", "echo control: gate (default, oto half-duplex) | vpio (Apple VoiceProcessingIO full-duplex AEC)")
	koeCmd.Flags().String("say", "", "debug: synthesize this text as the mic input (macOS say) — headless file mode")
	koeCmd.Flags().String("audio-in", "", "debug: WAV file to feed as the mic input — headless file mode")
	koeCmd.Flags().String("audio-out", "", "debug: capture the reply audio to this WAV")
	koeCmd.Flags().Int("audio-period", 0, "debug: renderInto pull size in samples (480 reproduces the framing bug; 0=960)")
	koeCmd.Flags().Bool("once", false, "debug: exit shortly after the reply finishes")
	koeCmd.Flags().Int("timeout", 0, "debug: hard exit after N seconds (0=none)")
	rootCmd.AddCommand(koeCmd)
}

const koePersona = `You are Kocoro, an AI coworker speaking by voice through Kocoro Desktop.

You are one self. Chatting and doing real work are both just you — never speak of a
backend, daemon, system, agent runner, or another Kocoro as someone else, and never
narrate where your work happens. If something takes time, you are the one doing it. You
may point at the screen only to reference something already shown there.

Reply in the language of the user's current utterance, not the user's usual language,
memory, or earlier turns. Keep it plain spoken prose, usually a sentence or two. Never
read markdown, JSON, code, URLs, file paths, or tool logs aloud. Don't start topics or
fill silence — speak only when the user addressed you or a real result is ready, and if
they tell you to stop, stop. If you did not clearly hear a request, don't guess; stay
quiet or ask briefly for a repeat.

Do the work rather than ask around it. Call do_task for anything past small talk — files,
research, current facts, any number or calculation, edits, messages, or any real action.
Your recall and mental arithmetic are unreliable, so route them through do_task; calling
the tool IS the answer. Never say a number, fact, date, or name that did not come back
from a do_task result.

As you call do_task, say one short line naming what you're doing, with no answer or number
in it. Then let it work — say nothing more until the result lands, then speak it briefly in
your own voice. If the result carries a spoken_summary, say exactly that. Before anything
irreversible or outbound, restate it and wait for a clear yes.`

// onceGrace is how long after the reply finishes (→ "listening") --once waits
// before exiting, so a quick follow-up (e.g. an async do_task result) still lands.
const onceGrace = 3 * time.Second

const (
	audioStartTimeout         = 20 * time.Second
	audioStartTimeoutExitCode = 124
	warmSessionTTL            = 45 * time.Minute
)

// OpenAI/Gateway realtime client secrets carry an expires_at; Koe currently only
// consumes the value, so keep the local warm cache below the typical 10-minute
// server lifetime and retry with a fresh mint if a cached secret is rejected.
const warmMintTTL = 8 * time.Minute

type warmMint struct {
	mint func(context.Context) (string, error)
	ttl  time.Duration

	mu       sync.Mutex
	value    string
	mintedAt time.Time
	inFlight bool
}

func newWarmMint(ctx context.Context, mint func(context.Context) (string, error), ttl time.Duration) *warmMint {
	w := &warmMint{mint: mint, ttl: ttl}
	w.prefetch(ctx)
	return w
}

func (w *warmMint) take(ctx context.Context) (string, bool, error) {
	now := time.Now()
	w.mu.Lock()
	if w.value != "" && now.Sub(w.mintedAt) < w.ttl {
		v := w.value
		w.value = ""
		w.mintedAt = time.Time{}
		w.mu.Unlock()
		w.prefetch(context.Background())
		return v, true, nil
	}
	w.mu.Unlock()
	v, err := w.mint(ctx)
	return v, false, err
}

func (w *warmMint) prefetch(ctx context.Context) {
	w.mu.Lock()
	if w.inFlight || (w.value != "" && time.Since(w.mintedAt) < w.ttl) {
		w.mu.Unlock()
		return
	}
	w.inFlight = true
	w.mu.Unlock()

	go func() {
		pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		defer cancel()
		v, err := w.mint(pctx)

		w.mu.Lock()
		defer w.mu.Unlock()
		w.inFlight = false
		if err != nil || v == "" {
			if err != nil {
				log.Printf("koe[timing]: warm mint failed: %v", err)
			}
			return
		}
		w.value = v
		w.mintedAt = time.Now()
		log.Printf("koe[timing]: warm mint ready")
	}()
}

func newBurstID() string {
	var b [8]byte
	_, _ = rand.Read(b[:])
	return "burst-" + hex.EncodeToString(b[:])
}

func koeAudioStartTimeout() time.Duration {
	raw := os.Getenv("KOE_AUDIO_START_TIMEOUT_MS")
	if raw == "" {
		return audioStartTimeout
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return audioStartTimeout
	}
	return time.Duration(ms) * time.Millisecond
}

func koeWarmSessionTTL() time.Duration {
	raw := os.Getenv("KOE_WARM_SESSION_TTL_MS")
	if raw == "" {
		return warmSessionTTL
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return warmSessionTTL
	}
	return time.Duration(ms) * time.Millisecond
}

func armAudioStartWatchdog(label string, timeout time.Duration) func() {
	done := make(chan struct{})
	go func() {
		timer := time.NewTimer(timeout)
		defer timer.Stop()
		select {
		case <-done:
		case <-timer.C:
			log.Printf("koe: %s audio start timed out after %s; exiting for supervisor restart", label, timeout)
			os.Exit(audioStartTimeoutExitCode)
		}
	}()
	return func() { close(done) }
}

func desktopParentDone(ctx context.Context) <-chan struct{} {
	done := make(chan struct{})
	parentPID := os.Getppid()
	if parentPID <= 1 {
		close(done)
		return done
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if ppid := os.Getppid(); ppid != parentPID || ppid <= 1 {
					log.Printf("koe: desktop parent exited; shutting down parent_pid=%d current_ppid=%d", parentPID, ppid)
					close(done)
					return
				}
			}
		}
	}()
	return done
}

// koeLanguageInstruction maps the koe.language config ("en"/"ja"/"zh"; empty =
// follow the utterance) to a persona directive that pins the reply language,
// overriding koePersona's default "reply in the user's current utterance language".
func koeLanguageInstruction(lang string) string {
	switch lang {
	case "en":
		return "Always reply in English, regardless of the language the user speaks."
	case "ja":
		return "Always reply in Japanese (日本語), regardless of the language the user speaks."
	case "zh":
		return "Always reply in Chinese (简体中文), regardless of the language the user speaks."
	default:
		return ""
	}
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

	// A user-pinned reply language (Settings → Voice → Language) overrides
	// koePersona's default "reply in the user's current utterance language".
	// Empty (follow system / auto) keeps that default behavior.
	if instr := koeLanguageInstruction(cfg.language); instr != "" {
		persona = persona + " " + instr
	}

	// ── Kocoro Desktop (control-port) mode: warm session + call-scoped audio ──
	// Koe pre-warms the next OpenAI session while idle, but it opens the local audio
	// device only for an active Desktop call. This keeps VPIO from holding the mic
	// and macOS voice-processing output path while Kocoro voice is idle.
	if cfg.controlPort != "" {
		return runDesktopCall(ctx, cfg, client, resolver, persona, mintEK, onUsage)
	}

	// ── Standalone / headless mode: always-on (CLI + E2E + --say/--audio-in) ──
	state := koe.NewCallState(newBurstID(), cfg.agent)
	disp := koe.NewDispatcher(client, resolver, state, nil)
	audio, err := koe.NewAudioIO()
	if err != nil {
		return fmt.Errorf("audio init: %v", err)
	}
	startAudio := audio.Start
	fullDuplexAEC := cfg.aec == "vpio"
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
		fullDuplexAEC = false
	} else if fullDuplexAEC {
		startAudio = audio.StartVPIO
	}
	disarmAudioWatchdog := func() {}
	if fullDuplexAEC && !fileMode {
		disarmAudioWatchdog = armAudioStartWatchdog("standalone vpio", koeAudioStartTimeout())
	}
	if err := startAudio(); err != nil {
		disarmAudioWatchdog()
		return fmt.Errorf("audio start: %v", err)
	}
	disarmAudioWatchdog()
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
		OnVoiceState:  onVoiceState,
		Model:         cfg.model,
		Voice:         cfg.voice,
		OnUsage:       onUsage,
		FullDuplexAEC: fullDuplexAEC,
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

// runDesktopCall is the resident control-port loop. Desktop keeps a warm Realtime
// session ready while idle, but the audio device is call-scoped: /call/start opens
// the selected backend, /call/end closes the used session and audio, then warms
// the next session without touching the mic.
func runDesktopCall(ctx context.Context, cfg koeConfig, client *koe.DaemonClient,
	resolver *koe.AgentResolver, persona string,
	mintEK func(context.Context) (string, error), onUsage func(json.RawMessage)) error {

	fullDuplexAEC := cfg.aec == "vpio"

	var ctrl *koe.ControlServer
	var callContext koe.StartCallRequest
	newSessionState := func() (*koe.CallState, *koe.Dispatcher) {
		state := koe.NewCallState(newBurstID(), cfg.agent)
		state.SetCallContext(callContext)
		disp := koe.NewDispatcher(client, resolver, state, func(_ context.Context, action string) error {
			if ctrl == nil {
				return nil
			}
			ctrl.EmitControlApp(action)
			return nil
		})
		return state, disp
	}
	warm := newWarmMint(ctx, mintEK, warmMintTTL)

	// The RealtimeConn is warmed while idle, then consumed by one foreground call.
	// callActive gates mic frames inside pumpSendTrack; inactive sessions drain and
	// discard local capture, so OpenAI never hears the room before the double-tap.
	var sessMu sync.Mutex
	var curConn *koe.RealtimeConn
	var curState *koe.CallState
	var curAudio *koe.AudioIO
	var curAudioStarted bool
	var sessionCancel context.CancelFunc
	var sessionSeq uint64
	var warming bool
	var sessionReady bool
	var callActive bool
	var callStarted time.Time
	var readyEmitted bool
	idleSessionTTL := koeWarmSessionTTL()

	stopSessionResources := func(conn *koe.RealtimeConn, cancel context.CancelFunc, audio *koe.AudioIO) {
		if cancel != nil {
			cancel()
		}
		if conn != nil {
			conn.Close()
		}
		if audio != nil {
			audio.Stop()
		}
	}
	startSessionAudioLocked := func(reason string) error {
		if curAudioStarted {
			return nil
		}
		if curAudio == nil {
			return fmt.Errorf("audio session unavailable")
		}
		curAudio.PrepareForCall()
		startAudio := curAudio.Start
		if fullDuplexAEC {
			startAudio = curAudio.StartVPIO
		}
		started := time.Now()
		disarmAudioWatchdog := armAudioStartWatchdog("desktop call audio", koeAudioStartTimeout())
		if err := startAudio(); err != nil {
			disarmAudioWatchdog()
			return err
		}
		curAudioStarted = true
		curAudio.SetPlaybackEnabled(false)
		disarmAudioWatchdog()
		log.Printf("koe[timing]: desktop call audio ready in %dms aec=%s reason=%s", time.Since(started).Milliseconds(), cfg.aec, reason)
		return nil
	}
	emitReadyLocked := func() {
		if !callActive || !sessionReady || !curAudioStarted || readyEmitted {
			return
		}
		readyEmitted = true
		if !callStarted.IsZero() {
			log.Printf("koe[timing]: call ready in %dms warm_session=true", time.Since(callStarted).Milliseconds())
		}
		ctrl.EmitCallState("on_call")
		ctrl.EmitVoiceState("listening")
	}
	closeSessionLocked := func() (*koe.RealtimeConn, context.CancelFunc, *koe.AudioIO) {
		sessionSeq++
		conn, cancel := curConn, sessionCancel
		audio := curAudio
		curConn, curState, curAudio, sessionCancel = nil, nil, nil, nil
		curAudioStarted = false
		callContext = koe.StartCallRequest{}
		warming = false
		sessionReady = false
		readyEmitted = false
		callStarted = time.Time{}
		return conn, cancel, audio
	}
	var ensureWarmSessionLocked func(string)
	var scheduleWarmRotationLocked func(uint64, string)
	var handleSessionClosed func(uint64, error)
	scheduleWarmRetry := func(reason string) {
		go func() {
			select {
			case <-time.After(5 * time.Second):
			case <-ctx.Done():
				return
			}
			sessMu.Lock()
			defer sessMu.Unlock()
			if curConn == nil && !warming {
				ensureWarmSessionLocked(reason)
			}
		}()
	}
	scheduleWarmRotationLocked = func(seq uint64, reason string) {
		if idleSessionTTL <= 0 {
			return
		}
		go func() {
			select {
			case <-time.After(idleSessionTTL):
			case <-ctx.Done():
				return
			}
			sessMu.Lock()
			if seq != sessionSeq || callActive || curConn == nil {
				sessMu.Unlock()
				return
			}
			log.Printf("koe[timing]: refreshing idle warm session after %s reason=%s", idleSessionTTL, reason)
			conn, cancel, audio := closeSessionLocked()
			ensureWarmSessionLocked("ttl_refresh")
			sessMu.Unlock()
			stopSessionResources(conn, cancel, audio)
		}()
	}
	handleSessionClosed = func(seq uint64, err error) {
		sessMu.Lock()
		if seq != sessionSeq {
			sessMu.Unlock()
			return
		}
		log.Printf("koe: warm session closed: %v", err)
		wasActive := callActive
		callActive = false
		conn, cancel, audio := closeSessionLocked()
		if wasActive {
			ctrl.EmitVoiceState("idle")
			ctrl.EmitCallState("ended")
		}
		ensureWarmSessionLocked("session_closed")
		sessMu.Unlock()
		stopSessionResources(conn, cancel, audio)
	}
	failActiveCallLocked := func(msg string, err error) {
		log.Printf("koe: %s: %v", msg, err)
		if callActive {
			callActive = false
			callStarted = time.Time{}
			readyEmitted = false
			ctrl.EmitVoiceState("idle")
			ctrl.EmitCallState("ended")
		}
	}
	ensureWarmSessionLocked = func(reason string) {
		if ctx.Err() != nil {
			return
		}
		if curConn != nil || warming {
			return
		}
		audio, aerr := koe.NewAudioIO()
		if aerr != nil {
			failActiveCallLocked("audio init failed", aerr)
			scheduleWarmRetry("audio_init_retry")
			return
		}
		audio.SetPlaybackEnabled(false)
		started := time.Now()
		warming = true
		sessionReady = false
		readyEmitted = false
		sessionSeq++
		seq := sessionSeq
		state, disp := newSessionState()
		curState = state
		curAudio = audio
		curAudioStarted = false
		sessionCtx, cancel := context.WithCancel(ctx)
		sessionCancel = cancel
		log.Printf("koe[timing]: warming realtime session reason=%s burst=%s", reason, state.BurstID())

		go func() {
			mctx, mcancel := context.WithTimeout(sessionCtx, 15*time.Second)
			ek, cachedMint, merr := warm.take(mctx)
			mcancel()
			if merr != nil {
				var conn *koe.RealtimeConn
				var scancel context.CancelFunc
				var audio *koe.AudioIO
				sessMu.Lock()
				if seq == sessionSeq {
					failActiveCallLocked("mint failed", merr)
					conn, scancel, audio = closeSessionLocked()
					scheduleWarmRetry("mint_retry")
				}
				sessMu.Unlock()
				if conn != nil || scancel != nil || audio != nil {
					stopSessionResources(conn, scancel, audio)
				} else {
					cancel()
				}
				return
			}
			log.Printf("koe[timing]: warm session mint ready in %dms warm=%t reason=%s", time.Since(started).Milliseconds(), cachedMint, reason)

			isActive := func() bool {
				sessMu.Lock()
				defer sessMu.Unlock()
				return seq == sessionSeq && callActive
			}
			onVoiceState := func(s string) {
				if isActive() {
					ctrl.EmitVoiceState(s)
				}
			}
			onVoiceLevel := func(s string, level float64) {
				if isActive() {
					ctrl.EmitVoiceLevel(s, level)
				}
			}
			onCallState := func(s string) {
				switch s {
				case "connecting":
					return
				case "on_call":
					sessMu.Lock()
					if seq == sessionSeq {
						sessionReady = true
						log.Printf("koe[timing]: warm session ready in %dms reason=%s", time.Since(started).Milliseconds(), reason)
						emitReadyLocked()
						scheduleWarmRotationLocked(seq, reason)
					}
					sessMu.Unlock()
				default:
					if isActive() {
						ctrl.EmitCallState(s)
					}
				}
			}
			callActiveFn := func() bool {
				sessMu.Lock()
				defer sessMu.Unlock()
				return seq == sessionSeq && callActive
			}

			connectWith := func(secret string) (*koe.RealtimeConn, error) {
				return koe.Connect(sessionCtx, audio, secret, persona, state, disp, koe.ConnectOptions{
					OnVoiceState:  onVoiceState,
					OnCallState:   onCallState,
					OnVoiceLevel:  onVoiceLevel,
					CallActive:    callActiveFn,
					Model:         cfg.model,
					Voice:         cfg.voice,
					OnUsage:       onUsage,
					FullDuplexAEC: fullDuplexAEC,
					OnClosed:      func(err error) { handleSessionClosed(seq, err) },
				})
			}
			conn, cerr := connectWith(ek)
			if cerr != nil && cachedMint {
				log.Printf("koe[timing]: warm session cached mint connect failed after %dms, retrying fresh: %v", time.Since(started).Milliseconds(), cerr)
				fctx, fcancel := context.WithTimeout(sessionCtx, 15*time.Second)
				fresh, ferr := mintEK(fctx)
				fcancel()
				if ferr == nil {
					conn, cerr = connectWith(fresh)
				} else {
					log.Printf("koe[timing]: warm session fresh mint retry failed: %v", ferr)
				}
			}
			sessMu.Lock()
			if seq != sessionSeq {
				sessMu.Unlock()
				if conn != nil {
					conn.Close()
				}
				cancel()
				return
			}
			if cerr != nil {
				failActiveCallLocked("connect failed", cerr)
				conn, scancel, audio := closeSessionLocked()
				scheduleWarmRetry("connect_retry")
				sessMu.Unlock()
				stopSessionResources(conn, scancel, audio)
				return
			}
			curConn = conn
			warming = false
			emitReadyLocked()
			sessMu.Unlock()
		}()
	}

	startCall := func(req koe.StartCallRequest) {
		sessMu.Lock()
		if callActive {
			sessMu.Unlock()
			return
		}
		callContext = req
		if curState != nil {
			// Warm sessions are created before Desktop knows which app/window was
			// foregrounded at the actual wake gesture. Patch the per-call context
			// into the live state before any do_task can be prepared.
			curState.SetCallContext(req)
		}
		callStarted = time.Now()
		readyEmitted = false
		ctrl.EmitCallState("connecting")
		if curConn == nil && !warming {
			ensureWarmSessionLocked("call_start")
		}
		if err := startSessionAudioLocked("call_start"); err != nil {
			log.Printf("koe: audio start failed: %v", err)
			ctrl.EmitVoiceState("idle")
			ctrl.EmitCallState("ended")
			conn, cancel, audio := closeSessionLocked()
			ensureWarmSessionLocked("audio_start_retry")
			sessMu.Unlock()
			stopSessionResources(conn, cancel, audio)
			return
		}
		callActive = true
		if curConn != nil && sessionReady {
			emitReadyLocked()
			sessMu.Unlock()
			return
		}
		ensureWarmSessionLocked("call_start")
		sessMu.Unlock()
	}

	endCall := func() {
		sessMu.Lock()
		if !callActive {
			sessMu.Unlock()
			return
		}
		callActive = false
		conn, cancel, audio := closeSessionLocked()
		ctrl.EmitVoiceState("idle")
		ctrl.EmitCallState("ended")
		ensureWarmSessionLocked("post_call")
		sessMu.Unlock()
		stopSessionResources(conn, cancel, audio)
	}

	ctrl = koe.NewControlServer(startCall, endCall)
	go func() {
		addr := "127.0.0.1:" + cfg.controlPort
		if err := http.ListenAndServe(addr, ctrl.Handler()); err != nil {
			log.Printf("koe: control server on %s exited: %v", addr, err)
		}
	}()

	sessMu.Lock()
	ensureWarmSessionLocked("startup")
	sessMu.Unlock()

	select {
	case <-ctx.Done():
	case <-desktopParentDone(ctx):
	}
	sessMu.Lock()
	conn, cancel, audio := closeSessionLocked()
	sessMu.Unlock()
	stopSessionResources(conn, cancel, audio)
	return nil
}
