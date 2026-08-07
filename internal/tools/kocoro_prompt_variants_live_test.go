package tools

import (
	"crypto/sha256"
	_ "embed"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

const (
	kocoroPromptVariantsGateEnv        = "KOCORO_PROMPT_VARIANTS_LIVE"
	kocoroPromptVariantsOutputEnv      = "KOCORO_PROMPT_VARIANTS_OUTPUT"
	kocoroPromptVariantsRepetitionsEnv = "KOCORO_PROMPT_VARIANTS_REPETITIONS"
	kocoroPromptVariantsSeedEnv        = "KOCORO_PROMPT_VARIANTS_SEED"
	kocoroPromptVariantsMaxCostEnv     = "KOCORO_PROMPT_VARIANTS_MAX_COST_USD"
	kocoroPromptVariantsPauseEnv       = "KOCORO_PROMPT_VARIANTS_PAUSE_MS"
	kocoroPromptVariantsWorkloadsEnv   = "KOCORO_PROMPT_VARIANTS_WORKLOADS"

	kocoroPromptVariantCurrent     = "current"
	kocoroPromptVariantMinimal     = "minimal_v1"
	kocoroPromptVariantLayered     = "layered_v1"
	kocoroPromptVariantConditional = "layered_conditional_v1"

	kocoroPromptComparisonRepetitions = 3
	kocoroPromptReleaseRepetitions    = 30
	kocoroPromptSlowestRunsLimit      = 3
)

//go:embed testdata/kocoro_prompt_variants/minimal_v1.txt
var kocoroPromptMinimalV1 string

//go:embed testdata/kocoro_prompt_variants/layered_v1.txt
var kocoroPromptLayeredV1 string

//go:embed testdata/kocoro_prompt_variants/web_policy_v1.txt
var kocoroPromptWebPolicyV1 string

//go:embed testdata/kocoro_prompt_variants/empty_result_policy_v1.txt
var kocoroPromptEmptyResultPolicyV1 string

//go:embed testdata/kocoro_prompt_variants/exact_output_policy_v1.txt
var kocoroPromptExactOutputPolicyV1 string

var kocoroPromptVariantNames = []string{
	kocoroPromptVariantCurrent,
	kocoroPromptVariantMinimal,
	kocoroPromptVariantLayered,
	kocoroPromptVariantConditional,
}

var kocoroPromptExperimentWorkloadNames = append(
	append([]string(nil), koeQualificationWorkloadNames...),
	"stable_no_search",
	"current_search_once",
	"empty_search_stop",
	"untrusted_tool_result",
)

type kocoroPromptExperimentConfig struct {
	koeQualificationRuntimeConfig
	outputPath string
	workloads  []string
}

type kocoroPromptVariantMetadata struct {
	Name             string `json:"name"`
	Source           string `json:"source"`
	Role             string `json:"role"`
	ProductCandidate bool   `json:"product_candidate"`
	FixtureChars     int    `json:"fixture_chars,omitempty"`
	FixtureHash      string `json:"fixture_hash,omitempty"`
}

type kocoroPromptMatchedFailure struct {
	Workload   string   `json:"workload"`
	Repetition int      `json:"repetition"`
	Kind       string   `json:"kind"`
	Variants   []string `json:"variants"`
}

type kocoroPromptComparisonAnalysis struct {
	ProductGatePassed       bool                         `json:"product_gate_passed"`
	ComparisonGatePassed    bool                         `json:"comparison_gate_passed"`
	MatchedCoverage         bool                         `json:"matched_coverage"`
	UniversalFailures       []kocoroPromptMatchedFailure `json:"universal_failures"`
	VariantSpecificFailures []kocoroPromptMatchedFailure `json:"variant_specific_failures"`
	ComparisonRuns          []koeQualificationRunReport  `json:"-"`
}

type kocoroPromptVariantSummary struct {
	Name                          string                           `json:"name"`
	Runs                          int                              `json:"runs"`
	SuccessfulTasks               int                              `json:"successful_tasks"`
	CorrectToolRuns               int                              `json:"correct_tool_runs"`
	TaskSuccessRate               float64                          `json:"task_success_rate"`
	ToolCorrectnessRate           float64                          `json:"tool_correctness_rate"`
	ContractFailureCount          int                              `json:"contract_failure_count"`
	RuntimeFailureCount           int                              `json:"runtime_failure_count"`
	DuplicateToolExecutions       int                              `json:"duplicate_tool_executions"`
	DuplicateSideEffectExecutions int                              `json:"duplicate_side_effect_executions"`
	CompletionCallsMean           float64                          `json:"completion_calls_mean"`
	TotalP50Millis                int64                            `json:"total_p50_millis"`
	TotalP95Millis                int64                            `json:"total_p95_millis"`
	TotalP99Millis                int64                            `json:"total_p99_millis"`
	TotalMaxMillis                int64                            `json:"total_max_millis"`
	SlowestRuns                   []kocoroPromptLatencyObservation `json:"slowest_runs"`
	FirstSemanticP50Millis        *int64                           `json:"first_semantic_p50_millis,omitempty"`
	InputTokensMean               float64                          `json:"input_tokens_mean"`
	OutputTokensMean              float64                          `json:"output_tokens_mean"`
	CostUSDTotal                  float64                          `json:"cost_usd_total"`
	SystemCharsMin                int                              `json:"system_chars_min"`
	SystemCharsMax                int                              `json:"system_chars_max"`
}

type kocoroPromptVariantCellSummary struct {
	Name                string  `json:"name"`
	Workload            string  `json:"workload"`
	Runs                int     `json:"runs"`
	SuccessfulRuns      int     `json:"successful_runs"`
	TaskSuccessRate     float64 `json:"task_success_rate"`
	ToolCorrectnessRate float64 `json:"tool_correctness_rate"`
	TotalP50Millis      int64   `json:"total_p50_millis"`
	TotalP95Millis      int64   `json:"total_p95_millis"`
	TotalP99Millis      int64   `json:"total_p99_millis"`
	TotalMaxMillis      int64   `json:"total_max_millis"`
	InputTokensMean     float64 `json:"input_tokens_mean"`
	CompletionCallsMean float64 `json:"completion_calls_mean"`
	CostUSDTotal        float64 `json:"cost_usd_total"`
}

type kocoroPromptLatencyObservation struct {
	Workload      string `json:"workload"`
	Repetition    int    `json:"repetition"`
	ScheduleIndex int    `json:"schedule_index"`
	TotalMillis   int64  `json:"total_millis"`
}

type kocoroPromptPairedComparison struct {
	Candidate               string  `json:"candidate"`
	Control                 string  `json:"control"`
	Pairs                   int     `json:"pairs"`
	CandidateLatencyWins    int     `json:"candidate_latency_wins"`
	ControlLatencyWins      int     `json:"control_latency_wins"`
	LatencyTies             int     `json:"latency_ties"`
	CandidateLatencyWinRate float64 `json:"candidate_latency_win_rate"`
	TotalDeltaP50Millis     int64   `json:"total_delta_p50_millis"`
	TotalDeltaP95Millis     int64   `json:"total_delta_p95_millis"`
	TotalDeltaP99Millis     int64   `json:"total_delta_p99_millis"`
	TotalDeltaMinMillis     int64   `json:"total_delta_min_millis"`
	TotalDeltaMaxMillis     int64   `json:"total_delta_max_millis"`
}

type kocoroPromptExperimentReport struct {
	SchemaVersion                int                              `json:"schema_version"`
	GeneratedAt                  string                           `json:"generated_at"`
	Complete                     bool                             `json:"complete"`
	Completed                    int                              `json:"completed"`
	Scheduled                    int                              `json:"scheduled"`
	Repetitions                  int                              `json:"repetitions_per_cell"`
	Seed                         int64                            `json:"seed"`
	Randomized                   bool                             `json:"randomized"`
	Interleaved                  bool                             `json:"interleaved"`
	ScheduleMode                 string                           `json:"schedule_mode"`
	SampleQualifying             bool                             `json:"sample_qualifying"`
	ComparisonQualifying         bool                             `json:"comparison_qualifying"`
	ReleaseQualifying            bool                             `json:"release_qualifying"`
	MinimumComparisonRepetitions int                              `json:"minimum_comparison_repetitions"`
	MinimumReleaseRepetitions    int                              `json:"minimum_release_repetitions"`
	ComparisonScope              string                           `json:"comparison_scope"`
	ControlledMode               string                           `json:"controlled_mode"`
	Winner                       string                           `json:"winner,omitempty"`
	WinnerStatus                 string                           `json:"winner_status"`
	SelectionReason              string                           `json:"selection_reason"`
	ObservedEfficiencyLeader     string                           `json:"observed_efficiency_leader,omitempty"`
	ObservedEfficiencyReason     string                           `json:"observed_efficiency_reason,omitempty"`
	MaxCostUSD                   float64                          `json:"max_cost_usd"`
	ReportedCostUSD              float64                          `json:"reported_cost_usd"`
	CostObserved                 bool                             `json:"cost_observed"`
	Variants                     []kocoroPromptVariantMetadata    `json:"variants"`
	Workloads                    []string                         `json:"workloads"`
	Runs                         []koeQualificationRunReport      `json:"runs"`
	Summary                      []kocoroPromptVariantSummary     `json:"summary"`
	ComparisonSummary            []kocoroPromptVariantSummary     `json:"comparison_summary"`
	CurrentControlPassed         bool                             `json:"current_control_passed"`
	ProductGatePassed            bool                             `json:"product_gate_passed"`
	ComparisonGatePassed         bool                             `json:"comparison_gate_passed"`
	UniversalFailures            []kocoroPromptMatchedFailure     `json:"universal_failures"`
	VariantSpecificFailures      []kocoroPromptMatchedFailure     `json:"variant_specific_failures"`
	Cells                        []kocoroPromptVariantCellSummary `json:"cells"`
	PairedComparisons            []kocoroPromptPairedComparison   `json:"paired_comparisons"`
	CoverageBoundaries           []string                         `json:"coverage_boundaries"`
}

func kocoroPromptVariantText(name string) string {
	switch name {
	case "", kocoroPromptVariantCurrent:
		return ""
	case kocoroPromptVariantMinimal:
		return strings.TrimSpace(kocoroPromptMinimalV1)
	case kocoroPromptVariantLayered:
		return strings.TrimSpace(kocoroPromptLayeredV1)
	case kocoroPromptVariantConditional:
		return strings.TrimSpace(kocoroPromptLayeredV1)
	default:
		panic("unknown Kocoro prompt variant: " + name)
	}
}

func kocoroPromptVariantTextForWorkload(name, workload string) string {
	text := kocoroPromptVariantText(name)
	if name != kocoroPromptVariantConditional {
		return text
	}
	blocks := []string{strings.TrimSpace(kocoroPromptExactOutputPolicyV1)}
	switch workload {
	case "stable_no_search", "current_search_once":
		blocks = append(blocks, strings.TrimSpace(kocoroPromptWebPolicyV1))
	case "empty_search_stop":
		blocks = append(
			blocks,
			strings.TrimSpace(kocoroPromptWebPolicyV1),
			strings.TrimSpace(kocoroPromptEmptyResultPolicyV1),
		)
	}
	return text + "\n\n" + strings.Join(blocks, "\n\n")
}

func TestKocoroPromptVariantFixtures(t *testing.T) {
	minimal := kocoroPromptVariantText(kocoroPromptVariantMinimal)
	layered := kocoroPromptVariantText(kocoroPromptVariantLayered)
	if len([]rune(minimal)) < 800 {
		t.Fatal("minimal prompt fixture is unexpectedly small")
	}
	for _, required := range []string{
		"user's macOS computer",
		"<persona_note>",
		"## Trust and Context",
		"## Progress and Stopping",
	} {
		if !strings.Contains(layered, required) {
			t.Fatalf("layered prompt fixture missing %q", required)
		}
	}
	if minimal == layered {
		t.Fatal("minimal and layered prompt fixtures are identical")
	}
	conditional := kocoroPromptVariantTextForWorkload(
		kocoroPromptVariantConditional,
		"empty_search_stop",
	)
	for _, required := range []string{
		"## Exact Output",
		"## Browser and Web",
		"## Empty Results and Recovery",
	} {
		if !strings.Contains(conditional, required) {
			t.Fatalf("conditional prompt fixture missing %q", required)
		}
	}
}

func TestKocoroPromptVariantOverrideDoesNotMutateOriginalRequest(t *testing.T) {
	original := koeQualificationRequestWithSystem("production")
	client := &koeQualificationLLMClient{systemPrompt: "candidate"}
	changed := client.withSystemPrompt(original)
	if got := changed.Messages[0].Content.Text(); got != "candidate" {
		t.Fatalf("changed system = %q", got)
	}
	if got := original.Messages[0].Content.Text(); got != "production" {
		t.Fatalf("original system mutated to %q", got)
	}
}

func TestKocoroPromptExperimentWorkloadsBuild(t *testing.T) {
	for _, name := range kocoroPromptExperimentWorkloadNames {
		workload := buildKoeQualificationWorkload(koeQualificationJob{
			Workload: name,
			Token:    "ABC123",
		})
		if workload.prompt == "" || workload.receipt == "" || workload.registry == nil {
			t.Fatalf("workload %q is incomplete", name)
		}
	}
}

func TestLoadKocoroPromptExperimentWorkloadsFiltersAndDeduplicates(t *testing.T) {
	t.Setenv(kocoroPromptVariantsWorkloadsEnv, "empty_search_stop, stable_no_search,empty_search_stop")
	got := loadKocoroPromptExperimentWorkloads(t)
	want := []string{"empty_search_stop", "stable_no_search"}
	if fmt.Sprint(got) != fmt.Sprint(want) {
		t.Fatalf("workloads=%v, want %v", got, want)
	}
}

func TestBuildKocoroPromptExperimentJobsInterleavesMatchedBlocks(t *testing.T) {
	cfg := kocoroPromptExperimentConfig{
		koeQualificationRuntimeConfig: koeQualificationRuntimeConfig{
			repetitions: 2,
			seed:        42,
		},
		workloads: []string{"no_tool", "one_tool"},
	}
	jobs := buildKocoroPromptExperimentJobs(cfg)
	if len(jobs) != 2*2*len(kocoroPromptVariantNames) {
		t.Fatalf("jobs=%d, want %d", len(jobs), 2*2*len(kocoroPromptVariantNames))
	}
	for start := 0; start < len(jobs); start += len(kocoroPromptVariantNames) {
		block := jobs[start : start+len(kocoroPromptVariantNames)]
		seen := make(map[string]bool, len(block))
		for _, job := range block {
			if job.Workload != block[0].Workload || job.Repetition != block[0].Repetition ||
				job.Token != block[0].Token {
				t.Fatalf("matched block at %d is not contiguous: %+v", start, block)
			}
			if seen[job.PromptVariant] {
				t.Fatalf("matched block at %d repeats variant %q", start, job.PromptVariant)
			}
			seen[job.PromptVariant] = true
		}
	}
}

func TestSummarizeKocoroPromptVariantsReportsP99AndSlowestRuns(t *testing.T) {
	values := []int64{100, 200, 300, 400, 1000}
	var runs []koeQualificationRunReport
	for index, value := range values {
		run := passingKocoroPromptRun(kocoroPromptVariantCurrent, "no_tool", index+1)
		run.TotalMillis = value
		run.ScheduleIndex = index + 10
		runs = append(runs, run)
	}
	summary := summarizeKocoroPromptVariants(runs)
	if len(summary) != 1 {
		t.Fatalf("summary=%d, want 1", len(summary))
	}
	got := summary[0]
	if got.TotalP50Millis != 300 || got.TotalP95Millis != 1000 ||
		got.TotalP99Millis != 1000 || got.TotalMaxMillis != 1000 {
		t.Fatalf("latency summary=%+v", got)
	}
	if len(got.SlowestRuns) != 3 || got.SlowestRuns[0].TotalMillis != 1000 ||
		got.SlowestRuns[0].ScheduleIndex != 14 || got.SlowestRuns[2].TotalMillis != 300 {
		t.Fatalf("slowest_runs=%+v", got.SlowestRuns)
	}
}

func TestSummarizeKocoroPromptPairedComparisons(t *testing.T) {
	control := []int64{100, 200, 300, 400, 1000}
	candidate := []int64{90, 220, 300, 350, 800}
	var runs []koeQualificationRunReport
	for index := range control {
		currentRun := passingKocoroPromptRun(kocoroPromptVariantCurrent, "no_tool", index+1)
		currentRun.TotalMillis = control[index]
		candidateRun := passingKocoroPromptRun(kocoroPromptVariantLayered, "no_tool", index+1)
		candidateRun.TotalMillis = candidate[index]
		runs = append(runs, candidateRun, currentRun)
	}
	comparisons := summarizeKocoroPromptPairedComparisons(runs)
	var got kocoroPromptPairedComparison
	for _, comparison := range comparisons {
		if comparison.Candidate == kocoroPromptVariantLayered {
			got = comparison
		}
	}
	if got.Pairs != 5 || got.CandidateLatencyWins != 3 ||
		got.ControlLatencyWins != 1 || got.LatencyTies != 1 ||
		math.Abs(got.CandidateLatencyWinRate-0.6) > 1e-9 {
		t.Fatalf("paired counts=%+v", got)
	}
	if got.TotalDeltaP50Millis != -10 || got.TotalDeltaP95Millis != 20 ||
		got.TotalDeltaP99Millis != 20 || got.TotalDeltaMinMillis != -200 ||
		got.TotalDeltaMaxMillis != 20 {
		t.Fatalf("paired deltas=%+v", got)
	}
}

func TestKocoroPromptReleaseQualificationRequiresThirtyRepetitions(t *testing.T) {
	reportFor := func(repetitions int) kocoroPromptExperimentReport {
		cfg := kocoroPromptExperimentConfig{
			koeQualificationRuntimeConfig: koeQualificationRuntimeConfig{
				repetitions: repetitions,
				seed:        42,
			},
			workloads: []string{"no_tool"},
		}
		jobs := buildKocoroPromptExperimentJobs(cfg)
		results := make([]koeQualificationRunReport, 0, len(jobs))
		for index, job := range jobs {
			run := passingKocoroPromptRun(job.PromptVariant, job.Workload, job.Repetition)
			if kocoroPromptVariantProductCandidate(job.PromptVariant) {
				run.InputTokens = 50
			}
			run.ScheduleIndex = index + 1
			results = append(results, run)
		}
		return newKocoroPromptExperimentReport(cfg, jobs, results, true)
	}
	comparison := reportFor(kocoroPromptComparisonRepetitions)
	if !comparison.SampleQualifying || !comparison.ComparisonQualifying ||
		comparison.ReleaseQualifying {
		t.Fatalf("three-repetition qualification=%+v", comparison)
	}
	twentyNine := reportFor(kocoroPromptReleaseRepetitions - 1)
	if twentyNine.ReleaseQualifying || twentyNine.WinnerStatus == "release_ready" {
		t.Fatalf("twenty-nine-repetition qualification=%+v", twentyNine)
	}
	release := reportFor(kocoroPromptReleaseRepetitions)
	if !release.ComparisonQualifying || !release.ReleaseQualifying ||
		release.WinnerStatus != "release_ready" {
		t.Fatalf("thirty-repetition qualification=%+v", release)
	}
}

func TestSelectKocoroPromptVariantProtectsTailLatency(t *testing.T) {
	passing := func(name string, p50, p95 int64, input float64) kocoroPromptVariantSummary {
		return kocoroPromptVariantSummary{
			Name: name, Runs: 3, SuccessfulTasks: 3, CorrectToolRuns: 3,
			TaskSuccessRate: 1, ToolCorrectnessRate: 1,
			TotalP50Millis: p50, TotalP95Millis: p95, InputTokensMean: input,
		}
	}
	winner, status, _ := selectKocoroPromptVariant([]kocoroPromptVariantSummary{
		passing(kocoroPromptVariantCurrent, 100, 100, 1000),
		passing(kocoroPromptVariantMinimal, 90, 120, 500),
		passing(kocoroPromptVariantLayered, 105, 105, 650),
	}, 3, true)
	if winner != kocoroPromptVariantLayered || status != "comparison_ready" {
		t.Fatalf("winner=%q status=%q, want layered_v1/comparison_ready", winner, status)
	}
}

func TestSelectKocoroPromptVariantNeverPromotesMinimalStressControl(t *testing.T) {
	passing := func(name string, p50, p95 int64, input float64) kocoroPromptVariantSummary {
		return kocoroPromptVariantSummary{
			Name: name, Runs: 3, SuccessfulTasks: 3, CorrectToolRuns: 3,
			TaskSuccessRate: 1, ToolCorrectnessRate: 1,
			TotalP50Millis: p50, TotalP95Millis: p95, InputTokensMean: input,
		}
	}
	winner, status, _ := selectKocoroPromptVariant([]kocoroPromptVariantSummary{
		passing(kocoroPromptVariantCurrent, 100, 100, 1000),
		passing(kocoroPromptVariantMinimal, 1, 1, 1),
		passing(kocoroPromptVariantLayered, 90, 90, 650),
	}, 3, true)
	if winner != kocoroPromptVariantLayered || status != "comparison_ready" {
		t.Fatalf("winner=%q status=%q, want layered_v1/comparison_ready", winner, status)
	}
}

func TestSelectObservedEfficiencyLeaderIncludesStressControls(t *testing.T) {
	passing := func(name string, p50, p95 int64, input float64) kocoroPromptVariantSummary {
		return kocoroPromptVariantSummary{
			Name: name, Runs: 3, SuccessfulTasks: 3, CorrectToolRuns: 3,
			TaskSuccessRate: 1, ToolCorrectnessRate: 1,
			TotalP50Millis: p50, TotalP95Millis: p95, InputTokensMean: input,
		}
	}
	leader, reason := selectObservedEfficiencyLeader([]kocoroPromptVariantSummary{
		passing(kocoroPromptVariantCurrent, 100, 100, 1000),
		passing(kocoroPromptVariantMinimal, 50, 60, 100),
		passing(kocoroPromptVariantLayered, 70, 80, 500),
	}, true)
	if leader != kocoroPromptVariantMinimal || !strings.Contains(reason, "not production eligibility") {
		t.Fatalf("leader=%q reason=%q, want minimal descriptive leader", leader, reason)
	}
}

func TestKocoroPromptSummaryCorrectRequiresNamedPassingVariant(t *testing.T) {
	passing := kocoroPromptVariantSummary{
		Name: kocoroPromptVariantCurrent, Runs: 1,
		SuccessfulTasks: 1, CorrectToolRuns: 1,
		TaskSuccessRate: 1, ToolCorrectnessRate: 1,
	}
	if !kocoroPromptSummaryCorrect([]kocoroPromptVariantSummary{passing}, kocoroPromptVariantCurrent) {
		t.Fatal("passing current control was not recognized")
	}
	if kocoroPromptSummaryCorrect([]kocoroPromptVariantSummary{passing}, kocoroPromptVariantMinimal) {
		t.Fatal("missing minimal variant was recognized as passing")
	}
}

func TestSelectKocoroPromptVariantFailsClosedWhenControlFails(t *testing.T) {
	current := kocoroPromptVariantSummary{
		Name: kocoroPromptVariantCurrent, Runs: 3,
		TaskSuccessRate: 2.0 / 3.0, ToolCorrectnessRate: 1,
	}
	if winner, status, _ := selectKocoroPromptVariant(
		[]kocoroPromptVariantSummary{current}, 3, true,
	); winner != "" || status != "none" {
		t.Fatalf("winner=%q status=%q, want empty/none", winner, status)
	}
}

func TestAnalyzeKocoroPromptExperimentKeepsUniversalFailureOutOfRelativeComparison(t *testing.T) {
	var runs []koeQualificationRunReport
	for _, name := range kocoroPromptVariantNames {
		runs = append(runs,
			passingKocoroPromptRun(name, "no_tool", 1),
			passingKocoroPromptRun(name, "serial_3", 1),
		)
		runs[len(runs)-1].TaskSuccess = false
		runs[len(runs)-1].Outcome = "task_incorrect"
	}
	analysis := analyzeKocoroPromptExperiment(runs, len(runs))
	if analysis.ProductGatePassed {
		t.Fatal("universal failure must keep the product gate closed")
	}
	if !analysis.ComparisonGatePassed {
		t.Fatal("a matched universal failure must not prevent relative comparison")
	}
	if len(analysis.UniversalFailures) != 1 || len(analysis.ComparisonRuns) != 4 {
		t.Fatalf(
			"universal=%d comparison_runs=%d, want 1/4",
			len(analysis.UniversalFailures), len(analysis.ComparisonRuns),
		)
	}
}

func TestAnalyzeKocoroPromptExperimentDisqualifiesOnlyFailingCandidate(t *testing.T) {
	var runs []koeQualificationRunReport
	for _, name := range kocoroPromptVariantNames {
		runs = append(runs, passingKocoroPromptRun(name, "one_tool", 1))
	}
	runs[1].ToolCorrectness = false
	analysis := analyzeKocoroPromptExperiment(runs, len(runs))
	if !analysis.ComparisonGatePassed {
		t.Fatal("a candidate failure must disqualify that candidate, not the matched comparison")
	}
	if len(analysis.VariantSpecificFailures) != 1 {
		t.Fatalf("variant failures=%d, want 1", len(analysis.VariantSpecificFailures))
	}
}

func TestAnalyzeKocoroPromptExperimentClosesComparisonWhenControlFails(t *testing.T) {
	var runs []koeQualificationRunReport
	for _, name := range kocoroPromptVariantNames {
		runs = append(runs, passingKocoroPromptRun(name, "one_tool", 1))
	}
	runs[0].TaskSuccess = false
	runs[0].Outcome = "task_incorrect"
	analysis := analyzeKocoroPromptExperiment(runs, len(runs))
	if analysis.ComparisonGatePassed {
		t.Fatal("a control-specific failure must close the comparison gate")
	}
}

func passingKocoroPromptRun(
	variant, workload string,
	repetition int,
) koeQualificationRunReport {
	return koeQualificationRunReport{
		PromptVariant:    variant,
		Workload:         workload,
		Repetition:       repetition,
		Outcome:          "success",
		TaskSuccess:      true,
		ToolCorrectness:  true,
		RouteExact:       true,
		ProviderExact:    true,
		ModelExact:       true,
		CachePolicyExact: true,
		TotalMillis:      100,
		InputTokens:      100,
	}
}

func koeQualificationRequestWithSystem(text string) client.CompletionRequest {
	return client.CompletionRequest{Messages: []client.Message{{
		Role:    "system",
		Content: client.NewTextContent(text),
	}}}
}

func TestKocoroPromptVariantsLive_AgentLoop(t *testing.T) {
	if os.Getenv(kocoroPromptVariantsGateEnv) != "1" {
		t.Skip("set KOCORO_PROMPT_VARIANTS_LIVE=1 to run the paid prompt comparison")
	}
	cfg := loadKocoroPromptExperimentConfig(t)
	jobs := buildKocoroPromptExperimentJobs(cfg)
	results := make([]koeQualificationRunReport, 0, len(jobs))
	partialPath := cfg.outputPath + ".partial"
	writeKocoroPromptExperimentReport(t, partialPath, cfg, jobs, results, false)

	for index, job := range jobs {
		result := runKoeQualificationJob(t.Context(), cfg.koeQualificationRuntimeConfig, job)
		result.ScheduleIndex = index + 1
		results = append(results, result)
		writeKocoroPromptExperimentReport(t, partialPath, cfg, jobs, results, false)
		if koeQualificationMissingCostRequiresAbort(result) {
			t.Fatalf(
				"cost observation missing: completed=%d scheduled=%d partial=%s",
				len(results), len(jobs), partialPath,
			)
		}
		cost := kocoroPromptReportedCost(results)
		if cost > cfg.maxCostUSD {
			t.Fatalf(
				"cost budget exceeded: completed=%d scheduled=%d reported_usd=%.6f max_usd=%.6f partial=%s",
				len(results), len(jobs), cost, cfg.maxCostUSD, partialPath,
			)
		}
		if len(results)%10 == 0 || len(results) == len(jobs) {
			t.Logf(
				"prompt comparison progress: completed=%d scheduled=%d partial=%s",
				len(results), len(jobs), partialPath,
			)
		}
		if len(results) < len(jobs) {
			waitKoeQualificationPause(t, t.Context(), cfg.pause)
		}
	}

	report := newKocoroPromptExperimentReport(cfg, jobs, results, true)
	if err := writeKoeQualificationJSON(cfg.outputPath, report); err != nil {
		t.Fatalf("write final prompt comparison: %v", err)
	}
	if err := os.Remove(partialPath); err != nil && !os.IsNotExist(err) {
		t.Fatalf("remove completed partial report: %v", err)
	}
	t.Logf(
		"prompt comparison complete: runs=%d repetitions=%d sample_qualifying=%t winner=%s status=%s cost_usd=%.6f path=%s",
		len(results), cfg.repetitions, report.SampleQualifying, report.Winner,
		report.WinnerStatus, report.ReportedCostUSD, cfg.outputPath,
	)
}

func loadKocoroPromptExperimentConfig(t *testing.T) kocoroPromptExperimentConfig {
	t.Helper()
	endpoint := strings.TrimSpace(os.Getenv(koeQualificationEndpointEnv))
	if endpoint == "" {
		endpoint = koeQualificationEndpointFromUserConfig()
	}
	apiKey := strings.TrimSpace(os.Getenv(koeQualificationAPIKeyEnv))
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("TOOLSEARCH_CLOUD_API_KEY"))
	}
	if apiKey == "" {
		apiKey = koeQualificationAPIKeyFromCredentialStore()
	}
	if endpoint == "" || apiKey == "" {
		t.Fatal("prompt comparison needs the configured endpoint and signed-in credential")
	}

	repetitions := 1
	if raw := strings.TrimSpace(os.Getenv(kocoroPromptVariantsRepetitionsEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 1 || value > 100 {
			t.Fatal("KOCORO_PROMPT_VARIANTS_REPETITIONS must be an integer from 1 through 100")
		}
		repetitions = value
	}
	seed := koeQualificationDefaultSeed
	if raw := strings.TrimSpace(os.Getenv(kocoroPromptVariantsSeedEnv)); raw != "" {
		value, err := strconv.ParseInt(raw, 10, 64)
		if err != nil {
			t.Fatal("KOCORO_PROMPT_VARIANTS_SEED must be a signed 64-bit integer")
		}
		seed = value
	}
	maxCost := 5.0
	if raw := strings.TrimSpace(os.Getenv(kocoroPromptVariantsMaxCostEnv)); raw != "" {
		value, err := strconv.ParseFloat(raw, 64)
		if err != nil || !koeQualificationPositiveFinite(value) || value > 25 {
			t.Fatal("KOCORO_PROMPT_VARIANTS_MAX_COST_USD must be greater than 0 and at most 25")
		}
		maxCost = value
	}
	pause := time.Duration(0)
	if raw := strings.TrimSpace(os.Getenv(kocoroPromptVariantsPauseEnv)); raw != "" {
		value, err := strconv.Atoi(raw)
		if err != nil || value < 0 || value > 60_000 {
			t.Fatal("KOCORO_PROMPT_VARIANTS_PAUSE_MS must be an integer from 0 through 60000")
		}
		pause = time.Duration(value) * time.Millisecond
	}
	outputPath := strings.TrimSpace(os.Getenv(kocoroPromptVariantsOutputEnv))
	if outputPath == "" {
		outputPath = filepath.Join(os.TempDir(), fmt.Sprintf("kocoro-prompt-variants-%d.json", seed))
	}
	return kocoroPromptExperimentConfig{
		koeQualificationRuntimeConfig: koeQualificationRuntimeConfig{
			endpoint:    strings.TrimRight(endpoint, "/"),
			apiKey:      apiKey,
			repetitions: repetitions,
			seed:        seed,
			smoke:       repetitions < 3,
			maxCostUSD:  maxCost,
			pause:       pause,
		},
		outputPath: outputPath,
		workloads:  loadKocoroPromptExperimentWorkloads(t),
	}
}

