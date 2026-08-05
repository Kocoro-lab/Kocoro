package context

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// summarizeInputCapTokens is the transcript budget for one small-tier
// summarize call, expressed in TOKENS because that is the limit the API
// actually enforces.
//   - Workload: compaction of a tool-heavy session, where the transcript is
//     dominated by JSON tool results — the densest content this pipeline sees.
//   - Symptom when it binds: the transcript is head+tail elided (single-shot)
//     or split into more fold chunks, so the summary covers the middle less
//     precisely. Cheap compared to the alternative below.
//   - Override: raise together with the small tier's context window. The
//     budget must stay far enough under that window to also cover
//     summarizePrompt, the fold path's running summary, and MaxTokens.
//
// 150K against the small tier's 200K window leaves ~50K for all of that.
const summarizeInputCapTokens = 150_000

// conservativeBytesPerToken converts a byte length into a deliberately HIGH
// token estimate. Measured 2026-08-05 against the live small tier by diffing
// input_tokens across two requests differing only by a known blob:
//
//	json fixture   2.60 bytes/token   ← densest measured
//	chinese prose  2.71
//	go source      3.43
//	english prose  4.78
//
// 2.5 sits below every measured value, so the estimate never runs low.
//
// Do NOT substitute EstimateTokens here. Its charsPerToken=3.5 is tuned to be
// representative, not conservative, and on dense content it under-counts by
// ~25% — which is exactly how the 2026-08-05 production incident happened: a
// 540,000-byte cap was documented as "≈180K tokens" and billed 213,719 against
// a 200K window, so every compaction attempt 400'd and the session could never
// recover (it grew ~2K tokens per failed turn instead).
const conservativeBytesPerToken = 2.5

// summarizeInputCapBytes is the byte ceiling derived from the token budget.
// Slicing needs a byte offset, so the budget is converted once here rather
// than re-derived at each call site.
const summarizeInputCapBytes = int(float64(summarizeInputCapTokens) * conservativeBytesPerToken)

// SummarizeInputCapBytes exposes the ceiling so sibling layers and their tests
// size payloads against the real budget instead of mirroring a literal that
// silently goes stale when the budget moves (same rationale as
// CompactAbsoluteBufferTokens).
const SummarizeInputCapBytes = summarizeInputCapBytes

// estimateSummarizeTokens returns the conservative token estimate for s. Used
// for the safety gate and the drift warning, never for cost accounting.
func estimateSummarizeTokens(s string) int {
	return int(math.Ceil(float64(len(s)) / conservativeBytesPerToken))
}

