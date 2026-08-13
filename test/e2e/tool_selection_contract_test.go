package e2e

import (
	"fmt"
	"testing"
)

const (
	toolChoiceComparisonRepetitions = 3
	toolChoiceReleaseRepetitions    = 5
)

type toolChoiceRun struct {
	Case          string         `json:"case"`
	Repetition    int            `json:"repetition"`
	ScheduleIndex int            `json:"schedule_index"`
	Correct       bool           `json:"correct"`
	Failures      []string       `json:"failures"`
	ToolCounts    map[string]int `json:"tool_counts"`
	ToolFlow      []string       `json:"tool_flow"`
	LatencyMillis int64          `json:"latency_millis"`
	CostUSD       float64        `json:"cost_usd"`
	CostObserved  bool           `json:"cost_observed"`
	Answer        string         `json:"answer"`
}

type toolChoiceReport struct {
	SchemaVersion        string          `json:"schema_version"`
	GeneratedAt          string          `json:"generated_at"`
	Complete             bool            `json:"complete"`
	Sample               string          `json:"sample"`
	RepetitionsPerCase   int             `json:"repetitions_per_case"`
	Seed                 int64           `json:"seed"`
	Scheduled            int             `json:"scheduled"`
	Completed            int             `json:"completed"`
	CorrectRuns          int             `json:"correct_runs"`
	ComparisonQualifying bool            `json:"comparison_qualifying"`
	ReleaseQualifying    bool            `json:"release_qualifying"`
	ReportedCostUSD      float64         `json:"reported_cost_usd"`
	MaxCostUSD           float64         `json:"max_cost_usd"`
	CostObserved         bool            `json:"cost_observed"`
	Runs                 []toolChoiceRun `json:"runs"`
}

func toolChoiceCaseNames() []string {
	return []string{
		"exact_file_read",
		"dedicated_content_search",
		"directory_listing",
		"file_write_effect",
		"shell_automation",
		"no_tool_rewrite",
	}
}

func finalizeToolChoiceReport(report *toolChoiceReport) {
	report.Completed = len(report.Runs)
	report.Complete = report.Completed == report.Scheduled
	report.CorrectRuns = 0
	report.ReportedCostUSD = 0
	report.CostObserved = len(report.Runs) > 0
	for _, run := range report.Runs {
		if run.Correct {
			report.CorrectRuns++
		}
		report.ReportedCostUSD += run.CostUSD
		report.CostObserved = report.CostObserved && run.CostObserved
	}
	coverageComplete := toolChoiceCoverageComplete(report.RepetitionsPerCase, report.Scheduled, report.Runs)
	allCorrect := report.Complete && coverageComplete && report.Scheduled > 0 && report.CorrectRuns == report.Scheduled && report.ReportedCostUSD <= report.MaxCostUSD
	report.ComparisonQualifying = allCorrect && report.CostObserved && report.RepetitionsPerCase >= toolChoiceComparisonRepetitions
	report.ReleaseQualifying = allCorrect && report.CostObserved && report.Sample == "release" && report.RepetitionsPerCase >= toolChoiceReleaseRepetitions
}

func toolChoiceCoverageComplete(repetitions, scheduled int, runs []toolChoiceRun) bool {
	caseNames := toolChoiceCaseNames()
	if repetitions < 1 || scheduled != len(caseNames)*repetitions || len(runs) != scheduled {
		return false
	}
	knownCases := make(map[string]bool, len(caseNames))
	for _, name := range caseNames {
		knownCases[name] = true
	}
	seen := make(map[string]bool, len(runs))
	for _, run := range runs {
		if !knownCases[run.Case] || run.Repetition < 1 || run.Repetition > repetitions {
			return false
		}
		cell := fmt.Sprintf("%s/%d", run.Case, run.Repetition)
		if seen[cell] {
			return false
		}
		seen[cell] = true
	}
	return len(seen) == scheduled
}

func TestOffline_ToolChoiceQualificationFailsClosed(t *testing.T) {
	passingRuns := func(repetitions int) []toolChoiceRun {
		caseNames := toolChoiceCaseNames()
		runs := make([]toolChoiceRun, 0, len(caseNames)*repetitions)
		for repetition := 1; repetition <= repetitions; repetition++ {
			for _, name := range caseNames {
				runs = append(runs, toolChoiceRun{
					Case: name, Repetition: repetition,
					Correct: true, CostUSD: 0.01, CostObserved: true,
				})
			}
		}
		return runs
	}
	comparisonRuns := passingRuns(toolChoiceComparisonRepetitions)
	comparison := toolChoiceReport{
		Complete: true, Sample: "comparison", RepetitionsPerCase: toolChoiceComparisonRepetitions,
		Scheduled: len(comparisonRuns), MaxCostUSD: 8, Runs: comparisonRuns,
	}
	finalizeToolChoiceReport(&comparison)
	if !comparison.ComparisonQualifying || comparison.ReleaseQualifying {
		t.Fatalf("comparison qualification=%+v", comparison)
	}
	releaseRuns := passingRuns(toolChoiceReleaseRepetitions)
	release := toolChoiceReport{
		Complete: true, Sample: "release", RepetitionsPerCase: toolChoiceReleaseRepetitions,
		Scheduled: len(releaseRuns), MaxCostUSD: 8, Runs: releaseRuns,
	}
	finalizeToolChoiceReport(&release)
	if !release.ReleaseQualifying {
		t.Fatal("complete all-correct release report did not qualify")
	}
	for name, mutate := range map[string]func(*toolChoiceReport){
		"incomplete": func(report *toolChoiceReport) { report.Runs = report.Runs[:len(report.Runs)-1] },
		"incorrect":  func(report *toolChoiceReport) { report.Runs[0].Correct = false },
		"over_cost": func(report *toolChoiceReport) {
			report.Runs[0].CostUSD = report.MaxCostUSD + 0.01
		},
		"undersized": func(report *toolChoiceReport) { report.RepetitionsPerCase = toolChoiceReleaseRepetitions - 1 },
		"no_cost": func(report *toolChoiceReport) {
			report.Runs[0].CostObserved = false
		},
		"duplicate_cell": func(report *toolChoiceReport) {
			report.Runs[0].Case = report.Runs[1].Case
			report.Runs[0].Repetition = report.Runs[1].Repetition
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := release
			candidate.Runs = append([]toolChoiceRun(nil), release.Runs...)
			mutate(&candidate)
			finalizeToolChoiceReport(&candidate)
			if candidate.ReleaseQualifying {
				t.Fatal("invalid report unexpectedly release-qualified")
			}
		})
	}
}
