package config

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"

	"github.com/Kocoro-lab/ShanClaw/internal/keychain"
)

// withTestKeychain swaps keychainStoreOpener for an in-memory backend so
// setup tests on darwin don't pollute the real Keychain. Returns the mem
// backend so tests can assert on writes.
func withTestKeychain(t *testing.T) *keychain.MemBackend {
	t.Helper()
	be := keychain.NewMemBackend()
	prev := keychainStoreOpener
	keychainStoreOpener = func() (*keychain.Store, error) {
		return keychain.NewStore(be, nil), nil
	}
	t.Cleanup(func() { keychainStoreOpener = prev })
	return be
}

func TestRunSetup_OllamaProvider(t *testing.T) {
	ollama := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/":
			w.Write([]byte("Ollama is running"))
		case "/api/tags":
			json.NewEncoder(w).Encode(map[string]any{
				"models": []map[string]any{
					{"name": "qwen3:4b", "size": 2500000000, "details": map[string]string{"parameter_size": "4B"}},
					{"name": "llama3.1:8b", "size": 5200000000, "details": map[string]string{"parameter_size": "8B"}},
				},
			})
		default:
			w.WriteHeader(404)
		}
	}))
	defer ollama.Close()

	cfg := &Config{}
	// Simulate: choose "2" (Ollama) → enter endpoint → choose model "1"
	input := strings.NewReader("2\n" + ollama.URL + "\n1\n")
	var output bytes.Buffer

	err := RunSetup(cfg, input, &output)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if cfg.Provider != "ollama" {
		t.Errorf("expected provider=ollama, got %q", cfg.Provider)
	}
	if cfg.Ollama.Model != "qwen3:4b" {
		t.Errorf("expected model=qwen3:4b, got %q", cfg.Ollama.Model)
	}
	if cfg.Ollama.Endpoint != ollama.URL {
		t.Errorf("expected endpoint=%s, got %q", ollama.URL, cfg.Ollama.Endpoint)
	}
}

func TestRunSetup_GatewayProvider(t *testing.T) {
	gw := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
	}))
	defer gw.Close()

	be := withTestKeychain(t)

	cfg := &Config{}
	// Simulate: choose "1" (Cloud) → enter endpoint → enter API key
	input := strings.NewReader("1\n" + gw.URL + "\ntest-key\n")
	var output bytes.Buffer

	err := RunSetup(cfg, input, &output)
	if err != nil {
		t.Fatalf("setup failed: %v", err)
	}
	if cfg.Provider != "" && cfg.Provider != "gateway" {
		t.Errorf("expected provider=gateway or empty, got %q", cfg.Provider)
	}
	if runtime.GOOS == "darwin" {
		// macOS path: api_key is routed to Keychain and cleared from cfg.
		if cfg.APIKey != "" {
			t.Errorf("on darwin, cfg.APIKey should be cleared after Keychain write, got %q", cfg.APIKey)
		}
		snap := be.Snapshot()
		var legacyVal string
		for k, v := range snap {
			if strings.Contains(k, keychain.AccountLegacy) && strings.HasPrefix(k, keychain.ServiceDaemonAPIKey) {
				legacyVal = v
			}
		}
		if legacyVal != "test-key" {
			t.Errorf("expected Keychain legacy entry=test-key, got %q (snapshot=%v)", legacyVal, snap)
		}
	} else {
		// Non-darwin: api_key stays in cfg.APIKey (legacy yaml path).
		if cfg.APIKey != "test-key" {
			t.Errorf("expected api_key=test-key, got %q", cfg.APIKey)
		}
	}
}
