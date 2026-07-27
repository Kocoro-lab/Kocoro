package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCoordinateDragV1CanonicalFixturesRoundTrip(t *testing.T) {
	requestPayload := loadCoordinateFixture(t, "coordinate_drag.request.v1.json")
	envelope, err := DecodeCoordinateDragRPCRequestV1(requestPayload)
	if err != nil {
		t.Fatal(err)
	}
	if len(envelope.Params.Waypoints) != 3 ||
		envelope.Params.Waypoints[1].DisplayID != 1 ||
		envelope.Params.Waypoints[1].QuartzPoint != (CoordinateMouseEventPointV1{X: 350.5, Y: 450.5}) {
		t.Fatalf("coordinate_drag waypoints lost on decode: %+v", envelope.Params.Waypoints)
	}
	encoded, err := EncodeCoordinateDragRPCRequestV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, requestPayload, encoded)
	for _, name := range []string{
		"coordinate_drag.response.completed_unverified.v1.json",
		"coordinate_drag.response.user_interference.v1.json",
		"coordinate_drag.response.physical_interference.v1.json",
		"coordinate_drag.response.mouse_up_unverified.v1.json",
		"coordinate_drag.response.mouse_up_unverified_before_motion.v1.json",
		"coordinate_drag.response.failed.v1.json",
	} {
		payload := loadCoordinateFixture(t, name)
		result, err := DecodeCoordinateDragResultV1(payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := EncodeCoordinateDragResultV1(result)
		if err != nil {
			t.Fatal(err)
		}
		assertCoordinateJSONRoundTrip(t, payload, encoded)
		if result.RetrySafe {
			t.Fatalf("%s marked drag retry-safe", name)
		}
	}
}

func TestCoordinateDragV1RejectsUnknownDuplicateAndInvalidAuthority(t *testing.T) {
	payload := loadCoordinateFixture(t, "coordinate_drag.request.v1.json")
	duplicate := bytes.Replace(payload, []byte(`"generation": 7`),
		[]byte(`"generation": 7, "\u0067eneration": 7`), 1)
	if _, err := DecodeCoordinateDragRPCRequestV1(duplicate); err == nil {
		t.Fatal("escaped-equivalent duplicate passed")
	}
	base := coordinateMouseJSONMap(t, payload)
	params := base["params"].(map[string]any)
	params["extra"] = true
	if _, err := DecodeCoordinateDragRPCRequestV1(marshalCoordinateMouseJSON(t, base)); err == nil {
		t.Fatal("unknown field passed")
	}
	base = coordinateMouseJSONMap(t, payload)
	base["params"].(map[string]any)["duration_ms"] = 1000
	if _, err := DecodeCoordinateDragRPCRequestV1(marshalCoordinateMouseJSON(t, base)); err == nil {
		t.Fatal("unbounded duration passed")
	}
	base = coordinateMouseJSONMap(t, payload)
	waypoints := base["params"].(map[string]any)["waypoints"].([]any)
	waypoints[0].(map[string]any)["display_id"] = float64(99)
	if _, err := DecodeCoordinateDragRPCRequestV1(marshalCoordinateMouseJSON(t, base)); err == nil {
		t.Fatal("waypoint endpoint with mismatched display authority passed")
	}
}

