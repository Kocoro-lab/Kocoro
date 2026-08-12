package e2e

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/executionprofile"
)

const (
	fastPinnedRequalifyGateEnv   = "KOCORO_FAST_PINNED_REQUALIFY"
	fastPinnedRequalifyInputEnv  = "KOCORO_FAST_PINNED_REQUALIFY_INPUT"
	fastPinnedRequalifyOutputEnv = "KOCORO_FAST_PINNED_REQUALIFY_OUTPUT"
)

type fastPinnedCacheRecordingClient struct {
	requests []client.CompletionRequest
	cached   bool
}

func (c *fastPinnedCacheRecordingClient) Complete(
	_ context.Context,
	req client.CompletionRequest,
) (*client.CompletionResponse, error) {
	c.requests = append(c.requests, req)
	return &client.CompletionResponse{Cached: c.cached}, nil
}

func (c *fastPinnedCacheRecordingClient) CompleteStream(
	_ context.Context,
	req client.CompletionRequest,
	_ func(client.StreamDelta),
) (*client.CompletionResponse, error) {
	c.requests = append(c.requests, req)
	return &client.CompletionResponse{Cached: c.cached}, nil
}

func TestOffline_FastPinnedTrialsDisableWholeResponseCache(t *testing.T) {
	first := fastPinnedTrialID(7, "case_a", 1)
	if first != fastPinnedTrialID(7, "case_a", 1) || first == fastPinnedTrialID(7, "case_a", 2) {
		t.Fatalf("trial IDs are not stable and repetition-distinct: %q", first)
	}
	if !strings.Contains(fastPinnedTrialPrompt("task", first), first) {
		t.Fatal("trial prompt does not carry its identity")
	}

	inner := &fastPinnedCacheRecordingClient{}
	wrapped := &fastPinnedCacheOffClient{inner: inner}
	if _, err := wrapped.Complete(context.Background(), client.CompletionRequest{}); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if len(inner.requests) != 1 || inner.requests[0].ResponseCachePolicy != executionprofile.ResponseCacheOff {
		t.Fatalf("request cache policy = %+v, want off", inner.requests)
	}
	if isolated, cached := wrapped.observations(); !isolated || cached {
		t.Fatalf("cache observations isolated=%t cached=%t", isolated, cached)
	}

	cachedInner := &fastPinnedCacheRecordingClient{cached: true}
	cachedWrapped := &fastPinnedCacheOffClient{inner: cachedInner}
	if _, err := cachedWrapped.CompleteStream(context.Background(), client.CompletionRequest{}, func(client.StreamDelta) {}); err != nil {
		t.Fatalf("complete stream: %v", err)
	}
	if isolated, cached := cachedWrapped.observations(); isolated || !cached {
		t.Fatalf("cached response was not rejected: isolated=%t cached=%t", isolated, cached)
	}
}

