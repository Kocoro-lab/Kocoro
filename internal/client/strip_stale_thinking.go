package client

// stripStaleThinkingBlocks returns a transformed copy of messages where every
// assistant message except the most recent one has its `thinking` and
// `redacted_thinking` content blocks removed.
//
// Why: Cloud's _convert_messages_to_claude_format places its rolling
// cache_control marker on claude_messages[-2] (per docs/cache-strategy.md
// line 91). Anthropic's API rejects requests where that marker lands on a
// thinking block ("messages.N.content.0.thinking.cache_control: Extra
// inputs are not permitted"). Assistant turns inevitably start with a
// `thinking` block when extended thinking is on, so even a 4-message
// conversation routinely trips the constraint.
//
// Preserving the most recent assistant's thinking satisfies Anthropic's
// "immediately preceding assistant turn must include thinking + signature"
// rule for tool-use loops, so the active reasoning thread stays valid.
// Older assistant turns lose their thinking continuity — that is the
// trade Anthropic's wire contract forces on us until Cloud's marker
// placement is fixed.
//
// Returns the same slice unmodified when no strip is needed. When at
// least one strip happens, a fresh slice + freshly-constructed
// MessageContent values are returned so callers' state isn't aliased.
//
// Safe for concurrent use on the same input slice; never mutates input.
func stripStaleThinkingBlocks(messages []Message) []Message {
	if len(messages) == 0 {
		return messages
	}
	// Find the index of the most recent assistant message; that one keeps
	// its thinking blocks. Anything earlier is fair game.
	lastAssistantIdx := -1
	for i := len(messages) - 1; i >= 0; i-- {
		if messages[i].Role == "assistant" {
			lastAssistantIdx = i
			break
		}
	}
	if lastAssistantIdx <= 0 {
		// No older assistant turns to strip — either no assistant at all,
		// or the first message in the list IS the last assistant.
		return messages
	}

	var out []Message
	stripped := false
	for i, msg := range messages {
		if i >= lastAssistantIdx || msg.Role != "assistant" {
			if out != nil {
				out[i] = msg
			}
			continue
		}
		if !msg.Content.HasBlocks() {
			if out != nil {
				out[i] = msg
			}
			continue
		}
		blocks := msg.Content.Blocks()
		hasThinking := false
		for _, b := range blocks {
			if b.Type == "thinking" || b.Type == "redacted_thinking" {
				hasThinking = true
				break
			}
		}
		if !hasThinking {
			if out != nil {
				out[i] = msg
			}
			continue
		}
		// First time we actually need to mutate: lazily clone the slice.
		if out == nil {
			out = make([]Message, len(messages))
			copy(out, messages)
		}
		filtered := make([]ContentBlock, 0, len(blocks))
		for _, b := range blocks {
			if b.Type == "thinking" || b.Type == "redacted_thinking" {
				continue
			}
			filtered = append(filtered, b)
		}
		// If every block was a thinking variant, replace with a single
		// empty-text block so the assistant turn remains structurally
		// well-formed (Anthropic accepts an empty text block in an
		// assistant turn; an entirely empty content array is rejected).
		if len(filtered) == 0 {
			filtered = []ContentBlock{{Type: "text", Text: ""}}
		}
		out[i] = msg
		out[i].Content = NewBlockContent(filtered)
		stripped = true
	}
	if !stripped {
		return messages
	}
	return out
}
