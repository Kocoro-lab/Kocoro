package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

func TestSessionSideEffectJournalPersistsDigestOnlyAndCheckpointsWithTranscript(t *testing.T) {
	storeDir := t.TempDir()
	manager := session.NewManager(storeDir)
	t.Cleanup(func() { _ = manager.Close() })
	sess := manager.NewSessionWithID("side-effect-session-001")
	journal := newSessionSideEffectJournal(manager, sess, "run_test_001", "attempt_test_001")

	const (
		rawArgs   = `{"recipient":"private@example.com","body":"secret text"}`
		toolUseID = "provider-tool-use-private-001"
	)
	prepared, err := journal.Prepare(context.Background(), agent.SideEffectExecution{
		ToolUseID:       toolUseID,
		ToolName:        "send_message",
		ArgumentsSHA256: session.ToolExecutionDigest(rawArgs),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := journal.MarkDispatching(context.Background(), prepared.ExecutionID); err != nil {
		t.Fatalf("MarkDispatching: %v", err)
	}
	resultDigest := session.ToolExecutionDigest("sent")
	if err := journal.MarkCommitted(context.Background(), prepared.ExecutionID, resultDigest); err != nil {
		t.Fatalf("MarkCommitted: %v", err)
	}

	sess.Messages = append(sess.Messages, client.Message{
		Role: "user",
		Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolResultBlock(toolUseID, "sent", false),
		}),
	})
	rollback, err := stageTranscriptToolExecutionsForSave(sess)
	if err != nil {
		t.Fatalf("stage transcript checkpoint: %v", err)
	}
	if err := manager.Save(); err != nil {
		rollback()
		t.Fatalf("Save checkpoint: %v", err)
	}

	persisted, err := manager.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(persisted.ToolExecutions) != 1 || persisted.ToolExecutions[0].State != session.ToolExecutionCheckpointed {
		t.Fatalf("persisted executions = %#v", persisted.ToolExecutions)
	}
	data, err := os.ReadFile(filepath.Join(storeDir, sess.ID+".json"))
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	for _, secret := range []string{rawArgs, "private@example.com", "secret text"} {
		if strings.Contains(string(data), secret) {
			t.Fatalf("session ledger leaked raw value %q", secret)
		}
	}
	ledgerData, err := json.Marshal(persisted.ToolExecutions)
	if err != nil {
		t.Fatalf("Marshal ledger: %v", err)
	}
	if strings.Contains(string(ledgerData), toolUseID) {
		t.Fatal("tool execution ledger retained the provider tool-use id")
	}
}

