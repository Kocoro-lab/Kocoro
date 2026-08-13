//go:build live

package e2e

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

// TestLive_FastLongChainMechanism is a paid stress qualification for sequential
// AgentLoop mechanics. It does not qualify general-purpose task quality.
func TestLive_FastLongChainMechanism(t *testing.T) {
	if os.Getenv(fastLongChainGateEnv) != "1" {
		t.Skipf("set %s=1 to authorize the paid Fast long-chain lane", fastLongChainGateEnv)
	}
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Fatal("also set SHANNON_E2E_LIVE=1 to authorize real provider calls")
	}

	cfg := loadFastLongChainConfig(t)
	provider := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)
	resolveCtx, resolveCancel := context.WithTimeout(context.Background(), 10*time.Second)
	cloudProfile, resolveErr := provider.ResolveKoeExecutionProfile(resolveCtx)
	resolveCancel()
	profile := executionprofile.Resolve(executionprofile.ResolutionInput{
		RequestedMode: executionprofile.ModeFast,
		FastEnabled:   true,
		CloudProfile:  &cloudProfile,
		CloudError:    resolveErr,
	})
	if resolveErr != nil || profile.ValidateFast() != nil {
		t.Fatalf("resolve sealed Fast profile: resolve_error=%v validation_error=%v", resolveErr, profile.ValidateFast())
	}

	suiteCtx, suiteCancel := context.WithTimeout(context.Background(), fastLongChainSuiteTimeout)
	defer suiteCancel()
	trials := make([]fastLongChainTrial, 0, cfg.repetitions)
	for repetition := 1; repetition <= cfg.repetitions; repetition++ {
		if suiteCtx.Err() != nil {
			break
		}
		nonce, trialID, err := newFastLongChainNonce()
		if err != nil {
			t.Fatalf("create per-run nonce: %v", err)
		}
		wrapped := newFastLongChainCacheClient(provider, profile.ProfileID)
		trial := runFastLongChainTrial(t, suiteCtx, wrapped, profile, nonce, trialID, repetition, true)
		trials = append(trials, trial)
		t.Logf("fast_long_chain rep=%d correct=%t failures=%v tools=%d bad=%d iterations=%d llm=%d latency_ms=%d cost_usd=%.6f cache_off=%t cached=%t",
			trial.Repetition, trial.Correct, trial.Failures, trial.ToolCalls, trial.BadToolCalls,
			trial.Status.IterationCount, trial.LLMCalls, trial.LatencyMillis, trial.CostUSD,
			trial.Cache.AllRequestsOff, trial.Cache.WholeResponseCached)
		if trial.CostUSD > fastLongChainPerRunMaxCostUSD || fastLongChainTotalCost(trials) > fastLongChainSuiteMaxCostUSD {
			break
		}
	}

	report := newFastLongChainReport(cfg, profile, trials)
	if err := writeFastLongChainReport(cfg.outputPath, report); err != nil {
		t.Fatalf("write Fast long-chain report: %v", err)
	}
	if !report.Complete || !report.MechanismQualifying {
		t.Fatalf("Fast long-chain mechanism gate failed closed: complete=%t qualifying=%t failures=%v report=%s",
			report.Complete, report.MechanismQualifying, report.QualificationFailures, cfg.outputPath)
	}
	if cfg.sample == "release" && !report.MechanismReleaseQualifying {
		t.Fatalf("Fast long-chain release gate did not qualify: failures=%v report=%s",
			report.QualificationFailures, cfg.outputPath)
	}
}
