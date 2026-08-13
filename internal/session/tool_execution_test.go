package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

func newTestToolExecution(t *testing.T, toolUseID, arguments string) ToolExecutionRecord {
	t.Helper()
	record, err := NewToolExecutionRecord(
		"run-test",
		"attempt-test",
		"calendar_create_event",
		toolUseID,
		arguments,
		time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC),
	)
	if err != nil {
		t.Fatal(err)
	}
	return record
}

func TestToolExecutionRecordPersistsDigestsOnly(t *testing.T) {
	const (
		toolUseID = "toolu_private_123"
		arguments = `{"recipient":"alice@example.com","body":"private text"}`
		result    = "created event with private title"
	)
	record := newTestToolExecution(t, toolUseID, arguments)
	sess := Session{ToolExecutions: []ToolExecutionRecord{record}}
	if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, ToolExecutionDigest(result), record.UpdatedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}

	data, err := json.Marshal(sess)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(data)
	for _, secret := range []string{toolUseID, arguments, "alice@example.com", "private text", result} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("serialized ledger leaked raw payload %q: %s", secret, serialized)
		}
	}
	for _, digest := range []string{ToolExecutionDigest(toolUseID), ToolExecutionDigest(arguments), ToolExecutionDigest(result)} {
		if !strings.Contains(serialized, digest) {
			t.Fatalf("serialized ledger is missing digest %q: %s", digest, serialized)
		}
	}
}

func TestNewToolExecutionRecordFromDigestDoesNotDoubleHash(t *testing.T) {
	digest := ToolExecutionDigest(`{"private":"value"}`)
	record, err := NewToolExecutionRecordFromDigest(
		"run-test", "attempt-test", "calendar_create_event", "toolu_digest", digest, time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if record.ArgumentsDigest != digest {
		t.Fatalf("arguments digest = %q, want %q", record.ArgumentsDigest, digest)
	}
	if _, err := NewToolExecutionRecordFromDigest(
		"run-test", "attempt-test", "calendar_create_event", "toolu_digest", "invalid", time.Now(),
	); !errors.Is(err, ErrInvalidToolExecutionLedger) {
		t.Fatalf("invalid digest error = %v, want invalid ledger", err)
	}
}

func TestToolExecutionStateMachineRejectsUnsafeTransitions(t *testing.T) {
	record := newTestToolExecution(t, "toolu_state", `{}`)
	sess := Session{}
	if err := sess.AddToolExecution(record); err != nil {
		t.Fatal(err)
	}
	resultDigest := ToolExecutionDigest("ok")

	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, resultDigest, record.UpdatedAt.Add(time.Second)); !errors.Is(err, ErrInvalidToolExecutionLedger) {
		t.Fatalf("prepared -> committed error = %v, want invalid ledger", err)
	}
	if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, "not-a-digest", record.UpdatedAt.Add(2*time.Second)); !errors.Is(err, ErrInvalidToolExecutionLedger) {
		t.Fatalf("invalid result digest error = %v, want invalid ledger", err)
	}
	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, resultDigest, record.UpdatedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.AbandonToolExecution(record.ExecutionID, record.UpdatedAt.Add(3*time.Second)); !errors.Is(err, ErrInvalidToolExecutionLedger) {
		t.Fatalf("committed -> abandoned error = %v, want invalid ledger", err)
	}
	if got := sess.BlockingToolExecutions("run-test"); len(got) != 1 || got[0].State != ToolExecutionCommitted {
		t.Fatalf("blocking executions = %#v", got)
	}
}

