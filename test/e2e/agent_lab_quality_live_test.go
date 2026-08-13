//go:build live

package e2e

import (
	"context"
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
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

type agentLabQualityCacheOffClient struct {
	inner client.LLMClient

	mu       sync.Mutex
	policies []executionprofile.ResponseCachePolicy
}

func (c *agentLabQualityCacheOffClient) Complete(
	ctx context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	c.record(req.ResponseCachePolicy)
	return c.inner.Complete(ctx, req)
}

func (c *agentLabQualityCacheOffClient) CompleteStream(
	ctx context.Context,
	req client.CompletionRequest,
	onDelta func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	req.ResponseCachePolicy = executionprofile.ResponseCacheOff
	c.record(req.ResponseCachePolicy)
	return c.inner.CompleteStream(ctx, req, onDelta)
}

func (c *agentLabQualityCacheOffClient) record(policy executionprofile.ResponseCachePolicy) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.policies = append(c.policies, policy)
}

func (c *agentLabQualityCacheOffClient) allRequestsCacheOff() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.policies) == 0 {
		return false
	}
	for _, policy := range c.policies {
		if policy != executionprofile.ResponseCacheOff {
			return false
		}
	}
	return true
}

type agentLabBoundedResearchTool struct {
	mu    sync.Mutex
	calls int
}

type agentLabDeferredBashTool struct {
	mu    sync.Mutex
	calls int
}

func (*agentLabDeferredBashTool) Info() agent.ToolInfo {
	return (&tools.BashTool{}).Info()
}

func (*agentLabDeferredBashTool) ToolExposure() agent.ToolExposure {
	return agent.ToolExposureDeferred
}

