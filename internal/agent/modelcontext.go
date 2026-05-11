package agent

import "strings"

// modelContextWindow maps known model IDs to their context window in tokens.
//
// Source of truth: Anthropic / OpenAI / Google / xAI official model docs and
// shannon-cloud/config/models.yaml. Update both when bumping a window.
//
// Used by AgentLoop.reportLLMUsage to auto-adjust the compaction threshold
// based on the model that actually served the request (read from
// CompletionResponse.Model). Auto-adjust only applies when the user did not
// explicitly configure agent.context_window — see SetContextWindowExplicit.
//
// Unknown models leave contextWindow untouched (graceful degradation).
var modelContextWindow = map[string]int{
	// --- Anthropic (1M-context: GA, no beta header, standard pricing) ---
	"claude-sonnet-4-6":          1_000_000,
	"claude-opus-4-6":            1_000_000,
	"claude-opus-4-7":            1_000_000,
	"claude-mythos-preview":      1_000_000,

	// --- Anthropic (200K, dated forms) ---
	"claude-sonnet-4-5-20250929": 200_000,
	"claude-haiku-4-5-20251001":  200_000,
	"claude-opus-4-5-20251101":   200_000,
	"claude-opus-4-1-20250805":   200_000,
	"claude-sonnet-4-20250514":   200_000,
	"claude-opus-4-20250514":     200_000,

	// --- Anthropic (200K, dateless floating tags) ---
	// Symmetric with the 1M dateless entries above. Without these, a cloud
	// failover that returns response.model="claude-sonnet-4-5" (dateless)
	// would hit (0,false) and leave the loop at the prior 1M assumption,
	// then 400 on the next prompt. (PR review item #3.)
	"claude-sonnet-4-5": 200_000,
	"claude-haiku-4-5":  200_000,
	"claude-opus-4-5":   200_000,
	"claude-opus-4-1":   200_000,

	// --- OpenAI ---
	"gpt-5.1":                400_000,
	"gpt-5.1-chat-latest":    400_000,
	"gpt-5-pro-2025-10-06":   400_000,
	"gpt-5-mini-2025-08-07":  400_000,
	"gpt-5-nano-2025-08-07":  400_000,
	"gpt-4.1-2025-04-14":     128_000,

	// --- Google Gemini ---
	"gemini-3-pro-preview":  1_000_000,
	"gemini-2.5-pro":        1_048_576,
	"gemini-2.5-flash":      1_048_576,
	"gemini-2.5-flash-lite": 1_048_576,
	"gemini-2.0-flash":      1_048_576,
	"gemini-2.0-flash-lite": 1_048_576,

	// --- xAI Grok ---
	"grok-4-1-fast-non-reasoning": 2_000_000,
	"grok-4-1-fast-reasoning":     2_000_000,
	"grok-4.20-0309-reasoning":    2_000_000,

	// --- Others routed by Shannon Cloud (medium/large tiers) ---
	"kimi-k2-turbo-preview": 256_000,
	"kimi-k2.5":             256_000,
	"kimi-k2-thinking":      256_000,
}

// modelContextWindowPrefix handles forward-compat for dated variants of
// dateless model IDs (e.g., a future "claude-sonnet-4-6-20260301" snapshot
// of claude-sonnet-4-6). Both 1M and 200K families are listed symmetrically
// — without 200K prefixes a cloud failover from a 1M priority-1 model
// (sonnet-4-6) to a future-dated 200K model (e.g. claude-haiku-4-5-20260601)
// would return (0,false), the loop would keep its 1M assumption, and the
// next request would 400 at the actual 200K cap. See PR review item #3.
//
// Lookup uses longest-prefix-first via prefixLookupOrder so that the more
// specific "claude-sonnet-4-6-" (1M) wins over the broader
// "claude-sonnet-4-" (200K) when both could match.
var modelContextWindowPrefix = map[string]int{
	// 1M families
	"claude-sonnet-4-6-": 1_000_000,
	"claude-opus-4-6-":   1_000_000,
	"claude-opus-4-7-":   1_000_000,
	// 200K families. Note: we intentionally do NOT add a broader
	// "claude-sonnet-4-" prefix — that would silently catch hypothetical
	// future "claude-sonnet-4-NN" forms (e.g. claude-sonnet-4-60) and
	// down-cap a model whose actual window is unknown. Per the
	// "sonnet 4.6 numeric suffix is NOT a dated variant" test, unknown
	// model IDs must return (0,false) so callers leave the existing
	// contextWindow untouched (graceful degradation).
	"claude-sonnet-4-5-": 200_000,
	"claude-haiku-4-5-":  200_000,
	"claude-opus-4-5-":   200_000,
	"claude-opus-4-1-":   200_000,
}

// prefixLookupOrder is modelContextWindowPrefix's keys sorted by length
// descending. Used to enforce longest-prefix-first matching since Go map
// iteration is randomized. Re-built lazily on first call.
var prefixLookupOrder []string

func init() {
	prefixLookupOrder = make([]string, 0, len(modelContextWindowPrefix))
	for k := range modelContextWindowPrefix {
		prefixLookupOrder = append(prefixLookupOrder, k)
	}
	// Sort by length descending (so "claude-sonnet-4-6-" beats "claude-sonnet-4-").
	for i := 1; i < len(prefixLookupOrder); i++ {
		for j := i; j > 0 && len(prefixLookupOrder[j]) > len(prefixLookupOrder[j-1]); j-- {
			prefixLookupOrder[j], prefixLookupOrder[j-1] = prefixLookupOrder[j-1], prefixLookupOrder[j]
		}
	}
}

// LookupModelContextWindow returns the known context window for a model ID
// (including future dated variants of known dateless families). Returns
// (0, false) when the model is unknown — callers should leave existing
// contextWindow untouched in that case.
func LookupModelContextWindow(modelID string) (int, bool) {
	if modelID == "" {
		return 0, false
	}
	if v, ok := modelContextWindow[modelID]; ok {
		return v, true
	}
	for _, prefix := range prefixLookupOrder {
		if strings.HasPrefix(modelID, prefix) {
			return modelContextWindowPrefix[prefix], true
		}
	}
	return 0, false
}
