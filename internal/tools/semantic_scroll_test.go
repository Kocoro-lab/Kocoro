package tools

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func loadSemanticScrollFixture(t *testing.T, name string) []byte {
	t.Helper()
	payload, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return payload
}

func TestSemanticScrollV1StrictWireAndTaggedResults(t *testing.T) {
	envelope, err := DecodeSemanticScrollRPCRequestV1(
		loadSemanticScrollFixture(t, "semantic_scroll.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	if envelope.ID != 907 || envelope.Method != "semantic_scroll_v1" ||
		envelope.Params.Axis != "vertical" || envelope.Params.Direction != "increment" ||
		envelope.Params.Steps != 3 {
		t.Fatalf("request=%+v", envelope)
	}
	for _, name := range []string{
		"semantic_scroll.response.verified.v1.json",
		"semantic_scroll.response.fallback_required.v1.json",
		"semantic_scroll.response.commit_unknown.v1.json",
	} {
		result, err := DecodeSemanticScrollResultV1(loadSemanticScrollFixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.RetrySafe {
			t.Fatalf("%s claimed retry safety: %+v", name, result)
		}
	}
	code := "controller_cancelled"
	for _, result := range []SemanticScrollResultV1{
		{
			SchemaVersion: 1, Status: "cancelled", CommitState: "not_committed",
			Phase: "cancelled", FailureCode: &code, StepsCompleted: 1, ExpectedSteps: 3,
		},
		{
			SchemaVersion: 1, Status: "user_interference", CommitState: "not_committed",
			Phase: "user_interference", FailureCode: func() *string {
				value := "physical_input_interference"
				return &value
			}(), StepsCompleted: 1, ExpectedSteps: 3,
		},
	} {
		if err := result.ValidateTaggedUnion(); err == nil {
			t.Fatalf("accepted not_committed result with completed steps: %+v", result)
		}
	}
}

func TestSemanticScrollV1RejectsUnknownDuplicateAndInvalidAuthority(t *testing.T) {
	valid := loadSemanticScrollFixture(t, "semantic_scroll.request.v1.json")
	unknown := append([]byte(nil), valid...)
	unknown = []byte(string(unknown[:len(unknown)-3]) + ",\n  \"unknown\": true\n}\n")
	if _, err := DecodeSemanticScrollRPCRequestV1(unknown); err == nil {
		t.Fatal("accepted unknown request member")
	}
	duplicate := []byte(`{"id":907,"id":908,"method":"semantic_scroll_v1","params":{}}`)
	if _, err := DecodeSemanticScrollRPCRequestV1(duplicate); err == nil {
		t.Fatal("accepted duplicate request member")
	}
	envelope, err := DecodeSemanticScrollRPCRequestV1(valid)
	if err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*SemanticScrollRequestV1){
		func(r *SemanticScrollRequestV1) { r.Ref = "" },
		func(r *SemanticScrollRequestV1) { r.ExpectedFingerprint = "" },
		func(r *SemanticScrollRequestV1) { r.Axis = "diagonal" },
		func(r *SemanticScrollRequestV1) { r.Direction = "page" },
		func(r *SemanticScrollRequestV1) { r.Steps = 0 },
		func(r *SemanticScrollRequestV1) { r.Steps = 11 },
		func(r *SemanticScrollRequestV1) { r.FallbackPolicy = "synthetic" },
	} {
		request := envelope.Params
		mutate(&request)
		if err := request.Validate(); err == nil {
			t.Fatalf("accepted invalid request: %+v", request)
		}
	}
}

func TestAXClientSemanticScrollV1UsesTypedTransportAndClassifiesAmbiguity(t *testing.T) {
	envelope, err := DecodeSemanticScrollRPCRequestV1(
		loadSemanticScrollFixture(t, "semantic_scroll.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.Params
	request.CommitDeadlineAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)

	t.Run("typed acknowledgement", func(t *testing.T) {
		writer := &coordinateMouseTestWriter{}
		client := coordinateMouseTestClient(writer)
		writer.afterWrite = func(payload []byte) {
			written, decodeErr := DecodeSemanticScrollRPCRequestV1(payload)
			if decodeErr != nil {
				t.Error(decodeErr)
				return
			}
			client.pendingMu.Lock()
			response := client.pending[written.ID]
			client.pendingMu.Unlock()
			response <- AXResponse{ID: written.ID, Result: loadSemanticScrollFixture(
				t, "semantic_scroll.response.verified.v1.json")}
		}
		result, err := client.semanticScrollV1(context.Background(), request)
		if err != nil || result.Status != "verified" || writer.writeCount() != 1 {
			t.Fatalf("result=%+v err=%v writes=%d", result, err, writer.writeCount())
		}
	})

	for _, test := range []struct {
		name   string
		writer *coordinateMouseTestWriter
	}{
		{name: "write failure", writer: &coordinateMouseTestWriter{writeErr: io.ErrClosedPipe}},
		{name: "malformed acknowledgement", writer: &coordinateMouseTestWriter{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := coordinateMouseTestClient(test.writer)
			if test.name == "malformed acknowledgement" {
				test.writer.afterWrite = func(payload []byte) {
					written, _ := DecodeSemanticScrollRPCRequestV1(payload)
					client.pendingMu.Lock()
					response := client.pending[written.ID]
					client.pendingMu.Unlock()
					response <- AXResponse{ID: written.ID, Result: []byte(`{"status":"verified"}`)}
				}
			}
			_, err := client.semanticScrollV1(context.Background(), request)
			var unknown *SemanticScrollCommitUnknownErrorV1
			if !errors.As(err, &unknown) || unknown.RetrySafe() {
				t.Fatalf("error %T %v is not commit unknown", err, err)
			}
		})
	}
}

func TestAXClientSemanticScrollV1CancellationBeforeWriteHasNoSideEffect(t *testing.T) {
	envelope, err := DecodeSemanticScrollRPCRequestV1(
		loadSemanticScrollFixture(t, "semantic_scroll.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.Params
	request.CommitDeadlineAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = client.semanticScrollV1(ctx, request)
	if !errors.Is(err, context.Canceled) || writer.writeCount() != 0 {
		t.Fatalf("err=%v writes=%d", err, writer.writeCount())
	}
}

func TestSemanticScrollCancellationMarkerMatchesCrossLanguageFixture(t *testing.T) {
	envelope, err := DecodeSemanticScrollRPCRequestV1(
		loadSemanticScrollFixture(t, "semantic_scroll.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		RequestID int64  `json:"request_id"`
		Basename  string `json:"basename"`
	}
	if err := json.Unmarshal(loadSemanticScrollFixture(
		t, "semantic_scroll.cancellation_marker.v1.json"), &fixture); err != nil {
		t.Fatal(err)
	}
	got := filepath.Base(semanticScrollCancellationMarkerPathV1(fixture.RequestID, envelope.Params))
	if got != fixture.Basename {
		t.Fatalf("marker basename=%q want=%q", got, fixture.Basename)
	}
}

func TestAXClientSemanticScrollV1CancellationAfterWriteWaitsForTypedHelperAck(t *testing.T) {
	envelope, err := DecodeSemanticScrollRPCRequestV1(
		loadSemanticScrollFixture(t, "semantic_scroll.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.Params
	request.CommitDeadlineAt = time.Now().Add(3 * time.Second).UTC().Format(time.RFC3339Nano)
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	ctx, cancel := context.WithCancel(context.Background())
	requestIDs := make(chan int64, 1)
	writer.afterWrite = func(payload []byte) {
		written, decodeErr := DecodeSemanticScrollRPCRequestV1(payload)
		if decodeErr != nil {
			t.Error(decodeErr)
			return
		}
		requestIDs <- written.ID
		cancel()
	}
	type outcome struct {
		result SemanticScrollResultV1
		err    error
	}
	done := make(chan outcome, 1)
	go func() {
		result, callErr := client.semanticScrollV1(ctx, request)
		done <- outcome{result: result, err: callErr}
	}()
	requestID := <-requestIDs
	select {
	case early := <-done:
		t.Fatalf("cancel released action before helper ack: %+v %v", early.result, early.err)
	case <-time.After(40 * time.Millisecond):
	}
	marker := semanticScrollCancellationMarkerPathV1(requestID, request)
	if info, err := os.Stat(marker); err != nil {
		t.Fatalf("cancel marker missing before ack: %v", err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("cancel marker permissions=%#o want 0600", info.Mode().Perm())
	}
	code := "controller_cancelled"
	ack := SemanticScrollResultV1{
		SchemaVersion: 1, Status: "cancelled", CommitState: "committed",
		Phase: "cancelled", FailureCode: &code, StepsCompleted: 1,
		ExpectedSteps: request.Steps,
	}
	payload, err := json.Marshal(ack)
	if err != nil {
		t.Fatal(err)
	}
	client.pendingMu.Lock()
	response := client.pending[requestID]
	client.pendingMu.Unlock()
	response <- AXResponse{ID: requestID, Result: payload}
	select {
	case got := <-done:
		if got.err != nil || got.result.Status != "cancelled" || got.result.StepsCompleted != 1 {
			t.Fatalf("typed cancellation=%+v err=%v", got.result, got.err)
		}
	case <-time.After(time.Second):
		t.Fatal("scroll did not return after helper cancellation ack")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cancel marker survived ack: %v", err)
	}
}

func TestAXClientSemanticScrollV1HardTimeoutRetainsCancellationFence(t *testing.T) {
	envelope, err := DecodeSemanticScrollRPCRequestV1(
		loadSemanticScrollFixture(t, "semantic_scroll.request.v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.Params
	request.CommitDeadlineAt = time.Now().Add(20 * time.Millisecond).UTC().Format(time.RFC3339Nano)
	writer := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(writer)
	requestIDs := make(chan int64, 1)
	writer.afterWrite = func(payload []byte) {
		written, decodeErr := DecodeSemanticScrollRPCRequestV1(payload)
		if decodeErr != nil {
			t.Error(decodeErr)
			return
		}
		requestIDs <- written.ID
	}
	_, err = client.semanticScrollV1(context.Background(), request)
	var unknown *SemanticScrollCommitUnknownErrorV1
	if !errors.As(err, &unknown) {
		t.Fatalf("hard timeout error = %T %v, want commit unknown", err, err)
	}
	requestID := <-requestIDs
	marker := semanticScrollCancellationMarkerPathV1(requestID, request)
	defer os.Remove(marker)
	if info, statErr := os.Stat(marker); statErr != nil {
		t.Fatalf("hard timeout released cancellation fence: %v", statErr)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("hard-timeout marker permissions=%#o want 0600", info.Mode().Perm())
	}
}