func (t *agentLabDeferredBashTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if result, valid := agent.ValidateToolArguments(t.Info(), argsJSON); !valid {
		return result, nil
	}
	var args struct {
		Command     string `json:"command"`
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil {
		return agent.ValidationError(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if !strings.Contains(args.Command, "700") || !strings.Contains(args.Command, "31") ||
		!strings.Contains(args.Command, "AGENT_LAB_DEFERRED_") {
		return agent.ValidationError("bash command must calculate 700+31 and print the requested marker"), nil
	}
	return agent.ToolResult{Content: agentLabQualityDeferredMarker}, nil
}

func (*agentLabDeferredBashTool) RequiresApproval() bool            { return false }
func (*agentLabDeferredBashTool) IsReadOnlyCall(string) bool        { return true }
func (*agentLabDeferredBashTool) IsConcurrencySafeCall(string) bool { return true }

func (t *agentLabDeferredBashTool) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func (*agentLabBoundedResearchTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "bounded_research",
		Description: "Read the one deterministic, bounded research record for this quality gate. Call exactly once when requested.",
		Parameters: map[string]any{
			"type":       "object",
			"properties": map[string]any{},
		},
	}
}

func (t *agentLabBoundedResearchTool) Run(_ context.Context, _ string) (agent.ToolResult, error) {
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return agent.ToolResult{Content: `{"url":"` + agentLabQualityResearchURL + `","fact":"IANA maintains example.com and related domains for documentation purposes."}`}, nil
}

func (*agentLabBoundedResearchTool) RequiresApproval() bool            { return false }
func (*agentLabBoundedResearchTool) IsReadOnlyCall(string) bool        { return true }
func (*agentLabBoundedResearchTool) IsConcurrencySafeCall(string) bool { return true }

func (t *agentLabBoundedResearchTool) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

func TestLive_AgentLabGeneralPurposeQuality(t *testing.T) {
	if os.Getenv(agentLabQualityGateEnv) != "1" {
		t.Skip("set KOCORO_AGENT_LAB_QUALITY_LIVE=1 to authorize the paid quality gate")
	}
	if os.Getenv("SHANNON_E2E_LIVE") != "1" {
		t.Fatal("also set SHANNON_E2E_LIVE=1 to authorize real provider calls")
	}
	cfg := loadAgentLabQualityConfig(t)
	cases := agentLabQualityCases()
	jobs := buildAgentLabQualityJobs(len(cases), cfg.repetitions, cfg.seed)
	provider := client.NewGatewayClient(cfg.endpoint, cfg.apiKey)
	results := make([]agentLabQualityRun, 0, len(jobs))

	for index, job := range jobs {
		result := runAgentLabQualityCase(t, provider, cfg, cases[job.caseIndex], job.repetition, index+1)
		results = append(results, result)
		t.Logf("agent_lab_quality case=%s rep=%d correct=%t failures=%v latency_ms=%d llm_calls=%d input=%d output=%d cost_usd=%.8f research_calls=%d tool_search_calls=%d deferred_bash_calls=%d",
			result.Case, result.Repetition, result.Correct, result.Failures,
			result.LatencyMillis, result.LLMCalls, result.InputTokens,
			result.OutputTokens, result.CostUSD, result.ResearchToolCalls,
			result.ToolSearchCalls, result.DeferredBashCalls)
		if agentLabQualityReportedCost(results) > cfg.maxCostUSD {
			break
		}
	}

	report := newAgentLabQualityReport(cfg, jobs, results)
	if err := writeAgentLabQualityJSON(cfg.outputPath, report); err != nil {
		t.Fatalf("write AgentLab quality report: %v", err)
	}
	if !report.Complete {
		t.Fatalf("quality gate stopped before completion: completed=%d scheduled=%d cost_usd=%.8f max_cost_usd=%.8f report=%s",
			report.Completed, report.Scheduled, report.ReportedCostUSD, report.MaxCostUSD, cfg.outputPath)
	}
	if report.CorrectRuns != report.Completed {
		t.Fatalf("quality gate failed: correct=%d completed=%d failures=%v report=%s",
			report.CorrectRuns, report.Completed, report.Failures, cfg.outputPath)
	}
	if !report.ComparisonQualifying {
		t.Fatalf("quality comparison did not qualify: usage_observed=%t cost_observed=%t cost_usd=%.8f max_cost_usd=%.8f repetitions=%d report=%s",
			report.UsageObserved, report.CostObserved, report.ReportedCostUSD,
			report.MaxCostUSD, report.RepetitionsPerCase, cfg.outputPath)
	}
	if cfg.sample == "release" && !report.ReleaseQualifying {
		t.Fatalf("quality release did not qualify: repetitions=%d minimum=%d report=%s",
			report.RepetitionsPerCase, report.MinimumReleaseRepetitions, cfg.outputPath)
	}
	t.Logf("agent_lab_quality complete runs=%d repetitions=%d comparison_qualifying=%t release_qualifying=%t p50_ms=%d p95_ms=%d p99_ms=%d cost_usd=%.8f report=%s",
		report.Completed, report.RepetitionsPerCase, report.ComparisonQualifying,
		report.ReleaseQualifying, report.LatencyP50Millis, report.LatencyP95Millis,
		report.LatencyP99Millis, report.ReportedCostUSD, cfg.outputPath)
}

func loadAgentLabQualityConfig(t *testing.T) agentLabQualityConfig {
	t.Helper()
	endpoint := strings.TrimSpace(os.Getenv("SHANNON_E2E_ENDPOINT"))
	apiKey := strings.TrimSpace(os.Getenv("SHANNON_E2E_API_KEY"))
	modelTier := "medium"
	specificModel := ""
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
		if loaded.ModelTier != "" {
			modelTier = loaded.ModelTier
		}
		specificModel = loaded.Agent.Model
	}
	if endpoint == "" || apiKey == "" {
		t.Fatal("quality gate needs SHANNON_E2E_ENDPOINT/SHANNON_E2E_API_KEY or configured Cloud credentials; set KOCORO_FORCE_KEYCHAIN_HYDRATE=1 to authorize test-only credential hydration")
	}
	repetitions := agentLabQualityComparisonRepetitions
	if raw := strings.TrimSpace(os.Getenv(agentLabQualityRepetitionsEnv)); raw != "" {
		value, parseErr := strconv.Atoi(raw)
		if parseErr != nil || value < 1 || value > 100 {
			t.Fatalf("%s must be an integer in [1,100]", agentLabQualityRepetitionsEnv)
		}
		repetitions = value
	}
	sample := strings.TrimSpace(os.Getenv(agentLabQualitySampleEnv))
	if sample == "" {
		sample = "smoke"
	}
	if sample != "smoke" && sample != "release" {
		t.Fatalf("%s must be smoke or release", agentLabQualitySampleEnv)
	}
	if sample == "release" && repetitions < agentLabQualityReleaseRepetitions {
		t.Fatalf("release quality sample requires %s >= %d", agentLabQualityRepetitionsEnv, agentLabQualityReleaseRepetitions)
	}
	seed := agentLabQualityDefaultSeed
	if raw := strings.TrimSpace(os.Getenv(agentLabQualitySeedEnv)); raw != "" {
		value, parseErr := strconv.ParseInt(raw, 10, 64)
		if parseErr != nil {
			t.Fatalf("%s must be a signed 64-bit integer", agentLabQualitySeedEnv)
		}
		seed = value
	}
	maxCostUSD := 5.0
	if raw := strings.TrimSpace(os.Getenv(agentLabQualityMaxCostEnv)); raw != "" {
		value, parseErr := strconv.ParseFloat(raw, 64)
		if parseErr != nil || math.IsNaN(value) || math.IsInf(value, 0) || value <= 0 || value > 50 {
			t.Fatalf("%s must be greater than 0 and at most 50", agentLabQualityMaxCostEnv)
		}
		maxCostUSD = value
	}
	outputPath := strings.TrimSpace(os.Getenv(agentLabQualityOutputEnv))
	if outputPath == "" {
		outputPath = filepath.Join(os.TempDir(), fmt.Sprintf("kocoro-agent-lab-quality-%d.json", seed))
	}
	return agentLabQualityConfig{
		endpoint: endpoint, apiKey: apiKey, modelTier: modelTier,
		specificModel: specificModel, outputPath: outputPath, sample: sample,
		repetitions: repetitions, seed: seed, maxCostUSD: maxCostUSD,
	}
}

func runAgentLabQualityCase(
	t *testing.T,
	provider client.LLMClient,
	cfg agentLabQualityConfig,
	tc agentLabQualityCase,
	repetition int,
	scheduleIndex int,
) agentLabQualityRun {
	t.Helper()
	wrapped := &agentLabQualityCacheOffClient{inner: provider}
	registry := agent.NewToolRegistry()
	var researchTool *agentLabBoundedResearchTool
	var deferredBash *agentLabDeferredBashTool
	var questionAsker *agentLabQuestionAsker
	var stepProbe *agentLabStepProbeTool
	var blockedFetch *agentLabBlockedWebFetchTool
	if tc.research {
		researchTool = &agentLabBoundedResearchTool{}
		registry.Register(researchTool)
	}
	if tc.deferred {
		deferredBash = &agentLabDeferredBashTool{}
		registry.Register(deferredBash)
	}
	if tc.question {
		questionAsker = &agentLabQuestionAsker{}
		registry.Register(&tools.AskUserQuestionTool{})
	}
	if tc.progress {
		stepProbe = &agentLabStepProbeTool{}
		registry.Register(stepProbe)
	}
	if tc.webEmpty {
		blockedFetch = &agentLabBlockedWebFetchTool{}
		registry.Register(blockedFetch)
	}
	loop := agent.NewAgentLoop(wrapped, registry, cfg.modelTier, t.TempDir(), 5, 30_000, 200, nil, nil, nil)
	loop.SetCacheSource("agent_lab_quality")
	loop.SetSkillDiscovery(false)
	loop.SetMaxTokens(600)
	loop.SetTemperature(0)
	if tc.source != "" {
		loop.SetSource(tc.source)
	}
	if cfg.specificModel != "" {
		loop.SetSpecificModel(cfg.specificModel)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	if questionAsker != nil {
		ctx = agent.WithQuestionAsker(ctx, questionAsker)
	}
	started := time.Now()
	answer, usage, err := loop.Run(ctx, tc.prompt, nil, nil)
	result := agentLabQualityRun{
		Case: tc.name, Repetition: repetition, ScheduleIndex: scheduleIndex,
		LatencyMillis: time.Since(started).Milliseconds(), Answer: strings.TrimSpace(answer),
		ResponseCachePolicy: string(executionprofile.ResponseCacheOff),
	}
	if researchTool != nil {
		result.ResearchToolCalls = researchTool.count()
	}
	if deferredBash != nil {
		result.DeferredBashCalls = deferredBash.count()
		result.ToolSearchCalls = countAgentLabQualityToolUses(loop.RunMessages(), "tool_search")
	}
	if err != nil {
		result.Failures = append(result.Failures, "provider_or_loop_error:"+err.Error())
	}
	if usage != nil {
		result.LLMCalls = usage.LLMCalls
		result.InputTokens = usage.InputTokens
		result.OutputTokens = usage.OutputTokens
		result.TotalTokens = usage.TotalTokens
		result.CostUSD = usage.CostUSD
		result.UsageObserved = usage.LLMCalls > 0 && usage.TotalTokens > 0
		result.CostObserved = usage.LLMCalls > 0 && usage.CostUSD > 0
	}
	if !wrapped.allRequestsCacheOff() {
		result.Failures = append(result.Failures, "response_cache_policy_not_off")
	}
	switch {
	case tc.deferred:
		result.Failures = append(result.Failures, validateAgentLabQualityDeferred(
			result.Answer, result.ToolSearchCalls, result.DeferredBashCalls)...)
	case tc.question:
		result.Failures = append(result.Failures, validateAgentLabQualityQuestion(
			result.Answer, questionAsker.snapshot())...)
	case tc.progress:
		result.Failures = append(result.Failures, validateAgentLabQualityProgress(
			result.Answer, countAgentLabMidTurnNotes(loop.RunMessages()), stepProbe.count())...)
	case tc.webEmpty:
		result.Failures = append(result.Failures, validateAgentLabQualityWebHonesty(
			result.Answer, blockedFetch.count())...)
	default:
		result.Failures = append(result.Failures, tc.validate(result.Answer, result.ResearchToolCalls)...)
	}
	result.Failures = uniqueAgentLabQualityFailures(result.Failures)
	result.Correct = len(result.Failures) == 0
	return result
}

// ---- Behavior-contract cases restored with the layered prompt (2026-08-07) ----
// Each traces to a clause the prompt rework had dropped: the structured-question
// MUST gate + placeholder-option ban, the mid-task progress-update rule, and
// web empty-result honesty. Unlike the instruction-following cases above, these
// prompts do NOT tell the model which behavior the validator checks — the
// behavior must come from the system prompt clause itself.

type agentLabQuestionAsker struct {
	mu       sync.Mutex
	requests []agent.UIQuestionRequest
}

func (a *agentLabQuestionAsker) AskUserQuestion(_ context.Context, req agent.UIQuestionRequest) agent.UIQuestionResult {
	a.mu.Lock()
	a.requests = append(a.requests, req)
	a.mu.Unlock()
	answers := make([]agent.UIQuestionAnswer, 0, len(req.Questions))
	for _, q := range req.Questions {
		value := "OK"
		if len(q.Options) > 0 {
			value = q.Options[0].Label
		}
		answers = append(answers, agent.UIQuestionAnswer{Question: q.Question, Values: []string{value}})
	}
	return agent.UIQuestionResult{Action: agent.QuestionActionAnswer, Answers: answers}
}

func (a *agentLabQuestionAsker) snapshot() []agent.UIQuestionRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]agent.UIQuestionRequest(nil), a.requests...)
}

