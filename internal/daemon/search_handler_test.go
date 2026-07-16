package daemon

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// searchResponse mirrors the GET /search wire shape a client decodes: one hit
// per matching session plus the pagination wrapper.
type searchResponse struct {
	Results []session.SessionHit `json:"results"`
	Total   int                  `json:"total"`
	HasMore bool                 `json:"has_more"`
}

// newSearchServer seeds a default-scope session and a named-agent session (both
// containing the word "pineapple") and returns the FULL production router. Tests
// enter from the outside via handler.ServeHTTP, not by calling handleSearch
// directly — the seam (routing, scope resolution, paging) is what breaks.
func newSearchServer(t *testing.T) (http.Handler, string) {
	t.Helper()
	dir := t.TempDir()
	shannonDir := filepath.Join(dir, "shannon")
	agentsDir := filepath.Join(shannonDir, "agents")

	seedSession(t, filepath.Join(shannonDir, "sessions"), "default chat", "pineapple in default")
	agentSlug := "agent-xyz789"
	seedSession(t, filepath.Join(agentsDir, agentSlug, "sessions"), "agent chat", "pineapple in agent")
	writeAgentDefinition(t, agentsDir, agentSlug)

	deps := &ServerDeps{AgentsDir: agentsDir, ShannonDir: shannonDir, SessionCache: NewSessionCache(shannonDir)}
	srv := NewServer(0, nil, deps, "test")
	return srv.Handler(), agentSlug
}

func getSearch(t *testing.T, handler http.Handler, query string) (*httptest.ResponseRecorder, searchResponse) {
	t.Helper()
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/search"+query, nil))
	var resp searchResponse
	if rec.Code == http.StatusOK {
		if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode %q: %v (body=%s)", query, err, rec.Body.String())
		}
	}
	return rec, resp
}

func TestHandleSearch_ScopeAll_MergesAndPages(t *testing.T) {
	handler, agentSlug := newSearchServer(t)

	rec, resp := getSearch(t, handler, "?q=pineapple")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// scope defaults to "all": both the default and the named-agent session match.
	if resp.Total != 2 || len(resp.Results) != 2 || resp.HasMore {
		t.Fatalf("scope=all default: total=%d len=%d hasMore=%v, want 2/2/false", resp.Total, len(resp.Results), resp.HasMore)
	}
	seen := map[string]bool{}
	for _, h := range resp.Results {
		seen[h.Agent] = true
	}
	if !seen[""] || !seen[agentSlug] {
		t.Fatalf("expected hits from both default and %q, got %+v", agentSlug, resp.Results)
	}

	// limit=1 windows the merged set: page 1 has_more, page 2 finishes it.
	rec1, page1 := getSearch(t, handler, "?q=pineapple&limit=1&offset=0")
	if rec1.Code != http.StatusOK || page1.Total != 2 || len(page1.Results) != 1 || !page1.HasMore {
		t.Fatalf("page1: code=%d total=%d len=%d hasMore=%v", rec1.Code, page1.Total, len(page1.Results), page1.HasMore)
	}
	_, page2 := getSearch(t, handler, "?q=pineapple&limit=1&offset=1")
	if page2.Total != 2 || len(page2.Results) != 1 || page2.HasMore {
		t.Fatalf("page2: total=%d len=%d hasMore=%v", page2.Total, len(page2.Results), page2.HasMore)
	}
	if page1.Results[0].SessionID == page2.Results[0].SessionID {
		t.Fatalf("pages overlap: both returned %s", page1.Results[0].SessionID)
	}
}

func TestHandleSearch_ScopeAgent_OnlyThatAgent(t *testing.T) {
	handler, agentSlug := newSearchServer(t)

	rec, resp := getSearch(t, handler, "?q=pineapple&scope="+agentSlug)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if resp.Total != 1 || len(resp.Results) != 1 {
		t.Fatalf("scope=%q: total=%d len=%d, want 1/1", agentSlug, resp.Total, len(resp.Results))
	}
	if resp.Results[0].Agent != agentSlug {
		t.Fatalf("scope=%q returned agent %q", agentSlug, resp.Results[0].Agent)
	}

	// scope=default targets only the default agent (empty agent attribution).
	_, def := getSearch(t, handler, "?q=pineapple&scope=default")
	if def.Total != 1 || def.Results[0].Agent != "" {
		t.Fatalf("scope=default: total=%d agent=%q, want 1/empty", def.Total, def.Results[0].Agent)
	}
}

