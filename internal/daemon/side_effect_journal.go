package daemon

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// sessionSideEffectJournal persists the state surrounding one material
// Tool.Run in the authoritative session JSON. It deliberately stores only
// opaque identities and digests; tool arguments and results stay in memory.
type sessionSideEffectJournal struct {
	mu        sync.Mutex
	manager   *session.Manager
	session   *session.Session
	runID     string
	attemptID string
}

func newSessionSideEffectJournal(
	manager *session.Manager,
	sess *session.Session,
	runID string,
	attemptID string,
) *sessionSideEffectJournal {
	return &sessionSideEffectJournal{
		manager:   manager,
		session:   sess,
		runID:     runID,
		attemptID: attemptID,
	}
}

func (j *sessionSideEffectJournal) Prepare(
	_ context.Context,
	execution agent.SideEffectExecution,
) (agent.PreparedSideEffectExecution, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.validate(); err != nil {
		return agent.PreparedSideEffectExecution{}, err
	}
	record, err := session.NewToolExecutionRecordFromDigest(
		j.runID,
		j.attemptID,
		execution.ToolName,
		execution.ToolUseID,
		execution.ArgumentsSHA256,
		time.Now(),
	)
	if err != nil {
		return agent.PreparedSideEffectExecution{}, err
	}
	before := cloneToolExecutions(j.session.ToolExecutions)
	if err := j.session.AddToolExecution(record); err != nil {
		return agent.PreparedSideEffectExecution{}, err
	}
	if err := j.manager.Save(); err != nil {
		j.session.ToolExecutions = before
		return agent.PreparedSideEffectExecution{}, fmt.Errorf("persist prepared side-effect execution: %w", err)
	}
	return agent.PreparedSideEffectExecution{
		ExecutionID:    record.ExecutionID,
		IdempotencyKey: record.IdempotencyKey,
	}, nil
}

func (j *sessionSideEffectJournal) MarkDispatching(_ context.Context, executionID string) error {
	return j.transition(executionID, func() error {
		return j.session.MarkToolExecutionDispatching(executionID, time.Now())
	})
}

func (j *sessionSideEffectJournal) MarkCommitted(_ context.Context, executionID, resultDigest string) error {
	return j.transition(executionID, func() error {
		return j.session.MarkToolExecutionCommitted(executionID, resultDigest, time.Now())
	})
}

func (j *sessionSideEffectJournal) MarkAbandoned(_ context.Context, executionID, _ string) error {
	return j.transition(executionID, func() error {
		return j.session.AbandonToolExecution(executionID, time.Now())
	})
}

func (j *sessionSideEffectJournal) MarkOutcomeUnknown(_ context.Context, executionID, resultDigest string) error {
	return j.transition(executionID, func() error {
		return j.session.MarkToolExecutionOutcomeUnknown(executionID, resultDigest, time.Now())
	})
}

func (j *sessionSideEffectJournal) transition(executionID string, mutate func() error) error {
	j.mu.Lock()
	defer j.mu.Unlock()
	if err := j.validate(); err != nil {
		return err
	}
	before := cloneToolExecutions(j.session.ToolExecutions)
	if err := mutate(); err != nil {
		return err
	}
	if err := j.manager.Save(); err != nil {
		j.session.ToolExecutions = before
		return fmt.Errorf("persist side-effect execution %s: %w", executionID, err)
	}
	return nil
}

func (j *sessionSideEffectJournal) validate() error {
	if j == nil || j.manager == nil || j.session == nil {
		return fmt.Errorf("side-effect journal is unavailable")
	}
	if j.runID == "" || j.attemptID == "" {
		return fmt.Errorf("side-effect journal has no run identity")
	}
	return nil
}

func cloneToolExecutions(records []session.ToolExecutionRecord) []session.ToolExecutionRecord {
	return append([]session.ToolExecutionRecord(nil), records...)
}

func stageCommittedToolExecutionsForSave(sess *session.Session, runID string) func() {
	if sess == nil {
		return func() {}
	}
	before := cloneToolExecutions(sess.ToolExecutions)
	sess.CheckpointCommittedToolExecutions(runID, time.Now())
	return func() {
		sess.ToolExecutions = before
	}
}

func stageCommittedToolExecutionsForCheckpoint(
	reason agent.CheckpointReason,
	sess *session.Session,
	runID string,
) func() {
	if reason == agent.CheckpointReasonSideEffectPrepared {
		return func() {}
	}
	return stageCommittedToolExecutionsForSave(sess, runID)
}

