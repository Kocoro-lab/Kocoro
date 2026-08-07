package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/memory"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

type memoryRecallABClient struct {
	inner client.LLMClient
}

func (c memoryRecallABClient) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	return c.inner.Complete(ctx, req)
}

func (c memoryRecallABClient) CompleteStream(ctx context.Context, req client.CompletionRequest, onDelta func(client.StreamDelta)) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	return c.inner.CompleteStream(ctx, req, onDelta)
}

type memoryRecallABService struct {
	mu                sync.Mutex
	preflightBatches  int
	preflightQueries  int
	preflightWithData int
	explicitQueries   int
	explicitWithData  int
	explicitModes     []string
	sessionQueries    int
	sessionWithData   int
}

type memoryRecallABCounts struct {
	PreflightBatches, PreflightQueries, PreflightWithData int
	ExplicitQueries, ExplicitWithData                     int
	ExplicitModes                                         []string
	SessionQueries, SessionWithData                       int
}

func (*memoryRecallABService) Status() memory.ServiceStatus { return memory.StatusReady }

func (s *memoryRecallABService) Query(_ context.Context, intent memory.QueryIntent) (*memory.ResponseEnvelope, memory.ErrorClass, error) {
	response := memoryRecallABResponse(intent)
	s.mu.Lock()
	s.explicitQueries++
	s.explicitModes = append(s.explicitModes, string(intent.Mode))
	if memoryRecallABHasData(response) {
		s.explicitWithData++
	}
	s.mu.Unlock()
	return response, memory.ClassOK, nil
}

func (s *memoryRecallABService) QueryBatch(_ context.Context, intents []memory.QueryIntent) []memory.QueryResult {
	s.mu.Lock()
	s.preflightBatches++
	s.preflightQueries += len(intents)
	s.mu.Unlock()
	results := make([]memory.QueryResult, len(intents))
	for i, intent := range intents {
		response := memoryRecallABResponse(intent)
		results[i] = memory.QueryResult{Envelope: response, Class: memory.ClassOK}
		if memoryRecallABHasData(response) {
			s.mu.Lock()
			s.preflightWithData++
			s.mu.Unlock()
		}
	}
	return results
}

func (s *memoryRecallABService) counts() memoryRecallABCounts {
	s.mu.Lock()
	defer s.mu.Unlock()
	return memoryRecallABCounts{
		PreflightBatches: s.preflightBatches, PreflightQueries: s.preflightQueries,
		PreflightWithData: s.preflightWithData, ExplicitQueries: s.explicitQueries,
		ExplicitWithData: s.explicitWithData, ExplicitModes: append([]string(nil), s.explicitModes...),
		SessionQueries: s.sessionQueries, SessionWithData: s.sessionWithData,
	}
}

func memoryRecallABHasData(response *memory.ResponseEnvelope) bool {
	return response != nil && response.MemoryBlock != nil && len(response.MemoryBlock.Groups) > 0
}

type memoryRecallABSessionSearch struct{ service *memoryRecallABService }

func (t *memoryRecallABSessionSearch) Info() agent.ToolInfo {
	return (&tools.SessionSearchTool{}).Info()
}

func (*memoryRecallABSessionSearch) RequiresApproval() bool     { return false }
func (*memoryRecallABSessionSearch) IsReadOnlyCall(string) bool { return true }

