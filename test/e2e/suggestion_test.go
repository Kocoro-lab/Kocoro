package e2e

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
// that the suggestion + speculation paths set. Responses are selected by
// inspecting the last message's text — the suggestion path appends
// SuggestionPrompt; speculation appends the filtered suggestion text.
type fakeGateway struct {
	mu       sync.Mutex
	requests []client.CompletionRequest
	// suggestionText is what the gateway returns for the suggestion call
	// (the one whose last message contains agent.SuggestionPrompt).
	suggestionText string
	// speculationText is what the gateway returns for the speculation call
	// (the one whose last message contains the filtered suggestion text).
	speculationText string
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
		g.mu.Unlock()

		// Pick a response by sniffing the last message's text content. We
		// can't use MessageContent.Text() here because UnmarshalJSON already
		// folded a string-content into mc.text — handy.
		var last string
		if n := len(req.Messages); n > 0 {
			last = req.Messages[n-1].Content.Text()
		}

		var output string
		switch {
		case strings.Contains(last, "Predict the user's most likely next message"):
			output = g.suggestionText
		default:
			output = g.speculationText
		}

		resp := client.CompletionResponse{
			Provider:   "anthropic",
			Model:      "claude-sonnet-4-6",
			OutputText: output,
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
// rooted in a temp shannon dir, and a pre-seeded session ready for accept
// to append onto.
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
// fake gateway → real GatewayClient → agent.GenerateSuggestion / RunSpeculation
// → SuggestionState → GET /suggestion serves the cached suggestion → POST
// /accept persists (suggestion, speculated_response) to the session.
//
// Scope (option II from the plan): exercises the suggestion package through
// the real *GatewayClient, then drives the daemon's real HTTP handlers via
// *Server.Handler(). The post-Run hook in RunAgent (which invokes
// fireSuggestionAfterRun as a goroutine) is unexported and is not exercised
// directly here; the test inlines the same call sequence (GenerateSuggestion
// → state.Set → bus.Emit → RunSpeculation → state.SetSpeculation → bus.Emit)
// so any drift between the two paths is caught by unit tests in
// internal/daemon/runner_test.go rather than this E2E.
func TestE2E_PromptSuggestion_HappyPath(t *testing.T) {
	const (
		expectedSuggestion = "fix the bug"
		expectedResponse   = "Here is the fix"
	)

	gw := &fakeGateway{
		suggestionText:  expectedSuggestion,
		speculationText: expectedResponse,
	}
	gwServer := httptest.NewServer(gw.handler())
	defer gwServer.Close()

	gwClient := client.NewGatewayClient(gwServer.URL, "test-api-key")

	deps, agentName, sessionID := newSuggestionDeps(t, gwClient)

	srv := daemon.NewServer(0, nil, deps, "test")
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	bus := deps.EventBus
	if bus == nil {
		// NewServer does not auto-wire deps.EventBus; the runner uses
		// s.eventBus via the closure. We create one here to verify the
		// payload shape that production code emits.
		bus = daemon.NewEventBus()
		deps.EventBus = bus
	}
	eventCh := bus.Subscribe()
	defer bus.Unsubscribe(eventCh)

	// Synthesize the "main" CompletionRequest that production code captures
	// from the last LLM call and forwards into fireSuggestionAfterRun. We
	// only need a couple of messages — the SuggestionPrompt is appended by
	// BuildForkedSuggestionRequest.
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
	emitSuggestionEvent(bus, sessionID, agentName, sugRes.Text, false)

	// Speculation path — same fake gateway, branches on last-message text.
	specRes, err := agent.RunSpeculationWithUsage(ctx, gwClient, main, sugRes.Text)
	if err != nil {
		t.Fatalf("RunSpeculationWithUsage: %v", err)
	}
	if specRes.Text != expectedResponse {
		t.Fatalf("speculation text = %q, want %q", specRes.Text, expectedResponse)
	}
	deps.Suggestions.SetSpeculation(sessionID, sugRes.Text, specRes.Text)
	emitSuggestionEvent(bus, sessionID, agentName, sugRes.Text, true)

	// Assert both gateway calls hit /v1/completions with the on-wire
	// cache-safety flag the suggestion package promises (SkipCacheWrite=true).
	// ForkedKind is intentionally stripped before transmission (json:"-")
	// because it is SHANNON_CACHE_DEBUG-only telemetry — cache-debug.log
	// correlation lives client-side; nothing on the gateway needs it. Tagging
	// of ForkedKind is covered by forkedrequest_test.go's byte-equality test.
	if got := gw.requestCount(); got != 2 {
		t.Fatalf("gateway request count = %d, want 2", got)
	}
	sugReq := gw.requestAt(0)
	specReq := gw.requestAt(1)
	if !sugReq.SkipCacheWrite || !specReq.SkipCacheWrite {
		t.Errorf("forked calls should set SkipCacheWrite=true; got sugReq=%v specReq=%v",
			sugReq.SkipCacheWrite, specReq.SkipCacheWrite)
	}
	// Validate the appended messages contain the expected prompts so we
	// know the fake gateway's last-message switch actually steered each
	// call to the right scripted response.
	if n := len(sugReq.Messages); n == 0 || !strings.Contains(sugReq.Messages[n-1].Content.Text(), "Predict the user's most likely next message") {
		t.Errorf("suggestion request last message should be SuggestionPrompt; got %d msgs", n)
	}
	if n := len(specReq.Messages); n == 0 || specReq.Messages[n-1].Content.Text() != expectedSuggestion {
		t.Errorf("speculation request last message should be suggestion text %q; got msgs=%+v", expectedSuggestion, specReq.Messages)
	}

	// EventBus must have observed both ready events (suggestion-only first,
	// then has_speculation=true after the speculation call lands).
	if err := waitForReadyEvent(eventCh, sessionID, false, 1*time.Second); err != nil {
		t.Fatalf("first ready event: %v", err)
	}
	if err := waitForReadyEvent(eventCh, sessionID, true, 1*time.Second); err != nil {
		t.Fatalf("speculation ready event: %v", err)
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
	if hs, _ := getBody["has_speculation"].(bool); !hs {
		t.Errorf("GET suggestion has_speculation = %v, want true", getBody["has_speculation"])
	}

	// POST /suggestion/accept — Desktop instant-display path; must persist
	// the (suggestion, speculated_response) pair to the session atomically.
	postURL := httpServer.URL + "/agents/" + agentName + "/sessions/" + sessionID + "/suggestion/accept"
	postResp, err := http.Post(postURL, "application/json", nil)
	if err != nil {
		t.Fatalf("POST accept: %v", err)
	}
	if postResp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(postResp.Body)
		postResp.Body.Close()
		t.Fatalf("POST accept status = %d, body = %s", postResp.StatusCode, body)
	}
	var acceptBody map[string]any
	if err := json.NewDecoder(postResp.Body).Decode(&acceptBody); err != nil {
		postResp.Body.Close()
		t.Fatalf("decode accept body: %v", err)
	}
	postResp.Body.Close()
	if acceptBody["suggestion"] != expectedSuggestion {
		t.Errorf("accept suggestion = %v, want %q", acceptBody["suggestion"], expectedSuggestion)
	}
	if acceptBody["speculated_response"] != expectedResponse {
		t.Errorf("accept speculated_response = %v, want %q", acceptBody["speculated_response"], expectedResponse)
	}

	// Confirm SuggestionState was cleared by /accept — the suggestion is
	// consumed once the user has acted on it (prevents stale re-serve on a
	// duplicate poll).
	if _, ok := deps.Suggestions.Get(sessionID); ok {
		t.Error("expected SuggestionState to be cleared after /accept")
	}

	// Session must now have the two appended messages persisted, ready for
	// the next turn's context.
	sessMgr := deps.SessionCache.GetOrCreate(agentName)
	persisted, err := sessMgr.Load(sessionID)
	if err != nil {
		t.Fatalf("reload session: %v", err)
	}
	if got, want := len(persisted.Messages), 4; got != want {
		t.Fatalf("messages after accept = %d, want %d (2 seed + 2 appended)", got, want)
	}
	if persisted.Messages[2].Role != "user" || persisted.Messages[2].Content.Text() != expectedSuggestion {
		t.Errorf("appended user msg = %+v, want role=user content=%q", persisted.Messages[2], expectedSuggestion)
	}
	if persisted.Messages[3].Role != "assistant" || persisted.Messages[3].Content.Text() != expectedResponse {
		t.Errorf("appended assistant msg = %+v, want role=assistant content=%q", persisted.Messages[3], expectedResponse)
	}
}

// TestE2E_PromptSuggestion_FilteredOutput verifies that a model reply
// rejected by FilterSuggestion does not produce a stored suggestion or an
// SSE event — the silent-failure mode that protects users from low-quality
// model output. Speculation is intentionally not exercised because the
// runner skips it when the suggestion filter rejects.
func TestE2E_PromptSuggestion_FilteredOutput(t *testing.T) {
	gw := &fakeGateway{
		// "skip" is a meta-marker the filter rejects unconditionally.
		suggestionText:  "skip",
		speculationText: "should not be requested",
	}
	gwServer := httptest.NewServer(gw.handler())
	defer gwServer.Close()
	gwClient := client.NewGatewayClient(gwServer.URL, "test-api-key")

	deps, _, sessionID := newSuggestionDeps(t, gwClient)
	srv := daemon.NewServer(0, nil, deps, "test")
	httpServer := httptest.NewServer(srv.Handler())
	defer httpServer.Close()

	main := client.CompletionRequest{
		Messages: []client.Message{
			{Role: "user", Content: client.NewTextContent("hello")},
		},
		SessionID: sessionID,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()

	res, err := agent.GenerateSuggestionWithUsage(ctx, gwClient, main)
	if err != nil {
		t.Fatalf("GenerateSuggestionWithUsage: %v", err)
	}
	if res.Text != "" {
		t.Fatalf("filter should have rejected %q; got non-empty text = %q", gw.suggestionText, res.Text)
	}
	if res.Usage.CacheReadTokens != 1150 {
		t.Errorf("usage should be populated even on filter rejection; cache_read_tokens = %d", res.Usage.CacheReadTokens)
	}

	// GET must 404 — no suggestion was stored.
	getURL := httpServer.URL + "/agents/myagent/sessions/" + sessionID + "/suggestion"
	resp, err := http.Get(getURL)
	if err != nil {
		t.Fatalf("GET suggestion: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("GET status = %d, want 404 after filter rejection", resp.StatusCode)
	}

	// Only one gateway call was made — speculation must NOT be invoked when
	// the suggestion filter rejects.
	if gw.requestCount() != 1 {
		t.Errorf("gateway request count = %d, want 1 (speculation must be skipped)", gw.requestCount())
	}
}

// emitSuggestionEvent mirrors the daemon's post-Run hook event shape so the
// EventBus subscriber assertion exercises the same JSON wire format Desktop
// will parse.
func emitSuggestionEvent(bus *daemon.EventBus, sessionID, agentName, text string, hasSpec bool) {
	payload, _ := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"agent":           agentName,
		"text":            text,
		"has_speculation": hasSpec,
	})
	bus.Emit(daemon.Event{Type: daemon.EventSuggestionReady, Payload: payload})
}

// waitForReadyEvent drains the bus channel until a suggestion_ready event
// matching (sessionID, hasSpec) arrives or the deadline expires.
func waitForReadyEvent(ch <-chan daemon.Event, sessionID string, hasSpec bool, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case evt := <-ch:
			if evt.Type != daemon.EventSuggestionReady {
				continue
			}
			var p map[string]any
			if err := json.Unmarshal(evt.Payload, &p); err != nil {
				return err
			}
			gotSession, _ := p["session_id"].(string)
			gotHas, _ := p["has_speculation"].(bool)
			if gotSession == sessionID && gotHas == hasSpec {
				return nil
			}
		case <-time.After(timeout):
			return errTimeout
		}
	}
	return errTimeout
}

// errTimeout is a sentinel for waitForReadyEvent so the call sites can
// distinguish "no event arrived" from a marshal/parse failure.
var errTimeout = &timeoutErr{}

type timeoutErr struct{}

func (*timeoutErr) Error() string { return "timed out waiting for suggestion_ready event" }
