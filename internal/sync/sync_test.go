package sync

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	stdsync "sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/session"
)

// stubAudit captures audit events for assertions.
type stubAudit struct {
	mu     stdsync.Mutex
	events []map[string]any
}

func (s *stubAudit) Log(event string, fields map[string]any) {
	merged := map[string]any{"_event": event}
	for k, v := range fields {
		merged[k] = v
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, merged)
}

func (s *stubAudit) snapshot() []map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]map[string]any(nil), s.events...)
}

// stubUploader returns a canned response without hitting the network.
type stubUploader struct {
	respFn func(client.SyncBatchRequest) (client.SyncBatchResponse, error)
	calls  int
}

func (s *stubUploader) Send(_ context.Context, batch client.SyncBatchRequest) (client.SyncBatchResponse, error) {
	s.calls++
	return s.respFn(batch)
}

func TestSyncRun_HappyPath(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	// Seed one session in the default sessions dir.
	sd := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	idx, err := session.OpenIndex(sd)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	sess := &session.Session{
		ID:        "s1",
		CreatedAt: now.Add(-1 * time.Minute),
		UpdatedAt: now.Add(-1 * time.Minute),
	}
	if err := idx.UpsertSession(sess); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	idx.Close()
	// Also write the JSON file so the loader finds it.
	jsonPath := filepath.Join(sd, "s1.json")
	body, _ := json.Marshal(sess)
	if err := os.WriteFile(jsonPath, body, 0o644); err != nil {
		t.Fatalf("write session json: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Enabled = true

	uploader := &stubUploader{
		respFn: func(b client.SyncBatchRequest) (client.SyncBatchResponse, error) {
			return client.SyncBatchResponse{Accepted: []string{"s1"}}, nil
		},
	}
	audit := &stubAudit{}
	loader := func(dir, id string) ([]byte, error) {
		return os.ReadFile(filepath.Join(dir, id+".json"))
	}

	deps := Deps{
		Cfg:       cfg,
		HomeDir:   home,
		ClientVer: "shanclaw/test",
		Uploader:  uploader,
		Loader:    loader,
		Audit:     audit,
		Now:       func() time.Time { return now },
	}

	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}

	if uploader.calls != 1 {
		t.Errorf("expected 1 upload call, got %d", uploader.calls)
	}

	m, err := ReadMarker(filepath.Join(home, "sync_marker.json"))
	if err != nil {
		t.Fatalf("ReadMarker: %v", err)
	}
	if !m.LastSyncAt.Equal(sess.UpdatedAt) {
		t.Errorf("LastSyncAt: got %v, want %v", m.LastSyncAt, sess.UpdatedAt)
	}
	if m.LastSyncCount != 1 {
		t.Errorf("LastSyncCount: got %d, want 1", m.LastSyncCount)
	}
	if m.LastSyncOutcome != OutcomeOK {
		t.Errorf("LastSyncOutcome: got %q, want %q", m.LastSyncOutcome, OutcomeOK)
	}

	if len(audit.snapshot()) == 0 {
		t.Errorf("expected at least one audit event")
	}
}

func TestSyncRun_DisabledIsNoop(t *testing.T) {
	home := t.TempDir()

	cfg := DefaultConfig()
	cfg.Enabled = false

	uploader := &stubUploader{respFn: func(b client.SyncBatchRequest) (client.SyncBatchResponse, error) {
		t.Fatalf("uploader must not be called when disabled")
		return client.SyncBatchResponse{}, nil
	}}
	audit := &stubAudit{}

	deps := Deps{
		Cfg: cfg, HomeDir: home, Uploader: uploader, Audit: audit,
		Now: func() time.Time { return time.Now().UTC() },
	}
	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run (disabled): %v", err)
	}
	if uploader.calls != 0 {
		t.Errorf("expected 0 upload calls, got %d", uploader.calls)
	}
	events := audit.snapshot()
	if len(events) != 1 || events[0]["outcome"] != OutcomeNoop {
		t.Errorf("expected single noop audit event, got %+v", events)
	}
}

func TestSyncRun_FlockSerializes(t *testing.T) {
	// Two concurrent Run calls on the same HomeDir; second should block then
	// either no-op (marker advanced) or run fresh.
	home := t.TempDir()
	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.LockTimeout = 5 * time.Second

	uploader := &stubUploader{respFn: func(b client.SyncBatchRequest) (client.SyncBatchResponse, error) {
		return client.SyncBatchResponse{}, nil
	}}
	deps := Deps{
		Cfg: cfg, HomeDir: home, Uploader: uploader, Audit: &stubAudit{},
		Loader: func(dir, id string) ([]byte, error) { return nil, os.ErrNotExist },
		Now:    func() time.Time { return time.Now().UTC() },
	}

	done := make(chan error, 2)
	go func() { done <- Run(context.Background(), deps) }()
	go func() { done <- Run(context.Background(), deps) }()

	for i := 0; i < 2; i++ {
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("Run #%d: %v", i, err)
			}
		case <-time.After(10 * time.Second):
			t.Fatalf("Run #%d timed out — flock likely deadlocked", i)
		}
	}
}