func TestOffline_FastPinnedQualificationFailsClosed(t *testing.T) {
	const releaseRepetitions = agentLabQualityReleaseRepetitions
	caseNames := []string{"case_a", "case_b"}
	passing := fastPinnedQualificationFixture(caseNames, releaseRepetitions)

	t.Run("complete release sample qualifies", func(t *testing.T) {
		report := newFastPinnedQualificationReport(
			"fixture.v1", 1, "release", releaseRepetitions,
			caseNames, passing, 1, 5,
		)
		if !report.Complete || !report.ComparisonQualifying || !report.ReleaseQualifying {
			t.Fatalf("complete release fixture did not qualify: %+v", report)
		}
		for _, cell := range report.Cells {
			if cell.MedianMillis != 800 || cell.P90Millis != 1400 || cell.MeanLLMCalls != 8 {
				t.Fatalf("cell observability was not preserved: %+v", cell)
			}
		}
		body, err := json.Marshal(report)
		if err != nil {
			t.Fatalf("marshal report: %v", err)
		}
		for _, field := range []string{`"median_ms":800`, `"p90_ms":1400`, `"mean_llm_calls":8`} {
			if !strings.Contains(string(body), field) {
				t.Fatalf("machine-readable report missing %s: %s", field, body)
			}
		}
	})

	t.Run("smoke sample cannot release qualify", func(t *testing.T) {
		report := newFastPinnedQualificationReport(
			"fixture.v1", 1, "smoke", releaseRepetitions,
			caseNames, passing, 1, 5,
		)
		if !report.ComparisonQualifying || report.ReleaseQualifying {
			t.Fatalf("smoke qualification=%+v", report)
		}
	})

	t.Run("Fast 90 of 90 can release qualify when Full is 89 of 90 correct", func(t *testing.T) {
		armCases := []string{"case_a", "case_b", "case_c", "case_d", "case_e", "case_f"}
		runs := fastPinnedQualificationFixture(armCases, releaseRepetitions)
		for index := range runs {
			if runs[index].Arm == "full" {
				runs[index].Correct = false
				runs[index].Failures = []string{"fixture_failure"}
				break
			}
		}
		report := newFastPinnedQualificationReport(
			"fixture.v3", 1, "release", releaseRepetitions,
			armCases, runs, 1, 5,
		)
		fast := fastPinnedArm(t, report, "fast")
		full := fastPinnedArm(t, report, "full")
		if fast.Scheduled != 90 || fast.Completed != 90 || fast.CorrectRuns != 90 ||
			!fast.Complete || !fast.Correct || !fast.ObservabilityObserved ||
			!fast.ComparisonQualifying || !fast.ReleaseQualifying || !report.FastReleaseQualifying {
			t.Fatalf("Fast arm did not independently qualify: %+v", fast)
		}
		if full.Scheduled != 90 || full.Completed != 90 || full.CorrectRuns != 89 ||
			!full.Complete || full.Correct || full.ComparisonQualifying ||
			full.ReleaseQualifying || report.FullReleaseQualifying {
			t.Fatalf("incorrect Full arm qualified: %+v", full)
		}
		if !report.Complete || report.ComparisonQualifying || report.ReleaseQualifying {
			t.Fatalf("legacy global qualification changed semantics: %+v", report)
		}
	})

	t.Run("missing Fast cell blocks Fast release", func(t *testing.T) {
		armCases := []string{"case_a", "case_b", "case_c", "case_d", "case_e", "case_f"}
		runs := fastPinnedQualificationFixture(armCases, releaseRepetitions)
		for index, run := range runs {
			if run.Arm == "fast" {
				runs = append(runs[:index], runs[index+1:]...)
				break
			}
		}
		report := newFastPinnedQualificationReport(
			"fixture.v3", 1, "release", releaseRepetitions,
			armCases, runs, 1, 5,
		)
		fast := fastPinnedArm(t, report, "fast")
		if fast.Complete || fast.ComparisonQualifying || fast.ReleaseQualifying || report.FastReleaseQualifying {
			t.Fatalf("incomplete Fast arm qualified: %+v", fast)
		}
	})

	t.Run("missing Fast observability blocks Fast release", func(t *testing.T) {
		armCases := []string{"case_a", "case_b", "case_c", "case_d", "case_e", "case_f"}
		runs := fastPinnedQualificationFixture(armCases, releaseRepetitions)
		for index := range runs {
			if runs[index].Arm == "fast" {
				runs[index].UsageObserved = false
				break
			}
		}
		report := newFastPinnedQualificationReport(
			"fixture.v3", 1, "release", releaseRepetitions,
			armCases, runs, 1, 5,
		)
		fast := fastPinnedArm(t, report, "fast")
		if !fast.Complete || fast.ObservabilityObserved || fast.ComparisonQualifying ||
			fast.ReleaseQualifying || report.FastReleaseQualifying {
			t.Fatalf("unobserved Fast arm qualified: %+v", fast)
		}
	})

	t.Run("lane cost ceiling blocks arm release", func(t *testing.T) {
		report := newFastPinnedQualificationReport(
			"fixture.v3", 1, "release", releaseRepetitions,
			caseNames, passing, 6, 5,
		)
		if report.FastReleaseQualifying || report.FullReleaseQualifying {
			t.Fatalf("cost ceiling did not block arm release: %+v", report.Arms)
		}
	})

	tests := []struct {
		name         string
		mutate       func([]fastABRun) []fastABRun
		want         string
		wantComplete bool
	}{
		{
			name: "incorrect run",
			mutate: func(runs []fastABRun) []fastABRun {
				runs[0].Correct = false
				runs[0].Failures = []string{"fixture_failure"}
				return runs
			},
			want:         "incorrect_runs",
			wantComplete: true,
		},
		{
			name: "missing cell run",
			mutate: func(runs []fastABRun) []fastABRun {
				return runs[1:]
			},
			want: "incomplete_cell",
		},
		{
			name: "duplicate masks missing cell",
			mutate: func(runs []fastABRun) []fastABRun {
				runs[0] = runs[1]
				return runs
			},
			want: "duplicate_repetition",
		},
		{
			name: "usage missing",
			mutate: func(runs []fastABRun) []fastABRun {
				runs[0].UsageObserved = false
				return runs
			},
			want:         "usage_not_observed",
			wantComplete: true,
		},
		{
			name: "cost missing",
			mutate: func(runs []fastABRun) []fastABRun {
				runs[0].CostObserved = false
				return runs
			},
			want:         "cost_not_observed",
			wantComplete: true,
		},
		{
			name: "response cache isolation missing",
			mutate: func(runs []fastABRun) []fastABRun {
				runs[0].CachePolicy = ""
				return runs
			},
			want:         "response_cache_not_isolated",
			wantComplete: true,
		},
		{
			name: "cached response observed",
			mutate: func(runs []fastABRun) []fastABRun {
				runs[0].Cached = true
				return runs
			},
			want:         "response_cache_not_isolated",
			wantComplete: true,
		},
		{
			name: "trial identity mismatch",
			mutate: func(runs []fastABRun) []fastABRun {
				runs[0].TrialID = "wrong"
				return runs
			},
			want: "trial_id_mismatch",
		},
		{
			name: "terminal answer missing",
			mutate: func(runs []fastABRun) []fastABRun {
				runs[0].Answer = ""
				return runs
			},
			want:         "terminal_answer_missing",
			wantComplete: true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			runs := append([]fastABRun(nil), passing...)
			runs = tc.mutate(runs)
			report := newFastPinnedQualificationReport(
				"fixture.v1", 1, "release", releaseRepetitions,
				caseNames, runs, 1, 5,
			)
			if report.Complete != tc.wantComplete || report.ComparisonQualifying || report.ReleaseQualifying {
				t.Fatalf("invalid fixture qualified: %+v", report)
			}
			if !containsFastPinnedFailure(report.QualificationFailures, tc.want) {
				t.Fatalf("failures %v do not contain %q", report.QualificationFailures, tc.want)
			}
		})
	}

	t.Run("undersized release sample cannot qualify", func(t *testing.T) {
		repetitions := agentLabQualityReleaseRepetitions - 1
		runs := fastPinnedQualificationFixture(caseNames, repetitions)
		report := newFastPinnedQualificationReport(
			"fixture.v1", 1, "release", repetitions,
			caseNames, runs, 1, 5,
		)
		if !report.Complete || !report.ComparisonQualifying || report.ReleaseQualifying {
			t.Fatalf("undersized release qualification=%+v", report)
		}
	})

	t.Run("report writer replaces an existing target", func(t *testing.T) {
		dir := t.TempDir()
		path := filepath.Join(dir, "report.json")
		if err := os.WriteFile(path, []byte("stale"), 0o600); err != nil {
			t.Fatalf("write stale report: %v", err)
		}
		report := newFastPinnedQualificationReport(
			"fixture.v2", 1, "release", releaseRepetitions,
			caseNames, passing, 1, 5,
		)
		if err := writeFastPinnedQualificationReport(path, report); err != nil {
			t.Fatalf("replace existing report: %v", err)
		}
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read replaced report: %v", err)
		}
		if !strings.Contains(string(body), `"schema_version": "fixture.v2"`) {
			t.Fatalf("existing target was not replaced: %s", body)
		}
		matches, err := filepath.Glob(filepath.Join(dir, ".tmp-*"))
		if err != nil {
			t.Fatalf("glob atomic-write leftovers: %v", err)
		}
		if len(matches) != 0 {
			t.Fatalf("atomic writer left temp files: %v", matches)
		}
	})
}

