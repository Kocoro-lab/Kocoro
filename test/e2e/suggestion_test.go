package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// fakeGateway mimics the cloud gateway's POST /v1/completions endpoint for
// offline E2E coverage. It captures every CompletionRequest it sees so the
// test can assert on the forked-request fields (ForkedKind, SkipCacheWrite)
// that the suggestion path sets.
type fakeGateway struct {
	mu       sync.Mutex
	requests []client.CompletionRequest
	// reply is what the gateway returns for every call.
	reply string
}

func (g *fakeGateway) requestCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.requests)
}

func (g *fakeGateway) requestAt(i int) client.CompletionRequest {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.requests[i]
}

func (g *fakeGateway) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/completions", func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		var req client.CompletionRequest
		if err := json.Unmarshal(raw, &req); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		g.mu.Lock()
		g.requests = append(g.requests, req)
		reply := g.reply
		g.mu.Unlock()

		resp := client.CompletionResponse{
			Provider:   "anthropic",
			Model:      "claude-sonnet-4-6",
			OutputText: reply,
			Usage: client.Usage{
				InputTokens:     1200,
				OutputTokens:    30,
				CacheReadTokens: 1150,
				CostUSD:         0.0042,
			},
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(resp)
	})
	return mux
}

// newSuggestionDeps wires the minimum daemon state needed to drive the
// prompt-suggestion handlers: an AgentsDir with "myagent", a SessionCache
// rooted in a temp shannon dir, and a pre-seeded session.
func newSuggestionDeps(t *testing.T, gw *client.GatewayClient) (*daemon.ServerDeps, string, string) {
	t.Helper()

	shannonDir := t.TempDir()
	agentsDir := filepath.Join(shannonDir, "agents")
	if err := os.MkdirAll(filepath.Join(agentsDir, "myagent"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, "myagent", "AGENT.md"), []byte("# myagent\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	const sessionID = "sess-e2e"
	sessionsDir := filepath.Join(shannonDir, "agents", "myagent", "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	store := session.NewStore(sessionsDir)
	if err := store.Save(&session.Session{
		ID: sessionID,
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello there")},
			{Role: "assistant", Content: client.NewTextContent("hi, how can I help?")},
		},
	}); err != nil {
		t.Fatal(err)
	}
	store.Close()

	deps := &daemon.ServerDeps{
		GW:           gw,
		ShannonDir:   shannonDir,
		AgentsDir:    agentsDir,
		SessionCache: daemon.NewSessionCache(shannonDir),
	}
	return deps, "myagent", sessionID
}

// TestE2E_PromptSuggestion_HappyPath verifies the production happy path:
// fake gateway → real GatewayClient → agent.GenerateSuggestionWithUsage →
// SuggestionState → GET /suggestion serves the cached suggestion → POST
// /accept consumes it.
//
// Scope (option II from the plan): exercises the suggestion package through
// the real *GatewayClient, then drives the daemon's real HTTP handlers via
// *Server.Handler(). The post-Run hook in RunAgent (which invokes
// fireSuggestionAfterRun as a goroutine) is unexported and is not exercised
// directly here — internal/daemon/runner_test.go covers that path.
func TestE2E_PromptSuggestion_HappyPath(t *testing.T) {
	const expectedSuggestion = "fix the bug"

	gw := &fakeGateway{reply: expectedSuggestion}
	gwServer := httptest.NewServer(gw.handler())
	defer gwServer.Close()

	gwClient := client.NewGatewayClient(gwServer.URL, "test-api-key")
	deps, agentName, sessionID := newSuggestionDeps(t, gwClient)
	bus := daemon.NewEventBus()
	deps.EventBus = bus
	eventCh := bus.Subscribe()

	// NewServer wires deps.Suggestions automatically (server.go:149).
	srv := daemon.NewServer(0, nil, deps, "test")
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	// Build the main turn's snapshot. Real production gets this from
	// loop.LastSentRequest(); we synthesize one with the same shape.
	main := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello there")},
			{Role: "assistant", Content: client.NewTextContent("hi, how can I help?")},
		},
		ModelTier:   "medium",
		Temperature: 0.7,
		MaxTokens:   1024,
		SessionID:   sessionID,
		CacheSource: "shanclaw",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	// Suggestion path — hits fake gateway, decodes through real GatewayClient.
	sugRes, err := agent.GenerateSuggestionWithUsage(ctx, gwClient, main)
	if err != nil {
		t.Fatalf("GenerateSuggestionWithUsage: %v", err)
	}
	if sugRes.Text != expectedSuggestion {
		t.Fatalf("suggestion text = %q, want %q", sugRes.Text, expectedSuggestion)
	}
	if sugRes.Usage.CacheReadTokens != 1150 {
		t.Errorf("suggestion cache_read_tokens = %d, want 1150 (forked call should expose usage)", sugRes.Usage.CacheReadTokens)
	}

	deps.Suggestions.Set(sessionID, sugRes.Text, time.Now())
	emitSuggestionEvent(bus, sessionID, agentName, sugRes.Text)

	// Exactly one gateway call (suggestion only — speculation is gone).
	if got := gw.requestCount(); got != 1 {
		t.Fatalf("gateway request count = %d, want 1", got)
	}
	sugReq := gw.requestAt(0)
	if !sugReq.SkipCacheWrite {
		t.Errorf("forked suggestion call should set SkipCacheWrite=true")
	}
	// Validate the appended message is SuggestionPrompt.
	if n := len(sugReq.Messages); n == 0 ||
		sugReq.Messages[n-1].Content.Text() == "" ||
		!stringContains(sugReq.Messages[n-1].Content.Text(), "Predict the user's most likely next message") {
		t.Errorf("suggestion request last message should be SuggestionPrompt; got %d msgs", n)
	}

	// EventBus saw the suggestion_ready event.
	if err := waitForReadyEvent(eventCh, sessionID, 1*time.Second); err != nil {
		t.Fatalf("ready event: %v", err)
	}

	// GET /suggestion — Desktop poll path.
	getURL := httpServer.URL + "/agents/" + agentName + "/sessions/" + sessionID + "/suggestion"
	resp, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET suggestion: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Fatalf("GET suggestion status = %d, body = %s", resp.StatusCode, body)
	}
	var getBody map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&getBody); err != nil {
		resp.Body.Close()
		t.Fatalf("decode GET body: %v", err)
	}
	resp.Body.Close()
	if getBody["text"] != expectedSuggestion {
		t.Errorf("GET suggestion text = %v, want %q", getBody["text"], expectedSuggestion)
	}

	// POST /accept — Desktop fills input + user presses Enter.
	acceptURL := httpServer.URL + "/agents/" + agentName + "/sessions/" + sessionID + "/suggestion/accept"
	acceptResp, err := http.Post(acceptURL, "application/json", nil)
	if err != nil {
		t.Fatalf("POST accept: %v", err)
	}
	if acceptResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(acceptResp.Body)
		acceptResp.Body.Close()
		t.Fatalf("POST accept status = %d, body = %s", acceptResp.StatusCode, body)
	}
	var acceptBody map[string]any
	if err := json.NewDecoder(acceptResp.Body).Decode(&acceptBody); err != nil {
		acceptResp.Body.Close()
		t.Fatalf("decode accept body: %v", err)
	}
	acceptResp.Body.Close()
	if acceptBody["text"] != expectedSuggestion {
		t.Errorf("accept text = %v, want %q", acceptBody["text"], expectedSuggestion)
	}
	if acceptBody["suggestion"] != expectedSuggestion {
		t.Errorf("accept suggestion = %v, want %q (echoed)", acceptBody["suggestion"], expectedSuggestion)
	}
	if _, hasField := acceptBody["speculated_response"]; hasField {
		t.Errorf("accept body should NOT contain speculated_response: %v", acceptBody)
	}

	// After accept, GET /suggestion 404s (Clear consumed it).
	resp2, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET suggestion (post-accept): %v", err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != http.StatusNotFound {
		t.Errorf("GET suggestion post-accept status = %d, want 404 (Clear should have consumed it)", resp2.StatusCode)
	}
}