func TestSyncRun_PartialOutcome(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)

	// Seed two sessions.
	sd := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	idx, err := session.OpenIndex(sd)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	for _, id := range []string{"good", "bad"} {
		s := &session.Session{ID: id, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)}
		if err := idx.UpsertSession(s); err != nil {
			t.Fatalf("UpsertSession %s: %v", id, err)
		}
		body, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(sd, id+".json"), body, 0o644); err != nil {
			t.Fatalf("write session json %s: %v", id, err)
		}
	}
	idx.Close()

	cfg := DefaultConfig()
	cfg.Enabled = true

	uploader := &stubUploader{
		respFn: func(b client.SyncBatchRequest) (client.SyncBatchResponse, error) {
			return client.SyncBatchResponse{
				Accepted: []string{"good"},
				Rejected: []client.RejectedEntry{
					{ID: "bad", Reason: "cloud_rejected_retryable"},
				},
			}, nil
		},
	}
	deps := Deps{
		Cfg: cfg, HomeDir: home, Uploader: uploader, Audit: &stubAudit{},
		Loader: func(dir, id string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, id+".json"))
		},
		Now: func() time.Time { return now },
	}
	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	m, _ := ReadMarker(filepath.Join(home, "sync_marker.json"))
	if m.LastSyncOutcome != OutcomePartial {
		t.Errorf("outcome: got %q, want partial", m.LastSyncOutcome)
	}
	if m.LastSyncCount != 1 {
		t.Errorf("accepted count: got %d, want 1", m.LastSyncCount)
	}
	fe, ok := m.Failed["bad"]
	if !ok {
		t.Fatalf("expected marker.Failed[bad]")
	}
	if fe.Category != CategoryTransient {
		t.Errorf("Category: got %q, want transient", fe.Category)
	}
	if fe.NextAttemptAt == nil {
		t.Errorf("transient should have NextAttemptAt set")
	}
}

func TestSyncRun_TransportErrorPreservesMarker(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	sd := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	idx, err := session.OpenIndex(sd)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	s := &session.Session{ID: "x", CreatedAt: now, UpdatedAt: now}
	if err := idx.UpsertSession(s); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	body, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(sd, "x.json"), body, 0o644); err != nil {
		t.Fatalf("write session json: %v", err)
	}
	idx.Close()

	cfg := DefaultConfig()
	cfg.Enabled = true

	uploader := &stubUploader{
		respFn: func(b client.SyncBatchRequest) (client.SyncBatchResponse, error) {
			return client.SyncBatchResponse{}, fmt.Errorf("network down")
		},
	}
	deps := Deps{
		Cfg: cfg, HomeDir: home, Uploader: uploader, Audit: &stubAudit{},
		Loader: func(dir, id string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, id+".json"))
		},
		Now: func() time.Time { return now },
	}
	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run should not return error on transport failure (it logs and noops the marker): %v", err)
	}
	m, _ := ReadMarker(filepath.Join(home, "sync_marker.json"))
	if m.LastSyncOutcome != OutcomeTransportError {
		t.Errorf("outcome: got %q, want transport_error", m.LastSyncOutcome)
	}
	if !m.LastSyncAt.IsZero() {
		t.Errorf("LastSyncAt should NOT advance on transport error; got %v", m.LastSyncAt)
	}
}