func TestOffline_FastPinnedV4RequiresObservedEfficientTrajectory(t *testing.T) {
	caseNames := []string{"multi_file_synthesis", "constrained_schedule"}
	runs := fastPinnedQualificationFixture(caseNames, agentLabQualityReleaseRepetitions)
	for index := range runs {
		run := &runs[index]
		run.LLMCalls = 2
		run.Status = fastPinnedRunStatus{IterationCount: 2}
		run.TrajectoryObserved = true
		run.LoopEvents = []agent.RunTraceEvent{
			{
				Seq: 1, Iteration: 1, Type: agent.RunTraceEventModelResponse,
				Model: &agent.RunTraceModelResponse{Attempt: 1, FinishReason: "tool_use"},
			},
			{
				Seq: 2, Iteration: 2, Type: agent.RunTraceEventTerminal,
				Terminal: &agent.RunTraceTerminal{IterationCount: 2},
			},
		}
		if run.Case == "multi_file_synthesis" {
			run.ToolTrajectory = fastPinnedTrace(1, 0, 3, "file_read", "file_read", "file_read")
		}
		run.Efficiency = evaluateFastPinnedEfficiency(run.Case, run.LLMCalls, run.Status, run.ToolTrajectory)
		if !run.Efficiency.Qualifying {
			t.Fatalf("fixture efficiency failed: %+v", run.Efficiency)
		}
	}

	report := newFastPinnedQualificationReport(
		"fixture.v4", 1, "release", agentLabQualityReleaseRepetitions,
		caseNames, runs, 1, 5,
	)
	if !report.FastReleaseQualifying || !report.FullReleaseQualifying ||
		!report.TrajectoryObserved || !report.EfficiencyObserved || !report.EfficiencyQualifying ||
		!report.FastPerformanceObserved || !report.FastPerformanceQualifying {
		t.Fatalf("observed efficient v4 fixture did not qualify: %+v", report)
	}

	regressed := append([]fastABRun(nil), runs...)
	for index := range regressed {
		if regressed[index].Arm == "fast" {
			regressed[index].LatencyMillis *= 2
			regressed[index].CostUSD *= 2
		}
	}
	regressedReport := newFastPinnedQualificationReport(
		"fixture.v4", 1, "release", agentLabQualityReleaseRepetitions,
		caseNames, regressed, 1, 5,
	)
	if !regressedReport.Complete || regressedReport.FastReleaseQualifying || regressedReport.FastPerformanceQualifying ||
		!containsFastPinnedFailure(regressedReport.QualificationFailures, "fast_relative_performance_regressed") {
		t.Fatalf("slower and more expensive Fast arm qualified: %+v", regressedReport)
	}

	missingTrace := append([]fastABRun(nil), runs...)
	missingTrace[0].TrajectoryObserved = false
	missingTrace[0].ToolTrajectory = nil
	missingReport := newFastPinnedQualificationReport(
		"fixture.v4", 1, "release", agentLabQualityReleaseRepetitions,
		caseNames, missingTrace, 1, 5,
	)
	if missingReport.FastReleaseQualifying || missingReport.ReleaseQualifying ||
		!containsFastPinnedFailure(missingReport.QualificationFailures, "trajectory_not_observed") {
		t.Fatalf("missing trajectory qualified: %+v", missingReport)
	}

	mismatchedTerminal := append([]fastABRun(nil), runs...)
	mismatchedTerminal[0].LoopEvents = append([]agent.RunTraceEvent(nil), runs[0].LoopEvents...)
	mismatchedTerminal[0].LoopEvents[1].Terminal = &agent.RunTraceTerminal{Partial: true, IterationCount: 2}
	mismatchReport := newFastPinnedQualificationReport(
		"fixture.v4", 1, "release", agentLabQualityReleaseRepetitions,
		caseNames, mismatchedTerminal, 1, 5,
	)
	if mismatchReport.ReleaseQualifying || mismatchReport.TrajectoryObserved {
		t.Fatalf("terminal trace mismatch qualified: %+v", mismatchReport)
	}

	legacyRuns := fastPinnedQualificationFixture(caseNames, agentLabQualityReleaseRepetitions)
	legacyAsV4 := newFastPinnedQualificationReport(
		"fixture.v4", 1, "release", agentLabQualityReleaseRepetitions,
		caseNames, legacyRuns, 1, 5,
	)
	if legacyAsV4.FastReleaseQualifying || legacyAsV4.FullReleaseQualifying ||
		legacyAsV4.ReleaseQualifying || legacyAsV4.TrajectoryObserved || legacyAsV4.EfficiencyObserved {
		t.Fatalf("v3-style evidence gained v4 qualification: %+v", legacyAsV4)
	}
}