func loadKocoroPromptExperimentWorkloads(t *testing.T) []string {
	t.Helper()
	raw := strings.TrimSpace(os.Getenv(kocoroPromptVariantsWorkloadsEnv))
	if raw == "" {
		return append([]string(nil), kocoroPromptExperimentWorkloadNames...)
	}
	allowed := make(map[string]bool, len(kocoroPromptExperimentWorkloadNames))
	for _, name := range kocoroPromptExperimentWorkloadNames {
		allowed[name] = true
	}
	var selected []string
	seen := make(map[string]bool)
	for _, item := range strings.Split(raw, ",") {
		name := strings.TrimSpace(item)
		if !allowed[name] {
			t.Fatalf("unknown KOCORO_PROMPT_VARIANTS_WORKLOADS entry %q", name)
		}
		if !seen[name] {
			selected = append(selected, name)
			seen[name] = true
		}
	}
	if len(selected) == 0 {
		t.Fatal("KOCORO_PROMPT_VARIANTS_WORKLOADS must select at least one workload")
	}
	return selected
}

func buildKocoroPromptExperimentJobs(cfg kocoroPromptExperimentConfig) []koeQualificationJob {
	type matchedBlock struct {
		workload   string
		repetition int
	}
	blocks := make([]matchedBlock, 0, len(cfg.workloads)*cfg.repetitions)
	for _, workload := range cfg.workloads {
		for repetition := 1; repetition <= cfg.repetitions; repetition++ {
			blocks = append(blocks, matchedBlock{workload: workload, repetition: repetition})
		}
	}
	rng := rand.New(rand.NewSource(cfg.seed))
	rng.Shuffle(len(blocks), func(i, j int) { blocks[i], blocks[j] = blocks[j], blocks[i] })
	jobs := make([]koeQualificationJob, 0,
		len(blocks)*len(kocoroPromptVariantNames))
	for _, block := range blocks {
		variants := append([]string(nil), kocoroPromptVariantNames...)
		rng.Shuffle(len(variants), func(i, j int) { variants[i], variants[j] = variants[j], variants[i] })
		token := koeQualificationToken(cfg.seed, block.workload, block.repetition)
		for _, variant := range variants {
			jobs = append(jobs, koeQualificationJob{
				Lane:          koeQualificationFastLane,
				Workload:      block.workload,
				Repetition:    block.repetition,
				Token:         token,
				PromptVariant: variant,
			})
		}
	}
	return jobs
}

