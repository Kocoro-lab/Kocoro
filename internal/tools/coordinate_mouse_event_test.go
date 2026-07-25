package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCoordinateMouseEventV1CanonicalFixturesRoundTrip(t *testing.T) {
	for _, name := range []string{
		"coordinate_mouse_event.request.move.v1.json",
		"coordinate_mouse_event.request.click.v1.json",
		"coordinate_mouse_event.request.risk_click.v1.json",
	} {
		payload := loadCoordinateFixture(t, name)
		envelope, err := DecodeCoordinateMouseEventRPCRequestV1(payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := EncodeCoordinateMouseEventRPCRequestV1(envelope)
		if err != nil {
			t.Fatal(err)
		}
		assertCoordinateJSONRoundTrip(t, payload, encoded)
	}

	for _, name := range []string{
		"coordinate_mouse_event.response.move.completed.v1.json",
		"coordinate_mouse_event.response.click.completed_unverified.v1.json",
		"coordinate_mouse_event.response.user_interference.v1.json",
		"coordinate_mouse_event.response.failed.stale_topology.v1.json",
		"coordinate_mouse_event.response.failed.point_occluded.v1.json",
		"coordinate_mouse_event.response.failed.endpoint.v1.json",
	} {
		payload := loadCoordinateFixture(t, name)
		result, err := DecodeCoordinateMouseEventResultV1(payload)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		encoded, err := EncodeCoordinateMouseEventResultV1(result)
		if err != nil {
			t.Fatal(err)
		}
		assertCoordinateJSONRoundTrip(t, payload, encoded)
		if result.RetrySafe {
			t.Fatalf("%s unexpectedly marked mutation retry-safe", name)
		}
	}
}

func TestCoordinateMouseEventV1PhysicalInterferenceTaggedUnion(t *testing.T) {
	result, err := DecodeCoordinateMouseEventResultV1(
		loadCoordinateFixture(t, "coordinate_mouse_event.response.user_interference.v1.json"))
	if err != nil {
		t.Fatalf("valid physical interference rejected: %v", err)
	}
	if result.Status != "user_interference" || result.PrimaryActionCommitted != true ||
		result.PointerEndpoint == nil || result.PointerEndpoint.Verified {
		t.Fatalf("physical interference result lost commit/endpoint truth: %+v", result)
	}

	invalid := coordinateMouseJSONMap(
		t, loadCoordinateFixture(t, "coordinate_mouse_event.response.user_interference.v1.json"))
	invalid["failure_code"] = "pointer_endpoint_not_verified"
	if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, invalid)); err == nil {
		t.Fatal("user_interference accepted a non-interference failure code")
	}

	preCommit := coordinateMouseJSONMap(
		t, loadCoordinateFixture(t, "coordinate_mouse_event.response.user_interference.v1.json"))
	preCommit["action"] = "move"
	preCommit["primary_action_committed"] = false
	preCommit["pointer_motion_committed"] = false
	preCommit["pointer_endpoint"] = nil
	if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, preCommit)); err != nil {
		t.Fatalf("pre-commit physical user presence rejected: %v", err)
	}
}

func TestCoordinateMouseEventV1CrashGateOutcomesAreStrictAndTruthful(t *testing.T) {
	observed := CoordinateMouseEventPointV1{X: 200.5, Y: 300.5}
	endpoint := &CoordinateMouseEventPointerEndpointV1{
		Requested: observed,
		Observed:  &observed,
		Tolerance: coordinateMouseEndpointToleranceV1,
		Verified:  true,
	}
	blocked := "input_commit_blocked"
	result := CoordinateMouseEventResultV1{
		SchemaVersion: 1, Status: "failed", Action: "click",
		PrimaryActionCommitted: false, PointerMotionCommitted: true,
		Phase: "action", FailureCode: &blocked, RetrySafe: false,
		PointerEndpoint: endpoint,
	}
	if err := result.ValidateTaggedUnion(); err != nil {
		t.Fatalf("valid pre-commit gate block rejected: %v", err)
	}

	interrupted := "input_sequence_interrupted_after_commit"
	result.Status = "completed_unverified"
	result.PrimaryActionCommitted = true
	result.Phase = "post_verification"
	result.FailureCode = &interrupted
	if err := result.ValidateTaggedUnion(); err != nil {
		t.Fatalf("valid partial multi-click outcome rejected: %v", err)
	}

	result.PrimaryActionCommitted = false
	if err := result.ValidateTaggedUnion(); err == nil {
		t.Fatal("partial multi-click outcome omitted committed primary action")
	}
}

