package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// seedSession writes a session with one user message into sessionsDir and
// returns its ID.
func seedSession(t *testing.T, sessionsDir, title, text string) string {
	t.Helper()
	mgr := session.NewManager(sessionsDir)
	sess := mgr.NewSession()
	sess.Title = title
	sess.Messages = append(sess.Messages,
		client.Message{Role: "user", Content: client.NewTextContent(text)})
	if err := mgr.Save(); err != nil {
		t.Fatalf("save %q: %v", title, err)
	}
	mgr.Close()
	return sess.ID
}

// writeAgentDefinition drops an AGENT.md so ListAgents recognizes the slug.
func writeAgentDefinition(t *testing.T, agentsDir, slug string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(agentsDir, slug), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(agentsDir, slug, "AGENT.md"), []byte("# agent\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestHandleSessions_ScopeAll_MergesAcrossAgents(t *testing.T) {
	dir := t.TempDir()
	shannonDir := filepath.Join(dir, "shannon")
	agentsDir := filepath.Join(shannonDir, "agents")

	// Default scope session.
	defID := seedSession(t, filepath.Join(shannonDir, "sessions"), "default chat", "hello default")
	// Named agent session. Its presence under agents/<slug> makes ListAgents see it.
	agentSlug := "agent-abc123"
	agID := seedSession(t, filepath.Join(agentsDir, agentSlug, "sessions"), "agent chat", "hello agent")
	writeAgentDefinition(t, agentsDir, agentSlug)

	s := &Server{deps: &ServerDeps{
		ShannonDir:   shannonDir,
		AgentsDir:    agentsDir,
		SessionCache: NewSessionCache(shannonDir),
	}}

	// scope=all
	req := httptest.NewRequest(http.MethodGet, "/sessions?scope=all", nil)
	rr := httptest.NewRecorder()
	s.handleSessions(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Sessions []session.SessionSummary `json:"sessions"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	byID := map[string]session.SessionSummary{}
	for _, s := range resp.Sessions {
		byID[s.ID] = s
	}
	if len(byID) != 2 {
		t.Fatalf("expected 2 merged sessions, got %d: %s", len(byID), rr.Body.String())
	}
	if got := byID[defID].Agent; got != "" {
		t.Errorf("default session Agent = %q, want empty", got)
	}
	if got := byID[agID].Agent; got != agentSlug {
		t.Errorf("agent session Agent = %q, want %q", got, agentSlug)
	}

	// Non-scope path unchanged: only default sessions, agent field = "".
	req2 := httptest.NewRequest(http.MethodGet, "/sessions", nil)
	rr2 := httptest.NewRecorder()
	s.handleSessions(rr2, req2)
	var resp2 struct {
		Sessions []session.SessionSummary `json:"sessions"`
	}
	if err := json.Unmarshal(rr2.Body.Bytes(), &resp2); err != nil {
		t.Fatalf("decode2: %v", err)
	}
	if len(resp2.Sessions) != 1 || resp2.Sessions[0].ID != defID {
		t.Fatalf("default-scope path changed: %s", rr2.Body.String())
	}
	if resp2.Sessions[0].Agent != "" {
		t.Errorf("default-scope Agent = %q, want empty", resp2.Sessions[0].Agent)
	}
}

func TestHandleSessionSearch_ScopeAll(t *testing.T) {
	dir := t.TempDir()
	shannonDir := filepath.Join(dir, "shannon")
	agentsDir := filepath.Join(shannonDir, "agents")

	seedSession(t, filepath.Join(shannonDir, "sessions"), "default chat", "pineapple in default")
	agentSlug := "agent-xyz789"
	seedSession(t, filepath.Join(agentsDir, agentSlug, "sessions"), "agent chat", "pineapple in agent")
	writeAgentDefinition(t, agentsDir, agentSlug)

	s := &Server{deps: &ServerDeps{
		ShannonDir:   shannonDir,
		AgentsDir:    agentsDir,
		SessionCache: NewSessionCache(shannonDir),
	}}

	req := httptest.NewRequest(http.MethodGet, "/sessions/search?scope=all&q=pineapple", nil)
	rr := httptest.NewRecorder()
	s.handleSessionSearch(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rr.Code, rr.Body.String())
	}
	var resp struct {
		Results []session.SearchResult `json:"results"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v (body=%s)", err, rr.Body.String())
	}
	agentsSeen := map[string]bool{}
	for _, r := range resp.Results {
		agentsSeen[r.Agent] = true
	}
	if !agentsSeen[""] || !agentsSeen[agentSlug] {
		t.Fatalf("expected hits from both default and %q, got: %s", agentSlug, rr.Body.String())
	}
}

// TestWireFixture_HTTPSessionsScopeAll pins the GET /sessions?scope=all response
// shape (merged across default + named agents, each tagged with its `agent`
// scope) against the committed fixture, driving the FULL production router.
func TestWireFixture_HTTPSessionsScopeAll(t *testing.T) {
	fixture := loadWireFixture(t, "http_get.sessions.scope_all.response.json")

	dir := t.TempDir()
	shannonDir := filepath.Join(dir, "shannon")
	agentsDir := filepath.Join(shannonDir, "agents")

	// Seed default first, then the agent — monotonic UpdatedAt makes the agent
	// session the most recent, so it sorts first (matching the fixture order).
	seedSession(t, filepath.Join(shannonDir, "sessions"), "default chat", "hello default")
	agentSlug := "agent-abc123"
	seedSession(t, filepath.Join(agentsDir, agentSlug, "sessions"), "agent chat", "hello agent")
	writeAgentDefinition(t, agentsDir, agentSlug)

	deps := &ServerDeps{AgentsDir: agentsDir, ShannonDir: shannonDir, SessionCache: NewSessionCache(shannonDir)}
	srv := NewServer(0, nil, deps, "test")
	handler := srv.Handler()

	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/sessions?scope=all", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /sessions?scope=all = %d, body %s", rec.Code, rec.Body.String())
	}

	produced := parseJSONMap(t, rec.Body.Bytes())
	fixSessions := fixture["sessions"].([]any)
	prodSessions, ok := produced["sessions"].([]any)
	if !ok || len(prodSessions) != len(fixSessions) {
		t.Fatalf("sessions count: want %d, got body %s", len(fixSessions), rec.Body.String())
	}
	// Normalize the dynamic id/created_at/updated_at on each element (generated
	// per run) to the fixture's placeholder before the deep compare — same
	// pattern as the other list fixtures. Order is contractual (updated_at desc).
	for i := range prodSessions {
		pm := prodSessions[i].(map[string]any)
		fm := fixSessions[i].(map[string]any)
		normalizePrefixedID(t, pm, fm, "id", "2026-")
		normalizeRFC3339(t, pm, fm, "created_at")
		normalizeRFC3339(t, pm, fm, "updated_at")
	}
	assertSemanticEqual(t, fixture, produced)

	// Consumer-shaped decode: the new `agent` field must survive as the scope
	// attribution (empty = default, slug otherwise), plus the pagination wrapper.
	var decoded struct {
		Sessions []struct {
			Title string `json:"title"`
			Agent string `json:"agent"`
		} `json:"sessions"`
		Total   int  `json:"total"`
		HasMore bool `json:"has_more"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &decoded); err != nil {
		t.Fatalf("consumer decode failed: %v", err)
	}
	if decoded.Sessions[0].Agent != agentSlug || decoded.Sessions[1].Agent != "" {
		t.Fatalf("agent attribution lost: %+v", decoded.Sessions)
	}
	if decoded.Total != 2 || decoded.HasMore {
		t.Fatalf("pagination wrapper wrong: total=%d has_more=%v", decoded.Total, decoded.HasMore)
	}
}

// TestHandleSessions_Pagination verifies limit/offset windowing, has_more, and
// the pinned-DESC then updated_at-DESC ordering across a scope=all merge.
func TestHandleSessions_Pagination(t *testing.T) {
	dir := t.TempDir()
	shannonDir := filepath.Join(dir, "shannon")
	agentsDir := filepath.Join(shannonDir, "agents")

	// Seed 5 default sessions in ascending recency (s0 oldest .. s4 newest).
	var ids []string
	for i := 0; i < 5; i++ {
		ids = append(ids, seedSession(t, filepath.Join(shannonDir, "sessions"),
			"chat", "msg"))
	}
	// ids[0] is oldest, ids[4] newest. Pin the oldest so it must lead despite
	// being least-recent.
	pinnedMgr := session.NewManager(filepath.Join(shannonDir, "sessions"))
	if err := pinnedMgr.PatchFlags(ids[0], boolPtr(true), nil); err != nil {
		t.Fatalf("pin: %v", err)
	}
	pinnedMgr.Close()

	s := &Server{deps: &ServerDeps{
		ShannonDir:   shannonDir,
		AgentsDir:    agentsDir,
		SessionCache: NewSessionCache(shannonDir),
	}}

	get := func(query string) (page []session.SessionSummary, total int, hasMore bool) {
		req := httptest.NewRequest(http.MethodGet, "/sessions"+query, nil)
		rr := httptest.NewRecorder()
		s.handleSessions(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Sessions []session.SessionSummary `json:"sessions"`
			Total    int                      `json:"total"`
			HasMore  bool                     `json:"has_more"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return resp.Sessions, resp.Total, resp.HasMore
	}

	// First page of 2: pinned-oldest leads, then newest of the rest.
	page, total, hasMore := get("?limit=2&offset=0")
	if total != 5 || !hasMore || len(page) != 2 {
		t.Fatalf("page1: total=%d hasMore=%v len=%d", total, hasMore, len(page))
	}
	if page[0].ID != ids[0] || !page[0].Pinned {
		t.Fatalf("pinned session did not lead: %+v", page[0])
	}
	if page[1].ID != ids[4] {
		t.Fatalf("page1[1] want newest %s, got %s", ids[4], page[1].ID)
	}

	// Second page of 2.
	page2, _, hasMore2 := get("?limit=2&offset=2")
	if len(page2) != 2 || !hasMore2 {
		t.Fatalf("page2: len=%d hasMore=%v", len(page2), hasMore2)
	}
	if page2[0].ID != ids[3] || page2[1].ID != ids[2] {
		t.Fatalf("page2 order wrong: %s, %s", page2[0].ID, page2[1].ID)
	}

	// Last page: 1 item, no more.
	page3, _, hasMore3 := get("?limit=2&offset=4")
	if len(page3) != 1 || hasMore3 {
		t.Fatalf("page3: len=%d hasMore=%v", len(page3), hasMore3)
	}
	if page3[0].ID != ids[1] {
		t.Fatalf("page3 want %s, got %s", ids[1], page3[0].ID)
	}

	// Offset past the end: empty page, has_more false, total intact.
	empty, total4, hasMore4 := get("?limit=2&offset=99")
	if len(empty) != 0 || hasMore4 || total4 != 5 {
		t.Fatalf("overshoot: len=%d hasMore=%v total=%d", len(empty), hasMore4, total4)
	}

	// No params: default limit 100 returns all 5, no more.
	all, _, hasMoreAll := get("")
	if len(all) != 5 || hasMoreAll {
		t.Fatalf("default page: len=%d hasMore=%v", len(all), hasMoreAll)
	}
}