func TestToolExecutionFailedNoEffectIsDefinitiveAndNonBlocking(t *testing.T) {
	record := newTestToolExecution(t, "toolu_no_effect", `{}`)
	sess := Session{ToolExecutions: []ToolExecutionRecord{record}}
	if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionFailedNoEffect(record.ExecutionID, "not-a-digest", record.UpdatedAt.Add(2*time.Second)); !errors.Is(err, ErrInvalidToolExecutionLedger) {
		t.Fatalf("invalid result digest error = %v, want invalid ledger", err)
	}
	resultDigest := ToolExecutionDigest("known failure without external effect")
	if err := sess.MarkToolExecutionFailedNoEffect(record.ExecutionID, resultDigest, record.UpdatedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := sess.ToolExecutions[0].State; got != ToolExecutionFailedNoEffect {
		t.Fatalf("state = %q, want failed_no_effect", got)
	}
	if got := sess.ToolExecutions[0].ResultDigest; got != resultDigest {
		t.Fatalf("result digest = %q, want %q", got, resultDigest)
	}
	if blocked := sess.BlockingToolExecutions("run-test"); len(blocked) != 0 {
		t.Fatalf("known no-effect execution blocked recovery: %#v", blocked)
	}
}

func TestStoreSaveCheckpointsCommittedRecordWithMatchingToolResult(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	record := newTestToolExecution(t, "toolu_checkpoint", `{"title":"private"}`)
	sess := &Session{
		ID:    "session-checkpoint",
		Title: "test",
		Messages: []client.Message{
			{Role: "assistant", Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolUseBlock("toolu_checkpoint", "calendar_create_event", json.RawMessage(`{"title":"private"}`)),
			})},
			{Role: "user", Content: client.NewBlockContent([]client.ContentBlock{
				client.NewToolResultBlock("toolu_checkpoint", "private result", false),
			})},
		},
		ToolExecutions: []ToolExecutionRecord{record},
	}
	if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, ToolExecutionDigest("private result"), record.UpdatedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	if got := sess.ToolExecutions[0].State; got != ToolExecutionCheckpointed {
		t.Fatalf("in-memory state = %q, want checkpointed", got)
	}
	loaded, err := store.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := loaded.ToolExecutions[0].State; got != ToolExecutionCheckpointed {
		t.Fatalf("persisted state = %q, want checkpointed", got)
	}
	if blocked := loaded.BlockingToolExecutions("run-test"); len(blocked) != 0 {
		t.Fatalf("checkpointed execution still blocks recovery: %#v", blocked)
	}
}

func TestStoreSaveLeavesCommittedRecordWithoutMatchingToolResult(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	record := newTestToolExecution(t, "toolu_missing", `{}`)
	sess := &Session{ID: "session-missing-result", Title: "test", ToolExecutions: []ToolExecutionRecord{record}}
	if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, ToolExecutionDigest("ok"), record.UpdatedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	if got := sess.ToolExecutions[0].State; got != ToolExecutionCommitted {
		t.Fatalf("state = %q, want committed", got)
	}
	if blocked := sess.BlockingToolExecutions("run-test"); len(blocked) != 1 {
		t.Fatalf("blocking executions = %#v, want one", blocked)
	}
}

