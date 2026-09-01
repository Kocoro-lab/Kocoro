package daemon

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// newReloadRestartTestServer builds a Server whose /config/reload can run
// end-to-end without touching the real filesystem config or Cloud: the
// gateway points at a stub upstream and loadConfigWithRevision is replaced
// by the test.
func newReloadRestartTestServer(t *testing.T, liveCfg, reloadedCfg *config.Config) *Server {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Bare array: the shape both tool-list endpoints decode, so the
		// reload's overlay refresh takes the clean path instead of the
		// keep-existing degrade path.
		_, _ = w.Write([]byte(`[]`))
	}))
	t.Cleanup(upstream.Close)

	shannonDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(shannonDir, "config.yaml"), []byte("{}\n"), 0600); err != nil {
		t.Fatal(err)
	}
	srv := NewServer(0, nil, &ServerDeps{
		ShannonDir: shannonDir,
		Config:     liveCfg,
		GW:         client.NewGatewayClient(upstream.URL, "test-key"),
	}, "test")
	srv.loadConfigWithRevision = func() (*config.Config, string, error) {
		return reloadedCfg, "test-revision", nil
	}
	return srv
}

func postConfigReload(t *testing.T, srv *Server) map[string]interface{} {
	t.Helper()
	rr := httptest.NewRecorder()
	srv.handleConfigReload(rr, httptest.NewRequest(http.MethodPost, "/config/reload", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("POST /config/reload = %d, body=%s", rr.Code, rr.Body.String())
	}
	var response map[string]interface{}
	if err := json.Unmarshal(rr.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	return response
}

// Isolated mode (--isolated --isolated-api-key-stdin) keeps the API key in
// process memory only; yaml never persists it. The legacy "api_key changed in
// yaml" restart heuristic must not fire on the resulting stdin-key vs empty
// yaml-key mismatch — the reload does not touch the runtime key, so the
// restart signal would be a false positive on every reload.
func TestConfigReloadIsolatedInMemoryKeyDoesNotRequireRestart(t *testing.T) {
	srv := newReloadRestartTestServer(t,
		&config.Config{APIKey: "stdin-injected-key"},
		&config.Config{},
	)
	srv.credentialStoreDisabled = func() bool { return true }

	response := postConfigReload(t, srv)
	if response["restart_required"] == true {
		t.Fatalf("isolated reload reported restart_required: reason=%v", response["restart_reason"])
	}
}

// Legacy path regression guard: with no AuthManager and the credential store
// enabled, a real yaml api_key edit still requires a restart because the
// GatewayClient captured the old key at construction.
func TestConfigReloadLegacyYamlAPIKeyChangeStillRequiresRestart(t *testing.T) {
	srv := newReloadRestartTestServer(t,
		&config.Config{APIKey: "old-key"},
		&config.Config{APIKey: "new-key"},
	)

	response := postConfigReload(t, srv)
	if response["restart_required"] != true {
		t.Fatalf("legacy yaml api_key change did not report restart_required: %v", response)
	}
	reason, _ := response["restart_reason"].(string)
	if !strings.Contains(reason, "api_key changed in yaml") {
		t.Fatalf("restart_reason = %q, want it to mention api_key changed in yaml", reason)
	}
}
