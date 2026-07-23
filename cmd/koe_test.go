//go:build darwin && cgo

package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/koe"
)

func TestKoeAgentListLine(t *testing.T) {
	got := koeAgentListLine([]koe.AgentSummary{
		{Slug: "investment-analyst"},
		{Slug: "finance", DisplayName: "金融分析"},
	})
	for _, want := range []string{"investment-analyst", "finance", "金融分析", "which agents exist"} {
		if !strings.Contains(got, want) {
			t.Errorf("koeAgentListLine missing %q; got: %s", want, got)
		}
	}
	if koeAgentListLine(nil) != "" {
		t.Error("empty agents should yield empty line")
	}
}

func TestKoeCmdRegistered(t *testing.T) {
	var found bool
	for _, c := range rootCmd.Commands() {
		if c.Name() == "koe" {
			found = true
		}
	}
	if !found {
		t.Error("koe subcommand not registered on rootCmd")
	}
}

func TestKoeConfigDefaults(t *testing.T) {
	cfg := defaultKoeConfig()
	if cfg.model != "gpt-realtime-2.1-mini" {
		t.Errorf("default model = %q", cfg.model)
	}
	if cfg.daemonURL == "" {
		t.Error("daemonURL default must be non-empty")
	}
	if cfg.aec != "" {
		t.Errorf("default aec = %q, want empty gate fallback", cfg.aec)
	}
	if cfg.audioProcessing != audioProcessingAuto {
		t.Errorf("default audioProcessing = %q, want auto", cfg.audioProcessing)
	}
}

func TestKoeCmdHasAudioFlags(t *testing.T) {
	if koeCmd.Flags().Lookup("aec") == nil {
		t.Fatal("koe command must expose --aec for VPIO opt-in testing")
	}
	if koeCmd.Flags().Lookup("audio-processing") == nil {
		t.Fatal("koe command must expose --audio-processing for device voice processing control")
	}
}

