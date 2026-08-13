//go:build live

package e2e

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
	"github.com/Kocoro-lab/ShanClaw/internal/tools"
)

const (
	agentLabQualityGateEnv        = "KOCORO_AGENT_LAB_QUALITY_LIVE"
	agentLabQualityOutputEnv      = "KOCORO_AGENT_LAB_QUALITY_OUTPUT"
	agentLabQualityRepetitionsEnv = "KOCORO_AGENT_LAB_QUALITY_REPETITIONS"
	agentLabQualitySeedEnv        = "KOCORO_AGENT_LAB_QUALITY_SEED"
	agentLabQualitySampleEnv      = "KOCORO_AGENT_LAB_QUALITY_SAMPLE"
	agentLabQualityMaxCostEnv     = "KOCORO_AGENT_LAB_QUALITY_MAX_COST_USD"

	agentLabQualityComparisonRepetitions = 3
	// Release threshold halved from 30 on 2026-08-10 (user decision):
	// 15 all-pass bounds the per-cell failure rate under ~18% (95% CI)
	// and pooled arms far tighter; 30 doubled the cost for little
	// marginal evidence. Comparison runs stay at 3-5.
	agentLabQualityReleaseRepetitions = 15
	agentLabQualityDefaultSeed        = int64(20260807)
	agentLabQualityResearchURL        = "https://www.iana.org/help/example-domains"
	agentLabQualityDeferredMarker     = "AGENT_LAB_DEFERRED_731"
)

type agentLabQualityConfig struct {
	endpoint      string
	apiKey        string
	modelTier     string
	specificModel string
	outputPath    string
	sample        string
	repetitions   int
	seed          int64
	maxCostUSD    float64
}

type agentLabQualityCase struct {
	name     string
	prompt   string
	source   string
	research bool
	deferred bool
	question bool
	progress bool
	webEmpty bool
	validate func(string, int) []string
}

type agentLabQualityJob struct {
	caseIndex  int
	repetition int
}

type agentLabQualityRun struct {
	Case                string   `json:"case"`
	Repetition          int      `json:"repetition"`
	ScheduleIndex       int      `json:"schedule_index"`
	Correct             bool     `json:"correct"`
	Failures            []string `json:"failures"`
	LatencyMillis       int64    `json:"latency_millis"`
	LLMCalls            int      `json:"llm_calls"`
	InputTokens         int      `json:"input_tokens"`
	OutputTokens        int      `json:"output_tokens"`
	TotalTokens         int      `json:"total_tokens"`
	CostUSD             float64  `json:"cost_usd"`
	UsageObserved       bool     `json:"usage_observed"`
	CostObserved        bool     `json:"cost_observed"`
	ResearchToolCalls   int      `json:"research_tool_calls"`
	ToolSearchCalls     int      `json:"tool_search_calls"`
	DeferredBashCalls   int      `json:"deferred_bash_calls"`
	ResponseCachePolicy string   `json:"response_cache_policy"`
	Answer              string   `json:"answer"`
}

type agentLabQualityCaseSummary struct {
	Case              string  `json:"case"`
	Runs              int     `json:"runs"`
	CorrectRuns       int     `json:"correct_runs"`
	CorrectnessRate   float64 `json:"correctness_rate"`
	LatencyP50Millis  int64   `json:"latency_p50_millis"`
	LatencyP95Millis  int64   `json:"latency_p95_millis"`
	LatencyP99Millis  int64   `json:"latency_p99_millis"`
	InputTokensTotal  int     `json:"input_tokens_total"`
	OutputTokensTotal int     `json:"output_tokens_total"`
	CostUSDTotal      float64 `json:"cost_usd_total"`
}

type agentLabQualityFailure struct {
	Case          string `json:"case"`
	Repetition    int    `json:"repetition"`
	ScheduleIndex int    `json:"schedule_index"`
	Class         string `json:"class"`
}

type agentLabQualityReport struct {
	SchemaVersion                string                       `json:"schema_version"`
	GeneratedAt                  string                       `json:"generated_at"`
	Complete                     bool                         `json:"complete"`
	Sample                       string                       `json:"sample"`
	RepetitionsPerCase           int                          `json:"repetitions_per_case"`
	MinimumComparisonRepetitions int                          `json:"minimum_comparison_repetitions"`
	MinimumReleaseRepetitions    int                          `json:"minimum_release_repetitions"`
	Seed                         int64                        `json:"seed"`
	Randomized                   bool                         `json:"randomized"`
	Scheduled                    int                          `json:"scheduled"`
	Completed                    int                          `json:"completed"`
	CorrectRuns                  int                          `json:"correct_runs"`
	CorrectnessRate              float64                      `json:"correctness_rate"`
	ComparisonQualifying         bool                         `json:"comparison_qualifying"`
	ReleaseQualifying            bool                         `json:"release_qualifying"`
	UsageObserved                bool                         `json:"usage_observed"`
	CostObserved                 bool                         `json:"cost_observed"`
	LatencyP50Millis             int64                        `json:"latency_p50_millis"`
	LatencyP95Millis             int64                        `json:"latency_p95_millis"`
	LatencyP99Millis             int64                        `json:"latency_p99_millis"`
	InputTokensTotal             int                          `json:"input_tokens_total"`
	OutputTokensTotal            int                          `json:"output_tokens_total"`
	TotalTokens                  int                          `json:"total_tokens"`
	ReportedCostUSD              float64                      `json:"reported_cost_usd"`
	MaxCostUSD                   float64                      `json:"max_cost_usd"`
	Runs                         []agentLabQualityRun         `json:"runs"`
	Cases                        []agentLabQualityCaseSummary `json:"cases"`
	Failures                     []agentLabQualityFailure     `json:"failures"`
	CoverageBoundaries           []string                     `json:"coverage_boundaries"`
}

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

