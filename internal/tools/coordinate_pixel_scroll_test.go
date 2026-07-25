package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestCoordinatePixelScrollV1CanonicalFixturesRoundTrip(t *testing.T) {
	payload := loadCoordinateFixture(t, "coordinate_pixel_scroll.request.v1.json")
	envelope, err := DecodeCoordinatePixelScrollRPCRequestV1(payload)
	if err != nil {
		t.Fatal(err)
	}
	if envelope.Params.ProviderDeltaX != 37 || envelope.Params.ProviderDeltaY != -618 {
		t.Fatalf("provider deltas changed: %+v", envelope.Params)
	}
	encoded, err := EncodeCoordinatePixelScrollRPCRequestV1(envelope)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, payload, encoded)
	for _, name := range []string{
		"coordinate_pixel_scroll.response.committed_unverified.v1.json",
		"coordinate_pixel_scroll.response.user_interference.v1.json",
		"coordinate_pixel_scroll.response.failed.v1.json",
		"coordinate_pixel_scroll.response.commit_unknown.v1.json",
	} {
		payload := loadCoordinateFixture(t, name)
		result, err := DecodeCoordinatePixelScrollResultV1(payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := EncodeCoordinatePixelScrollResultV1(result)
		if err != nil {
			t.Fatal(err)
		}
		assertCoordinateJSONRoundTrip(t, payload, encoded)
		if result.RetrySafe {
			t.Fatalf("%s marked retry-safe", name)
		}
	}
}

func TestCoordinatePixelScrollV1RejectsUnknownDuplicateAndUnsafeDelta(t *testing.T) {
	payload := loadCoordinateFixture(t, "coordinate_pixel_scroll.request.v1.json")
	duplicate := bytes.Replace(payload, []byte(`"provider_delta_y": -618`),
		[]byte(`"provider_delta_y": -618, "\u0070rovider_delta_y": -618`), 1)
	if _, err := DecodeCoordinatePixelScrollRPCRequestV1(duplicate); err == nil {
		t.Fatal("escaped-equivalent duplicate passed")
	}
	base := coordinateMouseJSONMap(t, payload)
	base["params"].(map[string]any)["extra"] = true
	if _, err := DecodeCoordinatePixelScrollRPCRequestV1(
		marshalCoordinateMouseJSON(t, base)); err == nil {
		t.Fatal("unknown member passed")
	}
	base = coordinateMouseJSONMap(t, payload)
	base["params"].(map[string]any)["provider_delta_y"] = float64(math.MinInt32)
	if _, err := DecodeCoordinatePixelScrollRPCRequestV1(
		marshalCoordinateMouseJSON(t, base)); err == nil {
		t.Fatal("delta whose sign inverse is not Int32-representable passed")
	}
	base = coordinateMouseJSONMap(t, payload)
	base["params"].(map[string]any)["provider_to_quartz_scale_y"] = 2.0 / 3.0
	if _, err := DecodeCoordinatePixelScrollRPCRequestV1(
		marshalCoordinateMouseJSON(t, base)); err == nil {
		t.Fatal("request with tampered frame scale but stale CG delta passed")
	}
}

