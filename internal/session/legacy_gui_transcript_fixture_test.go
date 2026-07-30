package session_test

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	"github.com/Kocoro-lab/ShanClaw/internal/share"
)

type persistedToolUse struct {
	ID    string
	Name  string
	Input json.RawMessage
	Wire  []byte
}

func toolUsesFromHistory(t *testing.T, messages []client.Message) []persistedToolUse {
	t.Helper()
	var uses []persistedToolUse
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
				uses = append(uses, persistedToolUse{
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

func assertToolUsesEqual(t *testing.T, want, got []persistedToolUse) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("tool_use count = %d, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i].ID != want[i].ID || got[i].Name != want[i].Name || !bytes.Equal(got[i].Wire, want[i].Wire) {
			t.Fatalf("tool_use[%d] changed across history maintenance:\n  want: id=%q name=%q input=%s\n  got:  id=%q name=%q input=%s",
				i, want[i].ID, want[i].Name, want[i].Input, got[i].ID, got[i].Name, got[i].Input)
		}
	}
}

// This is the storage/redaction half of the shared retirement fixture. The
// agent-package companion runs the real context compactor over the same JSON.
func TestLegacyPairedGUITranscript_LoadRedactSaveWithoutExecutors(t *testing.T) {
	registry := agent.NewToolRegistry()
	if registry.Has("computer") || registry.Has("accessibility") {
		t.Fatal("fixture test must run with both legacy executors unregistered")
	}

	fixture, err := os.ReadFile(filepath.Join("testdata", "legacy_paired_gui_transcript.v1.json"))
	if err != nil {
		t.Fatalf("read historical transcript fixture: %v", err)
	}
	sessionsDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(sessionsDir, "legacy-paired-gui.json"), fixture, 0600); err != nil {
		t.Fatalf("stage historical transcript fixture: %v", err)
	}
	store := session.NewStore(sessionsDir)
	t.Cleanup(func() { _ = store.Close() })

	loaded, err := store.Load("legacy-paired-gui")
	if err != nil {
		t.Fatalf("load historical transcript: %v", err)
	}
	wantUses := toolUsesFromHistory(t, loaded.Messages)
	if len(wantUses) != 2 || wantUses[0].Name != "computer" || wantUses[1].Name != "accessibility" {
		t.Fatalf("fixture must contain ordered computer + accessibility pairs, got %+v", wantUses)
	}
	const historicalAccessibilityInput = `{"action":"read_tree","app":"Slack","description":"Inspect interactive Slack controls","filter":"interactive"}`
	if !bytes.Equal(wantUses[1].Input, []byte(historicalAccessibilityInput)) {
		t.Fatalf("fixture accessibility call is not the historical executable read_tree schema: %s", wantUses[1].Input)
	}

	// Share/publication redaction operates on a copy and cannot corrupt the
	// provider-visible tool pairs that remain in the resumable local session.
	redacted, _ := share.Sanitize(loaded.Messages, loaded.MessageMeta, share.Options{HomeDir: "/Users/alice"})
	redactedJSON, err := json.Marshal(redacted)
	if err != nil {
		t.Fatalf("marshal redacted transcript: %v", err)
	}
	if bytes.Contains(redactedJSON, []byte("/Users/alice")) {
		t.Fatalf("share-copy redaction leaked fixture home path: %s", redactedJSON)
	}
	assertToolUsesEqual(t, wantUses, toolUsesFromHistory(t, loaded.Messages))

	if err := store.Save(loaded); err != nil {
		t.Fatalf("save maintained historical transcript: %v", err)
	}
	reloaded, err := store.Load("legacy-paired-gui")
	if err != nil {
		t.Fatalf("reload maintained historical transcript: %v", err)
	}
	assertToolUsesEqual(t, wantUses, toolUsesFromHistory(t, reloaded.Messages))
	if got := reloaded.Messages[2].Content.Blocks()[0].ToolUseID; got != wantUses[0].ID {
		t.Fatalf("computer tool_result pairing changed: got %q, want %q", got, wantUses[0].ID)
	}
	if got := reloaded.Messages[4].Content.Blocks()[0].ToolUseID; got != wantUses[1].ID {
		t.Fatalf("accessibility tool_result pairing changed: got %q, want %q", got, wantUses[1].ID)
	}
}