func boolPtr(b bool) *bool { return &b }

// TestHandleSessions_SingleScopeUnlimited verifies the backward-compat rule:
// the single-scope path with NO explicit limit returns ALL sessions (uncapped),
// while scope=all defaults to the 100 cap, and an explicit limit is honored on
// both paths.
func TestHandleSessions_SingleScopeUnlimited(t *testing.T) {
	dir := t.TempDir()
	shannonDir := filepath.Join(dir, "shannon")
	agentsDir := filepath.Join(shannonDir, "agents")

	// Seed 150 default-scope sessions — above the 100 default cap.
	const n = 150
	for i := 0; i < n; i++ {
		seedSession(t, filepath.Join(shannonDir, "sessions"), "chat", "msg")
	}

	s := &Server{deps: &ServerDeps{
		ShannonDir:   shannonDir,
		AgentsDir:    agentsDir,
		SessionCache: NewSessionCache(shannonDir),
	}}

	get := func(query string) (n, total int, hasMore bool) {
		req := httptest.NewRequest(http.MethodGet, "/sessions"+query, nil)
		rr := httptest.NewRecorder()
		s.handleSessions(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("status = %d body %s", rr.Code, rr.Body.String())
		}
		var resp struct {
			Sessions []session.SessionSummary `json:"sessions"`
			Total    int                      `json:"total"`
			HasMore  bool                     `json:"has_more"`
		}
		if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return len(resp.Sessions), resp.Total, resp.HasMore
	}

	// single-scope, no limit → UNLIMITED (all 150, has_more false).
	got, total, hasMore := get("")
	if got != n || total != n || hasMore {
		t.Fatalf("single-scope unlimited: got=%d total=%d hasMore=%v, want %d/%d/false", got, total, hasMore, n, n)
	}

	// scope=all, no limit → capped at 100, has_more true, total 150.
	got, total, hasMore = get("?scope=all")
	if got != defaultSessionsPageLimit || total != n || !hasMore {
		t.Fatalf("scope=all default cap: got=%d total=%d hasMore=%v, want 100/%d/true", got, total, hasMore, n)
	}

	// single-scope, explicit limit → honored (opt-in pagination).
	got, total, hasMore = get("?limit=50")
	if got != 50 || total != n || !hasMore {
		t.Fatalf("single-scope explicit limit: got=%d total=%d hasMore=%v, want 50/%d/true", got, total, hasMore, n)
	}

	// scope=all, explicit limit → honored.
	got, total, hasMore = get("?scope=all&limit=120")
	if got != 120 || total != n || !hasMore {
		t.Fatalf("scope=all explicit limit: got=%d total=%d hasMore=%v, want 120/%d/true", got, total, hasMore, n)
	}
}
