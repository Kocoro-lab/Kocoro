package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"
)

// maxToolConcurrency caps the number of read-only tool calls the loop runs
// in parallel within a single read-only batch (write-batches always run with
// concurrency 1 to preserve ordering).
//
// Workload: parallel file_read / grep / glob over a small/medium repo (e.g.
// "read these 14 screenshots" or "grep these 18 paths for an identifier") —
// the iteration's read-only fan-out arrives in one batch from the model.
// Symptom when binds: a read-only batch with more than this many calls is
// split into multiple sub-batches that execute back-to-back; user sees the
// turn take noticeably longer and an extra prompt-cache cycle is paid before
// the next assistant message. Bumped 10 → 20 because "read 14 files in
// parallel" was being split into 10 + 4 (wasted a cache cycle).
// Override: not user-configurable yet — file an issue if your workload
// routinely fans out beyond 20 read-only calls per turn.
const maxToolConcurrency = 20

// isReadOnly checks if a tool call is read-only by testing the ReadOnlyChecker
// optional interface. Tools without the interface default to false (fail-closed).
func isReadOnly(ac approvedToolCall) bool {
	checker, ok := ac.tool.(ReadOnlyChecker)
	if !ok {
		return false
	}
	return checker.IsReadOnlyCall(ac.argsStr)
}

// isConcurrencySafe returns whether a tool call can share a concurrent batch
// with other calls. If the tool implements ConcurrencySafeChecker, its answer
// wins; otherwise fall back to isReadOnly so existing tools keep current
// behavior. This means adding the new interface to one tool (e.g. BashTool)
// has no effect on any other tool's grouping.
func isConcurrencySafe(ac approvedToolCall) bool {
	if checker, ok := ac.tool.(ConcurrencySafeChecker); ok {
		return checker.IsConcurrencySafeCall(ac.argsStr)
	}
	return isReadOnly(ac)
}

// partitionToolCalls groups approved tool calls into execution batches.
// Consecutive concurrency-safe calls share a batch; non-safe calls each get
// their own sequential batch of size 1.
func partitionToolCalls(approved []approvedToolCall) [][]approvedToolCall {
	if len(approved) == 0 {
		return nil
	}
	var batches [][]approvedToolCall
	var currentBatch []approvedToolCall
	currentIsSafe := false

	for i, ac := range approved {
		safe := isConcurrencySafe(ac)
		if i == 0 {
			currentBatch = []approvedToolCall{ac}
			currentIsSafe = safe
			continue
		}
		if safe && currentIsSafe {
			currentBatch = append(currentBatch, ac)
		} else {
			batches = append(batches, currentBatch)
			currentBatch = []approvedToolCall{ac}
			currentIsSafe = safe
		}
	}
	if len(currentBatch) > 0 {
		batches = append(batches, currentBatch)
	}
	return batches
}