// agentLabStepProbeTool is a deterministic three-step task: each call reports
// the step it completed and what remains. It gives the model a genuinely
// multi-step trajectory without prescribing any communication behavior.
type agentLabStepProbeTool struct {
	mu    sync.Mutex
	calls int
}

func (t *agentLabStepProbeTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "step_probe",
		Description: "Advance the three-step probe task by one step. Call with step=1, then 2, then 3.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"step": map[string]any{"type": "integer", "description": "Step number to run: 1, 2, or 3."},
			},
		},
		Required: []string{"step"},
	}
}

func (t *agentLabStepProbeTool) RequiresApproval() bool     { return false }
func (t *agentLabStepProbeTool) IsReadOnlyCall(string) bool { return false }

func (t *agentLabStepProbeTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		Step int `json:"step"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || args.Step < 1 || args.Step > 3 {
		return agent.ValidationError("step must be 1, 2, or 3"), nil
	}
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	if args.Step < 3 {
		return agent.ToolResult{Content: fmt.Sprintf("STEP-%d COMPLETE. Remaining steps: %d. Call step_probe with step=%d next.", args.Step, 3-args.Step, args.Step+1)}, nil
	}
	return agent.ToolResult{Content: "STEP-3 COMPLETE. All steps finished. Completion marker: PROBE-DONE-731."}, nil
}

func (t *agentLabStepProbeTool) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}

// agentLabBlockedWebFetchTool simulates the anti-bot/empty-page failure mode
// the Web Results honesty clause was tuned against. Every fetch is blocked.
type agentLabBlockedWebFetchTool struct {
	mu    sync.Mutex
	calls int
}

func (t *agentLabBlockedWebFetchTool) Info() agent.ToolInfo {
	return agent.ToolInfo{
		Name:        "web_fetch",
		Description: "Fetch a web page by URL and return its textual content.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"url": map[string]any{"type": "string", "description": "Absolute URL to fetch."},
			},
		},
		Required: []string{"url"},
	}
}

func (t *agentLabBlockedWebFetchTool) RequiresApproval() bool     { return false }
func (t *agentLabBlockedWebFetchTool) IsReadOnlyCall(string) bool { return true }

func (t *agentLabBlockedWebFetchTool) Run(_ context.Context, argsJSON string) (agent.ToolResult, error) {
	var args struct {
		URL string `json:"url"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &args); err != nil || strings.TrimSpace(args.URL) == "" {
		return agent.ValidationError("url is required"), nil
	}
	t.mu.Lock()
	t.calls++
	t.mu.Unlock()
	return agent.ToolResult{Content: "HTTP 403 Forbidden\nAccess denied: automated-traffic challenge (captcha). No page content is available.", IsError: true}, nil
}

func (t *agentLabBlockedWebFetchTool) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.calls
}
