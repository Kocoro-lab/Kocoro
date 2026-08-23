package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const (
	testRealtimePrincipalA = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	testRealtimePrincipalB = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
)

func realtimeUsageFixture(responseID string) []byte {
	body, _ := json.Marshal(map[string]any{
		"provider":    "cloud",
		"model":       "realtime",
		"response_id": responseID,
		"usage":       map[string]any{"input_tokens": 2, "output_tokens": 3},
	})
	return body
}

func TestRealtimeUsageOutboxFailureRetainsThenSuccessDeletes(t *testing.T) {
	outbox := newRealtimeUsageOutbox(t.TempDir())
	body := realtimeUsageFixture("response-1")
	created, err := outbox.enqueue(body, testRealtimePrincipalA)
	if err != nil || !created {
		t.Fatalf("enqueue = created=%t err=%v", created, err)
	}
	fail := func(context.Context, json.RawMessage) error { return errors.New("temporary failure") }
	if err := outbox.replay(context.Background(), testRealtimePrincipalA, fail); err == nil {
		t.Fatal("failed replay must report an error")
	}
	paths, err := outbox.pendingPaths(testRealtimePrincipalA)
	if err != nil || len(paths) != 1 {
		t.Fatalf("failed replay pending paths = %v, err=%v; want one", paths, err)
	}
	if err := outbox.replay(context.Background(), testRealtimePrincipalA, func(context.Context, json.RawMessage) error { return nil }); err != nil {
		t.Fatalf("successful replay: %v", err)
	}
	paths, err = outbox.pendingPaths(testRealtimePrincipalA)
	if err != nil || len(paths) != 0 {
		t.Fatalf("successful replay pending paths = %v, err=%v; want empty", paths, err)
	}
}

func TestRealtimeUsageOutboxRestartReplaysPendingEntry(t *testing.T) {
	dir := t.TempDir()
	first := newRealtimeUsageOutbox(dir)
	if _, err := first.enqueue(realtimeUsageFixture("response-restart"), testRealtimePrincipalA); err != nil {
		t.Fatalf("enqueue before restart: %v", err)
	}

	second := newRealtimeUsageOutbox(dir)
	var delivered []byte
	if err := second.replay(context.Background(), testRealtimePrincipalA, func(_ context.Context, body json.RawMessage) error {
		delivered = append([]byte(nil), body...)
		return nil
	}); err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if string(delivered) != string(realtimeUsageFixture("response-restart")) {
		t.Fatalf("delivered body = %s, want original body", delivered)
	}
	if paths, err := second.pendingPaths(testRealtimePrincipalA); err != nil || len(paths) != 0 {
		t.Fatalf("pending after restart replay = %v, err=%v; want empty", paths, err)
	}
}

func TestRealtimeUsageOutboxRestartRecoversFsyncedPendingTemp(t *testing.T) {
	dir := t.TempDir()
	first := newRealtimeUsageOutbox(dir)
	if err := first.ensurePrincipalDir(testRealtimePrincipalA); err != nil {
		t.Fatalf("create principal dir: %v", err)
	}
	body := realtimeUsageFixture("response-crash-window")
	tmp, err := os.CreateTemp(filepath.Join(first.dir, testRealtimePrincipalA), ".pending-*")
	if err != nil {
		t.Fatalf("create pending temp: %v", err)
	}
	tmpPath := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		t.Fatalf("chmod pending temp: %v", err)
	}
	if _, err := tmp.Write(body); err != nil {
		t.Fatalf("write pending temp: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		t.Fatalf("sync pending temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close pending temp: %v", err)
	}
	// Simulate a crash after the temp fsync but before canonical rename.
	second := newRealtimeUsageOutbox(dir)
	var delivered []byte
	if err := second.replay(context.Background(), testRealtimePrincipalA, func(_ context.Context, got json.RawMessage) error {
		delivered = append([]byte(nil), got...)
		return nil
	}); err != nil {
		t.Fatalf("replay recovered temp: %v", err)
	}
	if string(delivered) != string(body) {
		t.Fatalf("recovered body = %s, want %s", delivered, body)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("pending temp still exists, err=%v", err)
	}
}

