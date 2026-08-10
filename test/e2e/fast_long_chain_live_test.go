package e2e

// Fast-only long-chain mechanism lane for the production AgentLoop.
//
// Offline integration (no provider):
//
//	go test ./test/e2e -run TestOffline_FastLongChainMechanism -count=1 -v
//
// Paid smoke (three repetitions by default):
//
//	SHANNON_E2E_LIVE=1 KOCORO_FAST_LONG_CHAIN_LIVE=1 go test ./test/e2e -run TestLive_FastLongChainMechanism -count=1 -timeout 60m -v
//
// Paid release qualification (exactly fifteen repetitions):
//
//	SHANNON_E2E_LIVE=1 KOCORO_FAST_LONG_CHAIN_LIVE=1 KOCORO_FAST_LONG_CHAIN_SAMPLE=release go test ./test/e2e -run TestLive_FastLongChainMechanism -count=1 -timeout 60m -v

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	agentsapi "github.com/Kocoro-lab/ShanClaw/internal/agents"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
)

const (
	fastLongChainGateEnv   = "KOCORO_FAST_LONG_CHAIN_LIVE"
	fastLongChainSampleEnv = "KOCORO_FAST_LONG_CHAIN_SAMPLE"
	fastLongChainRepsEnv   = "KOCORO_FAST_LONG_CHAIN_REPETITIONS"
	fastLongChainOutputEnv = "KOCORO_FAST_LONG_CHAIN_OUTPUT"

	fastLongChainSteps              = 56
	fastLongChainExpectedIterations = fastLongChainSteps + 1
	fastLongChainMaxIterations      = 64
	fastLongChainMaxTokens          = 700
	fastLongChainSmokeRepetitions   = 3
	fastLongChainReleaseRepetitions = 15
	fastLongChainPerRunTimeout      = 10 * time.Minute
	fastLongChainSuiteTimeout       = 60 * time.Minute
	fastLongChainPerRunMaxCostUSD   = 0.10
	fastLongChainSuiteMaxCostUSD    = 1.50
)

type fastLongChainProbeTool struct {
	mu       sync.Mutex
	nonce    []byte
	target   int
	calls    int
	badCalls int
}

func (*fastLongChainProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "fast_long_chain_step",
		Description: "Advance one step in a bounded sequential state machine. Start with step=1 and token=INIT, then use the exact next_token returned by the previous successful step. Never batch calls.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step":  map[string]any{"type": "integer"},
				"token": map[string]any{"type": "string"},
			},
		},
		Required: []string{"step", "token"},
	}
}

func (*fastLongChainProbeTool) RequiresApproval() bool     { return false }
func (*fastLongChainProbeTool) IsReadOnlyCall(string) bool { return false }
func (*fastLongChainProbeTool) TrustsDistinctOutcomeProgress() bool {
	return true
}

