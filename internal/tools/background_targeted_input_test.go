package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestBackgroundTargetedInputV1CanonicalCrossLanguageFixture(t *testing.T) {
	payload := loadCoordinateFixture(
		t, "background_targeted_input.request.type.v1.json")
	envelope, err := DecodeBackgroundTargetedInputRPCRequestV1(payload)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.ID != 905 ||
		envelope.Params.Input.Action != "type" ||
		envelope.Params.TargetLaunchDate != "2026-07-28T06:00:00Z" ||
		envelope.Params.PreservedFrontmostPID != 84 {
		t.Fatalf("decoded background request = %+v", envelope)
	}
	encoded, err := EncodeBackgroundTargetedInputRPCRequestV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, payload, encoded)
}

func TestBackgroundTargetedInputV1StrictWireAndPolicy(t *testing.T) {
	request := canonicalBackgroundTargetedInputRequest(t, "type")
	payload, err := EncodeBackgroundTargetedInputRPCRequestV1(
		BackgroundTargetedInputRPCRequestV1{
			ID: 905, Method: "background_targeted_input", Params: request,
		})
	if err != nil {
		t.Fatal(err)
	}

	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	object["extra"] = true
	malformed, _ := json.Marshal(object)
	if _, err := DecodeBackgroundTargetedInputRPCRequestV1(malformed); err == nil {
		t.Fatal("unknown request field passed strict decoder")
	}
	duplicate := bytes.Replace(
		payload,
		[]byte(`"id":905`),
		[]byte(`"id":905,"\u0069d":905`),
		1,
	)
	if _, err := DecodeBackgroundTargetedInputRPCRequestV1(duplicate); err == nil {
		t.Fatal("escaped-equivalent duplicate member passed strict decoder")
	}
	if _, err := DecodeBackgroundTargetedInputRPCRequestV1(
		append(payload, []byte(`{"trailing":true}`)...),
	); err == nil {
		t.Fatal("trailing JSON value passed strict decoder")
	}
	whitespacePath := canonicalBackgroundTargetedInputRequest(t, "type")
	whitespacePath.FocusedPath += " "
	whitespacePath.Input.Path = &whitespacePath.FocusedPath
	if err := whitespacePath.Validate(); err == nil {
		t.Fatal("background input accepted a whitespace-mutated AX path")
	}

	for _, text := range []string{"line\nbreak", "tab\tvalue", "nul\x00value"} {
		rejected := canonicalBackgroundTargetedInputRequest(t, "type")
		rejected.Input.Text = &text
		if err := rejected.Validate(); err == nil {
			t.Fatalf("background type accepted control text %q", text)
		}
	}
	for _, test := range []struct {
		name      string
		keys      []string
		modifiers []string
		rejected  bool
	}{
		{name: "return", keys: []string{"return"}, rejected: true},
		{name: "command-return", keys: []string{"return"}, modifiers: []string{"command"}, rejected: true},
		{name: "shift-delete", keys: []string{"delete"}, modifiers: []string{"shift"}, rejected: true},
		{name: "space", keys: []string{"space"}, modifiers: []string{}},
		{name: "left", keys: []string{"left"}, modifiers: []string{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := canonicalBackgroundTargetedInputRequest(t, "keypress")
			candidate.Input.Keys = &test.keys
			candidate.Input.Modifiers = &test.modifiers
			err := candidate.Validate()
			if test.rejected && err == nil {
				t.Fatal("consequential background key was accepted")
			}
			if !test.rejected && err != nil {
				t.Fatalf("ordinary background key was rejected: %v", err)
			}
		})
	}
}

func TestAXClientBackgroundTargetedInputV1WritesDistinctRPCAndPreservesAck(
	t *testing.T,
) {
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	writer.afterWrite = func(requestBytes []byte) {
		envelope, err := DecodeBackgroundTargetedInputRPCRequestV1(
			bytes.TrimSpace(requestBytes))
		if err != nil {
			t.Error(err)
			return
		}
		if envelope.Method != "background_targeted_input" ||
			envelope.Params.PreservedFrontmostBundleID != "com.apple.TextEdit" {
			t.Errorf("background RPC authority = %+v", envelope)
			return
		}
		postcondition := "target_value_matches_expected_edit"
		result, err := EncodeTargetBoundInputResultV1(TargetBoundInputResultV1{
			SchemaVersion: 1, Status: "verified", Action: "type",
			InputCommitted: true, Phase: "post_verification",
			Postcondition: &postcondition,
		})
		if err != nil {
			t.Error(err)
			return
		}
		client.pendingMu.Lock()
		response := client.pending[envelope.ID]
		client.pendingMu.Unlock()
		response <- AXResponse{ID: envelope.ID, Result: result}
	}

	result, err := client.BackgroundTargetedInputV1(
		context.Background(),
		canonicalBackgroundTargetedInputRequest(t, "type"),
	)
	if err != nil || result.Status != "verified" || !result.InputCommitted {
		t.Fatalf("background acknowledgement = %+v, %v", result, err)
	}
	if writer.writeCount() != 1 {
		t.Fatalf("background mutation wrote %d requests", writer.writeCount())
	}
	if strings.Contains(string(writer.writes[0]), "target_bound_input\"") &&
		!strings.Contains(string(writer.writes[0]), "background_targeted_input\"") {
		t.Fatalf("background mutation used foreground RPC: %s", writer.writes[0])
	}
}

func canonicalBackgroundTargetedInputRequest(
	t *testing.T,
	action string,
) BackgroundTargetedInputRequestV1 {
	t.Helper()
	input := canonicalTargetBoundInputRequest(t, action)
	if action == "keypress" {
		keys := []string{"left"}
		modifiers := []string{}
		input.Keys = &keys
		input.Modifiers = &modifiers
	}
	return BackgroundTargetedInputRequestV1{
		SchemaVersion:                1,
		Input:                        input,
		FocusedRef:                   "e2",
		FocusedPath:                  "window[0]/AXTextField[0]",
		ExpectedFocusedRole:          "AXTextField",
		ExpectedFocusedFingerprint:   "axf_e2",
		TargetLaunchDate:             "2026-07-28T06:00:00Z",
		PreservedFrontmostPID:        84,
		PreservedFrontmostBundleID:   "com.apple.TextEdit",
		PreservedFrontmostLaunchDate: "2026-07-28T05:00:00Z",
	}
}
