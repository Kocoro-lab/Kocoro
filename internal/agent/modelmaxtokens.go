package agent

import "strings"

// modelMaxOutputTokens maps known model IDs to their per-response output
// ceiling — the hard cap on a single completion's output tokens, distinct
// from the context window.
//
// Source of truth:
//   - Upstream provider model docs (for each vendor)
//   - shannon-cloud/config/models.yaml (cloud-side enforcement)
//
// Used by AgentLoop when constructing CompletionRequest.MaxTokens: if the
// user did not set agent.max_tokens explicitly (config default 0), we look
// up the model and pass its physical upper limit. This stops the daemon
// from artificially capping output at the legacy 32K default when the
// model and cloud both allow 64K.
//
// Re-verify on every model launch — providers occasionally raise caps
// without bumping the family number. Keep in sync with
// shannon-cloud/config/models.yaml#max_tokens.
//
// Unknown model → fallback defaultMaxOutputTokens (32K) — conservative
// enough to cover Opus 4.1 era and any third-party / Ollama model.
var modelMaxOutputTokens = map[string]int{
	// --- 1M-context families (output cap is independent of context) ---
	"claude-sonnet-4-6": 64_000,
	"claude-opus-4-6":   64_000,
	"claude-opus-4-7":   64_000,

	// --- 200K, dated forms ---
	"claude-sonnet-4-5-20250929": 64_000,
	"claude-haiku-4-5-20251001":  64_000,
	"claude-opus-4-5-20251101":   64_000,
	"claude-sonnet-4-20250514":   64_000,
	"claude-opus-4-1-20250805":   32_000, // 4.1 family caps lower
	"claude-opus-4-20250514":     32_000,

	// --- 200K, dateless floating tags ---
	"claude-sonnet-4-5": 64_000,
	"claude-haiku-4-5":  64_000,
	"claude-opus-4-5":   64_000,
	"claude-opus-4-1":   32_000,

	// --- GPT-5 / GPT-4.1 (Responses API: max_output_tokens cap) ---
	"gpt-5.1":               128_000,
	"gpt-5.1-chat-latest":   128_000,
	"gpt-5-pro-2025-10-06":  128_000,
	"gpt-5-mini-2025-08-07": 128_000,
	"gpt-5-nano-2025-08-07": 128_000,
	"gpt-4.1-2025-04-14":    32_000,
}

// modelMaxOutputTokensPrefix matches forward-compat dated variants of
// dateless family IDs. Same pattern as modelContextWindowPrefix.
var modelMaxOutputTokensPrefix = map[string]int{
	"claude-sonnet-4-6-": 64_000,
	"claude-opus-4-6-":   64_000,
	"claude-opus-4-7-":   64_000,
	"claude-sonnet-4-5-": 64_000,
	"claude-haiku-4-5-":  64_000,
	"claude-opus-4-5-":   64_000,
	"claude-opus-4-1-":   32_000,
}

// maxTokensPrefixOrder is modelMaxOutputTokensPrefix's keys sorted by length
// descending so longest-prefix-first matching is deterministic.
var maxTokensPrefixOrder []string

// defaultMaxOutputTokens is the fallback when the model is unknown — large
// enough to be useful for most workloads, small enough to be safe against
// providers that don't actually allow the higher caps.
const defaultMaxOutputTokens = 32_000

func init() {
	maxTokensPrefixOrder = make([]string, 0, len(modelMaxOutputTokensPrefix))
	for k := range modelMaxOutputTokensPrefix {
		maxTokensPrefixOrder = append(maxTokensPrefixOrder, k)
	}
	for i := 1; i < len(maxTokensPrefixOrder); i++ {
		for j := i; j > 0 && len(maxTokensPrefixOrder[j]) > len(maxTokensPrefixOrder[j-1]); j-- {
			maxTokensPrefixOrder[j], maxTokensPrefixOrder[j-1] = maxTokensPrefixOrder[j-1], maxTokensPrefixOrder[j]
		}
	}
}

// MaxTokensForModel returns the per-response output cap for a model ID,
// falling back to defaultMaxOutputTokens for unknown models. Empty string
// also returns the default (e.g. when AgentLoop.model has not been
// populated yet).
func MaxTokensForModel(modelID string) int {
	if modelID == "" {
		return defaultMaxOutputTokens
	}
	if v, ok := modelMaxOutputTokens[modelID]; ok {
		return v
	}
	for _, prefix := range maxTokensPrefixOrder {
		if strings.HasPrefix(modelID, prefix) {
			return modelMaxOutputTokensPrefix[prefix]
		}
	}
	return defaultMaxOutputTokens
}