func TestOffline_AgentLabQualityContractValidators(t *testing.T) {
	cases := agentLabQualityCases()
	passing := map[string]struct {
		answer    string
		toolCalls int
	}{
		"zh_two_sentence_email": {
			answer: "王经理，因发烧我明天请假一天。交接清单已放在团队文档，感谢理解。",
		},
		"notes_summary": {
			answer: "决定：Project Argo 采用蓝色包装，预算上限为 48,000 美元。\n风险：传感器延迟 6 天。\n行动：Lin 在 2026-09-18 进行下一次检查；会议时间为 2026-09-14 09:30 JST。",
		},
		"deadline_plan": {
			answer: "Draft | Wed 09:00-11:00\nReview | Thu 14:00-15:00\nFinal | Fri 15:00-15:30",
		},
		"voice_style": {
			answer: "先确认预算，再联系供应商。",
		},
		"bounded_research": {
			answer:    "example.com 由 IANA 保留用于文档示例：[" + agentLabQualityResearchURL + "](" + agentLabQualityResearchURL + ")",
			toolCalls: 1,
		},
		"deferred_automation": {
			answer:    agentLabQualityDeferredMarker,
			toolCalls: 1,
		},
	}
	for _, tc := range cases {
		if tc.question || tc.progress || tc.webEmpty {
			continue // pinned separately below with their capture-shaped inputs
		}
		fixture := passing[tc.name]
		if tc.deferred {
			if failures := validateAgentLabQualityDeferred(fixture.answer, 1, fixture.toolCalls); len(failures) != 0 {
				t.Errorf("passing %s failed: %v", tc.name, failures)
			}
			continue
		}
		if failures := tc.validate(fixture.answer, fixture.toolCalls); len(failures) != 0 {
			t.Errorf("passing %s failed: %v", tc.name, failures)
		}
	}

	failing := map[string]struct {
		answer    string
		toolCalls int
	}{
		"zh_two_sentence_email": {answer: "邮件已发送给王经理。"},
		"notes_summary":         {answer: "决定：Project Argo 改用红色包装。"},
		"deadline_plan":         {answer: "Draft | Thu 09:00-11:00\nReview | Fri 14:00-15:00\nFinal | Sat 15:00-15:30"},
		"voice_style":           {answer: "我建议你首先应该花一些时间仔细确认预算，因为这样比较稳妥。"},
		"bounded_research":      {answer: "凭记忆看是文档用途。", toolCalls: 0},
		"deferred_automation":   {answer: agentLabQualityDeferredMarker, toolCalls: 0},
	}
	for _, tc := range cases {
		if tc.question || tc.progress || tc.webEmpty {
			continue // pinned separately below with their capture-shaped inputs
		}
		fixture := failing[tc.name]
		if tc.deferred {
			if failures := validateAgentLabQualityDeferred(fixture.answer, 0, fixture.toolCalls); len(failures) == 0 {
				t.Errorf("failing %s unexpectedly passed", tc.name)
			}
			continue
		}
		if failures := tc.validate(fixture.answer, fixture.toolCalls); len(failures) == 0 {
			t.Errorf("failing %s unexpectedly passed", tc.name)
		}
	}

	// Behavior-contract validator pins (capture-shaped inputs).
	goodQuestion := []agent.UIQuestionRequest{{Questions: []agent.UIQuestion{{
		Question: "每周例会定在哪个时间？",
		Options: []agent.UIQuestionOption{
			{Label: "周一 10:00"}, {Label: "周三 15:00"}, {Label: "周五 09:30"},
		},
	}}}}
	if failures := validateAgentLabQualityQuestion("已确认：每周例会定在周一 10:00。", goodQuestion); len(failures) != 0 {
		t.Errorf("passing structured_question_fork failed: %v", failures)
	}
	if failures := validateAgentLabQualityQuestion("你想选哪一个？请告诉我。", nil); len(failures) == 0 {
		t.Error("prose-only question flow unexpectedly passed")
	}
	badOptions := []agent.UIQuestionRequest{{Questions: []agent.UIQuestion{{
		Question: "选哪个？",
		Options:  []agent.UIQuestionOption{{Label: "周一 10:00"}, {Label: "其他（自定义）"}},
	}}}}
	if failures := validateAgentLabQualityQuestion("已确认。", badOptions); !slices.ContainsFunc(failures, func(f string) bool {
		return strings.HasPrefix(f, "placeholder_option:")
	}) {
		t.Errorf("placeholder option was not rejected: %v", failures)
	}
	if failures := validateAgentLabQualityProgress("PROBE-DONE-731，三步全部完成。", 2, 3); len(failures) != 0 {
		t.Errorf("passing progress_updates failed: %v", failures)
	}
	if failures := validateAgentLabQualityProgress("PROBE-DONE-731", 0, 3); !slices.Contains(failures, "silent_until_final_answer") {
		t.Errorf("silent run was not rejected: %v", failures)
	}
	if failures := validateAgentLabQualityWebHonesty("页面被反爬拦截（403），无法获取价格，建议提供其他来源。", 1); len(failures) != 0 {
		t.Errorf("passing web_empty_honesty failed: %v", failures)
	}
	if failures := validateAgentLabQualityWebHonesty("企业版每月 49 美元。", 1); !slices.Contains(failures, "invented_price_from_blocked_page") {
		t.Errorf("invented price was not rejected: %v", failures)
	}
	if failures := validateAgentLabQualityResearch(
		"IANA 文档用途："+agentLabQualityResearchURL+" https://unexpected.test/source",
		1,
	); !slices.Contains(failures, "research_unexpected_source") {
		t.Fatalf("different research URL was not rejected: %v", failures)
	}
}

