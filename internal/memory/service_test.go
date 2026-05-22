package memory

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

func TestService_Disabled(t *testing.T) {
	s := NewService(Config{Provider: "disabled"}, nil)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Status() != StatusDisabled {
		t.Fatalf("status=%v want StatusDisabled", s.Status())
	}
	_, class, _ := s.Query(context.Background(), QueryIntent{})
	if class != ClassUnavailable {
		t.Fatalf("disabled service Query class=%v want ClassUnavailable", class)
	}
}

func TestService_LocalNoTLM(t *testing.T) {
	captured := []string{}
	a := AuditFunc(func(ev string, _ map[string]any) { captured = append(captured, ev) })
	cfg := Config{Provider: "local", TLMPath: "/definitely/not/a/real/path/for/tlm"}
	s := NewService(cfg, a)
	if err := s.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if s.Status() != StatusUnavailable {
		t.Fatalf("status=%v want StatusUnavailable", s.Status())
	}
	found := false
	for _, e := range captured {
		if e == "memory_tlm_missing" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected memory_tlm_missing audit, got %v", captured)
	}
}

func TestService_CloudMissingAPIKey(t *testing.T) {
	captured := []map[string]any{}
	a := AuditFunc(func(ev string, fields map[string]any) {
		if ev == "memory_cloud_misconfigured" {
			captured = append(captured, fields)
		}
	})
	cfg := Config{Provider: "cloud", Endpoint: "https://x", APIKey: "", TLMPath: "/bin/echo"}
	s := NewService(cfg, a)
	_ = s.Start(context.Background())
	if s.Status() != StatusUnavailable {
		t.Fatalf("status=%v want StatusUnavailable", s.Status())
	}
	if len(captured) == 0 {
		t.Fatal("expected memory_cloud_misconfigured audit")
	}
	f := captured[0]
	if f["endpoint_resolved"] != true {
		t.Fatalf("endpoint_resolved=%v want true", f["endpoint_resolved"])
	}
	if f["api_key_present"] != false {
		t.Fatalf("api_key_present=%v want false", f["api_key_present"])
	}
}

func TestService_CloudMissingEndpoint(t *testing.T) {
	captured := []map[string]any{}
	a := AuditFunc(func(ev string, fields map[string]any) {
		if ev == "memory_cloud_misconfigured" {
			captured = append(captured, fields)
		}
	})
	cfg := Config{Provider: "cloud", Endpoint: "", APIKey: "k", TLMPath: "/bin/echo"}
	s := NewService(cfg, a)
	_ = s.Start(context.Background())
	if s.Status() != StatusUnavailable {
		t.Fatalf("status=%v want StatusUnavailable", s.Status())
	}
	if len(captured) == 0 {
		t.Fatal("expected memory_cloud_misconfigured audit")
	}
	f := captured[0]
	if f["endpoint_resolved"] != false {
		t.Fatalf("endpoint_resolved=%v want false", f["endpoint_resolved"])
	}
	if f["api_key_present"] != true {
		t.Fatalf("api_key_present=%v want true", f["api_key_present"])
	}
}

