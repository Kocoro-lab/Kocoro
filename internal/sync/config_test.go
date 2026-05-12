package sync

import (
	"testing"
	"time"

	"github.com/spf13/viper"
)

func TestLoadConfig_Defaults(t *testing.T) {
	viper.Reset()
	// Mirror the defaults from internal/config/config.go that this test depends on.
	// In real use, internal/config.Load() sets these. For unit isolation we set
	// them via the same SetDefault calls.
	setSyncDefaults(viper.GetViper())

	cfg := LoadConfig(viper.GetViper())

	if cfg.Enabled != false {
		t.Errorf("Enabled: got %v, want false (sync is opt-in alongside Episodic Memory)", cfg.Enabled)
	}
	if cfg.BatchMaxSessions != 25 {
		t.Errorf("BatchMaxSessions: got %d, want 25", cfg.BatchMaxSessions)
	}
	if cfg.BatchMaxBytes != 5*1024*1024 {
		t.Errorf("BatchMaxBytes: got %d, want %d", cfg.BatchMaxBytes, 5*1024*1024)
	}
	if cfg.SingleSessionMaxBytes != 4*1024*1024 {
		t.Errorf("SingleSessionMaxBytes: got %d, want %d", cfg.SingleSessionMaxBytes, 4*1024*1024)
	}
	if cfg.DaemonInterval != 24*time.Hour {
		t.Errorf("DaemonInterval: got %v, want 24h", cfg.DaemonInterval)
	}
	if cfg.DaemonStartupDelay != 60*time.Second {
		t.Errorf("DaemonStartupDelay: got %v, want 60s", cfg.DaemonStartupDelay)
	}
	if cfg.FailedMaxAttemptsTransient != 5 {
		t.Errorf("FailedMaxAttemptsTransient: got %d, want 5", cfg.FailedMaxAttemptsTransient)
	}
	if cfg.LockTimeout != 30*time.Second {
		t.Errorf("LockTimeout: got %v, want 30s", cfg.LockTimeout)
	}
}

func TestResolveEndpoint(t *testing.T) {
	v := viper.New()
	v.Set("cloud.endpoint", "https://cloud.example.com")

	// 1. sync.endpoint unset → cloud.endpoint wins.
	cfg := Config{}
	if got := ResolveEndpoint(cfg, v); got != "https://cloud.example.com" {
		t.Errorf("default fallback: got %q, want cloud.example.com", got)
	}

	// 2. sync.endpoint set → overrides cloud.endpoint.
	cfg.Endpoint = "https://sync.example.com"
	if got := ResolveEndpoint(cfg, v); got != "https://sync.example.com" {
		t.Errorf("override: got %q, want sync.example.com", got)
	}

	// 3. Both empty → empty string (caller's responsibility to error).
	v2 := viper.New()
	if got := ResolveEndpoint(Config{}, v2); got != "" {
		t.Errorf("both empty: got %q, want empty", got)
	}
}
