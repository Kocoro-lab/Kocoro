package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestTargetBoundInputV1StrictRequestAndResultContracts(t *testing.T) {
	request := canonicalTargetBoundInputRequest(t, "hotkey")
	payload, err := EncodeTargetBoundInputRPCRequestV1(TargetBoundInputRPCRequestV1{
		ID: 901, Method: "target_bound_input", Params: request,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTargetBoundInputRPCRequestV1(payload); err != nil {
		t.Fatal(err)
	}

	var object map[string]any
	if err := json.Unmarshal(payload, &object); err != nil {
		t.Fatal(err)
	}
	object["extra"] = true
	malformed, _ := json.Marshal(object)
	if _, err := DecodeTargetBoundInputRPCRequestV1(malformed); err == nil {
		t.Fatal("unknown request field passed strict decoder")
	}
	duplicate := bytes.Replace(payload, []byte(`"id":901`), []byte(`"id":901,"\u0069d":901`), 1)
	if _, err := DecodeTargetBoundInputRPCRequestV1(duplicate); err == nil {
		t.Fatal("escaped-equivalent duplicate request member passed strict decoder")
	}

	failure := "postcondition_not_declared"
	result := TargetBoundInputResultV1{
		SchemaVersion: 1, Status: "completed_unverified", Action: "hotkey",
		InputCommitted: true, ClipboardTouched: false, ClipboardRestored: false,
		Phase: "post_verification", FailureCode: &failure, RetrySafe: false,
	}
	encoded, err := EncodeTargetBoundInputResultV1(result)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := DecodeTargetBoundInputResultV1(encoded); err != nil {
		t.Fatal(err)
	}
	var resultObject map[string]any
	if err := json.Unmarshal(encoded, &resultObject); err != nil {
		t.Fatal(err)
	}
	resultObject["text"] = "secret"
	malformedResult, _ := json.Marshal(resultObject)
	if _, err := DecodeTargetBoundInputResultV1(malformedResult); err == nil {
		t.Fatal("content-bearing result passed strict decoder")
	}
	duplicateResult := bytes.Replace(
		encoded, []byte(`"status":"completed_unverified"`),
		[]byte(`"status":"completed_unverified","status":"completed_unverified"`), 1)
	if _, err := DecodeTargetBoundInputResultV1(duplicateResult); err == nil {
		t.Fatal("duplicate result member passed strict decoder")
	}
	if bytes.Contains(encoded, []byte("secret")) {
		t.Fatal("content-free result unexpectedly included typed input")
	}
}

func TestTargetBoundInputV1CanonicalCrossLanguageFixtures(t *testing.T) {
	for _, name := range []string{
		"target_bound_input.request.type.v1.json",
		"target_bound_input.request.hotkey.v1.json",
		"target_bound_input.request.keypress.v1.json",
	} {
		payload := loadCoordinateFixture(t, name)
		envelope, err := DecodeTargetBoundInputRPCRequestV1(payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := EncodeTargetBoundInputRPCRequestV1(envelope)
		if err != nil {
			t.Fatal(err)
		}
		assertCoordinateJSONRoundTrip(t, payload, encoded)
	}
	for _, name := range []string{
		"target_bound_input.response.type.verified.v1.json",
		"target_bound_input.response.type.completed_unverified.v1.json",
		"target_bound_input.response.hotkey.user_interference.v1.json",
		"target_bound_input.response.hotkey.physical_interference.v1.json",
	} {
		payload := loadCoordinateFixture(t, name)
		result, err := DecodeTargetBoundInputResultV1(payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := EncodeTargetBoundInputResultV1(result)
		if err != nil {
			t.Fatal(err)
		}
		assertCoordinateJSONRoundTrip(t, payload, encoded)
	}
}

func TestTargetBoundInputV1CancellationMarkerMatchesCrossLanguageFixture(t *testing.T) {
	var fixture struct {
		SchemaVersion int    `json:"schema_version"`
		RequestID     int64  `json:"request_id"`
		PID           int    `json:"pid"`
		BundleID      string `json:"bundle_id"`
		WindowID      uint32 `json:"window_id"`
		Basename      string `json:"basename"`
	}
	if err := json.Unmarshal(loadCoordinateFixture(
		t, "target_bound_input.cancellation_marker.v1.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	request := TargetBoundInputRequestV1{
		PID: fixture.PID, BundleID: fixture.BundleID, WindowID: fixture.WindowID,
	}
	path := targetBoundInputCancellationMarkerPathV1(request, fixture.RequestID)
	if got := strings.TrimPrefix(path, "/tmp/"); got != fixture.Basename {
		t.Fatalf("marker basename = %q, want %q", got, fixture.Basename)
	}
}

func TestTargetBoundInputV1PhysicalInterferenceAndMonitorLossAreStrict(t *testing.T) {
	if _, err := DecodeTargetBoundInputResultV1(loadCoordinateFixture(
		t, "target_bound_input.response.hotkey.physical_interference.v1.json")); err != nil {
		t.Fatalf("physical interference fixture rejected: %v", err)
	}
	failure := "interference_detection_unavailable"
	result := TargetBoundInputResultV1{
		SchemaVersion: 1, Status: "completed_unverified", Action: "type",
		InputCommitted: true, ClipboardTouched: true, ClipboardRestored: true,
		Phase: "post_verification", FailureCode: &failure,
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		t.Fatalf("post-commit monitor loss rejected: %v", err)
	}
	wrong := "frontmost_process_mismatch"
	result.Status = "user_interference"
	result.Phase = "user_interference"
	result.FailureCode = &wrong
	if err := result.ValidateTaggedUnion(); err == nil {
		t.Fatal("user_interference accepted a non-physical failure code")
	}
}

func TestTargetBoundInputV1TaggedUnionRejectsMalformedTuples(t *testing.T) {
	request := canonicalTargetBoundInputRequest(t, "type")
	request.Key = stringPointer("p")
	if err := request.Validate(); err == nil {
		t.Fatal("type request accepted hotkey-only key")
	}
	request = canonicalTargetBoundInputRequest(t, "hotkey")
	request.Modifiers = nil
	if err := request.Validate(); err == nil {
		t.Fatal("hotkey request accepted null modifiers")
	}
	for _, mutate := range []func(*TargetBoundInputRequestV1){
		func(request *TargetBoundInputRequestV1) { request.Ref = nil },
		func(request *TargetBoundInputRequestV1) { request.Path = nil },
		func(request *TargetBoundInputRequestV1) { request.ExpectedRole = nil },
		func(request *TargetBoundInputRequestV1) { request.ExpectedFingerprint = nil },
	} {
		request = canonicalTargetBoundInputRequest(t, "type")
		mutate(&request)
		if err := request.Validate(); err == nil {
			t.Fatal("type request accepted missing focused-element authority")
		}
	}
	request = canonicalTargetBoundInputRequest(t, "hotkey")
	request.Ref = stringPointer("e2")
	if err := request.Validate(); err == nil {
		t.Fatal("window-bound hotkey accepted element authority")
	}

	failure := "postcondition_not_declared"
	result := TargetBoundInputResultV1{
		SchemaVersion: 1, Status: "completed_unverified", Action: "type",
		InputCommitted: false, Phase: "post_verification", FailureCode: &failure,
	}
	if err := result.ValidateTaggedUnion(); err == nil {
		t.Fatal("completed result accepted uncommitted input")
	}
	result.InputCommitted = true
	result.RetrySafe = true
	if err := result.ValidateTaggedUnion(); err == nil {
		t.Fatal("mutation result accepted retry_safe=true")
	}

	clipboardOwnershipLost := "clipboard_ownership_lost_before_input"
	result = TargetBoundInputResultV1{
		SchemaVersion: 1, Status: "failed", Action: "type",
		InputCommitted: false, ClipboardTouched: true, ClipboardRestored: false,
		Phase: "action", FailureCode: &clipboardOwnershipLost,
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		t.Fatalf("typed clipboard ownership loss was rejected: %v", err)
	}

	postcondition := "target_value_matches_expected_edit"
	verified := TargetBoundInputResultV1{
		SchemaVersion: 1, Status: "verified", Action: "type",
		InputCommitted: true, ClipboardTouched: true, ClipboardRestored: true,
		Phase: "post_verification", Postcondition: &postcondition,
	}
	if err := verified.ValidateTaggedUnion(); err != nil {
		t.Fatalf("exact type postcondition was rejected: %v", err)
	}
	verified.Action = "hotkey"
	if err := verified.ValidateTaggedUnion(); err == nil {
		t.Fatal("hotkey claimed an undeclared verified postcondition")
	}
}

func TestAXClientTargetBoundInputV1WaitsForTypedAckAfterCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	release := make(chan struct{})
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	writer.afterWrite = func(requestBytes []byte) {
		cancel()
		go func() {
			<-release
			envelope, err := DecodeTargetBoundInputRPCRequestV1(bytes.TrimSpace(requestBytes))
			if err != nil {
				return
			}
			failure := "postcondition_not_declared"
			result, _ := EncodeTargetBoundInputResultV1(TargetBoundInputResultV1{
				SchemaVersion: 1, Status: "completed_unverified", Action: envelope.Params.Action,
				InputCommitted: true, Phase: "post_verification", FailureCode: &failure,
			})
			client.pendingMu.Lock()
			response := client.pending[envelope.ID]
			client.pendingMu.Unlock()
			response <- AXResponse{ID: envelope.ID, Result: result}
		}()
	}
	type outcome struct {
		result TargetBoundInputResultV1
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, err := client.targetBoundInputV1(ctx, canonicalTargetBoundInputRequest(t, "hotkey"))
		done <- outcome{result: result, err: err}
	}()
	select {
	case got := <-done:
		t.Fatalf("returned before typed helper acknowledgement: %+v %v", got.result, got.err)
	case <-time.After(30 * time.Millisecond):
	}
	close(release)
	select {
	case got := <-done:
		if got.err != nil || !got.result.InputCommitted {
			t.Fatalf("typed acknowledgement lost: %+v %v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("did not return after helper acknowledgement")
	}
	if writer.writeCount() != 1 {
		t.Fatalf("mutation wrote %d requests", writer.writeCount())
	}
}

func TestAXClientTargetBoundInputV1PostWriteAmbiguityIsCommitUnknown(t *testing.T) {
	for _, test := range []struct {
		name   string
		writer *coordinateMouseTestWriter
	}{
		{name: "write", writer: &coordinateMouseTestWriter{writeErr: io.ErrClosedPipe}},
		{name: "EOF", writer: &coordinateMouseTestWriter{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := coordinateMouseTestClient(test.writer)
			if test.name == "EOF" {
				test.writer.afterWrite = func([]byte) { client.readLoop(strings.NewReader("")) }
			}
			_, err := client.targetBoundInputV1(context.Background(), canonicalTargetBoundInputRequest(t, "type"))
			var commitUnknown *TargetBoundInputCommitUnknownErrorV1
			if !errors.As(err, &commitUnknown) || commitUnknown.RetrySafe() {
				t.Fatalf("error %T %v is not non-retryable commit unknown", err, err)
			}
		})
	}
}

func canonicalTargetBoundInputRequest(t *testing.T, action string) TargetBoundInputRequestV1 {
	t.Helper()
	deadline := time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	request := TargetBoundInputRequestV1{
		SchemaVersion: 1, PID: 42, BundleID: "com.apple.Notes", WindowID: 7001,
		ExpectedWindowAXBounds: CoordinateQuartzRectV1{X: 100, Y: 200, Width: 800, Height: 600},
		Action:                 action, CommitDeadlineAt: deadline,
	}
	if action == "type" {
		request.Ref = stringPointer("e2")
		request.Path = stringPointer("window[0]/AXTextField[0]")
		request.ExpectedRole = stringPointer("AXTextField")
		request.ExpectedFingerprint = stringPointer("axf_e2")
		request.Text = stringPointer("secret input")
	} else if action == "keypress" {
		request.Keys = &[]string{"p", "a", "g", "e", "down"}
		request.Modifiers = &[]string{"command", "shift"}
	} else {
		request.Key = stringPointer("p")
		request.Modifiers = &[]string{"command", "shift"}
	}
	return request
}
