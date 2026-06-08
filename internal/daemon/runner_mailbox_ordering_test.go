package daemon

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/agenttypes"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
	_ "modernc.org/sqlite"
)

// mailboxOrderingDeps builds a ServerDeps whose SessionCache is backed by a
// real on-disk mailbox store. The gateway is left unset — the caller wires it
// after starting an httptest server. Returns the deps and the store so tests
// can inspect consumed_at state via LoadPendingByRoute (pending == NULL).
func mailboxOrderingDeps(t *testing.T) (*ServerDeps, *MailboxStore) {
	t.Helper()
	shanDir := t.TempDir()
	db, err := sql.Open("sqlite", filepath.Join(shanDir, "mailbox.db"))
	if err != nil {
		t.Fatalf("open mailbox db: %v", err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { db.Close() })
	store, err := NewMailboxStore(db)
	if err != nil {
		t.Fatalf("new mailbox store: %v", err)
	}
	sc := NewSessionCacheWithMailbox(shanDir, store, 100)
	deps := &ServerDeps{
		Config: &config.Config{
			Provider:  "gateway",
			ModelTier: "medium",
			Agent:     config.AgentConfig{MaxIterations: 2},
		},
		Registry:     agent.NewToolRegistry(),
		BaselineReg:  agent.NewToolRegistry(),
		SessionCache: sc,
		ShannonDir:   shanDir,
		AgentsDir:    filepath.Join(shanDir, "agents"),
	}
	return deps, store
}

// firstCallPendingGateway wraps a fakeGatewayBackend and, on the FIRST LLM
// call, snapshots the store's pending-row count for routeKey. This is the
// observation hook for issue #163: the first LLM call happens inside loop.Run,
// strictly AFTER the mailbox is drained into the prompt but (for empty-Source
// routes) BEFORE the post-loop final save that first persists it. Recording
// the pending count here proves whether MarkMailboxConsumed fired at drain time
// (pre-fix: 0) or was correctly deferred past the drain (post-fix: still 1).
func firstCallPendingGateway(store *MailboxStore, routeKey, reply string) (http.HandlerFunc, func() int) {
	var once sync.Once
	var pendingAtFirstCall int
	inner := (&fakeGatewayBackend{reply: reply}).handler()
	h := func(w http.ResponseWriter, r *http.Request) {
		once.Do(func() {
			pend, _ := store.LoadPendingByRoute(routeKey)
			pendingAtFirstCall = len(pend)
		})
		inner(w, r)
	}
	return h, func() int { return pendingAtFirstCall }
}

// TestRunAgent_MailboxConsumedDeferredUntilSave is the issue #163 ordering
// proof. It routes a run with an EMPTY Source so the only save that persists
// the drained mailbox text is the post-loop final save — making the first LLM
// call a clean observation point that sits AFTER the drain but BEFORE any
// content-bearing save.
//
// Pre-fix (MarkMailboxConsumed at drain time, runner.go:1485): the SQLite row
// is already consumed when the first LLM call fires, so pendingAtFirstCall == 0
// and a daemon crash before the final save would lose the message silently.
//
// Post-fix: consumption is deferred to the first successful session.Save, so
// pendingAtFirstCall == 1 (row still pending → recoverable on crash), and the
// row is consumed only after the run persists.
func TestRunAgent_MailboxConsumedDeferredUntilSave(t *testing.T) {
	const sessID = "ordering-test-163-deferred"
	routeKey := "session:" + sanitizeRouteValue(sessID)

	deps, store := mailboxOrderingDeps(t)
	defer deps.SessionCache.CloseAll()

	h, getPending := firstCallPendingGateway(store, routeKey, "ack")
	ts := httptest.NewServer(h)
	defer ts.Close()
	deps.GW = client.NewGatewayClient(ts.URL, "test-key")

	// Seed one durable mailbox message on the route (persist + in-memory).
	msg := agenttypes.QueuedMessage{
		ID:         "mbx-deferred-1",
		Source:     "ws",
		Text:       "queued question",
		Priority:   agenttypes.PriorityNext,
		EnqueuedAt: time.Now(),
	}
	if out, err := deps.SessionCache.EnqueueMessage(routeKey, msg); err != nil || out != MailboxQueued {
		t.Fatalf("seed enqueue: out=%v err=%v", out, err)
	}

	req := RunAgentRequest{
		Text:       "live prompt",
		SessionID:  sessID,
		NewSession: true, // SessionID wins in ComputeRouteKey → session:<id>; resume-or-create with this id
		// Source intentionally empty: no pre-loop user-message save, so the
		// drained text is first persisted by the post-loop final save.
	}
	if _, err := RunAgent(context.Background(), deps, req, nullEventHandler{}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if got := getPending(); got != 1 {
		t.Fatalf("ORDERING VIOLATION (#163): mailbox row consumed before first save — pending at first LLM call = %d, want 1", got)
	}

	pendAfter, err := store.LoadPendingByRoute(routeKey)
	if err != nil {
		t.Fatalf("load pending after run: %v", err)
	}
	if len(pendAfter) != 0 {
		t.Fatalf("after a successful run the row must be consumed, still pending = %d", len(pendAfter))
	}

	// The drained text must actually be in the persisted transcript — proves we
	// consumed only because the content was durably saved, not regardless.
	assertDrainedTextPersisted(t, deps, sessID, "queued question")
}

// TestRunAgent_MailboxConsumedAtPreLoopSaveForSourced is the IM-path regression
// guard. For a non-empty Source (Slack/LINE/Feishu) the drained text is first
// persisted by the pre-loop user-message save (runner.go:1808, BEFORE loop.Run),
// so by the time the first LLM call fires the row is ALREADY consumed. This pins
// that the consume is wired to that early save (maximizing the durability
// window the daemon closes), and is not masked by the final-save backstop if the
// pre-loop wiring is later dropped (which would surface here as pending=1).
func TestRunAgent_MailboxConsumedAtPreLoopSaveForSourced(t *testing.T) {
	// Messaging source + sender → route key default:slack:<channel>:<sender>.
	const channel, sender = "C123", "U999"
	routeKey := ComputeRouteKey(RunAgentRequest{Source: "slack", Channel: channel, Sender: sender})
	if routeKey == "" {
		t.Fatalf("precondition: expected non-empty messaging route key")
	}

	deps, store := mailboxOrderingDeps(t)
	defer deps.SessionCache.CloseAll()

	h, getPending := firstCallPendingGateway(store, routeKey, "ack")
	ts := httptest.NewServer(h)
	defer ts.Close()
	deps.GW = client.NewGatewayClient(ts.URL, "test-key")

	msg := agenttypes.QueuedMessage{
		ID:         "mbx-sourced-1",
		Source:     "ws",
		Text:       "queued from slack",
		Priority:   agenttypes.PriorityNext,
		EnqueuedAt: time.Now(),
	}
	if out, err := deps.SessionCache.EnqueueMessage(routeKey, msg); err != nil || out != MailboxQueued {
		t.Fatalf("seed enqueue: out=%v err=%v", out, err)
	}

	req := RunAgentRequest{
		Text:    "live prompt",
		Source:  "slack",
		Channel: channel,
		Sender:  sender,
	}
	if _, err := RunAgent(context.Background(), deps, req, nullEventHandler{}); err != nil {
		t.Fatalf("RunAgent: %v", err)
	}

	if got := getPending(); got != 0 {
		t.Fatalf("sourced path: row should be consumed by the pre-loop save before the first LLM call, pending = %d, want 0", got)
	}
	pendAfter, _ := store.LoadPendingByRoute(routeKey)
	if len(pendAfter) != 0 {
		t.Fatalf("after run, sourced row must stay consumed, pending = %d", len(pendAfter))
	}
	assertDrainedTextPersisted(t, deps, sessIDForRoute(t, deps, routeKey), "queued from slack")
}

// TestRunAgent_MailboxStaysPendingOnHardError pins the crash-durability
// guarantee that is the whole reason for issue #163: a turn that hard-errors
// before any clean content save must leave the drained row PENDING
// (consumed_at IS NULL) so daemon-startup recovery replays it. The hard-error
// stub save is deliberately not a consumeDrainedMailbox call site.
func TestRunAgent_MailboxStaysPendingOnHardError(t *testing.T) {
	const sessID = "ordering-test-163-harderror"
	routeKey := "session:" + sanitizeRouteValue(sessID)

	deps, store := mailboxOrderingDeps(t)
	defer deps.SessionCache.CloseAll()

	// Always-500 gateway → the first (and only) LLM call hard-errors before any
	// checkpoint or final save can persist the drained text and consume the row.
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "synthetic upstream failure", http.StatusInternalServerError)
	}))
	defer ts.Close()
	deps.GW = client.NewGatewayClient(ts.URL, "test-key")

	if out, err := deps.SessionCache.EnqueueMessage(routeKey, agenttypes.QueuedMessage{
		ID: "mbx-harderr-1", Source: "ws", Text: "queued question",
		Priority: agenttypes.PriorityNext, EnqueuedAt: time.Now(),
	}); err != nil || out != MailboxQueued {
		t.Fatalf("seed enqueue: out=%v err=%v", out, err)
	}

	_, err := RunAgent(context.Background(), deps, RunAgentRequest{
		Text:       "live prompt",
		SessionID:  sessID,
		NewSession: true, // empty Source → no pre-loop save; hard error before final save
	}, nullEventHandler{})
	if err == nil {
		t.Fatal("expected hard error from always-500 gateway")
	}

	pend, lerr := store.LoadPendingByRoute(routeKey)
	if lerr != nil {
		t.Fatalf("load pending: %v", lerr)
	}
	if len(pend) != 1 {
		t.Fatalf("hard error must leave the drained row pending for recovery, pending = %d, want 1", len(pend))
	}
}

// assertDrainedTextPersisted resumes the session by id and fails unless some
// user message contains want (the drained mailbox text is prepended into the
// run's first user turn).
func assertDrainedTextPersisted(t *testing.T, deps *ServerDeps, sessID, want string) {
	t.Helper()
	mgr := session.NewManager(deps.SessionCache.SessionsDir(""))
	defer mgr.Close()
	sess, err := mgr.Resume(sessID)
	if err != nil {
		t.Fatalf("resume %q to verify persisted text: %v", sessID, err)
	}
	for _, m := range sess.Messages {
		if m.Role == "user" && strings.Contains(m.Content.Text(), want) {
			return
		}
	}
	t.Fatalf("drained text %q not found in any persisted user message of session %q", want, sessID)
}

// sessIDForRoute returns the session id currently bound to routeKey in the
// cache (set during RunAgent). Used by the sourced-path test whose session id
// is daemon-minted rather than client-supplied.
func sessIDForRoute(t *testing.T, deps *ServerDeps, routeKey string) string {
	t.Helper()
	id := deps.SessionCache.RouteSessionID(routeKey)
	if id == "" {
		t.Fatalf("no session id bound to route %q", routeKey)
	}
	return id
}
