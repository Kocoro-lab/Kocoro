package agent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
)

type recordingSideEffectJournal struct {
	mu            sync.Mutex
	events        []string
	prepared      []SideEffectExecution
	commitErr     error
	dispatchErr   error
	prepareErr    error
	abandonErr    error
	unknownErr    error
	resultDigests []string
}

func (j *recordingSideEffectJournal) record(event string) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.events = append(j.events, event)
}

func (j *recordingSideEffectJournal) snapshotEvents() []string {
	j.mu.Lock()
	defer j.mu.Unlock()
	return append([]string(nil), j.events...)
}

func (j *recordingSideEffectJournal) Prepare(_ context.Context, execution SideEffectExecution) (PreparedSideEffectExecution, error) {
	j.mu.Lock()
	j.events = append(j.events, "prepare")
	j.prepared = append(j.prepared, execution)
	j.mu.Unlock()
	if j.prepareErr != nil {
		return PreparedSideEffectExecution{}, j.prepareErr
	}
	return PreparedSideEffectExecution{ExecutionID: "exec-opaque", IdempotencyKey: "idem-opaque"}, nil
}

func (j *recordingSideEffectJournal) MarkDispatching(context.Context, string) error {
	j.record("dispatching")
	return j.dispatchErr
}

func (j *recordingSideEffectJournal) MarkCommitted(_ context.Context, _ string, digest string) error {
	j.mu.Lock()
	j.events = append(j.events, "committed")
	j.resultDigests = append(j.resultDigests, digest)
	j.mu.Unlock()
	return j.commitErr
}

func (j *recordingSideEffectJournal) MarkAbandoned(_ context.Context, _ string, digest string) error {
	j.mu.Lock()
	j.events = append(j.events, "abandoned")
	j.resultDigests = append(j.resultDigests, digest)
	j.mu.Unlock()
	return j.abandonErr
}

func (j *recordingSideEffectJournal) MarkOutcomeUnknown(_ context.Context, _ string, digest string) error {
	j.mu.Lock()
	j.events = append(j.events, "outcome_unknown")
	j.resultDigests = append(j.resultDigests, digest)
	j.mu.Unlock()
	return j.unknownErr
}

type journalWriteTool struct {
	runs       atomic.Int32
	journal    *recordingSideEffectJournal
	result     ToolResult
	runErr     error
	ctxCapture SideEffectExecutionContext
}

func (*journalWriteTool) Info() ToolInfo {
	return ToolInfo{Name: "journal_write", Parameters: map[string]any{"type": "object"}}
}
func (*journalWriteTool) RequiresApproval() bool { return false }
func (t *journalWriteTool) Run(ctx context.Context, _ string) (ToolResult, error) {
	t.runs.Add(1)
	if t.journal != nil {
		t.journal.record("run")
	}
	t.ctxCapture, _ = SideEffectExecutionFromContext(ctx)
	return t.result, t.runErr
}

type journalReadOnlyTool struct{ runs atomic.Int32 }

func (*journalReadOnlyTool) Info() ToolInfo {
	return ToolInfo{Name: "journal_read", Parameters: map[string]any{"type": "object"}}
}
func (*journalReadOnlyTool) RequiresApproval() bool     { return false }
func (*journalReadOnlyTool) IsReadOnlyCall(string) bool { return true }
func (t *journalReadOnlyTool) Run(context.Context, string) (ToolResult, error) {
	t.runs.Add(1)
	return ToolResult{Content: "observed"}, nil
}

type journalNonMaterialTool struct{ runs atomic.Int32 }

func (*journalNonMaterialTool) Info() ToolInfo {
	return ToolInfo{Name: "journal_local", Parameters: map[string]any{"type": "object"}}
}
func (*journalNonMaterialTool) RequiresApproval() bool            { return false }
func (*journalNonMaterialTool) HasMaterialSideEffect(string) bool { return false }
func (t *journalNonMaterialTool) Run(context.Context, string) (ToolResult, error) {
	t.runs.Add(1)
	return ToolResult{Content: "local"}, nil
}