func newKocoroPromptExperimentReport(
	cfg kocoroPromptExperimentConfig,
	jobs []koeQualificationJob,
	results []koeQualificationRunReport,
	complete bool,
) kocoroPromptExperimentReport {
	summary := summarizeKocoroPromptVariants(results)
	analysis := analyzeKocoroPromptExperiment(results, len(jobs))
	comparisonSummary := summarizeKocoroPromptVariants(analysis.ComparisonRuns)
	winner, status, reason := selectKocoroPromptVariant(
		comparisonSummary, cfg.repetitions,
		complete && analysis.ComparisonGatePassed,
	)
	efficiencyLeader, efficiencyReason := selectObservedEfficiencyLeader(summary, complete)
	sampleQualifying := complete && cfg.repetitions >= kocoroPromptComparisonRepetitions &&
		len(results) == len(jobs)
	comparisonQualifying := sampleQualifying && analysis.ComparisonGatePassed
	releaseQualifying := complete && cfg.repetitions >= kocoroPromptReleaseRepetitions &&
		len(results) == len(jobs) && analysis.ProductGatePassed && winner != ""
	if status == "release_ready" && !releaseQualifying {
		status = "comparison_ready"
		reason += " Release qualification remains closed because the complete product gate and candidate selection requirements did not both pass."
	}
	if len(analysis.UniversalFailures) > 0 && winner != "" {
		reason += " Matched failures shared by every variant were excluded only from relative selection; the product gate remains closed."
	}
	return kocoroPromptExperimentReport{
		SchemaVersion:                1,
		GeneratedAt:                  time.Now().UTC().Format(time.RFC3339Nano),
		Complete:                     complete,
		Completed:                    len(results),
		Scheduled:                    len(jobs),
		Repetitions:                  cfg.repetitions,
		Seed:                         cfg.seed,
		Randomized:                   true,
		Interleaved:                  true,
		ScheduleMode:                 "randomized_matched_blocks_with_randomized_variant_order",
		SampleQualifying:             sampleQualifying,
		ComparisonQualifying:         comparisonQualifying,
		ReleaseQualifying:            releaseQualifying,
		MinimumComparisonRepetitions: kocoroPromptComparisonRepetitions,
		MinimumReleaseRepetitions:    kocoroPromptReleaseRepetitions,
		ComparisonScope:              "system_instructions_with_constant_agent_loop_tools_mode_and_workloads",
		ControlledMode:               koeQualificationFastLane,
		Winner:                       winner,
		WinnerStatus:                 status,
		SelectionReason:              reason,
		ObservedEfficiencyLeader:     efficiencyLeader,
		ObservedEfficiencyReason:     efficiencyReason,
		MaxCostUSD:                   cfg.maxCostUSD,
		ReportedCostUSD:              kocoroPromptReportedCost(results),
		CostObserved:                 kocoroPromptCostObserved(results),
		Variants:                     kocoroPromptVariantMetadataList(),
		Workloads:                    append([]string(nil), cfg.workloads...),
		Runs:                         append([]koeQualificationRunReport(nil), results...),
		Summary:                      summary,
		ComparisonSummary:            comparisonSummary,
		CurrentControlPassed:         complete && kocoroPromptSummaryCorrect(summary, kocoroPromptVariantCurrent),
		ProductGatePassed:            complete && analysis.ProductGatePassed,
		ComparisonGatePassed:         complete && analysis.ComparisonGatePassed,
		UniversalFailures:            analysis.UniversalFailures,
		VariantSpecificFailures:      analysis.VariantSpecificFailures,
		Cells:                        summarizeKocoroPromptVariantCells(results),
		PairedComparisons:            summarizeKocoroPromptPairedComparisons(results),
		CoverageBoundaries: []string{
			"The comparison exercises the production agent loop and provider with deterministic in-memory tools; it does not mutate user files or external services.",
			"Voice routing, microphone behavior, signed-in app UI, and physical interaction are outside this comparison.",
			"Three repetitions per cell are comparison-ready but do not establish rare-failure rates or qualify a release.",
			"Release qualification fails closed below 30 complete matched repetitions per workload and requires the product correctness gate to pass.",
			"Paired latency deltas are candidate minus current control; latency win rate is candidate wins divided by all matched pairs, including ties in the denominator.",
			"Each aggregate exposes P99, maximum latency, and the three slowest runs so tail outliers remain inspectable by workload, repetition, and schedule position.",
		},
	}
}