func guardInterruptedToolExecutions(
	manager *session.Manager,
	sess *session.Session,
	state session.InterruptedTurn,
) error {
	if manager == nil || sess == nil {
		return fmt.Errorf("inspect interrupted side-effect executions: session unavailable")
	}
	if err := sess.ValidateToolExecutions(); err != nil {
		return fmt.Errorf("inspect interrupted side-effect executions: %w", err)
	}
	appendInterruptedSyntheticToolResults(sess, state.RunID)
	sess.AbandonPreparedToolExecutions(state.RunID, time.Now())
	blocked := sess.BlockingToolExecutions(state.RunID)
	if len(blocked) == 0 {
		return nil
	}
	for _, record := range blocked {
		switch record.State {
		case session.ToolExecutionDispatching, session.ToolExecutionCommitted:
			if err := sess.MarkToolExecutionOutcomeUnknown(record.ExecutionID, record.ResultDigest, time.Now()); err != nil {
				return fmt.Errorf("mark interrupted side-effect execution uncertain: %w", err)
			}
		}
	}

	const warning = "The previous turn stopped after an external action was dispatched but before its result was durably recorded. The action was not retried. Review the external system before retrying it."
	if !hasSideEffectReviewWarning(sess.Messages) {
		sess.Messages = append(sess.Messages, client.Message{
			Role:    "assistant",
			Content: client.NewTextContent(warning),
		})
		source := state.Source
		if source == "" {
			source = "local"
		}
		sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{
			Source:         source,
			Timestamp:      session.TimePtr(time.Now()),
			SystemInjected: true,
		})
	}
	sess.InProgress = false
	sess.InterruptedTurn = nil
	if err := manager.SavePreservingUpdatedAt(); err != nil {
		return fmt.Errorf("persist interrupted side-effect review boundary: %w", err)
	}
	return errInterruptedRecoveryReviewRequired
}

func appendInterruptedSyntheticToolResults(sess *session.Session, runID string) {
	if sess == nil {
		return
	}
	recordsByToolUseDigest := make(map[string]session.ToolExecutionRecord)
	for _, record := range sess.ToolExecutions {
		if runID == "" || record.RunID == runID {
			recordsByToolUseDigest[record.ToolUseIDDigest] = record
		}
	}
	completed := make(map[string]struct{})
	for _, message := range sess.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type == "tool_result" && block.ToolUseID != "" {
				completed[block.ToolUseID] = struct{}{}
			}
		}
	}

	var synthetic []client.ContentBlock
	for _, message := range sess.Messages {
		for _, block := range message.Content.Blocks() {
			if block.Type != "tool_use" || block.ID == "" {
				continue
			}
			if _, ok := completed[block.ID]; ok {
				continue
			}
			content := "The tool call did not reach a durably recorded side-effect dispatch before the process stopped. It is safe to continue; run it again only if it is still needed."
			if record, ok := recordsByToolUseDigest[session.ToolExecutionDigest(block.ID)]; ok {
				switch record.State {
				case session.ToolExecutionDispatching,
					session.ToolExecutionCommitted,
					session.ToolExecutionOutcomeUnknown:
					content = "The tool call was dispatched, but its outcome could not be durably confirmed. It was not retried. Review the external system before retrying it."
				}
			}
			synthetic = append(synthetic, client.NewToolResultBlock(block.ID, content, true))
			completed[block.ID] = struct{}{}
		}
	}
	if len(synthetic) == 0 {
		return
	}
	sess.Messages = append(sess.Messages, client.Message{
		Role:    "user",
		Content: client.NewBlockContent(synthetic),
	})
	sess.MessageMeta = append(sess.MessageMeta, session.MessageMeta{
		Source:         "local",
		Timestamp:      session.TimePtr(time.Now()),
		SystemInjected: true,
	})
}

func hasSideEffectReviewWarning(messages []client.Message) bool {
	const marker = "Review the external system before retrying"
	start := len(messages) - 3
	if start < 0 {
		start = 0
	}
	for _, message := range messages[start:] {
		if message.Role == "assistant" && strings.Contains(message.Content.Text(), marker) {
			return true
		}
	}
	return false
}

var _ agent.SideEffectExecutionJournal = (*sessionSideEffectJournal)(nil)
