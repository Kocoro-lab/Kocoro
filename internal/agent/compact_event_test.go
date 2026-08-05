package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestCompressOldToolResults_EmitsCompactEvents verifies the wiring between
// the agent-side mutation sites and client.LogCacheCompactEvent. Without
// these events, the analyst sees `msg_hashes[k]` flips in the next req log
// line with no explanation of which compaction path caused them.
//
// Fixture: 25 tool_result messages so the earliest land at distFromEnd >= 20
// (Tier 1 strip-to-metadata zone). Pass 1 should emit `tier1` events for the
// stripped messages and `tier2` for those landing in the head+tail truncation
// zone.
func TestCompressOldToolResults_EmitsCompactEvents(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("SHANNON_CACHE_DEBUG", "1")

	messages := []client.Message{
		{Role: "system", Content: client.NewTextContent("system")},
	}
	for i := range 25 {
		body := strings.Repeat(fmt.Sprintf("entry-%02d ", i), 60)
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
				client.NewToolResultBlock(toolID, body, false),
			}),
		})
	}

	compressOldToolResults(context.Background(), messages, 8, 300, nil, "")

	data, err := os.ReadFile(filepath.Join(tmp, ".shannon", "logs", "cache-debug.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}

	type event struct {
		Dir     string `json:"dir"`
		Action  string `json:"action"`
		MsgIdx  int    `json:"msg_idx"`
		OldHash string `json:"old_hash"`
		NewHash string `json:"new_hash"`
	}
	var events []event
	for line := range strings.SplitSeq(string(data), "\n") {
		if line == "" {
			continue
		}
		var ev event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		if ev.Dir == "compact" {
			events = append(events, ev)
		}
	}

	if len(events) == 0 {
		t.Fatalf("expected at least one compact event, got none.\nlog:\n%s", data)
	}

	gotTier1, gotTier2 := false, false
	for _, ev := range events {
		switch ev.Action {
		case "tier1":
			gotTier1 = true
		case "tier2":
			gotTier2 = true
		}
		if ev.OldHash == ev.NewHash {
			t.Errorf("compact event with unchanged bytes leaked: %+v", ev)
		}
	}
	if !gotTier1 {
		t.Errorf("expected at least one tier1 event in 25-result fixture: %+v", events)
	}
	if !gotTier2 {
		t.Errorf("expected at least one tier2 event in 25-result fixture: %+v", events)
	}
}