func TestOffline_AgentLabQualityQualificationFailsClosed(t *testing.T) {
	cfg := agentLabQualityConfig{sample: "smoke", repetitions: 3, seed: 1, maxCostUSD: 5}
	jobs := buildAgentLabQualityJobs(1, cfg.repetitions, cfg.seed)
	results := make([]agentLabQualityRun, len(jobs))
	for index := range results {
		results[index] = agentLabQualityRun{
			Case: "fixture", Repetition: index + 1, ScheduleIndex: index + 1,
			Correct: true, LatencyMillis: int64(100 + index), LLMCalls: 1,
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
			CostUSD: 0.001, UsageObserved: true, CostObserved: true,
			ResponseCachePolicy: string(executionprofile.ResponseCacheOff),
		}
	}
	report := newAgentLabQualityReport(cfg, jobs, results)
	if !report.ComparisonQualifying || report.ReleaseQualifying {
		t.Fatalf("three-repetition report qualification=%+v", report)
	}

	cfg.repetitions = agentLabQualityReleaseRepetitions - 1
	jobs = buildAgentLabQualityJobs(1, cfg.repetitions, cfg.seed)
	results = repeatAgentLabQualityFixture(jobs)
	if report := newAgentLabQualityReport(cfg, jobs, results); report.ReleaseQualifying {
		t.Fatalf("%d repetitions unexpectedly release-qualified", agentLabQualityReleaseRepetitions-1)
	}

	cfg.sample = "release"
	cfg.repetitions = agentLabQualityReleaseRepetitions
	jobs = buildAgentLabQualityJobs(1, cfg.repetitions, cfg.seed)
	results = repeatAgentLabQualityFixture(jobs)
	report = newAgentLabQualityReport(cfg, jobs, results)
	if !report.ReleaseQualifying {
		t.Fatalf("%d complete passing repetitions did not release-qualify: %+v", agentLabQualityReleaseRepetitions, report)
	}
	costLimited := cfg
	costLimited.maxCostUSD = report.ReportedCostUSD / 2
	if overBudget := newAgentLabQualityReport(costLimited, jobs, results); overBudget.ComparisonQualifying || overBudget.ReleaseQualifying {
		t.Fatalf("over-budget report unexpectedly qualified: %+v", overBudget)
	}
	results[0].Correct = false
	results[0].Failures = []string{"fixture_failure"}
	if report := newAgentLabQualityReport(cfg, jobs, results); report.ReleaseQualifying {
		t.Fatal("incorrect release sample unexpectedly qualified")
	}
}

func TestOffline_AgentLabQualityLaneRequiresExplicitPaidGate(t *testing.T) {
	command := exec.Command(filepath.Join(repoRoot(), "scripts", "agent-lab.sh"), t.TempDir())
	command.Env = append(os.Environ(),
		"AGENT_LAB_LANE=quality_live",
		"KOCORO_AGENT_LAB_QUALITY_LIVE=",
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("quality lane unexpectedly ran without its paid gate")
	}
	if !strings.Contains(string(output), "KOCORO_AGENT_LAB_QUALITY_LIVE=1") {
		t.Fatalf("quality lane gate error is not actionable: %s", output)
	}
}

func TestOffline_AgentLabQualityLaneRejectsUndersizedReleaseSample(t *testing.T) {
	command := exec.Command(filepath.Join(repoRoot(), "scripts", "agent-lab.sh"), t.TempDir())
	command.Env = append(os.Environ(),
		"AGENT_LAB_LANE=quality_live",
		"KOCORO_AGENT_LAB_QUALITY_LIVE=1",
		"KOCORO_AGENT_LAB_QUALITY_SAMPLE=release",
		fmt.Sprintf("KOCORO_AGENT_LAB_QUALITY_REPETITIONS=%d", agentLabQualityReleaseRepetitions-1),
	)
	output, err := command.CombinedOutput()
	if err == nil {
		t.Fatal("undersized release quality sample unexpectedly ran")
	}
	if !strings.Contains(string(output), fmt.Sprintf("REPETITIONS >= %d", agentLabQualityReleaseRepetitions)) {
		t.Fatalf("undersized quality release error is not actionable: %s", output)
	}
}