func TestSyncRun_PermanentFailureDoesNotChurn(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	sd := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}

	// Seed an oversized session (so single_session_max_bytes triggers).
	idx, err := session.OpenIndex(sd)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	s := &session.Session{ID: "huge", CreatedAt: now, UpdatedAt: now}
	if err := idx.UpsertSession(s); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	idx.Close()

	// Loader returns a body well above the cap.
	loader := func(dir, id string) ([]byte, error) {
		return []byte(strings.Repeat("a", 5*1024*1024)), nil // 5 MB
	}

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.SingleSessionMaxBytes = 4 * 1024 * 1024 // 4 MB cap

	uploader := &stubUploader{respFn: func(b client.SyncBatchRequest) (client.SyncBatchResponse, error) {
		t.Fatalf("uploader should not be called for oversized session")
		return client.SyncBatchResponse{}, nil
	}}
	deps := Deps{
		Cfg: cfg, HomeDir: home, Uploader: uploader, Audit: &stubAudit{},
		Loader: loader,
		Now:    func() time.Time { return now },
	}

	// Run #1: oversized session is recorded as permanent failure with attempts=1.
	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run #1: %v", err)
	}
	m1, _ := ReadMarker(filepath.Join(home, "sync_marker.json"))
	fe1, ok := m1.Failed["huge"]
	if !ok {
		t.Fatalf("Run #1: expected marker.Failed[huge]")
	}
	if fe1.Category != CategoryPermanent {
		t.Fatalf("Run #1: category got %q, want permanent", fe1.Category)
	}
	if fe1.Attempts != 1 {
		t.Errorf("Run #1: attempts got %d, want 1", fe1.Attempts)
	}
	if !fe1.LastObservedUpdatedAt.Equal(now) {
		t.Errorf("Run #1: LastObservedUpdatedAt got %v, want %v", fe1.LastObservedUpdatedAt, now)
	}

	// Run #2 (same data, no edit): attempts MUST stay at 1.
	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run #2: %v", err)
	}
	m2, _ := ReadMarker(filepath.Join(home, "sync_marker.json"))
	fe2, ok := m2.Failed["huge"]
	if !ok {
		t.Fatalf("Run #2: marker.Failed[huge] should still exist")
	}
	if fe2.Attempts != 1 {
		t.Errorf("Run #2 (no churn): attempts got %d, want 1 (permanent failure must not churn)", fe2.Attempts)
	}

	// Run #3 (same data, third time): still 1.
	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run #3: %v", err)
	}
	m3, _ := ReadMarker(filepath.Join(home, "sync_marker.json"))
	if m3.Failed["huge"].Attempts != 1 {
		t.Errorf("Run #3 (no churn): attempts got %d, want 1", m3.Failed["huge"].Attempts)
	}

	// Now simulate a session edit: bump UpdatedAt and re-Upsert. The next
	// Run should attempt again (attempts → 2) because LastObservedUpdatedAt
	// is now older than the new UpdatedAt.
	idx2, err := session.OpenIndex(sd)
	if err != nil {
		t.Fatalf("OpenIndex (post-edit): %v", err)
	}
	editedTime := now.Add(1 * time.Hour)
	s.UpdatedAt = editedTime
	if err := idx2.UpsertSession(s); err != nil {
		t.Fatalf("UpsertSession (post-edit): %v", err)
	}
	idx2.Close()

	deps.Now = func() time.Time { return editedTime.Add(1 * time.Minute) }
	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run #4 (post-edit): %v", err)
	}
	m4, _ := ReadMarker(filepath.Join(home, "sync_marker.json"))
	if m4.Failed["huge"].Attempts != 2 {
		t.Errorf("Run #4 (post-edit): attempts got %d, want 2 (edit must trigger fresh attempt)", m4.Failed["huge"].Attempts)
	}
}

// TestSyncRun_SentCountReflectsAttemptedBatchesOnly verifies the audit
// "sent" field counts only sessions in batches the uploader actually saw,
// not sessions queued behind a transport error. With BatchMaxSessions=1 and
// 3 candidates, the uploader is called for batch #1 (success) and batch #2
// (transport error); batch #3 is never attempted. "sent" must equal 2.
func TestSyncRun_SentCountReflectsAttemptedBatchesOnly(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	sd := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}

	idx, err := session.OpenIndex(sd)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	for _, id := range []string{"s1", "s2", "s3"} {
		s := &session.Session{ID: id, CreatedAt: now.Add(-time.Minute), UpdatedAt: now.Add(-time.Minute)}
		if err := idx.UpsertSession(s); err != nil {
			t.Fatalf("UpsertSession %s: %v", id, err)
		}
		body, _ := json.Marshal(s)
		if err := os.WriteFile(filepath.Join(sd, id+".json"), body, 0o644); err != nil {
			t.Fatalf("write %s.json: %v", id, err)
		}
	}
	idx.Close()

	cfg := DefaultConfig()
	cfg.Enabled = true
	cfg.BatchMaxSessions = 1 // force three single-session batches

	uploader := &stubUploader{
		respFn: func(b client.SyncBatchRequest) (client.SyncBatchResponse, error) {
			// First call: accept whatever ID it has. Subsequent calls: transport error.
			// stubUploader.calls is incremented BEFORE respFn runs, so calls==1 is the first.
			return client.SyncBatchResponse{}, nil // overridden below
		},
	}
	uploader.respFn = func(b client.SyncBatchRequest) (client.SyncBatchResponse, error) {
		if uploader.calls == 1 {
			ids := make([]string, 0, len(b.Sessions))
			for _, env := range b.Sessions {
				var probe struct {
					ID string `json:"id"`
				}
				_ = json.Unmarshal(env.Session, &probe)
				ids = append(ids, probe.ID)
			}
			return client.SyncBatchResponse{Accepted: ids}, nil
		}
		return client.SyncBatchResponse{}, fmt.Errorf("network down")
	}

	auditSink := &stubAudit{}
	deps := Deps{
		Cfg: cfg, HomeDir: home, Uploader: uploader, Audit: auditSink,
		Loader: func(dir, id string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, id+".json"))
		},
		Now: func() time.Time { return now },
	}

	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if uploader.calls != 2 {
		t.Fatalf("expected 2 uploader calls (1 success + 1 transport error, then break); got %d", uploader.calls)
	}

	// Find the main session_sync audit event (not the noop variants).
	var mainEvent map[string]any
	events := auditSink.snapshot()
	for _, e := range events {
		if e["_event"] == "session_sync" {
			if _, hasSent := e["sent"]; hasSent {
				mainEvent = e
				break
			}
		}
	}
	if mainEvent == nil {
		t.Fatalf("expected a session_sync audit event with 'sent' field; got %+v", events)
	}
	gotSent, _ := mainEvent["sent"].(int)
	if gotSent != 2 {
		t.Errorf("audit 'sent': got %d, want 2 (only attempted batches: #1 succeeded, #2 errored, #3 never sent)", gotSent)
	}
	if mainEvent["outcome"] != OutcomeTransportError {
		t.Errorf("outcome: got %v, want transport_error", mainEvent["outcome"])
	}
}