func journalApprovedCall(tool Tool, index int) approvedToolCall {
	return approvedToolCall{
		index: index,
		fc: client.FunctionCall{
			ID:        fmt.Sprintf("call-%d", index),
			Name:      tool.Info().Name,
			Arguments: json.RawMessage(`{"value":"private"}`),
		},
		tool:    tool,
		argsStr: `{"value":"private"}`,
	}
}

func TestExecuteBatches_SideEffectJournalOrdersDurableBoundary(t *testing.T) {
	journal := &recordingSideEffectJournal{}
	tool := &journalWriteTool{journal: journal, result: ToolResult{Content: "created"}}
	results := make([]toolExecResult, 1)
	err := executeBatches(
		context.Background(),
		[][]approvedToolCall{{journalApprovedCall(tool, 0)}},
		results, nil, nil, "request",
		sideEffectBatchHooks{
			journal: journal,
			checkpointPrepared: func(context.Context) error {
				journal.record("checkpoint")
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("executeBatches: %v", err)
	}
	if got, want := fmt.Sprint(journal.snapshotEvents()), "[prepare checkpoint dispatching run committed]"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if tool.ctxCapture.ExecutionID != "exec-opaque" || tool.ctxCapture.IdempotencyKey != "idem-opaque" {
		t.Fatalf("tool context = %+v", tool.ctxCapture)
	}
	if len(journal.prepared) != 1 || len(journal.prepared[0].ArgumentsSHA256) != 64 {
		t.Fatalf("prepared execution = %+v", journal.prepared)
	}
	if journal.prepared[0].ArgumentsSHA256 == `{"value":"private"}` {
		t.Fatal("journal received raw arguments instead of a digest")
	}
	if len(journal.resultDigests) != 1 || len(journal.resultDigests[0]) != 64 {
		t.Fatalf("result digests = %v", journal.resultDigests)
	}
	if results[0].result.Content != "created" {
		t.Fatalf("result = %+v", results[0].result)
	}
}

func TestExecuteBatches_SideEffectJournalBypassesNonMaterialCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool Tool
	}{
		{name: "read_only", tool: &journalReadOnlyTool{}},
		{name: "explicit_non_material", tool: &journalNonMaterialTool{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			journal := &recordingSideEffectJournal{}
			results := make([]toolExecResult, 1)
			checkpoints := 0
			err := executeBatches(
				context.Background(),
				[][]approvedToolCall{{journalApprovedCall(tc.tool, 0)}},
				results, nil, nil, "",
				sideEffectBatchHooks{
					journal: journal,
					checkpointPrepared: func(context.Context) error {
						checkpoints++
						return nil
					},
				},
			)
			if err != nil {
				t.Fatalf("executeBatches: %v", err)
			}
			if got := journal.snapshotEvents(); len(got) != 0 {
				t.Fatalf("journal events = %v, want none", got)
			}
			if checkpoints != 0 {
				t.Fatalf("checkpoints = %d, want 0", checkpoints)
			}
		})
	}
}

func TestExecuteBatches_SideEffectJournalFailsClosedBeforeDispatch(t *testing.T) {
	journal := &recordingSideEffectJournal{}
	tool := &journalWriteTool{journal: journal, result: ToolResult{Content: "must not run"}}
	results := make([]toolExecResult, 1)
	err := executeBatches(
		context.Background(),
		[][]approvedToolCall{{journalApprovedCall(tool, 0)}},
		results, nil, nil, "",
		sideEffectBatchHooks{journal: journal},
	)
	if !errors.Is(err, ErrSideEffectJournalUnavailable) {
		t.Fatalf("error = %v, want journal unavailable", err)
	}
	if tool.runs.Load() != 0 {
		t.Fatalf("tool runs = %d, want 0", tool.runs.Load())
	}
	if got, want := fmt.Sprint(journal.snapshotEvents()), "[prepare abandoned]"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if !results[0].result.IsError {
		t.Fatalf("result = %+v, want error", results[0].result)
	}
}