func TestResolveAudioProcessingMode(t *testing.T) {
	tests := []struct {
		name        string
		raw         string
		mic         string
		speaker     string
		want        string
		wantBypass  bool
		wantReason  string
		wantErrPart string
	}{
		{
			name:       "auto unknown defaults to mac voice",
			raw:        "",
			mic:        "BuiltInMicrophoneDevice",
			speaker:    "BuiltInSpeakerDevice",
			want:       audioProcessingMacVoice,
			wantBypass: false,
			wantReason: "default_mac_voice",
		},
		{
			name:       "auto reachy uses clean device input",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Pollen Robotics:Reachy Mini Audio XVF3800:1,2",
			speaker:    "Reachy Mini Audio XVF3800",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device",
		},
		{
			name:       "auto conference speakerphone uses clean device input",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Jabra:SPEAK 750:1,2",
			speaker:    "AppleUSBAudioEngine:Jabra:SPEAK 750:1,2",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device:jabra speak",
		},
		{
			name:       "auto poly sync uses clean device input",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Poly:Sync 20:1,2",
			speaker:    "AppleUSBAudioEngine:Poly:Sync 20:1,2",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device:poly sync",
		},
		{
			name:       "auto yealink conference phone uses clean device input",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Yealink:CP900:1,2",
			speaker:    "AppleUSBAudioEngine:Yealink:CP900:1,2",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device:yealink cp",
		},
		{
			name:       "auto logitech room device uses clean device input",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Logitech:Rally Bar:1,2",
			speaker:    "AppleUSBAudioEngine:Logitech:Rally Bar:1,2",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device:logitech rally",
		},
		{
			name:       "auto shure stem uses clean device input",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Shure:STEM TABLE:1,2",
			speaker:    "AppleUSBAudioEngine:Shure:STEM TABLE:1,2",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device:shure stem",
		},
		{
			name:       "auto epos expand uses clean device input",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:EPOS:EXPAND 30:1,2",
			speaker:    "AppleUSBAudioEngine:EPOS:EXPAND 30:1,2",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device:epos expand",
		},
		{
			name:       "auto yamaha yvc uses clean device input",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Yamaha Corporation:YVC-200:1,2",
			speaker:    "AppleUSBAudioEngine:Yamaha Corporation:YVC-200:1,2",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device:yamaha yvc",
		},
		{
			name:       "auto konftel uses clean device input",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Konftel:Ego:1,2",
			speaker:    "AppleUSBAudioEngine:Konftel:Ego:1,2",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device:konftel",
		},
		{
			name:       "auto krisp requires routed speaker",
			raw:        audioProcessingAuto,
			mic:        "Krisp Microphone",
			speaker:    "Krisp Speaker",
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "known_self_processed_device:krisp",
		},
		{
			name:       "auto ignores self processed speaker without matching mic",
			raw:        audioProcessingAuto,
			mic:        "BuiltInMicrophoneDevice",
			speaker:    "Reachy Mini Audio XVF3800",
			want:       audioProcessingMacVoice,
			wantBypass: false,
			wantReason: "default_mac_voice",
		},
		{
			name:       "auto conference speakerphone mic alone keeps mac voice processing",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Jabra:SPEAK 750:1,2",
			speaker:    "BuiltInSpeakerDevice",
			want:       audioProcessingMacVoice,
			wantBypass: false,
			wantReason: "default_mac_voice",
		},
		{
			name:       "auto does not trust generic brand headset",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Jabra:Evolve2 65:1,2",
			speaker:    "BuiltInSpeakerDevice",
			want:       audioProcessingMacVoice,
			wantBypass: false,
			wantReason: "default_mac_voice",
		},
		{
			name:       "auto does not trust broad yealink brand",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:Yealink:USB Headset:1,2",
			speaker:    "BuiltInSpeakerDevice",
			want:       audioProcessingMacVoice,
			wantBypass: false,
			wantReason: "default_mac_voice",
		},
		{
			name:       "auto krisp mic alone keeps mac voice processing",
			raw:        audioProcessingAuto,
			mic:        "Krisp Microphone",
			speaker:    "BuiltInSpeakerDevice",
			want:       audioProcessingMacVoice,
			wantBypass: false,
			wantReason: "default_mac_voice",
		},
		{
			name:       "auto does not trust camera noise suppression devices",
			raw:        audioProcessingAuto,
			mic:        "AppleUSBAudioEngine:OBSBOT:Tiny 2:1,2",
			speaker:    "BuiltInSpeakerDevice",
			want:       audioProcessingMacVoice,
			wantBypass: false,
			wantReason: "default_mac_voice",
		},
		{
			name:       "auto does not trust nvidia broadcast mic alone",
			raw:        audioProcessingAuto,
			mic:        "NVIDIA Broadcast",
			speaker:    "BuiltInSpeakerDevice",
			want:       audioProcessingMacVoice,
			wantBypass: false,
			wantReason: "default_mac_voice",
		},
		{
			name:       "explicit mac voice",
			raw:        audioProcessingMacVoice,
			want:       audioProcessingMacVoice,
			wantBypass: false,
			wantReason: "explicit_mac_voice",
		},
		{
			name:       "explicit clean device",
			raw:        audioProcessingCleanDevice,
			want:       audioProcessingCleanDevice,
			wantBypass: true,
			wantReason: "explicit_clean_device",
		},
		{
			name:        "invalid",
			raw:         "raw",
			wantErrPart: "invalid --audio-processing",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := resolveAudioProcessingMode(tt.raw, tt.mic, tt.speaker)
			if tt.wantErrPart != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErrPart) {
					t.Fatalf("resolve error = %v, want containing %q", err, tt.wantErrPart)
				}
				return
			}
			if err != nil {
				t.Fatalf("resolve: %v", err)
			}
			if got.Resolved != tt.want || got.Bypass != tt.wantBypass {
				t.Fatalf("decision = %+v, want resolved=%s bypass=%v", got, tt.want, tt.wantBypass)
			}
			if !strings.Contains(got.Reason, tt.wantReason) {
				t.Fatalf("reason = %q, want containing %q", got.Reason, tt.wantReason)
			}
		})
	}
}

