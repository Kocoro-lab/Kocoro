package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func budgetToolPair(id, name, result string) []client.Message {
	return []client.Message{
		{
			Role: "assistant",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolUseBlock(id, name, nil),
			}),
		},
		{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock(id, result, false),
			}),
		},
	}
}

func toolResultTextAt(t *testing.T, msgs []client.Message, msgIdx int) string {
	t.Helper()
	blocks := msgs[msgIdx].Content.Blocks()
	if len(blocks) != 1 || blocks[0].Type != "tool_result" {
		t.Fatalf("message %d does not contain one tool_result block: %#v", msgIdx, blocks)
	}
	return client.ToolResultText(blocks[0])
}

func TestApplyToolResultBudget_ReplacesLargestFreshResult(t *testing.T) {
	dir := t.TempDir()
	msgs := append([]client.Message{{Role: "system", Content: client.NewTextContent("sys")}},
		budgetToolPair("toolu_small", "bash", strings.Repeat("s", 60))...)
	msgs = append(msgs, budgetToolPair("toolu_big", "grep", strings.Repeat("b", 220))...)

	state := NewToolResultReplacementState(nil)
	out, changed := applyToolResultBudget(msgs, state, ToolResultBudgetOptions{
		ShannonDir:        dir,
		SessionID:         "sess",
		AggregateCapChars: 200,
		MinCandidateChars: 50,
		PreviewChars:      20,
	})

	if !changed {
		t.Fatal("expected budget application to change messages")
	}
	big := toolResultTextAt(t, out, 4)
	if !strings.Contains(big, "[Tool result omitted from context:") {
		t.Fatalf("big result was not replaced: %q", big)
	}
	if !strings.Contains(big, "Preview (first 20 chars):") {
		t.Fatalf("replacement did not include preview: %q", big)
	}
	if strings.Contains(big, strings.Repeat("b", 60)) {
		t.Fatalf("replacement leaked too much original content")
	}
	if got := toolResultTextAt(t, msgs, 4); got != strings.Repeat("b", 220) {
		t.Fatalf("input transcript mutated: got %q", got)
	}
	if len(state.Replacements) != 1 {
		t.Fatalf("replacement state count = %d, want 1", len(state.Replacements))
	}
	path := filepath.Join(dir, "tmp", "tool_result_sess_toolu_big.txt")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected deterministic spill file %s: %v", path, err)
	}
}

func TestApplyToolResultBudget_SeenUnreplacedIDIsFrozen(t *testing.T) {
	msgs := append([]client.Message{{Role: "system", Content: client.NewTextContent("sys")}},
		budgetToolPair("toolu_seen", "bash", strings.Repeat("x", 300))...)
	state := NewToolResultReplacementState(nil)
	state.Seen["toolu_seen"] = true

	out, changed := applyToolResultBudget(msgs, state, ToolResultBudgetOptions{
		AggregateCapChars: 100,
		MinCandidateChars: 50,
		PreviewChars:      20,
	})

	if changed {
		t.Fatal("seen-but-unreplaced tool_result must stay byte-identical")
	}
	if got := toolResultTextAt(t, out, 2); got != strings.Repeat("x", 300) {
		t.Fatalf("seen content changed: %q", got)
	}
	if len(state.Replacements) != 0 {
		t.Fatalf("seen frozen result was replaced: %#v", state.Replacements)
	}
}

func TestApplyToolResultBudget_MarksAllFreshIDsSeen(t *testing.T) {
	msgs := append([]client.Message{{Role: "system", Content: client.NewTextContent("sys")}},
		budgetToolPair("toolu_small", "bash", "short output")...)
	state := NewToolResultReplacementState(nil)

	_, changed := applyToolResultBudget(msgs, state, ToolResultBudgetOptions{
		AggregateCapChars: 1000,
		MinCandidateChars: 50,
		PreviewChars:      20,
	})

	if changed {
		t.Fatal("small message should not be rewritten")
	}
	if !state.Seen["toolu_small"] {
		t.Fatalf("fresh unreplaced ID was not marked seen: %#v", state.Seen)
	}
}

func TestApplyToolResultBudget_ReappliesStoredReplacementByteIdentically(t *testing.T) {
	dir := t.TempDir()
	msgs := append([]client.Message{{Role: "system", Content: client.NewTextContent("sys")}},
		budgetToolPair("toolu_big", "bash", strings.Repeat("x", 220))...)

	state := NewToolResultReplacementState(map[string]string{
		"toolu_big": "[Tool result omitted from context: existing]\n\nPreview (first 3 chars):\nxxx",
	})
	out, changed := applyToolResultBudget(msgs, state, ToolResultBudgetOptions{
		ShannonDir:        dir,
		SessionID:         "sess",
		AggregateCapChars: 1000,
		MinCandidateChars: 50,
		PreviewChars:      20,
	})

	if !changed {
		t.Fatal("expected stored replacement to be reapplied even under cap")
	}
	got := toolResultTextAt(t, out, 2)
	want := "[Tool result omitted from context: existing]\n\nPreview (first 3 chars):\nxxx"
	if got != want {
		t.Fatalf("replacement drifted:\n got %q\nwant %q", got, want)
	}
}

