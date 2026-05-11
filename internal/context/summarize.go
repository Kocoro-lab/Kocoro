package context

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// summarizeInputCapChars limits the transcript length sent to the small-tier
// summarizer. ~540K chars ≈ 180K tokens at the 3 chars/token estimate
// used by buildTranscript, leaving 20K headroom under Haiku 4.5's 200K context
// window. This is a defense-in-depth: the reactive path already runs
// compressOldToolResults before calling GenerateSummary, but that pass is
// deliberately gentle (keepRecent=8, maxResultChars=300) and can leave a
// transcript over the small-tier cap when recent tool results are large.
//
// Without this guard, a 200K+ transcript fed to the summarizer 400s with
// "prompt is too long", which is exactly the cascade that caused the
// 2026-05-07 production incident.
const summarizeInputCapChars = 540_000

// capTranscriptForSummarize returns s unchanged if it fits, or a head+tail
// concatenation otherwise. Truncation marker is human-readable so the
// summarizer can note the gap in its analysis.
//
// Boundaries are adjusted to UTF-8 rune starts so multi-byte content
// (Chinese, Japanese, emoji, …) is never truncated mid-rune. Without this,
// a byte-aligned slice can leave a partial UTF-8 sequence at either end;
// json.Marshal would then replace the dangling bytes with U+FFFD before
// the summarizer ever sees them, polluting the input. Verified with a 1.4M
// byte all-Chinese transcript producing 2 U+FFFD chars under the previous
// impl. (See 2026-05-08 review Finding 3.)
func capTranscriptForSummarize(s string) string {
	if len(s) <= summarizeInputCapChars {
		return s
	}
	const marker = "\n\n[... transcript truncated for size — middle elided ...]\n\n"
	// Reserve full marker length, then split the remainder evenly between
	// head and tail. Worst-case output length = 2*half + len(marker) ≤ cap.
	// (Boundary adjustments below only shrink head/tail further, so the
	// inequality stays tight in the byte-aligned case and slack-by-up-to-3
	// in the multi-byte case — never crosses the cap.)
	half := (summarizeInputCapChars - len(marker)) / 2

	// Adjust head boundary down to a rune start. utf8.RuneStart returns
	// true at byte offsets that begin a UTF-8 codepoint; since we truncate
	// the middle, walking *backwards* a few bytes at most cannot extend
	// the head past the configured cap.
	headEnd := half
	for headEnd > 0 && !utf8.RuneStart(s[headEnd]) {
		headEnd--
	}

	// Adjust tail boundary up to a rune start. Walking *forwards* keeps
	// the result strictly within `half` bytes; combined with the head
	// adjustment, total result length is ≤ summarizeInputCapChars.
	tailStart := len(s) - half
	for tailStart < len(s) && !utf8.RuneStart(s[tailStart]) {
		tailStart++
	}

	return s[:headEnd] + marker + s[tailStart:]
}

const summarizePrompt = `Compress the following conversation into a concise summary using a two-phase approach.

Phase 1 — Write a chronological analysis inside <analysis> tags:
- Walk through the conversation in order
- Note every user correction, decision, or preference change
- Track files read, modified, or created
- Record errors, blockers, and their resolutions
- Note which skills were activated via use_skill and any tool_search schema loads

Phase 2 — Write the final summary inside <summary> tags. The summary MUST contain these labeled sections in this order:

## Current task & next steps
What the user is working on and what the model was about to do when compacted.

## User corrections & decisions
Every correction, preference, or explicit decision the user made. Highest-priority content — never omit.

## Open files / important reads
Files the model has read this session and still needs awareness of. List one per line as "path — one-line purpose" (e.g. "internal/agent/loop.go — core agentic loop being modified"). Do NOT include file contents; only paths + purpose. Omit files that were only glanced at and are no longer relevant.

## Active skill policies
Skills activated via use_skill whose guidance still applies. One bullet per skill: "skill-name — one-line what-it-enforces" (e.g. "test-driven-development — write failing test before implementation"). Do NOT reproduce SKILL.md bodies.

## Loaded tool capabilities
Tools whose schemas were pulled in via tool_search this session. One comma-separated line (e.g. "Loaded: linear_search_issues, linear_create_issue, github_list_prs"). Omit this section entirely if tool_search was never called.

Rules:
- Be factual and brief. The goal is continuation, not exposition.
- If a section has no content, omit its header rather than writing "none" or "N/A".
- Do not add sections beyond the five above.
- If the conversation does not fit the five-section structure (e.g. very short,
  error-dominated, or tool_search-heavy), put a single plain-prose paragraph
  summarising the work so far inside <summary>…</summary> instead of the five
  labeled sections. Never return an empty response — an empty summary silently
  skips context compaction and wastes the analysis pass.

Format your response as:
<analysis>
[chronological walkthrough]
</analysis>
<summary>
[structured summary with the sections above]
</summary>`

