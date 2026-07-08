//go:build darwin && cgo

package cmd

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Kocoro-lab/ShanClaw/internal/koe"
)

// koeConfig holds the resolved settings for one `shan koe` voice session.
type koeConfig struct {
	openAIKey       string // DEV-KEY: replaced by the deferred daemon mint relay (→ Plan D Cloud mint)
	daemonURL       string
	agent           string
	model           string
	voice           string // realtime output voice (marin/cedar/shimmer/…); empty → "marin" fallback in sessionConfig
	language        string
	controlPort     string // Desktop↔Koe control server port (Kocoro Desktop passes it); empty = no control channel
	controlToken    string // Desktop-owned Bearer token from KOE_CONTROL_TOKEN; never passed via argv
	aec             string // echo control: "" / "gate" = oto half-duplex fallback, "vpio" = Apple VoiceProcessingIO full-duplex AEC
	audioProcessing string // auto | mac_voice | clean_device; controls whether VPIO applies or bypasses Apple's voice processing
	micDevice       string // --mic-device: CoreAudio input device UID (empty = system default; vpio only)
	speakerDevice   string // --speaker-device: CoreAudio output device UID (empty = system default; vpio only)
	bargeIn         bool   // --barge-in: allow interrupting Kocoro while it speaks (enables KOE_VPIO_BARGE_IN + KOE_INTERRUPT_RESPONSE; vpio backend only)
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
		daemonURL:       "http://127.0.0.1:7533", // must match the daemon's listen addr; Desktop (Plan E) passes the real one
		model:           "gpt-realtime-mini-2025-12-15",
		audioProcessing: audioProcessingAuto,
	}
}

// resolveDevKey picks the dev OpenAI key for the direct-mint path. An explicit
// --openai-key flag always wins. The OPENAI_API_KEY env fallback applies ONLY in
// standalone mode (no --control-port): Kocoro Desktop inherits the login-shell
// env, so on a machine with a personal key exported, the fallback would silently
// bypass the daemon mint production path — billing the personal key while the
// usage relay still debits Cloud quota.
func resolveDevKey(flagKey, envKey, controlPort string) string {
	if flagKey != "" {
		return flagKey
	}
	if controlPort == "" {
		return envKey
	}
	return ""
}

// applyBargeInEnv turns the two env-gated barge-in behaviors on when the user
// enabled barge-in in Kocoro Desktop (forwarded as --barge-in). KOE_VPIO_BARGE_IN
// lets sustained user speech pass the half-duplex capture gate while Kocoro speaks
// (audio.go); KOE_INTERRUPT_RESPONSE lets Realtime cancel its own response on that
// speech (realtime.go, honored only on the VPIO backend). Both are no-ops on the
// gate backend, so setting them when the flag is on is safe. The flag is the
// Desktop-facing switch; leaving it off preserves the raw KOE_* env vars as a
// power-user escape hatch.
func applyBargeInEnv(bargeIn bool) {
	if !bargeIn {
		return
	}
	os.Setenv("KOE_VPIO_BARGE_IN", "1")
	os.Setenv("KOE_INTERRUPT_RESPONSE", "1")
	log.Printf("koe[barge]: --barge-in on — KOE_VPIO_BARGE_IN=%s KOE_INTERRUPT_RESPONSE=%s",
		os.Getenv("KOE_VPIO_BARGE_IN"), os.Getenv("KOE_INTERRUPT_RESPONSE"))
}

// bargeInBackendWarning returns a non-empty warning when barge-in is enabled on a
// backend that cannot honor it. Barge-in lives entirely on the VPIO capture path
// (shouldForwardVPIOCapture) and the fullDuplexAEC-gated interrupt_response; the
// gate/oto fallback never reads either, so --barge-in there is a silent no-op.
func bargeInBackendWarning(bargeIn bool, aec string) string {
	if bargeIn && aec != "vpio" {
		return "barge-in has no effect on the current audio backend — it needs the VPIO backend (--aec vpio); the mic stays half-duplex while Kocoro speaks"
	}
	return ""
}