func TestKoePersonaPinsCurrentUtteranceLanguage(t *testing.T) {
	// The current-utterance language rule is load-bearing (voice turns must follow
	// the just-spoken language, not the Desktop global preference — see
	// daemon.applyKoeResponseLanguage). The concrete English/Chinese examples were
	// dropped from the persona; the rule itself stays.
	for _, want := range []string{"current utterance", "not the user's usual"} {
		if !strings.Contains(koePersona, want) {
			t.Fatalf("koePersona missing language discipline %q", want)
		}
	}
}

// TestKoePersonaTeachesEndCallOnDismiss keeps the dismiss/hang-up guidance in the
// system prompt: gpt-realtime-mini does not call end_call without it (live 2026-07-08
// it verbally acknowledged "闭嘴" instead). It must name end_call, give concrete
// dismiss words, the double-tap re-activation, and separate it from cancel.
func TestKoePersonaTeachesEndCallOnDismiss(t *testing.T) {
	for _, want := range []string{"end_call", "闭嘴", "double-tapping the", "NOT cancel"} {
		if !strings.Contains(koePersona, want) {
			t.Errorf("koePersona missing dismiss/end_call guidance %q", want)
		}
	}
	// The old line that made the model verbally "stop" instead of hanging up is gone.
	if strings.Contains(koePersona, "if they tell you to stop, stop") {
		t.Error("koePersona still has the verbal-stop line that suppressed end_call")
	}
}

func TestKoePersonaSummaryAndDesktopDiscipline(t *testing.T) {
	// The do_task result carries the complete final reply: recaps/follow-ups it
	// covers are answered directly, while new action/freshness goes through
	// do_task. Kocoro Desktop is mentioned only for genuinely rich deliverables.
	for _, want := range []string{"full final user-facing reply", "never call do_task to re-fetch", "Mention Kocoro Desktop only when"} {
		if !strings.Contains(koePersona, want) {
			t.Errorf("koePersona missing result-handling guidance %q", want)
		}
	}
	// Kocoro Desktop is a proper noun, distinct from the desktop folder.
	if !strings.Contains(koePersona, "not the computer's desktop folder") {
		t.Error("koePersona should distinguish Kocoro Desktop from the computer's desktop folder")
	}
}

// TestKoePersonaUsesRealisticDirectAnswerExamples: nobody asks a voice assistant
// "1+1" — the direct-answer examples must be things users actually do. Since the
// 2026-07-10 tool-gating rework these span both stable public knowledge the model
// already holds (how reinforcement learning works, Newton's laws) and recapping
// what was already said in the call — not just in-call content.
func TestKoePersonaUsesRealisticDirectAnswerExamples(t *testing.T) {
	if strings.Contains(koePersona, "1+1") {
		t.Fatal("koePersona should not use toy arithmetic as the direct-answer example")
	}
	for _, want := range []string{"how reinforcement learning works", "recapping anything already said"} {
		if !strings.Contains(koePersona, want) {
			t.Fatalf("koePersona direct-answer examples missing %q", want)
		}
	}
}

// TestKoePersonaDropsOneSelfLecture: the "You are one self" identity lecture was
// removed on user decision (2026-07-02) — occasional first-person narration like
// "我去查一下" is acceptable; the paragraph was not earning its tokens. The
// Kocoro Desktop proper-noun rule it shared a paragraph with must survive.
func TestKoePersonaDropsOneSelfLecture(t *testing.T) {
	if strings.Contains(koePersona, "You are one self") {
		t.Fatal("the one-self lecture should be gone from koePersona")
	}
	if !strings.Contains(koePersona, "not the computer's desktop folder") {
		t.Fatal("dropping the one-self lecture must keep the Kocoro Desktop naming rule")
	}
}

