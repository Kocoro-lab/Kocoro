package e2e

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	daemonpkg "github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/images"
)

// Live E2E tests require SHANNON_E2E_LIVE=1.
// They make real LLM API calls and cost real tokens.
//
// Daemon tests use a temporary state directory and random localhost port. The
// isolated start mode suppresses Cloud WS, watchers, schedulers, MCP connects,
// and process-global browser cleanup while preserving the real HTTP/agent path.

func TestLive_OneShot_BasicQuery(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)

	out := runShan(t, bin, "what is 2+1")
	if !strings.Contains(out, "3") {
		t.Errorf("expected answer containing '3', got: %s", out)
	}
	// Should use Anthropic model, not GPT fallback
	if strings.Contains(out, "gpt-5-mini") {
		t.Error("should not fall back to gpt-5-mini — check cache_break fix")
	}
}

func TestLive_OneShot_AutoApproveToolUse(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)

	out := runShan(t, bin, "-y", "list files in the current directory")
	if !strings.Contains(out, "directory_list") && !strings.Contains(out, "bash") {
		t.Error("expected tool call (directory_list or bash)")
	}
}

func TestLive_OneShot_SessionCWD(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)

	tmpDir := t.TempDir()
	cmd := exec.Command(bin, "-y", "run pwd")
	cmd.Dir = tmpDir
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("shan failed: %v\n%s", err, stdout.String())
	}

	// Compare against the actual directory we set, resolving symlinks
	// (macOS: /tmp → /private/tmp, /var → /private/var)
	expected, _ := filepath.EvalSymlinks(tmpDir)
	out := stdout.String()
	if !strings.Contains(out, expected) && !strings.Contains(out, tmpDir) {
		t.Errorf("expected CWD %q or %q in output, got: %s", expected, tmpDir, out)
	}
}

func TestLive_BundledAgent_Explorer(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)

	out := runShan(t, bin, "--agent", "explorer", "what files are in this project")
	// Explorer should use read-only tools
	if strings.Contains(out, "file_write") || strings.Contains(out, "file_edit") {
		t.Error("explorer should not use write tools")
	}
}

func TestLive_BundledAgent_Reviewer(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)

	out := runShan(t, bin, "--agent", "reviewer", "review main.go")
	// The OnText / OnPreamble split (a398ecd, v0.1.3) stopped surfacing tool-call
	// names in one-shot stdout — only assistant text reaches the user. Assert the
	// reviewer actually engaged with the file by checking it cited something only
	// readable from the source: cmd.Execute() (the only symbol main.go references).
	// A length floor guards against trivially-short refusals or stub responses.
	if len(out) < 200 {
		t.Errorf("reviewer output too short (%d bytes); likely did not engage with file: %s", len(out), out)
	}
	if !strings.Contains(out, "cmd.Execute") && !strings.Contains(out, "main.go") && !strings.Contains(out, "package main") {
		t.Errorf("reviewer output lacks evidence of having read main.go (no mention of cmd.Execute / main.go / package main): %s", out)
	}
}

func TestLive_Daemon_MessageAndEditRetry(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)
	daemon := startIsolatedLiveDaemon(t, bin)

	// Send message
	resp := httpPost(t, daemon.baseURL+"/message", map[string]interface{}{
		"text": "what is 7+7",
	})
	sessionID, ok := resp["session_id"].(string)
	if !ok || sessionID == "" {
		t.Fatalf("no session_id in response: %v", resp)
	}
	reply, _ := resp["reply"].(string)
	if !strings.Contains(reply, "14") {
		t.Errorf("expected 14 in reply, got: %s", reply)
	}

	// GET session
	sessResp := httpGet(t, fmt.Sprintf("%s/sessions/%s", daemon.baseURL, sessionID))
	messages, ok := sessResp["messages"].([]interface{})
	if !ok || len(messages) < 2 {
		t.Fatalf("expected at least 2 messages, got: %v", sessResp)
	}

	// Edit & retry
	editResp := httpPost(t, fmt.Sprintf("%s/sessions/%s/edit", daemon.baseURL, sessionID), map[string]interface{}{
		"message_index": 0,
		"new_content":   "what is 9+9",
	})
	editReply, _ := editResp["reply"].(string)
	if !strings.Contains(editReply, "18") {
		t.Errorf("expected 18 in edit reply, got: %s", editReply)
	}

	// Verify truncation
	sessResp2 := httpGet(t, fmt.Sprintf("%s/sessions/%s", daemon.baseURL, sessionID))
	messages2, _ := sessResp2["messages"].([]interface{})
	if len(messages2) != 2 {
		t.Errorf("expected 2 messages after edit, got %d", len(messages2))
	}
}

