package agent

import (
	"context"
	"fmt"
	"sync"
	"time"
)

const maxToolConcurrency = 10

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
func executeBatches(ctx context.Context, batches [][]approvedToolCall, execResults []toolExecResult, readTracker *ReadTracker) {
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
				sem <- struct{}{} // acquire
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
