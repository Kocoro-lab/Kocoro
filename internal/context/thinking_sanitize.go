package context

import (
	"slices"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// DropMalformedThinking removes content blocks that would serialize to the
// wire shape the upstream LLM API rejects with:
//
//	messages.N.content.M.thinking.thinking: Field required
//
// Background. internal/client/gateway.go declares
//
//	Thinking string `json:"thinking,omitempty"`
//	Data     string `json:"data,omitempty"`
//
// so a thinking block whose Thinking text is empty marshals to
// `{"type":"thinking","signature":"..."}` — the required key is silently
// dropped by encoding/json. The upstream API treats that as a schema
// violation and rejects the entire request, taking down every following
// turn on the affected session until the offending block is removed.
//
// Empty thinking/redacted_thinking blocks can land in a session via a
// Cloud relay that lost the text in a transient SSE truncation but kept
// the matching signature block, or via a streaming idle timeout firing
// mid-block with the partially-accumulated ContentBlock persisted verbatim.
//
// Wiring (defense-in-depth, two layers):
//   - internal/agent/loop.messagesForLLM calls this on every outbound
//     request, covering the three CompletionRequest assembly sites with one
//     call.
//   - internal/session/manager.Load calls this on every reload, so disk
//     copies self-heal on first read and the next Save persists the clean
//     shape — no manual repair tool needed.
//
// Edge case. If filtering empties out a message's block list, a single
// empty-text block is inserted as a placeholder so the downstream
// RepairEmptyAssistantContent pass can decide whether to drop or merge it
// — keeping the empty-content policy centralized in one place
// (see sanitize.go).
//
// When SHANNON_CACHE_DEBUG=1, each affected message logs a
// "thinking_drop_empty" cache-compact event for drift attribution.
//
// Idempotent. Mutates the input slice in place; returns the same slice so
// callers can chain with RepairEmptyAssistantContent. Fast path: a single
// O(N+B) pre-scan returns immediately when no message qualifies, with no
// allocation.
func DropMalformedThinking(messages []client.Message) []client.Message {
	if !needsThinkingDrop(messages) {
		return messages
	}
	for i := range messages {
		old := messages[i].Content
		if !old.HasBlocks() {
			continue
		}
		blocks := old.Blocks()
		kept := make([]client.ContentBlock, 0, len(blocks))
		dropped := false
		for _, b := range blocks {
			if isMalformedThinking(b) {
				dropped = true
				continue
			}
			kept = append(kept, b)
		}
		if !dropped {
			continue
		}
		if len(kept) == 0 {
			kept = []client.ContentBlock{{Type: "text", Text: ""}}
		}
		messages[i].Content = client.NewBlockContent(kept)
		client.LogCacheCompactEvent("thinking_drop_empty", i, old, messages[i].Content)
	}
	return messages
}

func needsThinkingDrop(messages []client.Message) bool {
	for _, m := range messages {
		if !m.Content.HasBlocks() {
			continue
		}
		if slices.ContainsFunc(m.Content.Blocks(), isMalformedThinking) {
			return true
		}
	}
	return false
}

func isMalformedThinking(b client.ContentBlock) bool {
	switch b.Type {
	case "thinking":
		return b.Thinking == ""
	case "redacted_thinking":
		return b.Data == ""
	}
	return false
}
