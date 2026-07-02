package cmd

import (
	"context"
	"strings"
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
	if cfg.model != "gpt-realtime-mini-2025-12-15" {
		t.Errorf("default model = %q", cfg.model)
	}
	if cfg.daemonURL == "" {
		t.Error("daemonURL default must be non-empty")
	}
	if cfg.aec != "" {
		t.Errorf("default aec = %q, want empty gate fallback", cfg.aec)
	}
}

func TestKoeCmdHasAECFlag(t *testing.T) {
	if koeCmd.Flags().Lookup("aec") == nil {
		t.Fatal("koe command must expose --aec for VPIO opt-in testing")
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

func TestKoePersonaSummaryAndDesktopDiscipline(t *testing.T) {
	// The do_task result carries a context digest: recaps/follow-ups the digest
	// covers are answered directly (live 2026-07-02: half the delegations in one
	// call were re-fetch recaps); only detail beyond it goes back through do_task.
	// Kocoro Desktop is mentioned only for genuinely long/structured results.
	for _, want := range []string{"context digest", "never call do_task to re-fetch", "mention it only when"} {
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
// "1+1" — the direct-answer examples must be the things users actually do:
// recapping and follow-ups on what was already said in the call.
func TestKoePersonaUsesRealisticDirectAnswerExamples(t *testing.T) {
	if strings.Contains(koePersona, "1+1") {
		t.Fatal("koePersona should not use toy arithmetic as the direct-answer example")
	}
	if !strings.Contains(koePersona, "already said in this call") {
		t.Fatal("koePersona direct-answer examples should center on in-call content")
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

// TestKoePersonaAckIsVariedButContentFree: the do_task acknowledgement allows
// natural wording variety, but the no-content gate (no answer/number/step before
// the result) is load-bearing — it is what blocks hallucinated pre-answers.
func TestKoePersonaAckIsVariedButContentFree(t *testing.T) {
	for _, want := range []string{"Vary the wording", "must not contain any answer"} {
		if !strings.Contains(koePersona, want) {
			t.Fatalf("koePersona missing ack contract %q", want)
		}
	}
	if strings.Contains(koePersona, `say exactly "我来处理"`) {
		t.Fatal("koePersona must not mandate a single fixed acknowledgement phrase anymore")
	}
}

// TestKoePersonaDividesByInformationSource pins the front/back-brain split: the
// line is where the answer COMES FROM (conversation-internal one-step vs the
// outside world), not task difficulty. The old blanket "any number or
// calculation" rule routed 1+1 through a full agent turn.
func TestKoePersonaDividesByInformationSource(t *testing.T) {
	for _, want := range []string{"one obvious step", "outside this conversation"} {
		if !strings.Contains(koePersona, want) {
			t.Fatalf("koePersona missing information-source split %q", want)
		}
	}
	if strings.Contains(koePersona, "any number or calculation") {
		t.Fatal("koePersona must not keep the blanket number/calculation ban")
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