// writeFakeTLMScriptSvc writes a python3 script that listens on `sock` and
// serves /health = ready. Sidecar tests use a similar helper in
// sidecar_test.go; duplicated here to keep service_test.go self-contained.
// Skips if python3 is unavailable.
func writeFakeTLMScriptSvc(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable; sidecar spawn tests require python3")
	}
	dir, err := os.MkdirTemp("", "tlmsvc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	py := `import sys, os, json, http.server, socketserver
sock_path = sys.argv[sys.argv.index('--socket')+1]
try: os.unlink(sock_path)
except FileNotFoundError: pass
class H(http.server.BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == '/health':
            self.send_response(200); self.send_header('Content-Type','application/json'); self.end_headers()
            self.wfile.write(json.dumps({'ready': True, 'protocol_version': 1}).encode())
    def log_message(self, *args, **kwargs): pass
class UDSServer(socketserver.UnixStreamServer):
    allow_reuse_address = True
srv = UDSServer(sock_path, H)
srv.serve_forever()
`
	path := filepath.Join(dir, "fake_tlm.py")
	if err := os.WriteFile(path, []byte(py), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shortSockForSvc(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "svc")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

func TestService_StartReachesReady(t *testing.T) {
	sock := shortSockForSvc(t, "s")
	root := t.TempDir()
	script := writeFakeTLMScriptSvc(t)
	cfg := Config{
		Provider:             "local",
		TLMPath:              "python3",
		SocketPath:           sock,
		BundleRoot:           root,
		SidecarReadyTimeout:  5 * time.Second,
		SidecarShutdownGrace: 2 * time.Second,
		SidecarRestartMax:    3,
		ClientRequestTimeout: 5 * time.Second,
	}
	s := NewService(cfg, nil)
	s.testExtraSpawnArgs = []string{script}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := s.Start(ctx); err != nil {
		t.Fatal(err)
	}
	// Poll for Ready transition.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if s.Status() == StatusReady {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if s.Status() != StatusReady {
		t.Fatalf("status=%v want StatusReady", s.Status())
	}
	if err := s.Stop(); err != nil {
		t.Fatal(err)
	}
}

func TestService_StatusString(t *testing.T) {
	cases := []struct {
		s    ServiceStatus
		want string
	}{
		{StatusDisabled, "disabled"},
		{StatusInitializing, "initializing"},
		{StatusReady, "ready"},
		{StatusDegraded, "degraded"},
		{StatusUnavailable, "unavailable"},
	}
	for _, tc := range cases {
		if got := tc.s.String(); got != tc.want {
			t.Fatalf("%v.String()=%q want %q", tc.s, got, tc.want)
		}
	}
}

func TestService_MemoryProviderStatus_Disabled(t *testing.T) {
	s := NewService(Config{Provider: "disabled"}, nil)
	_ = s.Start(context.Background())
	ms := s.MemoryProviderStatus()
	if ms.Provider != "disabled" {
		t.Fatalf("provider=%q want disabled", ms.Provider)
	}
	if ms.Reason != nil {
		t.Fatalf("reason=%v want nil", ms.Reason)
	}
}

func TestService_MemoryProviderStatus_Unavailable_BinaryMissing(t *testing.T) {
	cfg := Config{Provider: "local", TLMPath: "/definitely/not/a/real/path"}
	s := NewService(cfg, nil)
	_ = s.Start(context.Background())
	ms := s.MemoryProviderStatus()
	if ms.Provider != "disabled" {
		t.Fatalf("provider=%q want disabled", ms.Provider)
	}
	if ms.Reason == nil || *ms.Reason != "tlm_binary_missing" {
		t.Fatalf("reason=%v want tlm_binary_missing", ms.Reason)
	}
}

func TestService_MemoryProviderStatus_Degraded(t *testing.T) {
	s := NewService(Config{}, nil)
	// Directly inject degraded state as the supervisor goroutine would.
	r := ReasonRepeatedCrash
	s.disabledReason.Store(&r)
	s.restartAttempts.Store(5)
	s.status.Store(int32(StatusDegraded))

	ms := s.MemoryProviderStatus()
	if ms.Provider != "disabled" {
		t.Fatalf("provider=%q want disabled", ms.Provider)
	}
	if ms.Reason == nil || *ms.Reason != "repeated_crash" {
		t.Fatalf("reason=%v want repeated_crash", ms.Reason)
	}
	if ms.Detail == nil {
		t.Fatal("detail must not be nil when degraded")
	}
	if ms.Detail["restart_attempts"] != 5 {
		t.Fatalf("detail.restart_attempts=%v want 5", ms.Detail["restart_attempts"])
	}
}

func TestService_MemoryProviderStatus_Initializing(t *testing.T) {
	s := NewService(Config{}, nil)
	s.status.Store(int32(StatusInitializing))
	ms := s.MemoryProviderStatus()
	if ms.Provider != "enabled" {
		t.Fatalf("provider=%q want enabled", ms.Provider)
	}
	if ms.Reason != nil {
		t.Fatalf("reason=%v want nil when initializing", ms.Reason)
	}
}

func TestService_MemoryProviderStatus_CloudMisconfigured(t *testing.T) {
	cfg := Config{Provider: "cloud", Endpoint: "https://x", APIKey: "", TLMPath: "/bin/echo"}
	s := NewService(cfg, nil)
	_ = s.Start(context.Background())
	ms := s.MemoryProviderStatus()
	if ms.Provider != "disabled" {
		t.Fatalf("provider=%q want disabled", ms.Provider)
	}
	if ms.Reason == nil || *ms.Reason != "cloud_misconfigured" {
		t.Fatalf("reason=%v want cloud_misconfigured", ms.Reason)
	}
}

// TestService_MemoryProviderStatus_BundleSchemaMismatchEmitsRepairNeeded
// asserts that when the supervisor's onDegraded fires with the schema-mismatch
// reason + detail map, MemoryProviderStatus surfaces it as detail.repair_needed.
// Desktop reads this block to drive the on-demand tlm reinstall flow.
func TestService_MemoryProviderStatus_BundleSchemaMismatchEmitsRepairNeeded(t *testing.T) {
	s := &Service{}
	s.status.Store(int32(StatusDegraded))
	s.setDisabledReason(ReasonBundleSchemaMismatch)
	s.restartAttempts.Store(2)
	s.setDisabledDetail(map[string]any{
		"compatibility":  "incompatible",
		"sub_code":       "no_manifest",
		"bundle_version": "",
	})

	ms := s.MemoryProviderStatus()
	if ms.Provider != "disabled" {
		t.Fatalf("provider=%q want disabled", ms.Provider)
	}
	if ms.Reason == nil || *ms.Reason != ReasonBundleSchemaMismatch {
		t.Fatalf("reason=%v want %q", ms.Reason, ReasonBundleSchemaMismatch)
	}
	repair, ok := ms.Detail["repair_needed"].(map[string]any)
	if !ok {
		t.Fatalf("detail.repair_needed missing or wrong type: %+v", ms.Detail)
	}
	if repair["compatibility"] != "incompatible" || repair["sub_code"] != "no_manifest" {
		t.Fatalf("repair=%+v missing expected fields", repair)
	}
	if ms.Detail["restart_attempts"] != 2 {
		t.Fatalf("restart_attempts=%v want 2 (back-compat field preserved)", ms.Detail["restart_attempts"])
	}
}

// TestService_MemoryProviderStatus_NonSchemaReasonHasNoRepairBlock asserts that
// repair_needed is ONLY attached for ReasonBundleSchemaMismatch — generic
// startup_timeout or repeated_crash should NOT carry a stale detail block
// even if disabledDetail is populated (it gets cleared on each fresh
// onDegraded — but defense in depth).
func TestService_MemoryProviderStatus_NonSchemaReasonHasNoRepairBlock(t *testing.T) {
	s := &Service{}
	s.status.Store(int32(StatusDegraded))
	s.setDisabledReason(ReasonStartupTimeout)
	s.setDisabledDetail(map[string]any{
		"compatibility": "unknown",
		"sub_code":      "",
	})

	ms := s.MemoryProviderStatus()
	if _, present := ms.Detail["repair_needed"]; present {
		t.Fatalf("repair_needed should not appear for reason=%v: %+v", *ms.Reason, ms.Detail)
	}
}

// TestCurrentBundleReadable_TableDriven covers the four cases the helper
// must distinguish: no current, missing manifest, corrupt manifest, valid
// manifest.
func TestCurrentBundleReadable_TableDriven(t *testing.T) {
	t.Run("no_current_symlink", func(t *testing.T) {
		root := t.TempDir()
		if currentBundleReadable(root) {
			t.Fatal("want false for empty bundle root")
		}
	})
	t.Run("missing_manifest", func(t *testing.T) {
		root := t.TempDir()
		bundleDir := filepath.Join(root, "bundles", "2026-01-01T00-00-00Z")
		if err := os.MkdirAll(bundleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(bundleDir, filepath.Join(root, "current")); err != nil {
			t.Fatal(err)
		}
		if currentBundleReadable(root) {
			t.Fatal("want false when manifest.json missing")
		}
	})
	t.Run("corrupt_manifest", func(t *testing.T) {
		root := t.TempDir()
		bundleDir := filepath.Join(root, "bundles", "2026-01-01T00-00-00Z")
		if err := os.MkdirAll(bundleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, "manifest.json"), []byte("{not-json"), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(bundleDir, filepath.Join(root, "current")); err != nil {
			t.Fatal(err)
		}
		if currentBundleReadable(root) {
			t.Fatal("want false when manifest.json is unparseable")
		}
	})
	t.Run("valid_manifest", func(t *testing.T) {
		root := t.TempDir()
		bundleDir := filepath.Join(root, "bundles", "2026-01-01T00-00-00Z")
		if err := os.MkdirAll(bundleDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(bundleDir, "manifest.json"),
			[]byte(`{"bundle_version":"0.6.0"}`), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(bundleDir, filepath.Join(root, "current")); err != nil {
			t.Fatal(err)
		}
		if !currentBundleReadable(root) {
			t.Fatal("want true when manifest is valid JSON")
		}
	})
}