func TestCoordinateMouseEventV1StrictRequestTaggedUnion(t *testing.T) {
	base := coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.request.click.v1.json"))
	for _, button := range []string{"left", "right", "wheel", "back", "forward"} {
		t.Run("official click button "+button, func(t *testing.T) {
			candidate := cloneCoordinateMouseJSONMap(t, base)
			candidate["params"].(map[string]any)["button"] = button
			if _, err := DecodeCoordinateMouseEventRPCRequestV1(
				marshalCoordinateMouseJSON(t, candidate),
			); err != nil {
				t.Fatalf("official click button rejected: %v", err)
			}
		})
	}
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown envelope field", mutate: func(v map[string]any) { v["extra"] = true }},
		{name: "missing nullable field", mutate: func(v map[string]any) { delete(v["params"].(map[string]any), "button") }},
		{name: "null required authority", mutate: func(v map[string]any) { v["params"].(map[string]any)["helper_boot_id"] = nil }},
		{name: "unknown nested point field", mutate: func(v map[string]any) {
			v["params"].(map[string]any)["quartz_point"].(map[string]any)["z"] = 1
		}},
		{name: "unknown action", mutate: func(v map[string]any) { v["params"].(map[string]any)["action"] = "drag" }},
		{name: "click without button", mutate: func(v map[string]any) { v["params"].(map[string]any)["button"] = nil }},
		{name: "click count too high", mutate: func(v map[string]any) { v["params"].(map[string]any)["click_count"] = 4 }},
		{name: "invalid deadline", mutate: func(v map[string]any) { v["params"].(map[string]any)["commit_deadline_at"] = "later" }},
		{name: "whitespace authority", mutate: func(v map[string]any) { v["params"].(map[string]any)["bundle_id"] = " com.example.fixture" }},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneCoordinateMouseJSONMap(t, base)
			test.mutate(candidate)
			if _, err := DecodeCoordinateMouseEventRPCRequestV1(marshalCoordinateMouseJSON(t, candidate)); err == nil {
				t.Fatal("malformed request passed strict decoder")
			}
		})
	}

	move := coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.request.move.v1.json"))
	move["params"].(map[string]any)["button"] = "left"
	if _, err := DecodeCoordinateMouseEventRPCRequestV1(marshalCoordinateMouseJSON(t, move)); err == nil {
		t.Fatal("move accepted click-only tagged field")
	}
}

func TestCoordinateMouseEventV1RiskAssertionBindsSingleLeftClickAndExactAuthority(t *testing.T) {
	base := coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.request.risk_click.v1.json"))
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "right click", mutate: func(v map[string]any) {
			v["params"].(map[string]any)["button"] = "right"
		}},
		{name: "double click", mutate: func(v map[string]any) {
			v["params"].(map[string]any)["click_count"] = float64(2)
		}},
		{name: "mismatched point", mutate: func(v map[string]any) {
			authority := v["params"].(map[string]any)["risk_assertion"].(map[string]any)["coordinate_authority"].(map[string]any)
			authority["quartz_point"].(map[string]any)["x"] = 201.5
		}},
		{name: "mismatched topology", mutate: func(v map[string]any) {
			authority := v["params"].(map[string]any)["risk_assertion"].(map[string]any)["coordinate_authority"].(map[string]any)
			authority["topology_ref"].(map[string]any)["generation"] = float64(8)
		}},
		{name: "commit beyond frame", mutate: func(v map[string]any) {
			v["params"].(map[string]any)["commit_deadline_at"] = "2026-07-22T12:03:40.001Z"
		}},
		{name: "unknown assertion field", mutate: func(v map[string]any) {
			v["params"].(map[string]any)["risk_assertion"].(map[string]any)["unexpected"] = true
		}},
		{name: "unknown coordinate authority field", mutate: func(v map[string]any) {
			authority := v["params"].(map[string]any)["risk_assertion"].(map[string]any)["coordinate_authority"].(map[string]any)
			authority["unexpected"] = true
		}},
		{name: "unknown source pixel field", mutate: func(v map[string]any) {
			authority := v["params"].(map[string]any)["risk_assertion"].(map[string]any)["coordinate_authority"].(map[string]any)
			authority["source_pixel"].(map[string]any)["z"] = float64(1)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneCoordinateMouseJSONMap(t, base)
			test.mutate(candidate)
			if _, err := DecodeCoordinateMouseEventRPCRequestV1(marshalCoordinateMouseJSON(t, candidate)); err == nil {
				t.Fatal("malformed risk assertion passed strict decoder")
			}
		})
	}
}