// warnIfDenserThanSafetyFloor compares the bytes actually sent against the
// tokens actually billed. conservativeBytesPerToken is a floor derived from
// measurement, not a guarantee: a future model, tokenizer, or content type can
// sit below it, and the only symptom would be summarize calls silently 400ing
// again. One stderr line at the moment of drift is what turns that into a
// diagnosable event instead of another timing-correlation investigation.
func warnIfDenserThanSafetyFloor(sentBytes int, usage client.Usage) {
	billed := usage.InputTokens + usage.CacheReadTokens + usage.CacheCreationTokens
	if sentBytes <= 0 || billed <= 0 {
		return
	}
	ratio := float64(sentBytes) / float64(billed)
	if ratio >= conservativeBytesPerToken {
		return
	}
	fmt.Fprintf(os.Stderr,
		"[context] summarize input denser than the safety floor: bytes=%d billed_tokens=%d ratio=%.2f floor=%.2f — lower summarizeInputCapTokens\n",
		sentBytes, billed, ratio, conservativeBytesPerToken)
}

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
	// Gate in tokens (the limit the API enforces); slice in bytes.
	if estimateSummarizeTokens(s) <= summarizeInputCapTokens {
		return s
	}
	const marker = "\n\n[... transcript truncated for size — middle elided ...]\n\n"
	// Reserve full marker length, then split the remainder evenly between
	// head and tail. Worst-case output length = 2*half + len(marker) ≤ cap.
	// (Boundary adjustments below only shrink head/tail further, so the
	// inequality stays tight in the byte-aligned case and slack-by-up-to-3
	// in the multi-byte case — never crosses the cap.)
	half := (summarizeInputCapBytes - len(marker)) / 2

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
	// adjustment, total result length is ≤ summarizeInputCapBytes.
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
- Preserve literal identifiers exactly as seen (commit hashes, URLs, ports,
  issue/PR numbers, version tags, file paths) inside the relevant sections —
  never paraphrase or round them.
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
//
// The candidate summary is audited before acceptance (auditSummary): it must
// carry the labeled section structure and echo every at-risk identifier from
// the droppable middle of the history. A failing summary gets ONE retry with
// the failure reasons appended; identifiers still missing after that are
// appended mechanically (appendAutoPreservedIdentifiers), so compaction can
// no longer silently lose them (2026-08-04 e2e: a one-line junk summary was
// accepted unvalidated and the identifiers were unrecoverable). An empty
// extraction keeps the existing semantics — callers treat "" as a failure
// with their own backoff.
func GenerateSummary(ctx context.Context, c Completer, messages []client.Message) (string, client.Usage, error) {
	raw := buildTranscript(messages)
	transcript := capTranscriptForSummarize(raw)
	var foldUsage client.Usage
	// Oversized transcript: sequentially fold earlier chunks into a running
	// summary instead of silently dropping the middle (head+tail). A failed
	// fold keeps the head+tail transcript — degrade, never lose the summary.
	if estimateSummarizeTokens(raw) > summarizeInputCapTokens {
		if folded, fu, ok := foldOversizedTranscript(ctx, c, messages); ok {
			transcript = folded
			foldUsage = fu
		} else {
			foldUsage = fu // partial fold calls were still billed
		}
	}
	identifiers := identifiersAtRisk(messages)
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

	sentBytes := len(summarizePrompt) + len(transcript)
	resp, err := c.Complete(ctx, req)
	if err != nil {
		// The size we sent is the first thing anyone needs when this call is
		// the one that 400'd — and a rejected call reports no usage, so the
		// ratio check below can never fire on exactly the failure it guards.
		fmt.Fprintf(os.Stderr,
			"[context] summarize call failed: sent_bytes=%d est_tokens=%d budget=%d: %v\n",
			sentBytes, estimateSummarizeTokens(transcript), summarizeInputCapTokens, err)
		return "", foldUsage, fmt.Errorf("summarization failed: %w", err)
	}
	warnIfDenserThanSafetyFloor(sentBytes, resp.Usage)
	usage := addUsage(foldUsage, resp.Usage)
	summary := extractSummary(resp.OutputText)

	// The audit only makes sense at compaction scale: a history ShapeHistory
	// cannot shape (MinShapeable) legitimately summarizes as short prose —
	// the prompt's own escape hatch — and has no at-risk identifiers.
	if len(messages) <= MinShapeable() {
		return summary, usage, nil
	}
	reasons := auditSummary(summary, identifiers)
	if summary == "" || len(reasons) == 0 {
		return summary, usage, nil
	}

	// Retry only for STRUCTURE failures — the one defect the mechanical
	// backstop cannot fix. Identifier-only failures skip the paid retry:
	// at compaction scale the droppable middle almost always contains
	// identifier-shaped tokens, so retrying on them would bill an extra
	// small-tier pass at essentially every compaction for something
	// appendAutoPreservedIdentifiers guarantees for free.
	structural := false
	for _, r := range reasons {
		if r == "missing_sections" {
			structural = true
			break
		}
	}
	if structural {
		fmt.Fprintf(os.Stderr, "[context] summary failed quality audit (%s), retrying once\n",
			strings.Join(reasons, "; "))
		retryReq := req
		retryReq.Messages = append(append([]client.Message{}, req.Messages...),
			client.Message{Role: "assistant", Content: client.NewTextContent(resp.OutputText)},
			client.Message{Role: "user", Content: client.NewTextContent(buildSummaryRetryMessage(reasons, identifiers))},
		)
		retryResp, retryErr := c.Complete(ctx, retryReq)
		if retryErr == nil {
			usage = addUsage(usage, retryResp.Usage)
			// Re-audit: a retry can come back WORSE (e.g. lose the sections
			// the first candidate had); keep whichever candidate fails less.
			if retried := extractSummary(retryResp.OutputText); retried != "" &&
				len(auditSummary(retried, identifiers)) <= len(reasons) {
				summary = retried
			}
		}
	}

	return appendAutoPreservedIdentifiers(summary, identifiers), usage, nil
}

