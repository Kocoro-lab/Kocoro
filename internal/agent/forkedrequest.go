package agent

import (
	"errors"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// ForkOptions controls the few legal divergences from the main request when
// building a forked completion call (prompt suggestion, speculation, future
// sub-agents). Every option here is a deliberate trade-off documented in the
// CACHE SAFETY section below — do not add fields without first verifying the
// new field cannot fragment the Anthropic prompt cache prefix.
type ForkOptions struct {
	// AppendMessages are added at the end of main.Messages. Must be non-empty
	// (a fork that appends nothing is semantically pointless). These are the
	// ONLY content the model sees that wasn't in the main turn.
	AppendMessages []client.Message

	// SkipCacheWrite signals the gateway to read existing prompt cache markers
	// without allocating new cache_control breakpoints on this request. Almost
	// always true for forks (we want main turn's cache, not to spend a breakpoint
	// slot writing our own). Default false in case a future caller wants to
	// write cache; require explicit true to be honored.
	SkipCacheWrite bool

	// DebugKind is logged by SHANNON_CACHE_DEBUG=1 mode to correlate this
	// forked call back to its parent main turn. Examples: "suggestion",
	// "speculation", "subagent-explorer". Never sent on the wire — strictly
	// telemetry. Keep short and stable for log grep ergonomics.
	DebugKind string

	// ToolsAllowlist, when non-nil, restricts the forked request's Tools array
	// to only tools whose Name appears in the allowlist. UNSAFE: this changes
	// the Tools byte representation and fragments the cache prefix. Use ONLY
	// for sub-agents that genuinely need a different tool surface (e.g. a
	// "review" sub-agent restricted to read-only tools). Never use for prompt
	// suggestion / speculation — they MUST inherit the full tools array.
	//
	// Callers that set this MUST emit an audit row tagged
	// "fork_tools_filtered" so cache-regression hunting later can correlate
	// fragmentation with this option.
	ToolsAllowlist []string
}

// BuildForkedRequest returns a CompletionRequest derived from `main` per the
// given opts. The returned value is byte-equal to main on every field except
// Messages (extended via opts.AppendMessages), SkipCacheWrite (set to
// opts.SkipCacheWrite), ForkedKind (set to opts.DebugKind), and — if
// opts.ToolsAllowlist is set — Tools (filtered).
//
// CACHE SAFETY: with default opts (SkipCacheWrite: true, ToolsAllowlist: nil),
// the entire main.Messages / Tools / system / model / API params prefix is
// byte-stable. Anthropic's prompt cache hits the full prefix on the forked
// call. Setting ToolsAllowlist breaks this — opt-in only.
//
// Returns an error if opts.AppendMessages is empty.
func BuildForkedRequest(main client.CompletionRequest, opts ForkOptions) (client.CompletionRequest, error) {
	if len(opts.AppendMessages) == 0 {
		return client.CompletionRequest{}, errors.New("forkedrequest: AppendMessages must be non-empty")
	}

	out := main // shallow copy of value-type struct

	// Defensive deep-copy of Messages — must NOT share backing array with main,
	// otherwise callers mutating `out.Messages[i]` would corrupt `main.Messages[i]`.
	out.Messages = make([]client.Message, 0, len(main.Messages)+len(opts.AppendMessages))
	out.Messages = append(out.Messages, main.Messages...)
	out.Messages = append(out.Messages, opts.AppendMessages...)

	out.SkipCacheWrite = opts.SkipCacheWrite
	out.ForkedKind = opts.DebugKind

	// CACHE-FRAGMENTING: only applied if explicitly opted-in.
	if opts.ToolsAllowlist != nil {
		allow := make(map[string]bool, len(opts.ToolsAllowlist))
		for _, n := range opts.ToolsAllowlist {
			allow[n] = true
		}
		filtered := make([]client.Tool, 0, len(main.Tools))
		for _, t := range main.Tools {
			if allow[t.Name] {
				filtered = append(filtered, t)
			}
		}
		out.Tools = filtered
	}

	return out, nil
}
