package context

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/runstatus"
)

// SanitizeHistory repairs malformed message history that would cause API errors.
// Specifically handles:
//   - tool role messages with plain text (no tool_result blocks) → dropped
//   - assistant messages that are just "[tool_call: ...]" placeholders → dropped
//   - assistant messages with empty content (no blocks AND empty text, or
//     blocks where every text block is empty) → dropped, with any newly-
//     adjacent users content-merged so tool_use/tool_result pairing
//     survives (see docs/empty-assistant-content-400.md)
//   - consecutive assistant messages without intervening user → merged into one
//   - assistant error messages (friendly run-failure output) → dropped
//   - orphaned tool_use blocks (no matching tool_result follows) → stripped
//   - stale tool_use blocks with null/empty Input (from sessions persisted
//     before the issue #45 fix) → Input rewritten to "{}"
//
// Returns a new slice; the original is not modified.
func SanitizeHistory(messages []client.Message) []client.Message {
	if len(messages) == 0 {
		return messages
	}

	// First pass: drop invalid messages.
	var cleaned []client.Message
	for _, msg := range messages {
		if shouldDrop(msg) {
			continue
		}
		cleaned = append(cleaned, msg)
	}

	// Second pass: repair empty assistant content. Runs BEFORE
	// mergeConsecutiveRoles because the content-preserving merge done
	// here for users adjacent-after-drop preserves tool_result blocks
	// that mergeConsecutiveRoles' keep-later semantics would discard,
	// orphaning the prior assistant's tool_use into a 400.
	repaired := RepairEmptyAssistantContent(cleaned)

	// Third pass: fix consecutive same-role messages.
	// Claude API requires strict user/assistant alternation.
	merged := mergeConsecutiveRoles(repaired)

	// Fourth pass: strip orphaned tool_use and tool_result blocks.
	// Runs after role merging so adjacency checks are reliable.
	// Both Anthropic and OpenAI reject conversations where tool_use/tool_result
	// blocks lack their matching counterpart.
	stripped := stripOrphanedToolPairs(merged)

	// Fifth pass: normalize stale tool_use Input fields. Protects users
	// resuming sessions persisted before the issue #45 fix from replaying a
	// poisoned "input":null into the next Anthropic request.
	normalized := normalizeStaleToolUseInputs(stripped)

	// Final pass: stripping may create new consecutive same-role sequences
	// (e.g. dropping an empty assistant leaves two adjacent user messages).
	result := mergeConsecutiveRoles(normalized)

	return result
}