func analyzeKocoroPromptExperiment(
	results []koeQualificationRunReport,
	scheduled int,
) kocoroPromptComparisonAnalysis {
	type matchedKey struct {
		workload   string
		repetition int
	}
	grouped := make(map[matchedKey][]koeQualificationRunReport)
	for _, result := range results {
		key := matchedKey{workload: result.Workload, repetition: result.Repetition}
		grouped[key] = append(grouped[key], result)
	}
	keys := make([]matchedKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].workload != keys[j].workload {
			return keys[i].workload < keys[j].workload
		}
		return keys[i].repetition < keys[j].repetition
	})

	analysis := kocoroPromptComparisonAnalysis{
		ProductGatePassed: len(results) == scheduled,
		MatchedCoverage:   len(results) == scheduled,
	}
	universalKeys := make(map[matchedKey]bool)
	for _, key := range keys {
		cell := grouped[key]
		seen := make(map[string]bool, len(cell))
		allFailedTaskOnly := len(cell) == len(kocoroPromptVariantNames)
		variants := make([]string, 0, len(cell))
		for _, run := range cell {
			if seen[run.PromptVariant] {
				analysis.MatchedCoverage = false
			}
			seen[run.PromptVariant] = true
			variants = append(variants, run.PromptVariant)
			if run.TaskSuccess || !kocoroPromptRunSafeApartFromTask(run) {
				allFailedTaskOnly = false
			}
			if !kocoroPromptRunPassed(run) {
				analysis.ProductGatePassed = false
			}
		}
		for _, name := range kocoroPromptVariantNames {
			if !seen[name] {
				analysis.MatchedCoverage = false
				allFailedTaskOnly = false
			}
		}
		sort.Strings(variants)
		if allFailedTaskOnly {
			universalKeys[key] = true
			analysis.UniversalFailures = append(
				analysis.UniversalFailures,
				kocoroPromptMatchedFailure{
					Workload: key.workload, Repetition: key.repetition,
					Kind: "task_incorrect_all_variants", Variants: variants,
				},
			)
			continue
		}
		for _, run := range cell {
			if kocoroPromptRunPassed(run) {
				continue
			}
			analysis.VariantSpecificFailures = append(
				analysis.VariantSpecificFailures,
				kocoroPromptMatchedFailure{
					Workload: key.workload, Repetition: key.repetition,
					Kind: run.Outcome, Variants: []string{run.PromptVariant},
				},
			)
		}
	}
	for _, result := range results {
		key := matchedKey{workload: result.Workload, repetition: result.Repetition}
		if !universalKeys[key] {
			analysis.ComparisonRuns = append(analysis.ComparisonRuns, result)
		}
	}
	analysis.ProductGatePassed = analysis.ProductGatePassed && analysis.MatchedCoverage
	analysis.ComparisonGatePassed = analysis.MatchedCoverage
	for _, failure := range analysis.VariantSpecificFailures {
		if len(failure.Variants) == 1 && failure.Variants[0] == kocoroPromptVariantCurrent {
			analysis.ComparisonGatePassed = false
			break
		}
	}
	return analysis
}

