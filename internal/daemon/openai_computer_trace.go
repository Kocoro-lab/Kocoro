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
	ModelCalls    int    `json:"model_calls,omitempty"`
	ModelTimeouts int    `json:"model_timeouts,omitempty"`
	BatchCount    int    `json:"batch_count,omitempty"`
	DurationMS    int64  `json:"duration_ms"`

	// Capture diagnostics are written as a separate structured audit event.
	// Keeping them out of this JSON prevents the audit logger's intentionally
	// small summary limit from truncating either object into invalid JSON.
	captureDiagnostics *agent.GUICaptureDiagnostics
}

type openAIComputerCaptureDiagnosticEventV1 struct {
	SchemaVersion      int        `json:"schema_version"`
	Phase              string     `json:"phase"`
	Attempt            int        `json:"attempt,omitempty"`
	BatchIndex         int        `json:"batch_index,omitempty"`
	ActionIndex        int        `json:"action_index,omitempty"`
	AppBundleID        string     `json:"app_bundle_id,omitempty"`
	CaptureStage       string     `json:"capture_stage"`
	TargetPID          int        `json:"target_pid"`
	TargetWindowID     uint32     `json:"target_window_id"`
	DisplayID          uint32     `json:"display_id"`
	BackingScaleFactor float64    `json:"backing_scale_factor"`
	ExpectedSizePX     [2]float64 `json:"expected_size_px"`
	MetadataSizePX     [2]int     `json:"metadata_size_px"`
	DecodedSizePX      [2]int     `json:"decoded_size_px"`
	PreWindowSize      [2]float64 `json:"pre_window_size"`
	PostWindowSize     [2]float64 `json:"post_window_size"`
}

type openAIComputerDisplayDiagnosticEventV1 struct {
	SchemaVersion          int        `json:"schema_version"`
	Phase                  string     `json:"phase"`
	Attempt                int        `json:"attempt,omitempty"`
	BatchIndex             int        `json:"batch_index,omitempty"`
	ActionIndex            int        `json:"action_index,omitempty"`
	AppBundleID            string     `json:"app_bundle_id,omitempty"`
	CaptureStage           string     `json:"capture_stage"`
	TargetPID              int        `json:"target_pid"`
	TargetWindowID         uint32     `json:"target_window_id"`
	TargetWindowBounds     [4]float64 `json:"target_window_bounds"`
	ActionableDisplayCount int        `json:"actionable_display_count"`
}

type openAIComputerDisplayCandidateDiagnosticEventV1 struct {
	SchemaVersion    int        `json:"schema_version"`
	TargetPID        int        `json:"target_pid"`
	TargetWindowID   uint32     `json:"target_window_id"`
	DisplayID        uint32     `json:"display_id"`
	DisplayBounds    [4]float64 `json:"display_bounds"`
	FailedPredicates []string   `json:"failed_predicates"`
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
	diagnostics := event.captureDiagnostics
	event.captureDiagnostics = nil
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
	if diagnostics == nil {
		return
	}
	if diagnostics.Stage == "display_actionability" {
		t.recordDisplayDiagnostics(event, diagnostics)
		return
	}
	diagnosticEvent := openAIComputerCaptureDiagnosticEventV1{
		SchemaVersion:      1,
		Phase:              event.Phase,
		Attempt:            event.Attempt,
		BatchIndex:         event.BatchIndex,
		ActionIndex:        event.ActionIndex,
		AppBundleID:        event.AppBundleID,
		CaptureStage:       diagnostics.Stage,
		TargetPID:          diagnostics.PID,
		TargetWindowID:     diagnostics.WindowID,
		DisplayID:          diagnostics.DisplayID,
		BackingScaleFactor: diagnostics.BackingScaleFactor,
		ExpectedSizePX: [2]float64{
			diagnostics.ExpectedWidthPX,
			diagnostics.ExpectedHeightPX,
		},
		MetadataSizePX: [2]int{
			diagnostics.MetadataWidthPX,
			diagnostics.MetadataHeightPX,
		},
		DecodedSizePX: [2]int{
			diagnostics.DecodedWidthPX,
			diagnostics.DecodedHeightPX,
		},
		PreWindowSize: [2]float64{
			diagnostics.PreWindowBounds.Width,
			diagnostics.PreWindowBounds.Height,
		},
		PostWindowSize: [2]float64{
			diagnostics.PostWindowBounds.Width,
			diagnostics.PostWindowBounds.Height,
		},
	}
	if diagnosticEvent.AppBundleID == "" {
		diagnosticEvent.AppBundleID = diagnostics.BundleID
	}
	diagnosticPayload, err := json.Marshal(diagnosticEvent)
	if err != nil {
		return
	}
	t.auditor.Log(audit.AuditEntry{
		Timestamp:    time.Now(),
		SessionID:    t.sessionID,
		Event:        "computer_use_capture_diagnostic_v1",
		InputSummary: string(diagnosticPayload),
	})
}

