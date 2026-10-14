package mcp

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// Observe real HTTP requests from both connection paths and after reconnect,
// rather than checking transport options that may never reach the wire.
func TestHTTPMCPUserAgent(t *testing.T) {
	for _, async := range []bool{false, true} {
		t.Run(fmt.Sprintf("async=%t", async), func(t *testing.T) {
			var mu sync.Mutex
			seen := map[string]int{}
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("User-Agent") != "Kocoro (https://github.com/Kocoro-lab/Kocoro)" {
					t.Errorf("unexpected User-Agent: %q", r.Header.Get("User-Agent"))
				}
				if r.Header.Get("Authorization") != "" {
					t.Error("anonymous caller sent Authorization")
				}
				if r.Method != http.MethodPost {
					w.WriteHeader(http.StatusMethodNotAllowed)
					return
				}
				var req struct {
					ID     any    `json:"id"`
					Method string `json:"method"`
					Params struct {
						Name string `json:"name"`
					} `json:"params"`
				}
				if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error(err)
					w.WriteHeader(http.StatusBadRequest)
					return
				}
				mu.Lock()
				seen[req.Method+"/"+req.Params.Name]++
				mu.Unlock()
				if req.ID == nil {
					w.WriteHeader(http.StatusAccepted)
					return
				}
				var result any
				switch req.Method {
				case "initialize":
					result = map[string]any{"protocolVersion": "2025-03-26", "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]any{"name": "fixture", "version": "1"}}
				case "tools/list":
					result = map[string]any{"tools": []any{
						map[string]any{"name": "web_search", "inputSchema": map[string]any{"type": "object"}},
						map[string]any{"name": "web_fetch", "inputSchema": map[string]any{"type": "object"}},
					}}
				case "tools/call":
					result = map[string]any{"content": []any{map[string]any{"type": "text", "text": "https://go.dev/doc/ " + req.Params.Name}}, "isError": false}
				default:
					t.Errorf("unexpected method: %s", req.Method)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": req.ID, "result": result})
			}))
			defer srv.Close()
			mgr := NewClientManager()
			defer mgr.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cfg := BuiltinMCPServers["parallel-search"].Config
			cfg.URL = srv.URL
			servers := map[string]MCPServerConfig{"parallel-search": cfg}
			if async {
				done := make(chan error, 1)
				mgr.StartConnectAll(ctx, servers, 10*time.Second, func(_ string, err error) { done <- err })
				select {
				case err := <-done:
					if err != nil {
						t.Fatal(err)
					}
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
			} else if _, err := mgr.ConnectAll(ctx, servers); err != nil {
				t.Fatal(err)
			}
			for _, name := range []string{"web_search", "web_fetch"} {
				text, isError, err := mgr.CallTool(ctx, "parallel-search", name, map[string]any{})
				if err != nil || isError || text != "https://go.dev/doc/ "+name {
					t.Fatalf("%s: %q %t %v", name, text, isError, err)
				}
			}
			mgr.Disconnect("parallel-search")
			if _, err := mgr.Reconnect(ctx, "parallel-search"); err != nil {
				t.Fatal(err)
			}
			if _, isError, err := mgr.CallTool(ctx, "parallel-search", "web_search", map[string]any{}); err != nil || isError {
				t.Fatalf("reconnect call: %t %v", isError, err)
			}
			mu.Lock()
			defer mu.Unlock()
			for method, count := range map[string]int{"initialize/": 2, "tools/list/": 2, "tools/call/web_search": 2, "tools/call/web_fetch": 1} {
				if seen[method] != count {
					t.Errorf("%s: got %d requests, want %d", method, seen[method], count)
				}
			}
		})
	}
}
