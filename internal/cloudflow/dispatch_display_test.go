package cloudflow

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
)

// TestRun_CloudAgentMessage_IsUnprefixedAndUsesCloudText asserts the daemon
// forwards cloud agent events WITHOUT baking a "[agentID]" prefix into the
// message (the agent_id is a separate structured field) and without overriding
// cloud's human-friendly text with a hardcoded English fallback.
//
// Go imports are per-file: this new test file declares its own internal/client
// import even though dispatch_test.go has one. captureHandler is package-level
// and reused here without redeclaring.
func TestRun_CloudAgentMessage_IsUnprefixedAndUsesCloudText(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/api/v1/tasks/stream"):
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			json.NewEncoder(w).Encode(map[string]any{"workflow_id": "wf-1", "task_id": "t-1"})
		case strings.HasPrefix(r.URL.Path, "/api/v1/stream/sse"):
			w.Header().Set("Content-Type", "text/event-stream")
			// Cloud sends a human-friendly message and a named agent.
			fmt.Fprintf(w, "event: AGENT_STARTED\ndata: %s\n\n", `{"agent_id":"Todoroki","message":"Todoroki is on it"}`)
			fmt.Fprintf(w, "event: thread.message.completed\ndata: %s\n\n", `{"response":"done"}`)
			fmt.Fprintf(w, "event: WORKFLOW_COMPLETED\ndata: {}\n\n")
			fmt.Fprintf(w, "event: done\ndata: [DONE]\n\n")
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/v1/tasks/"):
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"task_id": "t-1", "status": "TASK_STATUS_COMPLETED", "result": "done"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	gw := client.NewGatewayClient(srv.URL, "k")
	h := &captureHandler{}
	if _, err := Run(context.Background(), Request{Gateway: gw, APIKey: "k", Query: "q"}, h); err != nil {
		t.Fatalf("Run: %v", err)
	}
	// captureHandler.OnCloudAgent records "status:message".
	var startedMsg string
	for _, rec := range h.cloudAgents {
		if m, ok := strings.CutPrefix(rec, "started:"); ok {
			startedMsg = m
		}
	}
	if startedMsg == "" {
		t.Fatalf("no started cloud agent event captured: %v", h.cloudAgents)
	}
	if strings.Contains(startedMsg, "[Todoroki]") {
		t.Fatalf("message must NOT contain the [agentID] prefix, got %q", startedMsg)
	}
	if startedMsg != "Todoroki is on it" {
		t.Fatalf("message should be cloud's raw text, got %q", startedMsg)
	}
}
