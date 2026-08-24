//go:build darwin && cgo

package cmd

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/koe"
)

func TestRealtimeUsageRelayBoundsConcurrency(t *testing.T) {
	var active atomic.Int32
	var maximum atomic.Int32
	started := make(chan struct{}, 32)
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		current := active.Add(1)
		for {
			old := maximum.Load()
			if current <= old || maximum.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		<-release
		active.Add(-1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", t.TempDir())
	for i := 0; i < 32; i++ {
		body := []byte(fmt.Sprintf(`{"provider":"openai","response_id":"bounded-%d","usage":{"input_tokens":1}}`, i))
		if err := relay.Enqueue("", body); err != nil {
			t.Fatalf("enqueue report %d: %v", i, err)
		}
	}
	for i := 0; i < realtimeUsageRelayConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("relay worker did not start")
		}
	}
	select {
	case <-started:
		t.Fatal("relay exceeded its concurrency bound")
	case <-time.After(100 * time.Millisecond):
	}
	if got := maximum.Load(); got > realtimeUsageRelayConcurrency {
		t.Fatalf("maximum concurrent handoffs = %d, want <= %d", got, realtimeUsageRelayConcurrency)
	}

	close(release)
	relay.Close()
}

func TestRealtimeUsageRelayCloseDrainsAcceptedReport(t *testing.T) {
	started := make(chan struct{})
	release := make(chan struct{})
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		w.WriteHeader(http.StatusOK)
		close(served)
	}))
	defer srv.Close()

	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", t.TempDir())
	if err := relay.Enqueue("", []byte(`{"provider":"openai","response_id":"drain","usage":{"input_tokens":1}}`)); err != nil {
		t.Fatalf("enqueue report: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("relay did not start handoff")
	}

	closed := make(chan struct{})
	go func() {
		relay.Close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("Close returned before the accepted report was served")
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("Close did not finish after the handoff completed")
	}
	select {
	case <-served:
	case <-time.After(time.Second):
		t.Fatal("accepted report was not served before shutdown drain completed")
	}
}

func TestRealtimeUsageRelayReportsQueueSaturationWithoutDropping(t *testing.T) {
	const totalReports = 128
	started := make(chan struct{}, realtimeUsageRelayConcurrency)
	release := make(chan struct{})
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		select {
		case started <- struct{}{}:
		default:
		}
		<-release
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", t.TempDir())
	for i := 0; i < totalReports; i++ {
		body := []byte(fmt.Sprintf(`{"provider":"openai","response_id":"saturation-%d","usage":{"input_tokens":1}}`, i))
		if err := relay.Enqueue("", body); err != nil {
			t.Fatalf("enqueue report %d: %v", i, err)
		}
	}
	for i := 0; i < realtimeUsageRelayConcurrency; i++ {
		select {
		case <-started:
		case <-time.After(time.Second):
			t.Fatal("relay worker did not reach the blocking server")
		}
	}
	close(release)
	relay.Close()
	if got := served.Load(); got != totalReports {
		t.Fatalf("daemon received %d of %d enqueued reports", got, totalReports)
	}
}

func TestRealtimeUsageRelayRetriesAfterTemporaryFailure(t *testing.T) {
	t.Setenv("KOE_USAGE_RELAY_MAX_ATTEMPTS", "1")
	t.Setenv("KOE_USAGE_RELAY_BACKOFF_MS", "1")
	var attempts atomic.Int32
	var succeeded atomic.Int32
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if attempts.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		succeeded.Add(1)
		w.WriteHeader(http.StatusOK)
		select {
		case <-served:
		default:
			close(served)
		}
	}))
	defer srv.Close()

	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", t.TempDir())
	if err := relay.Enqueue("", []byte(`{"provider":"openai","response_id":"retry","usage":{"input_tokens":1}}`)); err != nil {
		t.Fatalf("enqueue report: %v", err)
	}
	select {
	case <-served:
	case <-time.After(3 * time.Second):
		t.Fatal("relay did not deliver after a temporary failure")
	}
	relay.Close()
	if got := succeeded.Load(); got != 1 {
		t.Fatalf("successful deliveries = %d, want 1", got)
	}
	if files, err := os.ReadDir(relay.spoolDir); err != nil {
		t.Fatalf("read spool: %v", err)
	} else {
		for _, file := range files {
			if strings.HasPrefix(file.Name(), "usage-") {
				t.Fatalf("delivered report remains in spool: %s", file.Name())
			}
		}
	}
}