func TestReconcileToolExecutionCheckpointsSupportsStrictLegacyXML(t *testing.T) {
	record := newTestToolExecution(t, "legacy-call-123", `{}`)
	legacyResult := `<tool_exec tool="calendar_create_event" call_id="legacy-call-123">
<input>{"title":"private"}</input>
<output status="ok">created</output>
</tool_exec>`
	sess := Session{
		Messages:       []client.Message{{Role: "user", Content: client.NewTextContent(legacyResult)}},
		ToolExecutions: []ToolExecutionRecord{record},
	}
	if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, ToolExecutionDigest("created"), record.UpdatedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	resultDigest := sess.ToolExecutions[0].ResultDigest
	if err := sess.ReconcileToolExecutionCheckpoints(record.UpdatedAt.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := sess.ToolExecutions[0].State; got != ToolExecutionCheckpointed {
		t.Fatalf("state = %q, want checkpointed", got)
	}
	if got := sess.ToolExecutions[0].ResultDigest; got != resultDigest {
		t.Fatalf("result digest changed: got %q, want %q", got, resultDigest)
	}
}

func legacyToolExecutionFixture(toolName, callID, status string) string {
	return fmt.Sprintf("<tool_exec tool=%s call_id=%s>\n<input>{}</input>\n<output status=%q>created</output>\n</tool_exec>",
		strconv.Quote(toolName), strconv.Quote(callID), status)
}

func TestReconcileToolExecutionCheckpointsSupportsArbitraryQuotedToolNames(t *testing.T) {
	toolNames := []string{
		"calendar-create",
		"calendar.create",
		"calendar:create",
		`calendar:"create`,
	}
	sess := Session{}
	var transcript strings.Builder
	for i, toolName := range toolNames {
		callID := fmt.Sprintf("legacy-name-%d", i)
		record := newTestToolExecution(t, callID, `{}`)
		record.ToolName = toolName
		sess.ToolExecutions = append(sess.ToolExecutions, record)
		if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := sess.MarkToolExecutionCommitted(record.ExecutionID, ToolExecutionDigest("created"), record.UpdatedAt.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if i > 0 {
			transcript.WriteString("\n\n")
		}
		transcript.WriteString(legacyToolExecutionFixture(toolName, callID, "ok"))
	}
	sess.Messages = []client.Message{{Role: "user", Content: client.NewTextContent(transcript.String())}}

	if err := sess.ReconcileToolExecutionCheckpoints(time.Date(2026, 8, 10, 12, 0, 3, 0, time.UTC)); err != nil {
		t.Fatal(err)
	}
	for _, record := range sess.ToolExecutions {
		if record.State != ToolExecutionCheckpointed {
			t.Fatalf("tool %q remained %q", record.ToolName, record.State)
		}
	}
}

func TestReconcileToolExecutionCheckpointsBadSiblingDoesNotBlockGoodSibling(t *testing.T) {
	bad := newTestToolExecution(t, "legacy-bad", `{}`)
	good := newTestToolExecution(t, "legacy-good", `{}`)
	sess := Session{ToolExecutions: []ToolExecutionRecord{bad, good}}
	for _, record := range sess.ToolExecutions {
		if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := sess.MarkToolExecutionCommitted(record.ExecutionID, ToolExecutionDigest("created"), record.UpdatedAt.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
	}
	badSibling := strings.Replace(
		legacyToolExecutionFixture("calendar_create_event", "legacy-bad", "ok"),
		`status="ok"`, `status="unknown"`, 1,
	)
	goodSibling := legacyToolExecutionFixture("calendar_create_event", "legacy-good", "ok")
	sess.Messages = []client.Message{{Role: "user", Content: client.NewTextContent(badSibling + "\n\n" + goodSibling)}}

	if err := sess.ReconcileToolExecutionCheckpoints(bad.UpdatedAt.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := sess.ToolExecutions[0].State; got != ToolExecutionCommitted {
		t.Fatalf("bad sibling state = %q, want committed", got)
	}
	if got := sess.ToolExecutions[1].State; got != ToolExecutionCheckpointed {
		t.Fatalf("good sibling state = %q, want checkpointed", got)
	}
}

func TestReconcileToolExecutionCheckpointsSupportsLegacyImageMessage(t *testing.T) {
	record := newTestToolExecution(t, "legacy-image", `{}`)
	sess := Session{
		Messages: []client.Message{{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "text", Text: legacyToolExecutionFixture("calendar_create_event", "legacy-image", "ok")},
				{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "aW1hZ2U="}},
			}),
		}},
		ToolExecutions: []ToolExecutionRecord{record},
	}
	if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, ToolExecutionDigest("created"), record.UpdatedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.ReconcileToolExecutionCheckpoints(record.UpdatedAt.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := sess.ToolExecutions[0].State; got != ToolExecutionCheckpointed {
		t.Fatalf("image-bearing result state = %q, want checkpointed", got)
	}
}

func TestReconcileToolExecutionCheckpointsRejectsNonStructuralLegacyText(t *testing.T) {
	validXML := `<tool_exec tool="calendar_create_event" call_id="legacy-call-123">
<input>{}</input>
<output status="ok">created</output>
</tool_exec>`
	tests := []struct {
		name     string
		text     string
		toolName string
		callID   string
	}{
		{name: "ordinary text", text: "plain XML tool result", toolName: "calendar_create_event", callID: "legacy-call-123"},
		{name: "quoted with prefix", text: "The tool returned:\n" + validXML, toolName: "calendar_create_event", callID: "legacy-call-123"},
		{name: "quoted with suffix", text: validXML + "\nThis was only an example.", toolName: "calendar_create_event", callID: "legacy-call-123"},
		{name: "wrong call id", text: validXML, toolName: "calendar_create_event", callID: "different-call"},
		{name: "wrong tool name", text: validXML, toolName: "email_send", callID: "legacy-call-123"},
		{name: "extra attribute", text: strings.Replace(validXML, ` call_id="`, ` source="quoted" call_id="`, 1), toolName: "calendar_create_event", callID: "legacy-call-123"},
		{name: "invalid status", text: strings.Replace(validXML, `status="ok"`, `status="unknown"`, 1), toolName: "calendar_create_event", callID: "legacy-call-123"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			record := newTestToolExecution(t, tc.callID, `{}`)
			record.ToolName = tc.toolName
			sess := Session{
				Messages:       []client.Message{{Role: "user", Content: client.NewTextContent(tc.text)}},
				ToolExecutions: []ToolExecutionRecord{record},
			}
			if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := sess.MarkToolExecutionCommitted(record.ExecutionID, ToolExecutionDigest("created"), record.UpdatedAt.Add(2*time.Second)); err != nil {
				t.Fatal(err)
			}
			if err := sess.ReconcileToolExecutionCheckpoints(record.UpdatedAt.Add(3 * time.Second)); err != nil {
				t.Fatal(err)
			}
			if got := sess.ToolExecutions[0].State; got != ToolExecutionCommitted {
				t.Fatalf("non-structural text changed state to %q", got)
			}
		})
	}
}

