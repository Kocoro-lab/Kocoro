package mcp

import (
	"fmt"

	"github.com/mark3labs/mcp-go/mcp"
)

// OutcomeUnknownError marks a tools/call whose request was dispatched to a
// live server but whose connection died before a response arrived. This is
// the in-doubt window: the server may have executed the tool's side effect
// and exited before writing the JSON-RPC response, or it may have died
// before acting — the wire cannot distinguish the two. Callers must NOT
// silently re-dispatch a non-idempotent tool on this error; the correct
// recovery is to verify the effect (or surface the ambiguity) and let the
// model or user decide.
//
// Unwrap exposes the underlying transport failure so IsTransportError
// classification is preserved through the wrap.
type OutcomeUnknownError struct {
	Server string
	Tool   string
	Err    error
}

func (e *OutcomeUnknownError) Error() string {
	return fmt.Sprintf("tools/call %s/%s was dispatched but the connection died before a response (outcome unknown): %v", e.Server, e.Tool, e.Err)
}

func (e *OutcomeUnknownError) Unwrap() error { return e.Err }

// ToolReplaySafe reports whether a tool may be automatically re-dispatched
// after a POST-dispatch transport failure. Only the tool's own MCP
// annotations can authorize that: a read-only or idempotent tool executes
// twice without harm; anything else (send-message, create-event, unannotated
// tools) must not be replayed because the first dispatch may already have
// taken effect. Annotations are server-supplied and advisory — absent
// annotations fail closed to "not safe".
//
// Pre-dispatch reconnects (connection known dead BEFORE the request was
// written) are not gated by this: a first dispatch on a fresh connection
// carries no duplication risk.
func ToolReplaySafe(t mcp.Tool) bool {
	a := t.Annotations
	return (a.ReadOnlyHint != nil && *a.ReadOnlyHint) ||
		(a.IdempotentHint != nil && *a.IdempotentHint)
}