func TestHandleSearch_InvalidScopeSlug_400(t *testing.T) {
	handler, _ := newSearchServer(t)
	// A slug with a traversal char fails ValidateAgentName BEFORE any path
	// concatenation — the gate must reject, not 500 or read another dir.
	rec, _ := getSearch(t, handler, "?q=pineapple&scope=../evil")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("invalid scope: status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

func TestHandleSearch_MissingQuery_400(t *testing.T) {
	handler, _ := newSearchServer(t)
	rec, _ := getSearch(t, handler, "")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("missing q: status = %d, want 400", rec.Code)
	}
}

func TestHandleSearch_OffsetPastEnd_EmptyPage(t *testing.T) {
	handler, _ := newSearchServer(t)
	rec, resp := getSearch(t, handler, "?q=pineapple&offset=99")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	// Overshoot must clamp: empty results, has_more false, total intact — never
	// a slice-bounds panic.
	if len(resp.Results) != 0 || resp.HasMore || resp.Total != 2 {
		t.Fatalf("overshoot: len=%d hasMore=%v total=%d, want 0/false/2", len(resp.Results), resp.HasMore, resp.Total)
	}
	// results must serialize as [] (not null) so clients can iterate safely.
	if resp.Results == nil {
		t.Fatal("empty page results decoded as nil; want non-null []")
	}
}

func TestHandleSearch_ShortTermWithOperator_400(t *testing.T) {
	handler, _ := newSearchServer(t)
	// "ab" is < 3 runes and OR is an FTS operator → invalid-query error, which
	// searchErrStatus must map to 400, not 500. Exercised through scope=all
	// (the query fails identically in every scope, so firstErr surfaces).
	rec, _ := getSearch(t, handler, "?q=ab+OR+pineapple")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("short-term+operator: status = %d, want 400 (body=%s)", rec.Code, rec.Body.String())
	}
}

// TestHandleSearch_LimitClampedTo100 seeds more matching sessions than the cap
// and asserts a caller-supplied limit above 100 is clamped, not honored — an
// unbounded limit would let one request pull the entire corpus.
func TestHandleSearch_LimitClampedTo100(t *testing.T) {
	dir := t.TempDir()
	shannonDir := filepath.Join(dir, "shannon")
	agentsDir := filepath.Join(shannonDir, "agents")
	const n = 150
	for i := 0; i < n; i++ {
		seedSession(t, filepath.Join(shannonDir, "sessions"), "chat", "pineapple here")
	}
	deps := &ServerDeps{AgentsDir: agentsDir, ShannonDir: shannonDir, SessionCache: NewSessionCache(shannonDir)}
	handler := NewServer(0, nil, deps, "test").Handler()

	rec, resp := getSearch(t, handler, "?q=pineapple&scope=default&limit=500")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(resp.Results) != 100 || resp.Total != n || !resp.HasMore {
		t.Fatalf("limit clamp: len=%d total=%d hasMore=%v, want 100/%d/true", len(resp.Results), resp.Total, resp.HasMore, n)
	}
}

// TestSearchErrStatus covers the error→HTTP-status mapping directly: a query
// validation error is a client 400, anything else (a broken index, an I/O
// failure) is a 500. The 500 branch is not cleanly reachable through the black-
// box handler without fault injection, so the mapping is verified at the unit.
func TestSearchErrStatus(t *testing.T) {
	if got := searchErrStatus(fmt.Errorf("invalid search query: terms too short")); got != http.StatusBadRequest {
		t.Errorf("invalid-query err → %d, want 400", got)
	}
	if got := searchErrStatus(fmt.Errorf("open index: disk I/O error")); got != http.StatusInternalServerError {
		t.Errorf("internal err → %d, want 500", got)
	}
	if got := searchErrStatus(nil); got != http.StatusInternalServerError {
		t.Errorf("nil err → %d, want 500 (defensive default)", got)
	}
}