// userSummarizePrompt is the system prompt for both GET /sessions/{id}/summary
// (Kocoro daemon) and POST /sessions/{id}/share's auto-generated summary.
//
// Two intentional design choices that may look unusual but are load-bearing:
//
//  1. The language examples below each include an instruction PHRASED IN THE
//     TARGET LANGUAGE ("日本語で書いてください", "한국어로 작성하세요"). This
//     primes the model into the target language and is significantly more
//     reliable than "write the summary in Japanese". Do not normalize these
//     back to English when refactoring.
//
//  2. The 11-language list is a TEACHING DEVICE for the model, not a
//     supported-languages whitelist. Adding more examples is cheap (~5 tokens
//     each, prompt is cached) and improves reliability on low-resource
//     languages; removing examples Haiku may not have seen reduces its
//     confidence to use that language unprompted. Expand the list when a new
//     user-base language emerges; the trailing "every other language" clause
//     covers the unbounded case.
const userSummarizePrompt = `You are a conversation summarizer. Read the following conversation and produce a clear, well-structured Markdown summary for a human reader.

Requirements:
- Match the USER's primary language exactly. Determine this from the user's
  own messages (NOT from code, file paths, or English-only tool output that
  appears in tool_result blocks — those are incidental noise).
  - If the user writes in 中文 → write the summary in 中文.
  - If the user writes in 日本語 → 日本語で書いてください.
  - If the user writes in 한국어 → 한국어로 작성하세요.
  - If the user writes in English → write in English.
  - Same rule for Español / Français / Deutsch / Português / Tiếng Việt /
    العربية / Русский / and every other language.
  - NEVER translate the user's language into a different one, even if some
    tool outputs or referenced code happen to be in English.
- Use Markdown formatting with headers and bullet points.
- Focus on: what was discussed, key decisions made, work completed, and
  remaining action items.
- Be concise but comprehensive — a reader should understand the
  conversation's outcome without reading the full transcript.
- Do NOT include internal LLM terminology (tool_call, context window,
  tokens, etc.).
- Do NOT wrap the output in code fences — output raw Markdown directly.`

// SummarizeForUser 调用 LLM 生成面向人类阅读的会话摘要。Default cache_source
// "helper" routes the call through standard user-quota billing — appropriate
// for the on-demand GET /sessions/{id}/summary endpoint where the user is
// the direct consumer.
func SummarizeForUser(ctx context.Context, c Completer, messages []client.Message) (string, error) {
	return SummarizeForUserWithSource(ctx, c, messages, "helper")
}