func TestReconcileToolExecutionCheckpointsRejectsNarrationInImageMessage(t *testing.T) {
	record := newTestToolExecution(t, "legacy-image-quoted", `{}`)
	sess := Session{
		Messages: []client.Message{{
			Role: "user",
			Content: client.NewBlockContent([]client.ContentBlock{
				{Type: "text", Text: "The tool returned:\n" + legacyToolExecutionFixture("calendar_create_event", "legacy-image-quoted", "ok")},
				{Type: "image", Source: &client.ImageSource{Type: "base64", MediaType: "image/png", Data: "aW1hZ2U="}},
			}),
		}},
		ToolExecutions: []ToolExecutionRecord{record},
	}
	if err := sess.MarkToolExecutionDispatching(record.ExecutionID, record.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, ToolExecutionDigest("created"), record.UpdatedAt.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := sess.ReconcileToolExecutionCheckpoints(record.UpdatedAt.Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if got := sess.ToolExecutions[0].State; got != ToolExecutionCommitted {
		t.Fatalf("narrated image result changed state to %q", got)
	}
}

func TestToolExecutionPreparedCanBeAbandonedWithoutClearingAmbiguousWork(t *testing.T) {
	prepared := newTestToolExecution(t, "toolu_prepared", `{}`)
	dispatching := newTestToolExecution(t, "toolu_dispatching", `{}`)
	sess := Session{ToolExecutions: []ToolExecutionRecord{prepared, dispatching}}
	if err := sess.MarkToolExecutionDispatching(dispatching.ExecutionID, dispatching.UpdatedAt.Add(time.Second)); err != nil {
		t.Fatal(err)
	}

	if got := sess.AbandonPreparedToolExecutions("run-test", prepared.UpdatedAt.Add(2*time.Second)); got != 1 {
		t.Fatalf("abandoned count = %d, want 1", got)
	}
	if got := sess.ToolExecutions[0].State; got != ToolExecutionAbandoned {
		t.Fatalf("prepared state = %q, want abandoned", got)
	}
	if got := sess.ToolExecutions[1].State; got != ToolExecutionDispatching {
		t.Fatalf("dispatching state = %q, want unchanged", got)
	}
	if blocked := sess.BlockingToolExecutions("run-test"); len(blocked) != 1 || blocked[0].ExecutionID != dispatching.ExecutionID {
		t.Fatalf("blocking executions = %#v", blocked)
	}
}

func TestStoreRejectsCorruptToolExecutionLedgerOnSaveAndLoad(t *testing.T) {
	dir := t.TempDir()
	store := NewStore(dir)
	defer store.Close()
	record := newTestToolExecution(t, "toolu_corrupt", `{}`)
	record.State = ToolExecutionState("invented")
	sess := &Session{ID: "session-corrupt-save", Title: "test", ToolExecutions: []ToolExecutionRecord{record}}
	if err := store.Save(sess); !errors.Is(err, ErrInvalidToolExecutionLedger) {
		t.Fatalf("save error = %v, want invalid ledger", err)
	}
	if _, err := os.Stat(filepath.Join(dir, sess.ID+".json")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("invalid session was persisted: %v", err)
	}

	record.State = ToolExecutionPrepared
	record.ArgumentsDigest = "broken"
	data, err := json.Marshal(&Session{ID: "session-corrupt-load", Title: "test", ToolExecutions: []ToolExecutionRecord{record}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "session-corrupt-load.json"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Load("session-corrupt-load"); !errors.Is(err, ErrInvalidToolExecutionLedger) {
		t.Fatalf("load error = %v, want invalid ledger", err)
	}
}

func TestStoreBoundsTerminalToolExecutionsWithoutTrimmingUnresolved(t *testing.T) {
	store := NewStore(t.TempDir())
	defer store.Close()
	base := time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC)
	sess := &Session{ID: "session-ledger-retention", Title: "test"}
	terminalIDs := make([]string, 0, MaxRetainedTerminalToolExecutions+4)
	for i := 0; i < MaxRetainedTerminalToolExecutions+4; i++ {
		record := newTestToolExecution(t, "terminal-"+string(rune(i+1)), `{}`)
		record.State = ToolExecutionCheckpointed
		record.ResultDigest = ToolExecutionDigest("result")
		record.PreparedAt = base.Add(time.Duration(i) * time.Second)
		record.UpdatedAt = record.PreparedAt
		terminalIDs = append(terminalIDs, record.ExecutionID)
		sess.ToolExecutions = append(sess.ToolExecutions, record)
	}

	states := []ToolExecutionState{
		ToolExecutionPrepared,
		ToolExecutionDispatching,
		ToolExecutionCommitted,
		ToolExecutionOutcomeUnknown,
	}
	unresolvedIDs := make(map[string]struct{}, len(states))
	for i, state := range states {
		record := newTestToolExecution(t, "unresolved-"+string(rune(i+1)), `{}`)
		record.State = state
		if state == ToolExecutionCommitted {
			record.ResultDigest = ToolExecutionDigest("known result")
		}
		record.PreparedAt = base.Add(time.Duration(1000+i) * time.Second)
		record.UpdatedAt = record.PreparedAt
		unresolvedIDs[record.ExecutionID] = struct{}{}
		sess.ToolExecutions = append(sess.ToolExecutions, record)
	}

	if err := store.Save(sess); err != nil {
		t.Fatal(err)
	}
	terminalCount := 0
	retained := make(map[string]struct{}, len(sess.ToolExecutions))
	for _, record := range sess.ToolExecutions {
		retained[record.ExecutionID] = struct{}{}
		if record.State == ToolExecutionCheckpointed || record.State == ToolExecutionAbandoned {
			terminalCount++
		}
	}
	if terminalCount != MaxRetainedTerminalToolExecutions {
		t.Fatalf("terminal count = %d, want %d", terminalCount, MaxRetainedTerminalToolExecutions)
	}
	for id := range unresolvedIDs {
		if _, ok := retained[id]; !ok {
			t.Fatalf("unresolved execution %q was trimmed", id)
		}
	}
	for _, id := range terminalIDs[:4] {
		if _, ok := retained[id]; ok {
			t.Fatalf("old terminal execution %q was retained", id)
		}
	}
}
