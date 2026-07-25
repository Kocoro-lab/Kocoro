package tools

import (
	"context"
	"errors"
	"io"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestSemanticPressV2CanonicalFixturesAndStrictWire(t *testing.T) {
	requestPayload := loadSemanticPressFixture(t, "semantic_press.request.v2.json")
	request, err := DecodeSemanticPressRPCRequestV2(requestPayload)
	if err != nil {
		t.Fatal(err)
	}
	if request.ID != 702 || request.Method != "semantic_press_v2" || request.Params.SchemaVersion != 2 ||
		request.Params.Ref != "e1" || request.Params.BundleID != "com.apple.Notes" {
		t.Fatalf("request = %+v", request)
	}
	for _, mutation := range [][]byte{
		[]byte(strings.Replace(string(requestPayload), `"fallback_policy": "none"`, `"fallback_policy": "none", "unknown": true`, 1)),
		[]byte(strings.Replace(string(requestPayload), `"pid": 42`, `"pid": 42, "pid": 43`, 1)),
	} {
		if _, err := DecodeSemanticPressRPCRequestV2(mutation); err == nil {
			t.Fatalf("strict request accepted %s", mutation)
		}
	}

	for _, name := range []string{
		"semantic_press.response.completed_unverified.v2.json",
		"semantic_press.response.user_interference_precommit.v2.json",
		"semantic_press.response.user_interference_postcommit.v2.json",
		"semantic_press.response.failed.v2.json",
		"semantic_press.response.monitor_lost_postcommit.v2.json",
		"semantic_press.response.commit_unknown.v2.json",
	} {
		result, err := DecodeSemanticPressResultV2(loadSemanticPressFixture(t, name))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if result.SchemaVersion != 2 || result.RetrySafe {
			t.Fatalf("%s = %+v", name, result)
		}
		if result.CommitState != "not_committed" && result.CommitState != "committed" &&
			result.CommitState != "unknown" {
			t.Fatalf("%s invalid commit state = %+v", name, result)
		}
	}
	unsupportedVerified := []byte(`{"schema_version":2,"status":"verified","commit_state":"committed","phase":"post_verification","failure_code":null,"postcondition":"target_changed","retry_safe":false}`)
	if _, err := DecodeSemanticPressResultV2(unsupportedVerified); err == nil {
		t.Fatal("semantic_press_v2 accepted verified without a declared causal predicate")
	}
}

func TestAXClientSemanticPressV2UsesDedicatedTypedMutationTransport(t *testing.T) {
	envelope, err := DecodeSemanticPressRPCRequestV2(
		loadSemanticPressFixture(t, "semantic_press.request.v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	request := envelope.Params
	request.CommitDeadlineAt = time.Now().Add(time.Second).UTC().Format(time.RFC3339Nano)
	w := &coordinateMouseTestWriter{}
	client := coordinateMouseTestClient(w)
	w.afterWrite = func(payload []byte) {
		written, decodeErr := DecodeSemanticPressRPCRequestV2(payload)
		if decodeErr != nil {
			t.Errorf("typed request on wire: %v", decodeErr)
			return
		}
		if written.Method != "semantic_press_v2" || written.Params != request {
			t.Errorf("wire request = %+v", written)
			return
		}
		client.pendingMu.Lock()
		response := client.pending[written.ID]
		client.pendingMu.Unlock()
		response <- AXResponse{
			ID: written.ID,
			Result: loadSemanticPressFixture(
				t, "semantic_press.response.commit_unknown.v2.json"),
		}
	}
	result, err := client.semanticPressV2(context.Background(), request)
	if err != nil || result.CommitState != "unknown" || result.RetrySafe {
		t.Fatalf("typed result=%+v err=%v", result, err)
	}
	if w.writeCount() != 1 {
		t.Fatalf("writes = %d", w.writeCount())
	}
}

func TestAXClientSemanticPressV2PostWriteAmbiguityIsCommitUnknown(t *testing.T) {
	envelope, err := DecodeSemanticPressRPCRequestV2(
		loadSemanticPressFixture(t, "semantic_press.request.v2.json"))
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
	} {
		t.Run(test.name, func(t *testing.T) {
			client := coordinateMouseTestClient(test.writer)
			if test.name == "malformed acknowledgement" {
				test.writer.afterWrite = func(payload []byte) {
					written, decodeErr := DecodeSemanticPressRPCRequestV2(payload)
					if decodeErr != nil {
						t.Error(decodeErr)
						return
					}
					client.pendingMu.Lock()
					response := client.pending[written.ID]
					client.pendingMu.Unlock()
					response <- AXResponse{ID: written.ID, Result: []byte(
						`{"schema_version":2,"status":"failed","press_committed":false,"phase":"preflight","failure_code":"path_not_found","postcondition":null,"retry_safe":false}`)}
				}
			}
			_, err := client.semanticPressV2(context.Background(), request)
			var commitUnknown *SemanticPressCommitUnknownErrorV2
			if !errors.As(err, &commitUnknown) || commitUnknown.RetrySafe() {
				t.Fatalf("error %T %v is not non-retryable commit unknown", err, err)
			}
		})
	}
}

func TestSemanticPressV2TransportUsesTypedEnvelopeAndStrictAck(t *testing.T) {
	client := &AXClient{}
	request, err := DecodeSemanticPressRPCRequestV2(loadSemanticPressFixture(t, "semantic_press.request.v2.json"))
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "darwin" {
		if _, err := client.SemanticPressV2(context.Background(), request.Params); err == nil {
			t.Fatal("non-macOS transport did not fail closed")
		}
	}
	if err := request.Params.Validate(); err != nil {
		t.Fatal(err)
	}
	invalid := request.Params
	invalid.FallbackPolicy = "synthetic"
	if err := invalid.Validate(); err == nil {
		t.Fatal("invalid fallback was accepted")
	}
	invalid = request.Params
	invalid.CommitDeadlineAt = time.Now().Add(5 * time.Second).Format(time.RFC3339Nano)
	if err := invalid.Validate(); err != nil {
		t.Fatal(err)
	}

	unknown := newSemanticPressCommitUnknownV2(errors.New("closed"))
	var typed *SemanticPressCommitUnknownErrorV2
	if !errors.As(unknown, &typed) || typed.RetrySafe() || !typed.CommitUnknown() {
		t.Fatalf("commit-unknown error = %T %v", unknown, unknown)
	}
}
