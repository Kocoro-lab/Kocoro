package sync

import (
	"time"

	"github.com/spf13/viper"
)

type Config struct {
	Enabled                    bool
	DryRun                     bool
	Endpoint                   string
	ExcludeAgents              []string
	ExcludeSources             []string
	BatchMaxSessions           int
	BatchMaxBytes              int
	SingleSessionMaxBytes      int
	DaemonInterval             time.Duration
	DaemonStartupDelay         time.Duration
	FailedMaxAttemptsTransient int
	LockTimeout                time.Duration
}

// setSyncDefaults registers all sync.* defaults on the given viper. This is
// duplicated in internal/config/config.go (where the central Load() runs);
// keep this list and that one in sync — the duplicate exists so unit tests in
// this package can establish defaults without importing internal/config.
//
// MUST stay in sync with the sync.* SetDefault calls in internal/config/config.go.
func setSyncDefaults(v *viper.Viper) {
	v.SetDefault("sync.enabled", false)
	v.SetDefault("sync.dry_run", false)
	v.SetDefault("sync.endpoint", "")
	v.SetDefault("sync.exclude_agents", []string{})
	v.SetDefault("sync.exclude_sources", []string{})
	v.SetDefault("sync.batch_max_sessions", 25)
	v.SetDefault("sync.batch_max_bytes", 5*1024*1024)
	v.SetDefault("sync.single_session_max_bytes", 4*1024*1024)
	v.SetDefault("sync.daemon_interval", "24h")
	v.SetDefault("sync.daemon_startup_delay", "60s")
	v.SetDefault("sync.failed_max_attempts_transient", 5)
	v.SetDefault("sync.lock_timeout", "30s")
}

func LoadConfig(v *viper.Viper) Config {
	return Config{
		Enabled:                    v.GetBool("sync.enabled"),
		DryRun:                     v.GetBool("sync.dry_run"),
		Endpoint:                   v.GetString("sync.endpoint"),
		ExcludeAgents:              v.GetStringSlice("sync.exclude_agents"),
		ExcludeSources:             v.GetStringSlice("sync.exclude_sources"),
		BatchMaxSessions:           v.GetInt("sync.batch_max_sessions"),
		BatchMaxBytes:              v.GetInt("sync.batch_max_bytes"),
		SingleSessionMaxBytes:      v.GetInt("sync.single_session_max_bytes"),
		DaemonInterval:             v.GetDuration("sync.daemon_interval"),
		DaemonStartupDelay:         v.GetDuration("sync.daemon_startup_delay"),
		FailedMaxAttemptsTransient: v.GetInt("sync.failed_max_attempts_transient"),
		LockTimeout:                v.GetDuration("sync.lock_timeout"),
	}
}

// ResolveEndpoint returns the upload endpoint to use, applying the override
// chain: sync.endpoint > cloud.endpoint. Returns "" if neither is set, in
// which case the caller should error out before constructing an uploader.
//
// Both CLI and daemon paths MUST use this helper instead of reading
// cloud.endpoint directly — otherwise sync.endpoint becomes documentation-only
// and silently fails to take effect when set.
func ResolveEndpoint(cfg Config, v *viper.Viper) string {
	if cfg.Endpoint != "" {
		return cfg.Endpoint
	}
	return v.GetString("cloud.endpoint")
}
