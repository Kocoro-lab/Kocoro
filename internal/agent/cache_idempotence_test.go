package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestCompressOldToolResults_IdempotentBytesAfterFirstPass guards the
// Anthropic prompt-cache prefix invariant: once a tool_result has been
// visited by the tiered compactor, subsequent passes must NOT mutate the
// on-wire bytes for that block.
//
// Without the CompressedTier marker, stripToMetadata recomputes
// `[result: %d chars, snipped]` against the *current* (already shrunk)
// content on every pass, so origLen ratchets down and bytes drift for several
// iterations. That drift is the smoking-gun root cause documented in
// docs/issues/cache-message-prefix-invalidation.md.
func TestCompressOldToolResults_IdempotentBytesAfterFirstPass(t *testing.T) {
	// 25 tool_result messages so the earliest ones land at distFromEnd >= 20
	// (Tier 1 zone). Use ~500-char bodies so the mechanical Tier 2 truncation
	// path runs (no LLM completer needed).
	messages := []client.Message{
		{Role: "system", Content: client.NewTextContent("system")},
	}
	for i := 0; i < 25; i++ {
		body := strings.Repeat(fmt.Sprintf("entry-%02d ", i), 60)
		toolID := fmt.Sprintf("tc%02d", i)
		// assistant message must carry the tool_use so buildToolCallMap finds it
		messages = append(messages, client.Message{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "tool_use", ID: toolID, Name: "bash", Input: json.RawMessage(`{"cmd":"x"}`)},
			}),
		})
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock(toolID, body, false),
			}),
		})
	}

	// Pass 1 legitimately rewrites bytes (raw -> compressed). Snapshot AFTER
	// it as the reference; passes 2..N must equal it byte-for-byte.
	compressOldToolResults(context.Background(), messages, 8, 300, nil, "")
	want, err := json.Marshal(messages)
	if err != nil {
		t.Fatalf("marshal pass1: %v", err)
	}

	for pass := 2; pass <= 5; pass++ {
		compressOldToolResults(context.Background(), messages, 8, 300, nil, "")
		got, err := json.Marshal(messages)
		if err != nil {
			t.Fatalf("marshal pass%d: %v", pass, err)
		}
		if !bytes.Equal(want, got) {
			// Find first divergence to keep the failure message small.
			n := len(want)
			if len(got) < n {
				n = len(got)
			}
			diffAt := -1
			for i := 0; i < n; i++ {
				if want[i] != got[i] {
					diffAt = i
					break
				}
			}
			start := diffAt - 40
			if start < 0 {
				start = 0
			}
			endW := diffAt + 40
			if endW > len(want) {
				endW = len(want)
			}
			endG := diffAt + 40
			if endG > len(got) {
				endG = len(got)
			}
			t.Fatalf("compression not idempotent — pass %d bytes diverged from pass 1 at offset %d:\n  want: ...%s...\n  got:  ...%s...",
				pass, diffAt, want[start:endW], got[start:endG])
		}
	}
}

// TestCompressOldToolResults_Tier2BlockSurvivesTier1Zone verifies that a
// block already marked CompressedTier == 2 (e.g. micro-compacted in an
// earlier turn) is NOT re-stripped to Tier 1 metadata when its distFromEnd
// crosses the tier1 threshold. Per the chosen fix in
// docs/issues/cache-message-prefix-invalidation.md, the micro-compact
// summary is the terminal compressed form — Tier 1 must short-circuit on
// already-compressed blocks.
func TestCompressOldToolResults_Tier2BlockSurvivesTier1Zone(t *testing.T) {
	preCompressed := "[micro-compact] HTTP 200, returned 5 results from /api/items"
	frozenBlock := client.NewToolResultBlock("tc00", preCompressed, false)
	frozenBlock.CompressedTier = 2

	messages := []client.Message{
		{Role: "system", Content: client.NewTextContent("system")},
		// Frozen Tier 2 block with paired tool_use first.
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			{Type: "tool_use", ID: "tc00", Name: "http", Input: json.RawMessage(`{"u":"/api/items"}`)},
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{frozenBlock})},
	}
	// Pad with 24 more (assistant tool_use + user tool_result) pairs so the
	// frozen block sits at distFromEnd >= 20.
	for i := 1; i < 25; i++ {
		toolID := fmt.Sprintf("tc%02d", i)
		messages = append(messages, client.Message{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "tool_use", ID: toolID, Name: "bash", Input: json.RawMessage(`{"cmd":"x"}`)},
			}),
		})
		messages = append(messages, client.Message{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock(toolID, "small ok", false),
			}),
		})
	}

	compressOldToolResults(context.Background(), messages, 8, 300, nil, "")

	blocks := messages[2].Content.Blocks()
	if len(blocks) == 0 || blocks[0].Type != "tool_result" {
		t.Fatalf("unexpected message shape at index 2: %+v", messages[2])
	}
	got := client.ToolResultText(blocks[0])
	if got != preCompressed {
		t.Errorf("Tier 2 block was overwritten despite CompressedTier=2:\n  expected: %q\n  got:      %q", preCompressed, got)
	}
	if blocks[0].CompressedTier != 2 {
		t.Errorf("CompressedTier should remain 2, got %d", blocks[0].CompressedTier)
	}
}
