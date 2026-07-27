package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
)

const openAIComputerTraceEventNameV1 = "computer_use_trace_v1"

// openAIComputerTraceEventV1 is deliberately content-free. It records the
// executor control path and timing without model text, typed text, coordinates,
// AX values, window titles, screenshots, response IDs, or provider call IDs.
type openAIComputerTraceEventV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Phase         string `json:"phase"`
	Status        string `json:"status"`
	Attempt       int    `json:"attempt,omitempty"`
	BatchIndex    int    `json:"batch_index,omitempty"`
	ActionIndex   int    `json:"action_index,omitempty"`
	ActionCount   int    `json:"action_count,omitempty"`
	ActionType    string `json:"action_type,omitempty"`
	AppBundleID   string `json:"app_bundle_id,omitempty"`
	CommitState   string `json:"commit_state,omitempty"`
	FailureCode   string `json:"failure_code,omitempty"`
	DurationMS    int64  `json:"duration_ms"`
}

type openAIComputerTraceV1 struct {
	auditor   *audit.AuditLogger
	sessionID string
}

func newOpenAIComputerTraceV1(
	auditor *audit.AuditLogger,
	request daemonGUIWorkflowRequest,
) *openAIComputerTraceV1 {
	if auditor == nil {
		return nil
	}
	return &openAIComputerTraceV1{
		auditor:   auditor,
		sessionID: request.SessionID,
	}
}

func (t *openAIComputerTraceV1) record(event openAIComputerTraceEventV1) {
	if t == nil || t.auditor == nil {
		return
	}
	event.SchemaVersion = 1
	payload, err := json.Marshal(event)
	if err != nil {
		return
	}
	t.auditor.Log(audit.AuditEntry{
		Timestamp:    time.Now(),
		SessionID:    t.sessionID,
		Event:        openAIComputerTraceEventNameV1,
		InputSummary: string(payload),
	})
}

func openAIComputerTraceStatusV1(result agent.ToolResult, err error) string {
	if err != nil || result.IsError {
		return "failed"
	}
	return "completed"
}

func openAIComputerTraceFailureCodeV1(
	result agent.ToolResult,
	err error,
) string {
	if result.GUIOutcome != nil &&
		strings.TrimSpace(result.GUIOutcome.FailureCode) != "" {
		return strings.TrimSpace(result.GUIOutcome.FailureCode)
	}
	for _, line := range strings.Split(result.Content, "\n") {
		line = strings.TrimSpace(line)
		if marker := strings.Index(line, "computer_use_error:"); marker >= 0 {
			return strings.TrimSpace(
				line[marker+len("computer_use_error:"):],
			)
		}
	}
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline_exceeded"
	case errors.Is(err, context.Canceled):
		return "cancelled"
	case err != nil:
		return "executor_error"
	case result.IsError:
		return "tool_error"
	default:
		return ""
	}
}
