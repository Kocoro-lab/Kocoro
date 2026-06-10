package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
)

// TestE2E_InjectEndpoint_Attachment_Injects verifies an attachment-bearing
// follow-up to an in-flight run is injected (lowered to InjectedMessage.Files)
// rather than rejected with 409 — the busy-state attachment-inject path.
func TestE2E_InjectEndpoint_Attachment_Injects(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)
	injectCh := make(chan agent.InjectedMessage, 5)
	sc.mu.Lock()
	sc.routes["session:sess-att"] = &routeEntry{injectCh: injectCh, done: make(chan struct{})}
	sc.mu.Unlock()

	deps := &ServerDeps{SessionCache: sc, ShannonDir: dir, AgentsDir: dir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx)
	time.Sleep(100 * time.Millisecond)

	// document content block (base64) exercises contentBlocksToInjected's
	// document passthrough into InjectedMessage.Files.
	body := strings.NewReader(`{"text":"look at this doc","session_id":"sess-att","source":"shanclaw","content":[{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQK"}}]}`)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/message", srv.Port()), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (attachment injected, not 409), got %d", resp.StatusCode)
	}
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "injected" {
		t.Errorf("expected status=injected, got %v", result)
	}
	select {
	case msg := <-injectCh:
		if msg.Text != "look at this doc" {
			t.Errorf("text = %q, want 'look at this doc'", msg.Text)
		}
		if len(msg.Files) != 1 || msg.Files[0].Type != "document" || msg.Files[0].Data != "JVBERi0xLjQK" {
			t.Fatalf("expected 1 document file (passthrough), got %+v", msg.Files)
		}
	default:
		t.Fatal("expected injected message with attachment in channel")
	}
}

// TestE2E_InjectEndpoint_InjectOnly_NoActiveRun_Returns409 verifies inject_only
// requests 409 (never start a new run) when no active run owns the route — the
// race guard that keeps the Desktop client's local-queue fallback from
// duplicating a run.
func TestE2E_InjectEndpoint_InjectOnly_NoActiveRun_Returns409(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir) // no route registered → HasActiveRun is false

	deps := &ServerDeps{SessionCache: sc, ShannonDir: dir, AgentsDir: dir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx)
	time.Sleep(100 * time.Millisecond)

	body := strings.NewReader(`{"text":"steer","session_id":"sess-none","source":"shanclaw","inject_only":true}`)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/message", srv.Port()), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("expected 409 (inject_only, no active run — must not start a new run), got %d", resp.StatusCode)
	}
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "rejected" || result["reason"] != "no_active_run" {
		t.Errorf("expected rejected/no_active_run, got %v", result)
	}
}

// TestE2E_InjectEndpoint_TextOnly_PropagatesClientMessageID verifies a plain
// text follow-up (no content blocks) to an in-flight run is injected AND its
// client_message_id round-trips into InjectedMessage.ClientMessageID. The id is
// load-bearing: the loop echoes it via injected_committed and Desktop keys its
// queued-draft card on it — a regression that drops it on the busy text path
// would silently break card-commit while the attachment path still worked.
func TestE2E_InjectEndpoint_TextOnly_PropagatesClientMessageID(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)
	injectCh := make(chan agent.InjectedMessage, 5)
	sc.mu.Lock()
	sc.routes["session:sess-txt"] = &routeEntry{injectCh: injectCh, done: make(chan struct{})}
	sc.mu.Unlock()

	deps := &ServerDeps{SessionCache: sc, ShannonDir: dir, AgentsDir: dir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx)
	time.Sleep(100 * time.Millisecond)

	body := strings.NewReader(`{"text":"also save to notion","session_id":"sess-txt","source":"desktop","inject_only":true,"client_message_id":"local-xyz"}`)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/message", srv.Port()), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 (text follow-up injected), got %d", resp.StatusCode)
	}
	var result map[string]string
	json.NewDecoder(resp.Body).Decode(&result)
	if result["status"] != "injected" {
		t.Errorf("expected status=injected, got %v", result)
	}
	select {
	case msg := <-injectCh:
		if msg.Text != "also save to notion" {
			t.Errorf("text = %q, want 'also save to notion'", msg.Text)
		}
		if msg.ClientMessageID != "local-xyz" {
			t.Fatalf("client_message_id = %q, want 'local-xyz' (load-bearing for card-commit)", msg.ClientMessageID)
		}
		if len(msg.Files) != 0 {
			t.Errorf("expected no files for text-only inject, got %+v", msg.Files)
		}
	default:
		t.Fatal("expected injected text message in channel")
	}
}

// TestE2E_RetractEndpoint_NoActiveRun_PlantsDurableTombstone verifies POST
// /inject/retract for a route with no in-flight run is idempotent (200) AND
// records a tombstone. This deliberately inverts the pre-2026-06 "no tombstone
// without an active run" rule: the no-active-run window is exactly where a
// retract racing run teardown used to lose — its target could land on the
// replacement run or already sit in the mailbox. Unbounded growth is handled
// by TTL + per-route cap (pruneInjectLedgerLocked), not by refusing to record.
func TestE2E_RetractEndpoint_NoActiveRun_PlantsDurableTombstone(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir) // no route registered → HasActiveRun false

	deps := &ServerDeps{SessionCache: sc, ShannonDir: dir, AgentsDir: dir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx)
	time.Sleep(100 * time.Millisecond)

	body := strings.NewReader(`{"client_message_id":"local-ghost","session_id":"sess-x","source":"desktop"}`)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/inject/retract", srv.Port()), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200 idempotent, got %d", resp.StatusCode)
	}
	var payload map[string]string
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if payload["status"] != "retracted" {
		t.Fatalf("status = %q, want retracted", payload["status"])
	}
	if !sc.ConsumeInjectRetracted("session:sess-x", "local-ghost") {
		t.Fatal("retract without an active run must plant a durable tombstone")
	}
}

// TestE2E_RetractEndpoint_ActiveRun_RecordsTombstone verifies the happy path:
// retract for a route WITH an in-flight run records a one-shot tombstone the
// loop's drain will consume.
func TestE2E_RetractEndpoint_ActiveRun_RecordsTombstone(t *testing.T) {
	dir := t.TempDir()
	sc := NewSessionCache(dir)
	sc.mu.Lock()
	sc.routes["session:sess-live"] = &routeEntry{injectCh: make(chan agent.InjectedMessage, 5), done: make(chan struct{})}
	sc.mu.Unlock()

	deps := &ServerDeps{SessionCache: sc, ShannonDir: dir, AgentsDir: dir}
	c := NewClient("ws://localhost:1/x", "", func(msg MessagePayload) string { return "" }, nil)
	srv := NewServer(0, c, deps, "test")
	srvCtx, srvCancel := context.WithCancel(context.Background())
	defer srvCancel()
	go srv.Start(srvCtx)
	time.Sleep(100 * time.Millisecond)

	body := strings.NewReader(`{"client_message_id":"local-live","session_id":"sess-live","source":"desktop"}`)
	resp, err := http.Post(fmt.Sprintf("http://127.0.0.1:%d/inject/retract", srv.Port()), "application/json", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	if !sc.ConsumeInjectRetracted("session:sess-live", "local-live") {
		t.Fatal("expected a tombstone recorded for the active route")
	}
}
