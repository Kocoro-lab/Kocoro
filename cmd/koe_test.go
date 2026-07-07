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
	if got := baseKoePersona(koeConfig{language: ""}); got != koePersona {
		t.Errorf("empty language should give the bare base persona")
	}
	zh := baseKoePersona(koeConfig{language: "zh"})
	if !strings.HasPrefix(zh, koePersona) || !strings.Contains(zh, koeLanguageInstruction("zh")) {
		t.Errorf("zh base persona missing base or language pin: %q", zh)
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
	for _, want := range []string{koePersona, "USER_CONTEXT_MARKER", koeAgentListLine(agents), koeLanguageInstruction("en")} {
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

// TestApplyBargeInEnv locks the flag→env bridge: --barge-in on flips both env-gated
// knobs to "1"; off leaves them untouched (power-user env escape hatch preserved).
func TestApplyBargeInEnv(t *testing.T) {
	t.Setenv("KOE_VPIO_BARGE_IN", "")
	t.Setenv("KOE_INTERRUPT_RESPONSE", "")

	applyBargeInEnv(false)
	if v := os.Getenv("KOE_VPIO_BARGE_IN"); v != "" {
		t.Fatalf("barge-in off set KOE_VPIO_BARGE_IN=%q, want unchanged", v)
	}
	if v := os.Getenv("KOE_INTERRUPT_RESPONSE"); v != "" {
		t.Fatalf("barge-in off set KOE_INTERRUPT_RESPONSE=%q, want unchanged", v)
	}

	applyBargeInEnv(true)
	if v := os.Getenv("KOE_VPIO_BARGE_IN"); v != "1" {
		t.Fatalf("KOE_VPIO_BARGE_IN=%q, want 1", v)
	}
	if v := os.Getenv("KOE_INTERRUPT_RESPONSE"); v != "1" {
		t.Fatalf("KOE_INTERRUPT_RESPONSE=%q, want 1", v)
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