// TestE2E_PromptSuggestion_FilteredOutput verifies that a model reply
// rejected by FilterSuggestion (e.g., "skip") results in no SSE event,
// no suggestion stored, and GET /suggestion 404.
func TestE2E_PromptSuggestion_FilteredOutput(t *testing.T) {
	gw := &fakeGateway{reply: "skip"} // FilterSuggestion rejects "skip"
	gwServer := httptest.NewServer(gw.handler())
	defer gwServer.Close()

	gwClient := client.NewGatewayClient(gwServer.URL, "test-api-key")
	deps, agentName, sessionID := newSuggestionDeps(t, gwClient)
	bus := daemon.NewEventBus()
	deps.EventBus = bus

	// NewServer wires deps.Suggestions automatically (server.go:149).
	srv := daemon.NewServer(0, nil, deps, "test")
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	main := client.CompletionRequest{
		Messages:    []client.Message{{Role: "user", Content: client.NewTextContent("hi")}},
		ModelTier:   "medium",
		SessionID:   sessionID,
		CacheSource: "shanclaw",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	res, err := agent.GenerateSuggestionWithUsage(ctx, gwClient, main)
	if err != nil {
		t.Fatalf("GenerateSuggestionWithUsage: %v", err)
	}
	if res.Text != "" {
		t.Errorf("filtered suggestion text = %q, want empty", res.Text)
	}
	// Gateway was called but no Set fired.
	if gw.requestCount() != 1 {
		t.Errorf("gateway should still be called once (filter happens client-side)")
	}
	if _, present := deps.Suggestions.Get(sessionID); present {
		t.Error("filtered suggestion should not land in state")
	}

	getURL := httpServer.URL + "/agents/" + agentName + "/sessions/" + sessionID + "/suggestion"
	resp, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404 (no suggestion stored)", resp.StatusCode)
	}
}

// emitSuggestionEvent mirrors the daemon's post-Run hook event shape so the
// test exercises the SSE encoding path even though we don't drive RunAgent
// directly.
func emitSuggestionEvent(bus *daemon.EventBus, sessionID, agentName, text string) {
	payload, _ := json.Marshal(map[string]any{
		"session_id": sessionID,
		"agent":      agentName,
		"text":       text,
	})
	bus.Emit(daemon.Event{Type: daemon.EventSuggestionReady, Payload: payload})
}

// waitForReadyEvent blocks until a matching suggestion_ready event appears.
func waitForReadyEvent(ch <-chan daemon.Event, sessionID string, timeout time.Duration) error {
	deadline := time.After(timeout)
	for {
		select {
		case ev := <-ch:
			if ev.Type != daemon.EventSuggestionReady {
				continue
			}
			var payload map[string]any
			if err := json.Unmarshal(ev.Payload, &payload); err != nil {
				continue
			}
			if payload["session_id"] != sessionID {
				continue
			}
			return nil
		case <-deadline:
			return &timeoutErr{}
		}
	}
}

type timeoutErr struct{}

func (*timeoutErr) Error() string { return "timed out waiting for suggestion_ready event" }

// stringContains is a tiny inline helper to avoid importing strings just for
// one substring check.
func stringContains(s, substr string) bool {
	for i := 0; i+len(substr) <= len(s); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}
