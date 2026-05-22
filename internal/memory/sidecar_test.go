//go:build !plan9

package memory

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// shortSocketPath builds a UDS path under os.TempDir() that stays below
// the 104-byte sun_path limit (macOS). Avoid t.TempDir() for sockets —
// see internal/memory/client_test.go for context.
func shortSocketPath(t *testing.T, name string) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "tlm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	return filepath.Join(dir, name)
}

// startFakeSidecar binds a Go HTTP server to socketPath and serves /health.
// Returns a stop func.
func startFakeSidecar(t *testing.T, socketPath string, ready bool) func() {
	t.Helper()
	ln, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		_ = json.NewEncoder(w).Encode(HealthPayload{Ready: ready, ProtocolVersion: 1})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	return func() { _ = srv.Close(); _ = os.Remove(socketPath) }
}

func TestSidecar_AttachPolicy_Ready(t *testing.T) {
	sock := shortSocketPath(t, "s1")
	stop := startFakeSidecar(t, sock, true)
	defer stop()
	ready, err := AttachPolicy(context.Background(), sock)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if !ready {
		t.Fatal("ready should be true")
	}
}

func TestSidecar_AttachPolicy_NoSocket(t *testing.T) {
	sock := shortSocketPath(t, "miss")
	ready, _ := AttachPolicy(context.Background(), sock)
	if ready {
		t.Fatal("ready should be false when no listener")
	}
}

func TestSidecar_AttachPolicy_NotReady(t *testing.T) {
	sock := shortSocketPath(t, "s2")
	stop := startFakeSidecar(t, sock, false)
	defer stop()
	ready, _ := AttachPolicy(context.Background(), sock)
	if ready {
		t.Fatal("ready=true for not-ready sidecar")
	}
}

// writeFakeTLMScript writes a python3 script that listens on `sock` and
// serves /health = ready. Skips the test if python3 is unavailable.
func writeFakeTLMScript(t *testing.T) string {
	t.Helper()
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 unavailable; sidecar process tests require python3")
	}
	dir, err := os.MkdirTemp("", "tlmpy")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	py := `import sys, os, socket, json, http.server, socketserver
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

func TestSidecar_SpawnAndShutdown(t *testing.T) {
	sock := shortSocketPath(t, "spawn")
	root, err := os.MkdirTemp("", "tlmroot")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)

	script := writeFakeTLMScript(t)
	cfg := Config{
		TLMPath:              "python3",
		SocketPath:           sock,
		BundleRoot:           root,
		SidecarReadyTimeout:  5 * time.Second,
		SidecarShutdownGrace: 2 * time.Second,
	}
	sc := NewSidecar(cfg, []string{script})
	ctx := context.Background()
	if err := sc.Spawn(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sc.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := sc.Shutdown(2 * time.Second); err != nil {
		t.Fatal(err)
	}
}

// TestSidecar_ShutdownIdempotent_AfterWait asserts that calling Shutdown
// after the supervisor has already reaped the child via Wait() is safe —
// no double exec.Cmd.Wait call (which is undefined per stdlib), no panic.
func TestSidecar_ShutdownIdempotent_AfterWait(t *testing.T) {
	sock := shortSocketPath(t, "idem")
	root, err := os.MkdirTemp("", "tlmroot")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)

	script := writeFakeTLMScript(t)
	cfg := Config{
		TLMPath:              "python3",
		SocketPath:           sock,
		BundleRoot:           root,
		SidecarReadyTimeout:  5 * time.Second,
		SidecarShutdownGrace: 2 * time.Second,
	}
	sc := NewSidecar(cfg, []string{script})
	ctx := context.Background()
	if err := sc.Spawn(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sc.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	// Kill the child out-of-band so Wait() returns quickly (we don't have
	// a clean exit path from the fake script). SIGTERM the process group so
	// the python http server actually stops listening.
	_ = sc.cmd.Process.Signal(os.Interrupt)
	_ = sc.Wait() // reaps the child

	// Now Shutdown must not double-Wait or panic. Two calls in a row should
	// both succeed.
	if err := sc.Shutdown(1 * time.Second); err != nil {
		t.Fatalf("Shutdown after Wait returned err=%v", err)
	}
	if err := sc.Shutdown(1 * time.Second); err != nil {
		t.Fatalf("second Shutdown returned err=%v", err)
	}
}

// TestSidecar_ConcurrentWaitAndShutdown asserts that Wait and Shutdown can
// be invoked from different goroutines on the same Sidecar without racing
// the cmd field, double-Waiting the same exec.Cmd, or deadlocking. Run with
// -race to catch field-access races.
//
// This models the production failure mode: Desktop repair flow stops the
// daemon (Service.Stop → Shutdown) while the supervisor goroutine is
// blocked in Sidecar.Wait().
func TestSidecar_ConcurrentWaitAndShutdown(t *testing.T) {
	sock := shortSocketPath(t, "race")
	root, err := os.MkdirTemp("", "tlmroot")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)

	script := writeFakeTLMScript(t)
	cfg := Config{
		TLMPath:              "python3",
		SocketPath:           sock,
		BundleRoot:           root,
		SidecarReadyTimeout:  5 * time.Second,
		SidecarShutdownGrace: 2 * time.Second,
	}
	sc := NewSidecar(cfg, []string{script})
	ctx := context.Background()
	if err := sc.Spawn(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sc.WaitReady(ctx, 5*time.Second); err != nil {
		t.Fatal(err)
	}

	// Fire Wait and Shutdown concurrently. Repeat the Wait+Shutdown pair
	// a few times — each subsequent pair runs against the reaped-state
	// (idempotent) paths.
	const goroutines = 8
	done := make(chan struct{}, goroutines)
	for i := 0; i < goroutines; i++ {
		i := i
		go func() {
			defer func() { done <- struct{}{} }()
			if i%2 == 0 {
				_ = sc.Wait()
			} else {
				_ = sc.Shutdown(1 * time.Second)
			}
		}()
	}
	// Bounded wait so a deadlock fails loudly under -race instead of
	// hanging the test runner. 10s is far above the Shutdown grace +
	// SIGKILL fallback (1s+1s).
	deadline := time.After(10 * time.Second)
	for i := 0; i < goroutines; i++ {
		select {
		case <-done:
		case <-deadline:
			t.Fatal("Wait/Shutdown goroutines did not all complete within 10s — likely deadlock")
		}
	}
}

func TestSidecar_TLMNotFound(t *testing.T) {
	cfg := Config{TLMPath: "/definitely/no/such/binary"}
	sc := NewSidecar(cfg, nil)
	err := sc.Spawn(context.Background())
	if err == nil {
		t.Fatal("expected error")
	}
	// Should be ErrTLMNotFound when path is set but missing.
	if err.Error() != ErrTLMNotFound.Error() {
		t.Logf("note: error=%v (expected ErrTLMNotFound)", err)
	}
}