func TestSyncRun_429SingleAttemptNoLoop(t *testing.T) {
	home := t.TempDir()
	now := time.Now().UTC().Truncate(time.Second)
	sd := filepath.Join(home, "sessions")
	if err := os.MkdirAll(sd, 0o755); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}

	idx, err := session.OpenIndex(sd)
	if err != nil {
		t.Fatalf("OpenIndex: %v", err)
	}
	s := &session.Session{ID: "x", CreatedAt: now, UpdatedAt: now}
	if err := idx.UpsertSession(s); err != nil {
		t.Fatalf("UpsertSession: %v", err)
	}
	idx.Close()
	body, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(sd, "x.json"), body, 0o644); err != nil {
		t.Fatalf("write session json: %v", err)
	}

	cfg := DefaultConfig()
	cfg.Enabled = true

	uploader := &stubUploader{
		respFn: func(b client.SyncBatchRequest) (client.SyncBatchResponse, error) {
			return client.SyncBatchResponse{}, fmt.Errorf("sync returned 429: rate limited")
		},
	}
	deps := Deps{
		Cfg: cfg, HomeDir: home, Uploader: uploader, Audit: &stubAudit{},
		Loader: func(dir, id string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, id+".json"))
		},
		Now: func() time.Time { return now },
	}

	if err := Run(context.Background(), deps); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if uploader.calls != 1 {
		t.Errorf("429 must not loop: uploader.calls = %d, want 1", uploader.calls)
	}
	m, _ := ReadMarker(filepath.Join(home, "sync_marker.json"))
	if m.LastSyncOutcome != OutcomeTransportError {
		t.Errorf("outcome: got %q, want transport_error", m.LastSyncOutcome)
	}
	if !m.LastSyncAt.IsZero() {
		t.Errorf("LastSyncAt must NOT advance on 429; got %v", m.LastSyncAt)
	}
}

func TestAcquireFlock_RespectsContextCancellation(t *testing.T) {
	dir := t.TempDir()
	lockPath := filepath.Join(dir, "test.lock")

	// First caller holds the lock.
	releaseFirst, err := acquireFlock(context.Background(), lockPath, 30*time.Second)
	if err != nil {
		t.Fatalf("first acquireFlock: %v", err)
	}
	defer releaseFirst()

	// Second caller blocks on the lock with a long timeout, but its ctx will
	// be canceled. It must return promptly with ctx.Err(), NOT wait the full
	// 30s LockTimeout.
	ctx, cancel := context.WithCancel(context.Background())
	type result struct {
		release func()
		err     error
		elapsed time.Duration
	}
	done := make(chan result, 1)
	go func() {
		start := time.Now()
		rel, err := acquireFlock(ctx, lockPath, 30*time.Second)
		done <- result{release: rel, err: err, elapsed: time.Since(start)}
	}()

	// Let the second caller block for a moment, then cancel.
	time.Sleep(150 * time.Millisecond)
	cancel()

	select {
	case r := <-done:
		if r.err == nil {
			t.Fatalf("expected ctx.Err() after cancellation, got nil error (lock acquired?)")
			r.release()
		}
		if !errors.Is(r.err, context.Canceled) {
			t.Errorf("expected context.Canceled, got %v", r.err)
		}
		if r.elapsed > 1*time.Second {
			t.Errorf("acquireFlock took %v after cancel — should return promptly (well under 1s)", r.elapsed)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("acquireFlock did not return within 2s after ctx cancel — ctx not respected")
	}
}
