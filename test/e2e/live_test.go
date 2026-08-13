//go:build live

package e2e

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	daemonpkg "github.com/Kocoro-lab/ShanClaw/internal/daemon"
	"github.com/Kocoro-lab/ShanClaw/internal/images"
	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	_ "golang.org/x/image/webp"
	"gopkg.in/yaml.v3"
)

// Live E2E tests require SHANNON_E2E_LIVE=1.
// They make real LLM API calls and cost real tokens.
//
// Daemon tests use a temporary state directory and random localhost port. The
// isolated start mode suppresses Cloud WS, watchers, schedulers, credential
// store access, non-allowlisted MCP connects, and process-global browser cleanup
// while preserving the real HTTP/agent/provider path.

func TestLive_OneShotCoreAndBundledAgentsSmoke(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)

	tests := []struct {
		name    string
		args    []string
		prepare func(t *testing.T) (cwd string, verify func(t *testing.T, result map[string]interface{}))
	}{
		{
			name: "core_reasoning",
			args: []string{
				`Calculate 37 * 41. Return exactly one JSON object with this schema: {"schema":"kocoro-live-smoke-v1","product":<integer>,"parity":"odd or even"}.`,
			},
			prepare: func(t *testing.T) (string, func(*testing.T, map[string]interface{})) {
				return "", func(t *testing.T, result map[string]interface{}) {
					requireJSONNumber(t, result, "product", 1517)
					requireJSONString(t, result, "parity", "odd")
				}
			},
		},
		{
			name: "explorer_reads_project_facts",
			args: []string{
				"--agent", "explorer",
				`Inspect service.yaml and dependencies.txt in the current directory. Return exactly one JSON object with this schema: {"schema":"kocoro-live-smoke-v1","service":<string>,"port":<integer>,"dependency":<string>,"source_files":[<strings>]}. Do not infer values without reading the files.`,
			},
			prepare: func(t *testing.T) (string, func(*testing.T, map[string]interface{})) {
				cwd := neutralTempDir(t, "live-smoke-explorer-*")
				writeLiveSmokeFixture(t, cwd, "service.yaml", "service: aurora-ledger\nport: 43127\ndependency_ref: dependencies.txt\n")
				writeLiveSmokeFixture(t, cwd, "dependencies.txt", "primary=quartz-index-v7\n")
				return cwd, func(t *testing.T, result map[string]interface{}) {
					requireJSONString(t, result, "service", "aurora-ledger")
					requireJSONNumber(t, result, "port", 43127)
					requireJSONString(t, result, "dependency", "quartz-index-v7")
					requireJSONStringSet(t, result, "source_files", []string{"dependencies.txt", "service.yaml"})
				}
			},
		},
		{
			name: "reviewer_finds_concrete_bug_without_editing",
			args: []string{
				"--agent", "reviewer",
				`Review review_target.go in the current directory for its primary correctness bug. Return exactly one JSON object with this schema: {"schema":"kocoro-live-smoke-v1","symbol":<string>,"severity":"high or medium or low","actual_expression":<string>,"correct_expression":<string>,"failure_condition":<string>}. Do not modify the file.`,
			},
			prepare: func(t *testing.T) (string, func(*testing.T, map[string]interface{})) {
				cwd := neutralTempDir(t, "live-smoke-reviewer-*")
				const source = "package ledger\n\n// Remaining returns unspent capacity.\nfunc Remaining(total, used uint64) uint64 {\n\treturn used - total\n}\n"
				writeLiveSmokeFixture(t, cwd, "review_target.go", source)
				return cwd, func(t *testing.T, result map[string]interface{}) {
					requireJSONString(t, result, "symbol", "Remaining")
					severity, _ := result["severity"].(string)
					if severity != "high" && severity != "medium" {
						t.Errorf("severity = %q, want high or medium", severity)
					}
					requireJSONString(t, result, "actual_expression", "used - total")
					requireJSONString(t, result, "correct_expression", "total - used")
					failure, _ := result["failure_condition"].(string)
					failure = strings.ToLower(failure)
					if !strings.Contains(failure, "used") || !strings.Contains(failure, "total") ||
						(!strings.Contains(failure, "<") && !strings.Contains(failure, "less")) {
						t.Errorf("failure_condition = %q, want the used < total underflow condition", result["failure_condition"])
					}
					got, err := os.ReadFile(filepath.Join(cwd, "review_target.go"))
					if err != nil {
						t.Fatalf("read reviewer fixture after run: %v", err)
					}
					if string(got) != source {
						t.Fatal("reviewer modified review_target.go")
					}
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cwd, verify := tt.prepare(t)
			out := runShanInDir(t, cwd, bin, tt.args...)
			if strings.Contains(out, "gpt-5-mini") {
				t.Error("should not fall back to gpt-5-mini — check cache_break fix")
			}
			result := decodeLiveSmokeResult(t, out)
			verify(t, result)
		})
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

func TestLive_Daemon_MessageAndEditRetry(t *testing.T) {
	skipUnlessLive(t)
	bin := testBinary(t)
	credentialBefore, hasCredentialFingerprint := liveCredentialStoreFingerprint(t)
	daemon := startIsolatedLiveDaemon(t, bin)

	// Send message
	messageStarted := time.Now()
	resp := httpPost(t, daemon.baseURL+"/message", map[string]interface{}{
		"text": "what is 7+7",
	})
	messageLatency := time.Since(messageStarted)
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
	editStarted := time.Now()
	editResp := httpPost(t, fmt.Sprintf("%s/sessions/%s/edit", daemon.baseURL, sessionID), map[string]interface{}{
		"message_index": 0,
		"new_content":   "what is 9+9",
	})
	editLatency := time.Since(editStarted)
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
	t.Logf(
		"real daemon message/edit E2E passed: initial_reply=%q edit_reply=%q messages_before=%d messages_after=%d initial_latency=%s edit_latency=%s total_cost=$%.6f",
		reply,
		editReply,
		len(messages),
		len(messages2),
		messageLatency.Round(time.Millisecond),
		editLatency.Round(time.Millisecond),
		runResultCost(resp)+runResultCost(editResp),
	)

	daemon.stop(t)
	if hasCredentialFingerprint {
		credentialAfter, ok := liveCredentialStoreFingerprint(t)
		if !ok || credentialAfter != credentialBefore {
			t.Fatal("isolated daemon changed the active OS credential-store entry")
		}
	}
}

func runResultCost(result map[string]interface{}) float64 {
	usage, _ := result["usage"].(map[string]interface{})
	cost, _ := usage["cost_usd"].(float64)
	return cost
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
	return runShanInDir(t, "", bin, args...)
}

func runShanInDir(t *testing.T, dir, bin string, args ...string) string {
	t.Helper()
	cmd := exec.Command(bin, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("shan %v failed: %v\n%s", args, err, stdout.String())
	}
	return stdout.String()
}

func writeLiveSmokeFixture(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
		t.Fatalf("write live smoke fixture %s: %v", name, err)
	}
}

func decodeLiveSmokeResult(t *testing.T, output string) map[string]interface{} {
	t.Helper()
	for offset := 0; offset < len(output); {
		relative := strings.IndexByte(output[offset:], '{')
		if relative < 0 {
			break
		}
		start := offset + relative
		var result map[string]interface{}
		if err := json.NewDecoder(strings.NewReader(output[start:])).Decode(&result); err == nil {
			if result["schema"] == "kocoro-live-smoke-v1" {
				return result
			}
		}
		offset = start + 1
	}
	t.Fatalf("output did not contain a valid kocoro-live-smoke-v1 JSON object: %s", output)
	return nil
}

func requireJSONString(t *testing.T, result map[string]interface{}, key, want string) {
	t.Helper()
	got, ok := result[key].(string)
	if !ok || got != want {
		t.Errorf("%s = %#v, want %q", key, result[key], want)
	}
}

func requireJSONNumber(t *testing.T, result map[string]interface{}, key string, want float64) {
	t.Helper()
	got, ok := result[key].(float64)
	if !ok || got != want {
		t.Errorf("%s = %#v, want %v", key, result[key], want)
	}
}

func requireJSONStringSet(t *testing.T, result map[string]interface{}, key string, want []string) {
	t.Helper()
	values, ok := result[key].([]interface{})
	if !ok || len(values) != len(want) {
		t.Errorf("%s = %#v, want exactly %v", key, result[key], want)
		return
	}
	got := make(map[string]bool, len(values))
	for _, value := range values {
		s, ok := value.(string)
		if !ok {
			t.Errorf("%s contains non-string value %#v", key, value)
			return
		}
		got[filepath.Base(s)] = true
	}
	for _, value := range want {
		if !got[value] {
			t.Errorf("%s = %#v, missing %q", key, result[key], value)
		}
	}
}

type isolatedLiveDaemon struct {
	baseURL       string
	stateDir      string
	cloudEndpoint string
	options       isolatedLiveOptions
	cmd           *exec.Cmd
	done          chan error
	output        synchronizedBuffer
}

type isolatedLiveOptions struct {
	EffortTier   string
	AutoApprove  bool
	MCPAllowlist string
	MCPServers   map[string]mcp.MCPServerConfig
}

type isolatedConfigFile struct {
	Endpoint string `yaml:"endpoint"`
	Daemon   struct {
		AutoApprove bool `yaml:"auto_approve"`
	} `yaml:"daemon"`
	Agent struct {
		EffortTier string `yaml:"effort_tier,omitempty"`
	} `yaml:"agent,omitempty"`
	MCPServers map[string]mcp.MCPServerConfig `yaml:"mcp_servers,omitempty"`
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

// writeIsolatedDaemonConfig persists only non-secret coordinates and explicit
// test capabilities. The API key is sent separately through stdin and never
// enters config.yaml, command arguments, environment variables, or logs.
func writeIsolatedDaemonConfig(t *testing.T, stateDir, endpoint string, options isolatedLiveOptions) {
	t.Helper()
	config := isolatedConfigFile{Endpoint: endpoint, MCPServers: options.MCPServers}
	config.Daemon.AutoApprove = options.AutoApprove
	config.Agent.EffortTier = options.EffortTier
	data, err := yaml.Marshal(config)
	if err != nil {
		t.Fatalf("marshal isolated daemon config: %v", err)
	}
	if bytes.Contains(data, []byte("api_key")) {
		t.Fatal("isolated daemon config unexpectedly contains an api_key field")
	}
	if err := os.WriteFile(filepath.Join(stateDir, "config.yaml"), data, 0o600); err != nil {
		t.Fatalf("seed isolated daemon config: %v", err)
	}
}

func startIsolatedLiveDaemon(t *testing.T, bin string, requestedOptions ...isolatedLiveOptions) *isolatedLiveDaemon {
	t.Helper()
	if len(requestedOptions) > 1 {
		t.Fatal("startIsolatedLiveDaemon accepts at most one options value")
	}
	var options isolatedLiveOptions
	if len(requestedOptions) == 1 {
		options = requestedOptions[0]
	}
	endpoint, apiKey := readCloudConfig(t)
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
	writeIsolatedDaemonConfig(t, stateDir, endpoint, options)
	daemon := &isolatedLiveDaemon{
		baseURL:       fmt.Sprintf("http://127.0.0.1:%d", port),
		stateDir:      stateDir,
		cloudEndpoint: endpoint,
		options:       options,
	}
	args := []string{
		"daemon", "start", "--isolated",
		"--state-dir", stateDir,
		"--port", strconv.Itoa(port),
		"--isolated-api-key-stdin",
	}
	if strings.TrimSpace(options.MCPAllowlist) != "" {
		args = append(args, "--isolated-mcp", options.MCPAllowlist)
	}
	cmd := exec.Command(bin, args...)
	cmd.Stdout = &daemon.output
	cmd.Stderr = &daemon.output
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatalf("isolated daemon stdin pipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("daemon start: %v", err)
	}
	daemon.cmd = cmd
	daemon.done = make(chan error, 1)
	go func() { daemon.done <- cmd.Wait() }()
	t.Cleanup(func() { daemon.stop(t) })
	if _, err := io.WriteString(stdin, apiKey); err != nil {
		daemon.stop(t)
		t.Fatalf("send isolated daemon credential: %v", err)
	}
	if err := stdin.Close(); err != nil {
		daemon.stop(t)
		t.Fatalf("close isolated daemon credential pipe: %v", err)
	}
	waitForDaemon(t, daemon, 15*time.Second)
	waitForIsolationMarkers(t, daemon, 3*time.Second)
	configBytes, err := os.ReadFile(filepath.Join(stateDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read isolated config after startup: %v", err)
	}
	if bytes.Contains(configBytes, []byte("api_key")) || bytes.Contains(configBytes, []byte(apiKey)) {
		t.Fatal("isolated daemon persisted its API key")
	}
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
		daemonpkg.IsolationMarkerAutomationDisabled,
		daemonpkg.IsolationMarkerCloudWSSuppressed,
		daemonpkg.IsolationMarkerBackgroundDisabled,
		daemonpkg.IsolationMarkerCredentialStoreDisabled,
	}
	// MCP is either fully off or narrowed to an --isolated-mcp allowlist. Both
	// are contained startups, so accept either marker — but require one of
	// them, so a build that silently stopped emitting an MCP decision (and
	// therefore might be connecting to every configured server) still fails.
	mcpMarkers := []string{
		daemonpkg.IsolationMarkerMCPDisabled,
		daemonpkg.IsolationMarkerMCPAllowlisted,
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
			sawMCPDecision := false
			for _, marker := range mcpMarkers {
				if strings.Contains(log, marker) {
					sawMCPDecision = true
					break
				}
			}
			complete = sawMCPDecision
		}
		if complete {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatalf("isolated daemon did not confirm contained startup\n%s", daemon.output.String())
}

// TestLive_ImageGenerateEditQuality exercises the production image and CDN
// path, then applies deterministic color/layout oracles to one deliberately
// simple generation and edit. It is not a general perceptual-quality score.
func TestLive_ImageGenerateEditQuality(t *testing.T) {
	skipUnlessLive(t)
	if os.Getenv("KOCORO_IMAGE_QUALITY_LIVE") != "1" {
		t.Skip("set KOCORO_IMAGE_QUALITY_LIVE=1 to run the paid image quality E2E")
	}
	sample := strings.TrimSpace(os.Getenv("KOCORO_IMAGE_QUALITY_SAMPLE"))
	if sample == "" {
		sample = "comparison"
	}
	repetitions := 1
	switch sample {
	case "comparison":
	case "release":
		repetitions = 5
	default:
		t.Fatalf("KOCORO_IMAGE_QUALITY_SAMPLE must be comparison or release, got %q", sample)
	}

	endpoint, apiKey := readCloudConfig(t)
	if endpoint == "" || apiKey == "" {
		t.Fatal("cloud.endpoint / cloud.api_key not configured in ~/.shannon/config.yaml")
	}
	client := images.NewClient(endpoint, apiKey, &http.Client{Timeout: 180 * time.Second})

	allPassed := true
	for repetition := 1; repetition <= repetitions; repetition++ {
		if !t.Run(fmt.Sprintf("repetition_%d", repetition), func(t *testing.T) {
			genCtx, genCancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer genCancel()
			gen, err := client.Generate(genCtx, images.GenerateRequest{
				Prompt:  "a single solid red circle centered on a plain pure white background, flat geometric design, no text, no shadow, no other objects",
				Size:    "1024x1024",
				Quality: "low",
				N:       1,
			})
			if err != nil {
				if errors.Is(err, images.ErrEndpointNotFound) {
					t.Fatalf("image endpoint not deployed at the configured gateway: %v", err)
				}
				t.Fatalf("Generate: %v", err)
			}
			if len(gen.Images) != 1 {
				t.Fatalf("Generate: expected 1 image, got %d", len(gen.Images))
			}
			if !strings.HasPrefix(gen.Images[0].URL, "https://static.kocoro.ai/") {
				t.Fatalf("Generate: expected https://static.kocoro.ai/ URL, got: %s", gen.Images[0].URL)
			}
			if gen.Model != "gpt-image-2" {
				t.Fatalf("Generate: expected gpt-image-2 model, got: %s", gen.Model)
			}
			if gen.Images[0].SizeBytes <= 0 {
				t.Fatalf("Generate: expected positive size_bytes, got: %d", gen.Images[0].SizeBytes)
			}
			source := downloadLiveImage(t, gen.Images[0].URL)
			if source.Bounds().Dx() != 1024 || source.Bounds().Dy() != 1024 {
				t.Fatalf("Generate: dimensions = %dx%d, want 1024x1024", source.Bounds().Dx(), source.Bounds().Dy())
			}
			if err := validateGeneratedImageSemantics(source); err != nil {
				t.Fatalf("Generate semantic oracle: %v", err)
			}

			editCtx, editCancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer editCancel()
			edit, err := client.Edit(editCtx, images.EditRequest{
				Prompt:    "keep the image unchanged except add one small solid blue square entirely inside the bottom-right quadrant, no text",
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
				t.Fatalf("Edit: expected https://static.kocoro.ai/ URL, got: %s", edit.Images[0].URL)
			}
			if edit.Images[0].URL == gen.Images[0].URL {
				t.Fatal("Edit returned the source URL")
			}
			edited := downloadLiveImage(t, edit.Images[0].URL)
			if err := validateEditedImageSemantics(source, edited); err != nil {
				t.Fatalf("Edit semantic oracle: %v", err)
			}
		}) {
			allPassed = false
		}
	}
	if !allPassed {
		t.FailNow()
	}
	t.Logf("image quality E2E passed: sample=%s repetitions=%d paid_calls=%d", sample, repetitions, repetitions*2)
}

func downloadLiveImage(t *testing.T, url string) image.Image {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		t.Fatalf("build image download request: %v", err)
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("download image: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		t.Fatalf("download image status = %s", response.Status)
	}
	const maximumImageBytes = 25 << 20
	data, err := io.ReadAll(io.LimitReader(response.Body, maximumImageBytes+1))
	if err != nil {
		t.Fatalf("read image: %v", err)
	}
	if len(data) > maximumImageBytes {
		t.Fatalf("downloaded image exceeds %d bytes", maximumImageBytes)
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		t.Fatalf("decode image with content type %q: %v", response.Header.Get("Content-Type"), err)
	}
	return decoded
}

// readCloudConfig resolves the explicit live-E2E endpoint and credential. A
// migrated credential is read from the existing store into process memory; it
// is never copied into the isolated state directory, environment, arguments,
// or logs.
func readCloudConfig(t *testing.T) (endpoint, apiKey string) {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve live E2E home: %v", err)
	}
	shannonDir := filepath.Join(home, ".shannon")
	data, err := os.ReadFile(filepath.Join(shannonDir, "config.yaml"))
	if err != nil {
		t.Fatalf("read live E2E config: %v", err)
	}
	var persisted struct {
		Endpoint string `yaml:"endpoint"`
		APIKey   string `yaml:"api_key"`
	}
	if err := yaml.Unmarshal(data, &persisted); err != nil {
		t.Fatalf("parse live E2E config: %v", err)
	}
	endpoint = strings.TrimSpace(persisted.Endpoint)
	apiKey = strings.TrimSpace(persisted.APIKey)
	if endpoint == "" {
		t.Fatal("live E2E config has no endpoint")
	}
	if apiKey == "" && keychain.Supported() {
		store, err := keychain.NewOSStoreAt(shannonDir, nil)
		if err != nil {
			t.Fatalf("open live E2E credential store: %v", err)
		}
		apiKey, err = store.GetAPIKey()
		if err != nil {
			t.Fatalf("read live E2E credential store: %v", err)
		}
		apiKey = strings.TrimSpace(apiKey)
	}
	if apiKey == "" {
		t.Fatal("live E2E has no configured API key")
	}
	return endpoint, apiKey
}

func liveCredentialStoreFingerprint(t *testing.T) (string, bool) {
	t.Helper()
	if !keychain.Supported() {
		return "", false
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("resolve credential-store home: %v", err)
	}
	store, err := keychain.NewOSStoreAt(filepath.Join(home, ".shannon"), nil)
	if err != nil {
		t.Fatalf("open credential store for fingerprint: %v", err)
	}
	userID, apiKey, err := store.GetActiveUserAndKey()
	if err != nil {
		t.Fatalf("read credential store for fingerprint: %v", err)
	}
	if userID == "" || apiKey == "" {
		return "", false
	}
	digest := sha256.Sum256([]byte(userID + "\x00" + apiKey))
	return fmt.Sprintf("%x", digest[:]), true
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

type sseFrame struct {
	Event string
	Data  []byte
}

type liveSSERun struct {
	Frames   []sseFrame
	Result   map[string]interface{}
	Duration time.Duration
	CostUSD  float64
}

func streamMessage(t *testing.T, baseURL string, body map[string]interface{}) liveSSERun {
	t.Helper()
	payload, err := json.Marshal(body)
	if err != nil {
		t.Fatalf("marshal SSE message: %v", err)
	}
	requestCtx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, baseURL+"/message", bytes.NewReader(payload))
	if err != nil {
		t.Fatalf("build SSE message request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")

	started := time.Now()
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatalf("POST SSE message: %v", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 16*1024))
		t.Fatalf("POST SSE message status=%s body=%s", response.Status, body)
	}

	run := liveSSERun{}
	scanner := bufio.NewScanner(response.Body)
	scanner.Buffer(make([]byte, 64*1024), 2*1024*1024)
	var event string
	var data strings.Builder
	dispatch := func() {
		if event == "" {
			data.Reset()
			return
		}
		frameData := []byte(strings.TrimSuffix(data.String(), "\n"))
		run.Frames = append(run.Frames, sseFrame{Event: event, Data: frameData})
		switch event {
		case "usage":
			var usage struct {
				CostUSD float64 `json:"cost_usd"`
			}
			if json.Unmarshal(frameData, &usage) == nil {
				run.CostUSD += usage.CostUSD
			}
		case "done":
			if err := json.Unmarshal(frameData, &run.Result); err != nil {
				t.Fatalf("decode SSE done frame: %v; data=%s", err, frameData)
			}
		case "error":
			t.Fatalf("live SSE returned error: %s", frameData)
		}
		event = ""
		data.Reset()
	}
	for scanner.Scan() {
		line := scanner.Text()
		switch {
		case line == "":
			dispatch()
		case strings.HasPrefix(line, "event:"):
			event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data.WriteString(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
			data.WriteByte('\n')
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read live SSE stream: %v", err)
	}
	dispatch()
	run.Duration = time.Since(started)
	if run.Result == nil {
		t.Fatalf("live SSE ended without done frame; events=%v", sseEventNames(run.Frames))
	}
	return run
}

func sseEventNames(frames []sseFrame) []string {
	names := make([]string, 0, len(frames))
	for _, frame := range frames {
		names = append(names, frame.Event)
	}
	return names
}