// Completer is the interface for making LLM completion calls.
// Satisfied by *client.GatewayClient.
type Completer interface {
	Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error)
}

// buildTranscript 将消息序列化为文本 transcript，跳过 system 消息。
func buildTranscript(messages []client.Message) string {
	var sb strings.Builder
	for _, m := range messages {
		if m.Role == "system" {
			continue
		}
		if t := messageText(m); t != "" {
			fmt.Fprintf(&sb, "[%s]: %s\n\n", m.Role, t)
		}
	}
	return sb.String()
}

// GenerateSummary calls the LLM (small tier) to summarize a conversation.
// It strips the system message from the input to avoid wasting tokens.
// Serializes both plain text and block content (tool_use, tool_result).
func GenerateSummary(ctx context.Context, c Completer, messages []client.Message) (string, client.Usage, error) {
	transcript := capTranscriptForSummarize(buildTranscript(messages))
	req := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent(summarizePrompt)},
			{Role: "user", Content: client.NewTextContent(transcript)},
		},
		ModelTier:   "small",
		Temperature: 0.2,
		MaxTokens:   2000,
		CacheSource: "helper",
	}

	resp, err := c.Complete(ctx, req)
	if err != nil {
		return "", client.Usage{}, fmt.Errorf("summarization failed: %w", err)
	}

	return extractSummary(resp.OutputText), resp.Usage, nil
}

const userSummarizePrompt = `You are a conversation summarizer. Read the following conversation and produce a clear, well-structured Markdown summary for a human reader.

Requirements:
- Write in the SAME LANGUAGE as the conversation (if the conversation is in Chinese, write in Chinese; if in English, write in English, etc.)
- Use Markdown formatting with headers and bullet points
- Focus on: what was discussed, key decisions made, work completed, and remaining action items
- Be concise but comprehensive — a reader should understand the conversation's outcome without reading the full transcript
- Do NOT include internal LLM terminology (tool_call, context window, tokens, etc.)
- Do NOT wrap the output in code fences — output raw Markdown directly`

// SummarizeForUser 调用 LLM 生成面向人类阅读的会话摘要。
func SummarizeForUser(ctx context.Context, c Completer, messages []client.Message) (string, error) {
	transcript := capTranscriptForSummarize(buildTranscript(messages))
	req := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent(userSummarizePrompt)},
			{Role: "user", Content: client.NewTextContent(transcript)},
		},
		ModelTier:   "small",
		Temperature: 0.2,
		MaxTokens:   2000,
		CacheSource: "helper",
	}

	resp, err := c.Complete(ctx, req)
	if err != nil {
		return "", fmt.Errorf("user summarization failed: %w", err)
	}

	return strings.TrimSpace(resp.OutputText), nil
}

