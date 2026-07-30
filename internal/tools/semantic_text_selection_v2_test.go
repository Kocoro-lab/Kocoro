package tools

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"
)

func TestSemanticTextSelectionV2CanonicalFixturesAndStrictWire(t *testing.T) {
	requestPayload := loadCoordinateFixture(t, "semantic_text_selection.request.v2.json")
	request, err := DecodeSemanticTextSelectionRPCRequestV2(requestPayload)
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != 703 || request.Method != "semantic_text_selection_v2" ||
		request.Params.SchemaVersion != 2 || request.Params.Ref != "e1" ||
		request.Params.FallbackPolicy != "report_unsupported" {
		t.Fatalf("request = %+v", request)
	}
	encodedRequest, err := EncodeSemanticTextSelectionRPCRequestV2(request)
	if err != nil {
		t.Fatal(err)
	}
	assertCoordinateJSONRoundTrip(t, requestPayload, encodedRequest)
	for _, mutation := range [][]byte{
		[]byte(strings.Replace(string(requestPayload), `"fallback_policy": "report_unsupported"`, `"fallback_policy": "report_unsupported", "unknown": true`, 1)),
		[]byte(strings.Replace(string(requestPayload), `"pid": 42`, `"pid": 42, "pid": 43`, 1)),
	} {
		if _, err := DecodeSemanticTextSelectionRPCRequestV2(mutation); err == nil {
			t.Fatalf("strict request accepted %s", mutation)
		}
	}

	for _, name := range []string{
		"semantic_text_selection.response.verified.v2.json",
		"semantic_text_selection.response.mismatch.v2.json",
		"semantic_text_selection.response.fallback_required.v2.json",
		"semantic_text_selection.response.failed.v2.json",
		"semantic_text_selection.response.user_interference_precommit.v2.json",
		"semantic_text_selection.response.user_interference_postcommit.v2.json",
		"semantic_text_selection.response.commit_unknown.v2.json",
	} {
		result, err := DecodeSemanticTextSelectionResultV2(loadCoordinateFixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.SchemaVersion != 2 || result.RetrySafe {
			t.Fatalf("%s = %+v", name, result)
		}
		encoded, err := EncodeSemanticTextSelectionResultV2(result)
		if err != nil {
			t.Fatalf("%s encode: %v", name, err)
		}
		assertCoordinateJSONRoundTrip(t, loadCoordinateFixture(t, name), encoded)
	}

	invalidVerified := []byte(`{"schema_version":2,"status":"verified","commit_state":"unknown","phase":"post_verification","failure_code":null,"retry_safe":false,"postcondition":"selected_range_matches","selected_range":{"location":5,"length":12}}`)
	if _, err := DecodeSemanticTextSelectionResultV2(invalidVerified); err == nil {
		t.Fatal("semantic selection accepted verified with unknown commit state")
	}
	unknownField := []byte(`{"schema_version":2,"status":"completed_unverified","commit_state":"unknown","phase":"action","failure_code":"ax_selection_commit_unknown","retry_safe":false,"postcondition":null,"selected_range":null,"selection_content":"secret"}`)
	if _, err := DecodeSemanticTextSelectionResultV2(unknownField); err == nil {
		t.Fatal("semantic selection accepted unknown result field")
	}
	duplicateResult := []byte(`{"schema_version":2,"status":"completed_unverified","commit_state":"unknown","commit_state":"committed","phase":"action","failure_code":"ax_selection_commit_unknown","retry_safe":false,"postcondition":null,"selected_range":null}`)
	if _, err := DecodeSemanticTextSelectionResultV2(duplicateResult); err == nil {
		t.Fatal("semantic selection accepted duplicate result field")
	}
}

func TestAXClientSemanticTextSelectionV2UsesDedicatedTypedMutationTransport(t *testing.T) {
	envelope, err := DecodeSemanticTextSelectionRPCRequestV2(
		loadCoordinateFixture(t, "semantic_text_selection.request.v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.Params
	request.CommitDeadlineAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	w := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(w)
	w.afterWrite = func(payload []byte) {
		written, decodeErr := DecodeSemanticTextSelectionRPCRequestV2(payload)
		if decodeErr != nil {
			t.Errorf("typed request on wire: %v", decodeErr)
			return
		}
		if written.Method != "semantic_text_selection_v2" || written.Params != request {
			t.Errorf("wire request = %+v", written)
			return
		}
		client.pendingMu.Lock()
		response := client.pending[written.ID]
		client.pendingMu.Unlock()
		response <- AXResponse{
			ID: written.ID,
			Result: loadCoordinateFixture(
				t, "semantic_text_selection.response.verified.v2.json"),
		}
	}
	result, err := client.semanticTextSelectionV2(context.Background(), request)
	if err != nil || result.Status != "verified" || result.CommitState != "committed" {
		t.Fatalf("typed result=%+v err=%v", result, err)
	}
	if w.writeCount() != 1 {
		t.Fatalf("writes = %d", w.writeCount())
	}
}

func TestAXClientSemanticTextSelectionV2PreservesExplicitObservedMismatch(t *testing.T) {
	envelope, err := DecodeSemanticTextSelectionRPCRequestV2(
		loadCoordinateFixture(t, "semantic_text_selection.request.v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.Params
	request.CommitDeadlineAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	w := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(w)
	w.afterWrite = func(payload []byte) {
		written, decodeErr := DecodeSemanticTextSelectionRPCRequestV2(payload)
		if decodeErr != nil {
			t.Error(decodeErr)
			return
		}
		client.pendingMu.Lock()
		response := client.pending[written.ID]
		client.pendingMu.Unlock()
		response <- AXResponse{
			ID: written.ID,
			Result: loadCoordinateFixture(
				t, "semantic_text_selection.response.mismatch.v2.json"),
		}
	}
	result, err := client.semanticTextSelectionV2(context.Background(), request)
	if err != nil || result.Status != "completed_unverified" ||
		result.CommitState != "committed" || result.SelectedRange == nil ||
		*result.SelectedRange == request.Range {
		t.Fatalf("explicit mismatch result=%+v err=%v", result, err)
	}
}

func TestAXClientSemanticTextSelectionV2PostWriteAmbiguityIsCommitUnknown(t *testing.T) {
	envelope, err := DecodeSemanticTextSelectionRPCRequestV2(
		loadCoordinateFixture(t, "semantic_text_selection.request.v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.Params
	request.CommitDeadlineAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	for _, test := range []struct {
		name   string
		writer *coordinateMouseTestWriter
	}{
		{name: "write", writer: &coordinateMouseTestWriter{writeErr: io.ErrClosedPipe}},
		{name: "malformed acknowledgement", writer: &coordinateMouseTestWriter{}},
		{name: "mismatched selected range", writer: &coordinateMouseTestWriter{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			client := coordinateMouseTestClient(test.writer)
			if test.name != "write" {
				test.writer.afterWrite = func(payload []byte) {
					written, decodeErr := DecodeSemanticTextSelectionRPCRequestV2(payload)
					if decodeErr != nil {
						t.Error(decodeErr)
						return
					}
					client.pendingMu.Lock()
					response := client.pending[written.ID]
					client.pendingMu.Unlock()
					if test.name == "malformed acknowledgement" {
						response <- AXResponse{ID: written.ID, Result: []byte(`{"schema_version":2,"status":"verified"}`)}
						return
					}
					response <- AXResponse{ID: written.ID, Result: []byte(`{"schema_version":2,"status":"verified","commit_state":"committed","phase":"post_verification","failure_code":null,"retry_safe":false,"postcondition":"selected_range_matches","selected_range":{"location":6,"length":12}}`)}
				}
			}
			_, err := client.semanticTextSelectionV2(context.Background(), request)
			var commitUnknown *SemanticTextSelectionCommitUnknownErrorV2
			if !errors.As(err, &commitUnknown) || commitUnknown.RetrySafe() {
				t.Fatalf("error %T %v is not non-retryable commit unknown", err, err)
			}
		})
	}
}