// TestKoePersonaForbidsDetailQuizzing pins the anti-interrogation rule (live
// 2026-07-02: Koe kept asking for the user's own email address across several
// calls instead of delegating): the ONLY allowed follow-up question is a repeat
// for unclear audio; vague or incomplete requests go to do_task as spoken.
func TestKoePersonaForbidsDetailQuizzing(t *testing.T) {
	for _, want := range []string{"never quiz the user", "could not clearly hear", "call do_task with it as spoken"} {
		if !strings.Contains(koePersona, want) {
			t.Fatalf("koePersona missing anti-quizzing rule %q", want)
		}
	}
}

// TestKoePersonaAckIsBareAndNoPreAnswer pins the voice-latency contract: one
// minimal acknowledgement, no narrated process or wait promise, and no guessed
// answer before the real task result lands. Direct answers get no stray ack.
func TestKoePersonaAckIsBareAndNoPreAnswer(t *testing.T) {
	for _, want := range []string{"use at most", "one bare clause", "narrate steps", "ask the user to wait", "before it lands", "only when you are actually about to call do_task"} {
		if !strings.Contains(koePersona, want) {
			t.Fatalf("koePersona missing ack contract %q", want)
		}
	}
	if strings.Contains(koePersona, `say exactly "我来处理"`) {
		t.Fatal("koePersona must not mandate a single fixed acknowledgement phrase anymore")
	}
}

