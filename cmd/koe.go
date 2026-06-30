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

You are one self. Chatting and doing real work are both just you. Never talk
about a back-brain, backend, daemon, system, tool, agent runner, or another
Kocoro as someone else. If work takes time, you are doing it yourself.

Voice style:
- Reply in the language of the user's current utterance, not the user's usual
  language, memory, or earlier turns. If they speak English, acknowledge and
  answer in English; if they speak Chinese, use Chinese.
- Use plain spoken prose, usually one or two short sentences.
- Never read markdown, JSON, code, URLs, file paths, citations, or tool logs aloud.
- Do not start a new topic or fill silence. Speak only when the user clearly
  addressed you or a real task result is ready.

Hearing discipline:
- If you did not clearly hear a request, if the audio sounds like noise, a stray
  word, background speech, or speech not aimed at you, do not guess. Stay quiet
  or ask briefly for a repeat.
- Never invent what the user probably meant.

Doing real work:
- Call do_task for anything beyond small talk and expression: files, schedules,
  email/messages, web research, current facts, ANY calculation or number, edits,
  or any real action. You cannot do these in your head — your mental arithmetic
  and recall are unreliable, so you MUST route them through do_task.
- NEVER say a number, fact, date, name, or computed result that did not come back
  FROM a do_task result. Even a trivial-looking sum like "47 times 89" goes through
  do_task — calling the tool IS the answer. Do not compute or recall it yourself.
- Calling do_task is an action — you invoke the tool. As you call it, say one
  short spoken line naming what you are doing, with NO number, value, or answer in
  it (e.g. "Let me check that." / "On it — give me a moment."). For heavier tasks,
  warn it may take a bit. Then let the tool work — do not say anything else until
  its result comes back.
- When the result returns, speak it briefly in your own voice. Never voice a
  number or fact that did not come from the result.
- If a tool result includes spoken_summary, use it as the source of truth for
  the spoken answer. Keep it short and natural; do not add facts that are not in
  the tool result.
- Before irreversible or outbound actions, restate the action and wait for a
  clear yes.