func kocoroPromptRunPassed(run koeQualificationRunReport) bool {
	return run.TaskSuccess && kocoroPromptRunSafeApartFromTask(run)
}

func kocoroPromptRunSafeApartFromTask(run koeQualificationRunReport) bool {
	return run.ToolCorrectness && run.RouteExact && run.ProviderExact &&
		run.ModelExact && run.CachePolicyExact && run.RuntimeErrorClass == "" &&
		len(run.HTTPStatuses) == 0 && run.TransportErrors == 0 &&
		run.DuplicateToolExecutions == 0 &&
		run.DuplicateSideEffectExecutions == 0
}

func summarizeKocoroPromptVariants(
	results []koeQualificationRunReport,
) []kocoroPromptVariantSummary {
	grouped := make(map[string][]koeQualificationRunReport)
	for _, result := range results {
		grouped[result.PromptVariant] = append(grouped[result.PromptVariant], result)
	}
	out := make([]kocoroPromptVariantSummary, 0, len(grouped))
	for _, name := range kocoroPromptVariantNames {
		runs := grouped[name]
		if len(runs) == 0 {
			continue
		}
		summary := kocoroPromptVariantSummary{Name: name, Runs: len(runs)}
		var totals, firstSemantic []int64
		summary.SystemCharsMin = math.MaxInt
		for _, run := range runs {
			summary.SuccessfulTasks += boolInt(run.TaskSuccess)
			summary.CorrectToolRuns += boolInt(run.ToolCorrectness)
			if !run.RouteExact || !run.ProviderExact || !run.ModelExact || !run.CachePolicyExact {
				summary.ContractFailureCount++
			}
			if run.RuntimeErrorClass != "" || len(run.HTTPStatuses) > 0 || run.TransportErrors > 0 {
				summary.RuntimeFailureCount++
			}
			summary.DuplicateToolExecutions += run.DuplicateToolExecutions
			summary.DuplicateSideEffectExecutions += run.DuplicateSideEffectExecutions
			summary.CompletionCallsMean += float64(run.CompletionCalls)
			summary.InputTokensMean += float64(run.InputTokens)
			summary.OutputTokensMean += float64(run.OutputTokens)
			summary.CostUSDTotal += run.CostUSD
			totals = append(totals, run.TotalMillis)
			if run.FirstSemanticDeltaMillis != nil {
				firstSemantic = append(firstSemantic, *run.FirstSemanticDeltaMillis)
			}
			if run.SystemChars < summary.SystemCharsMin {
				summary.SystemCharsMin = run.SystemChars
			}
			if run.SystemChars > summary.SystemCharsMax {
				summary.SystemCharsMax = run.SystemChars
			}
		}
		n := float64(len(runs))
		summary.TaskSuccessRate = float64(summary.SuccessfulTasks) / n
		summary.ToolCorrectnessRate = float64(summary.CorrectToolRuns) / n
		summary.CompletionCallsMean /= n
		summary.InputTokensMean /= n
		summary.OutputTokensMean /= n
		summary.TotalP50Millis = koeQualificationPercentile(totals, 0.50)
		summary.TotalP95Millis = koeQualificationPercentile(totals, 0.95)
		summary.TotalP99Millis = koeQualificationPercentile(totals, 0.99)
		summary.TotalMaxMillis = koeQualificationPercentile(totals, 1)
		summary.SlowestRuns = kocoroPromptSlowestRuns(runs, kocoroPromptSlowestRunsLimit)
		if len(firstSemantic) > 0 {
			value := koeQualificationPercentile(firstSemantic, 0.50)
			summary.FirstSemanticP50Millis = &value
		}
		out = append(out, summary)
	}
	return out
}

