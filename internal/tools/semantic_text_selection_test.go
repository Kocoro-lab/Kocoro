package tools

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"
)

func TestSemanticTextSelectionV1CanonicalFixturesRoundTrip(t *testing.T) {
	payload := loadCoordinateFixture(t, "semantic_text_selection.request.v1.json")
	envelope, err := DecodeSemanticTextSelectionRPCRequestV1(payload)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := EncodeSemanticTextSelectionRPCRequestV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, payload, encoded)
	for _, name := range []string{
		"semantic_text_selection.response.verified.v1.json",
		"semantic_text_selection.response.mismatch.v1.json",
		"semantic_text_selection.response.fallback_required.v1.json",
		"semantic_text_selection.response.user_interference.precommit.v1.json",
		"semantic_text_selection.response.user_interference.postcommit.v1.json",
		"semantic_text_selection.response.interference_unavailable.precommit.v1.json",
		"semantic_text_selection.response.interference_unavailable.postcommit.v1.json",
	} {
		payload := loadCoordinateFixture(t, name)
		result, err := DecodeSemanticTextSelectionResultV1(payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := EncodeSemanticTextSelectionResultV1(result)
		if err != nil {
			t.Fatal(err)
		}
		assertCoordinateJSONRoundTrip(t, payload, encoded)
	}
}

func TestSemanticTextSelectionV1AcceptsInterferenceTaggedUnionShapes(t *testing.T) {
	physicalInterference := "physical_input_interference"
	monitorUnavailable := "interference_detection_unavailable"
	for _, result := range []SemanticTextSelectionResultV1{
		{
			SchemaVersion: 1, Status: "user_interference", SelectionCommitted: false,
			Phase: "user_interference", FailureCode: &physicalInterference,
		},
		{
			SchemaVersion: 1, Status: "user_interference", SelectionCommitted: true,
			Phase: "user_interference", FailureCode: &physicalInterference,
		},
		{
			SchemaVersion: 1, Status: "failed", SelectionCommitted: false,
			Phase: "preflight", FailureCode: &monitorUnavailable,
		},
		{
			SchemaVersion: 1, Status: "completed_unverified", SelectionCommitted: true,
			Phase: "post_verification", FailureCode: &monitorUnavailable,
		},
	} {
		if err := result.ValidateTaggedUnion(); err != nil {
			t.Errorf("valid interference result rejected: %+v: %v", result, err)
		}
	}
}

func TestSemanticTextSelectionV1RejectsMalformedInterferenceTaggedUnionShapes(t *testing.T) {
	physicalInterference := "physical_input_interference"
	monitorUnavailable := "interference_detection_unavailable"
	selected := SemanticTextRangeV1{Location: 1, Length: 2}
	postcondition := "selected_range_matches"
	for _, result := range []SemanticTextSelectionResultV1{
		{
			SchemaVersion: 1, Status: "user_interference", SelectionCommitted: true,
			Phase: "post_verification", FailureCode: &physicalInterference,
		},
		{
			SchemaVersion: 1, Status: "user_interference", SelectionCommitted: false,
			Phase: "user_interference", FailureCode: &monitorUnavailable,
		},
		{
			SchemaVersion: 1, Status: "user_interference", SelectionCommitted: false,
			Phase: "user_interference", FailureCode: &physicalInterference,
			SelectedRange: &selected,
		},
		{
			SchemaVersion: 1, Status: "user_interference", SelectionCommitted: false,
			Phase: "user_interference", FailureCode: &physicalInterference,
			Postcondition: &postcondition,
		},
		{
			SchemaVersion: 1, Status: "failed", SelectionCommitted: true,
			Phase: "preflight", FailureCode: &monitorUnavailable,
		},
		{
			SchemaVersion: 1, Status: "completed_unverified", SelectionCommitted: false,
			Phase: "post_verification", FailureCode: &monitorUnavailable,
		},
	} {
		if err := result.ValidateTaggedUnion(); err == nil {
			t.Errorf("malformed interference result passed: %+v", result)
		}
	}
}