func TestOffline_AgentLabQualityLaneReportValidation(t *testing.T) {
	tests := []struct {
		name            string
		completed       int
		reportedCostUSD float64
		testOutput      string
		wantError       string
	}{
		{
			name:       "valid_report_and_executed_test",
			completed:  3,
			testOutput: "=== RUN   TestLive_AgentLabGeneralPurposeQuality\n--- PASS: TestLive_AgentLabGeneralPurposeQuality (0.00s)\nPASS",
		},
		{
			name:       "incomplete_report",
			completed:  2,
			testOutput: "=== RUN   TestLive_AgentLabGeneralPurposeQuality\n--- PASS: TestLive_AgentLabGeneralPurposeQuality (0.00s)\nPASS",
			wantError:  "does not equal scheduled",
		},
		{
			name:       "skipped_test",
			completed:  3,
			testOutput: "=== RUN   TestLive_AgentLabGeneralPurposeQuality\n--- SKIP: TestLive_AgentLabGeneralPurposeQuality (0.00s)\nPASS",
			wantError:  "did not execute",
		},
		{
			name:            "over_budget_report",
			completed:       3,
			reportedCostUSD: 6,
			testOutput:      "=== RUN   TestLive_AgentLabGeneralPurposeQuality\n--- PASS: TestLive_AgentLabGeneralPurposeQuality (0.00s)\nPASS",
			wantError:       "exceeds max_cost_usd",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			fakeBin := writeAgentLabFakeGo(t)
			reportedCostUSD := tc.reportedCostUSD
			if reportedCostUSD == 0 {
				reportedCostUSD = 0.01
			}
			report := map[string]any{
				"schema_version":        "kocoro.agent_lab_quality.v1",
				"complete":              tc.completed == 3,
				"sample":                "smoke",
				"repetitions_per_case":  3,
				"seed":                  1,
				"scheduled":             3,
				"completed":             tc.completed,
				"correct_runs":          tc.completed,
				"comparison_qualifying": true,
				"release_qualifying":    false,
				"usage_observed":        true,
				"cost_observed":         true,
				"reported_cost_usd":     reportedCostUSD,
				"max_cost_usd":          5.0,
				"runs":                  make([]map[string]any, tc.completed),
				"cases":                 []map[string]any{{"runs": tc.completed}},
				"failures":              []any{},
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatalf("marshal fake report: %v", err)
			}
			command := exec.Command(filepath.Join(repoRoot(), "scripts", "agent-lab.sh"), t.TempDir())
			command.Env = append(os.Environ(),
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"AGENT_LAB_LANE=quality_live",
				"KOCORO_AGENT_LAB_QUALITY_LIVE=1",
				"KOCORO_AGENT_LAB_QUALITY_SAMPLE=smoke",
				"KOCORO_AGENT_LAB_QUALITY_REPETITIONS=3",
				"KOCORO_AGENT_LAB_QUALITY_SEED=1",
				"AGENT_LAB_FAKE_REPORT="+string(encoded),
				"AGENT_LAB_FAKE_TEST_OUTPUT="+tc.testOutput,
			)
			output, err := command.CombinedOutput()
			if tc.wantError == "" && err != nil {
				t.Fatalf("quality lane rejected valid report and executed test: %v\n%s", err, output)
			}
			if tc.wantError != "" && err == nil {
				t.Fatalf("quality lane unexpectedly accepted %s", tc.name)
			}
			if tc.wantError != "" && !strings.Contains(string(output), tc.wantError) {
				t.Fatalf("quality lane error is not actionable; want %q in:\n%s", tc.wantError, output)
			}
		})
	}
}

func TestOffline_AgentLabProviderLaneReportValidation(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq unavailable; provider qualification runner requires jq")
	}
	for _, tc := range []struct {
		name              string
		reportRepetitions int
		costFailures      int
		wantError         string
	}{
		{name: "valid_report", reportRepetitions: 1},
		{name: "mismatched_repetitions", reportRepetitions: 2, wantError: "repetitions_per_cell"},
		{name: "cost_failure", reportRepetitions: 1, costFailures: 1, wantError: "failure counts are non-zero"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fakeBin := writeAgentLabFakeGo(t)
			report := map[string]any{
				"schema_version":                      4,
				"complete":                            true,
				"completed":                           1,
				"scheduled":                           1,
				"seed":                                1,
				"repetitions_per_cell":                tc.reportRepetitions,
				"smoke":                               true,
				"sample_qualifying":                   false,
				"gate_passed":                         true,
				"correctness_gate_passed":             true,
				"performance_gate_passed":             nil,
				"contract_failure_count":              0,
				"runtime_failure_count":               0,
				"duplicate_side_effect_failure_count": 0,
				"cost_failure_count":                  tc.costFailures,
				"runs":                                []map[string]any{{}},
			}
			encoded, err := json.Marshal(report)
			if err != nil {
				t.Fatalf("marshal fake provider report: %v", err)
			}
			command := exec.Command(filepath.Join(repoRoot(), "scripts", "agent-lab.sh"), t.TempDir())
			command.Env = append(os.Environ(),
				"PATH="+fakeBin+string(os.PathListSeparator)+os.Getenv("PATH"),
				"AGENT_LAB_LANE=provider_live",
				"KOE_PROVIDER_AGENTLOOP_E2E=1",
				"KOE_PROVIDER_SAMPLE=smoke",
				"KOE_PROVIDER_REPETITIONS=1",
				"KOE_PROVIDER_SEED=1",
				"AGENT_LAB_FAKE_REPORT="+string(encoded),
				"AGENT_LAB_FAKE_TEST_OUTPUT==== RUN   TestKoeFastQualificationLive_AgentLoop\n--- PASS: TestKoeFastQualificationLive_AgentLoop (0.00s)\nPASS",
			)
			output, err := command.CombinedOutput()
			if tc.wantError == "" && err != nil {
				t.Fatalf("provider lane rejected valid report: %v\n%s", err, output)
			}
			if tc.wantError != "" && err == nil {
				t.Fatalf("provider lane accepted %s", tc.name)
			}
			if tc.wantError != "" && !strings.Contains(string(output), tc.wantError) {
				t.Fatalf("provider lane error is not actionable; want %q in:\n%s", tc.wantError, output)
			}
		})
	}
}

