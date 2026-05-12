package agent

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"math"
	"strings"
)

const (
	// schemaTokenBudget is the maximum token budget we allow for tool schemas
	// before switching to deferred mode. 8000 tokens is roughly 28K chars of
	// compact schema JSON at the conservative 3.5 chars/token ratio.
	schemaTokenBudget = 8000

	// charsPerTokenSchema mirrors the context estimator's conservative ratio.
	charsPerTokenSchema = 3.5
)

// alwaysDeferTools enumerates local tools whose schemas should start out
// deferred regardless of the total schema budget. These are categories that
// most one-shot CLI tasks never use (macOS GUI automation, scheduling,
// headless process control). Keeping them in the deferred set means cold-start
// `tools[]` ships ~5K fewer tokens; sessions that DO need them pay one extra
// `tool_search` round-trip per session.
//
// memory_recall is intentionally NOT in this set: it is the explicit fallback
// path when the implicit memory preflight injects nothing (or the model needs
// a relation the injected block did not cover). Keeping it always-loaded
// avoids the "model hallucinates an older schema on first attempt → wasted
// turn" failure mode observed in production, and the ~1.5K extra schema bytes
// ride along in the cacheable system prefix so the cost is paid once per
// session, not per turn.
//
// The browser_* prefix is matched separately by shouldDeferByCategory.
//
// See docs/issues/cache-action-plan.md §1.2 for the data behind this list.
var alwaysDeferTools = map[string]bool{
	"computer":        true,
	"screenshot":      true,
	"applescript":     true,
	"accessibility":   true,
	"wait_for":        true,
	"ghostty":         true,
	"process":         true,
	"schedule_create": true,
	"schedule_list":   true,
	"schedule_remove": true,
	"schedule_update": true,
}

// shouldDeferByCategory reports whether a tool name belongs to a category
// that should be deferred by default (independent of total schema budget).
func shouldDeferByCategory(name string) bool {
	if alwaysDeferTools[name] {
		return true
	}
	return strings.HasPrefix(name, "browser_")
}

// estimateSchemaTokens returns a heuristic token count for the named tool
// schemas using compact JSON serialization.
func estimateSchemaTokens(reg *ToolRegistry, names []string) int {
	if reg == nil || len(names) == 0 {
		return 0
	}

	total := 0
	for _, name := range names {
		t, ok := reg.Get(name)
		if !ok {
			continue
		}
		data, err := json.Marshal(buildToolSchema(t))
		if err != nil {
			continue
		}
		total += int(math.Ceil(float64(len(data)) / charsPerTokenSchema))
	}
	return total
}

// shouldDefer returns true when sending all named schemas would exceed budget.
func shouldDefer(reg *ToolRegistry, names []string, budget int) bool {
	if budget <= 0 {
		return false
	}
	return estimateSchemaTokens(reg, names) > budget
}

// toolSchemaFingerprint hashes the effective serialized tool schemas in
// deterministic name order. This is used to invalidate warmed deferred
// schemas whenever the real toolset changes.
func toolSchemaFingerprint(reg *ToolRegistry) string {
	if reg == nil {
		return ""
	}

	h := sha256.New()
	for _, name := range reg.SortedNames() {
		t, ok := reg.Get(name)
		if !ok {
			continue
		}
		data, err := json.Marshal(buildToolSchema(t))
		if err != nil {
			continue
		}
		_, _ = h.Write([]byte(name))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write(data)
		_, _ = h.Write([]byte{'\n'})
	}
	return hex.EncodeToString(h.Sum(nil))
}