func summarizeKocoroPromptVariantCells(
	results []koeQualificationRunReport,
) []kocoroPromptVariantCellSummary {
	type cellKey struct {
		name     string
		workload string
	}
	grouped := make(map[cellKey][]koeQualificationRunReport)
	for _, result := range results {
		key := cellKey{name: result.PromptVariant, workload: result.Workload}
		grouped[key] = append(grouped[key], result)
	}
	keys := make([]cellKey, 0, len(grouped))
	for key := range grouped {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].name != keys[j].name {
			return keys[i].name < keys[j].name
		}
		return keys[i].workload < keys[j].workload
	})
	out := make([]kocoroPromptVariantCellSummary, 0, len(keys))
	for _, key := range keys {
		runs := grouped[key]
		cell := kocoroPromptVariantCellSummary{
			Name: key.name, Workload: key.workload, Runs: len(runs),
		}
		var totals []int64
		var toolCorrect int
		for _, run := range runs {
			if run.TaskSuccess && run.ToolCorrectness {
				cell.SuccessfulRuns++
			}
			toolCorrect += boolInt(run.ToolCorrectness)
			cell.InputTokensMean += float64(run.InputTokens)
			cell.CompletionCallsMean += float64(run.CompletionCalls)
			cell.CostUSDTotal += run.CostUSD
			totals = append(totals, run.TotalMillis)
		}
		n := float64(len(runs))
		cell.TaskSuccessRate = float64(cell.SuccessfulRuns) / n
		cell.ToolCorrectnessRate = float64(toolCorrect) / n
		cell.InputTokensMean /= n
		cell.CompletionCallsMean /= n
		cell.TotalP50Millis = koeQualificationPercentile(totals, 0.50)
		cell.TotalP95Millis = koeQualificationPercentile(totals, 0.95)
		cell.TotalP99Millis = koeQualificationPercentile(totals, 0.99)
		cell.TotalMaxMillis = koeQualificationPercentile(totals, 1)
		out = append(out, cell)
	}
	return out
}

