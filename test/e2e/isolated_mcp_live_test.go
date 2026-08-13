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
	"sync/atomic"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
)

const (
	browserOutcomeLiveEnv            = "KOCORO_BROWSER_OUTCOME_LIVE"
	browserOutcomeSampleEnv          = "KOCORO_BROWSER_OUTCOME_SAMPLE"
	browserOutcomeReleaseRepetitions = 5
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
	playwrightMCP := requirePlaywrightLivePrerequisites(t)

	const token = "KOCORO_ISOLATED_MCP_E2E_7F31"
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprintf(w, `<!doctype html><title>isolated mcp</title><main><span id="e2e-token">%s</span></main>`, token)
	}))
	defer page.Close()

	marker := filepath.Join(t.TempDir(), "unallowlisted-mcp-started")
	options := isolatedPlaywrightOptions(playwrightMCP, marker, "Isolated Playwright browser used only for this E2E page.")

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

// TestLive_BrowserOutcomeMatrix verifies two user-visible browser outcomes on
// deterministic local pages: following a link to extract facts and submitting
// a form exactly once. It intentionally leaves tool selection to the agent;
// the older allowlist test above keeps the lower-level MCP wiring diagnosis.
func TestLive_BrowserOutcomeMatrix(t *testing.T) {
	skipUnlessLive(t)
	if os.Getenv(browserOutcomeLiveEnv) != "1" {
		t.Skipf("set %s=1 to run the paid browser outcome matrix", browserOutcomeLiveEnv)
	}
	playwrightMCP := requirePlaywrightLivePrerequisites(t)

	sample := strings.TrimSpace(os.Getenv(browserOutcomeSampleEnv))
	if sample == "" {
		sample = "comparison"
	}
	repetitions := 1
	switch sample {
	case "comparison":
	case "release":
		repetitions = browserOutcomeReleaseRepetitions
	default:
		t.Fatalf("%s must be comparison or release, got %q", browserOutcomeSampleEnv, sample)
	}

	var detailViews atomic.Int32
	var validSubmissions atomic.Int32
	page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		switch r.URL.Path {
		case "/start":
			fmt.Fprint(w, `<!doctype html><title>Project index</title><main><a href="/details">Open project details</a></main>`)
		case "/details":
			detailViews.Add(1)
			fmt.Fprint(w, `<!doctype html><title>Project details</title><main><dl><dt>Project</dt><dd>Orbit</dd><dt>Approved units</dt><dd>731</dd><dt>Reviewer</dt><dd>Mina</dd></dl></main>`)
		case "/form":
			fmt.Fprint(w, `<!doctype html><title>Approval form</title><main><form method="post" action="/submit"><label>Approval code <input name="approval_code"></label><button type="submit">Submit approval</button></form></main>`)
		case "/submit":
			if r.Method != http.MethodPost {
				http.Error(w, "POST required", http.StatusMethodNotAllowed)
				return
			}
			if err := r.ParseForm(); err != nil || r.Form.Get("approval_code") != "ORBIT-731" {
				http.Error(w, "invalid approval code", http.StatusUnprocessableEntity)
				return
			}
			validSubmissions.Add(1)
			fmt.Fprint(w, `<!doctype html><title>Approval complete</title><main id="receipt">RECEIPT-BROWSER-731</main>`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer page.Close()

	marker := filepath.Join(t.TempDir(), "unallowlisted-mcp-started")
	options := isolatedPlaywrightOptions(playwrightMCP, marker, "Browser outcome qualification on deterministic local pages.")
	daemon := startIsolatedLiveDaemon(t, testBinary(t), options)
	waitForMCPState(t, daemon, "playwright", "connected", 45*time.Second)

	var totalCost float64
	allPassed := true
	for repetition := 1; repetition <= repetitions; repetition++ {
		if !t.Run(fmt.Sprintf("cross_page_read_%d", repetition), func(t *testing.T) {
			before := detailViews.Load()
			prompt := fmt.Sprintf(
				"Open %s in an interactive browser, follow the project details link, and reply with the project name, approved units, and reviewer. Do not use shell commands or a direct HTTP tool.",
				page.URL+"/start",
			)
			run := streamMessage(t, daemon.baseURL, map[string]interface{}{"text": prompt, "source": "kocoro"})
			totalCost += run.CostUSD
			reply, _ := run.Result["reply"].(string)
			for _, want := range []string{"Orbit", "731", "Mina"} {
				if !strings.Contains(reply, want) {
					t.Fatalf("cross-page reply missing %q: %q", want, reply)
				}
			}
			if detailViews.Load() <= before {
				t.Fatal("browser answer did not load the linked details page")
			}
			assertBrowserOutcomeTools(t, run.Frames)
		}) {
			allPassed = false
		}

		if !t.Run(fmt.Sprintf("form_submit_%d", repetition), func(t *testing.T) {
			before := validSubmissions.Load()
			prompt := fmt.Sprintf(
				"Open %s in an interactive browser, enter approval code ORBIT-731, submit the form once, and return the confirmation receipt shown by the page. Do not use shell commands or a direct HTTP tool.",
				page.URL+"/form",
			)
			run := streamMessage(t, daemon.baseURL, map[string]interface{}{"text": prompt, "source": "kocoro"})
			totalCost += run.CostUSD
			reply, _ := run.Result["reply"].(string)
			if !strings.Contains(reply, "RECEIPT-BROWSER-731") {
				t.Fatalf("form reply missing receipt: %q", reply)
			}
			if got := validSubmissions.Load() - before; got != 1 {
				t.Fatalf("valid form submissions = %d, want exactly 1", got)
			}
			assertBrowserOutcomeTools(t, run.Frames)
		}) {
			allPassed = false
		}
	}

	assertPathAbsent(t, marker, "unallowlisted MCP process started during browser outcome runs")
	if !allPassed {
		t.FailNow()
	}
	if sample == "release" && totalCost <= 0 {
		t.Fatal("release browser outcome run did not report provider cost")
	}
	t.Logf("browser outcome matrix passed: sample=%s repetitions=%d runs=%d cost=$%.6f", sample, repetitions, repetitions*2, totalCost)
}

func requirePlaywrightLivePrerequisites(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("Playwright browser E2E currently qualifies the macOS release path")
	}
	playwrightMCP, err := exec.LookPath("playwright-mcp")
	if err != nil {
		t.Fatalf("real E2E requires playwright-mcp on PATH: %v", err)
	}
	if _, err := os.Stat("/Applications/Google Chrome.app/Contents/MacOS/Google Chrome"); err != nil {
		t.Fatalf("real E2E requires Google Chrome: %v", err)
	}
	return playwrightMCP
}

func isolatedPlaywrightOptions(command, marker, context string) isolatedLiveOptions {
	return isolatedLiveOptions{
		MCPAllowlist: "playwright",
		MCPServers: map[string]mcp.MCPServerConfig{
			"playwright": {
				Command: command,
				Args: []string{
					"--headless",
					"--isolated",
					"--browser", "chrome",
					"--output-mode", "stdout",
				},
				Context:               context,
				KeepAlive:             true,
				ConnectTimeoutSeconds: 30,
			},
			"must-not-start": {
				Command: "/usr/bin/touch",
				Args:    []string{marker},
			},
		},
	}
}

func assertBrowserOutcomeTools(t *testing.T, frames []sseFrame) {
	t.Helper()
	tools := sseToolNames(frames)
	usedBrowser := false
	for _, name := range tools {
		if name == "bash" || name == "http" || name == "web_fetch" {
			t.Fatalf("browser outcome used forbidden shortcut %q; tools=%v", name, tools)
		}
		if name == "browser" || strings.HasPrefix(name, "browser_") {
			usedBrowser = true
		}
	}
	if !usedBrowser {
		t.Fatalf("browser outcome completed without a browser tool; tools=%v", tools)
	}
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
