//go:build live

package tools

import (
	"context"
	"errors"
	"os"
	"testing"
)

const (
	koeQualificationDiagnosticGateEnv     = "KOE_FAST_QUALIFICATION_DIAGNOSTIC_LIVE"
	koeQualificationDiagnosticLaneEnv     = "KOE_FAST_QUALIFICATION_DIAGNOSTIC_LANE"
	koeQualificationDiagnosticWorkloadEnv = "KOE_FAST_QUALIFICATION_DIAGNOSTIC_WORKLOAD"
	koeQualificationDiagnosticMinRate     = 0.90
)

func TestKoeQualificationDiagnosticReportIgnoresFormalScheduleShape(t *testing.T) {
	cfg := koeQualificationRuntimeConfig{
		repetitions: 1,
		maxCostUSD:  1,
	}
	jobs := []koeQualificationJob{{
		Lane:       koeQualificationFastLane,
		Workload:   "serial_3",
		Repetition: 1,
	}}
	results := []koeQualificationRunReport{{
		Lane:             koeQualificationFastLane,
		Workload:         "serial_3",
		Repetition:       1,
		RouteExact:       true,
		ProviderExact:    true,
		ModelExact:       true,
		CachePolicyExact: true,
		CompletionCalls:  1,
		CostObserved:     true,
	}}

	report := newKoeQualificationDiagnosticReport(
		cfg,
		jobs,
		results,
		true,
	)
	if !report.Complete {
		t.Fatal("diagnostic report should be complete")
	}
	if report.ContractFailureCount != 0 {
		t.Fatalf(
			"diagnostic contract failures = %d, want 0",
			report.ContractFailureCount,
		)
	}
	if report.GatePassed != nil || report.CorrectnessGatePassed != nil ||
		report.PerformanceGatePassed != nil {
		t.Fatal("diagnostic report must not claim formal gate results")
	}
}

// TestKoeFastQualificationLive_DiagnosticCell reruns one paid qualification
// cell without spending time or budget on the other thirteen cells. It writes
// the same content-free report schema as the formal matrix, but evaluates only
// hard runtime/contract/duplicate-side-effect invariants plus the cell's 90%
// task/tool floor. It is never a substitute for the formal cross-lane gate.
func TestKoeFastQualificationLive_DiagnosticCell(t *testing.T) {
	if os.Getenv(koeQualificationDiagnosticGateEnv) != "1" {
		t.Skip("set KOE_FAST_QUALIFICATION_DIAGNOSTIC_LIVE=1 to run a paid diagnostic cell")
	}
	cfg := loadKoeQualificationRuntimeConfig(t)
	lane := os.Getenv(koeQualificationDiagnosticLaneEnv)
	if lane == "" {
		lane = koeQualificationFastLane
	}
	if lane != koeQualificationFastLane &&
		lane != koeQualificationSonnetReferenceLane {
		t.Fatalf("unsupported diagnostic lane %q", lane)
	}
	workload := os.Getenv(koeQualificationDiagnosticWorkloadEnv)
	if workload == "" {
		workload = "serial_3"
	}
	found := false
	for _, candidate := range koeQualificationWorkloadNames {
		if candidate == workload {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("unsupported diagnostic workload %q", workload)
	}

	jobs := make([]koeQualificationJob, 0, cfg.repetitions)
	for repetition := 1; repetition <= cfg.repetitions; repetition++ {
		jobs = append(jobs, koeQualificationJob{
			Lane:       lane,
			Workload:   workload,
			Repetition: repetition,
			Token:      koeQualificationToken(cfg.seed, workload, repetition),
		})
	}

	ctx, cancel := context.WithTimeout(
		context.Background(),
		koeQualificationTotalTimeout,
	)
	defer cancel()
	results := make([]koeQualificationRunReport, 0, len(jobs))
	for index, job := range jobs {
		if err := ctx.Err(); err != nil {
			t.Fatalf("diagnostic cell timed out after %d/%d: %v", index, len(jobs), err)
		}
		result := runKoeQualificationJob(ctx, cfg, job)
		result.ScheduleIndex = index + 1
		results = append(results, result)
		report := newKoeQualificationDiagnosticReport(
			cfg,
			jobs,
			results,
			false,
		)
		if err := writeKoeQualificationReport(cfg.outputPath+".partial", report); err != nil {
			t.Fatalf("write diagnostic partial: %v", err)
		}
		if report.ReportedCostUSD > report.MaxCostUSD {
			t.Fatalf(
				"diagnostic cost budget exceeded: reported=%.6f max=%.6f",
				report.ReportedCostUSD,
				report.MaxCostUSD,
			)
		}
		if index+1 < len(jobs) {
			waitKoeQualificationPause(t, ctx, cfg.pause)
		}
	}

	report := newKoeQualificationDiagnosticReport(cfg, jobs, results, true)
	if err := writeKoeQualificationReport(cfg.outputPath, report); err != nil {
		t.Fatalf("write diagnostic report: %v", err)
	}
	if err := os.Remove(cfg.outputPath + ".partial"); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		t.Fatalf("remove diagnostic partial: %v", err)
	}
	if len(report.Summary) != 1 {
		t.Fatalf("diagnostic summaries = %d, want 1", len(report.Summary))
	}
	summary := report.Summary[0]
	t.Logf(
		"diagnostic cell lane=%s workload=%s runs=%d task=%.3f tool=%.3f runtime=%d contract=%d duplicate_side_effect=%d cost=%.6f report=%s",
		lane,
		workload,
		summary.Runs,
		summary.TaskSuccessRate,
		summary.ToolCorrectnessRate,
		report.RuntimeFailureCount,
		report.ContractFailureCount,
		report.DuplicateSideEffectFailures,
		report.ReportedCostUSD,
		cfg.outputPath,
	)
	if report.RuntimeFailureCount != 0 ||
		report.ContractFailureCount != 0 ||
		report.DuplicateSideEffectFailures != 0 ||
		summary.TaskSuccessRate < koeQualificationDiagnosticMinRate ||
		summary.ToolCorrectnessRate < koeQualificationDiagnosticMinRate {
		t.Errorf(
			"diagnostic cell failed: task=%.3f tool=%.3f runtime=%d contract=%d duplicate_side_effect=%d",
			summary.TaskSuccessRate,
			summary.ToolCorrectnessRate,
			report.RuntimeFailureCount,
			report.ContractFailureCount,
			report.DuplicateSideEffectFailures,
		)
	}
}

func newKoeQualificationDiagnosticReport(
	cfg koeQualificationRuntimeConfig,
	jobs []koeQualificationJob,
	results []koeQualificationRunReport,
	complete bool,
) koeQualificationReport {
	// A one-cell diagnostic intentionally does not satisfy the formal matrix's
	// fourteen-cell schedule contract. Evaluate the per-run route/profile,
	// runtime, duplicate-side-effect, and cost invariants without applying the
	// formal schedule or cross-lane performance gates.
	report := newKoeQualificationReport(cfg, jobs, results, false)
	report.Complete = complete
	report.PerformanceNote =
		"diagnostic cell only; formal schedule and performance gates not evaluated"
	return report
}