func TestRealtimeUsageRelayClosePreservesOutageForRestart(t *testing.T) {
	t.Setenv("KOE_USAGE_RELAY_MAX_ATTEMPTS", "1")
	t.Setenv("KOE_USAGE_RELAY_BACKOFF_MS", "1")
	spoolDir := t.TempDir()
	firstAttempt := make(chan struct{})
	var firstCount atomic.Int32
	srv1 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		firstCount.Add(1)
		select {
		case <-firstAttempt:
		default:
			close(firstAttempt)
		}
		w.WriteHeader(http.StatusServiceUnavailable)
	}))

	relay1 := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv1.URL), "", spoolDir)
	body := []byte(`{"provider":"openai","response_id":"restart","usage":{"input_tokens":1}}`)
	if err := relay1.Enqueue("", body); err != nil {
		t.Fatalf("enqueue report: %v", err)
	}
	select {
	case <-firstAttempt:
	case <-time.After(time.Second):
		t.Fatal("relay did not attempt the outage request")
	}
	relay1.Close()
	srv1.Close()

	var secondCount atomic.Int32
	secondServed := make(chan struct{})
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCount.Add(1)
		w.WriteHeader(http.StatusOK)
		select {
		case <-secondServed:
		default:
			close(secondServed)
		}
	}))
	defer srv2.Close()
	relay2 := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv2.URL), "", spoolDir)
	select {
	case <-secondServed:
	case <-time.After(3 * time.Second):
		t.Fatal("restart relay did not replay the preserved report")
	}
	relay2.Close()
	if firstCount.Load() < 1 || secondCount.Load() != 1 {
		t.Fatalf("attempts before/after restart = %d/%d, want at least 1/1", firstCount.Load(), secondCount.Load())
	}
	if files, err := os.ReadDir(spoolDir); err != nil {
		t.Fatalf("read spool: %v", err)
	} else {
		for _, file := range files {
			if strings.HasPrefix(file.Name(), "usage-") || strings.HasPrefix(file.Name(), ".pending-") {
				t.Fatalf("replayed report remains in spool: %s", file.Name())
			}
		}
	}
}

func TestRealtimeUsageRelayRecoversCompletePendingFile(t *testing.T) {
	spoolDir := t.TempDir()
	body := json.RawMessage(`{"provider":"openai","response_id":"pending-recovery","usage":{"input_tokens":1}}`)
	envelope, err := json.Marshal(realtimeUsageRelaySpoolEnvelope{Usage: body})
	if err != nil {
		t.Fatalf("marshal pending envelope: %v", err)
	}
	pendingPath := filepath.Join(spoolDir, ".pending-crash")
	if err := os.WriteFile(pendingPath, envelope, 0o600); err != nil {
		t.Fatalf("write pending envelope: %v", err)
	}
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		close(served)
	}))
	defer srv.Close()

	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", spoolDir)
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("complete pending file was not recovered")
	}
	relay.Close()
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("pending file still exists after recovery: %v", err)
	}
}

func TestRealtimeUsageRelayQuarantinesInvalidPendingFile(t *testing.T) {
	spoolDir := t.TempDir()
	pendingPath := filepath.Join(spoolDir, ".pending-partial")
	if err := os.WriteFile(pendingPath, []byte(`{"principal":"broken"`), 0o600); err != nil {
		t.Fatalf("write invalid pending file: %v", err)
	}
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		close(served)
	}))
	defer srv.Close()

	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", spoolDir)
	if err := relay.Enqueue("", []byte(`{"provider":"openai","response_id":"after-invalid","usage":{"input_tokens":1}}`)); err != nil {
		t.Fatalf("enqueue report after invalid pending: %v", err)
	}
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("valid report was blocked by invalid pending file")
	}
	relay.Close()
	if _, err := os.Stat(pendingPath); !os.IsNotExist(err) {
		t.Fatalf("invalid pending file was not isolated: %v", err)
	}
	if _, err := os.Stat(pendingPath + ".invalid"); err != nil {
		t.Fatalf("quarantined pending file missing: %v", err)
	}
}

func TestRealtimeUsageRelayDeduplicatesResponseID(t *testing.T) {
	var served atomic.Int32
	started := make(chan struct{})
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		close(started)
		<-release
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", t.TempDir())
	body1 := []byte(`{"provider":"openai","response_id":"same-response","usage":{"input_tokens":1}}`)
	body2 := []byte(`{"provider":"openai","response_id":"same-response","usage":{"input_tokens":999}}`)
	if err := relay.Enqueue("", body1); err != nil {
		t.Fatalf("enqueue first report: %v", err)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("relay did not start the first report")
	}
	if err := relay.Enqueue("", body2); err != nil {
		t.Fatalf("enqueue duplicate report: %v", err)
	}
	close(release)
	relay.Close()
	if got := served.Load(); got != 1 {
		t.Fatalf("daemon received %d reports for one response ID, want 1", got)
	}
}