func TestSemanticTextSelectionV1RejectsDuplicateAndUnknownFields(t *testing.T) {
	payload := loadCoordinateFixture(t, "semantic_text_selection.request.v1.json")
	duplicate := bytes.Replace(payload, []byte(`"location": 5`),
		[]byte(`"location": 5, "\u006cocation": 5`), 1)
	if _, err := DecodeSemanticTextSelectionRPCRequestV1(duplicate); err == nil {
		t.Fatal("duplicate range member passed")
	}
	base := coordinateMouseJSONMap(t, payload)
	base["params"].(map[string]any)["value"] = "must never be accepted"
	if _, err := DecodeSemanticTextSelectionRPCRequestV1(marshalCoordinateMouseJSON(t, base)); err == nil {
		t.Fatal("unknown sensitive value field passed")
	}
}

func TestSemanticTextSelectionV1AcceptsExplicitObservedMismatch(t *testing.T) {
	failure := "selected_range_mismatch"
	observed := SemanticTextRangeV1{Location: 1, Length: 2}
	result := SemanticTextSelectionResultV1{
		SchemaVersion: 1, Status: "completed_unverified",
		SelectionCommitted: true, Phase: "post_verification",
		FailureCode: &failure, SelectedRange: &observed,
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		t.Fatalf("explicit mismatch rejected: %v", err)
	}
	result.SelectedRange = nil
	if err := result.ValidateTaggedUnion(); err == nil {
		t.Fatal("selection mismatch without observed range passed")
	}
}

func TestAXClientSemanticTextSelectionV1ReturnsFallbackRequired(t *testing.T) {
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	writer.afterWrite = func(requestBytes []byte) {
		envelope, err := DecodeSemanticTextSelectionRPCRequestV1(bytes.TrimSpace(requestBytes))
		if err != nil {
			t.Error(err)
			return
		}
		client.pendingMu.Lock()
		channel := client.pending[envelope.ID]
		client.pendingMu.Unlock()
		channel <- AXResponse{ID: envelope.ID, Result: loadCoordinateFixture(
			t, "semantic_text_selection.response.fallback_required.v1.json")}
	}
	request, err := DecodeSemanticTextSelectionRPCRequestV1(
		loadCoordinateFixture(t, "semantic_text_selection.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	request.Params.CommitDeadlineAt = time.Now().Add(500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	result, err := client.semanticTextSelectionV1(context.Background(), request.Params)
	if err != nil || result.Status != "fallback_required" || result.SelectionCommitted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestAXClientSemanticTextSelectionV1CancellationWaitsForHelperAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	request, err := DecodeSemanticTextSelectionRPCRequestV1(
		loadCoordinateFixture(t, "semantic_text_selection.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	request.Params.CommitDeadlineAt = time.Now().Add(500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	requestIDs := make(chan int64, 1)
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	writer.afterWrite = func(payload []byte) {
		envelope, decodeErr := DecodeSemanticTextSelectionRPCRequestV1(bytes.TrimSpace(payload))
		if decodeErr != nil {
			t.Error(decodeErr)
			return
		}
		requestIDs <- envelope.ID
		cancel()
	}
	type outcome struct {
		result SemanticTextSelectionResultV1
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, callErr := client.semanticTextSelectionV1(ctx, request.Params)
		done <- outcome{result: result, err: callErr}
	}()
	requestID := <-requestIDs
	select {
	case early := <-done:
		t.Fatalf("cancel released semantic action before helper ack: %+v %v", early.result, early.err)
	case <-time.After(40 * time.Millisecond):
	}
	client.pendingMu.Lock()
	responseChannel := client.pending[requestID]
	client.pendingMu.Unlock()
	responseChannel <- AXResponse{ID: requestID, Result: loadCoordinateFixture(
		t, "semantic_text_selection.response.verified.v1.json")}
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.result.Status != "verified" ||
			!outcome.result.SelectionCommitted {
			t.Fatalf("outcome=%+v err=%v", outcome.result, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("semantic selection did not return after helper ack")
	}
}

func TestAXClientSemanticTextSelectionV1HardTimeoutIsBounded(t *testing.T) {
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	request, err := DecodeSemanticTextSelectionRPCRequestV1(
		loadCoordinateFixture(t, "semantic_text_selection.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	request.Params.CommitDeadlineAt = time.Now().Add(40 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	started := time.Now()
	_, err = client.semanticTextSelectionV1(context.Background(), request.Params)
	var commitUnknown *SemanticTextSelectionCommitUnknownErrorV1
	if !errors.As(err, &commitUnknown) || commitUnknown.RetrySafe() {
		t.Fatalf("error=%T %v", err, err)
	}
	if elapsed := time.Since(started); elapsed < 35*time.Millisecond || elapsed > time.Second {
		t.Fatalf("hard timeout elapsed %v", elapsed)
	}
}
