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
}

// BuildForkedRequest returns a CompletionRequest derived from `main` per the
// given opts. The returned value is byte-equal to main on every field except
// Messages (extended via opts.AppendMessages), SkipCacheWrite (set to
// opts.SkipCacheWrite), and ForkedKind (set to opts.DebugKind).
//
// # Cache key invariant (read before changing this function)
//
// The Anthropic prompt-cache server-side cache key is composed of:
//
//	  (1) system prompt
//	  (2) tools array (order + names + schemas)
//	  (3) model (specific model string — SpecificModel or model_tier resolution)
//	  (4) messages prefix (every Role + Content byte-equal)
//	  (5) thinking config (Type + BudgetTokens)
//
// Plus shannon-cloud adds:
//
//	  (6) cache_control block bytes (resolved by cloud from `cache_source`)
//
// For a forked call to hit the parent's cache, items (1)-(6) must be
// byte-identical to the parent's most recent request. This function
// preserves all of them by `out := main` shallow-copy + deep-copy of slice
// and pointer fields that would otherwise alias.
//
// # What this function does NOT defend against
//
// Once the returned CompletionRequest leaves this function, a caller is
// physically free to mutate any field on it. Doing so on Tools / Model /
// SpecificModel / ReasoningEffort / Temperature / MaxTokens / Thinking /
// CacheSource / Stream / ToolChoice / SessionID will break cache.
//
// The mistake CC's `runForkedAgent` documented hitting (forkedAgent.ts:96-103
// in their leaked rollback-version source) is exactly this pattern: a fork
// passes a smaller maxOutputTokens to bound suggestion output, which on their
// architecture clamps thinking.budget_tokens, which is in the cache key, which
// invalidates the cache. We don't have that specific clamping (cloud handles
// it), but the principle is identical: do NOT touch these fields after
// BuildForkedRequest returns. Tests in this package include a regression
// guard (TestBuildForkedRequest_CallerMutationBreaksByteEquality) that
// documents the failure mode.
//
// Returns an error if opts.AppendMessages is empty.
func BuildForkedRequest(main client.CompletionRequest, opts ForkOptions) (client.CompletionRequest, error) {
	if len(opts.AppendMessages) == 0 {
		return client.CompletionRequest{}, errors.New("forkedrequest: AppendMessages must be non-empty")
	}

	out := main // shallow copy of value-type struct
	// NOTE: in the no-allowlist path, out.Tools still shares its backing array
	// with main.Tools. Callers MUST treat the returned forked.Tools as read-only
	// — appending or mutating elements can corrupt the main request. The current
	// suggestion caller never writes to Tools, so this is fine in practice; a
	// future caller that needs a different tool surface (e.g. a "review" sub-
	// agent restricted to read-only tools) must reintroduce an explicit filter
	// option AND emit a fork_tools_filtered audit row — the cache-fragmentation
	// hazard is documented in CACHE INVARIANT above.

	// Defensive deep-copy of Messages — must NOT share backing array with main,
	// otherwise callers mutating `out.Messages[i]` would corrupt `main.Messages[i]`.
	out.Messages = make([]client.Message, 0, len(main.Messages)+len(opts.AppendMessages))
	out.Messages = append(out.Messages, main.Messages...)
	out.Messages = append(out.Messages, opts.AppendMessages...)

	// Thinking is a pointer — shallow-copy of `main` aliases the same struct.
	// A caller mutating out.Thinking.BudgetTokens would silently corrupt
	// main.Thinking, taking down both the suggestion AND the parent's cache
	// prefix. Deep-copy here closes that footgun.
	if main.Thinking != nil {
		thinkingCopy := *main.Thinking
		out.Thinking = &thinkingCopy
	}

	out.SkipCacheWrite = opts.SkipCacheWrite
	out.ForkedKind = opts.DebugKind

	return out, nil
}