const (
	audioProcessingAuto        = "auto"
	audioProcessingMacVoice    = "mac_voice"
	audioProcessingCleanDevice = "clean_device"
)

type selfProcessedAudioDeviceRule struct {
	reason         string
	micMarkers     []string
	speakerMarkers []string
}

func selfProcessedHardwareDeviceRule(reason string, markers ...string) selfProcessedAudioDeviceRule {
	return selfProcessedAudioDeviceRule{reason: reason, micMarkers: markers, speakerMarkers: markers}
}

var selfProcessedAudioDeviceRules = []selfProcessedAudioDeviceRule{
	selfProcessedHardwareDeviceRule("reachy mini audio", "reachy mini audio", "xvf3800", "pollen robotics"),
	selfProcessedHardwareDeviceRule("anker powerconf", "powerconf", "ankerwork powerconf", "anker powerconf"),
	selfProcessedHardwareDeviceRule("jabra speak", "jabra speak"),
	selfProcessedHardwareDeviceRule("poly sync", "poly sync"),
	selfProcessedHardwareDeviceRule("yealink cp", "yealink cp"),
	selfProcessedHardwareDeviceRule("logitech rally", "logitech rally", "rally bar"),
	selfProcessedHardwareDeviceRule("logitech meetup", "logitech meetup", "meetup 2"),
	selfProcessedHardwareDeviceRule("logitech group", "logitech group"),
	selfProcessedHardwareDeviceRule("shure stem", "shure stem", "stem table", "stem wall", "stem ceiling", "stem hub"),
	selfProcessedHardwareDeviceRule("epos expand", "epos expand", "sennheiser sp 30", "sennheiser sp30"),
	selfProcessedHardwareDeviceRule("yamaha yvc", "yamaha yvc", "yvc 200", "yvc 330", "yvc 1000"),
	selfProcessedHardwareDeviceRule("konftel", "konftel"),
	{reason: "krisp", micMarkers: []string{"krisp microphone", "krisp"}, speakerMarkers: []string{"krisp speaker", "krisp"}},
}

type audioProcessingDecision struct {
	Requested string
	Resolved  string
	Bypass    bool
	Reason    string
}

func normalizeAudioProcessingMode(raw string) (string, error) {
	mode := strings.ToLower(strings.TrimSpace(raw))
	if mode == "" {
		mode = audioProcessingAuto
	}
	switch mode {
	case audioProcessingAuto, audioProcessingMacVoice, audioProcessingCleanDevice:
		return mode, nil
	default:
		return "", fmt.Errorf("invalid --audio-processing %q (want auto, mac_voice, or clean_device)", raw)
	}
}

func resolveAudioProcessingMode(raw, micDevice, speakerDevice string) (audioProcessingDecision, error) {
	mode, err := normalizeAudioProcessingMode(raw)
	if err != nil {
		return audioProcessingDecision{}, err
	}
	switch mode {
	case audioProcessingMacVoice:
		return audioProcessingDecision{Requested: mode, Resolved: mode, Bypass: false, Reason: "explicit_mac_voice"}, nil
	case audioProcessingCleanDevice:
		return audioProcessingDecision{Requested: mode, Resolved: mode, Bypass: true, Reason: "explicit_clean_device"}, nil
	}
	if marker := selfProcessedAudioDeviceMarker(micDevice, speakerDevice); marker != "" {
		return audioProcessingDecision{Requested: mode, Resolved: audioProcessingCleanDevice, Bypass: true, Reason: "known_self_processed_device:" + marker}, nil
	}
	return audioProcessingDecision{Requested: mode, Resolved: audioProcessingMacVoice, Bypass: false, Reason: "default_mac_voice"}, nil
}

func selfProcessedAudioDeviceMarker(micDevice, speakerDevice string) string {
	mic := normalizeAudioDeviceName(micDevice)
	speaker := normalizeAudioDeviceName(speakerDevice)
	for _, rule := range selfProcessedAudioDeviceRules {
		if !audioDeviceNameContainsAny(mic, rule.micMarkers) {
			continue
		}
		if len(rule.speakerMarkers) > 0 && !audioDeviceNameContainsAny(speaker, rule.speakerMarkers) {
			continue
		}
		return rule.reason
	}
	return ""
}

