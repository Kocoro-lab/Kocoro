// Package e2e contains end-to-end tests for ShanClaw.
//
// Offline tests (no LLM API needed) run by default.
// Live tests require SHANNON_E2E_LIVE=1 and a configured endpoint+API key.
//
// TestMain builds a fresh shan binary from the current checkout into a temp
// directory. All tests that need the binary use testBinary() to get its path.
package e2e

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sync"
	"testing"
	"time"
)

var builtBinary string

func TestMain(m *testing.M) {
	// Build shan from current source into a temp dir.
	tmp, err := os.MkdirTemp("", "shan-e2e-*")
	if err != nil {
		panic("e2e: failed to create temp dir: " + err.Error())
	}

	bin := filepath.Join(tmp, "shan")
	cmd := exec.Command("go", "build", "-o", bin, ".")
	cmd.Dir = filepath.Join(repoRoot())
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		panic("e2e: failed to build shan: " + err.Error())
	}
	builtBinary = bin

	code := m.Run()
	os.RemoveAll(tmp)
	os.Exit(code)
}

func testBinary(t *testing.T) string {
	t.Helper()
	if builtBinary == "" {
		t.Fatal("shan binary not built — TestMain should have built it")
	}
	return builtBinary
}

// skipUnlessLive gates the live E2E suite on whether this machine can actually
// reach a Cloud endpoint — not on an opt-in environment variable.
//
// The gate used to require SHANNON_E2E_LIVE=1, which meant the default local
// `go test ./...` never exercised a single real model call. Prompt and tool
// changes were therefore validated only by string assertions, and a measured
// behavior regression (mid-run progress notes dropping from 67% to 17% of runs)
// passed every test in the repository. Inverting the gate costs a developer
// with a configured endpoint nothing and gives CI the same silence it had
// before, because CI has no endpoint to find.
//
// SHANNON_E2E_LIVE=1 still forces the suite on without probing (unchanged
// semantics for release runs); =0 forces it off for a fast local iteration.
func skipUnlessLive(t *testing.T) {
	t.Helper()
	if reason := liveGateReason(); reason != "" {
		t.Skip(reason)
	}
}

var (
	liveGateOnce   sync.Once
	liveGateResult string
)

func liveGateReason() string {
	switch os.Getenv("SHANNON_E2E_LIVE") {
	case "1":
		return ""
	case "0":
		return "SHANNON_E2E_LIVE=0 disables live E2E tests"
	}
	liveGateOnce.Do(func() { liveGateResult = probeLiveEndpoint() })
	return liveGateResult
}

// probeLiveEndpoint reads the endpoint and key straight off disk and opens one
// TCP connection. It deliberately never touches the OS credential store: a
// keychain read from a freshly built test binary can raise a blocking
// authorization dialog, which would turn `go test ./...` into something that
// hangs waiting for a click.
func probeLiveEndpoint() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "live E2E skipped: cannot resolve home directory"
	}
	data, err := os.ReadFile(filepath.Join(home, ".shannon", "config.yaml"))
	if err != nil {
		return "live E2E skipped: no ~/.shannon/config.yaml (set SHANNON_E2E_LIVE=1 to force)"
	}
	endpoint := firstYAMLScalar(data, `(?m)^\s*endpoint:\s*"?([^"\s]+)"?\s*$`)
	apiKey := firstYAMLScalar(data, `(?m)^api_key:\s*"?([^"\s]+)"?\s*$`)
	if endpoint == "" || apiKey == "" {
		return "live E2E skipped: no endpoint/api_key configured (set SHANNON_E2E_LIVE=1 to force)"
	}
	parsed, err := url.Parse(endpoint)
	if err != nil || parsed.Host == "" {
		return "live E2E skipped: unparseable endpoint in config.yaml"
	}
	host := parsed.Host
	if parsed.Port() == "" {
		if parsed.Scheme == "https" {
			host = net.JoinHostPort(host, "443")
		} else {
			host = net.JoinHostPort(host, "80")
		}
	}
	conn, err := net.DialTimeout("tcp", host, 2*time.Second)
	if err != nil {
		// A configured-but-unreachable endpoint is the common local case (the
		// Cloud stack is not running). Skipping with the reason beats failing
		// every live test with a connection error.
		return fmt.Sprintf("live E2E skipped: %s is not reachable (%v)", host, err)
	}
	_ = conn.Close()
	return ""
}

func firstYAMLScalar(data []byte, pattern string) string {
	if m := regexp.MustCompile(pattern).FindSubmatch(data); m != nil {
		return string(m[1])
	}
	return ""
}

func repoRoot() string {
	// test/e2e/ is two levels deep from repo root
	dir, _ := os.Getwd()
	return filepath.Join(dir, "..", "..")
}
