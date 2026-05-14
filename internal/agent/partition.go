package agent

import (
	"context"
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

// partitionToolCalls groups approved tool calls into execution batches.
// Consecutive read-only calls are grouped into a single concurrent batch.
// Non-read-only calls each get their own sequential batch of size 1.
func partitionToolCalls(approved []approvedToolCall) [][]approvedToolCall {
	if len(approved) == 0 {
		return nil
	}
	var batches [][]approvedToolCall
	var currentBatch []approvedToolCall
	currentIsReadOnly := false

	for i, ac := range approved {
		ro := isReadOnly(ac)
		if i == 0 {
			currentBatch = []approvedToolCall{ac}
			currentIsReadOnly = ro
			continue
		}
		if ro && currentIsReadOnly {
			currentBatch = append(currentBatch, ac)
		} else {
			batches = append(batches, currentBatch)
			currentBatch = []approvedToolCall{ac}
			currentIsReadOnly = ro
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
func executeBatches(ctx context.Context, batches [][]approvedToolCall, execResults []toolExecResult, readTracker *ReadTracker, handler EventHandler) {
	// Attach the handler's OnUsage as the per-run usage emitter so tools
	// that bill per call (gateway tools reporting xAI/Grok or SerpAPI costs)
	// can fold their usage into the session totals.
	if handler != nil {
		ctx = WithUsageEmit(ctx, handler.OnUsage)
	}
	for _, batch := range batches {
		if len(batch) == 1 {
			// Single call: run directly, no goroutine overhead.
			ac := batch[0]
			func() {
				defer func() {
					if r := recover(); r != nil {
						execResults[ac.index] = toolExecResult{
							result: ToolResult{Content: fmt.Sprintf("tool panicked: %v", r), IsError: true},
						}
					}
				}()
				if handler != nil {
					handler.OnToolCall(ac.fc.Name, ac.argsStr)
				}
				startTime := time.Now()
				result, runErr := ac.tool.Run(ctx, ac.argsStr)
				execResults[ac.index] = toolExecResult{result: result, elapsed: time.Since(startTime), err: runErr}
			}()
		} else {
			// Concurrent batch with semaphore.
			sem := make(chan struct{}, maxToolConcurrency)
			var wg sync.WaitGroup
			wg.Add(len(batch))
			for _, ac := range batch {
				sem <- struct{}{} // acquire — blocks until a slot is free
				// Emit "running" after acquiring the slot so the event reflects
				// actual execution start, not just batch membership. Called from
				// the main goroutine so handler writes stay serialized.
				if handler != nil {
					handler.OnToolCall(ac.fc.Name, ac.argsStr)
				}
				go func(ac approvedToolCall) {
					defer wg.Done()
					defer func() { <-sem }() // release
					defer func() {
						if r := recover(); r != nil {
							execResults[ac.index] = toolExecResult{
								result: ToolResult{Content: fmt.Sprintf("tool panicked: %v", r), IsError: true},
							}
						}
					}()
					startTime := time.Now()
					result, runErr := ac.tool.Run(ctx, ac.argsStr)
					execResults[ac.index] = toolExecResult{result: result, elapsed: time.Since(startTime), err: runErr}
				}(ac)
			}
			wg.Wait()
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
	}
}
