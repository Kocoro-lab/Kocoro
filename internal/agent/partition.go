package agent

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