// RepairEmptyAssistantContent removes assistant messages whose content would
// serialize to an empty JSON value via MessageContent.MarshalJSON — i.e.
//
//   - bare empty text content (`MessageContent{text: "", blocks: nil}`)
//   - empty block list (`MessageContent{blocks: []}` or `nil`)
//   - block list whose only members are `{type:"text", text:""}`
//
// Empty assistant content reaches Cloud's `_convert_messages_to_claude_format`
// as `""` and is rewritten to `[{"type":"text","text":""}]`. Cloud's rolling
// `cache_control` marker on `claude_messages[-2]` then lands on that empty
// text block, triggering Anthropic's 400
// `cache_control cannot be set for empty text blocks`.
// See docs/empty-assistant-content-400.md for the full data flow.
//
// Mixed blocks (e.g. `thinking + empty text`) keep the non-text blocks and
// strip just the empty text — the thinking block carries useful trajectory
// state across resume and the cache_control failure mode is text-block
// specific.
//
// When dropping an empty assistant leaves two adjacent user messages, they
// are content-merged here (NOT in mergeConsecutiveRoles, whose keep-later
// semantics would discard the prior tool_result block and orphan the
// preceding assistant's tool_use). User adjacency that existed in the
// INPUT (e.g. three rapid user messages with no assistant between them)
// is left for mergeConsecutiveRoles to handle in its established way.
//
// Scope note (asymmetry vs empty USER messages): the same cache_control
// 400 also triggers if Cloud's marker lands on an empty USER text block,
// but the only production source of empty content observed to date is
// the assistant final-response path. Empty user messages are out of
// scope; if a future incident surfaces them we can either expand this
// repair pass or add a complementary user-side guard.
//
// Cache-debug instrumentation: when SHANNON_CACHE_DEBUG=1, drops, strips
// and merges each emit a client.LogCacheCompactEvent row so the analyst
// can attribute cache_hashes drift on the next request to this repair
// pass. Per CLAUDE.md "Prompt Cache" — all wire-shape mutators MUST log.
//
// Fast-path: when no assistant message qualifies for repair, the input
// slice is returned unchanged (no allocation). The pre-scan is O(N+B).
//
// Returns a new slice when any repair was applied, or the input slice
// untouched when no repair was needed. Idempotent.
func RepairEmptyAssistantContent(messages []client.Message) []client.Message {
	if len(messages) == 0 {
		return messages
	}
	if !needsEmptyAssistantRepair(messages) {
		return messages
	}
	out := make([]client.Message, 0, len(messages))
	justDroppedAssistant := false
	for i, msg := range messages {
		if msg.Role == "assistant" {
			repaired, drop := repairAssistantBlocks(msg)
			if drop {
				// Drop the empty assistant. Cache-debug attribution: both
				// the actual old content and any natural "newContent we
				// could choose here marshal to `""`, so the dedup inside
				// LogCacheCompactEvent would silently swallow this row.
				// Pass emptyAssistantDropMarker (a unique JSON shape that
				// never enters the wire — the message is gone) so the row
				// actually lands in cache-debug.log.
				client.LogCacheCompactEvent("empty_assistant_drop", i, msg.Content, emptyAssistantDropMarker)
				justDroppedAssistant = true
				continue
			}
			justDroppedAssistant = false
			if !sameContent(msg.Content, repaired.Content) {
				// Empty text block(s) stripped; non-empty blocks survived.
				client.LogCacheCompactEvent("empty_text_block_strip", i, msg.Content, repaired.Content)
			}
			out = append(out, repaired)
			continue
		}
		// Non-assistant message after a dropped assistant: if it's a user
		// and the previous accumulated message is also a user, content-merge.
		if justDroppedAssistant && msg.Role == "user" && len(out) > 0 && out[len(out)-1].Role == "user" {
			oldContent := out[len(out)-1].Content
			merged := mergeTwoUsersPreservingContent(out[len(out)-1], msg)
			out[len(out)-1] = merged
			// Log against the input index `i` of the message that triggered
			// the merge (the "second" user). new = the merged content that
			// now sits at the prior user's index in `out`. Together with the
			// empty_assistant_drop row at the dropped position, the analyst
			// has a complete pair: "msg[X] dropped, msg[X+1] merged into
			// msg[X-1]".
			client.LogCacheCompactEvent("repair_user_merge", i, oldContent, merged.Content)
			justDroppedAssistant = false
			continue
		}
		justDroppedAssistant = false
		out = append(out, msg)
	}
	return out
}

// emptyAssistantDropMarker is the placeholder newContent passed to
// LogCacheCompactEvent when an empty assistant is dropped. It exists ONLY
// so the dedup inside LogCacheCompactEvent (skip when old/new marshal to
// equal bytes) doesn't swallow the log row. It never enters the wire —
// the message at that index is gone. The unique sentinel type lets a
// cache-debug-log parser ignore these blocks if needed.
var emptyAssistantDropMarker = client.NewBlockContent([]client.ContentBlock{
	{Type: "_kocoro_repair_drop_marker"},
})

// needsEmptyAssistantRepair returns true iff at least one assistant message
// has content that would be touched by repairAssistantBlocks (bare empty
// text/blocks, or any block list containing an empty text block). When this
// returns false, RepairEmptyAssistantContent can return its input unchanged
// without allocating a new slice — the common case for clean histories.
func needsEmptyAssistantRepair(messages []client.Message) bool {
	for _, msg := range messages {
		if msg.Role != "assistant" {
			continue
		}
		if !msg.Content.HasBlocks() {
			if msg.Content.Text() == "" {
				return true
			}
			continue
		}
		for _, b := range msg.Content.Blocks() {
			if b.Type == "text" && b.Text == "" {
				return true
			}
		}
	}
	return false
}

