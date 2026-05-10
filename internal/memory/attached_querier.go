package memory

import (
	"context"
	"time"
)

// AttachedQuerier is the CLI/TUI-side adapter that satisfies the tool's
// MemoryQuerier contract by dialing an existing sidecar over UDS. It never
// spawns a process. Callers must verify a sidecar is reachable via
// AttachPolicy before constructing one of these.
type AttachedQuerier struct {
	socket  string
	timeout time.Duration
}

func NewAttachedQuerier(socket string, timeout time.Duration) *AttachedQuerier {
	if timeout <= 0 {
		timeout = 5 * time.Second
	}
	return &AttachedQuerier{socket: socket, timeout: timeout}
}

// Status always returns StatusReady — the caller has already verified
// reachability via AttachPolicy. If the sidecar dies between AttachPolicy
// and a Query, the Client returns ClassUnavailable from its transport
// error path and the tool falls back.
func (a *AttachedQuerier) Status() ServiceStatus { return StatusReady }

func (a *AttachedQuerier) Query(ctx context.Context, intent QueryIntent) (*ResponseEnvelope, ErrorClass, error) {
	c := NewClient(a.socket, a.timeout)
	return c.Query(ctx, intent)
}

// QueryBatch runs Query for each intent concurrently and returns results
// in input order. Mirrors Service.QueryBatch so CLI/TUI attach paths
// can use AttachedQuerier wherever Service is accepted.
func (a *AttachedQuerier) QueryBatch(ctx context.Context, intents []QueryIntent) []QueryResult {
	return runQueryBatch(ctx, intents, a.Query)
}