func TestCoordinateDragV1RejectsPointerOnlyVerifiedClaim(t *testing.T) {
	result, err := DecodeCoordinateDragResultV1(
		loadCoordinateFixture(t, "coordinate_drag.response.completed_unverified.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	postcondition := "pointer_endpoint_reached"
	result.Status = "verified"
	result.FailureCode = nil
	result.Postcondition = &postcondition
	if err := result.ValidateTaggedUnion(); err == nil {
		t.Fatal("pointer endpoint alone was accepted as verified drag effect")
	}
}

func TestCoordinateDragV1RejectsImpossiblePointerMotionFlags(t *testing.T) {
	if _, err := DecodeCoordinateDragResultV1(loadCoordinateFixture(
		t, "coordinate_drag.response.invalid_pointer_motion.v1.json",
	)); err == nil {
		t.Fatal("completed_unverified without committed pointer motion passed")
	}
	result, err := DecodeCoordinateDragResultV1(
		loadCoordinateFixture(t, "coordinate_drag.response.failed.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	result.PointerMotionCommitted = true
	if err := result.ValidateTaggedUnion(); err == nil {
		t.Fatal("failed drag with committed pointer motion passed")
	}
}

func TestCoordinateDragV1AdmitsMonitoringLossAfterAcknowledgedMouseUp(t *testing.T) {
	failure := "interference_detection_unavailable"
	observed := CoordinateMouseEventPointV1{X: 200.5, Y: 300.5}
	result := CoordinateDragResultV1{
		SchemaVersion: 1, Status: "completed_unverified",
		DragCommitted: true, MouseDownCommitted: true,
		PointerMotionCommitted: false, MouseUpCommitted: true,
		PossibleDropSideEffect: true, Phase: "post_verification",
		FailureCode: &failure,
		PointerEndpoint: &CoordinateMouseEventPointerEndpointV1{
			Requested: CoordinateMouseEventPointV1{X: 500.5, Y: 300.5},
			Observed:  &observed,
			Tolerance: coordinateMouseEndpointToleranceV1,
			Verified:  false,
		},
	}
	payload, err := EncodeCoordinateDragResultV1(result)
	if err != nil {
		t.Fatalf("encode monitor-loss cleanup: %v", err)
	}
	decoded, err := DecodeCoordinateDragResultV1(payload)
	if err != nil || decoded.Status != "completed_unverified" ||
		decoded.PointerMotionCommitted || !decoded.MouseUpCommitted {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
}

func TestAXClientCoordinateDragV1AfterWriteCancellationWaitsForCleanupAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	requestEnvelope, err := DecodeCoordinateDragRPCRequestV1(
		loadCoordinateFixture(t, "coordinate_drag.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	requestEnvelope.Params.CommitDeadlineAt = time.Now().Add(500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	requestIDs := make(chan int64, 1)
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	writer.afterWrite = func(payload []byte) {
		envelope, decodeErr := DecodeCoordinateDragRPCRequestV1(bytes.TrimSpace(payload))
		if decodeErr != nil {
			t.Error(decodeErr)
			return
		}
		requestIDs <- envelope.ID
		cancel()
	}
	type outcome struct {
		result CoordinateDragResultV1
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, callErr := client.coordinateDragV1(ctx, requestEnvelope.Params)
		done <- outcome{result: result, err: callErr}
	}()
	requestID := <-requestIDs
	select {
	case early := <-done:
		t.Fatalf("cancel released drag before helper cleanup ack: %+v %v", early.result, early.err)
	case <-time.After(40 * time.Millisecond):
	}
	markerPath := coordinateDragCancellationMarkerPath(requestEnvelope.Params.HelperBootID, requestID)
	if _, err := os.Stat(markerPath); err != nil {
		t.Fatalf("cancel marker was not created before ack: %v", err)
	}
	client.pendingMu.Lock()
	responseChannel := client.pending[requestID]
	client.pendingMu.Unlock()
	responseChannel <- AXResponse{ID: requestID, Result: loadCoordinateFixture(
		t, "coordinate_drag.response.user_interference.v1.json")}
	select {
	case outcome := <-done:
		if outcome.err != nil || outcome.result.Status != "user_interference" ||
			!outcome.result.MouseUpCommitted {
			t.Fatalf("cleanup outcome = %+v err=%v", outcome.result, outcome.err)
		}
	case <-time.After(time.Second):
		t.Fatal("drag did not return after helper cleanup ack")
	}
	if _, err := os.Stat(markerPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel marker survived helper ack: %v", err)
	}
	if writer.writeCount() != 1 {
		t.Fatalf("writes = %d", writer.writeCount())
	}
}

func TestCoordinateDragCancellationMarkerMatchesCrossLanguageFixture(t *testing.T) {
	var fixture struct {
		SchemaVersion int    `json:"schema_version"`
		RequestID     int64  `json:"request_id"`
		HelperBootID  string `json:"helper_boot_id"`
		Basename      string `json:"basename"`
	}
	if err := json.Unmarshal(loadCoordinateFixture(
		t, "coordinate_drag.cancellation_marker.v1.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.SchemaVersion != 1 {
		t.Fatalf("schema = %d", fixture.SchemaVersion)
	}
	path := coordinateDragCancellationMarkerPath(fixture.HelperBootID, fixture.RequestID)
	if got := filepath.Base(path); got != fixture.Basename {
		t.Fatalf("marker basename = %q, want %q", got, fixture.Basename)
	}
}

func TestCoordinateDragDropAuthorityInterferenceCodesRemainWireValid(t *testing.T) {
	base, err := DecodeCoordinateDragResultV1(
		loadCoordinateFixture(t, "coordinate_drag.response.user_interference.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, code := range []string{
		"stale_topology", "window_bounds_mismatch", "start_display_not_actionable",
	} {
		t.Run(code, func(t *testing.T) {
			result := base
			result.FailureCode = &code
			payload, err := EncodeCoordinateDragResultV1(result)
			if err != nil {
				t.Fatalf("encode drop-time authority cleanup: %v", err)
			}
			decoded, err := DecodeCoordinateDragResultV1(payload)
			if err != nil || decoded.Status != "user_interference" ||
				decoded.FailureCode == nil || *decoded.FailureCode != code ||
				!decoded.MouseUpCommitted || !decoded.PossibleDropSideEffect {
				t.Fatalf("decoded=%+v err=%v", decoded, err)
			}
		})
	}
}

func TestAXClientCoordinateDragV1HardTimeoutIsBoundedByCommitDeadline(t *testing.T) {
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	request, err := DecodeCoordinateDragRPCRequestV1(
		loadCoordinateFixture(t, "coordinate_drag.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	request.Params.CommitDeadlineAt = time.Now().Add(40 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	started := time.Now()
	_, err = client.coordinateDragV1(context.Background(), request.Params)
	var commitUnknown *CoordinateDragCommitUnknownErrorV1
	if !errors.As(err, &commitUnknown) || commitUnknown.RetrySafe() {
		t.Fatalf("error = %T %v", err, err)
	}
	if elapsed := time.Since(started); elapsed < 35*time.Millisecond || elapsed > time.Second {
		t.Fatalf("hard timeout elapsed %v", elapsed)
	}
}

func TestAXClientCoordinateDragV1ReturnsTypedHelperResult(t *testing.T) {
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	writer.afterWrite = func(requestBytes []byte) {
		envelope, err := DecodeCoordinateDragRPCRequestV1(bytes.TrimSpace(requestBytes))
		if err != nil {
			t.Error(err)
			return
		}
		client.pendingMu.Lock()
		channel := client.pending[envelope.ID]
		client.pendingMu.Unlock()
		channel <- AXResponse{ID: envelope.ID, Result: loadCoordinateFixture(
			t, "coordinate_drag.response.completed_unverified.v1.json")}
	}
	request, err := DecodeCoordinateDragRPCRequestV1(
		loadCoordinateFixture(t, "coordinate_drag.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	request.Params.CommitDeadlineAt = time.Now().Add(500 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	result, err := client.coordinateDragV1(context.Background(), request.Params)
	if err != nil || result.Status != "completed_unverified" ||
		result.FailureCode == nil || *result.FailureCode != "drop_postcondition_not_declared" ||
		!result.MouseUpCommitted {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}
