package e2e

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Live E2E tests require SHANNON_E2E_LIVE=1.
// They make real LLM API calls and cost real tokens.
//
// Known limitation: daemon tests use the real ~/.shannon home and port 7533.
// Do not run while a real daemon is active. Future improvement: temp HOME +
// isolated port via env var override.

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

	// Both sides need symlink resolution for macOS where /var → /private/var.
	// The shan output contains the raw pwd result (unresolved), so check
	// against both the resolved and unresolved forms.
	resolved, _ := filepath.EvalSymlinks(tmpDir)
	out := stdout.String()
	if !strings.Contains(out, resolved) && !strings.Contains(out, tmpDir) {
		t.Errorf("expected CWD %q or %q in output, got: %s", resolved, tmpDir, out)
	}
}

func TestLive_BundledAgent_Scout(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)

	out := runShan(t, bin, "--agent", "scout", "what files are in this project")
	// Scout should use read-only tools
	if strings.Contains(out, "file_write") || strings.Contains(out, "file_edit") {
		t.Error("scout should not use write tools")
	}
}

func TestLive_BundledAgent_Checker(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)

	out := runShan(t, bin, "--agent", "checker", "review main.go")
	// Checker may use any read tool (file_read, grep, glob, bash with cat/head).
	// Verify it used at least one tool — progress lines show "⏵ <tool>(".
	usedTool := strings.Contains(out, "file_read") ||
		strings.Contains(out, "grep") ||
		strings.Contains(out, "glob") ||
		strings.Contains(out, "bash")
	if !usedTool {
		t.Errorf("checker should use at least one read tool, got: %s", out)
	}
}

func TestLive_Daemon_MessageAndEditRetry(t *testing.T) {
	skipUnlessLive(t)
	t.Skip("daemon tests use real ~/.shannon and port 7533 — skipped until daemon supports --port/--home isolation")
	bin := testBinary(t)

	// Start daemon
	daemonCmd := exec.Command(bin, "daemon", "start")
	daemonCmd.Stdout = os.Stderr
	daemonCmd.Stderr = os.Stderr
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	defer func() {
		exec.Command(bin, "daemon", "stop").Run()
		daemonCmd.Wait()
	}()

	// Wait for daemon to be ready
	waitForDaemon(t, 10*time.Second)

	// Send message
	resp := httpPost(t, "http://localhost:7533/message", map[string]interface{}{
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
	sessResp := httpGet(t, fmt.Sprintf("http://localhost:7533/sessions/%s", sessionID))
	messages, ok := sessResp["messages"].([]interface{})
	if !ok || len(messages) < 2 {
		t.Fatalf("expected at least 2 messages, got: %v", sessResp)
	}

	// Edit & retry
	editResp := httpPost(t, fmt.Sprintf("http://localhost:7533/sessions/%s/edit", sessionID), map[string]interface{}{
		"message_index": 0,
		"new_content":   "what is 9+9",
	})
	editReply, _ := editResp["reply"].(string)
	if !strings.Contains(editReply, "18") {
		t.Errorf("expected 18 in edit reply, got: %s", editReply)
	}

	// Verify truncation
	sessResp2 := httpGet(t, fmt.Sprintf("http://localhost:7533/sessions/%s", sessionID))
	messages2, _ := sessResp2["messages"].([]interface{})
	if len(messages2) != 2 {
		t.Errorf("expected 2 messages after edit, got %d", len(messages2))
	}
}

func TestLive_Daemon_AgentListIncludesBuiltins(t *testing.T) {
	skipUnlessLive(t)
	t.Skip("daemon tests use real ~/.shannon and port 7533 — skipped until daemon supports --port/--home isolation")
	bin := testBinary(t)

	daemonCmd := exec.Command(bin, "daemon", "start")
	daemonCmd.Stdout = os.Stderr
	daemonCmd.Stderr = os.Stderr
	if err := daemonCmd.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	defer func() {
		exec.Command(bin, "daemon", "stop").Run()
		daemonCmd.Wait()
	}()

	waitForDaemon(t, 10*time.Second)

	resp := httpGet(t, "http://localhost:7533/agents")
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
	for _, name := range []string{"scout", "checker"} {
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

func waitForDaemon(t *testing.T, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := http.Get("http://localhost:7533/health")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == 200 {
				return
			}
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("daemon did not become ready within timeout")
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
