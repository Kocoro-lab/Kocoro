package schedule

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// writeFakeSession writes a minimal session JSON file at the expected path
// for the given agent ("" = default). Mimics the on-disk shape that
// internal/session/store.go would write.
func writeFakeSession(t *testing.T, shannonDir, agent, sessionID string, messages []map[string]any) {
	t.Helper()
	var sessDir string
	if agent == "" {
		sessDir = filepath.Join(shannonDir, "sessions")
	} else {
		sessDir = filepath.Join(shannonDir, "agents", agent, "sessions")
	}
	if err := os.MkdirAll(sessDir, 0700); err != nil {
		t.Fatal(err)
	}
	payload := map[string]any{
		"id":             sessionID,
		"schema_version": 1,
		"messages":       messages,
	}
	data, _ := json.Marshal(payload)
	if err := os.WriteFile(filepath.Join(sessDir, sessionID+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

func TestSummarizeLastRun_NeverRun(t *testing.T) {
	sched := Schedule{ID: "x", Agent: "explorer"} // LastRunSessionID empty
	out, err := SummarizeLastRun(sched, t.TempDir(), 5)
	if err != nil {
		t.Fatalf("never-run path should not error: %v", err)
	}
	if out.SessionID != "" {
		t.Errorf("never-run SessionID should stay empty, got %q", out.SessionID)
	}
	if len(out.Turns) != 0 {
		t.Errorf("never-run Turns should be empty, got %d", len(out.Turns))
	}
	if out.LastRunAt != nil {
		t.Errorf("never-run LastRunAt should be nil, got %v", out.LastRunAt)
	}
}

// The critical test: the session has 6 messages total — 2 from prior
// interactive chat and 4 from THIS run. SummarizeLastRun must return only
// the 2 assistant turns from this run (indices 2..6), NOT the interactive
// chat that came before. This is the scenario that motivated the whole spec.
func TestSummarizeLastRun_SlicesByMessageRange(t *testing.T) {
	shan := t.TempDir()
	writeFakeSession(t, shan, "explorer", "sess-shared", []map[string]any{
		// indices 0-1: prior interactive chat — must NOT appear in output
		{"role": "user", "content": "tell me a joke"},
		{"role": "assistant", "content": "interactive chat reply"},
		// indices 2-5: this scheduled run wrote these
		{"role": "user", "content": "scheduled prompt"},
		{"role": "assistant", "content": "scheduled reply one"},
		{"role": "user", "content": "follow-up tool call"},
		{"role": "assistant", "content": "scheduled reply two"},
	})
	when := time.Now()
	sched := Schedule{
		ID:                       "x",
		Agent:                    "explorer",
		LastRunSessionID:         "sess-shared",
		LastRunAt:                &when,
		LastRunMessageStartIndex: 2,
		LastRunMessageEndIndex:   6,
	}

	out, err := SummarizeLastRun(sched, shan, 5)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if out.SessionID != "sess-shared" {
		t.Errorf("SessionID = %q, want sess-shared", out.SessionID)
	}
	if out.AgentName != "explorer" {
		t.Errorf("AgentName = %q, want explorer", out.AgentName)
	}
	if len(out.Turns) != 2 {
		t.Fatalf("Turns len = %d, want 2 (the run wrote 2 assistant turns)", len(out.Turns))
	}
	if out.Turns[0].Text != "scheduled reply one" || out.Turns[1].Text != "scheduled reply two" {
		t.Errorf("turns must be from the slice [2:6], got: %+v", out.Turns)
	}
	for _, turn := range out.Turns {
		if strings.Contains(turn.Text, "interactive chat reply") {
			t.Errorf("interactive-chat reply leaked into output: %+v", out.Turns)
		}
	}
}

func TestSummarizeLastRun_DefaultAgentSession(t *testing.T) {
	shan := t.TempDir()
	writeFakeSession(t, shan, "", "sess-default", []map[string]any{
		{"role": "user", "content": "q"},
		{"role": "assistant", "content": "a"},
	})
	sched := Schedule{
		ID:                       "x",
		Agent:                    "",
		LastRunSessionID:         "sess-default",
		LastRunMessageStartIndex: 0,
		LastRunMessageEndIndex:   2,
	}

	out, err := SummarizeLastRun(sched, shan, 5)
	if err != nil {
		t.Fatalf("show: %v", err)
	}
	if len(out.Turns) != 1 || out.Turns[0].Text != "a" {
		t.Errorf("turns: %+v", out.Turns)
	}
}

func TestSummarizeLastRun_MaxTurnsClampsHead(t *testing.T) {
	shan := t.TempDir()
	msgs := []map[string]any{}
	for i := 1; i <= 10; i++ {
		msgs = append(msgs, map[string]any{"role": "user", "content": "q"})
		msgs = append(msgs, map[string]any{"role": "assistant", "content": fmt.Sprintf("reply %d", i)})
	}
	writeFakeSession(t, shan, "x", "sess-many", msgs)
	sched := Schedule{
		ID:                       "x",
		Agent:                    "x",
		LastRunSessionID:         "sess-many",
		LastRunMessageStartIndex: 0,
		LastRunMessageEndIndex:   20,
	}

	out, _ := SummarizeLastRun(sched, shan, 3)
	if len(out.Turns) != 3 {
		t.Fatalf("max_turns=3 should produce 3 turns, got %d", len(out.Turns))
	}
	if !strings.HasSuffix(out.Turns[len(out.Turns)-1].Text, "10") {
		t.Errorf("max_turns should keep tail, got last turn %q", out.Turns[len(out.Turns)-1].Text)
	}
}

func TestSummarizeLastRun_ClampsOutOfRangeIndices(t *testing.T) {
	shan := t.TempDir()
	writeFakeSession(t, shan, "x", "sess-trunc", []map[string]any{
		{"role": "user", "content": "q"},
		{"role": "assistant", "content": "a"},
	})
	sched := Schedule{
		ID:                       "x",
		Agent:                    "x",
		LastRunSessionID:         "sess-trunc",
		LastRunMessageStartIndex: 10,
		LastRunMessageEndIndex:   20,
	}

	out, err := SummarizeLastRun(sched, shan, 5)
	if err != nil {
		t.Fatalf("out-of-range indices should clamp, not error: %v", err)
	}
	if len(out.Turns) != 0 {
		t.Errorf("clamped range should yield 0 turns, got %d", len(out.Turns))
	}
}

func TestSummarizeLastRun_LegacyNoRangeFallsBackToTail(t *testing.T) {
	shan := t.TempDir()
	writeFakeSession(t, shan, "x", "sess-legacy", []map[string]any{
		{"role": "user", "content": "q"},
		{"role": "assistant", "content": "fallback reply"},
	})
	sched := Schedule{
		ID:               "x",
		Agent:            "x",
		LastRunSessionID: "sess-legacy",
	}

	out, err := SummarizeLastRun(sched, shan, 5)
	if err != nil {
		t.Fatalf("legacy fallback should not error: %v", err)
	}
	if len(out.Turns) != 1 || out.Turns[0].Text != "fallback reply" {
		t.Errorf("legacy fallback should show session tail, got %+v", out.Turns)
	}
}

func TestSummarizeLastRun_NeverRun_TurnsIsEmptyArrayNotNull(t *testing.T) {
	sched := Schedule{ID: "x"} // never run
	out, _ := SummarizeLastRun(sched, t.TempDir(), 5)
	data, _ := json.Marshal(out)
	if !strings.Contains(string(data), `"turns":[]`) {
		t.Errorf("turns must serialize as [] not null, got: %s", data)
	}
	if strings.Contains(string(data), `"turns":null`) {
		t.Errorf("turns must not serialize as null, got: %s", data)
	}
}

func TestSummarizeLastRun_MissingFileTreatedAsNoRun(t *testing.T) {
	shan := t.TempDir()
	ranAt := time.Now()
	sched := Schedule{
		ID:                       "x",
		Agent:                    "explorer",
		LastRunSessionID:         "sess-gone",
		LastRunAt:                &ranAt,
		LastRunMessageStartIndex: 0,
		LastRunMessageEndIndex:   2,
	}

	// A deleted last-run session must not error: it degrades to the exact same
	// empty shape as a schedule that has never run (no id, no timestamp, no
	// turns), so clients render a neutral state.
	out, err := SummarizeLastRun(sched, shan, 5)
	if err != nil {
		t.Fatalf("missing session file should not error, got %v", err)
	}
	if out.SessionID != "" {
		t.Errorf("SessionID should be cleared when the session is gone, got %q", out.SessionID)
	}
	if out.LastRunAt != nil {
		t.Errorf("LastRunAt should be cleared when the session is gone, got %v", out.LastRunAt)
	}
	if len(out.Turns) != 0 {
		t.Errorf("Turns should be empty, got %d", len(out.Turns))
	}
}