// SummarizeForUserWithSource is the variant of SummarizeForUser that lets the
// caller pin a custom cache_source tag. The transcript / prompt / model tier
// remain identical to SummarizeForUser so the response shape is unchanged.
//
// Note: the share-page summary used to call this with cache_source="session_share".
// That path now lives in SummarizeForShareWithSource (short overview, ≤120
// chars). This function continues to back the interactive GET
// /sessions/{id}/summary endpoint where users want the full structured
// Markdown.
func SummarizeForUserWithSource(ctx context.Context, c Completer, messages []client.Message, cacheSource string) (string, error) {
	transcript := capTranscriptForSummarize(buildTranscript(messages))
	req := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent(userSummarizePrompt)},
			{Role: "user", Content: client.NewTextContent(transcript)},
		},
		ModelTier:   "small",
		Temperature: 0.2,
		MaxTokens:   2000,
		CacheSource: cacheSource,
	}

	resp, err := c.Complete(ctx, req)
	if err != nil {
		return "", fmt.Errorf("user summarization failed: %w", err)
	}

	return strings.TrimSpace(resp.OutputText), nil
}

// shareSummaryPrompt is a stricter variant of userSummarizePrompt for the
// share-page header. A share reader wants to know the topic at a glance, not
// read a structured Markdown report — so we explicitly cap to 2-3 sentences,
// ≤120 chars, plain text. MaxTokens=200 (in SummarizeForShareWithSource) is
// the hard ceiling that defends against a chatty model.
//
// The 11-language list mirrors userSummarizePrompt: it's a TEACHING DEVICE
// (priming Haiku to write in the user's language), not a whitelist. Keep both
// in sync if you add a language.
const shareSummaryPrompt = `You are a conversation summarizer producing a SHORT overview for a public share-page header. Read the following conversation and write a 2-3 sentence overview of what the user discussed and the conversation's outcome.

Requirements:
- Match the USER's primary language exactly. Determine this from the user's
  own messages (NOT from code, file paths, or English-only tool output that
  appears in tool_result blocks — those are incidental noise).
  - If the user writes in 中文 → write the summary in 中文.
  - If the user writes in 日本語 → 日本語で書いてください.
  - If the user writes in 한국어 → 한국어로 작성하세요.
  - If the user writes in English → write in English.
  - Same rule for Español / Français / Deutsch / Português / Tiếng Việt /
    العربية / Русский / and every other language.
  - NEVER translate the user's language into a different one, even if some
    tool outputs or referenced code happen to be in English.
- Output 2-3 sentences total, ≤120 characters. Skim-readable at a glance.
- Output PLAIN TEXT ONLY. Do NOT use Markdown headers (#), bullet points
  (- / *), numbered lists, code fences (~~~ or ` + "`" + ` ` + "`" + ` ` + "`" + `), bold (**), or italic (_).
- Focus on the topic and outcome. Do NOT include internal LLM terminology
  (tool_call, context window, tokens, etc.).
- Do NOT wrap the output in quotes or any other delimiter — output the
  sentences directly.`

// SummarizeForShareWithSource produces a short 2-3 sentence overview suitable
// for the share-page header (POST /sessions/{id}/share). Differences from
// SummarizeForUserWithSource:
//
//   - Uses shareSummaryPrompt — explicit 2-3 sentence / ≤120 char / plain-text
//     constraints instead of "concise but comprehensive" Markdown.
//   - MaxTokens=200 — hard ceiling so a chatty Haiku response can't dominate
//     the share page header even if it ignores the soft sentence budget.
//
// Same transcript construction + cap as SummarizeForUserWithSource (buildTranscript
// skips image content blocks, capTranscriptForSummarize bounds total input
// to 540K chars), so large sessions and base64-image-pasted text won't push
// Haiku past its 200K context window.
func SummarizeForShareWithSource(ctx context.Context, c Completer, messages []client.Message, cacheSource string) (string, error) {
	transcript := capTranscriptForSummarize(buildTranscript(messages))
	req := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "system", Content: client.NewTextContent(shareSummaryPrompt)},
			{Role: "user", Content: client.NewTextContent(transcript)},
		},
		ModelTier:   "small",
		Temperature: 0.2,
		MaxTokens:   200,
		CacheSource: cacheSource,
	}

	resp, err := c.Complete(ctx, req)
	if err != nil {
		return "", fmt.Errorf("share summarization failed: %w", err)
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