// TestOffline_FastPinnedRequalifyV2Artifact recalculates legacy comparison
// evidence solely from an existing report's raw runs. A legacy artifact can
// never gain current release qualification. It never constructs an LLM client.
//
//	KOCORO_FAST_PINNED_REQUALIFY=1 KOCORO_FAST_PINNED_REQUALIFY_INPUT=/path/report-v2.json KOCORO_FAST_PINNED_REQUALIFY_OUTPUT=/path/report-v3.json go test ./test/e2e -run '^TestOffline_FastPinnedRequalifyV2Artifact$' -count=1 -v
func TestOffline_FastPinnedRequalifyV2Artifact(t *testing.T) {
	if os.Getenv(fastPinnedRequalifyGateEnv) != "1" {
		t.Skipf("set %s=1 with input/output paths to requalify a v2 artifact", fastPinnedRequalifyGateEnv)
	}
	inputPath := strings.TrimSpace(os.Getenv(fastPinnedRequalifyInputEnv))
	outputPath := strings.TrimSpace(os.Getenv(fastPinnedRequalifyOutputEnv))
	if inputPath == "" || outputPath == "" {
		t.Fatalf("%s and %s are required", fastPinnedRequalifyInputEnv, fastPinnedRequalifyOutputEnv)
	}
	inputAbs, err := filepath.Abs(inputPath)
	if err != nil {
		t.Fatalf("resolve input path: %v", err)
	}
	outputAbs, err := filepath.Abs(outputPath)
	if err != nil {
		t.Fatalf("resolve output path: %v", err)
	}
	if inputAbs == outputAbs {
		t.Fatal("requalify output must not overwrite the source v2 artifact")
	}
	body, err := os.ReadFile(inputAbs)
	if err != nil {
		t.Fatalf("read v2 artifact: %v", err)
	}
	var source fastPinnedQualificationReport
	if err := json.Unmarshal(body, &source); err != nil {
		t.Fatalf("decode v2 artifact: %v", err)
	}
	if !strings.HasSuffix(source.SchemaVersion, ".v2") {
		t.Fatalf("source schema %q is not v2", source.SchemaVersion)
	}
	if source.RepetitionsPerCell < 1 || len(source.Runs) == 0 || len(source.Cells) == 0 {
		t.Fatalf("v2 artifact lacks raw qualification evidence: repetitions=%d runs=%d cells=%d",
			source.RepetitionsPerCell, len(source.Runs), len(source.Cells))
	}
	caseSet := map[string]bool{}
	for _, cell := range source.Cells {
		if cell.Case != "" {
			caseSet[cell.Case] = true
		}
	}
	caseNames := make([]string, 0, len(caseSet))
	for caseName := range caseSet {
		caseNames = append(caseNames, caseName)
	}
	if len(caseNames) == 0 {
		t.Fatal("v2 artifact contains no expected cases")
	}
	totalCost := 0.0
	for _, run := range source.Runs {
		totalCost += run.CostUSD
	}
	requalified := newFastPinnedQualificationReport(
		strings.TrimSuffix(source.SchemaVersion, ".v2")+".v3",
		source.Seed, "legacy_requalified", source.RepetitionsPerCell,
		caseNames, source.Runs, totalCost, source.MaxCostUSD,
	)
	if err := writeFastPinnedQualificationReport(outputAbs, requalified); err != nil {
		t.Fatalf("write v3 report: %v", err)
	}
	if requalified.FastReleaseQualifying || requalified.FullReleaseQualifying || requalified.ReleaseQualifying {
		t.Fatalf("legacy evidence gained release qualification: %+v", requalified)
	}
	if !fastPinnedArm(t, requalified, "fast").ComparisonQualifying {
		t.Fatalf("legacy Fast comparison evidence was not preserved: %+v", requalified)
	}
	t.Logf("requalified legacy v2 comparison evidence: release_qualifying=false report=%s", outputAbs)
}

