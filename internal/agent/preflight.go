package agent

import (
	"context"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// MemoryPreflightFunc optionally injects private episodic context before the
// first main-model call. It must fail silent: nil means proceed normally.
type MemoryPreflightFunc func(ctx context.Context, query string, opts MemoryPreflightOptions) *MemoryPreflightResult

type MemoryPreflightOptions struct {
	// ForceHelper asks the implementation to run its small-model compiler even
	// when cheap lexical gates do not fire. Production turns leave this false;
	// it exists for explicit diagnostics and targeted evaluation only.
	ForceHelper bool
	Trace       *MemoryPreflightTrace
}

type MemoryPreflightResult struct {
	Context string
	Usage   client.Usage
}

// MemoryPreflightTrace carries low-sensitivity observability for private
// memory preflight. It must never contain the user query, anchors, relation
// labels selected by the helper, or recalled memory text.
type MemoryPreflightTrace struct {
	Attempted        bool
	ForceHelper      bool
	HelperUsed       bool
	HelperDurationMs int64
	IntentSource     string
	IntentsCount     int
	Queried          bool
	QueryDurationMs  int64
	ResultsCount     int
	ContextReturned  bool
	ContextInjected  bool
	TotalDurationMs  int64
	Outcome          string
	ErrorClass       string
	HTTPStatus       int
}
