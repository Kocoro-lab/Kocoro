//go:build live

package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/agent"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/spf13/viper"
)

type parallelRequestObserver struct {
	base    http.RoundTripper
	mu      sync.Mutex
	methods map[string]int
	t       *testing.T
}

func (o *parallelRequestObserver) RoundTrip(r *http.Request) (*http.Response, error) {
	if r.URL.Host != "search.parallel.ai" {
		return o.base.RoundTrip(r)
	}
	if r.URL.String() != "https://search.parallel.ai/mcp" {
		o.t.Errorf("unexpected endpoint: %s", r.URL)
	}
	if r.Header.Get("User-Agent") != "Kocoro (https://github.com/Kocoro-lab/Kocoro)" {
		o.t.Errorf("unexpected User-Agent: %q", r.Header.Get("User-Agent"))
	}
	if r.Header.Get("Authorization") != "" || r.Header.Get("X-API-Key") != "" {
		o.t.Error("expected anonymous request")
	}
	if r.Body != nil {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			return nil, err
		}
		_ = r.Body.Close()
		r.Body = io.NopCloser(bytes.NewReader(body))
		var req struct {
			Method string `json:"method"`
			Params struct {
				Name string `json:"name"`
			} `json:"params"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			return nil, err
		}
		o.mu.Lock()
		o.methods[req.Method+"/"+req.Params.Name]++
		o.mu.Unlock()
	}
	resp, err := o.base.RoundTrip(r)
	if resp != nil {
		o.t.Logf("HTTP %d at %s; Retry-After=%q", resp.StatusCode, time.Now().UTC().Format(time.RFC3339), resp.Header.Get("Retry-After"))
	}
	return resp, err
}

// Run explicitly in a fresh test process: the isolation hooks are startup-only.
// SHANNON_PARALLEL_LIVE=1 go test -tags=live ./internal/tools -run '^TestParallelSearchLive$' -v -count=1
func TestParallelSearchLive(t *testing.T) {
	if os.Getenv("SHANNON_PARALLEL_LIVE") != "1" {
		t.Skip("set SHANNON_PARALLEL_LIVE=1 to call the anonymous service")
	}
	state := t.TempDir()
	if err := config.SetShannonDirOverrideForProcess(state); err != nil {
		t.Fatal(err)
	}
	config.DisableCredentialStoreForProcess()
	for _, key := range []string{"SHANNON_API_KEY", "PARALLEL_API_KEY", "API_KEY"} {
		t.Setenv(key, "")
	}
	viper.Reset()
	t.Cleanup(viper.Reset)
	load := func() *config.Config {
		cfg, err := config.Load()
		if err != nil {
			t.Fatal(err)
		}
		if cfg.APIKey != "" || !config.CredentialStoreDisabledForProcess() {
			t.Fatal("credentials are not isolated")
		}
		return cfg
	}
	cfg := load()
	if !cfg.MCPServers["parallel-search"].Disabled {
		t.Fatal("Parallel must start disabled")
	}
	if err := os.WriteFile(filepath.Join(state, "config.yaml"), []byte("mcp_servers:\n  parallel-search:\n    disabled: false\n"), 0600); err != nil {
		t.Fatal(err)
	}
	viper.Reset()
	cfg = load()
	if cfg.MCPServers["parallel-search"].Disabled {
		t.Fatal("documented opt-in did not load")
	}
	original := http.DefaultTransport
	observer := &parallelRequestObserver{base: original, methods: map[string]int{}, t: t}
	http.DefaultTransport = observer
	t.Cleanup(func() { http.DefaultTransport = original })
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	reg, mgr, cleanup, err := CompleteRegistration(ctx, nil, cfg, agent.NewToolRegistry())
	if err != nil {
		t.Fatal(err)
	}
	defer cleanup()
	if mgr == nil || !mgr.IsConnected("parallel-search") {
		t.Fatal("Parallel did not connect through registration")
	}
	for _, call := range []struct{ name, args, want string }{
		{"web_search", `{"objective":"Find the official Go documentation","search_queries":["Go official documentation"],"session_id":"kocoro-parallel-live-validation-session"}`, "go.dev"},
		{"web_fetch", `{"urls":["https://go.dev/doc/"],"objective":"Summarize the Go documentation topics","session_id":"kocoro-parallel-live-validation-session"}`, "Go"},
	} {
		tool, ok := reg.Get(call.name)
		if !ok {
			t.Fatalf("%s missing from registry", call.name)
		}
		if mt, ok := tool.(*MCPTool); !ok || mt.serverName != "parallel-search" || !mt.RequiresApproval() {
			t.Fatalf("unexpected tool: %T", tool)
		}
		result, err := tool.Run(ctx, call.args)
		if err != nil || result.IsError || !strings.Contains(result.Content, call.want) {
			t.Fatalf("%s: err=%v result=%+v", call.name, err, result)
		}
		t.Logf("%s returned %d bytes of useful content: %.500s", call.name, len(result.Content), result.Content)
	}
	observer.mu.Lock()
	defer observer.mu.Unlock()
	for _, method := range []string{"initialize/", "tools/list/", "tools/call/web_search", "tools/call/web_fetch"} {
		if observer.methods[method] < 1 {
			t.Errorf("did not observe %s", method)
		}
	}
}
