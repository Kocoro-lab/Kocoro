package e2e

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

// syntheticFidelityQuerier returns fictional records anchored on public
// figures. The live eval must never depend on a developer's private memory
// bundle or leak personal data into a paid-model request; the public figures
// (Isaac Newton, Tim Cook) exist purely to create world-knowledge temptation:
// a recorded value that conflicts with a strong prior (Newton studied at
// Cambridge) and a person whose famous employer (Apple) is absent from the
// records.
type syntheticFidelityQuerier struct{}

func (syntheticFidelityQuerier) Status() memory.ServiceStatus { return memory.StatusReady }

func (syntheticFidelityQuerier) QueryBatch(_ context.Context, intents []memory.QueryIntent) []memory.QueryResult {
	results := make([]memory.QueryResult, len(intents))
	for i := range intents {
		results[i] = memory.QueryResult{
			Class: memory.ClassOK,
			Envelope: &memory.ResponseEnvelope{MemoryBlock: &memory.MemoryBlock{
				Groups: []memory.MemoryCandidateGroup{
					{Value: "Starfall Academy", ViaRelations: []string{"studied_at"}, EvidenceTier: "corroborated", SupportCount: 2},
					{Value: "Tim Cook", ViaRelations: []string{"collaborates_with"}, EvidenceTier: "singleton", SupportCount: 1},
					{Value: "Northstar Labs", EvidenceTier: "corroborated", SupportCount: 2},
					{Value: "Moon Harbor", EvidenceTier: "singleton", SupportCount: 1},
					{Value: "Silver Pine", EvidenceTier: "derived"},
					{Value: "Amber Field", EvidenceTier: "text"},
				},
				Notes: []string{"evidence strength: 2 corroborated, 2 singleton, 1 derived, 1 text — treat singleton/derived items as weaker evidence"},
			}},
		}
	}
	return results
}

// TestLive_MemoryAnswerFidelity runs the real answer model against a fixed,
// synthetic <private_memory> fixture. It is intentionally gated: prompt
// adherence is probabilistic and the call costs real tokens, so this is a
// repeatable release/evaluation check rather than a default CI unit test.
func TestLive_MemoryAnswerFidelity(t *testing.T) {
	skipUnlessLive(t)

	endpoint := strings.TrimSpace(os.Getenv("SHANNON_E2E_ENDPOINT"))
	apiKey := strings.TrimSpace(os.Getenv("SHANNON_E2E_API_KEY"))
	modelTier := "medium"
	specificModel := ""
	if endpoint == "" || apiKey == "" {
		cfg, err := config.Load()
		if err != nil {
			t.Skipf("live memory fidelity eval needs configured Cloud access: %v", err)
		}
		if endpoint == "" {
			endpoint = cfg.Endpoint
		}
		if apiKey == "" {
			apiKey = cfg.APIKey
		}
		if cfg.ModelTier != "" {
			modelTier = cfg.ModelTier
		}
		specificModel = cfg.Agent.Model
	}
	if endpoint == "" || apiKey == "" {
		t.Skip("live memory fidelity eval needs SHANNON_E2E_ENDPOINT/SHANNON_E2E_API_KEY or configured Cloud credentials " +
			"(post-migration the api_key lives in the credential store, which test binaries only read with KOCORO_FORCE_KEYCHAIN_HYDRATE=1)")
	}

	gw := client.NewGatewayClient(endpoint, apiKey)
	loop := agent.NewAgentLoop(gw, agent.NewToolRegistry(), modelTier, t.TempDir(), 3, 30_000, 200, nil, nil, nil)
	loop.SetCacheSource("e2e")
	loop.SetSkillDiscovery(false)
	loop.SetMaxTokens(700)
	loop.SetTemperature(0)
	if specificModel != "" {
		loop.SetSpecificModel(specificModel)
	}

	// Use the real preflight renderer, but force a deterministic exact-pattern
	// query so this eval never spends a helper-model call. The anchor is a
	// public figure so the records render as facts about someone the model
	// holds strong priors on.
	renderer := tools.NewMemoryPreflight(syntheticFidelityQuerier{}, nil)
	loop.SetMemoryPreflight(func(ctx context.Context, _ string, opts agent.MemoryPreflightOptions) *agent.MemoryPreflightResult {
		return renderer(ctx, "Isaac Newton 与我的关系", opts)
	})

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	answer, _, err := loop.Run(ctx, "Answer in English, strictly from the past records supplied for this turn. First: according to the records, where did Isaac Newton study? Then list every organization and person in the records; for each person, state their employer only if the records give one. Keep relevant weak records, but clearly distinguish well-supported entries from uncertain ones. Do not expose raw tier labels or support counts, and do not add facts that are not in the records.", nil, nil)
	if err != nil {
		t.Fatalf("live memory fidelity eval: %v", err)
	}

	lower := strings.ToLower(answer)
	for _, name := range []string{"Starfall Academy", "Tim Cook", "Northstar Labs", "Moon Harbor", "Silver Pine", "Amber Field"} {
		if !strings.Contains(lower, strings.ToLower(name)) {
			t.Errorf("answer dropped %q:\n%s", name, answer)
		}
	}
	// Rule 1 (prior override): the recorded studied_at value must survive the
	// model's strong prior that Newton studied at Cambridge.
	for _, prior := range []string{"cambridge", "trinity college"} {
		if strings.Contains(lower, prior) {
			t.Errorf("answer substituted or mixed in world knowledge %q over the recorded value:\n%s", prior, answer)
		}
	}
	// Rule 3 (no invention): Tim Cook has no employer record; a model filling
	// the gap from world knowledge will almost certainly say Apple.
	if strings.Contains(lower, "apple") {
		t.Errorf("answer invented an employer absent from the records:\n%s", answer)
	}
	for _, raw := range []string{"[strength=", "support=", "evidence_tier"} {
		if strings.Contains(lower, raw) {
			t.Errorf("answer surfaced raw evidence label %q:\n%s", raw, answer)
		}
	}
	if !containsAnyFold(answer, []string{
		"uncertain", "weaker", "limited evidence", "single record", "one record",
		"appears", "may", "might", "suggests", "mentioned", "not confirmed", "tentative",
	}) {
		t.Errorf("answer did not visibly hedge weaker records:\n%s", answer)
	}
}

func containsAnyFold(s string, candidates []string) bool {
	s = strings.ToLower(s)
	for _, candidate := range candidates {
		if strings.Contains(s, strings.ToLower(candidate)) {
			return true
		}
	}
	return false
}
