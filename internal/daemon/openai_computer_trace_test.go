package daemon

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/audit"
)

func TestOpenAIComputerTraceV1WritesOnlyStructuredContentFreeFields(
	t *testing.T,
) {
	logDir := t.TempDir()
	logger, err := audit.NewAuditLogger(logDir)
	if err != nil {
		t.Fatal(err)
	}
	trace := newOpenAIComputerTraceV1(logger, daemonGUIWorkflowRequest{
		SessionID: "session-trace",
		TurnID:    "turn-trace",
	})
	trace.record(openAIComputerTraceWithCaptureDiagnosticsV1(openAIComputerTraceEventV1{
		Phase:              "action",
		Status:             "failed",
		BatchIndex:         2,
		ActionIndex:        3,
		ActionCount:        4,
		ActionType:         "type",
		AppBundleID:        "com.example.Editor",
		ExecutionLane:      "background_keyboard",
		ForegroundFallback: true,
		FallbackReason:     "frontmost_controller",
		FrontmostClass:     "controller",
		CommitState:        "not_committed",
		FailureCode:        "keyboard_target_unavailable",
		ModelCalls:         3,
		ModelTimeouts:      1,
		BatchCount:         2,
		DurationMS:         17,
	}, agent.ToolResult{
		GUICaptureDiagnostics: &agent.GUICaptureDiagnostics{
			Stage:              "decoded_dimensions",
			PID:                4242,
			BundleID:           "com.example.Editor",
			WindowID:           7001,
			PreWindowBounds:    agent.GUICaptureRect{X: 100, Y: 200, Width: 800, Height: 600},
			PostWindowBounds:   agent.GUICaptureRect{X: 100, Y: 200, Width: 800, Height: 600},
			DisplayID:          2,
			BackingScaleFactor: 2,
			ExpectedWidthPX:    1600,
			ExpectedHeightPX:   1200,
			MetadataWidthPX:    1512,
			MetadataHeightPX:   1134,
			DecodedWidthPX:     1512,
			DecodedHeightPX:    1134,
		},
	}))
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(logDir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Event        string `json:"event"`
		InputSummary string `json:"input_summary"`
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry struct {
			Event        string `json:"event"`
			InputSummary string `json:"input_summary"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if len(entries) != 2 {
		t.Fatalf("audit entries = %d, want 2: %s", len(entries), raw)
	}
	if entries[0].Event != openAIComputerTraceEventNameV1 {
		t.Fatalf("trace event = %q", entries[0].Event)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(entries[0].InputSummary), &payload); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"schema_version":      true,
		"phase":               true,
		"status":              true,
		"attempt":             true,
		"batch_index":         true,
		"action_index":        true,
		"action_count":        true,
		"action_type":         true,
		"app_bundle_id":       true,
		"execution_lane":      true,
		"foreground_fallback": true,
		"fallback_reason":     true,
		"frontmost_class":     true,
		"commit_state":        true,
		"failure_code":        true,
		"model_calls":         true,
		"model_timeouts":      true,
		"batch_count":         true,
		"duration_ms":         true,
	}
	for key := range payload {
		if !allowed[key] {
			t.Fatalf("trace emitted non-contract field %q: %s", key, entries[0].InputSummary)
		}
	}
	if payload["model_calls"] != float64(3) ||
		payload["model_timeouts"] != float64(1) ||
		payload["batch_count"] != float64(2) ||
		payload["execution_lane"] != "background_keyboard" ||
		payload["foreground_fallback"] != true ||
		payload["fallback_reason"] != "frontmost_controller" ||
		payload["frontmost_class"] != "controller" {
		t.Fatalf("trace lost provider counters: %s", entries[0].InputSummary)
	}
	for _, forbidden := range []string{
		"text", "coordinates", "window_title", "screenshot",
		"response_id", "call_id", "tool_use_id", "description",
	} {
		if _, found := payload[forbidden]; found {
			t.Fatalf("trace emitted content-bearing field %q", forbidden)
		}
	}
	if entries[1].Event != "computer_use_capture_diagnostic_v1" {
		t.Fatalf("diagnostic event = %q", entries[1].Event)
	}
	if len(entries[0].InputSummary) >= 500 ||
		len(entries[1].InputSummary) >= 500 {
		t.Fatalf(
			"structured trace exceeded audit summary limit: trace=%d diagnostic=%d",
			len(entries[0].InputSummary),
			len(entries[1].InputSummary),
		)
	}
	var diagnosticPayload map[string]any
	if err := json.Unmarshal(
		[]byte(entries[1].InputSummary),
		&diagnosticPayload,
	); err != nil {
		t.Fatal(err)
	}
	expectedSize, _ := diagnosticPayload["expected_size_px"].([]any)
	metadataSize, _ := diagnosticPayload["metadata_size_px"].([]any)
	if len(expectedSize) != 2 || expectedSize[0] != float64(1600) ||
		len(metadataSize) != 2 || metadataSize[0] != float64(1512) ||
		diagnosticPayload["target_window_id"] != float64(7001) {
		t.Fatalf(
			"trace lost capture diagnostics: %s",
			entries[1].InputSummary,
		)
	}
	for _, forbidden := range []string{
		"pre_window_x", "pre_window_y", "post_window_x", "post_window_y",
	} {
		if _, found := diagnosticPayload[forbidden]; found {
			t.Fatalf(
				"trace retained a desktop coordinate %q: %s",
				forbidden,
				entries[1].InputSummary,
			)
		}
	}
}

func TestOpenAIComputerTraceV1WritesBoundedDisplayPredicateDiagnostics(
	t *testing.T,
) {
	logDir := t.TempDir()
	logger, err := audit.NewAuditLogger(logDir)
	if err != nil {
		t.Fatal(err)
	}
	trace := newOpenAIComputerTraceV1(logger, daemonGUIWorkflowRequest{
		SessionID: "session-display-diagnostics",
		TurnID:    "turn-display-diagnostics",
	})
	trace.record(openAIComputerTraceWithCaptureDiagnosticsV1(
		openAIComputerTraceEventV1{
			Phase:       "initial_observation",
			Status:      "failed",
			Attempt:     1,
			AppBundleID: "com.example.Editor",
			FailureCode: "display_not_actionable",
			DurationMS:  17,
		},
		agent.ToolResult{
			GUICaptureDiagnostics: &agent.GUICaptureDiagnostics{
				Stage:                  "display_actionability",
				PID:                    4242,
				BundleID:               "com.example.Editor",
				WindowID:               7001,
				PreWindowBounds:        agent.GUICaptureRect{X: -100, Y: 200, Width: 800, Height: 600},
				PostWindowBounds:       agent.GUICaptureRect{X: -100, Y: 200, Width: 800, Height: 600},
				ActionableDisplayCount: 0,
				DisplayCandidates: []agent.GUIDisplayActionabilityDiagnostics{
					{
						DisplayID:        1,
						QuartzBounds:     agent.GUICaptureRect{X: 0, Y: 0, Width: 1280, Height: 800},
						IsActive:         true,
						IsOnline:         true,
						FailedPredicates: []string{"does_not_fully_contain_window"},
					},
					{
						DisplayID:           2,
						QuartzBounds:        agent.GUICaptureRect{X: -1600, Y: 100, Width: 1600, Height: 900},
						IsOnline:            true,
						FullyContainsWindow: true,
						FailedPredicates:    []string{"inactive"},
					},
				},
			},
		},
	))
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(logDir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	var entries []struct {
		Event        string `json:"event"`
		InputSummary string `json:"input_summary"`
	}
	for _, line := range strings.Split(strings.TrimSpace(string(raw)), "\n") {
		var entry struct {
			Event        string `json:"event"`
			InputSummary string `json:"input_summary"`
		}
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry)
	}
	if len(entries) != 4 {
		t.Fatalf("display diagnostic entries = %d, want 4: %s", len(entries), raw)
	}
	if entries[1].Event != "computer_use_display_diagnostic_v1" {
		t.Fatalf("display header event = %q", entries[1].Event)
	}
	var header map[string]any
	if err := json.Unmarshal([]byte(entries[1].InputSummary), &header); err != nil {
		t.Fatal(err)
	}
	targetBounds, _ := header["target_window_bounds"].([]any)
	if len(targetBounds) != 4 || targetBounds[0] != float64(-100) ||
		header["actionable_display_count"] != float64(0) {
		t.Fatalf("display header diagnostics = %s", entries[1].InputSummary)
	}
	for index, entry := range entries[2:] {
		if entry.Event != "computer_use_display_candidate_diagnostic_v1" {
			t.Fatalf("candidate %d event = %q", index, entry.Event)
		}
		if len(entry.InputSummary) >= 500 {
			t.Fatalf(
				"candidate %d exceeded audit summary limit: %d",
				index,
				len(entry.InputSummary),
			)
		}
		var candidate map[string]any
		if err := json.Unmarshal([]byte(entry.InputSummary), &candidate); err != nil {
			t.Fatal(err)
		}
		if _, found := candidate["window_title"]; found {
			t.Fatal("display diagnostics retained window content")
		}
	}
}

func TestOpenAIComputerTraceV1ExtractsNestedScreenshotFailureCode(
	t *testing.T,
) {
	result := agent.ToolResult{
		Content: "state_id: private\nscreenshot_warning: " +
			"[business error] computer_use_error: window_not_found\n" +
			"message: no normal window is ready",
	}
	if got := openAIComputerTraceFailureCodeV1(result, nil); got !=
		"window_not_found" {
		t.Fatalf("failure code = %q", got)
	}
}

func TestOpenAIComputerObservationRetrySkipsPermissionFailure(
	t *testing.T,
) {
	result := agent.PermissionError("observe app: Screen Recording permission denied")
	if retryOpenAIComputerObservationV1(result, nil) {
		t.Fatal("permission failure was treated as cold-app readiness")
	}
}

func TestOpenAIComputerObservationRetryAllowsTransientImageDimensionDrift(
	t *testing.T,
) {
	result := agent.ToolResult{
		Content: "state_id: private\nscreenshot_warning: " +
			"[transient error] computer_use_error: image_dimensions_mismatch\n" +
			"message: the exact target window capture was rejected",
		IsError:     true,
		IsRetryable: true,
	}
	if !retryOpenAIComputerObservationV1(result, nil) {
		t.Fatal("transient window image dimension drift was not retried")
	}
}

func TestOpenAIComputerObservationPreservesVisualOnlyActionabilityFailure(
	t *testing.T,
) {
	result := agent.ToolResult{
		Images: []agent.ImageBlock{{
			MediaType: "image/png",
			Data:      "dmlzdWFsLW9ubHk=",
		}},
		GUIObservation: &agent.GUIObservationOutcome{
			ActionabilityFailureCode: "display_not_actionable",
		},
	}
	if got := openAIComputerTraceFailureCodeV1(result, nil); got !=
		"display_not_actionable" {
		t.Fatalf("failure code = %q", got)
	}
	if got := openAIComputerObservationResultDetailV1(result); got !=
		"display_not_actionable: the target window was not fully contained in exactly one active, online, awake, unmirrored, unrotated display" {
		t.Fatalf("observation detail = %q", got)
	}
	if retryOpenAIComputerObservationV1(result, nil) {
		t.Fatal("deterministic visual-only display geometry was retried")
	}

	result.GUIObservation.ActionabilityFailureCode =
		"image_dimensions_mismatch"
	result.IsRetryable = true
	if !retryOpenAIComputerObservationV1(result, nil) {
		t.Fatal("transient visual-only image dimension drift was not retried")
	}
}