// executeBatches runs partitioned tool call batches sequentially.
// Read-only batches (len > 1) run concurrently with a channel semaphore
// capped at maxToolConcurrency. Write batches (len == 1) run directly.
// After each batch, readTracker is updated for successful file_read calls
// so that subsequent write batches can pass read-before-edit checks.
// If handler is non-nil, OnToolCall is fired for each call immediately
// before execution begins (so "running" status reflects actual execution).
func executeBatches(
	ctx context.Context,
	batches [][]approvedToolCall,
	execResults []toolExecResult,
	readTracker *ReadTracker,
	handler EventHandler,
	userRequest string,
	sideEffectHooks ...sideEffectBatchHooks,
) error {
	var hooks sideEffectBatchHooks
	if len(sideEffectHooks) > 0 {
		hooks = sideEffectHooks[0]
	}
	// Attach the handler's OnUsage as the per-run usage emitter so tools
	// that bill per call (gateway tools reporting xAI/Grok or SerpAPI costs)
	// can fold their usage into the session totals.
	if handler != nil {
		ctx = WithUsageEmit(ctx, handler.OnUsage)
	}
	// Every approved call needs a paired tool_result even when an earlier batch
	// aborts the response. Start from an explicit non-executed error and overwrite
	// it only when admission reaches Tool.Run. A zero-valued ToolResult would be
	// serialized as a false success and could make recovery believe the skipped
	// mutation completed.
	for _, batch := range batches {
		for _, ac := range batch {
			execResults[ac.index] = toolExecResult{
				result: TransientError(fmt.Sprintf(
					"%s was not executed because an earlier action in the same response stopped the batch; it is safe to run again if still needed",
					ac.fc.Name,
				)),
				name: ac.fc.Name,
			}
		}
	}
	for _, batch := range batches {
		var batchErr error
		if len(batch) == 1 {
			// Single call: run directly, no goroutine overhead.
			ac := batch[0]
			prepared, err := prepareApprovedToolCall(ctx, ac, hooks)
			if err != nil {
				execResults[ac.index] = toolExecResult{
					result: sideEffectJournalUnavailableResult(ac.fc.Name),
					name:   ac.fc.Name,
				}
				batchErr = err
			} else {
				if handler != nil {
					handler.OnToolCall(ac.fc.Name, ac.argsStr, ac.fc.ID)
				}
				execResults[ac.index], batchErr = runApprovedToolCall(ctx, ac, userRequest, prepared, hooks.journal)
			}
		} else {
			// Concurrent batch with semaphore.
			sem := make(chan struct{}, maxToolConcurrency)
			batchErrs := make(chan error, len(batch))
			var wg sync.WaitGroup
			wg.Add(len(batch))
			for _, ac := range batch {
				sem <- struct{}{} // acquire — blocks until a slot is free
				prepared, err := prepareApprovedToolCall(ctx, ac, hooks)
				if err != nil {
					<-sem
					execResults[ac.index] = toolExecResult{
						result: sideEffectJournalUnavailableResult(ac.fc.Name),
						name:   ac.fc.Name,
					}
					batchErrs <- err
					wg.Done()
					continue
				}
				// Emit "running" after acquiring the slot so the event reflects
				// actual execution start, not just batch membership. Called from
				// the main goroutine so handler writes stay serialized.
				if handler != nil {
					handler.OnToolCall(ac.fc.Name, ac.argsStr, ac.fc.ID)
				}
				go func(ac approvedToolCall, prepared *PreparedSideEffectExecution) {
					defer wg.Done()
					defer func() { <-sem }() // release
					result, err := runApprovedToolCall(ctx, ac, userRequest, prepared, hooks.journal)
					execResults[ac.index] = result
					if err != nil {
						batchErrs <- err
					}
				}(ac, prepared)
			}
			wg.Wait()
			close(batchErrs)
			for err := range batchErrs {
				if batchErr == nil {
					batchErr = err
				}
			}
		}

		// Inter-batch side effect: track file_read results for ReadTracker.
		if readTracker != nil {
			for _, ac := range batch {
				if ac.fc.Name == "file_read" {
					er := execResults[ac.index]
					if !er.result.IsError && er.err == nil {
						if p := extractPathArg(ac.argsStr); p != "" {
							readTracker.MarkRead(p)
						}
					}
				}
			}
		}
		if batchErr != nil {
			return batchErr
		}
	}
	return nil
}

type sideEffectBatchHooks struct {
	journal            SideEffectExecutionJournal
	checkpointPrepared func(context.Context) error
}

func prepareApprovedToolCall(
	ctx context.Context,
	ac approvedToolCall,
	hooks sideEffectBatchHooks,
) (*PreparedSideEffectExecution, error) {
	if hooks.journal == nil || !hasMaterialSideEffect(ac.tool, ac.argsStr) {
		return nil, nil
	}
	prepared, err := hooks.journal.Prepare(ctx, SideEffectExecution{
		ToolUseID:       ac.fc.ID,
		ToolName:        ac.fc.Name,
		ArgumentsSHA256: sideEffectArgumentsDigest(ac.argsStr),
	})
	if err == nil {
		err = validatePreparedSideEffect(prepared)
	}
	if err != nil {
		return nil, wrapSideEffectExecutionError(
			ErrSideEffectJournalUnavailable, ac.fc.Name, err,
		)
	}
	if hooks.checkpointPrepared == nil {
		err = errors.New("pre-dispatch checkpoint hook is not installed")
	} else {
		err = hooks.checkpointPrepared(ctx)
	}
	if err != nil {
		_ = hooks.journal.MarkAbandoned(context.WithoutCancel(ctx), prepared.ExecutionID, "")
		return nil, wrapSideEffectExecutionError(
			ErrSideEffectJournalUnavailable, ac.fc.Name, err,
		)
	}
	if err := hooks.journal.MarkDispatching(ctx, prepared.ExecutionID); err != nil {
		_ = hooks.journal.MarkAbandoned(context.WithoutCancel(ctx), prepared.ExecutionID, "")
		return nil, wrapSideEffectExecutionError(
			ErrSideEffectJournalUnavailable, ac.fc.Name, err,
		)
	}
	return &prepared, nil
}

