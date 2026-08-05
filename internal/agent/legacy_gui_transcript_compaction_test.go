package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

type legacyGUIFixture struct {
	Messages []client.Message `json:"messages"`
}

type persistedLegacyToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
	Wire  []byte
}

func legacyToolUsesFromHistory(t *testing.T, messages []client.Message) []persistedLegacyToolUse {
	t.Helper()
	var uses []persistedLegacyToolUse
	for _, message := range messages {
		if message.Role != "assistant" || !message.Content.HasBlocks() {
			continue
		}
		for _, block := range message.Content.Blocks() {
			if block.Type == "tool_use" {
				wire, err := json.Marshal(block)
				if err != nil {
					t.Fatalf("marshal provider-visible tool_use: %v", err)
				}
				uses = append(uses, persistedLegacyToolUse{
					ID:    block.ID,
					Name:  block.Name,
					Input: append(json.RawMessage(nil), block.Input...),
					Wire:  wire,
				})
			}
		}
	}
	return uses
}

func assertLegacyToolUsesEqual(t *testing.T, want, got []persistedLegacyToolUse) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tool_use count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Name != want[i].Name || !bytes.Equal(got[i].Wire, want[i].Wire) {
			t.Fatalf("tool_use[%d] changed during compaction:\n  want: id=%q name=%q input=%s\n  got:  id=%q name=%q input=%s",
				i, want[i].ID, want[i].Name, want[i].Input, got[i].ID, got[i].Name, got[i].Input)
		}
	}
}

// The canonical persisted-history fixture is also consumed by the session
// load/redact/save test. This package-level half exercises the real unexported
// compactor without adding a production dependency from agent back to session.
func TestLegacyPairedGUITranscript_CompactionPreservesToolIdentityWithoutExecutors(t *testing.T) {
	registry := NewToolRegistry()
	if registry.Has("computer") || registry.Has("accessibility") {
		t.Fatal("fixture test must run with both legacy executors unregistered")
	}

	raw, err := os.ReadFile(filepath.Join("..", "session", "testdata", "legacy_paired_gui_transcript.v1.json"))
	if err != nil {
		t.Fatalf("read historical transcript fixture: %v", err)
	}
	var fixture legacyGUIFixture
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatalf("decode historical transcript fixture: %v", err)
	}
	wantUses := legacyToolUsesFromHistory(t, fixture.Messages)
	if len(wantUses) != 2 || wantUses[0].Name != "computer" || wantUses[1].Name != "accessibility" {
		t.Fatalf("fixture must contain ordered computer + accessibility pairs, got %+v", wantUses)
	}

	compressOldToolResults(context.Background(), fixture.Messages, 1, 80, nil, "")
	assertLegacyToolUsesEqual(t, wantUses, legacyToolUsesFromHistory(t, fixture.Messages))

	firstResult := fixture.Messages[2].Content.Blocks()[0]
	if firstResult.Type != "tool_result" || firstResult.CompressedTier != 2 {
		t.Fatalf("old computer result was not compacted: %+v", firstResult)
	}
	if got := firstResult.ToolUseID; got != wantUses[0].ID {
		t.Fatalf("computer tool_result pairing changed: got %q, want %q", got, wantUses[0].ID)
	}
	if got := fixture.Messages[4].Content.Blocks()[0].ToolUseID; got != wantUses[1].ID {
		t.Fatalf("accessibility tool_result pairing changed: got %q, want %q", got, wantUses[1].ID)
	}

	// Exercise the serialization seam used by Store.Save without exporting the
	// unexported compactor or introducing an agent -> session import cycle.
	maintainedWire, err := json.Marshal(fixture)
	if err != nil {
		t.Fatalf("marshal compacted historical transcript: %v", err)
	}
	var maintained legacyGUIFixture
	if err := json.Unmarshal(maintainedWire, &maintained); err != nil {
		t.Fatalf("reload compacted historical transcript: %v", err)
	}
	assertLegacyToolUsesEqual(t, wantUses, legacyToolUsesFromHistory(t, maintained.Messages))
}