func kocoroPromptSlowestRuns(
	runs []koeQualificationRunReport,
	limit int,
) []kocoroPromptLatencyObservation {
	ordered := append([]koeQualificationRunReport(nil), runs...)
	sort.SliceStable(ordered, func(i, j int) bool {
		if ordered[i].TotalMillis != ordered[j].TotalMillis {
			return ordered[i].TotalMillis > ordered[j].TotalMillis
		}
		if ordered[i].Workload != ordered[j].Workload {
			return ordered[i].Workload < ordered[j].Workload
		}
		return ordered[i].Repetition < ordered[j].Repetition
	})
	if limit > len(ordered) {
		limit = len(ordered)
	}
	out := make([]kocoroPromptLatencyObservation, 0, limit)
	for _, run := range ordered[:limit] {
		out = append(out, kocoroPromptLatencyObservation{
			Workload: run.Workload, Repetition: run.Repetition,
			ScheduleIndex: run.ScheduleIndex, TotalMillis: run.TotalMillis,
		})
	}
	return out
}

func summarizeKocoroPromptPairedComparisons(
	results []koeQualificationRunReport,
) []kocoroPromptPairedComparison {
	type matchedKey struct {
		workload   string
		repetition int
	}
	matched := make(map[matchedKey]map[string]koeQualificationRunReport)
	for _, result := range results {
		key := matchedKey{workload: result.Workload, repetition: result.Repetition}
		if matched[key] == nil {
			matched[key] = make(map[string]koeQualificationRunReport)
		}
		matched[key][result.PromptVariant] = result
	}
	out := make([]kocoroPromptPairedComparison, 0, len(kocoroPromptVariantNames)-1)
	for _, candidate := range kocoroPromptVariantNames {
		if candidate == kocoroPromptVariantCurrent {
			continue
		}
		comparison := kocoroPromptPairedComparison{
			Candidate: candidate,
			Control:   kocoroPromptVariantCurrent,
		}
		var deltas []int64
		for _, variants := range matched {
			control, controlOK := variants[kocoroPromptVariantCurrent]
			candidateRun, candidateOK := variants[candidate]
			if !controlOK || !candidateOK {
				continue
			}
			delta := candidateRun.TotalMillis - control.TotalMillis
			deltas = append(deltas, delta)
			switch {
			case delta < 0:
				comparison.CandidateLatencyWins++
			case delta > 0:
				comparison.ControlLatencyWins++
			default:
				comparison.LatencyTies++
			}
		}
		comparison.Pairs = len(deltas)
		if comparison.Pairs > 0 {
			comparison.CandidateLatencyWinRate =
				float64(comparison.CandidateLatencyWins) / float64(comparison.Pairs)
			comparison.TotalDeltaP50Millis = koeQualificationPercentile(deltas, 0.50)
			comparison.TotalDeltaP95Millis = koeQualificationPercentile(deltas, 0.95)
			comparison.TotalDeltaP99Millis = koeQualificationPercentile(deltas, 0.99)
			comparison.TotalDeltaMinMillis = koeQualificationPercentile(deltas, 0)
			comparison.TotalDeltaMaxMillis = koeQualificationPercentile(deltas, 1)
		}
		out = append(out, comparison)
	}
	return out
}

