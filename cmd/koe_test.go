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
	"sync/atomic"
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
	if cfg.model != "gpt-realtime-2.1" {
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

// The pinned default is only defensible because --model still overrides it —
// that is the escape hatch for CLI and on-robot callers who want another tier.
func TestKoeModelFlagOverridesDefault(t *testing.T) {
	flags := koeCmd.Flags()
	if err := flags.Set("model", "gpt-realtime-2.1-mini"); err != nil {
		t.Fatalf("set --model: %v", err)
	}
	t.Cleanup(func() { _ = flags.Set("model", "") })

	cfg := defaultKoeConfig()
	if v, _ := flags.GetString("model"); v != "" {
		cfg.model = v
	}
	if cfg.model != "gpt-realtime-2.1-mini" {
		t.Errorf("model after --model = %q, want the flag value to win", cfg.model)
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
	for _, want := range []string{"end_call", "退出吧", "stop_speaking", "keeps the voice call active", "double-tapping the", "NOT cancel"} {
		if !strings.Contains(koePersona, want) {
			t.Errorf("koePersona missing stop/end guidance %q", want)
		}
	}
	if strings.Contains(koePersona, `"stop", "shut up",`) && strings.Contains(koePersona, "that is a hang-up") {
		t.Error("koePersona still teaches stop-talking as a hang-up")
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

func TestKoePersonaUsesSingleKocoroVoice(t *testing.T) {
	if !strings.Contains(koePersona, koe.VoiceIdentityInstructions) {
		t.Fatal("Koe persona must include the shared single-Kocoro identity contract")
	}
	for _, banned := range []string{
		"Kocoro already knows",
		"Kocoro's full final user-facing reply",
		"Kocoro's earlier work",
	} {
		if strings.Contains(koePersona, banned) {
			t.Fatalf("Koe persona still frames Kocoro as a separate worker or result source: %q", banned)
		}
	}
	if !strings.Contains(koePersona, "Kocoro Desktop") {
		t.Fatal("single-Kocoro wording must preserve the Kocoro Desktop app name")
	}
}

func TestKoePersonaUsesRealtimeStructureAndVoiceStyle(t *testing.T) {
	for _, want := range []string{
		"# Role and Objective",
		"# Personality and Tone",
		"# Language",
		"# When to Speak",
		"# Tools and Work",
		"# Task Handoff",
		"# Results",
		"# Stop, Cancel, and End Call",
		"calm, warm, and capable coworker",
		"Direct answers: use one or two short sentences.",
		"Task results: use at most three short conversational sentences.",
		"Vary acknowledgements and opening phrases",
	} {
		if !strings.Contains(koePersona, want) {
			t.Errorf("koePersona missing structured voice contract %q", want)
		}
	}
	if got := len(strings.Fields(koePersona)); got > 800 {
		t.Errorf("koePersona has %d words, want no more than 800", got)
	}
}

func TestKoePersonaDefaultsToOneTaskAndMakesExplicitParallelScopesDisjoint(t *testing.T) {
	combined := koePersona + koeMultiTaskPersona + koe.ParallelTaskInstructions
	for _, want := range []string{
		"Default to exactly one do_task call.",
		"only when the user explicitly asks",
		"each call must contain exactly one disjoint work unit",
		"Never send the full compound request in one call while also sending any of its parts",
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("Koe parallel contract missing %q", want)
		}
	}
	if got := strings.Count(combined, koe.ParallelTaskInstructions); got != 1 {
		t.Fatalf("assembled Koe session has %d complete parallel contracts, want 1", got)
	}
	if strings.Contains(combined, "either send one complete compound task, or split it") {
		t.Fatal("Koe parallel contract still offers the ambiguous compound-plus-split choice")
	}
}

func TestKoePersonaConfirmationAndClarificationHaveSinglePassSemantics(t *testing.T) {
	for _, want := range []string{
		"only if the exact action is not already",
		"pass it through do_task without asking again",
		"several tasks are active",
		"follow-up or cancellation is ambiguous",
	} {
		if !strings.Contains(koePersona, want) {
			t.Errorf("koePersona missing bounded confirmation/clarification rule %q", want)
		}
	}
	for _, forbidden := range []string{
		"Before anything irreversible or outbound, restate it and wait for a clear yes",
		"This is the only follow-up question allowed before doing the work",
	} {
		if strings.Contains(koePersona, forbidden) {
			t.Errorf("koePersona retains conflicting absolute rule %q", forbidden)
		}
	}
}

func TestKoePersonaHasExactlyOneLanguageAuthority(t *testing.T) {
	var doTaskDescription string
	for _, tool := range koe.ToolDefs() {
		if tool.Name == "do_task" {
			doTaskDescription = tool.Description
			break
		}
	}
	if doTaskDescription == "" {
		t.Fatal("do_task tool definition missing")
	}
	for _, lang := range []string{"", "en", "ja", "zh"} {
		persona := baseKoePersona(koeConfig{language: lang})
		if got := strings.Count(persona, "# Language"); got != 1 {
			t.Errorf("language=%q has %d language sections, want 1", lang, got)
		}
		if lang != "" && strings.Contains(persona, "current utterance") {
			t.Errorf("language=%q retains the conflicting follow-utterance rule", lang)
		}
		assembled := strings.ToLower(persona + "\n" + doTaskDescription)
		if lang != "" && (strings.Contains(assembled, "user's current language") ||
			strings.Contains(assembled, "language of the utterance")) {
			t.Errorf("language=%q assembled session retains an utterance-language override", lang)
		}
		if !strings.Contains(assembled, "active reply language") {
			t.Errorf("language=%q assembled session lacks the shared active-language handoff", lang)
		}
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

func TestLazyMintDoesNotPrefetch(t *testing.T) {
	var calls atomic.Int32
	w := newLazyMint(func(context.Context) (string, error) {
		calls.Add(1)
		return "ek_lazy", nil
	}, time.Minute)
	if got := calls.Load(); got != 0 {
		t.Fatalf("lazy constructor minted %d time(s), want 0", got)
	}
	secret, cached, err := w.take(context.Background())
	if err != nil || cached || secret != "ek_lazy" || calls.Load() != 1 {
		t.Fatalf("take = %q cached=%t calls=%d err=%v", secret, cached, calls.Load(), err)
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
	if !strings.Contains(zh, koeLanguageInstruction("zh")) ||
		strings.Contains(zh, koeDefaultLanguageSection) || strings.Count(zh, "# Language") != 1 {
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
	for _, want := range []string{"# Role and Objective", "USER_CONTEXT_MARKER", koeAgentListLine(agents), koeLanguageInstruction("en"), koeMultiTaskPersona} {
		if !strings.Contains(got, want) {
			t.Errorf("buildKoePersona missing %q", want)
		}
	}
	if strings.Contains(got, koeDefaultLanguageSection) || strings.Count(got, "# Language") != 1 {
		t.Errorf("buildKoePersona has conflicting language authorities: %q", got)
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
		mint := func(context.Context) (string, error) { return "", fmt.Errorf("no mint in test") }
		done <- runDesktopCall(ctx, koeConfig{controlPort: port, daemonURL: daemon.URL, model: "gpt-realtime-mini"},
			koe.NewDaemonClient(daemon.URL),
			&realtimeConnector{
				mode: koe.ProviderOpenAI, openAIModel: "gpt-realtime-mini", mint: mint,
				circuit: koe.NewOpenAICircuit(time.Minute),
			},
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

func TestRunDesktopCallWirelessDoesNotMintWhileIdle(t *testing.T) {
	t.Setenv("KOE_GAZE_GATE", "false")
	previousProbe := probeAudioCarrier
	probeAudioCarrier = func(path string) error {
		if path != "/tmp/test-wireless-audio.sock" {
			return fmt.Errorf("unexpected audio probe path %q", path)
		}
		return nil
	}
	t.Cleanup(func() { probeAudioCarrier = previousProbe })
	daemon := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/agents":
			_ = json.NewEncoder(w).Encode(map[string]any{"agents": []any{}})
		case "/koe/persona":
			_ = json.NewEncoder(w).Encode(map[string]any{"persona": ""})
		default:
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
		}
	}))
	defer daemon.Close()

	var mintCalls atomic.Int32
	port := freeTCPPort(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		done <- runDesktopCall(ctx, koeConfig{
			controlPort: port,
			daemonURL:   daemon.URL,
			audioSocket: "/tmp/test-wireless-audio.sock",
			model:       "gpt-realtime-mini",
			carrier: koe.CarrierProfile{
				Carrier:         koe.CarrierReachyWireless,
				ReachyDaemonURL: daemon.URL,
			},
		}, koe.NewDaemonClient(daemon.URL), &realtimeConnector{
			mode:        koe.ProviderAuto,
			openAIModel: "gpt-realtime-mini",
			qwenModel:   koe.DefaultQwenRealtimeModel,
			mint: func(context.Context) (string, error) {
				mintCalls.Add(1)
				return "ek_unexpected", nil
			},
			circuit: koe.NewOpenAICircuit(time.Minute),
		}, func(json.RawMessage) {})
	}()

	base := "http://127.0.0.1:" + port
	deadline := time.Now().Add(3 * time.Second)
	var status map[string]any
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/carrier/status")
		if err == nil {
			_ = json.NewDecoder(resp.Body).Decode(&status)
			resp.Body.Close()
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if status["call_state"] != "idle" || status["realtime_state"] != "disconnected" {
		t.Fatalf("wireless idle status = call %v realtime %v", status["call_state"], status["realtime_state"])
	}
	audioStatus, _ := status["audio"].(map[string]any)
	if audioStatus["state"] != "connected" {
		t.Fatalf("wireless audio status = %v", audioStatus)
	}
	time.Sleep(100 * time.Millisecond)
	if got := mintCalls.Load(); got != 0 {
		t.Fatalf("wireless idle minted %d time(s), want 0", got)
	}
	cancel()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("runDesktopCall: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("runDesktopCall did not stop")
	}
}

func TestResidentOwnerDoneWirelessIgnoresLauncherParent(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	done := residentOwnerDone(ctx, true)
	select {
	case <-done:
		t.Fatal("wireless owner channel closed before its context")
	default:
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("wireless owner channel did not follow context cancellation")
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

func TestCloseDesktopSessionStatePreservesActiveCallResults(t *testing.T) {
	mailbox := koe.NewResultMailbox()
	state := koe.NewCallState("active-call", "")
	mailbox.BeginBurst(state.BurstID())
	ticket := mailbox.BeginTaskResult(state.BurstID(), "response-1", "call-1")

	if got := closeDesktopSessionState(mailbox, state, false); got != state {
		t.Fatal("provider reconnect replaced the logical call state")
	}
	if id := mailbox.EnqueueTaskResult(ticket, koe.SayResult{Status: "ok", Reply: "complete result"}, false); id == 0 {
		t.Fatal("provider reconnect retired the active call result burst")
	}

	endedMailbox := koe.NewResultMailbox()
	endedState := koe.NewCallState("ended-call", "")
	endedMailbox.BeginBurst(endedState.BurstID())
	endedTicket := endedMailbox.BeginTaskResult(endedState.BurstID(), "response-2", "call-2")
	if got := closeDesktopSessionState(endedMailbox, endedState, true); got != nil {
		t.Fatal("explicit call end kept the logical call state")
	}
	if id := endedMailbox.EnqueueTaskResult(endedTicket, koe.SayResult{Status: "ok", Reply: "late result"}, false); id != 0 {
		t.Fatal("explicit call end kept accepting late voice results")
	}
}

func TestDesktopSessionPersonaDisclosesActiveReconnectContextBoundary(t *testing.T) {
	const base = "base voice persona"
	if got := desktopSessionPersona(base, "startup"); got != base {
		t.Fatalf("startup persona changed: %q", got)
	}
	recovered := desktopSessionPersona(base, "active_session_reconnect")
	for _, want := range []string{base, "does not contain the earlier voice conversation", "ask the user to restate", "task-result data"} {
		if !strings.Contains(recovered, want) {
			t.Fatalf("active reconnect persona missing %q: %s", want, recovered)
		}
	}
}

// TestApplyBargeInEnv locks the flag→env bridge: native floor is on while remote
// irreversible interruption is off.
func TestApplyBargeInEnv(t *testing.T) {
	t.Setenv("KOE_VPIO_BARGE_IN", "1")
	t.Setenv("KOE_NATIVE_FLOOR", "1")
	t.Setenv("KOE_INTERRUPT_RESPONSE", "1")

	applyBargeInEnv(false, false)
	if v := os.Getenv("KOE_VPIO_BARGE_IN"); v != "1" {
		t.Fatalf("implicit barge-in setting changed KOE_VPIO_BARGE_IN=%q", v)
	}
	if v := os.Getenv("KOE_CLIENT_RESPONSE"); v != "" {
		t.Fatalf("barge-in off set KOE_CLIENT_RESPONSE=%q, want unchanged", v)
	}

	applyBargeInEnv(false, true)
	if v := os.Getenv("KOE_VPIO_BARGE_IN"); v != "0" {
		t.Fatalf("explicit barge-in off left KOE_VPIO_BARGE_IN=%q, want 0", v)
	}
	if v := os.Getenv("KOE_INTERRUPT_RESPONSE"); v != "0" {
		t.Fatalf("explicit barge-in off left KOE_INTERRUPT_RESPONSE=%q, want 0", v)
	}
	if v := os.Getenv("KOE_NATIVE_FLOOR"); v != "0" {
		t.Fatalf("explicit barge-in off left KOE_NATIVE_FLOOR=%q, want 0", v)
	}

	applyBargeInEnv(true, true)
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

func TestFullDuplexAECForConfig(t *testing.T) {
	wireless, err := koe.ParseCarrierProfile(koe.CarrierInputs{Carrier: koe.CarrierReachyWireless})
	if err != nil {
		t.Fatal(err)
	}
	if !fullDuplexAECForConfig(koeConfig{carrier: wireless}) {
		t.Fatal("Wireless full_duplex carrier must use its XVF3800 AEC stream")
	}
	if !fullDuplexAECForConfig(koeConfig{aec: "vpio"}) {
		t.Fatal("VPIO must remain an AEC-capable full-duplex path")
	}
	if fullDuplexAECForConfig(koeConfig{}) {
		t.Fatal("plain Mac gate backend must remain half-duplex")
	}
}

// TestBargeInBackendWarning: barge-in works on either VPIO or an AEC-capable
// carrier; other paths must warn instead of silently no-op'ing.
func TestBargeInBackendWarning(t *testing.T) {
	if got := bargeInBackendWarning(true, true); got != "" {
		t.Errorf("barge-in on a full-duplex AEC path should not warn, got %q", got)
	}
	if got := bargeInBackendWarning(false, false); got != "" {
		t.Errorf("barge-in off should not warn, got %q", got)
	}
	if bargeInBackendWarning(true, false) == "" {
		t.Error("barge-in on a half-duplex path must warn")
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