func fastPinnedQualificationFixture(caseNames []string, repetitions int) []fastABRun {
	runs := make([]fastABRun, 0, len(caseNames)*2*repetitions)
	for _, caseName := range caseNames {
		for _, arm := range []string{"fast", "full"} {
			for repetition := 1; repetition <= repetitions; repetition++ {
				runs = append(runs, fastABRun{
					Case: caseName, Arm: arm, Repetition: repetition,
					TrialID:     fastPinnedTrialID(1, caseName, repetition),
					CachePolicy: string(executionprofile.ResponseCacheOff),
					Correct:     true, LatencyMillis: int64(repetition * 100),
					LLMCalls: repetition, TotalTokens: 15, CostUSD: 0.001,
					UsageObserved: true, CostObserved: true, Answer: "done",
				})
			}
		}
	}
	return runs
}

func containsFastPinnedFailure(failures []string, want string) bool {
	for _, failure := range failures {
		if strings.Contains(failure, want) {
			return true
		}
	}
	return false
}

func fastPinnedArm(t *testing.T, report fastPinnedQualificationReport, want string) fastPinnedArmSummary {
	t.Helper()
	for _, arm := range report.Arms {
		if arm.Arm == want {
			return arm
		}
	}
	t.Fatalf("report has no %s arm: %+v", want, report.Arms)
	return fastPinnedArmSummary{}
}