func TestCoordinateMouseEventV1RejectsDuplicateMembersAtAnyDepth(t *testing.T) {
	request := loadCoordinateFixture(t, "coordinate_mouse_event.request.click.v1.json")
	for _, test := range []struct {
		name     string
		old, new []byte
	}{
		{
			name: "top level",
			old:  []byte(`"id": 801,`),
			new:  []byte(`"id": 801, "id": 801,`),
		},
		{
			name: "nested",
			old:  []byte(`"action": "click",`),
			new:  []byte(`"action": "click", "action": "click",`),
		},
		{
			name: "escaped equivalent",
			old:  []byte(`"id": 801,`),
			new:  []byte(`"id": 801, "\u0069d": 801,`),
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := bytes.Replace(request, test.old, test.new, 1)
			if bytes.Equal(candidate, request) {
				t.Fatal("test mutation did not change fixture")
			}
			if _, err := DecodeCoordinateMouseEventRPCRequestV1(candidate); err == nil {
				t.Fatal("duplicate-member request passed strict decoder")
			}
		})
	}

	result := loadCoordinateFixture(t, "coordinate_mouse_event.response.move.completed.v1.json")
	result = bytes.Replace(
		result,
		[]byte(`"requested": {"x": 200.5, "y": 300.5}`),
		[]byte(`"requested": {"x": 200.5, "x": 200.5, "y": 300.5}`),
		1,
	)
	if _, err := DecodeCoordinateMouseEventResultV1(result); err == nil {
		t.Fatal("deep duplicate-member result passed strict decoder")
	}

	var arbitrary any
	if err := decodeStrictCoordinateJSON(
		[]byte(`{"items":[{"name":"first","\u006eame":"first"}]}`),
		&arbitrary,
	); err == nil {
		t.Fatal("duplicate member nested through an array passed shared strict decoder")
	}

	risk := loadCoordinateFixture(t, "coordinate_mouse_event.request.risk_click.v1.json")
	risk = bytes.Replace(
		risk,
		[]byte(`"frame_id": "frame_00112233445566778899aabbccddeeff",`),
		[]byte(`"frame_id": "frame_00112233445566778899aabbccddeeff", "frame_id": "frame_00112233445566778899aabbccddeeff",`),
		1,
	)
	if _, err := DecodeCoordinateMouseEventRPCRequestV1(risk); err == nil {
		t.Fatal("duplicate member in coordinate risk authority passed strict decoder")
	}
}