func TestCoordinatePixelScrollV1ResultMustAcknowledgeExactProviderAndCGDeltas(t *testing.T) {
	result, err := DecodeCoordinatePixelScrollResultV1(loadCoordinateFixture(
		t, "coordinate_pixel_scroll.response.committed_unverified.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	result.Requested.CGPointDeltaAxis1--
	if err := result.ValidateTaggedUnion(); err == nil {
		t.Fatal("response with mismatched provider-to-CG delta mapping passed")
	}
	result, err = DecodeCoordinatePixelScrollResultV1(loadCoordinateFixture(
		t, "coordinate_pixel_scroll.response.committed_unverified.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	result.Requested.ProviderToQuartzScaleX = 2
	if err := result.ValidateTaggedUnion(); err == nil {
		t.Fatal("response with tampered frame scale passed")
	}
}

func TestCoordinatePixelScrollV1ScalesProviderPixelsWithDeterministicRounding(t *testing.T) {
	tests := []struct {
		name                 string
		x, y                 int64
		scaleX, scaleY       float64
		wantAxis1, wantAxis2 int64
	}{
		{
			name: "two thirds frame", x: 37, y: -618,
			scaleX: 2.0 / 3.0, scaleY: 2.0 / 3.0,
			wantAxis1: 412, wantAxis2: -25,
		},
		{
			name: "anisotropic", x: 37, y: -618,
			scaleX: 2, scaleY: 0.5,
			wantAxis1: 309, wantAxis2: -74,
		},
		{
			name: "half ties away from zero", x: 1, y: -1,
			scaleX: 0.5, scaleY: 0.5,
			wantAxis1: 1, wantAxis2: -1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			axis1, axis2, ok := coordinatePixelScrollCGDeltasV1(
				test.x, test.y, test.scaleX, test.scaleY)
			if !ok || axis1 != test.wantAxis1 || axis2 != test.wantAxis2 {
				t.Fatalf("deltas=(%d,%d,%v), want (%d,%d,true)",
					axis1, axis2, ok, test.wantAxis1, test.wantAxis2)
			}
			rawAxis1 := -float64(test.y) * test.scaleY
			rawAxis2 := -float64(test.x) * test.scaleX
			if math.Abs(rawAxis1-float64(axis1)) > 0.5 ||
				math.Abs(rawAxis2-float64(axis2)) > 0.5 {
				t.Fatal("quantization exceeded 0.5 Quartz point")
			}
		})
	}
	for _, invalid := range []struct {
		x, y           int64
		scaleX, scaleY float64
	}{
		{x: 1, scaleX: 0.49, scaleY: 1},
		{x: math.MaxInt32, scaleX: 2, scaleY: 1},
		{x: 1, scaleX: 0, scaleY: 1},
		{x: 1, scaleX: math.Inf(1), scaleY: 1},
	} {
		if _, _, ok := coordinatePixelScrollCGDeltasV1(
			invalid.x, invalid.y, invalid.scaleX, invalid.scaleY); ok {
			t.Fatalf("unsafe scaled delta passed: %+v", invalid)
		}
	}
}

func TestCoordinatePixelScrollV1RejectsImpossibleTaggedUnionTuples(t *testing.T) {
	base, err := DecodeCoordinatePixelScrollResultV1(loadCoordinateFixture(
		t, "coordinate_pixel_scroll.response.committed_unverified.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, mutation := range []func(*CoordinatePixelScrollResultV1){
		func(result *CoordinatePixelScrollResultV1) {
			code := "cancelled_before_scroll"
			result.FailureCode = &code
		},
		func(result *CoordinatePixelScrollResultV1) {
			result.ScrollCommitState = "not_committed"
			code := "cancelled_after_scroll"
			result.FailureCode = &code
			result.Phase = "between_commits"
		},
		func(result *CoordinatePixelScrollResultV1) {
			result.ScrollCommitState = "not_committed"
			code := "scroll_not_committed"
			result.FailureCode = &code
		},
	} {
		result := base
		mutation(&result)
		if err := result.ValidateTaggedUnion(); err == nil {
			t.Fatalf("impossible result passed: %+v", result)
		}
	}
}

func TestCoordinatePixelScrollCancellationMarkerMatchesCrossLanguageFixture(t *testing.T) {
	var fixture struct {
		SchemaVersion int    `json:"schema_version"`
		RequestID     int64  `json:"request_id"`
		HelperBootID  string `json:"helper_boot_id"`
		Basename      string `json:"basename"`
	}
	if err := json.Unmarshal(loadCoordinateFixture(
		t, "coordinate_pixel_scroll.cancellation_marker.v1.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	path := coordinatePixelScrollCancellationMarkerPath(
		fixture.HelperBootID, fixture.RequestID)
	if got := filepath.Base(path); got != fixture.Basename {
		t.Fatalf("marker basename=%q want=%q", got, fixture.Basename)
	}
}

func TestAXClientCoordinatePixelScrollV1AfterWriteCancellationWaitsForTypedAck(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	envelope, err := DecodeCoordinatePixelScrollRPCRequestV1(
		loadCoordinateFixture(t, "coordinate_pixel_scroll.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	envelope.Params.CommitDeadlineAt = time.Now().Add(500 * time.Millisecond).
		UTC().Format(time.RFC3339Nano)
	requestIDs := make(chan int64, 1)
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	writer.afterWrite = func(payload []byte) {
		request, decodeErr := DecodeCoordinatePixelScrollRPCRequestV1(
			bytes.TrimSpace(payload))
		if decodeErr != nil {
			t.Error(decodeErr)
			return
		}
		requestIDs <- request.ID
		cancel()
	}
	type outcome struct {
		result CoordinatePixelScrollResultV1
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, callErr := client.coordinatePixelScrollV1(ctx, envelope.Params)
		done <- outcome{result: result, err: callErr}
	}()
	requestID := <-requestIDs
	select {
	case early := <-done:
		t.Fatalf("cancel released action before typed ack: %+v %v", early.result, early.err)
	case <-time.After(40 * time.Millisecond):
	}
	marker := coordinatePixelScrollCancellationMarkerPath(
		envelope.Params.HelperBootID, requestID)
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("cancel marker missing: %v", err)
	}
	client.pendingMu.Lock()
	responseChannel := client.pending[requestID]
	client.pendingMu.Unlock()
	responseChannel <- AXResponse{ID: requestID, Result: loadCoordinateFixture(
		t, "coordinate_pixel_scroll.response.user_interference.v1.json")}
	select {
	case got := <-done:
		if got.err != nil || got.result.Status != "user_interference" {
			t.Fatalf("result=%+v err=%v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("did not return after typed acknowledgement")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel marker survived acknowledgement: %v", err)
	}
}
