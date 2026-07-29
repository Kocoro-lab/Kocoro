package e2e

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

type computerUseAcceptanceManifestV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Suite         string `json:"suite"`
	RunPolicy     struct {
		ManualOnly            bool   `json:"manual_only"`
		PaidModelCalls        bool   `json:"paid_model_calls"`
		OnePointerOperator    bool   `json:"one_pointer_operator"`
		SecondaryDisplayPause bool   `json:"secondary_display_paused"`
		DefaultTimeoutSeconds int    `json:"default_timeout_seconds"`
		MaxFixCycles          int    `json:"max_fix_cycles_per_checkpoint"`
		ReproductionRequired  int    `json:"non_safety_reproduction_required"`
		OutOfScopeBehavior    string `json:"out_of_scope_behavior"`
	} `json:"run_policy"`
	PerformanceBaseline struct {
		CapturedAt         string `json:"captured_at"`
		Kind               string `json:"kind"`
		SampleSize         int    `json:"sample_size"`
		DurationP50MS      int    `json:"duration_p50_ms"`
		DurationP95MS      int    `json:"duration_p95_ms"`
		ModelCallsP50      int    `json:"model_calls_p50"`
		ModelCallsP95      int    `json:"model_calls_p95"`
		ProviderBatchesP50 int    `json:"provider_batches_p50"`
		ProviderBatchesP95 int    `json:"provider_batches_p95"`
		ModelTimeouts      int    `json:"model_timeouts"`
	} `json:"performance_baseline"`
	EvidencePolicy struct {
		SummaryScope                          string   `json:"summary_scope"`
		PassedMutationRequiresCommittedAction bool     `json:"passed_mutation_requires_committed_action"`
		ObservationOnlyCompletion             string   `json:"observation_only_completion"`
		ProviderActionMismatch                string   `json:"provider_action_mismatch"`
		FixBoundary                           string   `json:"fix_boundary"`
		FailureClasses                        []string `json:"failure_classes"`
	} `json:"evidence_policy"`
	Scenarios []struct {
		ID                string   `json:"id"`
		EnabledByDefault  bool     `json:"enabled_by_default"`
		Prompt            string   `json:"prompt"`
		Preconditions     []string `json:"preconditions"`
		ExpectedState     []string `json:"expected_state"`
		Risk              string   `json:"risk"`
		Tags              []string `json:"tags"`
		PerformanceBudget struct {
			DurationMS      int `json:"duration_ms"`
			ModelCalls      int `json:"model_calls"`
			ProviderBatches int `json:"provider_batches"`
		} `json:"performance_budget"`
	} `json:"scenarios"`
}