func TestCoordinateMouseEventV1StrictResultTaggedUnion(t *testing.T) {
	base := coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.response.move.completed.v1.json"))
	for _, test := range []struct {
		name   string
		mutate func(map[string]any)
	}{
		{name: "unknown field", mutate: func(v map[string]any) { v["extra"] = true }},
		{name: "missing nullable field", mutate: func(v map[string]any) { delete(v, "failure_code") }},
		{name: "null required boolean", mutate: func(v map[string]any) { v["retry_safe"] = nil }},
		{name: "mutation marked retry safe", mutate: func(v map[string]any) { v["retry_safe"] = true }},
		{name: "completed carries failure", mutate: func(v map[string]any) { v["failure_code"] = "pointer_endpoint_not_verified" }},
		{name: "completed lacks endpoint", mutate: func(v map[string]any) { v["pointer_endpoint"] = nil }},
		{name: "committed false", mutate: func(v map[string]any) { v["primary_action_committed"] = false }},
		{name: "unknown endpoint field", mutate: func(v map[string]any) {
			v["pointer_endpoint"].(map[string]any)["extra"] = true
		}},
		{name: "verified geometry mismatch", mutate: func(v map[string]any) {
			v["pointer_endpoint"].(map[string]any)["observed"].(map[string]any)["x"] = 210.5
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			candidate := cloneCoordinateMouseJSONMap(t, base)
			test.mutate(candidate)
			if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, candidate)); err == nil {
				t.Fatal("malformed result passed strict decoder")
			}
		})
	}

	failed := coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.response.failed.stale_topology.v1.json"))
	failed["failure_code"] = nil
	if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, failed)); err == nil {
		t.Fatal("failed result accepted null failure_code")
	}
	failed = coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.response.failed.stale_topology.v1.json"))
	failed["primary_action_committed"] = true
	if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, failed)); err == nil {
		t.Fatal("failed result accepted primary action commit")
	}

	trailing := append(loadCoordinateFixture(t, "coordinate_mouse_event.response.move.completed.v1.json"), []byte(`{}`)...)
	if _, err := DecodeCoordinateMouseEventResultV1(trailing); err == nil {
		t.Fatal("result decoder accepted trailing JSON")
	}
}

func TestCoordinateMouseEventV1PostMoveClickFailureUsesActionPhase(t *testing.T) {
	result := coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.response.move.completed.v1.json"))
	result["status"] = "failed"
	result["action"] = "click"
	result["primary_action_committed"] = false
	result["phase"] = "action"
	result["failure_code"] = "target_changed_before_click"
	if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, result)); err != nil {
		t.Fatalf("action-phase post-move failure rejected: %v", err)
	}
	result["phase"] = "preflight"
	if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, result)); err == nil {
		t.Fatal("post-move failure accepted stale preflight phase")
	}
}

func TestCoordinateMouseEventV1RiskRevalidationFailuresUseActionPhaseWithoutPrimaryCommit(t *testing.T) {
	for _, code := range []string{
		"risk_destination_drift",
		"risk_hit_target_drift",
		"risk_hit_target_unavailable",
	} {
		t.Run(code, func(t *testing.T) {
			result := coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.response.move.completed.v1.json"))
			result["status"] = "failed"
			result["action"] = "click"
			result["primary_action_committed"] = false
			result["phase"] = "action"
			result["failure_code"] = code
			if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, result)); err != nil {
				t.Fatalf("valid risk revalidation failure rejected: %v", err)
			}
		})
	}
}

func TestCoordinateMouseEventV1AcceptsPointOccludedAsFailClosedPreflight(t *testing.T) {
	if _, err := DecodeCoordinateMouseEventResultV1(
		loadCoordinateFixture(t, "coordinate_mouse_event.response.failed.point_occluded.v1.json"),
	); err != nil {
		t.Fatalf("point-occluded preflight result rejected: %v", err)
	}
}

func TestCoordinateMouseEventV1AcceptsStrictRawRequestFailureWithUnknownAction(t *testing.T) {
	result := coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.response.failed.stale_topology.v1.json"))
	result["action"] = "unknown"
	result["failure_code"] = "invalid_request"
	if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, result)); err != nil {
		t.Fatalf("strict raw-request failure rejected: %v", err)
	}
	result["failure_code"] = "stale_topology"
	if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, result)); err == nil {
		t.Fatal("unknown action accepted outside invalid_request failure tuple")
	}
	completed := coordinateMouseJSONMap(t, loadCoordinateFixture(t, "coordinate_mouse_event.response.click.completed_unverified.v1.json"))
	completed["action"] = "unknown"
	if _, err := DecodeCoordinateMouseEventResultV1(marshalCoordinateMouseJSON(t, completed)); err == nil {
		t.Fatal("completed_unverified accepted unknown action")
	}
}