func TestRealtimeUsageRelayIdentityIncludesProviderAndPrincipal(t *testing.T) {
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", t.TempDir())
	bodyOpenAI := []byte(`{"provider":"openai","response_id":"same-provider-id","usage":{"input_tokens":1}}`)
	bodyQwen := []byte(`{"provider":"qwen","response_id":"same-provider-id","usage":{"input_tokens":2}}`)
	if err := relay.Enqueue("principal-a", bodyOpenAI); err != nil {
		t.Fatalf("enqueue OpenAI report: %v", err)
	}
	if err := relay.Enqueue("principal-b", bodyQwen); err != nil {
		t.Fatalf("enqueue Qwen report: %v", err)
	}
	relay.Close()
	if got := served.Load(); got != 2 {
		t.Fatalf("provider/principal collision dropped %d of 2 reports", 2-got)
	}
}

func TestRealtimeUsageRelayMigratesLegacyResponseIDSpool(t *testing.T) {
	spoolDir := t.TempDir()
	body := json.RawMessage(`{"provider":"openai","response_id":"legacy-canonical","usage":{"input_tokens":1}}`)
	envelope, err := json.Marshal(realtimeUsageRelaySpoolEnvelope{Principal: "principal-a", Usage: body})
	if err != nil {
		t.Fatalf("marshal legacy envelope: %v", err)
	}
	legacyPath := realtimeUsageRelayLegacyTarget(spoolDir, "legacy-canonical")
	if err := os.WriteFile(legacyPath, envelope, 0o600); err != nil {
		t.Fatalf("write legacy spool: %v", err)
	}
	served := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		close(served)
	}))
	defer srv.Close()

	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", spoolDir)
	select {
	case <-served:
	case <-time.After(2 * time.Second):
		t.Fatal("legacy spool was not replayed")
	}
	relay.Close()
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("legacy spool path remains after migration: %v", err)
	}
}

func TestRealtimeUsageRelayLegacyCleanupOnlyRemovesMatchingCanonical(t *testing.T) {
	spoolDir := t.TempDir()
	principal := "principal-a"
	body := json.RawMessage(`{"provider":"openai","response_id":"legacy-cleanup","usage":{"input_tokens":1}}`)
	envelope, err := json.Marshal(realtimeUsageRelaySpoolEnvelope{Principal: principal, Usage: body})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	legacyPath := realtimeUsageRelayLegacyTarget(spoolDir, "legacy-cleanup")
	canonicalPath := realtimeUsageRelayTarget(spoolDir, principal, "openai", "legacy-cleanup")
	canonical := []byte(`{"principal":"principal-a","usage":{"provider":"openai","response_id":"legacy-cleanup","usage":{"input_tokens":99}}}`)
	if err := os.WriteFile(legacyPath, envelope, 0o600); err != nil {
		t.Fatalf("write legacy spool: %v", err)
	}
	if err := os.WriteFile(canonicalPath, canonical, 0o600); err != nil {
		t.Fatalf("write canonical spool: %v", err)
	}
	path, created, err := persistRealtimeUsageRelaySpool(spoolDir, principal, body)
	if err != nil || created || path != canonicalPath {
		t.Fatalf("persist duplicate = path=%q created=%t err=%v; want existing canonical", path, created, err)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("matching legacy spool remains: %v", err)
	}
	got, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical spool: %v", err)
	}
	if string(got) != string(canonical) {
		t.Fatalf("canonical spool changed: got %s, want %s", got, canonical)
	}
}

func TestRealtimeUsageRelayLegacyCleanupRejectsMismatchedCanonical(t *testing.T) {
	spoolDir := t.TempDir()
	principal := "principal-a"
	body := json.RawMessage(`{"provider":"openai","response_id":"legacy-mismatch","usage":{"input_tokens":1}}`)
	envelope, err := json.Marshal(realtimeUsageRelaySpoolEnvelope{Principal: principal, Usage: body})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	legacyPath := realtimeUsageRelayLegacyTarget(spoolDir, "legacy-mismatch")
	canonicalPath := realtimeUsageRelayTarget(spoolDir, principal, "openai", "legacy-mismatch")
	canonical := []byte(`{"principal":"principal-a","usage":{"provider":"qwen","response_id":"legacy-mismatch","usage":{"input_tokens":99}}}`)
	if err := os.WriteFile(legacyPath, envelope, 0o600); err != nil {
		t.Fatalf("write legacy spool: %v", err)
	}
	if err := os.WriteFile(canonicalPath, canonical, 0o600); err != nil {
		t.Fatalf("write canonical spool: %v", err)
	}
	if _, _, err := persistRealtimeUsageRelaySpool(spoolDir, principal, body); err == nil {
		t.Fatal("mismatched canonical spool was accepted")
	}
	if _, err := os.Stat(legacyPath); err != nil {
		t.Fatalf("mismatched legacy spool was removed: %v", err)
	}
	got, err := os.ReadFile(canonicalPath)
	if err != nil {
		t.Fatalf("read canonical spool: %v", err)
	}
	if string(got) != string(canonical) {
		t.Fatalf("mismatched canonical spool changed: got %s, want %s", got, canonical)
	}
}