func TestKoePersonaSeparatesCurrentHandoffFromLaterTurns(t *testing.T) {
	combined := strings.ToLower(koePersona + koeMultiTaskPersona)
	for _, want := range []string{
		"after the do_task call, emit no more audio in this response",
		"later user turns may continue normally while the task is running",
		"never narrate the delivery mechanics",
	} {
		if !strings.Contains(combined, want) {
			t.Fatalf("Koe handoff contract missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(koePersona), "say nothing more until\nthe result lands") {
		t.Fatal("Koe persona still conflates the current handoff response with later conversation turns")
	}
}

// TestKoePersonaDividesByInformationSource pins the split: the line is the NATURE
// OF THE INFORMATION the answer needs — stable public knowledge the model holds vs
// current/private/action — not task difficulty, and not the model's own sense of
// what it knows. The 2026-07-10 rework dropped the "your memory is unreliable"
// scare that pushed even settled knowledge (RL, Newton's laws) through do_task.
func TestKoePersonaDividesByInformationSource(t *testing.T) {
	for _, want := range []string{"one obvious step", "stable and public, versus current"} {
		if !strings.Contains(koePersona, want) {
			t.Fatalf("koePersona missing information-source split %q", want)
		}
	}
	for _, banned := range []string{"any number or calculation", "memory of the world", "calling the tool IS the answer"} {
		if strings.Contains(koePersona, banned) {
			t.Fatalf("koePersona must not keep removed scare/ban %q", banned)
		}
	}
}

// TestKoePersonaGuardsMidTaskCancel: background speech overheard mid-task must
// not kill the run — cancel only on a clear, explicit stop request; unsure →
// confirm briefly first (live 2026-07-02: noise cancelled a 53s report task).
func TestKoePersonaGuardsMidTaskCancel(t *testing.T) {
	for _, want := range []string{"clear, explicit request to stop", "ask briefly first"} {
		if !strings.Contains(koePersona, want) {
			t.Fatalf("koePersona missing mid-task cancel guard %q", want)
		}
	}
}

func TestKoePersonaTreatsLongCompoundRequestsAsActionable(t *testing.T) {
	for _, want := range []string{"Long or multi-part user utterances", "not wait for \"do it\""} {
		if !strings.Contains(koePersona, want) {
			t.Errorf("koePersona missing long-request execution guidance %q", want)
		}
	}
}

func TestKoeAudioStartTimeoutDefaultAndOverride(t *testing.T) {
	t.Setenv("KOE_AUDIO_START_TIMEOUT_MS", "")
	if got := koeAudioStartTimeout(); got != audioStartTimeout {
		t.Fatalf("default audio timeout = %s, want %s", got, audioStartTimeout)
	}

	t.Setenv("KOE_AUDIO_START_TIMEOUT_MS", "250")
	if got := koeAudioStartTimeout(); got != 250*time.Millisecond {
		t.Fatalf("override audio timeout = %s, want 250ms", got)
	}

	t.Setenv("KOE_AUDIO_START_TIMEOUT_MS", "bad")
	if got := koeAudioStartTimeout(); got != audioStartTimeout {
		t.Fatalf("invalid audio timeout = %s, want default %s", got, audioStartTimeout)
	}
}

func TestOnceVoiceStateHandlerWaitsForAssistantOutput(t *testing.T) {
	canceled := make(chan struct{}, 1)
	handler := onceVoiceStateHandler(func() { canceled <- struct{}{} }, 10*time.Millisecond)

	handler("listening")
	select {
	case <-canceled:
		t.Fatal("initial user-speech listening state must not cancel --once")
	case <-time.After(30 * time.Millisecond):
	}

	handler("speaking")
	handler("listening")
	select {
	case <-canceled:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("post-assistant listening state should cancel --once")
	}
}

func TestKoeWarmSessionTTLDefaultAndOverride(t *testing.T) {
	t.Setenv("KOE_WARM_SESSION_TTL_MS", "")
	if got := koeWarmSessionTTL(); got != warmSessionTTL {
		t.Fatalf("default warm session ttl = %s, want %s", got, warmSessionTTL)
	}

	t.Setenv("KOE_WARM_SESSION_TTL_MS", "500")
	if got := koeWarmSessionTTL(); got != 500*time.Millisecond {
		t.Fatalf("override warm session ttl = %s, want 500ms", got)
	}

	t.Setenv("KOE_WARM_SESSION_TTL_MS", "-1")
	if got := koeWarmSessionTTL(); got != warmSessionTTL {
		t.Fatalf("invalid warm session ttl = %s, want default %s", got, warmSessionTTL)
	}
}

func TestWarmMintTakeUsesCachedSecret(t *testing.T) {
	w := &warmMint{
		mint: func(context.Context) (string, error) {
			t.Fatal("cached warm mint should not call mint")
			return "", nil
		},
		ttl:      time.Minute,
		value:    "ek_cached",
		mintedAt: time.Now(),
		inFlight: true, // suppress async refill; this test only covers cache consumption
	}
	got, cached, err := w.take(context.Background())
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if got != "ek_cached" || !cached {
		t.Fatalf("take = %q cached=%v, want cached secret", got, cached)
	}
	if w.value != "" {
		t.Fatal("cached secret should be consumed exactly once")
	}
}

func TestWarmMintTakeMintsWhenExpired(t *testing.T) {
	var calls int
	w := &warmMint{
		mint: func(context.Context) (string, error) {
			calls++
			return "ek_fresh", nil
		},
		ttl:      time.Millisecond,
		value:    "ek_old",
		mintedAt: time.Now().Add(-time.Second),
	}
	got, cached, err := w.take(context.Background())
	if err != nil {
		t.Fatalf("take: %v", err)
	}
	if got != "ek_fresh" || cached || calls != 1 {
		t.Fatalf("take = %q cached=%v calls=%d, want fresh mint", got, cached, calls)
	}
}

// TestBaseKoePersona: the pre-fetch warm-session persona is the base persona plus
// (only) the pinned language — no user context / agent list yet.
func TestBaseKoePersona(t *testing.T) {
	t.Setenv("KOE_TASK_LEDGER", "0")
	if got := baseKoePersona(koeConfig{language: ""}); got != koePersona {
		t.Errorf("empty language should give the bare base persona")
	}
	zh := baseKoePersona(koeConfig{language: "zh"})
	if !strings.HasPrefix(zh, koePersona) || !strings.Contains(zh, koeLanguageInstruction("zh")) {
		t.Errorf("zh base persona missing base or language pin: %q", zh)
	}
	t.Setenv("KOE_TASK_LEDGER", "1")
	if got := baseKoePersona(koeConfig{}); !strings.Contains(got, koeMultiTaskPersona) {
		t.Error("ledger persona must teach immediate ack and multi-task addressing")
	}
}

// TestBuildKoePersonaAssembly: the full persona folds in the daemon-distilled user
// context, the agent list, and the pinned language.
func TestBuildKoePersonaAssembly(t *testing.T) {
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/koe/persona" {
			_ = json.NewEncoder(w).Encode(map[string]any{"persona": "USER_CONTEXT_MARKER"})
			return
		}
		http.NotFound(w, r)
	}))
	defer daemon.Close()

	agents := []koe.AgentSummary{{Slug: "finance", DisplayName: "Finance"}}
	got := buildKoePersona(context.Background(), koe.NewDaemonClient(daemon.URL), koeConfig{language: "en"}, agents)
	for _, want := range []string{koePersona, "USER_CONTEXT_MARKER", koeAgentListLine(agents), koeLanguageInstruction("en"), koeMultiTaskPersona} {
		if !strings.Contains(got, want) {
			t.Errorf("buildKoePersona missing %q", want)
		}
	}
}