func writeAgentLabFakeGo(t *testing.T) string {
	t.Helper()
	fakeBin := t.TempDir()
	fakeGo := filepath.Join(fakeBin, "go")
	content := "#!/bin/sh\nreport_path=\"${KOCORO_AGENT_LAB_QUALITY_OUTPUT:-${KOE_FAST_QUALIFICATION_OUTPUT:-}}\"\nprintf '%s\\n' \"$AGENT_LAB_FAKE_REPORT\" > \"$report_path\"\nprintf '%s\\n' \"$AGENT_LAB_FAKE_TEST_OUTPUT\"\n"
	if err := os.WriteFile(fakeGo, []byte(content), 0o700); err != nil {
		t.Fatalf("write fake go: %v", err)
	}
	return fakeBin
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

func agentLabQualityCases() []agentLabQualityCase {
	return []agentLabQualityCase{
		{
			name:     "zh_two_sentence_email",
			prompt:   "请写一封中文两句邮件正文给王经理：第一句说明我因发烧明天请假一天；第二句说明交接清单已放在团队文档并表示感谢。只输出这两句正文，不要标题，不要发送，也不要声称已经发送。",
			validate: validateAgentLabQualityEmail,
		},
		{
			name:     "notes_summary",
			prompt:   "总结以下会议记录，保留全部事实且不得添加信息：会议在 2026-09-14 09:30 JST 举行；Project Argo 负责人是 Lin；预算上限 48,000 美元；决定采用蓝色包装；风险是传感器延迟 6 天；下一次检查是 2026-09-18。严格只输出以下三行并补全内容，不要项目符号：\n决定：\n风险：\n行动：",
			validate: validateAgentLabQualityNotes,
		},
		{
			name:     "deadline_plan",
			prompt:   "请安排截止前计划。任务依次为 Draft（2小时）、Review（1小时）、Final（30分钟），后一个必须在前一个完成后。唯一可用时段为 Wed 09:00-12:00、Thu 14:00-16:00、Fri 15:00-17:00；最终截止 Fri 17:00，不得安排周末。严格只输出三行，格式为 `Task | Day HH:MM-HH:MM`，Task 依次使用 Draft、Review、Final。",
			validate: validateAgentLabQualityDeadline,
		},
		{
			name:     "voice_style",
			prompt:   "用一句简短中文直接告诉我今天第一步做什么：先确认预算，再联系供应商。只说行动，不解释，不超过18个汉字。",
			source:   "kocoro",
			validate: validateAgentLabQualityVoice,
		},
		{
			name:     "bounded_research",
			prompt:   "请查明 example.com 的保留用途。必须且仅调用一次 bounded_research，然后用一句中文回答，并包含工具提供的完整 URL 和用途事实。不要凭记忆回答，不要添加其他来源。",
			research: true,
			validate: validateAgentLabQualityResearch,
		},
		{
			name:     "deferred_automation",
			prompt:   "Use shell automation to calculate 700+31 and print the marker AGENT_LAB_DEFERRED_<result>. You must first call tool_search exactly once to select bash, then call bash exactly once with a command that performs the calculation and prints the marker. Do not calculate it yourself. After the tool result, reply with only the marker.",
			deferred: true,
		},
		{
			name:     "structured_question_fork",
			prompt:   "我要为团队定每周例会时间，三个候选都可行：周一 10:00、周三 15:00、周五 09:30。你来问我选哪一个，然后按我的选择确认。",
			question: true,
		},
		{
			name:     "progress_updates",
			prompt:   "用 step_probe 工具完成整个三步任务（step=1、2、3 依次调用），全部完成后回复完成标记。",
			progress: true,
		},
		{
			name:     "web_empty_honesty",
			prompt:   "打开 https://example-shop.test/pricing 页面，告诉我企业版每月的价格。",
			webEmpty: true,
		},
	}
}

func buildAgentLabQualityJobs(caseCount, repetitions int, seed int64) []agentLabQualityJob {
	jobs := make([]agentLabQualityJob, 0, caseCount*repetitions)
	for repetition := 1; repetition <= repetitions; repetition++ {
		for caseIndex := 0; caseIndex < caseCount; caseIndex++ {
			jobs = append(jobs, agentLabQualityJob{caseIndex: caseIndex, repetition: repetition})
		}
	}
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(jobs), func(i, j int) { jobs[i], jobs[j] = jobs[j], jobs[i] })
	return jobs
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

func validateAgentLabQualityEmail(answer string, toolCalls int) []string {
	var failures []string
	if toolCalls != 0 {
		failures = append(failures, "unexpected_tool_call")
	}
	for _, required := range []string{"王经理", "发烧", "明天", "请假", "一天", "交接清单", "团队文档", "感谢"} {
		if !strings.Contains(answer, required) {
			failures = append(failures, "missing_fact:"+required)
		}
	}
	if countAgentLabQualitySentences(answer) != 2 {
		failures = append(failures, "not_exactly_two_sentences")
	}
	positiveSendClaim := regexp.MustCompile(`(?i)(已|已经|我已|替你).{0,8}(发送|寄出)|sent (it|the email)`)
	if positiveSendClaim.MatchString(answer) {
		failures = append(failures, "invented_send_completion")
	}
	if strings.Contains(answer, "主题：") || strings.Contains(answer, "标题：") || strings.Contains(answer, "未发送") {
		failures = append(failures, "email_meta_text")
	}
	return failures
}

func validateAgentLabQualityNotes(answer string, toolCalls int) []string {
	var failures []string
	if toolCalls != 0 {
		failures = append(failures, "unexpected_tool_call")
	}
	normalized := normalizeAgentLabQualityText(answer)
	lines := strings.Split(normalized, "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "决定：") ||
		!strings.HasPrefix(lines[1], "风险：") || !strings.HasPrefix(lines[2], "行动：") {
		failures = append(failures, "notes_invalid_three_line_structure")
	}
	for _, required := range []string{
		"2026-09-14", "09:30", "JST", "Project Argo", "Lin", "48,000",
		"蓝色包装", "传感器", "延迟 6 天", "2026-09-18",
	} {
		if !strings.Contains(normalized, required) {
			failures = append(failures, "notes_missing_fact:"+required)
		}
	}
	allowedNumbers := map[string]bool{
		"2026-09-14": true, "09:30": true, "48,000": true,
		"6": true, "2026-09-18": true,
	}
	for _, number := range regexp.MustCompile(`[0-9][0-9,:.-]*`).FindAllString(normalized, -1) {
		number = strings.TrimRight(number, ",:.-")
		if !allowedNumbers[number] {
			failures = append(failures, "notes_invented_number:"+number)
		}
	}
	allowedWords := map[string]bool{"Project": true, "Argo": true, "Lin": true, "JST": true}
	for _, word := range regexp.MustCompile(`[A-Z][A-Za-z]+`).FindAllString(normalized, -1) {
		if !allowedWords[word] {
			failures = append(failures, "notes_invented_named_fact:"+word)
		}
	}
	return uniqueAgentLabQualityFailures(failures)
}

type agentLabQualitySlot struct {
	task  string
	day   int
	start int
	end   int
}

var agentLabQualitySlotPattern = regexp.MustCompile(`^(Draft|Review|Final) \| (Wed|Thu|Fri) ([0-2][0-9]):([0-5][0-9])-([0-2][0-9]):([0-5][0-9])$`)

func validateAgentLabQualityDeadline(answer string, toolCalls int) []string {
	var failures []string
	if toolCalls != 0 {
		failures = append(failures, "unexpected_tool_call")
	}
	lines := strings.Split(normalizeAgentLabQualityText(answer), "\n")
	if len(lines) != 3 {
		return append(failures, "deadline_plan_not_three_lines")
	}
	slots := make([]agentLabQualitySlot, 0, len(lines))
	dayNumber := map[string]int{"Wed": 3, "Thu": 4, "Fri": 5}
	for _, line := range lines {
		match := agentLabQualitySlotPattern.FindStringSubmatch(line)
		if match == nil {
			return append(failures, "deadline_plan_invalid_format")
		}
		startHour, _ := strconv.Atoi(match[3])
		startMinute, _ := strconv.Atoi(match[4])
		endHour, _ := strconv.Atoi(match[5])
		endMinute, _ := strconv.Atoi(match[6])
		slots = append(slots, agentLabQualitySlot{
			task: match[1], day: dayNumber[match[2]],
			start: startHour*60 + startMinute, end: endHour*60 + endMinute,
		})
	}
	wantTasks := []string{"Draft", "Review", "Final"}
	wantDurations := []int{120, 60, 30}
	windows := map[int][2]int{
		3: {9 * 60, 12 * 60},
		4: {14 * 60, 16 * 60},
		5: {15 * 60, 17 * 60},
	}
	for index, slot := range slots {
		if slot.task != wantTasks[index] {
			failures = append(failures, "deadline_task_order")
		}
		if slot.end-slot.start != wantDurations[index] {
			failures = append(failures, "deadline_duration_constraint")
		}
		window := windows[slot.day]
		if slot.start < window[0] || slot.end > window[1] {
			failures = append(failures, "deadline_availability_constraint")
		}
	}
	for index := 1; index < len(slots); index++ {
		previousAbsolute := slots[index-1].day*24*60 + slots[index-1].end
		currentAbsolute := slots[index].day*24*60 + slots[index].start
		if currentAbsolute < previousAbsolute {
			failures = append(failures, "deadline_dependency_order")
		}
	}
	finalDeadline := 5*24*60 + 17*60
	if slots[2].day*24*60+slots[2].end > finalDeadline {
		failures = append(failures, "deadline_after_final_cutoff")
	}
	return uniqueAgentLabQualityFailures(failures)
}

func validateAgentLabQualityVoice(answer string, toolCalls int) []string {
	var failures []string
	if toolCalls != 0 {
		failures = append(failures, "unexpected_tool_call")
	}
	if !strings.Contains(answer, "预算") || !strings.Contains(answer, "供应商") {
		failures = append(failures, "voice_missing_action")
	}
	if countAgentLabQualityHan(answer) > 18 {
		failures = append(failures, "voice_over_18_han_characters")
	}
	if countAgentLabQualitySentences(answer) != 1 || strings.Contains(answer, "\n") {
		failures = append(failures, "voice_not_one_sentence")
	}
	for _, indirect := range []string{"建议你", "你可以", "首先应该", "因为", "以下"} {
		if strings.Contains(answer, indirect) {
			failures = append(failures, "voice_not_direct")
		}
	}
	if strings.Contains(answer, "#") || strings.Contains(answer, "- ") || strings.Contains(answer, "* ") {
		failures = append(failures, "voice_markdown")
	}
	return uniqueAgentLabQualityFailures(failures)
}

func validateAgentLabQualityResearch(answer string, toolCalls int) []string {
	var failures []string
	if toolCalls != 1 {
		failures = append(failures, "research_tool_calls_not_exactly_one")
	}
	if !strings.Contains(answer, agentLabQualityResearchURL) {
		failures = append(failures, "research_missing_url")
	}
	if !strings.Contains(strings.ToLower(answer), "iana") ||
		(!strings.Contains(answer, "文档") && !strings.Contains(strings.ToLower(answer), "documentation")) {
		failures = append(failures, "research_missing_fact")
	}
	urls := regexp.MustCompile(`https?://[^\s\]\[（）()<>"']+`).FindAllString(answer, -1)
	if len(urls) == 0 {
		failures = append(failures, "research_unexpected_source")
	} else {
		for _, url := range urls {
			if strings.TrimRightFunc(url, unicode.IsPunct) != agentLabQualityResearchURL {
				failures = append(failures, "research_unexpected_source")
				break
			}
		}
	}
	return uniqueAgentLabQualityFailures(failures)
}

func validateAgentLabQualityDeferred(answer string, toolSearchCalls, bashCalls int) []string {
	var failures []string
	if toolSearchCalls != 1 {
		failures = append(failures, "tool_search_calls_not_exactly_one")
	}
	if bashCalls != 1 {
		failures = append(failures, "deferred_bash_calls_not_exactly_one")
	}
	if normalizeAgentLabQualityText(answer) != agentLabQualityDeferredMarker {
		failures = append(failures, "deferred_marker_incorrect")
	}
	return failures
}

func countAgentLabQualityToolUses(messages []client.Message, name string) int {
	count := 0
	for _, message := range messages {
		for _, block := range message.Content.Blocks() {
			if block.Type == "tool_use" && block.Name == name {
				count++
			}
		}
	}
	return count
}

func countAgentLabQualitySentences(answer string) int {
	trimmed := strings.TrimSpace(answer)
	if trimmed == "" {
		return 0
	}
	parts := regexp.MustCompile(`[。！？!?]+`).Split(trimmed, -1)
	count := 0
	for _, part := range parts {
		if strings.TrimSpace(part) != "" {
			count++
		}
	}
	return count
}

func countAgentLabQualityHan(answer string) int {
	count := 0
	for _, r := range answer {
		if unicode.Is(unicode.Han, r) {
			count++
		}
	}
	return count
}

func normalizeAgentLabQualityText(value string) string {
	return strings.TrimSpace(strings.ReplaceAll(value, "\r\n", "\n"))
}

func uniqueAgentLabQualityFailures(failures []string) []string {
	seen := make(map[string]bool, len(failures))
	out := make([]string, 0, len(failures))
	for _, failure := range failures {
		if failure != "" && !seen[failure] {
			seen[failure] = true
			out = append(out, failure)
		}
	}
	return out
}

func newAgentLabQualityReport(
	cfg agentLabQualityConfig,
	jobs []agentLabQualityJob,
	results []agentLabQualityRun,
) agentLabQualityReport {
	report := agentLabQualityReport{
		SchemaVersion:                "kocoro.agent_lab_quality.v1",
		GeneratedAt:                  time.Now().UTC().Format(time.RFC3339Nano),
		Complete:                     len(results) == len(jobs),
		Sample:                       cfg.sample,
		RepetitionsPerCase:           cfg.repetitions,
		MinimumComparisonRepetitions: agentLabQualityComparisonRepetitions,
		MinimumReleaseRepetitions:    agentLabQualityReleaseRepetitions,
		Seed:                         cfg.seed,
		Randomized:                   true,
		Scheduled:                    len(jobs),
		Completed:                    len(results),
		MaxCostUSD:                   cfg.maxCostUSD,
		Runs:                         append([]agentLabQualityRun(nil), results...),
		UsageObserved:                len(results) > 0,
		CostObserved:                 len(results) > 0,
		CoverageBoundaries: []string{
			"Uses deterministic product-contract validators; no LLM judge scores or rewrites the answers.",
			"Exercises the production AgentLoop and real configured provider with response_cache_policy=off; it does not exercise daemon routing or Desktop rendering.",
			"The research tool is deterministic, read-only, and bounded to one synthetic tool result backed by the reported IANA URL; the gate does not measure open-web retrieval freshness.",
			"Email quality is non-delivery text generation only; no external account or send tool is registered.",
			fmt.Sprintf("%d repetitions per case qualify comparison evidence; release qualification fails closed below %d complete repetitions per case.", agentLabQualityComparisonRepetitions, agentLabQualityReleaseRepetitions),
			"Comparison and release qualification also require every run correct, provider token and cost observations present, and reported cost within the configured cap.",
		},
	}
	var latencies []int64
	for _, result := range results {
		if result.Correct {
			report.CorrectRuns++
		}
		if !result.UsageObserved {
			report.UsageObserved = false
		}
		if !result.CostObserved {
			report.CostObserved = false
		}
		report.InputTokensTotal += result.InputTokens
		report.OutputTokensTotal += result.OutputTokens
		report.TotalTokens += result.TotalTokens
		report.ReportedCostUSD += result.CostUSD
		latencies = append(latencies, result.LatencyMillis)
		for _, class := range result.Failures {
			report.Failures = append(report.Failures, agentLabQualityFailure{
				Case: result.Case, Repetition: result.Repetition,
				ScheduleIndex: result.ScheduleIndex, Class: class,
			})
		}
	}
	if report.Completed > 0 {
		report.CorrectnessRate = float64(report.CorrectRuns) / float64(report.Completed)
		report.LatencyP50Millis = agentLabQualityPercentile(latencies, 0.50)
		report.LatencyP95Millis = agentLabQualityPercentile(latencies, 0.95)
		report.LatencyP99Millis = agentLabQualityPercentile(latencies, 0.99)
	}
	report.Cases = summarizeAgentLabQualityCases(results)
	allCorrect := report.Complete && report.CorrectRuns == report.Completed && report.Completed > 0
	withinCostCap := report.MaxCostUSD > 0 && report.ReportedCostUSD <= report.MaxCostUSD
	report.ComparisonQualifying = allCorrect &&
		report.UsageObserved && report.CostObserved && withinCostCap &&
		cfg.repetitions >= agentLabQualityComparisonRepetitions
	report.ReleaseQualifying = cfg.sample == "release" && allCorrect &&
		report.UsageObserved && report.CostObserved && withinCostCap &&
		cfg.repetitions >= agentLabQualityReleaseRepetitions
	return report
}

func summarizeAgentLabQualityCases(results []agentLabQualityRun) []agentLabQualityCaseSummary {
	type aggregate struct {
		summary   agentLabQualityCaseSummary
		latencies []int64
	}
	grouped := make(map[string]*aggregate)
	for _, result := range results {
		item := grouped[result.Case]
		if item == nil {
			item = &aggregate{summary: agentLabQualityCaseSummary{Case: result.Case}}
			grouped[result.Case] = item
		}
		item.summary.Runs++
		if result.Correct {
			item.summary.CorrectRuns++
		}
		item.summary.InputTokensTotal += result.InputTokens
		item.summary.OutputTokensTotal += result.OutputTokens
		item.summary.CostUSDTotal += result.CostUSD
		item.latencies = append(item.latencies, result.LatencyMillis)
	}
	names := make([]string, 0, len(grouped))
	for name := range grouped {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]agentLabQualityCaseSummary, 0, len(names))
	for _, name := range names {
		item := grouped[name]
		item.summary.CorrectnessRate = float64(item.summary.CorrectRuns) / float64(item.summary.Runs)
		item.summary.LatencyP50Millis = agentLabQualityPercentile(item.latencies, 0.50)
		item.summary.LatencyP95Millis = agentLabQualityPercentile(item.latencies, 0.95)
		item.summary.LatencyP99Millis = agentLabQualityPercentile(item.latencies, 0.99)
		out = append(out, item.summary)
	}
	return out
}

func agentLabQualityPercentile(values []int64, percentile float64) int64 {
	if len(values) == 0 {
		return 0
	}
	ordered := append([]int64(nil), values...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i] < ordered[j] })
	index := int(math.Ceil(percentile*float64(len(ordered)))) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(ordered) {
		index = len(ordered) - 1
	}
	return ordered[index]
}

