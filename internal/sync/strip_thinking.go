package sync

import (
	"encoding/json"
)

// stripThinkingFromSessionJSON returns a copy of the session JSON body with
// `thinking` and `redacted_thinking` content blocks removed from every
// assistant message's content array. Other fields and the message order are
// preserved verbatim.
//
// Why on the upload path: thinking content can contain sensitive intermediate
// reasoning (private deliberations the user never sees). The local session
// file keeps it for cross-turn trajectory continuity, but the cloud sync
// endpoint uses sessions for cross-device resume — it doesn't need thinking.
// Stripping keeps the disclosure surface tight while leaving roundtrip
// behavior intact for the on-disk JSON.
//
// Why on the byte level (rather than going through `*session.Session`): the
// sync loader at internal/sync/batcher.go:54 already returns marshaled JSON
// bytes, and BuildBatches applies the `SingleSessionMaxBytes` check on those
// bytes a few lines later. Calling this helper directly on the loader output
// makes the size check operate on the post-strip bytes — which is what users
// expect when they configure a size limit and turn on thinking-block uploads.
//
// On parse failure, returns the original body unchanged plus the parse error.
// The caller may opt to log + continue (preferred, to avoid blocking sync on
// a corrupt local file) or treat as load_error (strict).
//
// Note: on the mutation path the output is re-marshaled through map[string]any,
// which `encoding/json` emits with alphabetically-sorted keys. The returned
// bytes therefore are NOT byte-identical to the on-disk JSON even ignoring
// the stripped blocks (key order shifts). This is intentional and acceptable
// for the current upload path (the cloud ingest does structural parsing, not
// byte-hash dedup). If a future caller needs byte-equality with the on-disk
// file (e.g. for content-addressed dedup), swap the implementation to a
// surgical edit that preserves key order — e.g. walk + splice the JSON token
// stream rather than round-tripping through map[string]any.
func stripThinkingFromSessionJSON(body []byte) ([]byte, error) {
	if len(body) == 0 {
		return body, nil
	}

	var top map[string]any
	if err := json.Unmarshal(body, &top); err != nil {
		return body, err
	}

	rawMessages, ok := top["messages"].([]any)
	if !ok {
		// No messages array (or unexpected shape) — nothing to strip.
		return body, nil
	}

	mutated := false
	for _, rawMsg := range rawMessages {
		msg, ok := rawMsg.(map[string]any)
		if !ok {
			continue
		}
		role, _ := msg["role"].(string)
		if role != "assistant" {
			continue
		}
		rawContent, ok := msg["content"].([]any)
		if !ok {
			// content is a plain string or missing → no thinking blocks to drop.
			continue
		}

		filtered := make([]any, 0, len(rawContent))
		dropped := false
		for _, rawBlock := range rawContent {
			block, ok := rawBlock.(map[string]any)
			if !ok {
				// Non-object entry (shouldn't happen for assistant content,
				// but pass through defensively rather than silently drop).
				filtered = append(filtered, rawBlock)
				continue
			}
			blockType, _ := block["type"].(string)
			if blockType == "thinking" || blockType == "redacted_thinking" {
				dropped = true
				continue
			}
			filtered = append(filtered, rawBlock)
		}
		if dropped {
			// `msg` is the map[string]any reference already held inside
			// rawMessages[i] — mutating msg["content"] in place is enough;
			// no need to re-assign the slice element. Same for the outer
			// `top["messages"]` (already pointing at rawMessages).
			msg["content"] = filtered
			mutated = true
		}
	}

	if !mutated {
		return body, nil
	}
	return json.Marshal(top)
}