func TestRealtimeUsageRelayStartupDuplicateSendsOnce(t *testing.T) {
	spoolDir := t.TempDir()
	principal := "principal-a"
	body := json.RawMessage(`{"provider":"openai","response_id":"startup-duplicate","usage":{"input_tokens":1}}`)
	envelope, err := json.Marshal(realtimeUsageRelaySpoolEnvelope{Principal: principal, Usage: body})
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	legacyPath := realtimeUsageRelayLegacyTarget(spoolDir, "startup-duplicate")
	canonicalPath := realtimeUsageRelayTarget(spoolDir, principal, "openai", "startup-duplicate")
	if err := os.WriteFile(legacyPath, envelope, 0o600); err != nil {
		t.Fatalf("write legacy spool: %v", err)
	}
	if err := os.WriteFile(canonicalPath, envelope, 0o600); err != nil {
		t.Fatalf("write canonical spool: %v", err)
	}
	var served atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		served.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient(srv.URL), "", spoolDir)
	deadline := time.After(2 * time.Second)
	for served.Load() == 0 {
		select {
		case <-deadline:
			relay.Close()
			t.Fatal("startup duplicate was not replayed")
		default:
			time.Sleep(5 * time.Millisecond)
		}
	}
	relay.Close()
	if got := served.Load(); got != 1 {
		t.Fatalf("startup duplicate delivered %d reports, want 1", got)
	}
	if _, err := os.Stat(legacyPath); !os.IsNotExist(err) {
		t.Fatalf("startup legacy spool remains: %v", err)
	}
}

func TestRealtimeUsageRelayInstallIsNoReplaceConcurrently(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "canonical.json")
	sources := make([]string, 2)
	want := make([][]byte, 2)
	for i := range sources {
		file, err := os.CreateTemp(dir, ".source-*")
		if err != nil {
			t.Fatalf("create source %d: %v", i, err)
		}
		sources[i] = file.Name()
		want[i] = []byte(fmt.Sprintf("source-%d", i))
		if _, err := file.Write(want[i]); err != nil {
			t.Fatalf("write source %d: %v", i, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close source %d: %v", i, err)
		}
	}
	start := make(chan struct{})
	results := make(chan bool, len(sources))
	errs := make(chan error, len(sources))
	var wg sync.WaitGroup
	for _, source := range sources {
		wg.Add(1)
		go func(source string) {
			defer wg.Done()
			<-start
			installed, err := installRealtimeUsageRelayFile(source, target)
			results <- installed
			errs <- err
		}(source)
	}
	close(start)
	wg.Wait()
	close(results)
	close(errs)
	installedCount := 0
	for installed := range results {
		if installed {
			installedCount++
		}
	}
	for err := range errs {
		if err != nil {
			t.Fatalf("concurrent install: %v", err)
		}
	}
	if installedCount != 1 {
		t.Fatalf("concurrent installs = %d, want exactly one", installedCount)
	}
	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("read installed target: %v", err)
	}
	if string(got) != string(want[0]) && string(got) != string(want[1]) {
		t.Fatalf("target content = %q, want one source", got)
	}
}

func TestRealtimeUsageRelaySpoolPermissionsAndOversize(t *testing.T) {
	spoolDir := t.TempDir()
	relay := newRealtimeUsageRelayWithSpool(koe.NewDaemonClient("http://127.0.0.1:1"), "", spoolDir)
	body := json.RawMessage(`{"provider":"openai","response_id":"permissions","usage":{"input_tokens":1}}`)
	path, created, err := persistRealtimeUsageRelaySpool(spoolDir, "", body)
	if err != nil || !created {
		t.Fatalf("persist permissions fixture: path=%q created=%t err=%v", path, created, err)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatalf("stat spool entry: %v", err)
	} else if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("spool entry mode = %o, want 600", got)
	}
	large := `{"provider":"openai","response_id":"oversize","usage":{"input_tokens":1,"padding":"` + strings.Repeat("x", realtimeUsageRelayMaxBodyBytes) + `"}}`
	if err := relay.Enqueue("", []byte(large)); err == nil {
		t.Fatal("oversize report was admitted")
	}
	if info, err := os.Stat(spoolDir); err != nil {
		t.Fatalf("stat spool: %v", err)
	} else if got := info.Mode().Perm(); got != 0o700 {
		t.Fatalf("spool mode = %o, want 700", got)
	}
	relay.Close()
}
