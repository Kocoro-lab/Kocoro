package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
)

type loopAcceptanceCorpus struct {
	SchemaVersion string                `json:"schema_version"`
	Suite         string                `json:"suite"`
	Traces        []loopAcceptanceTrace `json:"traces"`
}

type loopAcceptanceTrace struct {
	ID          string                    `json:"id"`
	Class       string                    `json:"class"`
	Expectation loopAcceptanceExpectation `json:"expectation"`
	Events      []loopAcceptanceEvent     `json:"events"`
}

type loopAcceptanceExpectation struct {
	FirstNudgeMin  int `json:"first_nudge_min"`
	FirstNudgeMax  int `json:"first_nudge_max"`
	FirstForceMin  int `json:"first_force_stop_min"`
	FirstForceMax  int `json:"first_force_stop_max"`
	IdealStopAfter int `json:"ideal_stop_after,omitempty"`
	MaxWastedCalls int `json:"max_wasted_calls,omitempty"`
}

type loopAcceptanceEvent struct {
	Name            string `json:"name"`
	Args            string `json:"args"`
	IsError         bool   `json:"is_error,omitempty"`
	Error           string `json:"error,omitempty"`
	ResultSig       string `json:"result_sig,omitempty"`
	OutcomeSig      string `json:"outcome_sig,omitempty"`
	IsReadOnly      bool   `json:"is_read_only,omitempty"`
	IsNonActionable bool   `json:"is_non_actionable,omitempty"`
}

type loopAcceptanceTraceResult struct {
	ID             string `json:"id"`
	Class          string `json:"class"`
	EventCount     int    `json:"event_count"`
	FirstNudge     int    `json:"first_nudge"`
	FirstForceStop int    `json:"first_force_stop"`
	WastedCalls    int    `json:"wasted_calls"`
	Passed         bool   `json:"passed"`
}

type loopAcceptanceReport struct {
	SchemaVersion    string                      `json:"schema_version"`
	CorpusSchema     string                      `json:"corpus_schema"`
	Suite            string                      `json:"suite"`
	FinishedAt       time.Time                   `json:"finished_at"`
	TraceCount       int                         `json:"trace_count"`
	ProductiveCount  int                         `json:"productive_count"`
	LoopCount        int                         `json:"loop_count"`
	KnownGapCount    int                         `json:"known_gap_count,omitempty"`
	FalseSignals     int                         `json:"false_signals"`
	MissedLoops      int                         `json:"missed_loops"`
	TotalWastedCalls int                         `json:"total_wasted_calls"`
	Passed           bool                        `json:"passed"`
	Traces           []loopAcceptanceTraceResult `json:"traces"`
}

