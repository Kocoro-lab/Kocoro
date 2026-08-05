package context

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func chunkTranscriptLen(chunk []client.Message) int {
	return len(buildTranscript(chunk))
}

// promptAwareCompleter answers rolling-fold calls and the final structured
// call differently, keyed on the system prompt — robust to the exact chunk
// count instead of a positional output script.
type promptAwareCompleter struct {
	rollingOut string
	finalOut   string
	reqs       []client.CompletionRequest
}

func (p *promptAwareCompleter) Complete(_ context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	p.reqs = append(p.reqs, req)
	out := p.finalOut
	if req.Messages[0].Content.Text() == rollingSummaryPrompt {
		out = p.rollingOut
	}
	return &client.CompletionResponse{
		OutputText: out,
		Usage:      client.Usage{InputTokens: 100, OutputTokens: 50, TotalTokens: 150, CostUSD: 0.001},
	}, nil
}

func TestSplitTranscriptChunks_RespectsCap(t *testing.T) {
	var messages []client.Message
	body := strings.Repeat("filler content for the chunk splitter ", 50) // ~1.9K chars
	for i := 0; i < 40; i++ {
		messages = append(messages,
			client.Message{Role: "user", Content: client.NewTextContent(fmt.Sprintf("q%d %s", i, body))},
			client.Message{Role: "assistant", Content: client.NewTextContent(fmt.Sprintf("a%d %s", i, body))},
		)
	}

	const cap = 20_000
	chunks := splitTranscriptChunks(messages, cap)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	total := 0
	for i, ch := range chunks {
		if l := chunkTranscriptLen(ch); l > cap {
			t.Errorf("chunk %d transcript %d chars exceeds cap %d", i, l, cap)
		}
		total += len(ch)
	}
	if total != len(messages) {
		t.Errorf("chunks must cover all messages: got %d, want %d", total, len(messages))
	}
}

func TestSplitTranscriptChunks_NeverStartsChunkAtToolResult(t *testing.T) {
	big := strings.Repeat("x", 3_000)
	var messages []client.Message
	for i := 0; i < 20; i++ {
		id := fmt.Sprintf("tu-%d", i)
		messages = append(messages,
			client.Message{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolUseBlock(id, "bash", json.RawMessage(`{"cmd":"ls"}`)),
			})},
			client.Message{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock(id, big, false),
			})},
		)
	}

	chunks := splitTranscriptChunks(messages, 5_000)
	if len(chunks) < 2 {
		t.Fatalf("expected multiple chunks, got %d", len(chunks))
	}
	for i, ch := range chunks[1:] {
		first := ch[0]
		if first.Role == "user" && first.Content.HasBlocks() {
			for _, b := range first.Content.Blocks() {
				if b.Type == "tool_result" {
					t.Errorf("chunk %d starts at a tool_result — split separated it from its tool_use", i+1)
				}
			}
		}
	}
}

func TestSplitTranscriptChunks_OversizedMessageGetsOwnChunk(t *testing.T) {
	messages := []client.Message{
		{Role: "user", Content: client.NewTextContent("small")},
		{Role: "assistant", Content: client.NewTextContent(strings.Repeat("huge ", 5_000))}, // 25K, over cap
		{Role: "user", Content: client.NewTextContent("after")},
	}
	chunks := splitTranscriptChunks(messages, 10_000)
	total := 0
	for _, ch := range chunks {
		total += len(ch)
	}
	if total != 3 {
		t.Fatalf("all messages must be covered, got %d", total)
	}
}

// buildOversizedHistory produces a history whose transcript exceeds
// summarizeInputCapChars, with a distinctive identifier buried in the FIRST
// (earliest) portion — exactly what head+tail truncation loses today.
func buildOversizedHistory() []client.Message {
	filler := strings.Repeat("long conversation filler content for the summarizer ", 400) // ~21K chars
	var messages []client.Message
	messages = append(messages, client.Message{Role: "user", Content: client.NewTextContent(
		"early decision: deploy commit deadbeefcafe1234 to staging")})
	for i := 0; i < 30; i++ {
		messages = append(messages,
			client.Message{Role: "user", Content: client.NewTextContent(fmt.Sprintf("step %d %s", i, filler))},
			client.Message{Role: "assistant", Content: client.NewTextContent(fmt.Sprintf("done %d %s", i, filler))},
		)
	}
	return messages
}

