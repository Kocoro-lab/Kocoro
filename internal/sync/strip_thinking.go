package sync

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
)

// stripThinkingFromSessionJSON returns a copy of the session JSON body with
// `thinking` and `redacted_thinking` content blocks removed from every
// assistant message's content array, and the derived compaction checkpoint
// dropped entirely. Other fields and message order are kept.
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

	// UseNumber, not a plain Unmarshal: decoding into map[string]any turns every
	// JSON number into float64, whose 53-bit mantissa silently truncates the
	// integers real payloads carry — unix nanosecond timestamps (19 digits),
	// 64-bit row IDs, byte sizes — inside persisted tool inputs. Same hazard and
	// same fix as normalizeToolInput (see
	// TestNormalizeToolInput_PreservesLargeIntegerPrecision). This matters more
	// now that dropping a compaction checkpoint re-marshals sessions that carry
	// no thinking blocks at all, which previously returned the original bytes
	// untouched.
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.UseNumber()
	var top map[string]any
	if err := dec.Decode(&top); err != nil {
		return body, err
	}
	// A streaming Decoder stops at the first complete value, so it would accept
	// a truncated-then-concatenated file and silently re-marshal only the head —
	// where json.Unmarshal used to reject it and leave the bytes alone. The
	// loader hands us raw os.ReadFile output with no upstream validation, so
	// restore the strictness explicitly (same shape as rejectDuplicateJSONMembers).
	if token, err := dec.Token(); err != io.EOF {
		if err != nil {
			return body, err
		}
		return body, fmt.Errorf("unexpected trailing JSON value beginning with %v", token)
	}

	// The compaction checkpoint is DROPPED rather than stripped. It is derived
	// state — a compacted view of messages that are already in this payload —
	// so uploading it roughly doubles a compacted session's bytes while adding
	// nothing the cloud resume path can use. That matters because the size gate
	// a few lines downstream (BuildBatches → SingleSessionMaxBytes) marks an
	// oversize session `size_limit_exceeded`, which backoff.go treats as
	// PERMANENT: a long session that used to sync would silently stop syncing
	// forever the first time it compacted. Dropping also removes a second copy
	// of the same content from the disclosure surface for free.
	mutated := false
	if _, ok := top["compaction_checkpoint"]; ok {
		delete(top, "compaction_checkpoint")
		mutated = true
	}

	var messageArrays [][]any
	if rawMessages, ok := top["messages"].([]any); ok {
		messageArrays = append(messageArrays, rawMessages)
	}

	for _, rawMessages := range messageArrays {
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
				msg["content"] = filtered
				mutated = true
			}
		}
	}

	if !mutated {
		return body, nil
	}
	return json.Marshal(top)
}