func TestLoopDetectorAcceptanceCorpus(t *testing.T) {
	path := filepath.Join("testdata", "loop_detector_acceptance.v1.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var corpus loopAcceptanceCorpus
	if err := json.Unmarshal(raw, &corpus); err != nil {
		t.Fatal(err)
	}
	if corpus.SchemaVersion != "agent.loop_detector_acceptance.v1" || len(corpus.Traces) == 0 {
		t.Fatalf("invalid loop acceptance corpus: schema=%q traces=%d", corpus.SchemaVersion, len(corpus.Traces))
	}

	report := loopAcceptanceReport{
		SchemaVersion: "agent.loop_detector_acceptance_result.v1",
		CorpusSchema:  corpus.SchemaVersion,
		Suite:         corpus.Suite,
		FinishedAt:    time.Now().UTC(),
		TraceCount:    len(corpus.Traces),
	}
	seen := make(map[string]struct{}, len(corpus.Traces))
	for _, trace := range corpus.Traces {
		if _, exists := seen[trace.ID]; exists {
			t.Fatalf("duplicate loop acceptance trace %q", trace.ID)
		}
		seen[trace.ID] = struct{}{}
		result := replayLoopAcceptanceTrace(trace)
		report.Traces = append(report.Traces, result)
		switch trace.Class {
		case "productive":
			report.ProductiveCount++
			if result.FirstNudge != 0 || result.FirstForceStop != 0 {
				report.FalseSignals++
			}
		case "loop":
			report.LoopCount++
			if result.FirstForceStop == 0 {
				report.MissedLoops++
			}
		case "known-gap":
			// A genuine loop shape the detector deliberately does not force-stop
			// today (outcome-aware relaxation trade-off). The expectation pins
			// CURRENT behavior so drift is visible; it is excluded from the
			// missed-loop gate. When a detector change closes the gap, promote
			// the trace to class "loop" with the new stop position.
			report.KnownGapCount++
		default:
			t.Fatalf("trace %q has invalid class %q", trace.ID, trace.Class)
		}
		report.TotalWastedCalls += result.WastedCalls
		if !result.Passed {
			t.Errorf(
				"trace %s: nudge=%d force_stop=%d wasted=%d expectation=%+v",
				trace.ID,
				result.FirstNudge,
				result.FirstForceStop,
				result.WastedCalls,
				trace.Expectation,
			)
		}
	}
	report.Passed = report.FalseSignals == 0 && report.MissedLoops == 0
	for _, result := range report.Traces {
		report.Passed = report.Passed && result.Passed
	}
	if reportPath := os.Getenv("AGENT_LOOP_HARNESS_REPORT"); reportPath != "" {
		if err := writeLoopAcceptanceReport(reportPath, report); err != nil {
			t.Fatalf("write loop acceptance report: %v", err)
		}
		t.Logf("loop acceptance report=%s", reportPath)
	}
	if !report.Passed {
		t.Fatalf(
			"loop acceptance failed: false_signals=%d missed_loops=%d wasted_calls=%d",
			report.FalseSignals,
			report.MissedLoops,
			report.TotalWastedCalls,
		)
	}
}

func replayLoopAcceptanceTrace(trace loopAcceptanceTrace) loopAcceptanceTraceResult {
	result := loopAcceptanceTraceResult{
		ID:         trace.ID,
		Class:      trace.Class,
		EventCount: len(trace.Events),
	}
	detector := NewLoopDetector()
	for index, event := range trace.Events {
		action, _ := detector.CheckBefore(event.Name, event.Args, event.IsReadOnly)
		call := index + 1
		if action == LoopForceStop {
			if result.FirstForceStop == 0 {
				result.FirstForceStop = call
			}
			continue
		}
		detector.RecordOutcome(
			event.Name,
			event.Args,
			event.IsError,
			event.Error,
			event.ResultSig,
			event.OutcomeSig,
			event.IsReadOnly,
			event.IsNonActionable,
		)
		action, _ = detector.Check(event.Name)
		if action == LoopNudge && result.FirstNudge == 0 {
			result.FirstNudge = call
		}
		if action == LoopForceStop && result.FirstForceStop == 0 {
			result.FirstForceStop = call
		}
	}
	if trace.Expectation.IdealStopAfter > 0 && result.FirstForceStop > 0 {
		result.WastedCalls = result.FirstForceStop - trace.Expectation.IdealStopAfter
		if result.WastedCalls < 0 {
			result.WastedCalls = 0
		}
	}
	result.Passed = loopCallInRange(
		result.FirstNudge,
		trace.Expectation.FirstNudgeMin,
		trace.Expectation.FirstNudgeMax,
	) && loopCallInRange(
		result.FirstForceStop,
		trace.Expectation.FirstForceMin,
		trace.Expectation.FirstForceMax,
	) && result.WastedCalls <= trace.Expectation.MaxWastedCalls
	return result
}

func loopCallInRange(got, min, max int) bool {
	if min == 0 && max == 0 {
		return got == 0
	}
	return got >= min && got <= max
}

func writeLoopAcceptanceReport(path string, report loopAcceptanceReport) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(report, "", "  ")
	if err != nil {
		return err
	}
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, append(body, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return fmt.Errorf("publish report: %w", err)
	}
	return nil
}
