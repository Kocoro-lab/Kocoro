package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/viper"

	"github.com/Kocoro-lab/ShanClaw/internal/audit"
	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/config"
	"github.com/Kocoro-lab/ShanClaw/internal/sync"
)

var sessionsCmd = &cobra.Command{
	Use:   "sessions",
	Short: "Manage local sessions (sync to Cloud, search, etc.)",
}

var sessionsSyncCmd = &cobra.Command{
	Use:   "sync",
	Short: "Upload modified sessions to Shannon Cloud when sync is enabled",
	RunE: func(cmd *cobra.Command, args []string) error {
		if _, err := config.Load(); err != nil {
			return fmt.Errorf("config: %w", err)
		}
		home, err := os.UserHomeDir()
		if err != nil {
			return fmt.Errorf("locate home dir: %w", err)
		}
		shannonHome := filepath.Join(home, ".shannon")

		cfg := sync.LoadConfig(viper.GetViper())
		if !cfg.Enabled && !cfg.DryRun {
			fmt.Fprintln(cmd.OutOrStdout(), "sync is disabled (sync.enabled=false). Set sync.enabled=true to enable.")
			return nil
		}

		var uploader sync.Uploader
		outboxDir := filepath.Join(shannonHome, "sync_outbox")
		if cfg.DryRun {
			uploader = &sync.DryRunUploader{OutboxDir: outboxDir, Now: time.Now}
		} else {
			endpoint := sync.ResolveEndpoint(cfg, viper.GetViper())
			apiKey := viper.GetString("cloud.api_key")
			if endpoint == "" || apiKey == "" {
				return fmt.Errorf("upload endpoint (sync.endpoint or cloud.endpoint) and cloud.api_key must be configured (or set sync.dry_run=true)")
			}
			gw := client.NewGatewayClient(endpoint, apiKey)
			uploader = &sync.CloudUploader{Client: gw}
		}

		auditLogger, err := audit.NewAuditLogger(filepath.Join(shannonHome, "logs"))
		if err != nil {
			return fmt.Errorf("open audit log: %w", err)
		}
		defer auditLogger.Close()

		loader := func(dir, id string) ([]byte, error) {
			return os.ReadFile(filepath.Join(dir, id+".json"))
		}

		ver := readBuildInfo()

		deps := sync.Deps{
			Cfg:       cfg,
			HomeDir:   shannonHome,
			ClientVer: ver,
			Uploader:  uploader,
			Loader:    loader,
			Audit:     &auditAdapter{logger: auditLogger, stdout: cmd.OutOrStdout()},
			Now:       time.Now,
		}
		return sync.Run(context.Background(), deps)
	},
}

// auditAdapter bridges *audit.AuditLogger (which logs structured AuditEntry
// rows) to sync.AuditLogger (event + free-form fields). The fields map is
// JSON-encoded into AuditEntry.InputSummary so the JSON-lines audit log keeps
// a single shape. When stdout is non-nil, a human-readable one-line summary
// of session_sync events is also printed so CLI users see progress on exit
// (sync.Run itself is silent by design).
type auditAdapter struct {
	logger *audit.AuditLogger
	stdout io.Writer
}

func (a *auditAdapter) Log(event string, fields map[string]any) {
	if a == nil {
		return
	}
	summary := ""
	if len(fields) > 0 {
		if b, err := json.Marshal(fields); err == nil {
			summary = string(b)
		}
	}
	if a.logger != nil {
		a.logger.Log(audit.AuditEntry{
			Timestamp:    time.Now(),
			ToolName:     event,
			InputSummary: summary,
			Decision:     "info",
		})
	}
	if a.stdout != nil && event == "session_sync" {
		fmt.Fprintln(a.stdout, formatSyncSummary(fields))
	}
}

// formatSyncSummary renders a single-line human-readable view of a
// session_sync audit event. The field names mirror sync.Run's audit call so
// any future fields added there will show up as "key=<value>" tail items.
func formatSyncSummary(fields map[string]any) string {
	outcome, _ := fields["outcome"].(string)
	if outcome == "" {
		outcome = "unknown"
	}
	sent, _ := fields["sent"].(int)
	accepted, _ := fields["accepted"].(int)
	rtx, _ := fields["rejected_transient"].(int)
	rpm, _ := fields["rejected_permanent"].(int)
	carry, _ := fields["failed_carryover"].(int)
	reason, _ := fields["reason"].(string)
	transport, _ := fields["transport_error"].(bool)

	parts := []string{fmt.Sprintf("sync: outcome=%s sent=%d accepted=%d rejected_transient=%d rejected_permanent=%d failed_carryover=%d",
		outcome, sent, accepted, rtx, rpm, carry)}
	if reason != "" {
		parts = append(parts, "reason="+reason)
	}
	if transport {
		parts = append(parts, "transport_error=true")
	}
	return strings.Join(parts, " ")
}

func readBuildInfo() string {
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return "kocoro/" + info.Main.Version
	}
	return "kocoro/dev"
}

func init() {
	sessionsCmd.AddCommand(sessionsSyncCmd)
	rootCmd.AddCommand(sessionsCmd)
}