func TestAXClientCoordinateMouseEventV1PreCancelledDoesNotWrite(t *testing.T) {
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := client.coordinateMouseEventV1(ctx, canonicalCoordinateMouseRequest(t, "move"))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("pre-cancel error = %v", err)
	}
	if writer.writeCount() != 0 {
		t.Fatalf("pre-cancelled mutation wrote %d RPCs", writer.writeCount())
	}
}

func TestAXClientCoordinateMouseEventV1AfterWriteAmbiguityIsTypedAndNeverRetried(t *testing.T) {
	t.Run("context cancelled waits for typed helper acknowledgement", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		release := make(chan struct{})
		acknowledgement := loadCoordinateFixture(
			t, "coordinate_mouse_event.response.click.completed_unverified.v1.json")
		writer := &coordinateMouseTestWriter{}
		client := coordinateMouseTestClient(writer)
		writer.afterWrite = func(requestBytes []byte) {
			cancel()
			go func() {
				<-release
				envelope, err := DecodeCoordinateMouseEventRPCRequestV1(bytes.TrimSpace(requestBytes))
				if err != nil {
					return
				}
				client.pendingMu.Lock()
				responseChannel := client.pending[envelope.ID]
				client.pendingMu.Unlock()
				responseChannel <- AXResponse{
					ID:     envelope.ID,
					Result: acknowledgement,
				}
			}()
		}
		request := canonicalCoordinateMouseRequest(t, "click")
		request.CommitDeadlineAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
		type outcome struct {
			result CoordinateMouseEventResultV1
			err    error
		}
		done := make(chan outcome, 1)
		go func() {
			result, err := client.coordinateMouseEventV1(ctx, request)
			done <- outcome{result: result, err: err}
		}()
		select {
		case got := <-done:
			t.Fatalf("coordinate mutation returned before helper acknowledgement: %+v %v", got.result, got.err)
		case <-time.After(30 * time.Millisecond):
		}
		close(release)
		select {
		case got := <-done:
			if got.err != nil {
				t.Fatalf("acknowledged coordinate mutation error = %v", got.err)
			}
			if got.result.Status != "completed_unverified" || !got.result.PrimaryActionCommitted {
				t.Fatalf("typed helper acknowledgement lost: %+v", got.result)
			}
		case <-time.After(time.Second):
			t.Fatal("coordinate mutation did not return after helper acknowledgement")
		}
		if writer.writeCount() != 1 {
			t.Fatalf("acknowledged mutation wrote %d RPCs", writer.writeCount())
		}
	})

	t.Run("helper acknowledgement timeout", func(t *testing.T) {
		writer := &coordinateMouseTestWriter{}
		client := coordinateMouseTestClient(writer)
		request := canonicalCoordinateMouseRequest(t, "click")
		request.CommitDeadlineAt = time.Now().Add(20 * time.Millisecond).UTC().Format(time.RFC3339Nano)
		started := time.Now()
		_, err := client.coordinateMouseEventV1(context.Background(), request)
		assertCoordinateMouseCommitUnknown(t, err, nil)
		elapsed := time.Since(started)
		if elapsed < 100*time.Millisecond || elapsed > 500*time.Millisecond {
			t.Fatalf("coordinate acknowledgement timeout elapsed %v, want deadline plus bounded grace", elapsed)
		}
		if writer.writeCount() != 1 {
			t.Fatalf("timed out mutation wrote %d RPCs", writer.writeCount())
		}
	})

	t.Run("transport write", func(t *testing.T) {
		writer := &coordinateMouseTestWriter{writeErr: io.ErrClosedPipe}
		client := coordinateMouseTestClient(writer)
		_, err := client.coordinateMouseEventV1(context.Background(), canonicalCoordinateMouseRequest(t, "click"))
		assertCoordinateMouseCommitUnknown(t, err, io.ErrClosedPipe)
		if writer.writeCount() != 1 {
			t.Fatalf("ambiguous mutation wrote %d RPCs", writer.writeCount())
		}
	})

	t.Run("EOF awaiting result", func(t *testing.T) {
		writer := &coordinateMouseTestWriter{}
		client := coordinateMouseTestClient(writer)
		writer.afterWrite = func([]byte) { client.readLoop(strings.NewReader("")) }
		_, err := client.coordinateMouseEventV1(context.Background(), canonicalCoordinateMouseRequest(t, "click"))
		assertCoordinateMouseCommitUnknown(t, err, nil)
		if writer.writeCount() != 1 {
			t.Fatalf("ambiguous mutation wrote %d RPCs", writer.writeCount())
		}
	})
}

