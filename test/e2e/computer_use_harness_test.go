package e2e

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type computerUseAcceptanceManifestV1 struct {
	SchemaVersion int    `json:"schema_version"`
	Suite         string `json:"suite"`
	RunPolicy     struct {
		ManualOnly            bool `json:"manual_only"`
		PaidModelCalls        bool `json:"paid_model_calls"`
		OnePointerOperator    bool `json:"one_pointer_operator"`
		SecondaryDisplayPause bool `json:"secondary_display_paused"`
		DefaultTimeoutSeconds int  `json:"default_timeout_seconds"`
	} `json:"run_policy"`
	Scenarios []struct {
		ID               string   `json:"id"`
		EnabledByDefault bool     `json:"enabled_by_default"`
		Prompt           string   `json:"prompt"`
		Preconditions    []string `json:"preconditions"`
		ExpectedState    []string `json:"expected_state"`
		Risk             string   `json:"risk"`
		Tags             []string `json:"tags"`
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
		manifest.RunPolicy.DefaultTimeoutSeconds != 120 {
		t.Fatalf("unsafe or drifting run policy: %+v", manifest.RunPolicy)
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
			len(scenario.Tags) == 0 {
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
		"textedit_calculator_cross_app",
		"slack_draft_without_send",
		"slack_send_with_confirmation",
		"pointer_interference",
	} {
		if _, found := seen[required]; !found {
			t.Fatalf("required scenario %q is missing", required)
		}
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
		"scenario_id", "outcome", "build", "metrics",
		"visual_verification", "failures",
	} {
		if _, found := required[field]; !found {
			t.Fatalf("result schema does not require %q", field)
		}
		if _, found := schema.Properties[field]; !found {
			t.Fatalf("result schema has no property %q", field)
		}
	}
}
