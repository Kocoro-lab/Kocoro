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

// MaterialSideEffectChecker is an optional interface a Tool may implement to
// signal whether a specific invocation produces a MATERIAL side effect —
// external-state mutation or task work that a resumed continuation could
// double-execute. This is a third axis, orthogonal to both ReadOnlyChecker
// (caching/dedup) and ConcurrencySafeChecker (batch grouping): a tool can be
// non-read-only for scheduling reasons while having no material side effect
// at all (ask_user_question blocks on user input; use_skill only mutates
// run-local instruction state; process list/ports only observe).
//
// Consumers that gate on side effects (the skill-recommendation
// offer-before-side-effects invariant) consult this FIRST, fall back to
// IsReadOnlyCall, and treat unknown tools as side-effecting (fail closed).
// They deliberately do NOT consult ConcurrencySafeChecker — batch scheduling
// is orthogonal to side effects; a tool whose scheduling analysis happens to
// prove read-onlyness (bash) exposes that through its own implementation of
// THIS interface instead.
type MaterialSideEffectChecker interface {
	HasMaterialSideEffect(argsStr string) bool
}