func (t *memoryRecallABSessionSearch) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	if result, valid := agent.ValidateToolArguments(t.Info(), argsJSON); !valid {
		return result, nil
	}
	var args struct {
		Query string `json:"query"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid input: %v", err)), nil
	}
	query := strings.ToLower(args.Query)
	withData := strings.Contains(query, "医生") || strings.Contains(query, "doctor") || strings.Contains(query, "aster")
	t.service.mu.Lock()
	t.service.sessionQueries++
	if withData {
		t.service.sessionWithData++
	}
	t.service.mu.Unlock()
	if withData {
		return agent.ToolResult{Content: `[{"snippet":"My doctor is Dr. Aster Vale."}]`}, nil
	}
	return agent.ToolResult{Content: "No matching sessions found."}, nil
}

func memoryRecallABResponse(intent memory.QueryIntent) *memory.ResponseEnvelope {
	anchor := strings.ToLower(strings.Join(intent.AnchorMentions, " "))
	value := ""
	relation := "related_to"
	switch {
	case strings.Contains(anchor, "张三"):
		value, relation = "zhang.san@memory.test", "has_email"
	case strings.Contains(anchor, "小王"):
		value, relation = "+81-50-7421-6004", "has_handle_on"
	case strings.Contains(anchor, "aster"), strings.Contains(anchor, "医生"):
		value, relation = "Dr. Aster Vale", "related_to"
	case strings.Contains(anchor, "jordan vale"):
		value, relation = "jordan.vale@memory.test", "has_email"
	case strings.Contains(anchor, "haruto sato"):
		value, relation = "haruto.sato@memory.test", "has_email"
	}
	block := &memory.MemoryBlock{Notes: []string{"synthetic A/B fixture; answer only from these past records"}}
	if value == "" {
		reason := "no matching synthetic past record"
		block.NoDataReason = &reason
	} else {
		block.Groups = []memory.MemoryCandidateGroup{{
			Value:        value,
			ViaRelations: []string{relation},
			EvidenceTier: "corroborated",
			SupportCount: 2,
		}}
	}
	return &memory.ResponseEnvelope{ProtocolVersion: 1, BundleVersion: "memory-ab-fixture-v1", MemoryBlock: block}
}

type memoryRecallABCase struct {
	Name           string
	Prompt         string
	ExpectedAnswer string
	ShouldRecall   bool
}

type memoryRecallABResult struct {
	Case              string   `json:"case"`
	Variant           string   `json:"variant"`
	Repetition        int      `json:"repetition"`
	ShouldRecall      bool     `json:"should_recall"`
	RoutingCorrect    bool     `json:"routing_correct"`
	RoutingEfficient  bool     `json:"routing_efficient"`
	AnswerCorrect     bool     `json:"answer_correct"`
	Correct           bool     `json:"correct"`
	RecallTriggered   bool     `json:"recall_triggered"`
	PreflightBatches  int      `json:"preflight_batches"`
	PreflightQueries  int      `json:"preflight_queries"`
	PreflightWithData int      `json:"preflight_results_with_data"`
	PreflightHelpers  int      `json:"preflight_helpers"`
	ExplicitQueries   int      `json:"explicit_queries"`
	ExplicitWithData  int      `json:"explicit_results_with_data"`
	ExplicitModes     []string `json:"explicit_modes,omitempty"`
	SessionQueries    int      `json:"session_search_queries"`
	SessionWithData   int      `json:"session_search_results_with_data"`
	LLMCalls          int      `json:"llm_calls"`
	LatencyMs         int64    `json:"latency_ms"`
	InputTokens       int      `json:"input_tokens"`
	OutputTokens      int      `json:"output_tokens"`
	CostUSD           float64  `json:"cost_usd"`
	Answer            string   `json:"answer"`
}

func TestLive_MemoryRecallPreflightAB(t *testing.T) {
	skipUnlessLive(t)
	repetitions := memoryRecallABRepetitions(t)
	cases := []memoryRecallABCase{
		{Name: "zh_implicit_email", Prompt: "张三的邮箱是多少？只回答邮箱地址。", ExpectedAnswer: "zhang.san@memory.test", ShouldRecall: true},
		{Name: "zh_implicit_phone", Prompt: "小王电话是多少？只回答电话号码。", ExpectedAnswer: "+81-50-7421-6004", ShouldRecall: true},
		{Name: "zh_personal_doctor", Prompt: "我的医生叫什么？只回答姓名。", ExpectedAnswer: "Aster Vale", ShouldRecall: true},
		{Name: "en_named_email", Prompt: "What is Jordan Vale's email address? Reply with only the address.", ExpectedAnswer: "jordan.vale@memory.test", ShouldRecall: true},
		{Name: "ja_explicit_email", Prompt: "以前話した Haruto Sato のメールアドレスは？メールアドレスだけ答えて。", ExpectedAnswer: "haruto.sato@memory.test", ShouldRecall: true},
		{Name: "zh_ordinary_work", Prompt: "帮我写一封两句话的请假邮件，不要发送。", ShouldRecall: false},
		{Name: "en_public_knowledge", Prompt: "What is two plus two? Reply with only the number.", ShouldRecall: false},
		{Name: "ja_ambiguous_contact", Prompt: "あの人の連絡先は？", ShouldRecall: true},
	}

	rawClient, modelTier, specificModel := newLiveMemoryClient(t)
	gw := memoryRecallABClient{inner: rawClient}
	results := make([]memoryRecallABResult, 0, len(cases)*2*repetitions)
	for repetition := 1; repetition <= repetitions; repetition++ {
		variants := []string{"preflight", "model"}
		if repetition%2 == 0 {
			variants[0], variants[1] = variants[1], variants[0]
		}
		for _, tc := range cases {
			for _, variant := range variants {
				result := runMemoryRecallABCase(t, gw, modelTier, specificModel, tc, variant, repetition)
				results = append(results, result)
				t.Logf("memory_ab case=%s variant=%s rep=%d correct=%t efficient=%t triggered=%t preflight=%d/%d data=%d helper=%d explicit=%d data=%d modes=%v session=%d data=%d llm=%d latency_ms=%d cost_usd=%.8f answer=%q",
					result.Case, result.Variant, result.Repetition, result.Correct, result.RoutingEfficient, result.RecallTriggered,
					result.PreflightBatches, result.PreflightQueries, result.PreflightWithData, result.PreflightHelpers,
					result.ExplicitQueries, result.ExplicitWithData, result.ExplicitModes,
					result.SessionQueries, result.SessionWithData,
					result.LLMCalls, result.LatencyMs, result.CostUSD, result.Answer)
			}
		}
	}

	summary := summarizeMemoryRecallAB(results)
	if outputPath := strings.TrimSpace(os.Getenv("KOCORO_MEMORY_AB_OUTPUT")); outputPath != "" {
		report := map[string]any{
			"schema_version":       "kocoro.memory_recall_ab.v1",
			"generated_at":         time.Now().UTC().Format(time.RFC3339Nano),
			"repetitions_per_cell": repetitions,
			"results":              results,
			"summary":              summary,
			"coverage_boundaries": []string{
				"Uses deterministic synthetic memory responses, not a user's private sidecar bundle.",
				"Measures routing, answer fidelity, calls, latency, tokens, and cost; it does not measure bundle training quality.",
			},
		}
		if err := writeMemoryRecallABJSON(outputPath, report); err != nil {
			t.Fatalf("write memory A/B report: %v", err)
		}
	}
	body, err := json.Marshal(summary)
	if err != nil {
		t.Fatalf("marshal memory A/B summary: %v", err)
	}
	t.Logf("memory_ab_summary=%s", body)
}

func writeMemoryRecallABJSON(path string, report any) error {
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, body, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func memoryRecallABRepetitions(t *testing.T) int {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv("KOCORO_MEMORY_AB_REPETITIONS"))
	if raw == "" {
		return 1
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > 10 {
		t.Fatalf("KOCORO_MEMORY_AB_REPETITIONS must be an integer in [1,10], got %q", raw)
	}
	return n
}

func runMemoryRecallABCase(t *testing.T, llm client.LLMClient, modelTier, specificModel string, tc memoryRecallABCase, variant string, repetition int) memoryRecallABResult {
	t.Helper()
	service := &memoryRecallABService{}
	registry := agent.NewToolRegistry()
	registry.Register(&tools.MemoryTool{Service: service})
	registry.Register(&memoryRecallABSessionSearch{service: service})
	loop := agent.NewAgentLoop(llm, registry, modelTier, t.TempDir(), 4, 30_000, 200, nil, nil, nil)
	loop.SetCacheSource("memory_recall_ab")
	loop.SetSkillDiscovery(false)
	loop.SetMaxTokens(700)
	loop.SetTemperature(0)
	if specificModel != "" {
		loop.SetSpecificModel(specificModel)
	}
	var preflightUsage client.Usage
	preflightHelpers := 0
	if variant == "preflight" {
		preflight := tools.NewMemoryPreflight(service, llm)
		loop.SetMemoryPreflight(func(ctx context.Context, query string, opts agent.MemoryPreflightOptions) *agent.MemoryPreflightResult {
			result := preflight(ctx, query, opts)
			if result != nil && memoryRecallABUsageNonZero(result.Usage) {
				preflightUsage = result.Usage
				preflightHelpers++
			}
			return result
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	started := time.Now()
	answer, usage, err := loop.Run(ctx, tc.Prompt, nil, nil)
	if err != nil {
		t.Fatalf("memory A/B %s/%s repetition %d: %v", tc.Name, variant, repetition, err)
	}
	counts := service.counts()
	preflightDelta := agent.TurnUsage{}
	if memoryRecallABUsageNonZero(preflightUsage) {
		preflightDelta = agent.LLMUsageDelta(preflightUsage, "")
	}
	triggered := counts.PreflightQueries+counts.ExplicitQueries+counts.SessionQueries > 0
	routingCorrect := triggered == tc.ShouldRecall
	routingEfficient := counts.PreflightQueries+counts.ExplicitQueries+counts.SessionQueries <= 1
	answerCorrect := true
	if tc.ExpectedAnswer != "" {
		answerCorrect = strings.Contains(strings.ToLower(answer), strings.ToLower(tc.ExpectedAnswer))
	}
	correct := routingCorrect && routingEfficient && answerCorrect
	return memoryRecallABResult{
		Case: tc.Name, Variant: variant, Repetition: repetition, ShouldRecall: tc.ShouldRecall,
		RoutingCorrect: routingCorrect, RoutingEfficient: routingEfficient,
		AnswerCorrect: answerCorrect, Correct: correct,
		RecallTriggered: triggered, PreflightBatches: counts.PreflightBatches, PreflightQueries: counts.PreflightQueries,
		PreflightWithData: counts.PreflightWithData, PreflightHelpers: preflightHelpers,
		ExplicitQueries: counts.ExplicitQueries, ExplicitWithData: counts.ExplicitWithData, ExplicitModes: counts.ExplicitModes,
		SessionQueries: counts.SessionQueries, SessionWithData: counts.SessionWithData,
		LLMCalls:  usage.LLMCalls + preflightDelta.LLMCalls,
		LatencyMs: time.Since(started).Milliseconds(), InputTokens: usage.InputTokens + preflightDelta.InputTokens,
		OutputTokens: usage.OutputTokens + preflightDelta.OutputTokens,
		CostUSD:      usage.CostUSD + preflightDelta.CostUSD, Answer: strings.TrimSpace(answer),
	}
}

func memoryRecallABUsageNonZero(usage client.Usage) bool {
	return usage.TotalTokens != 0 || usage.InputTokens != 0 || usage.OutputTokens != 0 || usage.CostUSD != 0
}

func summarizeMemoryRecallAB(results []memoryRecallABResult) map[string]any {
	type totals struct {
		Runs, Correct, Efficient, Triggered, PreflightQueries, PreflightWithData, PreflightHelpers int
		ExplicitQueries, ExplicitWithData, SessionQueries, SessionWithData, LLMCalls               int
		PositiveRuns, PositiveTriggered, PositiveAnswers, NegativeRuns, NegativeTriggered          int
		LatencyMs, PositiveLatencyMs, NegativeLatencyMs                                            int64
		Latencies, PositiveLatencies, NegativeLatencies                                            []int64
		CostUSD                                                                                    float64
	}
	byVariant := map[string]*totals{}
	for _, result := range results {
		total := byVariant[result.Variant]
		if total == nil {
			total = &totals{}
			byVariant[result.Variant] = total
		}
		total.Runs++
		if result.Correct {
			total.Correct++
		}
		if result.RoutingEfficient {
			total.Efficient++
		}
		if result.RecallTriggered {
			total.Triggered++
		}
		total.PreflightQueries += result.PreflightQueries
		total.PreflightWithData += result.PreflightWithData
		total.PreflightHelpers += result.PreflightHelpers
		total.ExplicitQueries += result.ExplicitQueries
		total.ExplicitWithData += result.ExplicitWithData
		total.SessionQueries += result.SessionQueries
		total.SessionWithData += result.SessionWithData
		total.LLMCalls += result.LLMCalls
		total.LatencyMs += result.LatencyMs
		total.Latencies = append(total.Latencies, result.LatencyMs)
		if result.ShouldRecall {
			total.PositiveRuns++
			if result.RecallTriggered {
				total.PositiveTriggered++
			}
			if result.AnswerCorrect {
				total.PositiveAnswers++
			}
			total.PositiveLatencyMs += result.LatencyMs
			total.PositiveLatencies = append(total.PositiveLatencies, result.LatencyMs)
		} else {
			total.NegativeRuns++
			if result.RecallTriggered {
				total.NegativeTriggered++
			}
			total.NegativeLatencyMs += result.LatencyMs
			total.NegativeLatencies = append(total.NegativeLatencies, result.LatencyMs)
		}
		total.CostUSD += result.CostUSD
	}
	out := map[string]any{}
	for variant, total := range byVariant {
		out[variant] = map[string]any{
			"runs": total.Runs, "correct": total.Correct,
			"accuracy":                         float64(total.Correct) / float64(total.Runs),
			"routing_efficiency_rate":          float64(total.Efficient) / float64(total.Runs),
			"recall_triggered":                 total.Triggered,
			"preflight_queries":                total.PreflightQueries,
			"preflight_results_with_data":      total.PreflightWithData,
			"preflight_helpers":                total.PreflightHelpers,
			"explicit_queries":                 total.ExplicitQueries,
			"explicit_results_with_data":       total.ExplicitWithData,
			"session_search_queries":           total.SessionQueries,
			"session_search_results_with_data": total.SessionWithData,
			"mean_llm_calls":                   float64(total.LLMCalls) / float64(total.Runs),
			"mean_latency_ms":                  float64(total.LatencyMs) / float64(total.Runs),
			"median_latency_ms":                memoryRecallABMedian(total.Latencies),
			"positive": map[string]any{
				"runs":              total.PositiveRuns,
				"routing_recall":    float64(total.PositiveTriggered) / float64(total.PositiveRuns),
				"answer_success":    float64(total.PositiveAnswers) / float64(total.PositiveRuns),
				"mean_latency_ms":   float64(total.PositiveLatencyMs) / float64(total.PositiveRuns),
				"median_latency_ms": memoryRecallABMedian(total.PositiveLatencies),
			},
			"negative": map[string]any{
				"runs":                 total.NegativeRuns,
				"false_memory_queries": total.NegativeTriggered,
				"mean_latency_ms":      float64(total.NegativeLatencyMs) / float64(total.NegativeRuns),
				"median_latency_ms":    memoryRecallABMedian(total.NegativeLatencies),
			},
			"total_cost_usd": total.CostUSD,
		}
	}
	out["note"] = fmt.Sprintf("paired synthetic-memory routing evaluation; %d total runs", len(results))
	return out
}

func memoryRecallABMedian(values []int64) float64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	mid := len(ordered) / 2
	if len(ordered)%2 == 1 {
		return float64(ordered[mid])
	}
	return float64(ordered[mid-1]+ordered[mid]) / 2
}