// extractSummary extracts the <summary> content from a two-phase response.
// If <summary> tags are present, returns their content.
// If missing, strips any <analysis> block and returns the remainder.
// Never returns raw <analysis> content — ShapeHistory injects the summary verbatim.
func extractSummary(raw string) string {
	raw = strings.TrimSpace(raw)

	// Try to extract <summary>...</summary>
	if _, after, found := strings.Cut(raw, "<summary>"); found {
		if content, _, ok := strings.Cut(after, "</summary>"); ok {
			return strings.TrimSpace(content)
		}
		// Opening tag but no closing — take everything after the tag
		return strings.TrimSpace(after)
	}

	// No <summary> tags — strip <analysis>...</analysis> and return remainder
	result := raw
	for {
		before, rest, found := strings.Cut(result, "<analysis>")
		if !found {
			break
		}
		_, afterClose, closed := strings.Cut(rest, "</analysis>")
		if !closed {
			// Opening tag but no closing — strip from <analysis> onward
			result = before
			break
		}
		result = before + afterClose
	}

	result = strings.TrimSpace(result)
	if result == "" {
		// Everything was analysis with no summary — return empty.
		// ShapeHistory handles empty summaries gracefully (sliding window only).
		// Returning raw here would leak <analysis> scratch work into context.
		return ""
	}
	return result
}

// messageText extracts readable text from a message, handling both plain text
// and block content (tool_use, tool_result, text blocks).
func messageText(m client.Message) string {
	// Plain text message
	if !m.Content.HasBlocks() {
		return m.Content.Text()
	}

	// Block content — serialize each block type
	var sb strings.Builder
	for _, b := range m.Content.Blocks() {
		if text := summarizeContentBlock(b); text != "" {
			sb.WriteString(text)
			sb.WriteString(" ")
		}
	}
	return strings.TrimSpace(sb.String())
}

func summarizeContentBlock(b client.ContentBlock) string {
	switch b.Type {
	case "text":
		return b.Text
	case "tool_use":
		return summarizeToolUse(b)
	case "tool_result":
		return summarizeToolResult(b)
	case "tool_reference":
		if b.ToolName != "" {
			return fmt.Sprintf("[tool_reference: %s]", b.ToolName)
		}
	}
	return ""
}

func summarizeToolUse(b client.ContentBlock) string {
	if b.Name == "" {
		return ""
	}
	args := compactToolInput(b.Input)
	if args == "" {
		return fmt.Sprintf("[tool_call: %s]", b.Name)
	}
	return fmt.Sprintf("[tool_call: %s %s]", b.Name, args)
}

func summarizeToolResult(b client.ContentBlock) string {
	// Truncate base text BEFORE appending refs so "Loaded tools: ..." survives
	// near-limit tool_result bodies. Refs carry the tool_search loaded-schema
	// names — surfacing them in the summary is the whole point of this helper,
	// so we keep them in full rather than a second-pass truncate that could
	// clip them.
	text := truncateSummaryText(strings.TrimSpace(client.ToolResultText(b)), 450)
	if refs := toolReferenceNames(b); len(refs) > 0 {
		refText := "Loaded tools: " + strings.Join(refs, ", ")
		if text == "" {
			text = refText
		} else {
			text += "\n" + refText
		}
	}
	if text == "" {
		return ""
	}
	return fmt.Sprintf("[tool_result: %s]", text)
}

func toolReferenceNames(b client.ContentBlock) []string {
	// Comma-ok assertion is safe when ToolContent is a nil interface or carries
	// any non-[]ContentBlock value (e.g. the string shape — see ToolResultText).
	// Returns (nil, false) without panicking in both cases.
	nested, ok := b.ToolContent.([]client.ContentBlock)
	if !ok {
		return nil
	}
	names := make([]string, 0, len(nested))
	for _, child := range nested {
		if child.Type == "tool_reference" && child.ToolName != "" {
			names = append(names, child.ToolName)
		}
	}
	return names
}

func compactToolInput(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" || trimmed == "{}" || trimmed == "[]" {
		return ""
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err == nil {
		return truncateSummaryText(buf.String(), 240)
	}
	return truncateSummaryText(trimmed, 240)
}

func truncateSummaryText(text string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(text)
	if len(r) <= maxRunes {
		return text
	}
	return string(r[:maxRunes]) + "..."
}
