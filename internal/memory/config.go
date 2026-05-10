package memory

import (
	"os"
	"strings"
	"time"

	appconfig "github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/spf13/viper"
)

type Config struct {
	Provider               string
	Endpoint               string
	APIKey                 string
	SocketPath             string
	BundleRoot             string
	TLMPath                string
	BundlePullInterval     time.Duration
	BundlePullStartupDelay time.Duration
	SidecarReadyTimeout    time.Duration
	SidecarShutdownGrace   time.Duration
	SidecarRestartMax      int
	ClientRequestTimeout   time.Duration
}

// LoadConfig produces a typed view of memory.* viper keys. Defaults are
// registered in internal/config/config.go (single source of truth); this
// package's accessors must not register defaults.
func LoadConfig(v *viper.Viper) Config {
	return Config{
		Provider:               v.GetString("memory.provider"),
		Endpoint:               v.GetString("memory.endpoint"),
		APIKey:                 v.GetString("memory.api_key"),
		SocketPath:             expandPath(v.GetString("memory.socket_path")),
		BundleRoot:             expandPath(v.GetString("memory.bundle_root")),
		TLMPath:                expandPath(v.GetString("memory.tlm_path")),
		BundlePullInterval:     v.GetDuration("memory.bundle_pull_interval"),
		BundlePullStartupDelay: v.GetDuration("memory.bundle_pull_startup_delay"),
		SidecarReadyTimeout:    v.GetDuration("memory.sidecar_ready_timeout"),
		SidecarShutdownGrace:   v.GetDuration("memory.sidecar_shutdown_grace"),
		SidecarRestartMax:      v.GetInt("memory.sidecar_restart_max"),
		ClientRequestTimeout:   v.GetDuration("memory.client_request_timeout"),
	}
}

// LoadConfigFromRuntime produces the memory config from a RuntimeConfigForCWD
// result. CLI/TUI paths must use this instead of process-global viper so
// cwd-local `.shannon/config.local.yaml` memory overrides take effect.
func LoadConfigFromRuntime(cfg *appconfig.Config) Config {
	if cfg == nil {
		return Config{}
	}
	apiKey := strings.TrimSpace(cfg.Memory.APIKey)
	if apiKey == "" {
		apiKey = strings.TrimSpace(cfg.APIKey)
	}
	endpoint := strings.TrimSpace(cfg.Memory.Endpoint)
	if endpoint == "" {
		endpoint = strings.TrimSpace(cfg.Endpoint)
	}
	return Config{
		Provider:               cfg.Memory.Provider,
		Endpoint:               endpoint,
		APIKey:                 apiKey,
		SocketPath:             expandPath(cfg.Memory.SocketPath),
		BundleRoot:             expandPath(cfg.Memory.BundleRoot),
		TLMPath:                expandPath(cfg.Memory.TLMPath),
		BundlePullInterval:     cfg.Memory.BundlePullInterval,
		BundlePullStartupDelay: cfg.Memory.BundlePullStartupDelay,
		SidecarReadyTimeout:    cfg.Memory.SidecarReadyTimeout,
		SidecarShutdownGrace:   cfg.Memory.SidecarShutdownGrace,
		SidecarRestartMax:      cfg.Memory.SidecarRestartMax,
		ClientRequestTimeout:   cfg.Memory.ClientRequestTimeout,
	}
}

// ResolveAPIKey is the ONLY call site that reads memory.api_key / cloud.api_key.
// memory.api_key wins as override; falls back to cloud.api_key.
func ResolveAPIKey(v *viper.Viper) string {
	if k := strings.TrimSpace(v.GetString("memory.api_key")); k != "" {
		return k
	}
	return strings.TrimSpace(v.GetString("cloud.api_key"))
}

// ResolveEndpoint mirrors ResolveAPIKey for the endpoint pair.
func ResolveEndpoint(v *viper.Viper) string {
	if e := strings.TrimSpace(v.GetString("memory.endpoint")); e != "" {
		return e
	}
	return strings.TrimSpace(v.GetString("cloud.endpoint"))
}

// expandPath expands environment variables ($HOME, $TMPDIR, etc.) and leading ~.
// $TMPDIR falls back to os.TempDir() when the env var is unset (portable
// across macOS, Linux CI, and test sandboxes where $TMPDIR may be empty).
func expandPath(p string) string {
	if p == "" {
		return ""
	}
	// Substitute $TMPDIR explicitly before os.ExpandEnv so the fallback fires
	// even when $TMPDIR is unset (os.ExpandEnv would leave it as "").
	tmpdir := os.Getenv("TMPDIR")
	if tmpdir == "" {
		tmpdir = os.TempDir()
	}
	p = strings.ReplaceAll(p, "$TMPDIR", tmpdir)
	expanded := os.ExpandEnv(p)
	if strings.HasPrefix(expanded, "~") {
		return os.Getenv("HOME") + expanded[1:]
	}
	return expanded
}
