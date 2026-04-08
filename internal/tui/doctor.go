package tui

import (
	"fmt"
	"net/http"
	"os/exec"
	"runtime"
	"strings"
	"time"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

// runDoctor performs system diagnostics and returns formatted output.
func runDoctor(cfg *config.Config, version string, toolCount int) string {
	var sb strings.Builder
	sb.WriteString("  shan doctor\n")
	sb.WriteString("  " + strings.Repeat("─", 40) + "\n")

	// Version
	sb.WriteString(fmt.Sprintf("  ✓ Version      shan %s (%s, %s/%s)\n",
		version, runtime.Version(), runtime.GOOS, runtime.GOARCH))

	// Endpoint
	if cfg.Endpoint != "" {
		reachable := checkEndpoint(cfg.Endpoint)
		if reachable {
			sb.WriteString(fmt.Sprintf("  ✓ Endpoint     %s (reachable)\n", cfg.Endpoint))
		} else {
			sb.WriteString(fmt.Sprintf("  ✗ Endpoint     %s (unreachable)\n", cfg.Endpoint))
		}
	} else {
		sb.WriteString("  ✗ Endpoint     not configured\n")
	}

	// API Key
	if cfg.APIKey != "" {
		masked := maskAPIKey(cfg.APIKey)
		sb.WriteString(fmt.Sprintf("  ✓ API Key      configured (%s)\n", masked))
	} else {
		sb.WriteString("  ✗ API Key      not configured\n")
	}

	// Daemon status
	sb.WriteString(fmt.Sprintf("  %s\n", checkDaemon()))

	// Tools
	sb.WriteString(fmt.Sprintf("  ✓ Tools        %d registered\n", toolCount))

	// MCP servers
	mcpCount := len(cfg.MCPServers)
	sb.WriteString(fmt.Sprintf("  ✓ MCP          %d servers configured\n", mcpCount))

	// ripgrep
	rgPath, err := exec.LookPath("rg")
	if err == nil {
		sb.WriteString(fmt.Sprintf("  ✓ ripgrep      %s\n", rgPath))
	} else {
		sb.WriteString("  ✗ ripgrep      not found in PATH\n")
	}

	// Config dir
	shannonDir := config.ShannonDir()
	sb.WriteString(fmt.Sprintf("  ✓ Config       %s\n", shannonDir))

	sb.WriteString("  " + strings.Repeat("─", 40))
	return sb.String()
}

func maskAPIKey(key string) string {
	if len(key) < 10 {
		return "***"
	}
	return key[:4] + "..." + key[len(key)-4:]
}

func checkEndpoint(endpoint string) bool {
	client := &http.Client{Timeout: 3 * time.Second}
	resp, err := client.Head(endpoint)
	if err != nil {
		resp, err = client.Get(endpoint)
		if err != nil {
			return false
		}
	}
	resp.Body.Close()
	return true
}

func checkDaemon() string {
	shannonDir := config.ShannonDir()
	pidFile := shannonDir + "/daemon.pid"
	data, err := exec.Command("cat", pidFile).Output()
	if err != nil {
		return "✗ Daemon       not running"
	}
	pid := strings.TrimSpace(string(data))
	if pid == "" {
		return "✗ Daemon       not running"
	}
	err = exec.Command("kill", "-0", pid).Run()
	if err != nil {
		return "✗ Daemon       not running (stale PID " + pid + ")"
	}
	return "✓ Daemon       running (PID " + pid + ")"
}