func TestLive_Daemon_AgentListIncludesBuiltins(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)
	daemon := startIsolatedLiveDaemon(t, bin)

	resp := httpGet(t, daemon.baseURL+"/agents")
	agentsList, ok := resp["agents"].([]interface{})
	if !ok {
		t.Fatalf("expected agents array: %v", resp)
	}

	builtins := map[string]bool{}
	for _, a := range agentsList {
		m, _ := a.(map[string]interface{})
		if b, _ := m["builtin"].(bool); b {
			builtins[m["name"].(string)] = true
		}
	}
	for _, name := range []string{"explorer", "reviewer"} {
		if !builtins[name] {
			t.Errorf("expected builtin agent %q", name)
		}
	}
}

// ---------- helpers ----------

func runShan(t *testing.T, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("shan %v failed: %v\n%s", args, err, stdout.String())
	}
	return stdout.String()
}

type isolatedLiveDaemon struct {
	baseURL string
	cmd     *exec.Cmd
	done    chan error
	output  synchronizedBuffer
}

type synchronizedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (b *synchronizedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

func (b *synchronizedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

func startIsolatedLiveDaemon(t *testing.T, bin string) *isolatedLiveDaemon {
	t.Helper()
	port := 0
	for port == 0 || port == 7533 {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("reserve isolated daemon port: %v", err)
		}
		port = listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			t.Fatalf("release isolated daemon port: %v", err)
		}
	}

	stateDir := t.TempDir()
	daemon := &isolatedLiveDaemon{baseURL: fmt.Sprintf("http://127.0.0.1:%d", port)}
	cmd := exec.Command(
		bin, "daemon", "start", "--isolated",
		"--state-dir", stateDir,
		"--port", strconv.Itoa(port),
	)
	cmd.Stdout = &daemon.output
	cmd.Stderr = &daemon.output
	if err := cmd.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	daemon.cmd = cmd
	daemon.done = make(chan error, 1)
	go func() { daemon.done <- cmd.Wait() }()
	t.Cleanup(func() { daemon.stop(t) })
	waitForDaemon(t, daemon, 15*time.Second)
	waitForIsolationMarkers(t, daemon, 3*time.Second)
	return daemon
}

func (d *isolatedLiveDaemon) stop(t *testing.T) {
	t.Helper()
	if d == nil || d.cmd == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	request, _ := http.NewRequestWithContext(ctx, http.MethodPost, d.baseURL+"/shutdown", nil)
	response, err := http.DefaultClient.Do(request)
	if err == nil {
		response.Body.Close()
	}
	cancel()
	select {
	case <-d.done:
	case <-time.After(5 * time.Second):
		if err := d.cmd.Process.Kill(); err != nil {
			t.Errorf("kill isolated daemon pid=%d: %v", d.cmd.Process.Pid, err)
		} else {
			<-d.done
		}
	}
	d.cmd = nil
}

