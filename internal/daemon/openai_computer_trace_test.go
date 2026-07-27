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
	trace.record(openAIComputerTraceEventV1{
		Phase:       "action",
		Status:      "failed",
		BatchIndex:  2,
		ActionIndex: 3,
		ActionCount: 4,
		ActionType:  "type",
		AppBundleID: "com.example.Editor",
		CommitState: "not_committed",
		FailureCode: "keyboard_target_unavailable",
		DurationMS:  17,
	})
	if err := logger.Close(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(filepath.Join(logDir, "audit.log"))
	if err != nil {
		t.Fatal(err)
	}
	var entry struct {
		Event        string `json:"event"`
		InputSummary string `json:"input_summary"`
	}
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(raw))), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Event != openAIComputerTraceEventNameV1 {
		t.Fatalf("event = %q", entry.Event)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(entry.InputSummary), &payload); err != nil {
		t.Fatal(err)
	}
	allowed := map[string]bool{
		"schema_version": true,
		"phase":          true,
		"status":         true,
		"attempt":        true,
		"batch_index":    true,
		"action_index":   true,
		"action_count":   true,
		"action_type":    true,
		"app_bundle_id":  true,
		"commit_state":   true,
		"failure_code":   true,
		"duration_ms":    true,
	}
	for key := range payload {
		if !allowed[key] {
			t.Fatalf("trace emitted non-contract field %q: %s", key, entry.InputSummary)
		}
	}
	for _, forbidden := range []string{
		"text", "coordinates", "window_title", "screenshot",
		"response_id", "call_id", "tool_use_id", "description",
	} {
		if _, found := payload[forbidden]; found {
			t.Fatalf("trace emitted content-bearing field %q", forbidden)
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

func TestOpenAIComputerInitialObservationRetrySkipsPermissionFailure(
	t *testing.T,
) {
	result := agent.PermissionError("observe app: Screen Recording permission denied")
	if retryOpenAIComputerInitialObservationV1(result, nil, nil) {
		t.Fatal("permission failure was treated as cold-app readiness")
	}
}
