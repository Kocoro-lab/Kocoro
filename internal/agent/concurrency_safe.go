package agent

// ConcurrencySafeChecker is an optional interface a Tool may implement to
// signal whether a specific invocation can run concurrently with other tool
// calls in the same agent turn. This is orthogonal to ReadOnlyChecker:
// ReadOnlyChecker drives caching/dedup decisions, while ConcurrencySafeChecker
// drives the dispatcher's batch grouping.
//
// Tools that do NOT implement this interface fall back to their
// IsReadOnlyCall return value (see isConcurrencySafe in partition.go), so the
// default behavior is unchanged: read-only tools batch concurrently, others
// run sequentially. Explicit implementers can diverge — e.g. BashTool can
// return true for commands proven safe by static analysis even though the
// tool itself is not read-only.
type ConcurrencySafeChecker interface {
	IsConcurrencySafeCall(argsStr string) bool
}
