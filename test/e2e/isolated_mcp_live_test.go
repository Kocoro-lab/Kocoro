//go:build live

package e2e

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

// TestLive_IsolatedMCPAllowlist_FullPath exercises the real chain:
// current checkout binary -> isolated daemon -> config reload -> real provider
// -> real Playwright MCP subprocess -> real headless browser -> local HTTP page.
// A second configured process would create a marker if either initial startup
// or reload escaped the allowlist.
//
// This is a mechanism qualification, not a natural tool-selection evaluation:
// the prompt names the MCP entry points so a failure isolates allowlist wiring.
// Natural tool choice is covered separately by the content-search live E2E.
func TestLive_IsolatedMCPAllowlist_FullPath(t *testing.T) {
	skipUnlessLive(t)
	if runtime.GOOS != "darwin" {
		t.Skip("Playwright MCP full-path probe currently qualifies the macOS release path")
	}
	playwrightMCP, err := exec.LookPath("playwright-mcp")
	if err != nil {
		t.Fatalf("real E2E requires playwright-mcp on PATH: %v", err)
	}
	if _, err := os.Stat("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"); err != nil {
		t.Fatalf("real E2E requires Google Chrome: %v", err)
	}

	const token = "KOCORO_ISOLATED_MCP_E2E_7F31"
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><title>isolated mcp</title><main><span id="e2e-token">%s</span></main>`, token)
	}))
	defer page.Close()

	marker := filepath.Join(t.TempDir(), "unallowlisted-mcp-started")
	options := isolatedLiveOptions{
		MCPAllowlist: "playwright",
		MCPServers: map[string]mcp.MCPServerConfig{
			"playwright": {
				Command: playwrightMCP,
				Args: []string{
					"--headless",
					"--isolated",
					"--browser", "chrome",
					"--output-mode", "stdout",
				},
				Context:               "Isolated Playwright browser used only for this E2E page.",
				KeepAlive:             true,
				ConnectTimeoutSeconds: 30,
			},
			"must-not-start": {
				Command: "/usr/bin/touch",
				Args:    []string{marker},
			},
		},
	}

	daemon := startIsolatedLiveDaemon(t, testBinary(t), options)
	waitForMCPState(t, daemon, "playwright", "connected", 45*time.Second)
	assertPathAbsent(t, marker, "initial MCP registration escaped the allowlist")

	// Force the real MCP rebuild path, then prove the process-start allowlist is
	// reapplied before registration rather than only mutating the startup config.
	playwrightConfig := options.MCPServers["playwright"]
	playwrightConfig.Context += " Reloaded."
	options.MCPServers["playwright"] = playwrightConfig
	writeIsolatedDaemonConfig(t, daemon.stateDir, daemon.cloudEndpoint, options)
	reload := httpPost(t, daemon.baseURL+"/config/reload", map[string]interface{}{})
	if reload["status"] != "reloaded" {
		t.Fatalf("config reload response = %v", reload)
	}
	waitForMCPState(t, daemon, "playwright", "connected", 45*time.Second)
	assertPathAbsent(t, marker, "MCP config reload escaped the allowlist")

	prompt := fmt.Sprintf(
		"First call tool_search with exactly `select:browser_navigate`. Then call the returned Playwright MCP browser_navigate tool to open %s, read the exact text in #e2e-token from its page snapshot, and return it. Do not call the local `browser` tool, shell, HTTP, or web-search tools.",
		page.URL,
	)
	run := streamMessage(t, daemon.baseURL, map[string]interface{}{"text": prompt, "source": "kocoro"})
	reply, _ := run.Result["reply"].(string)
	if !strings.Contains(reply, token) {
		t.Fatalf("real Playwright reply missing token %q: %q", token, reply)
	}
	tools := sseToolNames(run.Frames)
	usedPlaywright := false
	for _, name := range tools {
		if strings.HasPrefix(name, "browser_") {
			usedPlaywright = true
			break
		}
	}
	if !usedPlaywright {
		t.Fatalf("provider returned the page token without a Playwright MCP call; tools=%v", tools)
	}
	assertPathAbsent(t, marker, "unallowlisted MCP process started during the model run")
	t.Logf("real isolated MCP E2E passed: latency=%s cost=$%.6f tools=%v", run.Duration.Round(time.Millisecond), run.CostUSD, tools)
}

func waitForMCPState(t *testing.T, daemon *isolatedLiveDaemon, name, want string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last interface{}
	for time.Now().Before(deadline) {
		status := httpGet(t, daemon.baseURL+"/config/status")
		servers, _ := status["mcp_servers"].(map[string]interface{})
		last = servers[name]
		if last == want {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Fatalf("MCP server %q state=%v, want %q within %s\ndaemon log:\n%s", name, last, want, timeout, daemon.output.String())
}

func assertPathAbsent(t *testing.T, path, message string) {
	t.Helper()
	if _, err := os.Stat(path); err == nil {
		t.Fatal(message)
	} else if !os.IsNotExist(err) {
		t.Fatalf("stat allowlist marker: %v", err)
	}
}

func sseToolNames(frames []sseFrame) []string {
	var names []string
	for _, frame := range frames {
		if frame.Event != "tool" {
			continue
		}
		var payload struct {
			Tool   string `json:"tool"`
			Status string `json:"status"`
		}
		if json.Unmarshal(frame.Data, &payload) == nil && payload.Tool != "" && payload.Status == "running" {
			names = append(names, payload.Tool)
		}
	}
	return names
}
