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
	// when cheap lexical gates do not fire. Callers should use it sparingly, for
	// example on the first user message of a memory-enabled conversation.
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
	Attempted       bool
	ForceHelper     bool
	HelperUsed      bool
	IntentSource    string
	IntentsCount    int
	Queried         bool
	ResultsCount    int
	ContextReturned bool
	ContextInjected bool
	Outcome         string
	ErrorClass      string
	HTTPStatus      int
}