func TestExecuteBatches_SideEffectOutcomeUnknownStopsFollowingBatch(t *testing.T) {
	journal := &recordingSideEffectJournal{}
	first := &journalWriteTool{journal: journal, result: TransientError("connection lost")}
	second := &journalWriteTool{journal: journal, result: ToolResult{Content: "second"}}
	results := make([]toolExecResult, 2)
	err := executeBatches(
		context.Background(),
		[][]approvedToolCall{
			{journalApprovedCall(first, 0)},
			{journalApprovedCall(second, 1)},
		},
		results, nil, nil, "",
		sideEffectBatchHooks{
			journal: journal,
			checkpointPrepared: func(context.Context) error {
				journal.record("checkpoint")
				return nil
			},
		},
	)
	if !errors.Is(err, ErrSideEffectOutcomeUnknown) {
		t.Fatalf("error = %v, want outcome unknown", err)
	}
	if first.runs.Load() != 1 || second.runs.Load() != 0 {
		t.Fatalf("tool runs = (%d, %d), want (1, 0)", first.runs.Load(), second.runs.Load())
	}
	if got, want := fmt.Sprint(journal.snapshotEvents()), "[prepare checkpoint dispatching run outcome_unknown]"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if !results[0].result.IsError || results[0].result.ErrorCategory != ErrCategoryBusiness {
		t.Fatalf("result = %+v, want non-retryable outcome-unknown error", results[0].result)
	}
}

func TestExecuteBatches_ConcurrentSideEffectOutcomesAreCollected(t *testing.T) {
	journal := &recordingSideEffectJournal{}
	first := &journalWriteTool{journal: journal, result: TransientError("first lost")}
	second := &journalWriteTool{journal: journal, result: TransientError("second lost")}
	results := make([]toolExecResult, 2)
	err := executeBatches(
		context.Background(),
		[][]approvedToolCall{{
			journalApprovedCall(first, 0),
			journalApprovedCall(second, 1),
		}},
		results, nil, nil, "",
		sideEffectBatchHooks{
			journal:            journal,
			checkpointPrepared: func(context.Context) error { return nil },
		},
	)
	if !errors.Is(err, ErrSideEffectOutcomeUnknown) {
		t.Fatalf("error = %v, want outcome unknown", err)
	}
	if first.runs.Load() != 1 || second.runs.Load() != 1 {
		t.Fatalf("tool runs = (%d, %d), want (1, 1)", first.runs.Load(), second.runs.Load())
	}
}

func TestExecuteBatches_CommitPersistenceFailureBecomesOutcomeUnknown(t *testing.T) {
	journal := &recordingSideEffectJournal{commitErr: errors.New("disk unavailable")}
	tool := &journalWriteTool{journal: journal, result: ToolResult{Content: "created"}}
	results := make([]toolExecResult, 1)
	err := executeBatches(
		context.Background(),
		[][]approvedToolCall{{journalApprovedCall(tool, 0)}},
		results, nil, nil, "",
		sideEffectBatchHooks{
			journal:            journal,
			checkpointPrepared: func(context.Context) error { return nil },
		},
	)
	if !errors.Is(err, ErrSideEffectOutcomeUnknown) {
		t.Fatalf("error = %v, want outcome unknown", err)
	}
	if got, want := fmt.Sprint(journal.snapshotEvents()), "[prepare dispatching run committed outcome_unknown]"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
	if !results[0].result.IsError || results[0].result.ErrorCategory != ErrCategoryBusiness {
		t.Fatalf("result = %+v, want non-retryable outcome-unknown error", results[0].result)
	}
}

