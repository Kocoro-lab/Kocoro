package agent

import (
	"context"
	"fmt"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// BuildSpeculationRequest returns a CompletionRequest that pre-runs the next
// assistant turn assuming the user accepts the suggestion text. Thin wrapper
// over BuildForkedRequest — the appended message is the actual suggestion
// (not the meta SuggestionPrompt), so the model produces a real response we
// can serve instantly if the user accepts.
//
// CACHE SAFETY: inherits the BuildForkedRequest invariant — byte-equal to main
// except for the one appended message, SkipCacheWrite, and ForkedKind.
func BuildSpeculationRequest(main client.CompletionRequest, suggestionText string) client.CompletionRequest {
	out, _ := BuildForkedRequest(main, ForkOptions{
		AppendMessages: []client.Message{{Role: "user", Content: client.NewTextContent(suggestionText)}},
		SkipCacheWrite: true,
		DebugKind:      "speculation",
	})
	return out
}

// SpeculationResult bundles the speculation response text with the cache/usage
// data from the forked call so the caller can audit cache health. Text is the
// raw model output (no filter applied — speculation responses are displayed
// verbatim if accepted); Usage and Model come from the gateway response.
type SpeculationResult struct {
	Text  string
	Usage client.Usage
	Model string
}

// RunSpeculation runs a single forked LLM call that pre-computes the
// assistant's response to suggestionText. Returns the assistant's content
// or empty string + error on gateway failure. No filter is applied — the
// output is destined for display verbatim if accepted.
//
// MVP scope: single Complete() call, no tool execution. Speculation that
// would have triggered tool_use is shown as the raw text leading up to the
// tool_use block (truncated at that boundary). A future Phase can extend
// to a full forked AgentLoop run with skipTranscript.
//
// Thin wrapper over RunSpeculationWithUsage that discards Usage/Model. Callers
// that need cache-health auditing (daemon post-Run hook) call the WithUsage
// variant directly.
func RunSpeculation(ctx context.Context, llm client.LLMClient, main client.CompletionRequest, suggestionText string) (string, error) {
	res, err := RunSpeculationWithUsage(ctx, llm, main, suggestionText)
	return res.Text, err
}

// RunSpeculationWithUsage is the Usage-returning variant of RunSpeculation.
// Used by the daemon's post-Run hook to write audit rows that record the
// forked call's cache_read_tokens / cache_creation_tokens.
func RunSpeculationWithUsage(ctx context.Context, llm client.LLMClient, main client.CompletionRequest, suggestionText string) (SpeculationResult, error) {
	req := BuildSpeculationRequest(main, suggestionText)

	resp, err := llm.Complete(ctx, req)
	if err != nil {
		return SpeculationResult{}, fmt.Errorf("speculation gateway call: %w", err)
	}
	if resp == nil {
		return SpeculationResult{}, nil
	}
	result := SpeculationResult{
		Text:  resp.OutputText,
		Usage: resp.Usage,
		Model: resp.Model,
	}
	return result, nil
}