// sameContent reports whether two MessageContent values serialize to byte-
// identical JSON. Used to decide whether a repair pass actually changed the
// content (and therefore whether to emit a cache-compact log row). Encoding
// equivalence beats reflective field comparison because MessageContent's
// MarshalJSON folds nil/empty block slices into the same `""` shape as bare
// empty text content — and that wire-form equivalence is what the cache
// debugger cares about.
func sameContent(a, b client.MessageContent) bool {
	ab, errA := json.Marshal(a)
	bb, errB := json.Marshal(b)
	if errA != nil || errB != nil {
		return false
	}
	return bytes.Equal(ab, bb)
}

// repairAssistantBlocks examines a single assistant message and returns
// either (msg, true) to drop, or (possibly-repaired msg, false) to keep.
//
// Rules:
//   - !HasBlocks() && Text() == "" → drop (bare empty)
//   - HasBlocks() with at least one empty text block:
//     strip the empty text blocks; if NO blocks remain → drop, else keep
//     the rebuilt message with the stripped blocks (thinking, tool_use,
//     non-empty text, etc. are preserved verbatim).
//   - All other shapes (non-empty text, tool_use, thinking-only, etc.) →
//     keep unchanged.
//
// Write/repair asymmetry on thinking-only: this differs from the write-
// site guard in loop.go (recoverVisibleTextFromBlocks +
// ErrEmptyFinalResponse), which refuses to persist a thinking-only final
// response because the user would receive no visible answer. Here, on
// the repair / resume path, a thinking-only assistant that survived a
// strip is kept — preserving the assistant's reasoning trail across
// resume is worth the small risk of an unusual mid-history block shape
// on the wire. The cache_control 400 mode is text-block-specific, so a
// thinking-only assistant in mid-history does not reintroduce that 400.
func repairAssistantBlocks(msg client.Message) (client.Message, bool) {
	if !msg.Content.HasBlocks() {
		if msg.Content.Text() == "" {
			return msg, true
		}
		return msg, false
	}

	src := msg.Content.Blocks()
	hasEmptyText := false
	for _, b := range src {
		if b.Type == "text" && b.Text == "" {
			hasEmptyText = true
			break
		}
	}
	if !hasEmptyText {
		return msg, false
	}

	kept := make([]client.ContentBlock, 0, len(src))
	for _, b := range src {
		if b.Type == "text" && b.Text == "" {
			continue
		}
		kept = append(kept, b)
	}
	if len(kept) == 0 {
		return msg, true
	}
	return client.Message{
		Role:       msg.Role,
		Content:    client.NewBlockContent(kept),
		Name:       msg.Name,
		ToolCallID: msg.ToolCallID,
	}, false
}