// TestRunDesktopCallBindsControlPortBeforeSlowAgentFetch verifies S9: the control
// listener must answer while the (slow) agent-registry fetch is still blocked, so
// Desktop is not locked out during a slow-daemon window. The mock daemon holds
// GET /agents open; the test asserts POST /call/mic already returns 409 no_active_call.
func TestRunDesktopCallBindsControlPortBeforeSlowAgentFetch(t *testing.T) {
	agentsHit := make(chan struct{})
	releaseAgents := make(chan struct{})
	var agentsOnce sync.Once
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents":
			agentsOnce.Do(func() { close(agentsHit) })
			select {
			case <-releaseAgents:
			case <-time.After(10 * time.Second): // safety net
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []any{}})
		case "/koe/persona":
			_ = json.NewEncoder(w).Encode(map[string]any{"persona": ""})
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer daemon.Close()

	port := freeTCPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() {
		done <- runDesktopCall(ctx, koeConfig{controlPort: port, daemonURL: daemon.URL, model: "gpt-realtime-mini"},
			koe.NewDaemonClient(daemon.URL),
			func(context.Context) (string, error) { return "", fmt.Errorf("no mint in test") },
			func(json.RawMessage) {})
	}()

	select {
	case <-agentsHit:
	case <-time.After(5 * time.Second):
		t.Fatal("ListAgents was never called — setup blocked before the fetch")
	}

	// /agents is now blocked. The control port must already respond.
	base := "http://127.0.0.1:" + port
	var resp *http.Response
	var err error
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, base+"/call/mic", strings.NewReader(`{"mic":"off"}`))
		if resp, err = http.DefaultClient.Do(req); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err != nil {
		t.Fatalf("control port unreachable while the agent fetch is blocked: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusConflict || !strings.Contains(string(body), "no_active_call") {
		t.Fatalf("/call/mic during pre-warm = %d %s, want 409 no_active_call", resp.StatusCode, body)
	}

	close(releaseAgents)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("runDesktopCall did not return after ctx cancel")
	}
}

// freeTCPPort grabs an ephemeral localhost port and releases it for the caller to
// bind. A tiny TOCTOU window is acceptable in a test.
func freeTCPPort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freeTCPPort: %v", err)
	}
	defer l.Close()
	_, port, _ := net.SplitHostPort(l.Addr().String())
	return port
}