func TestOffline_ComputerUseAcceptanceManifestIsFrozenAndSafe(t *testing.T) {
	path := filepath.Join(
		repoRoot(),
		"test",
		"e2e",
		"testdata",
		"computer_use_acceptance_manifest.v1.json",
	)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var manifest computerUseAcceptanceManifestV1
	if err := json.Unmarshal(raw, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.SchemaVersion != 1 ||
		manifest.Suite != "computer_use_core_acceptance" {
		t.Fatalf("unexpected manifest identity: %+v", manifest)
	}
	if !manifest.RunPolicy.ManualOnly ||
		!manifest.RunPolicy.PaidModelCalls ||
		!manifest.RunPolicy.OnePointerOperator ||
		!manifest.RunPolicy.SecondaryDisplayPause ||
		manifest.RunPolicy.DefaultTimeoutSeconds != 120 ||
		manifest.RunPolicy.MaxFixCycles != 1 ||
		manifest.RunPolicy.ReproductionRequired != 2 ||
		manifest.RunPolicy.OutOfScopeBehavior != "record_only" {
		t.Fatalf("unsafe or drifting run policy: %+v", manifest.RunPolicy)
	}
	if manifest.PerformanceBaseline.CapturedAt == "" ||
		manifest.PerformanceBaseline.Kind != "mixed_completed_trace_warning_only" ||
		manifest.PerformanceBaseline.SampleSize != 15 ||
		manifest.PerformanceBaseline.DurationP50MS != 29825 ||
		manifest.PerformanceBaseline.DurationP95MS != 66485 ||
		manifest.PerformanceBaseline.ModelCallsP50 != 6 ||
		manifest.PerformanceBaseline.ModelCallsP95 != 15 ||
		manifest.PerformanceBaseline.ProviderBatchesP50 != 5 ||
		manifest.PerformanceBaseline.ProviderBatchesP95 != 14 ||
		manifest.PerformanceBaseline.ModelTimeouts != 1 {
		t.Fatalf(
			"missing or drifting computer-use baseline: %+v",
			manifest.PerformanceBaseline,
		)
	}
	if manifest.EvidencePolicy.SummaryScope != "latest_task_boundary" ||
		!manifest.EvidencePolicy.PassedMutationRequiresCommittedAction ||
		manifest.EvidencePolicy.ObservationOnlyCompletion != "inconclusive" ||
		manifest.EvidencePolicy.ProviderActionMismatch !=
			"classify_before_runtime_change" ||
		manifest.EvidencePolicy.FixBoundary != "one_bounded_cycle" {
		t.Fatalf("unsafe or drifting evidence policy: %+v", manifest.EvidencePolicy)
	}
	failureClasses := make(
		map[string]struct{},
		len(manifest.EvidencePolicy.FailureClasses),
	)
	for _, class := range manifest.EvidencePolicy.FailureClasses {
		failureClasses[class] = struct{}{}
	}
	for _, required := range []string{
		"public_capability_regression",
		"unsupported_provider_action",
		"invalid_precondition_or_stale_state",
	} {
		if _, found := failureClasses[required]; !found {
			t.Fatalf("evidence policy is missing failure class %q", required)
		}
	}
	if len(manifest.Scenarios) < 7 {
		t.Fatalf("scenario count = %d", len(manifest.Scenarios))
	}
	seen := make(map[string]struct{}, len(manifest.Scenarios))
	for _, scenario := range manifest.Scenarios {
		if strings.TrimSpace(scenario.ID) == "" ||
			strings.TrimSpace(scenario.Prompt) == "" ||
			len(scenario.Preconditions) == 0 ||
			len(scenario.ExpectedState) == 0 ||
			len(scenario.Tags) == 0 ||
			scenario.PerformanceBudget.DurationMS <= 0 ||
			scenario.PerformanceBudget.ModelCalls <= 0 ||
			scenario.PerformanceBudget.ProviderBatches <= 0 {
			t.Fatalf("incomplete scenario: %+v", scenario)
		}
		if _, duplicate := seen[scenario.ID]; duplicate {
			t.Fatalf("duplicate scenario id %q", scenario.ID)
		}
		seen[scenario.ID] = struct{}{}
		if scenario.Risk == "consequential_send" && scenario.EnabledByDefault {
			t.Fatalf("consequential scenario %q is enabled by default", scenario.ID)
		}
	}
	for _, required := range []string{
		"calculator_cold_launch",
		"calculator_warm_window",
		"chrome_cold_url_navigation",
		"chrome_multi_window_url_navigation",
		"managed_browser_instance_coexistence",
		"textedit_calculator_cross_app",
		"slack_draft_without_send",
		"slack_send_with_confirmation",
		"pointer_interference",
		"textedit_background_scroll_preview",
		"background_required_drag_rejection",
		"pause_resume_during_background_input",
		"take_over_during_foreground_action",
		"stop_during_wait",
	} {
		if _, found := seen[required]; !found {
			t.Fatalf("required scenario %q is missing", required)
		}
	}
	for _, scenario := range manifest.Scenarios {
		expected := strings.Join(scenario.ExpectedState, "\n")
		switch scenario.ID {
		case "calculator_background_semantic":
			if !strings.Contains(
				expected,
				"four committed click actions",
			) {
				t.Fatalf("%s lacks action evidence", scenario.ID)
			}
		case "textedit_background_targeted_keyboard":
			if !strings.Contains(
				expected,
				"committed type action",
			) {
				t.Fatalf("%s lacks action evidence", scenario.ID)
			}
		case "textedit_background_scroll_preview":
			if !strings.Contains(
				expected,
				"committed scroll action",
			) || !strings.Contains(
				expected,
				"unsupported_provider_action",
			) {
				t.Fatalf("%s lacks action-variance evidence", scenario.ID)
			}
		case "background_required_drag_rejection":
			if !strings.Contains(
				expected,
				"not_committed drag action",
			) {
				t.Fatalf("%s lacks pre-commit rejection evidence", scenario.ID)
			}
		}
	}
}

func TestOffline_ComputerUseTraceSummaryIsContentFreeAndComplete(t *testing.T) {
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq is required by scripts/computer-use-trace.sh")
	}
	auditLog := filepath.Join(t.TempDir(), "audit.log")
	rows := []string{
		`{"timestamp":"2026-07-28T00:00:00Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"task\",\"status\":\"started\",\"duration_ms\":0}"}`,
		`{"timestamp":"2026-07-28T00:00:01Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"action\",\"status\":\"failed\",\"batch_index\":1,\"action_index\":1,\"action_count\":1,\"action_type\":\"drag\",\"commit_state\":\"not_committed\",\"failure_code\":\"old_task_failure\",\"duration_ms\":10}"}`,
		`{"timestamp":"2026-07-28T00:00:02Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"task\",\"status\":\"failed\",\"failure_code\":\"old_task_failure\",\"duration_ms\":20}"}`,
		`{"timestamp":"2026-07-28T00:00:00Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"task\",\"status\":\"started\",\"duration_ms\":0}"}`,
		`{"timestamp":"2026-07-28T00:00:01Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"initial_observation\",\"status\":\"completed\",\"attempt\":1,\"duration_ms\":10}"}`,
		`{"timestamp":"2026-07-28T00:00:02Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"action\",\"status\":\"completed\",\"batch_index\":1,\"action_index\":1,\"action_count\":3,\"action_type\":\"click\",\"commit_state\":\"committed_unverified\",\"failure_code\":\"click_postcondition_not_declared\",\"duration_ms\":20}"}`,
		`{"timestamp":"2026-07-28T00:00:02Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"action\",\"status\":\"failed\",\"batch_index\":1,\"action_index\":2,\"action_count\":3,\"action_type\":\"type\",\"commit_state\":\"commit_status_unknown\",\"failure_code\":\"keyboard_commit_unknown\",\"duration_ms\":20}"}`,
		`{"timestamp":"2026-07-28T00:00:02Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"action\",\"status\":\"failed\",\"batch_index\":1,\"action_index\":3,\"action_count\":3,\"action_type\":\"drag\",\"commit_state\":\"not_committed\",\"failure_code\":\"background_action_unsupported\",\"duration_ms\":20}"}`,
		`{"timestamp":"2026-07-28T00:00:03Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"final_observation\",\"status\":\"completed\",\"attempt\":1,\"batch_index\":1,\"duration_ms\":30}"}`,
		`{"timestamp":"2026-07-28T00:00:04Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"batch\",\"status\":\"completed\",\"batch_index\":1,\"duration_ms\":50}"}`,
		`{"timestamp":"2026-07-28T00:00:05Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"private_executor\",\"status\":\"completed\",\"model_calls\":2,\"model_timeouts\":0,\"batch_count\":1,\"duration_ms\":80}"}`,
		`{"timestamp":"2026-07-28T00:00:06Z","session_id":"session-summary","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"task\",\"status\":\"completed\",\"duration_ms\":100}"}`,
		`{"timestamp":"2026-07-28T00:00:07Z","session_id":"other","event":"computer_use_trace_v1","input_summary":"{\"schema_version\":1,\"phase\":\"task\",\"status\":\"failed\",\"failure_code\":\"must_not_leak\",\"duration_ms\":1}"}`,
	}
	if err := os.WriteFile(auditLog, []byte(strings.Join(rows, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(repoRoot(), "scripts", "computer-use-trace.sh")
	command := exec.Command(script, "session-summary", "--summary")
	command.Env = append(os.Environ(), "SHANNON_AUDIT_LOG="+auditLog)
	raw, err := command.Output()
	if err != nil {
		t.Fatalf("trace summary: %v", err)
	}
	var summary struct {
		SchemaVersion               int            `json:"schema_version"`
		SessionID                   string         `json:"session_id"`
		TaskCount                   int            `json:"task_count"`
		TaskOrdinal                 int            `json:"task_ordinal"`
		TaskStatus                  string         `json:"task_status"`
		DurationMS                  int            `json:"duration_ms"`
		ModelCalls                  int            `json:"model_calls"`
		ModelTimeouts               int            `json:"model_timeouts"`
		ProviderBatches             int            `json:"provider_batches"`
		Actions                     int            `json:"actions"`
		ActionTypes                 map[string]int `json:"action_types"`
		AttemptedMutatingActions    int            `json:"attempted_mutating_actions"`
		CommittedMutatingActions    int            `json:"committed_mutating_actions"`
		UnknownCommitActions        int            `json:"unknown_commit_actions"`
		NotCommittedMutatingActions int            `json:"not_committed_mutating_actions"`
		InitialObservationAttempts  int            `json:"initial_observation_attempts"`
		FinalObservationAttempts    int            `json:"final_observation_attempts"`
		FailedBatches               int            `json:"failed_batches"`
		Failures                    []struct {
			Phase string `json:"phase"`
			Code  string `json:"code"`
		} `json:"failures"`
	}
	if err := json.Unmarshal(raw, &summary); err != nil {
		t.Fatalf("decode trace summary: %v\n%s", err, raw)
	}
	if summary.SchemaVersion != 1 ||
		summary.SessionID != "session-summary" ||
		summary.TaskCount != 2 ||
		summary.TaskOrdinal != 2 ||
		summary.TaskStatus != "completed" ||
		summary.DurationMS != 100 ||
		summary.ModelCalls != 2 ||
		summary.ModelTimeouts != 0 ||
		summary.ProviderBatches != 1 ||
		summary.Actions != 3 ||
		summary.ActionTypes["click"] != 1 ||
		summary.ActionTypes["type"] != 1 ||
		summary.ActionTypes["drag"] != 1 ||
		summary.AttemptedMutatingActions != 3 ||
		summary.CommittedMutatingActions != 1 ||
		summary.UnknownCommitActions != 1 ||
		summary.NotCommittedMutatingActions != 1 ||
		summary.InitialObservationAttempts != 1 ||
		summary.FinalObservationAttempts != 1 ||
		summary.FailedBatches != 0 ||
		len(summary.Failures) != 3 {
		t.Fatalf("unexpected trace summary: %+v", summary)
	}
	if strings.Contains(string(raw), "must_not_leak") ||
		strings.Contains(string(raw), "old_task_failure") {
		t.Fatalf("trace summary mixed another session or task: %s", raw)
	}
}

func TestOffline_ComputerUseAcceptanceResultSchemaRequiresRuntimeEvidence(
	t *testing.T,
) {
	path := filepath.Join(
		repoRoot(),
		"test",
		"e2e",
		"testdata",
		"computer_use_acceptance_result.schema.v1.json",
	)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var schema struct {
		Schema     string         `json:"$schema"`
		Type       string         `json:"type"`
		Additional bool           `json:"additionalProperties"`
		Required   []string       `json:"required"`
		Properties map[string]any `json:"properties"`
		AllOf      []any          `json:"allOf"`
	}
	if err := json.Unmarshal(raw, &schema); err != nil {
		t.Fatal(err)
	}
	if schema.Schema != "https://json-schema.org/draft/2020-12/schema" ||
		schema.Type != "object" ||
		schema.Additional {
		t.Fatalf("unexpected result schema envelope: %+v", schema)
	}
	required := make(map[string]struct{}, len(schema.Required))
	for _, field := range schema.Required {
		required[field] = struct{}{}
	}
	for _, field := range []string{
		"scenario_id", "outcome", "classification", "build", "metrics",
		"trace_expectation_met", "visual_verification", "failures",
	} {
		if _, found := required[field]; !found {
			t.Fatalf("result schema does not require %q", field)
		}
		if _, found := schema.Properties[field]; !found {
			t.Fatalf("result schema has no property %q", field)
		}
	}
	metrics, ok := schema.Properties["metrics"].(map[string]any)
	if !ok {
		t.Fatal("result schema metrics is not an object schema")
	}
	requiredMetrics, ok := metrics["required"].([]any)
	if !ok {
		t.Fatal("result schema metrics.required is unavailable")
	}
	metricSet := make(map[string]struct{}, len(requiredMetrics))
	for _, raw := range requiredMetrics {
		value, ok := raw.(string)
		if ok {
			metricSet[value] = struct{}{}
		}
	}
	for _, field := range []string{
		"duration_ms",
		"model_calls",
		"model_timeouts",
		"provider_batches",
		"actions",
		"action_types",
		"attempted_mutating_actions",
		"committed_mutating_actions",
		"unknown_commit_actions",
		"not_committed_mutating_actions",
		"task_count",
		"task_ordinal",
		"initial_observation_attempts",
		"final_observation_attempts",
		"failed_batches",
		"confirmations",
	} {
		if _, found := metricSet[field]; !found {
			t.Fatalf("result schema metrics does not require %q", field)
		}
	}
	if len(schema.AllOf) != 1 {
		t.Fatalf("result schema must bind passed to trace evidence: %+v", schema.AllOf)
	}
	conditional, ok := schema.AllOf[0].(map[string]any)
	if !ok {
		t.Fatal("result schema pass condition is not an object")
	}
	then, ok := conditional["then"].(map[string]any)
	if !ok {
		t.Fatal("result schema pass condition has no then clause")
	}
	thenProperties, ok := then["properties"].(map[string]any)
	if !ok {
		t.Fatal("result schema pass condition has no properties")
	}
	traceExpectation, ok := thenProperties["trace_expectation_met"].(map[string]any)
	if !ok || traceExpectation["const"] != true {
		t.Fatalf(
			"passed result does not require trace evidence: %+v",
			traceExpectation,
		)
	}
}