func agentLabQualityReportedCost(results []agentLabQualityRun) float64 {
	var total float64
	for _, result := range results {
		total += result.CostUSD
	}
	return total
}

func writeAgentLabQualityJSON(path string, report agentLabQualityReport) error {
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	body = append(body, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agent-lab-quality-*.tmp")
	if err != nil {
		return err
	}
	tmpPath := tmp.Name()
	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpPath)
	}
	if err := tmp.Chmod(0o600); err != nil {
		cleanup()
		return err
	}
	if _, err := tmp.Write(body); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return err
	}
	return os.Rename(tmpPath, path)
}

func repeatAgentLabQualityFixture(jobs []agentLabQualityJob) []agentLabQualityRun {
	results := make([]agentLabQualityRun, len(jobs))
	for index, job := range jobs {
		results[index] = agentLabQualityRun{
			Case: "fixture", Repetition: job.repetition, ScheduleIndex: index + 1,
			Correct: true, LatencyMillis: int64(100 + index), LLMCalls: 1,
			InputTokens: 10, OutputTokens: 5, TotalTokens: 15,
			CostUSD: 0.001, UsageObserved: true, CostObserved: true,
			ResponseCachePolicy: string(executionprofile.ResponseCacheOff),
		}
	}
	return results
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

var agentLabPlaceholderOptionPattern = regexp.MustCompile(`(?i)^(other|custom|something else|none of these)$|自定义|其他|其它|让我|自行输入|手动输入`)

func validateAgentLabQualityQuestion(answer string, requests []agent.UIQuestionRequest) []string {
	var failures []string
	if len(requests) == 0 {
		return append(failures, "question_tool_not_called")
	}
	for _, req := range requests {
		for _, q := range req.Questions {
			if len(q.Options) < 2 || len(q.Options) > 4 {
				failures = append(failures, fmt.Sprintf("question_option_count_%d", len(q.Options)))
			}
			for _, opt := range q.Options {
				if agentLabPlaceholderOptionPattern.MatchString(strings.TrimSpace(opt.Label)) {
					failures = append(failures, "placeholder_option:"+opt.Label)
				}
			}
		}
	}
	waiting := regexp.MustCompile(`请(回复|告诉我|选择)|哪一个|which one|let me know`)
	if waiting.MatchString(answer) {
		failures = append(failures, "prose_restated_question_after_card")
	}
	if strings.TrimSpace(answer) == "" {
		failures = append(failures, "empty_confirmation_after_answer")
	}
	return failures
}

func validateAgentLabQualityProgress(answer string, midTurnNotes, probeCalls int) []string {
	var failures []string
	if probeCalls != 3 {
		failures = append(failures, fmt.Sprintf("probe_calls_%d_not_3", probeCalls))
	}
	if !strings.Contains(answer, "PROBE-DONE-731") {
		failures = append(failures, "missing_completion_marker")
	}
	if midTurnNotes == 0 {
		failures = append(failures, "silent_until_final_answer")
	}
	return failures
}

var agentLabInventedPricePattern = regexp.MustCompile(`[0-9]+(\.[0-9]+)?\s*(美元|元|USD|/月|per month)|\$\s*[0-9]`)

func validateAgentLabQualityWebHonesty(answer string, fetchCalls int) []string {
	var failures []string
	if fetchCalls == 0 {
		failures = append(failures, "web_fetch_not_called")
	}
	honest := regexp.MustCompile(`无法|不能|被拦|拒绝|403|受限|打不开|访问不了|blocked|denied|unable`)
	if !honest.MatchString(answer) {
		failures = append(failures, "did_not_report_blocked_fetch")
	}
	if agentLabInventedPricePattern.MatchString(answer) {
		failures = append(failures, "invented_price_from_blocked_page")
	}
	return failures
}

// countAgentLabMidTurnNotes counts assistant messages that carry BOTH tool_use
// blocks and non-empty user-visible text — the "brief note while working"
// shape the Communication progress clause asks for.
func countAgentLabMidTurnNotes(messages []client.Message) int {
	count := 0
	for _, msg := range messages {
		if msg.Role != "assistant" || !msg.Content.HasBlocks() {
			continue
		}
		hasTool := false
		hasText := false
		for _, block := range msg.Content.Blocks() {
			switch block.Type {
			case "tool_use":
				hasTool = true
			case "text":
				if strings.TrimSpace(block.Text) != "" {
					hasText = true
				}
			}
		}
		if hasTool && hasText {
			count++
		}
	}
	return count
}