func TestResolveDevKey(t *testing.T) {
	tests := []struct {
		name        string
		flagKey     string
		envKey      string
		controlPort string
		want        string
	}{
		{"standalone env fallback", "", "sk-env", "", "sk-env"},
		{"desktop mode ignores env", "", "sk-env", "17654", ""},
		{"explicit flag wins in desktop mode", "sk-flag", "sk-env", "17654", "sk-flag"},
		{"explicit flag wins standalone", "sk-flag", "sk-env", "", "sk-flag"},
		{"nothing set", "", "", "17654", ""},
	}
	for _, tt := range tests {
		if got := resolveDevKey(tt.flagKey, tt.envKey, tt.controlPort); got != tt.want {
			t.Errorf("%s: resolveDevKey(%q, %q, %q) = %q, want %q",
				tt.name, tt.flagKey, tt.envKey, tt.controlPort, got, tt.want)
		}
	}
}

func TestKoeCmdHasBargeInFlag(t *testing.T) {
	f := koeCmd.Flags().Lookup("barge-in")
	if f == nil {
		t.Fatal("koe command missing --barge-in flag")
	}
	if f.DefValue != "false" {
		t.Fatalf("--barge-in default = %q, want false", f.DefValue)
	}
}

// TestApplyBargeInEnv locks the flag→env bridge: native floor is on while remote
// irreversible interruption is off.
func TestApplyBargeInEnv(t *testing.T) {
	t.Setenv("KOE_VPIO_BARGE_IN", "")
	t.Setenv("KOE_NATIVE_FLOOR", "")
	t.Setenv("KOE_INTERRUPT_RESPONSE", "")

	applyBargeInEnv(false)
	if v := os.Getenv("KOE_VPIO_BARGE_IN"); v != "" {
		t.Fatalf("barge-in off set KOE_VPIO_BARGE_IN=%q, want unchanged", v)
	}
	if v := os.Getenv("KOE_INTERRUPT_RESPONSE"); v != "" {
		t.Fatalf("barge-in off set KOE_INTERRUPT_RESPONSE=%q, want unchanged", v)
	}
	if v := os.Getenv("KOE_NATIVE_FLOOR"); v != "" {
		t.Fatalf("barge-in off set KOE_NATIVE_FLOOR=%q, want unchanged", v)
	}

	applyBargeInEnv(true)
	if v := os.Getenv("KOE_VPIO_BARGE_IN"); v != "1" {
		t.Fatalf("KOE_VPIO_BARGE_IN=%q, want 1", v)
	}
	if v := os.Getenv("KOE_NATIVE_FLOOR"); v != "1" {
		t.Fatalf("KOE_NATIVE_FLOOR=%q, want 1", v)
	}
	if v := os.Getenv("KOE_INTERRUPT_RESPONSE"); v != "0" {
		t.Fatalf("KOE_INTERRUPT_RESPONSE=%q, want 0", v)
	}
}

// TestBargeInBackendWarning: --barge-in only works on the VPIO backend, so enabling
// it on the gate/oto backend must surface a warning instead of silently no-op'ing.
func TestBargeInBackendWarning(t *testing.T) {
	if got := bargeInBackendWarning(true, "vpio"); got != "" {
		t.Errorf("barge-in on vpio should not warn, got %q", got)
	}
	if got := bargeInBackendWarning(false, "gate"); got != "" {
		t.Errorf("barge-in off should not warn, got %q", got)
	}
	if got := bargeInBackendWarning(false, ""); got != "" {
		t.Errorf("barge-in off should not warn, got %q", got)
	}
	if bargeInBackendWarning(true, "gate") == "" {
		t.Error("barge-in on the gate backend must warn (it is a silent no-op otherwise)")
	}
	if bargeInBackendWarning(true, "") == "" {
		t.Error("barge-in with an empty backend (defaults to gate) must warn")
	}
}

// TestKoePersonaAllowsUserNameFromInstructions guards the Q2 fix: the
// anti-hallucination clause must carry an explicit exemption so the model can speak
// the persona-injected user name instead of conservatively suppressing it.
func TestKoePersonaAllowsUserNameFromInstructions(t *testing.T) {
	if !strings.Contains(koePersona, "established facts") {
		t.Fatal("koePersona missing the user-name/personal-context exemption to the anti-hallucination rule")
	}
}