func TestRealtimeUsageOutboxRemovesInvalidPendingTemp(t *testing.T) {
	outbox := newRealtimeUsageOutbox(t.TempDir())
	if err := outbox.ensurePrincipalDir(testRealtimePrincipalA); err != nil {
		t.Fatalf("create principal dir: %v", err)
	}
	tmp, err := os.CreateTemp(filepath.Join(outbox.dir, testRealtimePrincipalA), ".pending-*")
	if err != nil {
		t.Fatalf("create invalid pending temp: %v", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write([]byte(`{"response_id":"partial"`)); err != nil {
		t.Fatalf("write invalid pending temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close invalid pending temp: %v", err)
	}
	if _, err := outbox.enqueue(realtimeUsageFixture("response-after-invalid"), testRealtimePrincipalA); err != nil {
		t.Fatalf("enqueue after invalid temp: %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("invalid pending temp was not removed, err=%v", err)
	}
	if paths, err := outbox.pendingPaths(testRealtimePrincipalA); err != nil || len(paths) != 1 {
		t.Fatalf("valid queue after invalid temp = %v, err=%v; want one", paths, err)
	}
}

func TestRealtimeUsageOutboxRecoveryDoesNotOverwriteCanonicalEntry(t *testing.T) {
	dir := t.TempDir()
	outbox := newRealtimeUsageOutbox(dir)
	original := realtimeUsageFixture("response-recovery-dedup")
	if _, err := outbox.enqueue(original, testRealtimePrincipalA); err != nil {
		t.Fatalf("enqueue canonical entry: %v", err)
	}
	if err := outbox.ensurePrincipalDir(testRealtimePrincipalA); err != nil {
		t.Fatalf("ensure principal dir: %v", err)
	}
	duplicate := []byte(`{"provider":"different","model":"new","response_id":"response-recovery-dedup","usage":{"input_tokens":99}}`)
	tmp, err := os.CreateTemp(filepath.Join(outbox.dir, testRealtimePrincipalA), ".pending-*")
	if err != nil {
		t.Fatalf("create duplicate pending temp: %v", err)
	}
	tmpPath := tmp.Name()
	if _, err := tmp.Write(duplicate); err != nil {
		t.Fatalf("write duplicate pending temp: %v", err)
	}
	if err := tmp.Sync(); err != nil {
		t.Fatalf("sync duplicate pending temp: %v", err)
	}
	if err := tmp.Close(); err != nil {
		t.Fatalf("close duplicate pending temp: %v", err)
	}
	if err := outbox.replay(context.Background(), testRealtimePrincipalA, func(_ context.Context, got json.RawMessage) error {
		if string(got) != string(original) {
			t.Fatalf("replayed canonical body = %s, want %s", got, original)
		}
		return nil
	}); err != nil {
		t.Fatalf("replay canonical entry: %v", err)
	}
	if _, err := os.Stat(tmpPath); !os.IsNotExist(err) {
		t.Fatalf("duplicate pending temp still exists, err=%v", err)
	}
}

func TestRealtimeUsageOutboxDeduplicatesPendingResponseID(t *testing.T) {
	outbox := newRealtimeUsageOutbox(t.TempDir())
	firstBody := realtimeUsageFixture("response-same")
	secondBody := realtimeUsageFixture("response-same")
	created, err := outbox.enqueue(firstBody, testRealtimePrincipalA)
	if err != nil || !created {
		t.Fatalf("first enqueue = created=%t err=%v", created, err)
	}
	created, err = outbox.enqueue(secondBody, testRealtimePrincipalA)
	if err != nil || created {
		t.Fatalf("duplicate enqueue = created=%t err=%v; want false,nil", created, err)
	}
	paths, err := outbox.pendingPaths(testRealtimePrincipalA)
	if err != nil || len(paths) != 1 {
		t.Fatalf("deduplicated paths = %v, err=%v; want one", paths, err)
	}
	stored, err := os.ReadFile(filepath.Clean(paths[0]))
	if err != nil {
		t.Fatalf("read stored body: %v", err)
	}
	if string(stored) != string(firstBody) {
		t.Fatalf("duplicate replaced stored body: got %s, want %s", stored, firstBody)
	}
}

func TestRealtimeUsageOutboxEnqueueIsNotBlockedByCloudReplay(t *testing.T) {
	outbox := newRealtimeUsageOutbox(t.TempDir())
	if _, err := outbox.enqueue(realtimeUsageFixture("response-in-flight"), testRealtimePrincipalA); err != nil {
		t.Fatalf("enqueue first report: %v", err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- outbox.replay(context.Background(), testRealtimePrincipalA, func(context.Context, json.RawMessage) error {
			close(started)
			<-release
			return nil
		})
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("replay did not reach provider")
	}
	enqueueDone := make(chan error, 1)
	go func() {
		_, err := outbox.enqueue(realtimeUsageFixture("response-during-replay"), testRealtimePrincipalA)
		enqueueDone <- err
	}()
	select {
	case err := <-enqueueDone:
		if err != nil {
			t.Fatalf("enqueue during replay: %v", err)
		}
	case <-time.After(time.Second):
		close(release)
		t.Fatal("enqueue waited for Cloud replay to finish")
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("replay: %v", err)
	}
}

func TestRealtimeUsageOutboxDoesNotReplayAcrossPrincipals(t *testing.T) {
	outbox := newRealtimeUsageOutbox(t.TempDir())
	if _, err := outbox.enqueue(realtimeUsageFixture("response-account-switch"), testRealtimePrincipalA); err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	var sent int
	send := func(context.Context, json.RawMessage) error {
		sent++
		return nil
	}
	if err := outbox.replay(context.Background(), testRealtimePrincipalB, send); err != nil {
		t.Fatalf("replay under another principal: %v", err)
	}
	if sent != 0 {
		t.Fatalf("cross-principal replay sent %d entries", sent)
	}
	if paths, err := outbox.pendingPaths(testRealtimePrincipalA); err != nil || len(paths) != 1 {
		t.Fatalf("old principal entry = %v, err=%v; want retained", paths, err)
	}
	if err := outbox.replay(context.Background(), testRealtimePrincipalA, send); err != nil {
		t.Fatalf("replay under original principal: %v", err)
	}
	if sent != 1 {
		t.Fatalf("original principal sent %d entries, want 1", sent)
	}
}

func TestRealtimeUsageOutboxRejectsMissingResponseID(t *testing.T) {
	outbox := newRealtimeUsageOutbox(t.TempDir())
	if _, err := outbox.enqueue([]byte(`{"usage":{"input_tokens":1}}`), testRealtimePrincipalA); err == nil {
		t.Fatal("missing response_id must be rejected")
	}
}

func TestRealtimeUsageOutboxRejectsUntrustedPrincipalPath(t *testing.T) {
	outbox := newRealtimeUsageOutbox(t.TempDir())
	if _, err := outbox.enqueue(realtimeUsageFixture("response-principal"), "../../other-account"); err == nil {
		t.Fatal("path-like principal must be rejected")
	}
}

func TestRealtimeUsageOutboxRejectsOversizedBody(t *testing.T) {
	outbox := newRealtimeUsageOutbox(t.TempDir())
	body := make([]byte, realtimeUsageMaxBodyBytes+1)
	if _, err := outbox.enqueue(body, testRealtimePrincipalA); err == nil {
		t.Fatal("oversized body must be rejected")
	}
}
