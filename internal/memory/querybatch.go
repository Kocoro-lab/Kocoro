package memory

import (
	"context"
	"sync"
)

// QueryResult holds the per-call outcome from QueryBatch. Envelope is nil
// when the call produced no body (transport error, timeout, service not
// ready); callers MUST check Envelope != nil before reading fields.
type QueryResult struct {
	Envelope *ResponseEnvelope
	Class    ErrorClass
	Err      error
}

// QueryBatch runs Query for each intent concurrently and returns results in
// input order. All goroutines share the supplied ctx; cancel/deadline
// propagates to every in-flight call. Empty input returns nil.
//
// Per-call timeout: callers wanting a per-call cap should derive a child
// ctx with a tighter deadline before calling. The shared-ctx model is
// sufficient when all calls launch at once and a slow call is acceptable
// to drop together with the others on deadline.
func (s *Service) QueryBatch(ctx context.Context, intents []QueryIntent) []QueryResult {
	return runQueryBatch(ctx, intents, s.Query)
}

// queryFn is the call shape QueryBatch dispatches. Extracted so the
// concurrent runner is testable without a live Service.
type queryFn func(ctx context.Context, intent QueryIntent) (*ResponseEnvelope, ErrorClass, error)

func runQueryBatch(ctx context.Context, intents []QueryIntent, q queryFn) []QueryResult {
	if len(intents) == 0 {
		return nil
	}
	results := make([]QueryResult, len(intents))
	var wg sync.WaitGroup
	wg.Add(len(intents))
	for i, intent := range intents {
		go func(idx int, qi QueryIntent) {
			defer wg.Done()
			env, class, err := q(ctx, qi)
			results[idx] = QueryResult{Envelope: env, Class: class, Err: err}
		}(i, intent)
	}
	wg.Wait()
	return results
}