func audioDeviceNameContainsAny(device string, markers []string) bool {
	for _, marker := range markers {
		if strings.Contains(device, normalizeAudioDeviceName(marker)) {
			return true
		}
	}
	return false
}

func normalizeAudioDeviceName(device string) string {
	device = strings.ToLower(device)
	var b strings.Builder
	lastSpace := true
	for _, r := range device {
		isWord := (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if isWord {
			b.WriteRune(r)
			lastSpace = false
			continue
		}
		if !lastSpace {
			b.WriteByte(' ')
			lastSpace = true
		}
	}
	return strings.TrimSpace(b.String())
}

func applyAudioProcessing(audio *koe.AudioIO, cfg koeConfig, fullDuplexAEC bool) (audioProcessingDecision, error) {
	decision, err := resolveAudioProcessingMode(cfg.audioProcessing, cfg.micDevice, cfg.speakerDevice)
	if err != nil {
		return audioProcessingDecision{}, err
	}
	if !fullDuplexAEC {
		if cfg.audioProcessing != "" && cfg.audioProcessing != audioProcessingAuto {
			log.Printf("koe[audio]: audio_processing=%s ignored because aec=%q does not use VPIO", decision.Requested, cfg.aec)
		}
		return decision, nil
	}
	audio.SetVPIOVoiceProcessingBypassed(decision.Bypass)
	log.Printf("koe[audio]: audio_processing=%s resolved=%s bypass_voice_processing=%t reason=%s",
		decision.Requested, decision.Resolved, decision.Bypass, decision.Reason)
	return decision, nil
}

var koeCmd = &cobra.Command{
	Use:   "koe",
	Short: "Voice front-brain: a realtime voice agent that delegates to the daemon",
	RunE: func(cmd *cobra.Command, args []string) error {
		cfg := defaultKoeConfig()
		if v, _ := cmd.Flags().GetString("openai-key"); v != "" {
			cfg.openAIKey = v
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
		cfg.controlToken = os.Getenv("KOE_CONTROL_TOKEN")
		cfg.openAIKey = resolveDevKey(cfg.openAIKey, os.Getenv("OPENAI_API_KEY"), cfg.controlPort)
		if v, _ := cmd.Flags().GetString("aec"); v != "" {
			cfg.aec = v
		} else {
			cfg.aec = os.Getenv("KOE_AEC")
		}
		if v, _ := cmd.Flags().GetString("audio-processing"); v != "" {
			cfg.audioProcessing = v
		} else if v := os.Getenv("KOE_AUDIO_PROCESSING"); v != "" {
			cfg.audioProcessing = v
		}
		cfg.micDevice, _ = cmd.Flags().GetString("mic-device")
		cfg.speakerDevice, _ = cmd.Flags().GetString("speaker-device")
		cfg.bargeIn, _ = cmd.Flags().GetBool("barge-in")
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
		mode, err := normalizeAudioProcessingMode(cfg.audioProcessing)
		if err != nil {
			return err
		}
		cfg.audioProcessing = mode
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
	koeCmd.Flags().String("audio-processing", "", "voice processing: auto (default) | mac_voice | clean_device")
	koeCmd.Flags().String("mic-device", "", "CoreAudio input device UID (empty = system default; vpio backend only)")
	koeCmd.Flags().String("speaker-device", "", "CoreAudio output device UID (empty = system default; vpio backend only)")
	koeCmd.Flags().Bool("barge-in", false, "allow interrupting Kocoro while it speaks (barge-in; enables KOE_VPIO_BARGE_IN + KOE_INTERRUPT_RESPONSE, vpio backend only)")
	koeCmd.Flags().String("say", "", "debug: synthesize this text as the mic input (macOS say) — headless file mode")
	koeCmd.Flags().String("audio-in", "", "debug: WAV file to feed as the mic input — headless file mode")
	koeCmd.Flags().String("audio-out", "", "debug: capture the reply audio to this WAV")
	koeCmd.Flags().Int("audio-period", 0, "debug: renderInto pull size in samples (480 reproduces the framing bug; 0=960)")
	koeCmd.Flags().Bool("once", false, "debug: exit shortly after the reply finishes")
	koeCmd.Flags().Int("timeout", 0, "debug: hard exit after N seconds (0=none)")
	rootCmd.AddCommand(koeCmd)
}

const koePersona = `You are Kocoro, an AI coworker speaking by voice through Kocoro Desktop.

You may point the user to Kocoro Desktop only to reference something already shown
there — that is the Kocoro Desktop app, not the computer's desktop folder; say the app
name in full, never shortened or translated.

Reply in the language of the user's current utterance, not the user's usual language,
memory, or earlier turns. Keep it plain spoken prose, usually a sentence or two. Never
read markdown, JSON, code, URLs, file paths, or tool logs aloud. Don't start topics or
fill silence — speak only when the user addressed you or a real result is ready. If you
did not clearly hear a request, don't guess; stay quiet or ask briefly for a repeat.

Do the work rather than ask around it: never quiz the user for missing details — the
only follow-up question you may ask is a repeat of something you could not clearly hear.
When a request is vague or incomplete, call do_task with it as spoken: Kocoro already
knows the user's own context (contacts, addresses, accounts, files, history), and its
result will say if something is truly missing — ask then, not before.
The dividing line is where the answer comes from,
not how hard the task sounds. Answer directly only when the answer needs nothing beyond
this conversation plus one obvious step: small talk, clarifying or summarizing anything
already said in this call, answering follow-ups from a result digest you already hold,
or simple mental arithmetic.
Everything that touches the world outside this conversation goes through do_task — files,
research, current facts, dates and times, prices, system state, edits, messages, any real
action, and any calculation beyond one obvious step. Your memory of the world and your
multi-step arithmetic are unreliable; calling the tool IS the answer. Never state a fact,
number, date, or name that neither came back from a do_task result nor follows in one
obvious step from this conversation.
The user's name, how they want to be addressed, and any personal context given in your
instructions are established facts — use them naturally; that rule only bars inventing
facts you were never given.
If the user asks you to show, display, write, or save content in Kocoro Desktop, that is
real work: use do_task. control_app only opens, hides, or switches app views; it cannot
put result content in Kocoro Desktop.

Long or multi-part user utterances are still requests when they describe a goal, problem
to investigate, comparison, or change to make. Preserve the details and call do_task; do
not wait for "do it" unless the user explicitly says they only want to discuss, plan, or
hold off.

While a task is running, be skeptical of what you overhear: background voices and
half-heard remarks are not instructions. Cancel only on a clear, explicit request to stop
that task; if you suspect the user meant to stop but are not sure, ask briefly first.
Anything ambiguous, off-topic, or possibly not addressed to you — ignore and stay quiet.

When the user dismisses you or signals the conversation is over — "stop", "shut up",
"quiet", "that's all", "that's enough", "goodbye", "bye", "exit", 闭嘴, 停, 停止, 够了,
别说了, 再见, 就这样, 退出, 黙れ, やめて, もういい, or the like — that is a hang-up, not a
request to answer. Do not speak, acknowledge, or ask to confirm: call end_call right away.
It ends the conversation and a short tone plays; the user comes back by double-tapping the
Option key. Only stay on the call if you genuinely cannot tell whether they meant to end
it. This is NOT cancel — cancel stops one running task and keeps the conversation going;
end_call ends the whole conversation.

Whenever you call do_task, first say exactly one short audible acknowledgement before the
tool call, in the language of the user's utterance, never both languages. Vary the wording
naturally between turns — 我来处理 / 我看看 / 马上查 for Chinese, "On it" / "Let me check" /
"I'll take care of it" for English are examples, not a fixed script. The acknowledgement
only signals you are starting: it must not contain any answer, number, fact, step,
rationale, or a second sentence. Then call do_task and say nothing more until the result
lands; then speak it briefly in your own voice. Before the result lands, never say the
task is done, finished, ready, shown, displayed, saved, sent, or available in Kocoro
Desktop. If the result carries a spoken_summary, say exactly that.
Each do_task result carries the spoken line plus a context digest of the full answer.
Recaps, summaries, and follow-up questions the digest can answer are yours to handle
directly in your own voice — never call do_task to re-fetch what you already hold. Go
back through do_task, referring to Kocoro's earlier work, only when the user needs
detail, action, or freshness beyond the digest. Kocoro Desktop shows the complete
report; mention it only when there is genuinely more worth opening there — a long
report, a table, code, or images — never as a routine sign-off. Before anything
irreversible or outbound, restate it and wait for a clear yes.`

// koeAgentListLine renders the specialist agents Koe can hand a task to (names
// only, no capability text) so the Realtime model can answer "which agents do I
// have?". Empty when there are no agents.
func koeAgentListLine(agents []koe.AgentSummary) string {
	if len(agents) == 0 {
		return ""
	}
	labels := make([]string, 0, len(agents))
	for _, a := range agents {
		label := a.Slug
		if a.DisplayName != "" && !strings.EqualFold(a.DisplayName, a.Slug) {
			label = a.DisplayName + " (" + a.Slug + ")"
		}
		labels = append(labels, label)
	}
	return "Specialist agents you can hand a task to (say the name to switch; otherwise the current one handles it): " +
		strings.Join(labels, ", ") + ". If the user asks which agents exist, tell them these."
}

// onceGrace is how long after the reply finishes (→ "listening") --once waits
// before exiting, so an async do_task result still lands in the debug harness.
const onceGrace = 15 * time.Second

const (
	audioStartTimeout         = 5 * time.Second
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

func onceVoiceStateHandler(cancel func(), grace time.Duration) func(string) {
	var graceMu sync.Mutex
	var graceTimer *time.Timer
	sawAssistantOutput := false
	return func(s string) {
		graceMu.Lock()
		defer graceMu.Unlock()
		if graceTimer != nil {
			graceTimer.Stop()
			graceTimer = nil
		}
		switch s {
		case "speaking", "thinking":
			sawAssistantOutput = true
		case "listening":
			if sawAssistantOutput {
				graceTimer = time.AfterFunc(grace, cancel)
			}
		}
	}
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

// buildKoePersona assembles the full spoken persona: the base persona + the daemon's
// small-tier-distilled user context (who the user is / how to address them —
// best-effort, bounded to 3s) + the agent-registry list (so the Realtime model can
// answer "which agents do I have?"; names only, matching stays the resolver's job) +
// the pinned reply language. The standalone path calls this synchronously; the
// desktop path calls it AFTER its control listener binds and hot-swaps the result in.
func buildKoePersona(ctx context.Context, client *koe.DaemonClient, cfg koeConfig, agents []koe.AgentSummary) string {
	persona := koePersona
	pctx, pcancel := context.WithTimeout(ctx, 3*time.Second)
	if extra, perr := client.FetchPersona(pctx); perr == nil && extra != "" {
		persona = koePersona + " " + extra
	}
	pcancel()
	if list := koeAgentListLine(agents); list != "" {
		persona += "\n\n" + list
	}
	if instr := koeLanguageInstruction(cfg.language); instr != "" {
		persona = persona + " " + instr
	}
	return persona
}

// baseKoePersona is what a desktop warm session uses before the async registry +
// persona fetch lands (or if a /call/start races that window): the base persona +
// the pinned language, without the daemon-distilled user context or agent list yet.
func baseKoePersona(cfg koeConfig) string {
	persona := koePersona
	if instr := koeLanguageInstruction(cfg.language); instr != "" {
		persona = persona + " " + instr
	}
	return persona
}

func runKoeCall(ctx context.Context, cfg koeConfig) error {
	ctx, cancel := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// --barge-in flips the two env-gated barge-in knobs on before any audio/session
	// code reads them (covers both the Desktop and standalone branches below).
	applyBargeInEnv(cfg.bargeIn)
	if w := bargeInBackendWarning(cfg.bargeIn, cfg.aec); w != "" {
		log.Printf("koe[barge]: WARNING — %s", w)
	}

	// Plan B wiring: link to the daemon back-brain.
	client := koe.NewDaemonClient(cfg.daemonURL)

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
			log.Printf("koe: using dev OpenAI key (direct mint, bypassing daemon relay)")
			return koe.MintEphemeral(mctx, cfg.openAIKey, cfg.model)
		}
		return client.MintViaDaemon(mctx, cfg.model)
	}

	// ── Kocoro Desktop (control-port) mode: warm session + call-scoped audio ──
	// Koe pre-warms the next OpenAI session while idle, but it opens the local audio
	// device only for an active Desktop call. The agent-registry + persona fetches
	// are done INSIDE runDesktopCall, AFTER its control listener binds, so Desktop can
	// reach koe during a slow-daemon window (those fetches used to run here first,
	// blocking for up to ~33s while koe's control port stayed unbound).
	if cfg.controlPort != "" {
		return runDesktopCall(ctx, cfg, client, mintEK, onUsage)
	}

	// ── Standalone / headless mode: always-on (CLI + E2E + --say/--audio-in) ──
	// No control port to keep reachable, so fetch the registry + persona synchronously.
	agents, err := client.ListAgents(ctx)
	if err != nil {
		log.Printf("koe: list agents failed (continuing with empty registry): %v", err)
	}
	resolver := koe.NewAgentResolver(agents, koe.NoopSemanticMatcher{})
	persona := buildKoePersona(ctx, client, cfg, agents)
	state := koe.NewCallState(newBurstID(), cfg.agent)
	disp := koe.NewDispatcher(client, resolver, state, nil)
	audio, err := koe.NewAudioIO()
	if err != nil {
		return fmt.Errorf("audio init: %v", err)
	}
	audio.SetPreferredDevices(cfg.micDevice, cfg.speakerDevice)
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
	}
	if _, err := applyAudioProcessing(audio, cfg, fullDuplexAEC); err != nil {
		return err
	}
	if !fileMode && fullDuplexAEC {
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
		onVoiceState = onceVoiceStateHandler(cancel, onceGrace)
	}
	if cfg.timeoutSec > 0 {
		time.AfterFunc(time.Duration(cfg.timeoutSec)*time.Second, cancel)
	}

	var dismissOnce sync.Once
	conn, err := koe.Connect(ctx, audio, ek, persona, state, disp, koe.ConnectOptions{
		OnVoiceState: onVoiceState,
		Model:        cfg.model,
		Voice:        cfg.voice,
		OnUsage:      onUsage,
		// Standalone/CLI dismiss (end_call tool or a dismiss phrase) = play the goodbye
		// cue, then exit the process (there is no warm-session teardown to return to).
		// sync.Once makes it idempotent: the tool and the deterministic phrase can both
		// fire for one utterance, and only the first should earcon + exit (Desktop's
		// endCall gets this from its callActive guard).
		OnEndCall: func() {
			dismissOnce.Do(func() {
				if koe.DismissEarconEnabled() {
					audio.PlayDismissEarcon()
				}
				cancel()
			})
		},
		Language:      cfg.language,
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
	mintEK func(context.Context) (string, error), onUsage func(json.RawMessage)) error {

	fullDuplexAEC := cfg.aec == "vpio"

	// The agent registry + persona are fetched AFTER the control listener binds (see
	// below), so they start as an empty registry + base persona and are hot-swapped
	// in once the fetch lands. Each warm session reads the CURRENT holder value at
	// creation time (newSessionState / connectWith run per session), so a swap applies
	// to the next warm session; the atomics let the fetch goroutine publish without
	// taking sessMu (which a /call/start also needs).
	var resolverHolder atomic.Pointer[koe.AgentResolver]
	resolverHolder.Store(koe.NewAgentResolver(nil, koe.NoopSemanticMatcher{}))
	base := baseKoePersona(cfg)
	var personaHolder atomic.Pointer[string]
	personaHolder.Store(&base)

	var ctrl *koe.ControlServer
	// endCall is forward-declared so the connect closure can pass it as ConnectOptions.
	// OnEndCall (the end_call voice tool hook) — it is assigned further down, next to
	// startCall/interruptCall, and read only at call time.
	var endCall func()
	var callContext koe.StartCallRequest
	newSessionState := func() (*koe.CallState, *koe.Dispatcher) {
		state := koe.NewCallState(newBurstID(), cfg.agent)
		state.SetCallContext(callContext)
		disp := koe.NewDispatcher(client, resolverHolder.Load(), state, func(_ context.Context, action string) error {
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
	// Snapshot pointers for the control server's voice_state stamping. The
	// providers run inside ControlServer.broadcast — sometimes under sessMu
	// (emitReadyLocked) — so they must NOT take sessMu themselves. CallState /
	// AudioIO have their own locks, safe to touch here.
	var snapState atomic.Pointer[koe.CallState]
	var snapAudio atomic.Pointer[koe.AudioIO]
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
		// Sound the "ready" earcon once per call (async — the speaking gate mutes
		// the mic for its duration, so it can't self-trigger the server VAD; see
		// koe.PlayReadyEarcon). readyEmitted above already guarantees single-fire.
		if curAudio != nil && koe.ReadyEarconEnabled() {
			audio := curAudio
			go audio.PlayReadyEarcon()
		}
	}
	closeSessionLocked := func() (*koe.RealtimeConn, context.CancelFunc, *koe.AudioIO) {
		sessionSeq++
		conn, cancel := curConn, sessionCancel
		audio := curAudio
		curConn, curState, curAudio, sessionCancel = nil, nil, nil, nil
		snapState.Store(nil)
		snapAudio.Store(nil)
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
		audio.SetPreferredDevices(cfg.micDevice, cfg.speakerDevice)
		if _, err := applyAudioProcessing(audio, cfg, fullDuplexAEC); err != nil {
			failActiveCallLocked("audio processing config failed", err)
			scheduleWarmRetry("audio_processing_retry")
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
		snapState.Store(state)
		snapAudio.Store(audio)
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
				return koe.Connect(sessionCtx, audio, secret, *personaHolder.Load(), state, disp, koe.ConnectOptions{
					OnVoiceState:  onVoiceState,
					OnCallState:   onCallState,
					OnVoiceLevel:  onVoiceLevel,
					CallActive:    callActiveFn,
					Model:         cfg.model,
					Voice:         cfg.voice,
					OnUsage:       onUsage,
					OnEndCall:     func() { endCall() }, // end_call voice tool → hang up + goodbye earcon
					Language:      cfg.language,
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

	endCall = func() {
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
		// Stop/clear the old Realtime output before the goodbye cue. The audio device
		// stays open just long enough to play the cue, then is torn down below.
		if conn != nil {
			conn.InterruptOutput()
		}
		if cancel != nil {
			cancel()
		}
		if conn != nil {
			conn.Close()
		}
		if audio != nil && koe.DismissEarconEnabled() {
			audio.PlayDismissEarcon()
		}
		if audio != nil {
			audio.Stop()
		}
	}

	interruptCall := func() {
		sessMu.Lock()
		if !callActive || curConn == nil {
			sessMu.Unlock()
			return
		}
		conn := curConn
		sessMu.Unlock()
		conn.InterruptOutput()
	}

	ctrl = koe.NewControlServer(startCall, endCall, interruptCall)
	ctrl.SetToken(cfg.controlToken)
	ctrl.SetSnapshotProviders(
		func() bool { s := snapState.Load(); return s != nil && s.InFlight() != "" },
		func() bool { a := snapAudio.Load(); return a != nil && a.UserMicOff() },
	)
	ctrl.SetMicHandler(func(off bool) error {
		sessMu.Lock()
		audio := curAudio
		state := curState
		active := callActive
		sessMu.Unlock()
		if !active || audio == nil {
			return koe.ErrNoActiveCall
		}
		// Mute works in ANY active call (the Desktop trigger gesture mutes
		// instead of hanging up). A mute taken OUTSIDE a task window is
		// sticky: maybeRestoreUserMic's task-drain auto-restore must not
		// lift it — only the user does. Task-window mutes keep the original
		// koe-mic-off auto-restore. Any user restore clears the latch.
		if off {
			audio.SetUserMicSticky(state == nil || state.InFlight() == "")
		} else {
			audio.SetUserMicSticky(false)
		}
		audio.SetUserMicOff(off)
		ctrl.ReemitVoiceState()
		return nil
	})
	addr := "127.0.0.1:" + cfg.controlPort
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("control listen %s: %w", addr, err)
	}
	var controlClosing atomic.Bool
	go func() {
		// A dead control channel makes this process unreachable to Desktop, whose
		// respawn logic sees a live PID and never restarts it — so exit and let
		// Desktop's terminationHandler respawn koe cleanly on a fresh port.
		if err := http.Serve(ln, ctrl.Handler()); err != nil && !controlClosing.Load() {
			log.Fatalf("koe: control server on %s exited: %v", addr, err)
		}
	}()

	// Silent-input watchdog: in clamshell mode the OS default input stays the
	// built-in mic, which is covered and delivers pure silence — VPIO starts fine
	// and forwards ~0-RMS frames forever, so the call looks live but the model's
	// VAD never fires and Kocoro never responds. Sample the live input level and,
	// if it stays sub-floor while capture is expected, tell Desktop so it can warn
	// the user ("Kocoro can't hear you — check your microphone"). We do NOT restart
	// or rebind: in the reported case there is no other input device to switch to,
	// and a restart re-opens the same dead default (crash loop).
	go func() {
		floor := koe.MicSilenceFloor()
		window := koe.MicSilenceWindow()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		var watch koe.MicSilenceState
		var lastAudio *koe.AudioIO
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-ticker.C:
				audio := snapAudio.Load()
				if audio != lastAudio { // new call (or call ended) → fresh watch
					lastAudio = audio
					watch.Reset()
				}
				capturing := audio != nil && audio.VPIOActive() && audio.CaptureExpected()
				level := 0.0
				if audio != nil {
					level = audio.InputLevel()
				}
				switch watch.Observe(now, capturing, level, floor, window) {
				case koe.MicSilenceSilent:
					log.Printf("koe[mic]: no input above %.4f RMS for %s while capturing — mic likely unavailable (clamshell/covered/dead)", floor, window)
					ctrl.EmitMicStatus("silent")
				case koe.MicSilenceRecovered:
					log.Printf("koe[mic]: input recovered")
					ctrl.EmitMicStatus("ok")
				}
			}
		}
	}()

	// Fetch the agent registry + persona ONLY AFTER the listener above is bound, so
	// Desktop can already reach koe (409 no_active_call on /call/mic, or /call/start
	// kicking a warm session) during a slow-daemon window — ListAgents alone can block
	// up to 30s. Failure semantics are unchanged: a ListAgents error → empty registry,
	// FetchPersona → base persona. The values are published to the holders the warm
	// sessions read; a /call/start that races this window uses the base persona /
	// empty registry (graceful degradation), and the swapped-in full values apply from
	// the next warm session onward.
	agents, err := client.ListAgents(ctx)
	if err != nil {
		log.Printf("koe: list agents failed (continuing with empty registry): %v", err)
	}
	resolverHolder.Store(koe.NewAgentResolver(agents, koe.NoopSemanticMatcher{}))
	full := buildKoePersona(ctx, client, cfg, agents)
	personaHolder.Store(&full)

	sessMu.Lock()
	ensureWarmSessionLocked("startup")
	sessMu.Unlock()

	select {
	case <-ctx.Done():
	case <-desktopParentDone(ctx):
	}
	controlClosing.Store(true)
	_ = ln.Close()
	sessMu.Lock()
	conn, cancel, audio := closeSessionLocked()
	sessMu.Unlock()
	stopSessionResources(conn, cancel, audio)
	return nil
}