func TestSessionSideEffectJournalPrepareStagesUntilDispatchBoundary(t *testing.T) {
	manager := session.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close() })
	sess := manager.NewSessionWithID("side-effect-session-staged-prepare")
	if err := manager.Save(); err != nil {
		t.Fatal(err)
	}
	journal := newSessionSideEffectJournal(manager, sess, "run_staged_prepare", "attempt_staged_prepare")
	prepared, err := journal.Prepare(context.Background(), agent.SideEffectExecution{
		ToolUseID:       "tool-use-staged-prepare",
		ToolName:        "send_message",
		ArgumentsSHA256: session.ToolExecutionDigest(`{"body":"private"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	persisted, err := manager.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.ToolExecutions) != 0 {
		t.Fatalf("Prepare performed a redundant full-session save: %#v", persisted.ToolExecutions)
	}
	if err := journal.MarkDispatching(context.Background(), prepared.ExecutionID); err != nil {
		t.Fatal(err)
	}
	persisted, err = manager.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(persisted.ToolExecutions) != 1 || persisted.ToolExecutions[0].State != session.ToolExecutionDispatching {
		t.Fatalf("dispatch boundary was not durable: %#v", persisted.ToolExecutions)
	}
}

func TestGuardInterruptedToolExecutionsBlocksAmbiguousDispatchAndPersistsWarning(t *testing.T) {
	manager := session.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close() })
	sess := manager.NewSessionWithID("side-effect-session-002")
	state := session.InterruptedTurn{
		RunID:     "run_test_002",
		AttemptID: "attempt_test_002",
		Source:    "desktop",
		UpdatedAt: time.Now(),
	}
	sess.Messages = []client.Message{{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock("tool-use-002", "create_record", []byte(`{"value":2}`)),
		}),
	}}
	sess.MessageMeta = []session.MessageMeta{{Source: "desktop", Timestamp: session.TimePtr(time.Now())}}
	sess.InProgress = true
	sess.InterruptedTurn = &state
	journal := newSessionSideEffectJournal(manager, sess, state.RunID, state.AttemptID)
	prepared, err := journal.Prepare(context.Background(), agent.SideEffectExecution{
		ToolUseID:       "tool-use-002",
		ToolName:        "create_record",
		ArgumentsSHA256: session.ToolExecutionDigest(`{"value":2}`),
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	if err := journal.MarkDispatching(context.Background(), prepared.ExecutionID); err != nil {
		t.Fatalf("MarkDispatching: %v", err)
	}

	err = guardInterruptedToolExecutions(manager, sess, state)
	if !errors.Is(err, errInterruptedRecoveryReviewRequired) {
		t.Fatalf("guard error = %v", err)
	}
	persisted, err := manager.Load(sess.ID)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if persisted.InProgress || persisted.InterruptedTurn != nil {
		t.Fatalf("recovery marker was not cleared: %#v", persisted.InterruptedTurn)
	}
	if got := persisted.ToolExecutions[0].State; got != session.ToolExecutionOutcomeUnknown {
		t.Fatalf("state = %q", got)
	}
	if len(persisted.Messages) != 3 || !strings.Contains(persisted.Messages[2].Content.Text(), "was not retried") {
		t.Fatalf("warning message = %#v", persisted.Messages)
	}
	blocks := persisted.Messages[1].Content.Blocks()
	if len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != "tool-use-002" {
		t.Fatalf("synthetic outcome-unknown pair = %#v", blocks)
	}
}

func TestGuardInterruptedToolExecutionsContinuesAfterKnownNoEffectFailure(t *testing.T) {
	manager := session.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close() })
	sess := manager.NewSessionWithID("side-effect-session-no-effect")
	state := session.InterruptedTurn{
		RunID: "run_no_effect", AttemptID: "attempt_no_effect", Source: "desktop", UpdatedAt: time.Now(),
	}
	const toolUseID = "tool-use-no-effect"
	sess.Messages = []client.Message{{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock(toolUseID, "computer_use", []byte(`{"task":"locate"}`)),
		}),
	}}
	sess.MessageMeta = []session.MessageMeta{{Source: "desktop", Timestamp: session.TimePtr(time.Now())}}
	sess.InProgress = true
	sess.InterruptedTurn = &state
	journal := newSessionSideEffectJournal(manager, sess, state.RunID, state.AttemptID)
	prepared, err := journal.Prepare(context.Background(), agent.SideEffectExecution{
		ToolUseID: toolUseID, ToolName: "computer_use",
		ArgumentsSHA256: session.ToolExecutionDigest(`{"task":"locate"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.MarkDispatching(context.Background(), prepared.ExecutionID); err != nil {
		t.Fatal(err)
	}
	if err := journal.MarkFailedNoEffect(context.Background(), prepared.ExecutionID, session.ToolExecutionDigest("app_resolution_failed")); err != nil {
		t.Fatal(err)
	}

	if err := guardInterruptedToolExecutions(manager, sess, state); err != nil {
		t.Fatalf("known no-effect recovery was blocked: %v", err)
	}
	if blocked := sess.BlockingToolExecutions(state.RunID); len(blocked) != 0 {
		t.Fatalf("known no-effect recovery has blocking records: %#v", blocked)
	}
	blocks := sess.Messages[len(sess.Messages)-1].Content.Blocks()
	if len(blocks) != 1 || blocks[0].Type != "tool_result" || blocks[0].ToolUseID != toolUseID ||
		!strings.Contains(client.ToolResultText(blocks[0]), "safe to continue") {
		t.Fatalf("safe synthetic result = %#v", blocks)
	}
}

func TestPreparedSideEffectRecoveryPairsToolCallBeforeResume(t *testing.T) {
	manager := session.NewManager(t.TempDir())
	t.Cleanup(func() { _ = manager.Close() })
	sess := manager.NewSessionWithID("side-effect-session-003")
	state := session.InterruptedTurn{
		RunID:     "run1_00000000000000000000000000000003",
		AttemptID: "att1_00000000000000000000000000000003",
		Source:    "desktop",
		UpdatedAt: time.Now(),
	}
	const toolUseID = "prepared-tool-use-003"
	sess.Messages = []client.Message{{
		Role: "assistant",
		Content: client.NewBlockContent([]client.ContentBlock{
			client.NewToolUseBlock(toolUseID, "create_record", []byte(`{"value":3}`)),
		}),
	}}
	sess.MessageMeta = []session.MessageMeta{{Source: "desktop", Timestamp: session.TimePtr(time.Now())}}
	sess.InProgress = true
	sess.InterruptedTurn = &state
	record, err := session.NewToolExecutionRecordFromDigest(
		state.RunID,
		state.AttemptID,
		"create_record",
		toolUseID,
		session.ToolExecutionDigest(`{"value":3}`),
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AddToolExecution(record); err != nil {
		t.Fatal(err)
	}
	if err := manager.Save(); err != nil {
		t.Fatal(err)
	}
	req := RunAgentRequest{
		ResumeInterrupted:                    true,
		InterruptedResumeCheckpointUpdatedAt: state.UpdatedAt,
	}
	if err := claimInterruptedResume(manager, sess, &req, ""); err != nil {
		t.Fatalf("claimInterruptedResume: %v", err)
	}
	persisted, err := manager.Load(sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.ToolExecutions[0].State; got != session.ToolExecutionAbandoned {
		t.Fatalf("prepared state = %q", got)
	}
	paired := false
	for _, message := range persisted.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type == "tool_result" && block.ToolUseID == toolUseID && block.IsError {
				paired = true
			}
		}
	}
	if !paired {
		t.Fatal("prepared tool_use was not paired with a synthetic not-executed result")
	}
}

func TestPreparedCheckpointDoesNotCheckpointEarlierUncapturedWrite(t *testing.T) {
	sess := &session.Session{}
	record, err := session.NewToolExecutionRecordFromDigest(
		"run1_00000000000000000000000000000004",
		"att1_00000000000000000000000000000004",
		"first_write",
		"first-tool-use-004",
		session.ToolExecutionDigest(`{"value":4}`),
		time.Now(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := sess.AddToolExecution(record); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionDispatching(record.ExecutionID, time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := sess.MarkToolExecutionCommitted(record.ExecutionID, session.ToolExecutionDigest("created"), time.Now()); err != nil {
		t.Fatal(err)
	}
	rollback, err := stageTranscriptToolExecutionsForSave(sess)
	if err != nil {
		t.Fatalf("stage transcript checkpoint: %v", err)
	}
	defer rollback()
	if got := sess.ToolExecutions[0].State; got != session.ToolExecutionCommitted {
		t.Fatalf("pre-dispatch checkpoint changed earlier write to %q", got)
	}
}

func TestSideEffectDispatchCrashIsNotAutoReplayed(t *testing.T) {
	if os.Getenv("SHANNON_SIDE_EFFECT_CRASH_HELPER") == "1" {
		runSideEffectCrashHelper()
		return
	}
	root := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestSideEffectDispatchCrashIsNotAutoReplayed$")
	cmd.Env = append(os.Environ(),
		"SHANNON_SIDE_EFFECT_CRASH_HELPER=1",
		"SHANNON_SIDE_EFFECT_CRASH_DIR="+root,
	)
	err := cmd.Run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 97 {
		t.Fatalf("helper exit = %v", err)
	}

	manager := session.NewManager(filepath.Join(root, "sessions"))
	t.Cleanup(func() { _ = manager.Close() })
	sess, err := manager.Resume("side-effect-crash-001")
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if got := sess.ToolExecutions[0].State; got != session.ToolExecutionDispatching {
		t.Fatalf("crash state = %q", got)
	}
	req := RunAgentRequest{
		ResumeInterrupted:                    true,
		InterruptedResumePriorAttempts:       0,
		InterruptedResumeCheckpointUpdatedAt: sess.InterruptedTurn.UpdatedAt,
	}
	err = claimInterruptedResume(manager, sess, &req, "")
	if !errors.Is(err, errInterruptedRecoveryReviewRequired) {
		t.Fatalf("claimInterruptedResume error = %v", err)
	}
	effect, err := os.ReadFile(filepath.Join(root, "external-effect.txt"))
	if err != nil {
		t.Fatalf("ReadFile effect: %v", err)
	}
	if string(effect) != "once" {
		t.Fatalf("effect = %q", effect)
	}
}

func runSideEffectCrashHelper() {
	root := os.Getenv("SHANNON_SIDE_EFFECT_CRASH_DIR")
	manager := session.NewManager(filepath.Join(root, "sessions"))
	sess := manager.NewSessionWithID("side-effect-crash-001")
	state := session.InterruptedTurn{
		RunID:     "run_crash_001",
		AttemptID: "attempt_crash_001",
		Source:    "desktop",
		UpdatedAt: time.Now(),
	}
	sess.InProgress = true
	sess.InterruptedTurn = &state
	if err := manager.Save(); err != nil {
		panic(err)
	}
	journal := newSessionSideEffectJournal(manager, sess, state.RunID, state.AttemptID)
	prepared, err := journal.Prepare(context.Background(), agent.SideEffectExecution{
		ToolUseID:       "tool-use-crash-001",
		ToolName:        "external_write",
		ArgumentsSHA256: session.ToolExecutionDigest(`{"value":"once"}`),
	})
	if err != nil {
		panic(err)
	}
	if err := journal.MarkDispatching(context.Background(), prepared.ExecutionID); err != nil {
		panic(err)
	}
	if err := os.WriteFile(filepath.Join(root, "external-effect.txt"), []byte("once"), 0o600); err != nil {
		panic(err)
	}
	os.Exit(97)
}