// mergeTwoUsersPreservingContent joins two adjacent user messages such that
// content from both is preserved. This is the content-preserving counterpart
// to mergeConsecutiveRoles (which uses keep-later). Used only inside
// RepairEmptyAssistantContent, where dropping an empty assistant can leave a
// user{tool_result} immediately followed by a user{text} — losing the
// tool_result via keep-later would orphan the prior assistant's tool_use.
//
// Block merging:
//   - text + text   → joined with "\n\n"
//   - blocks + text → blocks copied, then a {type:"text",text:bText} appended
//   - text + blocks → {type:"text",text:aText} prepended to b's blocks
//   - blocks + blocks → concatenated
//
// Empty text on one side is skipped so we never introduce empty text blocks
// or stray separators.
func mergeTwoUsersPreservingContent(a, b client.Message) client.Message {
	aHas := a.Content.HasBlocks()
	bHas := b.Content.HasBlocks()
	aText := a.Content.Text()
	bText := b.Content.Text()

	switch {
	case !aHas && !bHas:
		joined := aText
		if aText != "" && bText != "" {
			joined = aText + "\n\n" + bText
		} else if aText == "" {
			joined = bText
		}
		return client.Message{Role: "user", Content: client.NewTextContent(joined)}

	case aHas && !bHas:
		aBlocks := a.Content.Blocks()
		merged := make([]client.ContentBlock, 0, len(aBlocks)+1)
		merged = append(merged, aBlocks...)
		if bText != "" {
			merged = append(merged, client.ContentBlock{Type: "text", Text: bText})
		}
		return client.Message{Role: "user", Content: client.NewBlockContent(merged)}

	case !aHas && bHas:
		bBlocks := b.Content.Blocks()
		merged := make([]client.ContentBlock, 0, 1+len(bBlocks))
		if aText != "" {
			merged = append(merged, client.ContentBlock{Type: "text", Text: aText})
		}
		merged = append(merged, bBlocks...)
		return client.Message{Role: "user", Content: client.NewBlockContent(merged)}

	default:
		aBlocks := a.Content.Blocks()
		bBlocks := b.Content.Blocks()
		merged := make([]client.ContentBlock, 0, len(aBlocks)+len(bBlocks))
		merged = append(merged, aBlocks...)
		merged = append(merged, bBlocks...)
		return client.Message{Role: "user", Content: client.NewBlockContent(merged)}
	}
}

// normalizeStaleToolUseInputs rebuilds messages whose tool_use blocks carry
// null/empty Input fields, replacing them with "{}". Non-mutating: messages
// with no affected blocks are passed through unchanged, and messages that
// require edits get a fresh block slice rather than an in-place rewrite.
func normalizeStaleToolUseInputs(messages []client.Message) []client.Message {
	if len(messages) == 0 {
		return messages
	}
	out := make([]client.Message, len(messages))
	for i, msg := range messages {
		if !msg.Content.HasBlocks() {
			out[i] = msg
			continue
		}
		src := msg.Content.Blocks()
		var rebuilt []client.ContentBlock
		for j, b := range src {
			if b.Type != "tool_use" || !isEmptyOrNullToolInput(b.Input) {
				if rebuilt != nil {
					rebuilt = append(rebuilt, b)
				}
				continue
			}
			if rebuilt == nil {
				rebuilt = make([]client.ContentBlock, 0, len(src))
				rebuilt = append(rebuilt, src[:j]...)
			}
			fixed := b
			fixed.Input = json.RawMessage("{}")
			rebuilt = append(rebuilt, fixed)
		}
		if rebuilt == nil {
			out[i] = msg
		} else {
			out[i] = client.Message{
				Role:       msg.Role,
				Content:    client.NewBlockContent(rebuilt),
				Name:       msg.Name,
				ToolCallID: msg.ToolCallID,
			}
		}
	}
	return out
}

// isEmptyOrNullToolInput mirrors client.normalizeToolInput's detection logic:
// treats nil, empty bytes, pure whitespace, and the literal token "null" as
// invalid tool_use input that must be rewritten to "{}".
func isEmptyOrNullToolInput(raw json.RawMessage) bool {
	trimmed := bytes.TrimSpace(raw)
	return len(trimmed) == 0 || bytes.Equal(trimmed, []byte("null"))
}

