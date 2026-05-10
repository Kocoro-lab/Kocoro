package memory

import (
	"testing"
	"time"

	appconfig "github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/spf13/viper"
)

func TestResolveAPIKey(t *testing.T) {
	cases := []struct{ mem, cloud, want string }{
		{"", "", ""},
		{"", "c", "c"},
		{"m", "", "m"},
		{"m", "c", "m"},
	}
	for _, tc := range cases {
		v := viper.New()
		v.Set("memory.api_key", tc.mem)
		v.Set("cloud.api_key", tc.cloud)
		if got := ResolveAPIKey(v); got != tc.want {
			t.Fatalf("mem=%q cloud=%q got %q want %q", tc.mem, tc.cloud, got, tc.want)
		}
	}
}

func TestResolveEndpoint(t *testing.T) {
	v := viper.New()
	v.Set("memory.endpoint", "")
	v.Set("cloud.endpoint", "https://c")
	if got := ResolveEndpoint(v); got != "https://c" {
		t.Fatalf("got %q", got)
	}
	v.Set("memory.endpoint", "https://m")
	if got := ResolveEndpoint(v); got != "https://m" {
		t.Fatalf("got %q", got)
	}
}

func TestLoadConfig_HomeExpansion(t *testing.T) {
	t.Setenv("HOME", "/tmp/fakehome")
	v := viper.New()
	v.Set("memory.bundle_root", "$HOME/.shannon/memory")
	v.Set("memory.socket_path", "$HOME/.shannon/memory.sock")
	v.Set("memory.bundle_pull_interval", "24h")
	v.Set("memory.sidecar_ready_timeout", "10s")
	cfg := LoadConfig(v)
	if cfg.BundleRoot != "/tmp/fakehome/.shannon/memory" {
		t.Fatalf("BundleRoot=%q", cfg.BundleRoot)
	}
	if cfg.SocketPath != "/tmp/fakehome/.shannon/memory.sock" {
		t.Fatalf("SocketPath=%q", cfg.SocketPath)
	}
}

func TestLoadConfigFromRuntime_UsesRuntimeMemoryOverlay(t *testing.T) {
	t.Setenv("HOME", "/tmp/fakehome")
	cfg := &appconfig.Config{
		Endpoint: "https://cloud.example",
		APIKey:   "cloud-key",
		Memory: appconfig.MemoryConfig{
			Provider:             "local",
			SocketPath:           "$HOME/tlm.sock",
			BundleRoot:           "$HOME/tlm-bundles",
			TLMPath:              "$HOME/bin/tlm",
			ClientRequestTimeout: 30 * time.Second,
		},
	}

	got := LoadConfigFromRuntime(cfg)

	if got.Provider != "local" {
		t.Fatalf("Provider=%q", got.Provider)
	}
	if got.SocketPath != "/tmp/fakehome/tlm.sock" {
		t.Fatalf("SocketPath=%q", got.SocketPath)
	}
	if got.BundleRoot != "/tmp/fakehome/tlm-bundles" {
		t.Fatalf("BundleRoot=%q", got.BundleRoot)
	}
	if got.TLMPath != "/tmp/fakehome/bin/tlm" {
		t.Fatalf("TLMPath=%q", got.TLMPath)
	}
	if got.Endpoint != "https://cloud.example" || got.APIKey != "cloud-key" {
		t.Fatalf("fallback endpoint/api = %q/%q", got.Endpoint, got.APIKey)
	}
	if got.ClientRequestTimeout != 30*time.Second {
		t.Fatalf("ClientRequestTimeout=%v", got.ClientRequestTimeout)
	}
}
