//go:build darwin && cgo

package koe

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSendRealtimeUsage(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/koe/realtime/usage" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		gotBody, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"cost_usd":0.01}`))
	}))
	defer srv.Close()

	err := NewDaemonClient(srv.URL).SendRealtimeUsage(context.Background(), json.RawMessage(`{"model":"m","response_id":"r1"}`))
	if err != nil {
		t.Fatalf("SendRealtimeUsage: %v", err)
	}
	if !strings.Contains(string(gotBody), `"response_id":"r1"`) {
		t.Errorf("daemon did not receive the usage body; got %s", gotBody)
	}
}

func TestReportUsageBuildsBillingBody(t *testing.T) {
	var got json.RawMessage
	h := &eventHandler{model: "gpt-realtime-mini-2025-12-15", onUsage: func(b json.RawMessage) { got = b }}
	h.reportUsage([]byte(`{"type":"response.done","response":{"id":"resp_1","usage":{"total_tokens":42,"output_token_details":{"audio_tokens":30}}}}`))

	if got == nil {
		t.Fatal("onUsage was not fired for a response.done carrying usage")
	}
	s := string(got)
	if !strings.Contains(s, `"response_id":"resp_1"`) ||
		!strings.Contains(s, `"model":"gpt-realtime-mini-2025-12-15"`) ||
		!strings.Contains(s, `"total_tokens":42`) {
		t.Errorf("billing body missing fields: %s", s)
	}
}

func TestReportUsageSkipsWhenNoUsage(t *testing.T) {
	fired := false
	h := &eventHandler{onUsage: func(json.RawMessage) { fired = true }}
	h.reportUsage([]byte(`{"type":"response.done","response":{"id":"resp_1"}}`)) // no usage field
	if fired {
		t.Error("onUsage fired for a response.done with no usage")
	}
}