func TestExecuteBatches_ValidationFailureIsDurablyAbandoned(t *testing.T) {
	journal := &recordingSideEffectJournal{}
	tool := &journalWriteTool{journal: journal, result: ValidationError("bad value")}
	results := make([]toolExecResult, 1)
	err := executeBatches(
		context.Background(),
		[][]approvedToolCall{{journalApprovedCall(tool, 0)}},
		results, nil, nil, "",
		sideEffectBatchHooks{
			journal: journal,
			checkpointPrepared: func(context.Context) error {
				journal.record("checkpoint")
				return nil
			},
		},
	)
	if err != nil {
		t.Fatalf("executeBatches: %v", err)
	}
	if got, want := fmt.Sprint(journal.snapshotEvents()), "[prepare checkpoint dispatching run abandoned]"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

func TestExecuteBatches_ComputerUseNoCommitFailureIsDurablyAbandoned(t *testing.T) {
	journal := &recordingSideEffectJournal{}
	result := BusinessError("policy blocked before input")
	result.ComputerUseOutcome = &ComputerUseTaskOutcome{
		Status: ComputerUseTaskNotCompleted,
		Effect: ComputerUseCommitNone,
	}
	tool := &journalWriteTool{journal: journal, result: result}
	results := make([]toolExecResult, 1)
	err := executeBatches(
		context.Background(),
		[][]approvedToolCall{{journalApprovedCall(tool, 0)}},
		results, nil, nil, "",
		sideEffectBatchHooks{
			journal:            journal,
			checkpointPrepared: func(context.Context) error { return nil },
		},
	)
	if err != nil {
		t.Fatalf("executeBatches: %v", err)
	}
	if got, want := fmt.Sprint(journal.snapshotEvents()), "[prepare dispatching run abandoned]"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

type journalLoopClient struct{ calls atomic.Int32 }

func (c *journalLoopClient) Complete(context.Context, client.CompletionRequest) (*client.CompletionResponse, error) {
	c.calls.Add(1)
	return &client.CompletionResponse{
		FinishReason: "tool_use",
		ToolCalls: []client.FunctionCall{{
			ID:        "side-effect-call",
			Name:      "journal_write",
			Arguments: json.RawMessage(`{"value":"private"}`),
		}},
	}, nil
}

func (c *journalLoopClient) CompleteStream(ctx context.Context, req client.CompletionRequest, _ func(client.StreamDelta)) (*client.CompletionResponse, error) {
	return c.Complete(ctx, req)
}

func TestAgentLoop_SideEffectJournalCheckpointsToolCallBeforeDispatch(t *testing.T) {
	llm := &journalLoopClient{}
	journal := &recordingSideEffectJournal{}
	tool := &journalWriteTool{journal: journal, result: TransientError("lost after dispatch")}
	registry := NewToolRegistry()
	registry.Register(tool)
	loop := NewAgentLoop(llm, registry, "medium", t.TempDir(), 4, 2000, 200, nil, nil, nil)
	loop.SetSideEffectExecutionJournal(journal)
	checkpointCalls := 0
	loop.SetCheckpointFunc(func(ctx context.Context) error {
		checkpointCalls++
		if checkpointCalls != 1 {
			if reason := CheckpointReasonFromContext(ctx); reason != "" {
				t.Fatal("terminal checkpoint retained the pre-dispatch checkpoint reason")
			}
			return nil
		}
		if reason := CheckpointReasonFromContext(ctx); reason != CheckpointReasonSideEffectPrepared {
			t.Fatalf("pre-dispatch checkpoint reason = %q", reason)
		}
		if got, want := fmt.Sprint(journal.snapshotEvents()), "[prepare]"; got != want {
			t.Fatalf("pre-dispatch journal events = %s, want %s", got, want)
		}
		for _, message := range loop.RunMessages() {
			for _, block := range message.Content.Blocks() {
				if block.Type == "tool_use" && block.ID == "side-effect-call" {
					return nil
				}
			}
		}
		t.Fatal("pre-dispatch checkpoint did not contain the assistant tool call")
		return nil
	})

	text, _, err := loop.Run(context.Background(), "mutate external state", nil, nil)
	if !errors.Is(err, ErrSideEffectOutcomeUnknown) {
		t.Fatalf("Run error = %v, want outcome unknown", err)
	}
	const wantText = "The external action may have completed, but its result could not be durably confirmed. It was not retried. Review the external system before retrying."
	if text != wantText {
		t.Fatalf("Run text = %q, want %q", text, wantText)
	}
	if llm.calls.Load() != 1 {
		t.Fatalf("LLM calls = %d, want 1", llm.calls.Load())
	}
	if checkpointCalls != 2 {
		t.Fatalf("checkpoint calls = %d, want pre-dispatch + terminal result", checkpointCalls)
	}
	status := loop.LastRunStatus()
	if !status.Partial || status.FailureCode != runstatus.CodeSideEffectOutcomeUnknown {
		t.Fatalf("run status = %+v", status)
	}
	assertNativeToolPairs(t, loop.RunMessages())
}