// stripOrphanedToolPairs removes unpaired tool_use and tool_result blocks.
// A tool_use in assistant[i] is valid only if messages[i+1] is a user message
// containing a tool_result with the same ID, and vice versa. Pairing is
// per-position: the same ID reused in a non-adjacent pair does not count.
// If stripping leaves a message with no content, it is dropped.
func stripOrphanedToolPairs(messages []client.Message) []client.Message {
	// Per-message set of valid tool IDs. An ID is valid at position i only
	// if it forms a proper adjacent pair (assistant[i] ↔ user[i+1]).
	validAt := make([]map[string]bool, len(messages))

	for i := 0; i+1 < len(messages); i++ {
		if messages[i].Role != "assistant" || !messages[i].Content.HasBlocks() {
			continue
		}
		next := messages[i+1]
		if next.Role != "user" || !next.Content.HasBlocks() {
			continue
		}

		useIDs := make(map[string]bool)
		for _, b := range messages[i].Content.Blocks() {
			if b.Type == "tool_use" && b.ID != "" {
				useIDs[b.ID] = true
			}
		}

		for _, b := range next.Content.Blocks() {
			if b.Type == "tool_result" && b.ToolUseID != "" && useIDs[b.ToolUseID] {
				if validAt[i] == nil {
					validAt[i] = make(map[string]bool)
				}
				if validAt[i+1] == nil {
					validAt[i+1] = make(map[string]bool)
				}
				validAt[i][b.ToolUseID] = true
				validAt[i+1][b.ToolUseID] = true
			}
		}
	}

	var out []client.Message
	for i, msg := range messages {
		if !msg.Content.HasBlocks() {
			out = append(out, msg)
			continue
		}

		switch msg.Role {
		case "assistant":
			kept := stripUnpairedBlocks(msg.Content.Blocks(), "tool_use", validAt[i])
			if kept == nil {
				continue
			}
			out = append(out, client.Message{Role: msg.Role, Content: client.NewBlockContent(kept)})

		case "user":
			kept := stripUnpairedBlocks(msg.Content.Blocks(), "tool_result", validAt[i])
			if kept == nil {
				continue
			}
			out = append(out, client.Message{Role: msg.Role, Content: client.NewBlockContent(kept)})

		default:
			out = append(out, msg)
		}
	}
	return out
}

// stripUnpairedBlocks removes blocks of blockType whose ID is not in validIDs
// for this position. For tool_use, checks block.ID; for tool_result, checks
// block.ToolUseID. Returns nil if no blocks remain.
func stripUnpairedBlocks(blocks []client.ContentBlock, blockType string, validIDs map[string]bool) []client.ContentBlock {
	hasOrphan := false
	for _, b := range blocks {
		if b.Type != blockType {
			continue
		}
		id := toolBlockID(b)
		if id != "" && !validIDs[id] {
			hasOrphan = true
			break
		}
	}
	if !hasOrphan {
		return blocks
	}

	var kept []client.ContentBlock
	for _, b := range blocks {
		if b.Type == blockType {
			id := toolBlockID(b)
			if id != "" && !validIDs[id] {
				continue
			}
		}
		kept = append(kept, b)
	}
	if len(kept) == 0 {
		return nil
	}
	return kept
}

// toolBlockID returns the tool pairing ID for a block: ID for tool_use,
// ToolUseID for tool_result.
func toolBlockID(b client.ContentBlock) string {
	switch b.Type {
	case "tool_use":
		return b.ID
	case "tool_result":
		return b.ToolUseID
	}
	return ""
}

// mergeConsecutiveRoles collapses consecutive same-role messages, keeping the later one.
func mergeConsecutiveRoles(messages []client.Message) []client.Message {
	var out []client.Message
	for i, msg := range messages {
		if i > 0 && msg.Role == messages[i-1].Role {
			switch msg.Role {
			case "assistant", "user":
				out[len(out)-1] = msg
				continue
			}
		}
		out = append(out, msg)
	}
	return out
}

// shouldDrop returns true for messages that are malformed or would cause API errors.
func shouldDrop(msg client.Message) bool {
	text := msg.Content.Text()

	switch msg.Role {
	case "tool":
		// Legacy tool-role messages are from old heartbeat persistence.
		// The current protocol uses user-role messages with tool_result blocks.
		// Drop all tool-role messages unconditionally — they are not recognized
		// by the pairing pass and would be rejected by the API.
		return true

	case "assistant":
		// Drop placeholder tool call text (from old heartbeat bug).
		if strings.HasPrefix(text, "[tool_call:") {
			return true
		}
		// Drop error marker from old heartbeat failures.
		if text == "[error: agent failed to respond]" {
			return true
		}
		// Drop persisted friendly run-failure messages — they contain no useful
		// context and just waste tokens.
		if isFriendlyError(text) {
			return true
		}
	}

	return false
}

// isFriendlyError returns true for friendly run-failure messages.
func isFriendlyError(text string) bool {
	return runstatus.IsFriendlyMessage(text)
}