func selectKocoroPromptVariant(
	summary []kocoroPromptVariantSummary,
	repetitions int,
	complete bool,
) (string, string, string) {
	if !complete {
		return "", "incomplete", "The randomized schedule has not completed."
	}
	byName := make(map[string]kocoroPromptVariantSummary, len(summary))
	for _, item := range summary {
		byName[item.Name] = item
	}
	current, ok := byName[kocoroPromptVariantCurrent]
	if !ok || !kocoroPromptVariantCorrect(current) {
		return "", "none", "The current control did not pass every correctness and runtime gate."
	}
	eligible := make([]kocoroPromptVariantSummary, 0, len(summary)-1)
	for _, item := range summary {
		if !kocoroPromptVariantProductCandidate(item.Name) ||
			!kocoroPromptVariantCorrect(item) {
			continue
		}
		if item.TotalP50Millis <= current.TotalP50Millis*110/100 &&
			item.TotalP95Millis <= current.TotalP95Millis*110/100 &&
			item.InputTokensMean <= current.InputTokensMean*0.70 {
			eligible = append(eligible, item)
		}
	}
	if len(eligible) == 0 {
		return "", "no_improvement", "No candidate preserved perfect observed correctness while staying within 10% of the current median and tail latency and reducing mean input tokens by at least 30%."
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].TotalP95Millis != eligible[j].TotalP95Millis {
			return eligible[i].TotalP95Millis < eligible[j].TotalP95Millis
		}
		if eligible[i].TotalP50Millis != eligible[j].TotalP50Millis {
			return eligible[i].TotalP50Millis < eligible[j].TotalP50Millis
		}
		if eligible[i].InputTokensMean != eligible[j].InputTokensMean {
			return eligible[i].InputTokensMean < eligible[j].InputTokensMean
		}
		return eligible[i].Name < eligible[j].Name
	})
	status := "provisional"
	if repetitions >= kocoroPromptReleaseRepetitions {
		status = "release_ready"
	} else if repetitions >= kocoroPromptComparisonRepetitions {
		status = "comparison_ready"
	}
	return eligible[0].Name, status,
		"Selected only among candidates with perfect observed correctness, at least 30% lower mean input tokens, and median and tail latency no more than 10% above current; then lowest tail latency, median latency, and input tokens."
}

func selectObservedEfficiencyLeader(
	summary []kocoroPromptVariantSummary,
	complete bool,
) (string, string) {
	if !complete {
		return "", "The randomized schedule has not completed."
	}
	eligible := make([]kocoroPromptVariantSummary, 0, len(summary))
	for _, item := range summary {
		if kocoroPromptVariantCorrect(item) {
			eligible = append(eligible, item)
		}
	}
	if len(eligible) == 0 {
		return "", "No variant passed every observed correctness and runtime gate."
	}
	sort.SliceStable(eligible, func(i, j int) bool {
		if eligible[i].TotalP95Millis != eligible[j].TotalP95Millis {
			return eligible[i].TotalP95Millis < eligible[j].TotalP95Millis
		}
		if eligible[i].TotalP50Millis != eligible[j].TotalP50Millis {
			return eligible[i].TotalP50Millis < eligible[j].TotalP50Millis
		}
		if eligible[i].InputTokensMean != eligible[j].InputTokensMean {
			return eligible[i].InputTokensMean < eligible[j].InputTokensMean
		}
		return eligible[i].Name < eligible[j].Name
	})
	return eligible[0].Name,
		"Observed efficiency leader across all variants that passed every measured correctness and runtime gate, ordered by tail latency, median latency, and mean input tokens. This is descriptive evidence, not production eligibility."
}

func kocoroPromptSummaryCorrect(summary []kocoroPromptVariantSummary, name string) bool {
	for _, item := range summary {
		if item.Name == name {
			return kocoroPromptVariantCorrect(item)
		}
	}
	return false
}

func kocoroPromptVariantProductCandidate(name string) bool {
	return name == kocoroPromptVariantLayered ||
		name == kocoroPromptVariantConditional
}

func kocoroPromptVariantCorrect(item kocoroPromptVariantSummary) bool {
	return item.TaskSuccessRate == 1 && item.ToolCorrectnessRate == 1 &&
		item.ContractFailureCount == 0 && item.RuntimeFailureCount == 0 &&
		item.DuplicateSideEffectExecutions == 0
}

func kocoroPromptVariantMetadataList() []kocoroPromptVariantMetadata {
	out := make([]kocoroPromptVariantMetadata, 0, len(kocoroPromptVariantNames))
	for _, name := range kocoroPromptVariantNames {
		text := kocoroPromptVariantText(name)
		item := kocoroPromptVariantMetadata{
			Name: name, ProductCandidate: kocoroPromptVariantProductCandidate(name),
			Role: "product_candidate",
		}
		if name == kocoroPromptVariantCurrent {
			item.Source = "production_assembled_system"
			item.Role = "control"
		} else if name == kocoroPromptVariantMinimal {
			item.Source = "embedded_fixture"
			item.Role = "stress_control"
		} else {
			item.Source = "embedded_fixture"
			if name == kocoroPromptVariantConditional {
				item.Source = "embedded_core_with_workload_conditional_fixtures"
				text += "\n\n" + strings.TrimSpace(kocoroPromptExactOutputPolicyV1) +
					"\n\n" + strings.TrimSpace(kocoroPromptWebPolicyV1) +
					"\n\n" + strings.TrimSpace(kocoroPromptEmptyResultPolicyV1)
			}
			item.FixtureChars = len([]rune(text))
			sum := sha256.Sum256([]byte(text))
			item.FixtureHash = fmt.Sprintf("%x", sum[:8])
		}
		out = append(out, item)
	}
	return out
}

func kocoroPromptReportedCost(results []koeQualificationRunReport) float64 {
	var total float64
	for _, result := range results {
		total += result.CostUSD
	}
	return total
}

func kocoroPromptCostObserved(results []koeQualificationRunReport) bool {
	if len(results) == 0 {
		return false
	}
	for _, result := range results {
		if result.CompletionCalls > 0 && !result.CostObserved && result.RuntimeErrorClass == "" {
			return false
		}
	}
	return true
}

func writeKocoroPromptExperimentReport(
	t *testing.T,
	path string,
	cfg kocoroPromptExperimentConfig,
	jobs []koeQualificationJob,
	results []koeQualificationRunReport,
	complete bool,
) {
	t.Helper()
	report := newKocoroPromptExperimentReport(cfg, jobs, results, complete)
	if err := writeKoeQualificationJSON(path, report); err != nil {
		t.Fatalf("write prompt comparison report: %v", err)
	}
}