func (t *openAIComputerTraceV1) recordDisplayDiagnostics(
	event openAIComputerTraceEventV1,
	diagnostics *agent.GUICaptureDiagnostics,
) {
	header := openAIComputerDisplayDiagnosticEventV1{
		SchemaVersion:  1,
		Phase:          event.Phase,
		Attempt:        event.Attempt,
		BatchIndex:     event.BatchIndex,
		ActionIndex:    event.ActionIndex,
		AppBundleID:    event.AppBundleID,
		CaptureStage:   diagnostics.Stage,
		TargetPID:      diagnostics.PID,
		TargetWindowID: diagnostics.WindowID,
		TargetWindowBounds: [4]float64{
			diagnostics.PreWindowBounds.X,
			diagnostics.PreWindowBounds.Y,
			diagnostics.PreWindowBounds.Width,
			diagnostics.PreWindowBounds.Height,
		},
		ActionableDisplayCount: diagnostics.ActionableDisplayCount,
	}
	if header.AppBundleID == "" {
		header.AppBundleID = diagnostics.BundleID
	}
	t.recordDiagnosticPayload(
		"computer_use_display_diagnostic_v1",
		header,
	)
	for _, display := range diagnostics.DisplayCandidates {
		t.recordDiagnosticPayload(
			"computer_use_display_candidate_diagnostic_v1",
			openAIComputerDisplayCandidateDiagnosticEventV1{
				SchemaVersion:  1,
				TargetPID:      diagnostics.PID,
				TargetWindowID: diagnostics.WindowID,
				DisplayID:      display.DisplayID,
				DisplayBounds: [4]float64{
					display.QuartzBounds.X,
					display.QuartzBounds.Y,
					display.QuartzBounds.Width,
					display.QuartzBounds.Height,
				},
				FailedPredicates: append(
					[]string(nil),
					display.FailedPredicates...,
				),
			},
		)
	}
}

func (t *openAIComputerTraceV1) recordDiagnosticPayload(
	eventName string,
	payload any,
) {
	diagnosticPayload, err := json.Marshal(payload)
	if err != nil {
		return
	}
	t.auditor.Log(audit.AuditEntry{
		Timestamp:    time.Now(),
		SessionID:    t.sessionID,
		Event:        eventName,
		InputSummary: string(diagnosticPayload),
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
	if result.GUIObservation != nil &&
		strings.TrimSpace(
			result.GUIObservation.ActionabilityFailureCode,
		) != "" {
		return strings.TrimSpace(
			result.GUIObservation.ActionabilityFailureCode,
		)
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

func openAIComputerTraceWithCaptureDiagnosticsV1(
	event openAIComputerTraceEventV1,
	result agent.ToolResult,
) openAIComputerTraceEventV1 {
	diagnostics := result.GUICaptureDiagnostics
	if diagnostics == nil {
		return event
	}
	if event.AppBundleID == "" {
		event.AppBundleID = diagnostics.BundleID
	}
	event.captureDiagnostics = diagnostics
	return event
}