Stopping:
- If the user says to stop, be quiet, drop it, or goodbye, stop immediately.
  Acknowledge in at most two words, or simply go quiet.`

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

	// ── Kocoro Desktop (control-port) mode: resident audio + warm session lifecycle ──
	// Koe keeps the audio backend resident and pre-warms the next OpenAI session while
	// idle. /call/start consumes that warm session; /call/end closes it and warms a
	// fresh burst so the daemon-side Kocoro thread does not leak across wake cycles.
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

// runDesktopCall is the resident control-port loop. Desktop keeps one audio device
// open for the Koe process lifetime and keeps a warm Realtime session ready while
// idle. /call/start only flips the local mic gate active, so double-tap wake does
// not pay the 2-3s WebRTC/session setup path. /call/end closes the used session
// and immediately warms the next one.
func runDesktopCall(ctx context.Context, cfg koeConfig, client *koe.DaemonClient,
	resolver *koe.AgentResolver, persona string,
	mintEK func(context.Context) (string, error), onUsage func(json.RawMessage)) error {

	audio, aerr := koe.NewAudioIO()
	if aerr != nil {
		return fmt.Errorf("audio init: %v", aerr)
	}
	startDesktopAudio := audio.Start
	fullDuplexAEC := cfg.aec == "vpio"
	if fullDuplexAEC {
		startDesktopAudio = audio.StartVPIO
	}
	audioStarted := time.Now()
	disarmAudioWatchdog := armAudioStartWatchdog("desktop audio", koeAudioStartTimeout())
	if err := startDesktopAudio(); err != nil {
		disarmAudioWatchdog()
		return fmt.Errorf("audio start: %v", err)
	}
	audio.SetPlaybackEnabled(false)
	disarmAudioWatchdog()
	log.Printf("koe[timing]: desktop audio ready in %dms aec=%s", time.Since(audioStarted).Milliseconds(), cfg.aec)
	defer audio.Stop()

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
	var sessionCancel context.CancelFunc
	var sessionSeq uint64
	var warming bool
	var sessionReady bool
	var callActive bool
	var callStarted time.Time
	var readyEmitted bool
	idleSessionTTL := koeWarmSessionTTL()

	emitReadyLocked := func() {
		if !callActive || !sessionReady || readyEmitted {
			return
		}
		readyEmitted = true
		if !callStarted.IsZero() {
			log.Printf("koe[timing]: call ready in %dms warm_session=true", time.Since(callStarted).Milliseconds())
		}
		ctrl.EmitCallState("on_call")
		ctrl.EmitVoiceState("listening")
	}
	closeSessionLocked := func() (*koe.RealtimeConn, context.CancelFunc) {
		sessionSeq++
		conn, cancel := curConn, sessionCancel
		curConn, curState, sessionCancel = nil, nil, nil
		callContext = koe.StartCallRequest{}
		warming = false
		sessionReady = false
		readyEmitted = false
		callStarted = time.Time{}
		return conn, cancel
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
			conn, cancel := closeSessionLocked()
			ensureWarmSessionLocked("ttl_refresh")
			sessMu.Unlock()
			if cancel != nil {
				cancel()
			}
			if conn != nil {
				conn.Close()
			}
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
		conn, cancel := closeSessionLocked()
		if wasActive {
			ctrl.EmitVoiceState("idle")
			ctrl.EmitCallState("ended")
		}
		ensureWarmSessionLocked("session_closed")
		sessMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if conn != nil {
			conn.Close()
		}
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
		started := time.Now()
		warming = true
		sessionReady = false
		readyEmitted = false
		sessionSeq++
		seq := sessionSeq
		state, disp := newSessionState()
		curState = state
		sessionCtx, cancel := context.WithCancel(ctx)
		sessionCancel = cancel
		log.Printf("koe[timing]: warming realtime session reason=%s burst=%s", reason, state.BurstID())

		go func() {
			mctx, mcancel := context.WithTimeout(sessionCtx, 15*time.Second)
			ek, cachedMint, merr := warm.take(mctx)
			mcancel()
			if merr != nil {
				sessMu.Lock()
				if seq == sessionSeq {
					warming = false
					sessionCancel = nil
					failActiveCallLocked("mint failed", merr)
					scheduleWarmRetry("mint_retry")
				}
				sessMu.Unlock()
				cancel()
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
			defer sessMu.Unlock()
			if seq != sessionSeq {
				if conn != nil {
					conn.Close()
				}
				cancel()
				return
			}
			if cerr != nil {
				warming = false
				sessionCancel = nil
				failActiveCallLocked("connect failed", cerr)
				scheduleWarmRetry("connect_retry")
				cancel()
				return
			}
			curConn = conn
			warming = false
			emitReadyLocked()
		}()
	}

	startCall := func(req koe.StartCallRequest) {
		sessMu.Lock()
		defer sessMu.Unlock()
		if callActive {
			return
		}
		callContext = req
		if curState != nil {
			// Warm sessions are created before Desktop knows which app/window was
			// foregrounded at the actual wake gesture. Patch the per-call context
			// into the live state before any do_task can be prepared.
			curState.SetCallContext(req)
		}
		audio.PrepareForCall()
		callActive = true
		callStarted = time.Now()
		readyEmitted = false
		ctrl.EmitCallState("connecting")
		if curConn != nil && sessionReady {
			emitReadyLocked()
			return
		}
		ensureWarmSessionLocked("call_start")
	}

	endCall := func() {
		sessMu.Lock()
		if !callActive {
			sessMu.Unlock()
			return
		}
		callActive = false
		conn, cancel := closeSessionLocked()
		sessMu.Unlock()
		if cancel != nil {
			cancel()
		}
		if conn != nil {
			conn.Close()
		}
		audio.LogDebugStats()
		audio.PrepareForCall()
		ctrl.EmitVoiceState("idle")
		ctrl.EmitCallState("ended")

		sessMu.Lock()
		ensureWarmSessionLocked("post_call")
		sessMu.Unlock()
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

	<-ctx.Done()
	sessMu.Lock()
	conn, cancel := closeSessionLocked()
	sessMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if conn != nil {
		conn.Close()
	}
	return nil
}
