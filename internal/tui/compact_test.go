package tui

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestCompact_TooShort(t *testing.T) {
	m := newCommandTestModel(t)
	// Session has 0 messages — too short
	result := m.runCompact("")()
	if result.err == nil {
		t.Error("expected error for too-short conversation")
	}
	if result.err != nil && !strings.Contains(result.err.Error(), "too short") {
		t.Errorf("expected 'too short' error, got: %v", result.err)
	}
}

func TestFormatCompactResult(t *testing.T) {
	msg := compactDoneMsg{
		beforeTokens: 50000,
		afterTokens:  8000,
		summary:      "User worked on TUI improvements",
	}
	result := formatCompactResult(msg)
	if !strings.Contains(result, "50,000") || !strings.Contains(result, "8,000") {
		t.Errorf("expected formatted token counts in result: %s", result)
	}
	if !strings.Contains(result, "TUI improvements") {
		t.Errorf("expected summary text in result: %s", result)
	}
}

func TestCompact_PreservesArchiveAndMetadataAndPersistsLiveCheckpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req client.CompletionRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		output := `<summary>
## Current task & next steps
Continue the checkpoint migration.
## User corrections & decisions
Keep the transcript lossless.
</summary>`
		if len(req.Messages) > 0 && strings.Contains(req.Messages[0].Content.Text(), "extracting durable knowledge") {
			output = "NONE"
		}
		_ = json.NewEncoder(w).Encode(client.CompletionResponse{
			OutputText: output,
			Usage:      client.Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15},
		})
	}))
	defer server.Close()

	m := newCommandTestModel(t)
	m.gateway = client.NewGatewayClient(server.URL, "")
	m.cfg.Agent.ContextWindow = 128000
	sess := m.sessions.Current()
	now := time.Now()
	for i := 0; i < 12; i++ {
		role := "user"
		if i%2 == 1 {
			role = "assistant"
		}
		sess.Messages = append(sess.Messages, client.Message{Role: role, Content: client.NewTextContent(strings.Repeat("history ", 100))})
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{
			Source:    "local",
			MessageID: string(rune('a' + i)),
			Timestamp: session.TimePtr(now.Add(time.Duration(i) * time.Second)),
		})
	}
	originalMessages := append([]client.Message(nil), sess.Messages...)
	originalMeta := append([]session.MessageMeta(nil), sess.MessageMeta...)

	result := m.runCompact("")()
	if result.err != nil {
		t.Fatalf("runCompact: %v", result.err)
	}
	if !reflect.DeepEqual(sess.Messages, originalMessages) {
		t.Fatal("manual compaction mutated the lossless transcript")
	}
	if !reflect.DeepEqual(sess.MessageMeta, originalMeta) {
		t.Fatal("manual compaction mutated message metadata")
	}
	cp := sess.CompactionCheckpoint
	if cp == nil || cp.SchemaVersion != session.CompactionCheckpointSchemaVersion || cp.ArchiveThroughIndex != len(originalMessages) {
		t.Fatalf("checkpoint was not persisted against archive: %#v", cp)
	}
	if len(cp.Messages) >= len(originalMessages) {
		t.Fatalf("checkpoint did not compact live context: %d >= %d", len(cp.Messages), len(originalMessages))
	}
	if got := sess.HistoryForLoop(); !reflect.DeepEqual(got, cp.Messages) {
		t.Fatalf("live history does not resolve to checkpoint: got=%#v cp=%#v", got, cp.Messages)
	}

	loaded, err := m.sessions.Load(sess.ID)
	if err != nil {
		t.Fatalf("reload compacted session: %v", err)
	}
	if loaded.CompactionCheckpoint == nil || len(loaded.Messages) != len(originalMessages) {
		t.Fatalf("disk round-trip lost checkpoint or archive: cp=%#v archive=%d", loaded.CompactionCheckpoint, len(loaded.Messages))
	}
}
