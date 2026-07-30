package tools

import (
	"encoding/json"
	"os"
	"testing"
)

func loadSemanticPressFixture(t *testing.T, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatalf("read semantic_press fixture: %v", err)
	}
	return payload
}

func TestLegacySemanticPressV1RequestFixtureRemainsDecodable(t *testing.T) {
	var request struct {
		ID     int64  `json:"id"`
		Method string `json:"method"`
		Params struct {
			PID                 int    `json:"pid"`
			WindowID            int    `json:"window_id"`
			Path                string `json:"path"`
			ExpectedRole        string `json:"expected_role"`
			ExpectedFingerprint string `json:"expected_fingerprint"`
			FallbackPolicy      string `json:"fallback_policy"`
		} `json:"params"`
	}
	if err := json.Unmarshal(loadSemanticPressFixture(t, "semantic_press.request.v1.json"), &request); err != nil {
		t.Fatal(err)
	}
	if request.ID != 701 || request.Method != "semantic_press" {
		t.Fatalf("request envelope = %+v", request)
	}
	params := legacySemanticPressV1ParamsForFixture(7001, refEntry{
		pid: 42, path: "window[0]/AXButton[0]", role: "AXButton", fingerprint: "axf_target",
	})
	if params["pid"] != request.Params.PID || params["window_id"] != request.Params.WindowID ||
		params["path"] != request.Params.Path || params["expected_role"] != request.Params.ExpectedRole ||
		params["expected_fingerprint"] != request.Params.ExpectedFingerprint ||
		params["fallback_policy"] != request.Params.FallbackPolicy {
		t.Fatalf("legacy params = %+v, fixture = %+v", params, request.Params)
	}
}

func TestSemanticPressResponseFixturesDecodeThroughProduction(t *testing.T) {
	tests := []struct {
		name      string
		status    string
		committed bool
		failure   string
	}{
		{name: "semantic_press.response.changed_unverified.v1.json", status: "completed_unverified", committed: true, failure: "postcondition_not_declared"},
		{name: "semantic_press.response.completed_unverified.v1.json", status: "completed_unverified", committed: true, failure: "postcondition_not_observed"},
		{name: "semantic_press.response.failed.v1.json", status: "failed", failure: "fingerprint_ambiguous"},
	}
	for _, test := range tests {
		t.Run(test.status, func(t *testing.T) {
			result, err := decodeComputerUseSemanticPressResult(loadSemanticPressFixture(t, test.name))
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != test.status || result.PressCommitted == nil || *result.PressCommitted != test.committed {
				t.Fatalf("decoded result = %+v", result)
			}
			if test.failure == "" {
				if result.FailureCode != nil {
					t.Fatalf("failure_code = %v, want nil", *result.FailureCode)
				}
			} else if result.FailureCode == nil || *result.FailureCode != test.failure {
				t.Fatalf("failure_code = %v, want %q", result.FailureCode, test.failure)
			}
		})
	}
}