func runApprovedToolCall(
	ctx context.Context,
	ac approvedToolCall,
	userRequest string,
	prepared *PreparedSideEffectExecution,
	journal SideEffectExecutionJournal,
) (executionResult toolExecResult, fatalErr error) {
	executionResult.name = ac.fc.Name
	var startTime time.Time
	defer func() {
		if !startTime.IsZero() {
			executionResult.elapsed = time.Since(startTime)
		}
		if recovered := recover(); recovered != nil {
			executionResult.result = ToolResult{
				Content: fmt.Sprintf("tool panicked: %v", recovered),
				IsError: true,
			}
			if prepared == nil || journal == nil {
				// Preserve the legacy no-journal panic result, which did not
				// report a duration because Tool.Run never returned normally.
				executionResult.elapsed = 0
				return
			}
			_ = journal.MarkOutcomeUnknown(
				context.WithoutCancel(ctx),
				prepared.ExecutionID,
				sideEffectResultDigest(executionResult.result, nil),
			)
			executionResult.result = sideEffectOutcomeUnknownResult(ac.fc.Name)
			fatalErr = wrapSideEffectExecutionError(
				ErrSideEffectOutcomeUnknown, ac.fc.Name,
				fmt.Errorf("tool panicked"),
			)
		}
	}()

	toolCtx, toolCancel := dispatchCtx(ctx, ac.tool)
	if toolCancel != nil {
		defer toolCancel()
	}
	toolCtx = withToolInvocation(toolCtx, ToolInvocation{
		ToolName:    ac.fc.Name,
		ToolUseID:   ac.fc.ID,
		UserRequest: userRequest,
	})
	if prepared != nil {
		toolCtx = withSideEffectExecution(toolCtx, *prepared)
	}

	startTime = time.Now()
	executionResult.executed = true
	executionResult.result, executionResult.err = ac.tool.Run(toolCtx, ac.argsStr)
	if prepared == nil || journal == nil {
		return executionResult, nil
	}

	digest := sideEffectResultDigest(executionResult.result, executionResult.err)
	journalCtx := context.WithoutCancel(ctx)
	failed := executionResult.err != nil || executionResult.result.IsError
	if !failed {
		if err := journal.MarkCommitted(journalCtx, prepared.ExecutionID, digest); err == nil {
			return executionResult, nil
		} else {
			_ = journal.MarkOutcomeUnknown(journalCtx, prepared.ExecutionID, digest)
			executionResult.result = sideEffectOutcomeUnknownResult(ac.fc.Name)
			executionResult.err = nil
			return executionResult, wrapSideEffectExecutionError(
				ErrSideEffectOutcomeUnknown, ac.fc.Name, err,
			)
		}
	}

	knownNoEffect := executionResult.result.ErrorCategory == ErrCategoryValidation
	explicitUnknown := executionResult.err != nil || executionResult.result.SideEffectOutcomeUnknown
	if outcome := executionResult.result.ComputerUseOutcome; outcome != nil &&
		outcome.Validate() == nil {
		knownNoEffect = knownNoEffect || outcome.Effect == ComputerUseCommitNone
		explicitUnknown = explicitUnknown || outcome.Effect == ComputerUseCommitUnknown
	}
	if reporter, ok := ac.tool.(SideEffectNoEffectReporter); ok {
		knownNoEffect = knownNoEffect || reporter.SideEffectFailedWithoutEffect(
			ac.argsStr, executionResult.result, executionResult.err,
		)
	}
	if explicitUnknown {
		markErr := journal.MarkOutcomeUnknown(journalCtx, prepared.ExecutionID, digest)
		executionResult.result = sideEffectOutcomeUnknownResult(ac.fc.Name)
		executionResult.err = nil
		return executionResult, wrapSideEffectExecutionError(
			ErrSideEffectOutcomeUnknown, ac.fc.Name, markErr,
		)
	}
	if knownNoEffect {
		if err := journal.MarkFailedNoEffect(journalCtx, prepared.ExecutionID, digest); err == nil {
			return executionResult, nil
		} else {
			_ = journal.MarkOutcomeUnknown(journalCtx, prepared.ExecutionID, digest)
			executionResult.result = sideEffectOutcomeUnknownResult(ac.fc.Name)
			executionResult.err = nil
			return executionResult, wrapSideEffectExecutionError(
				ErrSideEffectOutcomeUnknown, ac.fc.Name, err,
			)
		}
	}

	// Tool.Run returned a definitive response. IsError describes the tool's
	// domain outcome, not a lost transport response, so preserve it for the
	// agent and checkpoint it exactly like a successful result. Only explicit
	// post-dispatch/no-response evidence above is outcome-unknown.
	if err := journal.MarkCommitted(journalCtx, prepared.ExecutionID, digest); err == nil {
		return executionResult, nil
	} else {
		_ = journal.MarkOutcomeUnknown(journalCtx, prepared.ExecutionID, digest)
		executionResult.result = sideEffectOutcomeUnknownResult(ac.fc.Name)
		executionResult.err = nil
		return executionResult, wrapSideEffectExecutionError(
			ErrSideEffectOutcomeUnknown, ac.fc.Name, err,
		)
	}
}
