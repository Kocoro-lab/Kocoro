package context

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// maxSummaryFoldChunks bounds the small-tier calls one oversized-transcript
// summarization may spend. Workload: multi-day IM threads whose transcript
// exceeds summarizeInputCapChars several times over — a 6-chunk fold covers
// ~3.2M chars of transcript, i.e. weeks of chat. Symptom when it binds: the
// OLDEST chunks beyond the cap are elided with a marker instead of folded,
// so the running summary starts later in the conversation (recent fidelity
// is always preserved). Override: edit this constant.
const maxSummaryFoldChunks = 6

// rollingSummaryPrompt drives the sequential fold over transcript chunks
// (ported from openclaw's summarizeChunks: each chunk call receives the
// running summary of everything before it and produces the updated one).
const rollingSummaryPrompt = `You are compressing a very long conversation in sequential chunks. You receive the running summary of everything BEFORE this chunk, followed by the next chunk of the transcript. Update the running summary so it covers both.

Rules:
- Be factual and brief; keep the running summary under ~1500 words.
- Preserve literal identifiers exactly as seen (commit hashes, URLs, ports,
  issue/PR numbers, version tags, file paths) — never paraphrase or round them.
- Keep every user correction, decision, and preference change.
- Output ONLY the updated running summary text, no tags or commentary.`

// splitTranscriptChunks splits messages into consecutive groups whose
// serialized transcripts each stay near capChars. An assistant message
// carrying tool_use blocks and the user message carrying its tool_result
// form an atomic unit — a chunk boundary never separates them, so the
// summarizer always sees a call next to its result. A single unit larger
// than capChars gets its own chunk (the per-chunk transcript is still
// capped by capTranscriptForSummarize before any LLM call).
func splitTranscriptChunks(messages []client.Message, capChars int) [][]client.Message {
	var chunks [][]client.Message
	var current []client.Message
	currentLen := 0

	flush := func() {
		if len(current) > 0 {
			chunks = append(chunks, current)
			current = nil
			currentLen = 0
		}
	}

	msgLen := func(m client.Message) int {
		t := messageText(m)
		if t == "" {
			return 0
		}
		return len(t) + len(m.Role) + 6 // "[role]: " + "\n\n" framing
	}

	for i := 0; i < len(messages); i++ {
		m := messages[i]
		if m.Role == "system" {
			continue
		}
		unit := []client.Message{m}
		unitLen := msgLen(m)
		// Pair a tool_use with its tool_result, skipping any system messages
		// between them — buildTranscript drops system messages anyway, and a
		// system message must not break the atomic unit.
		if m.Role == "assistant" && hasToolUseBlock(m) {
			next := i + 1
			for next < len(messages) && messages[next].Role == "system" {
				next++
			}
			if next < len(messages) && hasToolResultBlock(messages[next]) {
				unit = append(unit, messages[next])
				unitLen += msgLen(messages[next])
				i = next
			}
		}
		if currentLen > 0 && currentLen+unitLen > capChars {
			flush()
		}
		current = append(current, unit...)
		currentLen += unitLen
		if unitLen > capChars {
			flush()
		}
	}
	flush()
	return chunks
}

func hasToolResultBlock(m client.Message) bool {
	if m.Role != "user" || !m.Content.HasBlocks() {
		return false
	}
	for _, b := range m.Content.Blocks() {
		if b.Type == "tool_result" {
			return true
		}
	}
	return false
}

// foldOversizedTranscript sequentially folds all but the last transcript
// chunk into a running summary and returns the user content for the final
// structured summarize call (running summary + last-chunk transcript).
// ok=false means the caller must fall back to the single-shot head+tail
// path — a failed or empty chunk call must degrade, never lose the whole
// summary. The returned usage covers the fold calls made either way.
func foldOversizedTranscript(ctx context.Context, c Completer, messages []client.Message) (string, client.Usage, bool) {
	var usage client.Usage
	chunks := splitTranscriptChunks(messages, summarizeInputCapChars)
	if len(chunks) <= 1 {
		return "", usage, false
	}

	rolling := ""
	if len(chunks) > maxSummaryFoldChunks {
		rolling = "[earliest part of the conversation elided for size]"
		chunks = chunks[len(chunks)-maxSummaryFoldChunks:]
	}
	// One line up front plus one per chunk: the fold is up to 6× the input
	// volume of the single-shot path and fires on exactly the biggest
	// sessions, so a slow fold must be attributable (and countable for the
	// gap #2 re-open telemetry) instead of looking like a hang. The fold
	// runs inside one PhaseAwaitingLLM window — on very large transcripts
	// the sequential calls can approach agent.idle_hard_timeout_secs; the
	// per-chunk lines below are the operator's signal to raise it.
	fmt.Fprintf(os.Stderr, "[context] transcript fold engaged: %d chunks\n", len(chunks))

	for i, ch := range chunks[:len(chunks)-1] {
		prior := rolling
		if prior == "" {
			prior = "(none yet — this is the first chunk)"
		}
		user := "[Running summary of the conversation so far]\n" + prior +
			"\n\n[Next chunk of the transcript]\n" + capTranscriptForSummarize(buildTranscript(ch))
		resp, err := c.Complete(ctx, client.CompletionRequest{
			Messages: []client.Message{
				{Role: "system", Content: client.NewTextContent(rollingSummaryPrompt)},
				{Role: "user", Content: client.NewTextContent(user)},
			},
			ModelTier:   "small",
			Temperature: 0.2,
			MaxTokens:   2000,
			CacheSource: "helper",
		})
		if err != nil {
			fmt.Fprintf(os.Stderr, "[context] transcript fold chunk %d/%d failed (%v), falling back to head+tail\n", i+1, len(chunks), err)
			return "", usage, false
		}
		usage = addUsage(usage, resp.Usage)
		rolling = strings.TrimSpace(resp.OutputText)
		if rolling == "" {
			fmt.Fprintf(os.Stderr, "[context] transcript fold chunk %d/%d returned empty, falling back to head+tail\n", i+1, len(chunks))
			return "", usage, false
		}
		fmt.Fprintf(os.Stderr, "[context] transcript fold chunk %d/%d done\n", i+1, len(chunks))
	}

	final := "[Running summary of the earlier part of this conversation]\n" + rolling +
		"\n\n[Transcript of the most recent part]\n" + capTranscriptForSummarize(buildTranscript(chunks[len(chunks)-1]))
	return final, usage, true
}