func waitForDaemon(t *testing.T, daemon *isolatedLiveDaemon, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		select {
		case err := <-daemon.done:
			daemon.cmd = nil
			t.Fatalf("isolated daemon exited before readiness: %v\n%s", err, daemon.output.String())
		default:
		}
		resp, err := http.Get(daemon.baseURL + "/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("isolated daemon did not become ready within timeout\n%s", daemon.output.String())
}

func waitForIsolationMarkers(t *testing.T, daemon *isolatedLiveDaemon, timeout time.Duration) {
	t.Helper()
	want := []string{
		daemonpkg.IsolationMarkerMCPDisabled,
		daemonpkg.IsolationMarkerAutomationDisabled,
		daemonpkg.IsolationMarkerCloudWSSuppressed,
		daemonpkg.IsolationMarkerBackgroundDisabled,
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		log := daemon.output.String()
		complete := true
		for _, marker := range want {
			if !strings.Contains(log, marker) {
				complete = false
				break
			}
		}
		if complete {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("isolated daemon did not confirm contained startup\n%s", daemon.output.String())
}

// TestLive_GenerateAndEditImage exercises the v0.1.4 generate_image +
// edit_image pipeline against the configured Shannon Cloud endpoint. It
// bypasses the agent loop's per-call approval gate (which legitimately blocks
// image tools under -y) and calls the production HTTP client directly,
// validating the wire contract end-to-end. This is a paid live test; current
// cost expectations are maintained in the private QA process.
func TestLive_GenerateAndEditImage(t *testing.T) {
	skipUnlessLive(t)

	endpoint, apiKey := readCloudConfig(t)
	if endpoint == "" || apiKey == "" {
		t.Skip("cloud.endpoint / cloud.api_key not configured in ~/.shannon/config.yaml")
	}
	client := images.NewClient(endpoint, apiKey, &http.Client{Timeout: 180 * time.Second})

	// Generate a tiny low-quality image to keep cost minimal.
	genCtx, genCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer genCancel()
	gen, err := client.Generate(genCtx, images.GenerateRequest{
		Prompt:  "a red circle on white background, simple flat design",
		Size:    "1024x1024",
		Quality: "low",
		N:       1,
	})
	if err != nil {
		// A 404 means this gateway has no image endpoint deployed — an
		// environment state (e.g. stale local Cloud image), not a code
		// regression in this repo. Every other error stays fatal.
		if errors.Is(err, images.ErrEndpointNotFound) {
			t.Skipf("image endpoint not deployed at this gateway: %v", err)
		}
		t.Fatalf("Generate: %v", err)
	}
	if len(gen.Images) != 1 {
		t.Fatalf("Generate: expected 1 image, got %d", len(gen.Images))
	}
	if !strings.HasPrefix(gen.Images[0].URL, "https://static.kocoro.ai/") {
		t.Errorf("Generate: expected https://static.kocoro.ai/ URL, got: %s", gen.Images[0].URL)
	}
	if gen.Model != "gpt-image-2" {
		t.Errorf("Generate: expected gpt-image-2 model, got: %s", gen.Model)
	}
	if gen.Images[0].SizeBytes <= 0 {
		t.Errorf("Generate: expected positive size_bytes, got: %d", gen.Images[0].SizeBytes)
	}

	// Edit using the URL we just generated — round-trips the CDN allowlist.
	editCtx, editCancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer editCancel()
	edit, err := client.Edit(editCtx, images.EditRequest{
		Prompt:    "add a small blue square in the bottom-right corner",
		ImageURLs: []string{gen.Images[0].URL},
		Size:      "1024x1024",
		Quality:   "low",
		N:         1,
	})
	if err != nil {
		t.Fatalf("Edit: %v", err)
	}
	if len(edit.Images) != 1 {
		t.Fatalf("Edit: expected 1 image, got %d", len(edit.Images))
	}
	if !strings.HasPrefix(edit.Images[0].URL, "https://static.kocoro.ai/") {
		t.Errorf("Edit: expected https://static.kocoro.ai/ URL, got: %s", edit.Images[0].URL)
	}
	if edit.Images[0].URL == gen.Images[0].URL {
		t.Errorf("Edit returned identical URL to source — server may have shortcut without editing")
	}
}

// readCloudConfig pulls cloud.endpoint and cloud.api_key from the user's
// ~/.shannon/config.yaml without depending on internal/config (which would
// pull in the entire package graph). The format is stable enough for a
// targeted regex match here.
func readCloudConfig(t *testing.T) (endpoint, apiKey string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("UserHomeDir: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(home, ".shannon", "config.yaml"))
	if err != nil {
		t.Skipf("~/.shannon/config.yaml: %v", err)
	}
	endpointRe := regexp.MustCompile(`(?m)^\s*endpoint:\s*"?([^"\s]+)"?\s*$`)
	apiKeyRe := regexp.MustCompile(`(?m)^\s*api_key:\s*"?([^"\s]+)"?\s*$`)
	if m := endpointRe.FindSubmatch(data); m != nil {
		endpoint = string(m[1])
	}
	if m := apiKeyRe.FindSubmatch(data); m != nil {
		apiKey = string(m[1])
	}
	return endpoint, apiKey
}

func httpGet(t *testing.T, url string) map[string]interface{} {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode GET %s: %v", url, err)
	}
	return result
}

func httpPost(t *testing.T, url string, body map[string]interface{}) map[string]interface{} {
	t.Helper()
	b, _ := json.Marshal(body)
	resp, err := http.Post(url, "application/json", bytes.NewReader(b))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		t.Fatalf("decode POST %s: %v", url, err)
	}
	return result
}