func TestAXClientCoordinateMouseEventV1ReturnsExplicitHelperFailure(t *testing.T) {
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	writer.afterWrite = func(requestBytes []byte) {
		envelope, err := DecodeCoordinateMouseEventRPCRequestV1(bytes.TrimSpace(requestBytes))
		if err != nil {
			t.Error(err)
			return
		}
		client.pendingMu.Lock()
		responseChannel := client.pending[envelope.ID]
		client.pendingMu.Unlock()
		responseChannel <- AXResponse{
			ID:     envelope.ID,
			Result: loadCoordinateFixture(t, "coordinate_mouse_event.response.failed.stale_topology.v1.json"),
		}
	}

	result, err := client.coordinateMouseEventV1(
		context.Background(),
		canonicalCoordinateMouseRequest(t, "click"))
	if err != nil {
		t.Fatalf("explicit helper failure became transport ambiguity: %v", err)
	}
	if result.Status != "failed" || result.FailureCode == nil || *result.FailureCode != "stale_topology" || result.RetrySafe {
		t.Fatalf("helper failure lost typed result: %+v", result)
	}
	if writer.writeCount() != 1 {
		t.Fatalf("helper failure wrote %d RPCs", writer.writeCount())
	}
}

func canonicalCoordinateMouseRequest(t *testing.T, action string) CoordinateMouseEventRequestV1 {
	t.Helper()
	name := "coordinate_mouse_event.request." + action + ".v1.json"
	envelope, err := DecodeCoordinateMouseEventRPCRequestV1(loadCoordinateFixture(t, name))
	if err != nil {
		t.Fatal(err)
	}
	return envelope.Params
}

type coordinateMouseTestWriter struct {
	mu         sync.Mutex
	writes     [][]byte
	afterWrite func([]byte)
	writeErr   error
}

func (writer *coordinateMouseTestWriter) Write(data []byte) (int, error) {
	copyOfData := append([]byte(nil), data...)
	writer.mu.Lock()
	writer.writes = append(writer.writes, copyOfData)
	writeErr := writer.writeErr
	afterWrite := writer.afterWrite
	writer.mu.Unlock()
	if afterWrite != nil {
		afterWrite(copyOfData)
	}
	if writeErr != nil {
		return 0, writeErr
	}
	return len(data), nil
}

func (writer *coordinateMouseTestWriter) Close() error { return nil }

func (writer *coordinateMouseTestWriter) writeCount() int {
	writer.mu.Lock()
	defer writer.mu.Unlock()
	return len(writer.writes)
}

func coordinateMouseTestClient(writer io.WriteCloser) *AXClient {
	return &AXClient{
		writer:  writer,
		started: true,
		pending: make(map[int64]chan AXResponse),
	}
}

func assertCoordinateMouseCommitUnknown(t *testing.T, err error, cause error) {
	t.Helper()
	var commitUnknown *CoordinateMouseEventCommitUnknownErrorV1
	if !errors.As(err, &commitUnknown) {
		t.Fatalf("error %T %v is not typed commit-unknown", err, err)
	}
	if commitUnknown.RetrySafe() {
		t.Fatal("commit-unknown mutation was marked retry-safe")
	}
	if cause != nil && !errors.Is(err, cause) {
		t.Fatalf("commit-unknown error %v does not wrap %v", err, cause)
	}
}

func coordinateMouseJSONMap(t *testing.T, payload []byte) map[string]any {
	t.Helper()
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()
	var value map[string]any
	if err := decoder.Decode(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

func cloneCoordinateMouseJSONMap(t *testing.T, value map[string]any) map[string]any {
	t.Helper()
	return coordinateMouseJSONMap(t, marshalCoordinateMouseJSON(t, value))
}

func marshalCoordinateMouseJSON(t *testing.T, value map[string]any) []byte {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}