func TestGenerateSummary_ChunkedFoldCoversEarlyContent(t *testing.T) {
	messages := buildOversizedHistory()
	if l := len(buildTranscript(messages)); l <= summarizeInputCapChars {
		t.Fatalf("test workload too small: transcript %d chars must exceed cap %d", l, summarizeInputCapChars)
	}

	c := &promptAwareCompleter{
		rollingOut: "rolling summary carrying deadbeefcafe1234 from the early chunk",
		finalOut: `<summary>## Current task & next steps
Continue the steps; early decision preserved: deadbeefcafe1234.</summary>`,
	}

	summary, _, err := GenerateSummary(context.Background(), c, messages)
	if err != nil {
		t.Fatalf("GenerateSummary: %v", err)
	}
	if len(c.reqs) < 2 {
		t.Fatalf("expected rolling chunk calls plus the final structured call, got %d calls", len(c.reqs))
	}

	// Every rolling call must fit the small-tier input cap.
	for i, r := range c.reqs {
		for _, m := range r.Messages {
			if l := len(m.Content.Text()); l > summarizeInputCapChars {
				t.Errorf("call %d message exceeds input cap: %d chars", i, l)
			}
		}
	}

	// The FINAL call must be the structured two-phase prompt and must carry
	// the rolling summary of the earlier chunks.
	last := c.reqs[len(c.reqs)-1]
	if last.Messages[0].Content.Text() != summarizePrompt {
		t.Error("final call must use the structured two-phase summarize prompt")
	}
	finalUser := last.Messages[1].Content.Text()
	if !strings.Contains(finalUser, "deadbeefcafe1234") {
		t.Error("final call lost the early-chunk content (rolling summary not threaded through)")
	}
	if !strings.Contains(summary, "deadbeefcafe1234") {
		t.Errorf("summary lost the early identifier:\n%s", summary)
	}
}

// failNthCompleter fails call n (1-based) and otherwise delegates to scripted
// outputs.
type failNthCompleter struct {
	scripted scriptedCompleter
	failCall int
	calls    int
}

func (f *failNthCompleter) Complete(ctx context.Context, req client.CompletionRequest) (*client.CompletionResponse, error) {
	f.calls++
	if f.calls == f.failCall {
		return nil, errors.New("chunk call failed")
	}
	return f.scripted.Complete(ctx, req)
}

func TestGenerateSummary_ChunkFailureFallsBackToHeadTail(t *testing.T) {
	messages := buildOversizedHistory()
	final := `<summary>## Current task & next steps
Continue the steps.</summary>`
	c := &failNthCompleter{
		scripted: scriptedCompleter{outputs: []string{final}},
		failCall: 1, // first rolling chunk call fails
	}

	summary, _, err := GenerateSummary(context.Background(), c, messages)
	if err != nil {
		t.Fatalf("a failed chunk call must degrade, not fail the summary: %v", err)
	}
	if summary == "" {
		t.Fatal("expected a summary from the head+tail fallback")
	}
	// The fallback final call carries the truncation marker of the old
	// single-shot path.
	last := c.scripted.reqs[len(c.scripted.reqs)-1]
	if !strings.Contains(last.Messages[1].Content.Text(), "transcript truncated for size") {
		t.Error("fallback must summarize the head+tail-capped full transcript")
	}
}

func TestGenerateSummary_SmallTranscriptStaysSingleCall(t *testing.T) {
	c := &scriptedCompleter{outputs: []string{"<summary>short prose</summary>"}}
	messages := []client.Message{
		{Role: "user", Content: client.NewTextContent("small request")},
		{Role: "assistant", Content: client.NewTextContent("small reply")},
	}
	if _, _, err := GenerateSummary(context.Background(), c, messages); err != nil {
		t.Fatalf("GenerateSummary: %v", err)
	}
	if len(c.reqs) != 1 {
		t.Errorf("small transcript must stay a single summarize call, got %d", len(c.reqs))
	}
}