func (t *fastLongChainProbeTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		Step  int    `json:"step"`
		Token string `json:"token"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		t.mu.Lock()
		t.badCalls++
		t.mu.Unlock()
		return agent.ValidationError("step and token must be valid JSON fields"), nil
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	expectedStep := t.calls + 1
	expectedToken := "INIT"
	if expectedStep > 1 {
		expectedToken = fastLongChainToken(t.nonce, expectedStep-1)
	}
	if args.Step != expectedStep || strings.TrimSpace(args.Token) != expectedToken {
		t.badCalls++
		return agent.ValidationError(fmt.Sprintf(
			"expected step=%d and token=%s; state did not advance",
			expectedStep, expectedToken,
		)), nil
	}

	t.calls++
	if t.calls == t.target {
		return agent.ToolResult{Content: fmt.Sprintf(
			"CHAIN COMPLETE. final_code=%s", fastLongChainFinalCode(t.nonce),
		)}, nil
	}
	return agent.ToolResult{Content: fmt.Sprintf(
		"STEP %02d COMPLETE. next_step=%d next_token=%s",
		t.calls, t.calls+1, fastLongChainToken(t.nonce, t.calls),
	)}, nil
}

func (t *fastLongChainProbeTool) snapshot() (calls, badCalls int) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls, t.badCalls
}

func fastLongChainToken(nonce []byte, step int) string {
	mac := hmac.New(sha256.New, nonce)
	_, _ = fmt.Fprintf(mac, "fast-long-chain-step:%d", step)
	return "LC-" + hex.EncodeToString(mac.Sum(nil)[:10])
}

func fastLongChainFinalCode(nonce []byte) string {
	mac := hmac.New(sha256.New, nonce)
	_, _ = mac.Write([]byte("fast-long-chain-final"))
	return "FAST-LONG-" + strings.ToUpper(hex.EncodeToString(mac.Sum(nil)[:8]))
}

func newFastLongChainNonce() ([]byte, string, error) {
	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, "", err
	}
	digest := sha256.Sum256(nonce)
	return nonce, hex.EncodeToString(digest[:6]), nil
}

type fastLongChainCacheClient struct {
	inner             client.LLMClient
	expectedProfileID string

	mu               sync.Mutex
	requests         int
	responses        int
	allCacheOff      bool
	anyCached        bool
	allProfilePinned bool
	allMaxTokensSafe bool
	allTemperature0  bool
}

func newFastLongChainCacheClient(inner client.LLMClient, profileID string) *fastLongChainCacheClient {
	return &fastLongChainCacheClient{
		inner: inner, expectedProfileID: profileID,
		allCacheOff: true, allProfilePinned: true,
		allMaxTokensSafe: true, allTemperature0: true,
	}
}

func (c *fastLongChainCacheClient) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	c.recordRequest(req)
	resp, err := c.inner.Complete(ctx, req)
	c.recordResponse(resp)
	return resp, err
}

func (c *fastLongChainCacheClient) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	onDelta func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	c.recordRequest(req)
	resp, err := c.inner.CompleteStream(ctx, req, onDelta)
	c.recordResponse(resp)
	return resp, err
}

func (c *fastLongChainCacheClient) recordRequest(req client.CompletionRequest) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.requests++
	c.allCacheOff = c.allCacheOff && req.ResponseCachePolicy == executionprofile.ResponseCacheOff
	c.allProfilePinned = c.allProfilePinned && req.ExecutionProfileID == c.expectedProfileID && req.ExecutionProfileID != ""
	c.allMaxTokensSafe = c.allMaxTokensSafe && req.MaxTokens > 0 && req.MaxTokens <= fastLongChainMaxTokens
	c.allTemperature0 = c.allTemperature0 && req.Temperature == 0
}

func (c *fastLongChainCacheClient) recordResponse(resp *client.CompletionResponse) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if resp == nil {
		return
	}
	c.responses++
	c.anyCached = c.anyCached || resp.Cached
}

type fastLongChainCacheObservation struct {
	Requests            int    `json:"requests"`
	Responses           int    `json:"responses"`
	ResponseCachePolicy string `json:"response_cache_policy"`
	AllRequestsOff      bool   `json:"all_requests_off"`
	WholeResponseCached bool   `json:"whole_response_cached"`
	ProfilePinned       bool   `json:"profile_pinned"`
	MaxTokensWithinCap  bool   `json:"max_tokens_within_cap"`
	TemperatureZero     bool   `json:"temperature_zero"`
}

func (c *fastLongChainCacheClient) observation() fastLongChainCacheObservation {
	c.mu.Lock()
	defer c.mu.Unlock()
	return fastLongChainCacheObservation{
		Requests: c.requests, Responses: c.responses,
		ResponseCachePolicy: string(executionprofile.ResponseCacheOff),
		AllRequestsOff:      c.requests > 0 && c.allCacheOff,
		WholeResponseCached: c.anyCached,
		ProfilePinned:       c.requests > 0 && c.allProfilePinned,
		MaxTokensWithinCap:  c.requests > 0 && c.allMaxTokensSafe,
		TemperatureZero:     c.requests > 0 && c.allTemperature0,
	}
}

type fastLongChainTrialStatus struct {
	Partial        bool   `json:"partial"`
	FailureCode    string `json:"failure_code"`
	LastTool       string `json:"last_tool"`
	RetryCount     int    `json:"retry_count"`
	IterationCount int    `json:"iteration_count"`
}

type fastLongChainTrial struct {
	Repetition        int                           `json:"repetition"`
	TrialID           string                        `json:"trial_id"`
	Correct           bool                          `json:"correct"`
	Failures          []string                      `json:"failures"`
	LatencyMillis     int64                         `json:"latency_millis"`
	ToolCalls         int                           `json:"tool_calls"`
	BadToolCalls      int                           `json:"bad_tool_calls"`
	Status            fastLongChainTrialStatus      `json:"status"`
	Error             string                        `json:"error,omitempty"`
	Answer            string                        `json:"answer"`
	UsageObserved     bool                          `json:"usage_observed"`
	CostObserved      bool                          `json:"cost_observed"`
	LLMCalls          int                           `json:"llm_calls"`
	InputTokens       int                           `json:"input_tokens"`
	OutputTokens      int                           `json:"output_tokens"`
	TotalTokens       int                           `json:"total_tokens"`
	CostUSD           float64                       `json:"cost_usd"`
	CacheReadTokens   int                           `json:"cache_read_tokens"`
	CacheCreateTokens int                           `json:"cache_creation_tokens"`
	Cache             fastLongChainCacheObservation `json:"cache"`
}

type fastLongChainReport struct {
	SchemaVersion              string               `json:"schema_version"`
	GeneratedAt                string               `json:"generated_at"`
	Sample                     string               `json:"sample"`
	Complete                   bool                 `json:"complete"`
	MechanismQualifying        bool                 `json:"mechanism_qualifying"`
	MechanismReleaseQualifying bool                 `json:"mechanism_release_qualifying"`
	Repetitions                int                  `json:"repetitions"`
	MinimumReleaseRepetitions  int                  `json:"minimum_release_repetitions"`
	Scheduled                  int                  `json:"scheduled"`
	Completed                  int                  `json:"completed"`
	CorrectRuns                int                  `json:"correct_runs"`
	TargetToolSteps            int                  `json:"target_tool_steps"`
	ExpectedIterations         int                  `json:"expected_iterations"`
	MaxIterations              int                  `json:"max_iterations"`
	MaxTokens                  int                  `json:"max_tokens"`
	PerRunTimeoutSeconds       int                  `json:"per_run_timeout_seconds"`
	SuiteTimeoutSeconds        int                  `json:"suite_timeout_seconds"`
	PerRunMaxCostUSD           float64              `json:"per_run_max_cost_usd"`
	SuiteMaxCostUSD            float64              `json:"suite_max_cost_usd"`
	ReportedCostUSD            float64              `json:"reported_cost_usd"`
	UsageObserved              bool                 `json:"usage_observed"`
	CostObserved               bool                 `json:"cost_observed"`
	CacheIsolationObserved     bool                 `json:"cache_isolation_observed"`
	FastProfileObserved        bool                 `json:"fast_profile_observed"`
	FastProfileName            string               `json:"fast_profile_name"`
	FastProfileVersion         int                  `json:"fast_profile_version"`
	FastModel                  string               `json:"fast_model"`
	Trials                     []fastLongChainTrial `json:"trials"`
	QualificationFailures      []string             `json:"qualification_failures"`
	CoverageBoundaries         []string             `json:"coverage_boundaries"`
}

type fastLongChainFakeClient struct {
	mu      sync.Mutex
	nonce   []byte
	profile executionprofile.Profile
	calls   int
}

func (c *fastLongChainFakeClient) Complete(_ context.Context, _ client.CompletionRequest) (*client.CompletionResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.calls++
	if c.calls <= fastLongChainSteps {
		step := c.calls
		token := "INIT"
		if step > 1 {
			token = fastLongChainToken(c.nonce, step-1)
		}
		return &client.CompletionResponse{
			Provider: "openai", Model: c.profile.Model, FinishReason: "tool_use",
			ToolCalls: []client.FunctionCall{{
				ID: fmt.Sprintf("long-chain-%02d", step), Name: "fast_long_chain_step",
				Arguments: json.RawMessage(fmt.Sprintf(`{"step":%d,"token":%q}`, step, token)),
			}},
			Usage:     client.Usage{InputTokens: 100 + step, OutputTokens: 20, TotalTokens: 120 + step, CostUSD: 0.0001},
			RequestID: fmt.Sprintf("offline-long-chain-%02d", step),
		}, nil
	}
	if c.calls == fastLongChainExpectedIterations {
		return &client.CompletionResponse{
			Provider: "openai", Model: c.profile.Model,
			OutputText: "Final code: " + fastLongChainFinalCode(c.nonce), FinishReason: "end_turn",
			Usage:     client.Usage{InputTokens: 180, OutputTokens: 20, TotalTokens: 200, CostUSD: 0.0001},
			RequestID: "offline-long-chain-final",
		}, nil
	}
	return nil, fmt.Errorf("unexpected completion call %d", c.calls)
}

func (c *fastLongChainFakeClient) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func fastLongChainFixtureProfile() executionprofile.Profile {
	return executionprofile.Profile{
		RequestedMode: executionprofile.ModeFast, EffectiveMode: executionprofile.ModeFast,
		SchemaVersion: executionprofile.FastSchemaVersion,
		ProfileName:   executionprofile.FastProfileName, ProfileVersion: executionprofile.FastProfileVersion,
		ProfileID: "kfp1_fast_long_chain_offline", Provider: "openai", Model: "gpt-5.6-luna",
		APISurface: "openai_responses", ToolContract: executionprofile.FastToolContract,
		SupportsFunctions: true, ReasoningEffort: "medium", ServiceTier: "fast",
		ParallelToolCalls: true, ResponseCachePolicy: executionprofile.ResponseCacheOff,
		ResolutionReason: "offline_fast_long_chain_fixture",
	}
}

func TestOffline_FastLongChainMechanism(t *testing.T) {
	nonce, _, err := newFastLongChainNonce()
	if err != nil {
		t.Fatalf("create per-run nonce: %v", err)
	}
	guard := &fastLongChainProbeTool{nonce: nonce, target: 2}
	badResult, badErr := guard.Run(context.Background(), `{"step":2,"token":"INIT"}`)
	if badErr != nil || !badResult.IsError || badResult.ErrorCategory != agent.ErrCategoryValidation {
		t.Fatalf("bad call result = %+v, err=%v; want validation error", badResult, badErr)
	}
	if calls, badCalls := guard.snapshot(); calls != 0 || badCalls != 1 {
		t.Fatalf("bad call changed state: calls=%d bad_calls=%d, want 0/1", calls, badCalls)
	}
	if goodResult, goodErr := guard.Run(context.Background(), `{"step":1,"token":"INIT"}`); goodErr != nil || goodResult.IsError {
		t.Fatalf("step 1 after rejected call = %+v, err=%v; want success", goodResult, goodErr)
	}
	if calls, badCalls := guard.snapshot(); calls != 1 || badCalls != 1 {
		t.Fatalf("valid call after rejection state: calls=%d bad_calls=%d, want 1/1", calls, badCalls)
	}
	profile := fastLongChainFixtureProfile()
	if err := profile.ValidateFast(); err != nil {
		t.Fatalf("offline Fast profile: %v", err)
	}
	fake := &fastLongChainFakeClient{nonce: nonce, profile: profile}
	trial := runFastLongChainTrial(t, context.Background(), newFastLongChainCacheClient(fake, profile.ProfileID), profile, nonce, "offline", 1, false)
	if !trial.Correct {
		t.Fatalf("offline 56-step AgentLoop integration failed: %v", trial.Failures)
	}
	if trial.ToolCalls != fastLongChainSteps || trial.LLMCalls != fastLongChainExpectedIterations {
		t.Fatalf("offline calls tool=%d llm=%d, want %d/%d",
			trial.ToolCalls, trial.LLMCalls, fastLongChainSteps, fastLongChainExpectedIterations)
	}
	report := newFastLongChainReport(fastLongChainConfig{sample: "smoke", repetitions: 1}, profile, []fastLongChainTrial{trial})
	if !report.Complete || !report.MechanismQualifying || report.MechanismReleaseQualifying {
		t.Fatalf("offline smoke report qualification = %+v", report)
	}
	outputPath := filepath.Join(t.TempDir(), "fast-long-chain.json")
	if err := writeFastLongChainReport(outputPath, report); err != nil {
		t.Fatalf("atomically write offline report: %v", err)
	}
	var decoded fastLongChainReport
	body, err := os.ReadFile(outputPath)
	if err != nil {
		t.Fatalf("read offline report: %v", err)
	}
	if err := json.Unmarshal(body, &decoded); err != nil {
		t.Fatalf("decode offline report: %v", err)
	}
	if !decoded.Complete || !decoded.MechanismQualifying {
		t.Fatalf("decoded offline report lost qualification: %+v", decoded)
	}
	releaseTrials := make([]fastLongChainTrial, fastLongChainReleaseRepetitions)
	for i := range releaseTrials {
		releaseTrials[i] = trial
		releaseTrials[i].Repetition = i + 1
		releaseTrials[i].TrialID = fmt.Sprintf("offline-release-%02d", i+1)
	}
	releaseCfg := fastLongChainConfig{sample: "release", repetitions: fastLongChainReleaseRepetitions}
	releaseReport := newFastLongChainReport(releaseCfg, profile, releaseTrials)
	if !releaseReport.MechanismReleaseQualifying {
		t.Fatalf("15 complete offline fixtures did not release-qualify: %v", releaseReport.QualificationFailures)
	}
	incompleteReport := newFastLongChainReport(releaseCfg, profile, releaseTrials[:fastLongChainReleaseRepetitions-1])
	if incompleteReport.Complete || incompleteReport.MechanismReleaseQualifying {
		t.Fatalf("incomplete release fixtures unexpectedly qualified: %+v", incompleteReport)
	}
}

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

type fastLongChainConfig struct {
	endpoint    string
	apiKey      string
	sample      string
	repetitions int
	outputPath  string
}

func loadFastLongChainConfig(t *testing.T) fastLongChainConfig {
	t.Helper()
	sample := strings.TrimSpace(os.Getenv(fastLongChainSampleEnv))
	if sample == "" {
		sample = "smoke"
	}
	if sample != "smoke" && sample != "release" {
		t.Fatalf("%s must be smoke or release", fastLongChainSampleEnv)
	}
	repetitions := fastLongChainSmokeRepetitions
	if sample == "release" {
		repetitions = fastLongChainReleaseRepetitions
	}
	if raw := strings.TrimSpace(os.Getenv(fastLongChainRepsEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > fastLongChainReleaseRepetitions {
			t.Fatalf("%s must be an integer in [1,%d]", fastLongChainRepsEnv, fastLongChainReleaseRepetitions)
		}
		repetitions = value
	}
	if sample == "release" && repetitions != fastLongChainReleaseRepetitions {
		t.Fatalf("release sample requires %s=%d", fastLongChainRepsEnv, fastLongChainReleaseRepetitions)
	}

	endpoint := strings.TrimSpace(os.Getenv("SHANNON_E2E_ENDPOINT"))
	apiKey := strings.TrimSpace(os.Getenv("SHANNON_E2E_API_KEY"))
	loaded, err := config.Load()
	if err != nil && (endpoint == "" || apiKey == "") {
		t.Fatalf("load configured Cloud access: %v", err)
	}
	if loaded != nil {
		if endpoint == "" {
			endpoint = loaded.Endpoint
		}
		if apiKey == "" {
			apiKey = loaded.APIKey
		}
	}
	if endpoint == "" || apiKey == "" {
		t.Fatal("Fast long-chain lane needs SHANNON_E2E_ENDPOINT/SHANNON_E2E_API_KEY or configured Cloud credentials")
	}
	outputPath := strings.TrimSpace(os.Getenv(fastLongChainOutputEnv))
	if outputPath == "" {
		outputPath = filepath.Join(os.TempDir(), "kocoro-fast-long-chain-"+sample+".json")
	}
	return fastLongChainConfig{
		endpoint: endpoint, apiKey: apiKey, sample: sample,
		repetitions: repetitions, outputPath: outputPath,
	}
}

func runFastLongChainTrial(
	t *testing.T,
	parentCtx context.Context,
	llm *fastLongChainCacheClient,
	profile executionprofile.Profile,
	nonce []byte,
	trialID string,
	repetition int,
	requireReportedCost bool,
) fastLongChainTrial {
	t.Helper()
	probe := &fastLongChainProbeTool{nonce: append([]byte(nil), nonce...), target: fastLongChainSteps}
	registry := agent.NewToolRegistry()
	registry.Register(probe)
	loop := agent.NewAgentLoop(llm, registry, "medium", t.TempDir(), fastLongChainMaxIterations, 30_000, 200, nil, nil, nil)
	loop.SetCacheSource("fast_long_chain")
	loop.SetSkillDiscovery(false)
	loop.SetMaxTokens(fastLongChainMaxTokens)
	loop.SetTemperature(0)
	loop.SetKoeExecutionProfile(profile)

	ctx, cancel := context.WithTimeout(parentCtx, fastLongChainPerRunTimeout)
	defer cancel()
	started := time.Now()
	answer, usage, runErr := loop.Run(ctx,
		fmt.Sprintf("Call fast_long_chain_step exactly %d times in strict sequence. Start with step=1 and token=INIT. After every successful result, call only the returned next_step with the exact next_token; never guess, skip, retry, or batch steps. After CHAIN COMPLETE, stop calling tools and reply with the final_code.", fastLongChainSteps),
		nil, nil)
	status := loop.LastRunStatus()
	toolCalls, badCalls := probe.snapshot()
	trial := fastLongChainTrial{
		Repetition: repetition, TrialID: trialID,
		LatencyMillis: time.Since(started).Milliseconds(), ToolCalls: toolCalls, BadToolCalls: badCalls,
		Status: fastLongChainTrialStatus{
			Partial: status.Partial, FailureCode: string(status.FailureCode), LastTool: status.LastTool,
			RetryCount: status.RetryCount, IterationCount: status.IterationCount,
		},
		Answer: strings.TrimSpace(answer), Cache: llm.observation(),
	}
	if runErr != nil {
		trial.Error = runErr.Error()
		trial.Failures = append(trial.Failures, "provider_or_loop_error")
	}
	if usage != nil {
		trial.LLMCalls = usage.LLMCalls
		trial.InputTokens = usage.InputTokens
		trial.OutputTokens = usage.OutputTokens
		trial.TotalTokens = usage.TotalTokens
		trial.CostUSD = usage.CostUSD
		trial.CacheReadTokens = usage.CacheReadTokens
		trial.CacheCreateTokens = usage.CacheCreationTokens
		trial.UsageObserved = usage.LLMCalls > 0 && usage.TotalTokens > 0
		trial.CostObserved = usage.LLMCalls > 0 && usage.CostUSD > 0
	}
	if !requireReportedCost {
		trial.CostObserved = usage != nil
	}
	if toolCalls != fastLongChainSteps {
		trial.Failures = append(trial.Failures, fmt.Sprintf("tool_calls:%d_want_%d", toolCalls, fastLongChainSteps))
	}
	if badCalls != 0 {
		trial.Failures = append(trial.Failures, fmt.Sprintf("bad_tool_calls:%d", badCalls))
	}
	if status.Partial || status.FailureCode != runstatus.CodeNone {
		trial.Failures = append(trial.Failures, fmt.Sprintf("abnormal_status:partial=%t_code=%s", status.Partial, status.FailureCode))
	}
	if status.RetryCount != 0 {
		trial.Failures = append(trial.Failures, fmt.Sprintf("loop_retries:%d", status.RetryCount))
	}
	if status.IterationCount != fastLongChainExpectedIterations {
		trial.Failures = append(trial.Failures, fmt.Sprintf("iterations:%d_want_%d", status.IterationCount, fastLongChainExpectedIterations))
	}
	if status.LastTool != "fast_long_chain_step" {
		trial.Failures = append(trial.Failures, "last_tool_not_long_chain_probe")
	}
	if trial.LLMCalls != fastLongChainExpectedIterations {
		trial.Failures = append(trial.Failures, fmt.Sprintf("llm_calls:%d_want_%d", trial.LLMCalls, fastLongChainExpectedIterations))
	}
	if !strings.Contains(trial.Answer, fastLongChainFinalCode(nonce)) {
		trial.Failures = append(trial.Failures, "final_code_missing")
	}
	if !trial.UsageObserved {
		trial.Failures = append(trial.Failures, "usage_not_observed")
	}
	if requireReportedCost && !trial.CostObserved {
		trial.Failures = append(trial.Failures, "cost_not_observed")
	}
	if trial.CostUSD > fastLongChainPerRunMaxCostUSD {
		trial.Failures = append(trial.Failures, fmt.Sprintf("per_run_cost_exceeded:%.6f", trial.CostUSD))
	}
	if !trial.Cache.AllRequestsOff || trial.Cache.WholeResponseCached {
		trial.Failures = append(trial.Failures, "response_cache_not_isolated")
	}
	if !trial.Cache.ProfilePinned {
		trial.Failures = append(trial.Failures, "fast_profile_not_pinned")
	}
	if !trial.Cache.MaxTokensWithinCap {
		trial.Failures = append(trial.Failures, "max_tokens_not_within_cap")
	}
	if !trial.Cache.TemperatureZero {
		trial.Failures = append(trial.Failures, "temperature_not_zero")
	}
	if trial.Cache.Requests != fastLongChainExpectedIterations || trial.Cache.Responses != fastLongChainExpectedIterations {
		trial.Failures = append(trial.Failures, fmt.Sprintf(
			"cache_observation_count:%d/%d_want_%d",
			trial.Cache.Requests, trial.Cache.Responses, fastLongChainExpectedIterations,
		))
	}
	trial.Failures = fastLongChainUniqueStrings(trial.Failures)
	trial.Correct = len(trial.Failures) == 0
	return trial
}

func newFastLongChainReport(
	cfg fastLongChainConfig,
	profile executionprofile.Profile,
	trials []fastLongChainTrial,
) fastLongChainReport {
	report := fastLongChainReport{
		SchemaVersion: "kocoro.fast_long_chain.v1", GeneratedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Sample: cfg.sample, Repetitions: cfg.repetitions,
		MinimumReleaseRepetitions: fastLongChainReleaseRepetitions,
		Scheduled:                 cfg.repetitions, Completed: len(trials),
		TargetToolSteps: fastLongChainSteps, ExpectedIterations: fastLongChainExpectedIterations,
		MaxIterations: fastLongChainMaxIterations, MaxTokens: fastLongChainMaxTokens,
		PerRunTimeoutSeconds: int(fastLongChainPerRunTimeout.Seconds()),
		SuiteTimeoutSeconds:  int(fastLongChainSuiteTimeout.Seconds()),
		PerRunMaxCostUSD:     fastLongChainPerRunMaxCostUSD, SuiteMaxCostUSD: fastLongChainSuiteMaxCostUSD,
		FastProfileObserved: profile.ValidateFast() == nil,
		FastProfileName:     profile.ProfileName, FastProfileVersion: profile.ProfileVersion, FastModel: profile.Model,
		Trials: append([]fastLongChainTrial(nil), trials...),
		CoverageBoundaries: []string{
			"Exercises the production AgentLoop with one non-GUI, in-memory BoundedProgressTool and a sealed Fast profile.",
			"Isolates long sequential tool-loop mechanics; it does not exercise GUI budget elevation, compaction, daemon routing, Desktop rendering, or external side effects.",
			"A passing synthetic chain is necessary mechanism evidence, not sufficient general-purpose Fast quality evidence.",
		},
	}
	report.Complete = report.Completed == report.Scheduled
	report.UsageObserved = len(trials) > 0
	report.CostObserved = len(trials) > 0
	report.CacheIsolationObserved = len(trials) > 0
	allCorrect := len(trials) > 0
	for _, trial := range trials {
		report.ReportedCostUSD += trial.CostUSD
		if trial.Correct {
			report.CorrectRuns++
		} else {
			allCorrect = false
		}
		report.UsageObserved = report.UsageObserved && trial.UsageObserved
		report.CostObserved = report.CostObserved && trial.CostObserved
		report.CacheIsolationObserved = report.CacheIsolationObserved &&
			trial.Cache.AllRequestsOff && !trial.Cache.WholeResponseCached
		report.FastProfileObserved = report.FastProfileObserved && trial.Cache.ProfilePinned
		if trial.CostUSD > fastLongChainPerRunMaxCostUSD {
			report.QualificationFailures = append(report.QualificationFailures,
				fmt.Sprintf("per_run_cost_exceeded:rep_%d:%.6f", trial.Repetition, trial.CostUSD))
		}
		for _, failure := range trial.Failures {
			report.QualificationFailures = append(report.QualificationFailures,
				fmt.Sprintf("rep_%d:%s", trial.Repetition, failure))
		}
	}
	if !report.Complete {
		report.QualificationFailures = append(report.QualificationFailures,
			fmt.Sprintf("incomplete:%d/%d", report.Completed, report.Scheduled))
	}
	if report.ReportedCostUSD > fastLongChainSuiteMaxCostUSD {
		report.QualificationFailures = append(report.QualificationFailures,
			fmt.Sprintf("suite_cost_exceeded:%.6f", report.ReportedCostUSD))
	}
	if !report.FastProfileObserved {
		report.QualificationFailures = append(report.QualificationFailures, "fast_profile_invalid")
	}
	if !report.UsageObserved {
		report.QualificationFailures = append(report.QualificationFailures, "usage_not_observed")
	}
	if !report.CostObserved {
		report.QualificationFailures = append(report.QualificationFailures, "cost_not_observed")
	}
	if !report.CacheIsolationObserved {
		report.QualificationFailures = append(report.QualificationFailures, "cache_isolation_not_observed")
	}
	report.QualificationFailures = fastLongChainUniqueStrings(report.QualificationFailures)
	report.MechanismQualifying = report.Complete && allCorrect &&
		report.UsageObserved && report.CostObserved && report.CacheIsolationObserved &&
		report.FastProfileObserved && report.ReportedCostUSD <= fastLongChainSuiteMaxCostUSD &&
		len(report.QualificationFailures) == 0
	report.MechanismReleaseQualifying = cfg.sample == "release" &&
		cfg.repetitions == fastLongChainReleaseRepetitions && report.MechanismQualifying
	return report
}

func fastLongChainTotalCost(trials []fastLongChainTrial) float64 {
	total := 0.0
	for _, trial := range trials {
		total += trial.CostUSD
	}
	return total
}

func fastLongChainUniqueStrings(values []string) []string {
	seen := make(map[string]bool, len(values))
	unique := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		unique = append(unique, value)
	}
	return unique
}

func writeFastLongChainReport(path string, report fastLongChainReport) error {
	if strings.TrimSpace(path) == "" {
		return fmt.Errorf("report path is required")
	}
	if math.IsNaN(report.ReportedCostUSD) || math.IsInf(report.ReportedCostUSD, 0) {
		return fmt.Errorf("reported cost is not finite")
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return agentsapi.AtomicWrite(path, body)
}
