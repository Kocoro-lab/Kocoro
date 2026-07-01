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