func TestApplyToolResultBudget_ReappliesStoredReplacementRehydratesSpillFile(t *testing.T) {
	dir := t.TempDir()
	content := strings.Repeat("x", 220)
	path := filepath.Join(dir, "tmp", "tool_result_sess_toolu_big.txt")
	replacement := "[Tool result omitted from context: " + path + " (220 chars)]\n\nPreview (first 20 chars):\n" + strings.Repeat("x", 20)
	msgs := append([]client.Message{{Role: "system", Content: client.NewTextContent("sys")}},
		budgetToolPair("toolu_big", "bash", content)...)
	state := NewToolResultReplacementState(map[string]string{"toolu_big": replacement})

	out, changed := applyToolResultBudget(msgs, state, ToolResultBudgetOptions{
		ShannonDir:        dir,
		SessionID:         "sess",
		AggregateCapChars: 1000,
		MinCandidateChars: 50,
		PreviewChars:      20,
	})

	if !changed {
		t.Fatal("expected stored replacement to be reapplied")
	}
	if got := toolResultTextAt(t, out, 2); got != replacement {
		t.Fatalf("replacement drifted:\n got %q\nwant %q", got, replacement)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("expected rehydrated spill file %s: %v", path, err)
	}
	if string(data) != content {
		t.Fatalf("rehydrated spill content mismatch")
	}
}

func TestApplyToolResultBudget_SkipsUnsafeToolClasses(t *testing.T) {
	dir := t.TempDir()
	msgs := append([]client.Message{{Role: "system", Content: client.NewTextContent("sys")}},
		budgetToolPair("toolu_think", "think", strings.Repeat("t", 300))...)
	msgs = append(msgs, budgetToolPair("toolu_browser", "browser_navigate", strings.Repeat("b", 300))...)

	state := NewToolResultReplacementState(nil)
	out, changed := applyToolResultBudget(msgs, state, ToolResultBudgetOptions{
		ShannonDir:        dir,
		SessionID:         "sess",
		AggregateCapChars: 100,
		MinCandidateChars: 50,
		PreviewChars:      20,
	})

	if changed {
		t.Fatal("unsafe tools should not be replaced")
	}
	if len(state.Replacements) != 0 {
		t.Fatalf("unexpected replacements: %#v", state.Replacements)
	}
	if got := toolResultTextAt(t, out, 2); got != strings.Repeat("t", 300) {
		t.Fatalf("think result changed: %q", got)
	}
}

func TestApplyToolResultBudget_SkipsUnlimitedToolPolicy(t *testing.T) {
	msgs := append([]client.Message{{Role: "system", Content: client.NewTextContent("sys")}},
		budgetToolPair("toolu_read", "file_read", strings.Repeat("r", 300))...)
	state := NewToolResultReplacementState(nil)

	out, changed := applyToolResultBudget(msgs, state, ToolResultBudgetOptions{
		AggregateCapChars: 100,
		MinCandidateChars: 50,
		PreviewChars:      20,
		ToolMaxResultSizeChars: map[string]int{
			"file_read": UnlimitedToolResultSizeChars,
		},
	})

	if changed {
		t.Fatal("unlimited/self-bounded tool must not be budget-replaced")
	}
	if got := toolResultTextAt(t, out, 2); got != strings.Repeat("r", 300) {
		t.Fatalf("file_read changed: %q", got)
	}
}

func TestApplyToolResultBudget_PerUserMessageBudgetWithMultipleBlocks(t *testing.T) {
	msgs := []client.Message{
		{Role: "system", Content: client.NewTextContent("sys")},
		{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("toolu_a", "bash", nil),
			client.NewToolUseBlock("toolu_b", "grep", nil),
			client.NewToolUseBlock("toolu_c", "glob", nil),
		})},
		{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock("toolu_a", strings.Repeat("a", 90), false),
			client.NewToolResultBlock("toolu_b", strings.Repeat("b", 1000), false),
			client.NewToolResultBlock("toolu_c", strings.Repeat("c", 70), false),
		})},
	}

	state := NewToolResultReplacementState(nil)
	out, changed := applyToolResultBudget(msgs, state, ToolResultBudgetOptions{
		SessionID:         "sess",
		AggregateCapChars: 500,
		MinCandidateChars: 50,
		PreviewChars:      10,
	})

	if !changed {
		t.Fatal("expected aggregate budget replacement")
	}
	blocks := out[2].Content.Blocks()
	if strings.HasPrefix(client.ToolResultText(blocks[1]), strings.Repeat("b", 50)) {
		t.Fatalf("largest block was not replaced: %q", client.ToolResultText(blocks[1]))
	}
	if client.ToolResultText(blocks[0]) != strings.Repeat("a", 90) {
		t.Fatalf("smaller block changed unexpectedly")
	}
}
