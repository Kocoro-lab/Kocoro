//go:build live

package e2e

import (
	"fmt"
	"math"
	"sort"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

const fastPinnedEfficiencyPolicyID = "fast-pinned-efficiency.v1"

type fastPinnedRunStatus struct {
	Partial        bool   `json:"partial"`
	FailureCode    string `json:"failure_code,omitempty"`
	LastTool       string `json:"last_tool,omitempty"`
	RetryCount     int    `json:"retry_count"`
	IterationCount int    `json:"iteration_count"`
}

type fastPinnedEfficiency struct {
	PolicyID                  string   `json:"policy_id"`
	Qualifying                bool     `json:"qualifying"`
	Violations                []string `json:"violations,omitempty"`
	RequestedToolCalls        int      `json:"requested_tool_calls"`
	ExecutedToolCalls         int      `json:"executed_tool_calls"`
	ModelToolBatches          int      `json:"model_tool_batches"`
	ExecutionBatches          int      `json:"execution_batches"`
	ParallelExecutionBatches  int      `json:"parallel_execution_batches"`
	CriticalToolDurationMills int64    `json:"critical_tool_duration_ms"`
}

type fastPinnedEfficiencyBound struct {
	minTools int
	maxTools int
	maxLLM   int
	exact    bool
}

var fastPinnedEfficiencyBounds = map[string]fastPinnedEfficiencyBound{
	"multi_file_synthesis":  {minTools: 3, maxTools: 6, maxLLM: 7},
	"constrained_schedule":  {minTools: 0, maxTools: 3, maxLLM: 4},
	"error_recovery":        {minTools: 2, maxTools: 5, maxLLM: 6},
	"data_pipeline":         {minTools: 2, maxTools: 4, maxLLM: 5},
	"state_chain":           {minTools: 5, maxTools: 5, maxLLM: 7, exact: true},
	"doc_reason":            {minTools: 1, maxTools: 3, maxLLM: 4},
	"long_chain_12":         {minTools: 12, maxTools: 12, maxLLM: 14, exact: true},
	"config_rewrite":        {minTools: 2, maxTools: 5, maxLLM: 6},
	"cross_doc_3hop":        {minTools: 4, maxTools: 7, maxLLM: 8},
	"stateful_ledger":       {minTools: 13, maxTools: 13, maxLLM: 15, exact: true},
	"compaction_checkpoint": {minTools: 0, maxTools: 3, maxLLM: 6},
}

func fastPinnedProcessObserved(run fastABRun) bool {
	if !run.TrajectoryObserved || run.Status.IterationCount < 1 {
		return false
	}
	modelObserved := false
	terminalObserved := false
	terminalCount := 0
	compactionApplied := false
	for _, event := range run.LoopEvents {
		if event.Type == agent.RunTraceEventModelResponse && event.Model != nil {
			modelObserved = true
		}
		if event.Type == agent.RunTraceEventCompaction && event.Compaction != nil && event.Compaction.Applied {
			compactionApplied = true
		}
		if event.Type == agent.RunTraceEventTerminal && event.Terminal != nil {
			terminalCount++
			terminal := event.Terminal
			terminalObserved = terminal.Partial == run.Status.Partial &&
				terminal.FailureCode == run.Status.FailureCode &&
				terminal.LastTool == run.Status.LastTool &&
				terminal.RetryCount == run.Status.RetryCount &&
				terminal.IterationCount == run.Status.IterationCount
		}
	}
	if run.Case == "compaction_checkpoint" && !compactionApplied {
		return false
	}
	return modelObserved && terminalCount == 1 && terminalObserved
}

func evaluateFastPinnedEfficiency(caseName string, llmCalls int, status fastPinnedRunStatus, trajectory []agent.RunTraceEvent) fastPinnedEfficiency {
	eff := fastPinnedEfficiency{PolicyID: fastPinnedEfficiencyPolicyID}
	bound, ok := fastPinnedEfficiencyBounds[caseName]
	if !ok {
		eff.Violations = append(eff.Violations, "missing_case_policy")
	}
	modelBatches := map[int]bool{}
	type executionBatchKey struct{ iteration, index int }
	executionBatches := map[executionBatchKey]bool{}
	parallelBatches := map[executionBatchKey]bool{}
	criticalDurations := map[executionBatchKey]int64{}
	for _, event := range trajectory {
		if event.Type != agent.RunTraceEventToolOutcome || event.Tool == nil {
			continue
		}
		eff.RequestedToolCalls++
		tool := event.Tool
		modelBatches[tool.ModelBatchID] = true
		if tool.Executed {
			eff.ExecutedToolCalls++
		}
		if tool.ExecutionBatchIndex != nil {
			key := executionBatchKey{iteration: event.Iteration, index: *tool.ExecutionBatchIndex}
			executionBatches[key] = true
			if tool.ExecutionParallel {
				parallelBatches[key] = true
			}
			if tool.DurationMilliseconds > criticalDurations[key] {
				criticalDurations[key] = tool.DurationMilliseconds
			}
		}
		if caseName != "error_recovery" && tool.Outcome != "succeeded" {
			eff.Violations = append(eff.Violations, "unexpected_tool_outcome:"+tool.Outcome)
		}
		if tool.Outcome == "duplicate" || tool.Outcome == "loop_blocked" || tool.Outcome == "replay_blocked" {
			eff.Violations = append(eff.Violations, "runaway_signal:"+tool.Outcome)
		}
	}
	eff.ModelToolBatches = len(modelBatches)
	eff.ExecutionBatches = len(executionBatches)
	eff.ParallelExecutionBatches = len(parallelBatches)
	for _, duration := range criticalDurations {
		eff.CriticalToolDurationMills += duration
	}
	if ok {
		if eff.ExecutedToolCalls < bound.minTools || eff.ExecutedToolCalls > bound.maxTools {
			kind := "outside"
			if bound.exact {
				kind = "not_exact"
			}
			eff.Violations = append(eff.Violations,
				fmt.Sprintf("executed_tool_calls_%d_%s_%d_%d", eff.ExecutedToolCalls, kind, bound.minTools, bound.maxTools))
		}
		if llmCalls < 1 || llmCalls > bound.maxLLM {
			eff.Violations = append(eff.Violations,
				fmt.Sprintf("llm_calls_%d_outside_1_%d", llmCalls, bound.maxLLM))
		}
	}
	if status.Partial || status.FailureCode != "" {
		eff.Violations = append(eff.Violations, "abnormal_terminal")
	}
	if status.RetryCount > 1 {
		eff.Violations = append(eff.Violations, fmt.Sprintf("provider_retries_%d_gt_1", status.RetryCount))
	}
	if (caseName == "state_chain" || caseName == "long_chain_12" || caseName == "stateful_ledger") && eff.ParallelExecutionBatches > 0 {
		eff.Violations = append(eff.Violations, "dependent_chain_executed_in_parallel")
	}
	eff.Violations = uniqueSortedFastPinned(eff.Violations)
	eff.Qualifying = len(eff.Violations) == 0
	return eff
}

func uniqueSortedFastPinned(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	seen := map[string]bool{}
	for _, value := range values {
		seen[value] = true
	}
	result := make([]string, 0, len(seen))
	for value := range seen {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func fastPinnedTrace(iteration, batch, tools int, names ...string) []agent.RunTraceEvent {
	trace := make([]agent.RunTraceEvent, 0, len(names))
	for index, name := range names {
		batchIndex := batch
		trace = append(trace, agent.RunTraceEvent{
			Seq: int64(index + 1), Iteration: iteration, Type: agent.RunTraceEventToolOutcome,
			Tool: &agent.RunTraceToolOutcome{
				Ordinal: index + 1, Name: name, ModelBatchID: iteration,
				ModelBatchIndex: index, ModelBatchSize: tools,
				ExecutionBatchIndex: &batchIndex, ExecutionBatchSize: tools,
				ExecutionParallel: tools > 1, MaxConcurrency: tools,
				Executed: true, Outcome: "succeeded",
			},
		})
	}
	return trace
}

func TestOffline_FastPinnedEfficiencyAllowsAlternativePathsAndFailsClosed(t *testing.T) {
	clean := fastPinnedRunStatus{IterationCount: 2}
	parallel := fastPinnedTrace(1, 0, 3, "file_read", "file_read", "file_read")
	if got := evaluateFastPinnedEfficiency("multi_file_synthesis", 2, clean, parallel); !got.Qualifying || got.ParallelExecutionBatches != 1 {
		t.Fatalf("parallel valid path rejected: %+v", got)
	}

	var sequential []agent.RunTraceEvent
	for iteration := 1; iteration <= 3; iteration++ {
		step := fastPinnedTrace(iteration, 0, 1, "file_read")
		step[0].Seq = int64(iteration)
		sequential = append(sequential, step...)
	}
	if got := evaluateFastPinnedEfficiency("multi_file_synthesis", 4, clean, sequential); !got.Qualifying || got.ParallelExecutionBatches != 0 || got.ModelToolBatches != 3 {
		t.Fatalf("sequential valid path rejected: %+v", got)
	}

	tooMany := append([]agent.RunTraceEvent(nil), sequential...)
	for iteration := 4; iteration <= 7; iteration++ {
		step := fastPinnedTrace(iteration, 0, 1, "file_read")
		step[0].Seq = int64(iteration)
		tooMany = append(tooMany, step...)
	}
	got := evaluateFastPinnedEfficiency("multi_file_synthesis", 8, clean, tooMany)
	if got.Qualifying || !strings.Contains(strings.Join(got.Violations, ","), "executed_tool_calls_7") ||
		!strings.Contains(strings.Join(got.Violations, ","), "llm_calls_8") {
		t.Fatalf("runaway path qualified: %+v", got)
	}

	partial := clean
	partial.Partial = true
	partial.FailureCode = "iteration_limit"
	if got := evaluateFastPinnedEfficiency("multi_file_synthesis", 2, partial, parallel); got.Qualifying {
		t.Fatalf("partial terminal qualified: %+v", got)
	}
}

func TestOffline_FastPinnedProcessObservationRequiresCompactionOnlyForDedicatedCase(t *testing.T) {
	status := fastPinnedRunStatus{IterationCount: 2}
	base := []agent.RunTraceEvent{
		{Seq: 1, Iteration: 1, Type: agent.RunTraceEventModelResponse, Model: &agent.RunTraceModelResponse{Attempt: 1}},
		{Seq: 2, Iteration: 2, Type: agent.RunTraceEventTerminal, Terminal: &agent.RunTraceTerminal{IterationCount: 2}},
	}
	ordinary := fastABRun{Case: "multi_file_synthesis", TrajectoryObserved: true, Status: status, LoopEvents: base}
	if !fastPinnedProcessObserved(ordinary) {
		t.Fatal("ordinary case incorrectly required a compaction event")
	}
	compaction := ordinary
	compaction.Case = "compaction_checkpoint"
	if fastPinnedProcessObserved(compaction) {
		t.Fatal("dedicated compaction case passed without applied compaction")
	}
	compaction.LoopEvents = append(append([]agent.RunTraceEvent(nil), base...), agent.RunTraceEvent{
		Seq: 3, Iteration: 1, Type: agent.RunTraceEventCompaction,
		Compaction: &agent.RunTraceCompaction{Phase: "preflight", Status: "applied", Applied: true, MessagesDropped: 4},
	})
	if !fastPinnedProcessObserved(compaction) {
		t.Fatal("dedicated compaction case rejected an applied compaction event")
	}
}

func TestOffline_FastPinnedHandlerAccumulatesAllUsageChannels(t *testing.T) {
	handler := &fastABHandler{}
	handler.OnUsage(agent.TurnUsage{LLMCalls: 1, InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CostUSD: 0.02})
	handler.OnUsage(agent.TurnUsage{LLMCalls: 1, InputTokens: 7, OutputTokens: 3, TotalTokens: 10, CostUSD: 0.01})
	handler.OnUsage(agent.TurnUsage{TotalTokens: 100, CostUSD: 0.04})

	got := handler.accumulatedUsage()
	if got.LLM.LLMCalls != 2 || got.LLM.TotalTokens != 25 || got.ToolCalls != 1 || math.Abs(got.TotalCostUSD()-0.07) > 1e-9 {
		t.Fatalf("accumulated usage = %+v, want two LLM calls plus one billed tool", got)
	}
}
