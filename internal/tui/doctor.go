package tui

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"

	"github.com/Kocoro-lab/ShanClaw/internal/client"
	"github.com/Kocoro-lab/ShanClaw/internal/mcp"
	"github.com/Kocoro-lab/ShanClaw/internal/permissions"
)

type doctorCheck struct {
	name   string
	ok     bool
	detail string
}

type doctorDoneMsg struct {
	checks []doctorCheck
}

// runDoctorChecks runs all sync diagnostic checks.
func runDoctorChecks(shannonDir, apiKey string, perms *permissions.PermissionsConfig, mcpServers map[string]mcp.MCPServerConfig, toolCount int) []doctorCheck {
	var checks []doctorCheck

	// 1. Config directory
	if shannonDir != "" {
		cfgPath := filepath.Join(shannonDir, "config.yaml")
		if _, err := os.Stat(cfgPath); err == nil {
			checks = append(checks, doctorCheck{"Config", true, cfgPath})
		} else {
			checks = append(checks, doctorCheck{"Config", false, "config.yaml not found in " + shannonDir})
		}
	} else {
		checks = append(checks, doctorCheck{"Config", false, "shannon directory not set"})
	}

	// 2. API key
	if apiKey != "" {
		masked := "****"
		if len(apiKey) > 4 {
			masked = "****" + apiKey[len(apiKey)-4:]
		}
		checks = append(checks, doctorCheck{"API key", true, masked})
	} else {
		checks = append(checks, doctorCheck{"API key", false, "not configured"})
	}

	// 3. Tools
	checks = append(checks, doctorCheck{"Tools", toolCount > 0, fmt.Sprintf("%d registered", toolCount)})

	// 4. Sessions dir writable
	sessDir := filepath.Join(shannonDir, "sessions")
	if tmpFile, err := os.CreateTemp(sessDir, ".doctor-check-*"); err == nil {
		tmpFile.Close()
		os.Remove(tmpFile.Name())
		checks = append(checks, doctorCheck{"Sessions dir", true, sessDir})
	} else {
		checks = append(checks, doctorCheck{"Sessions dir", false, fmt.Sprintf("not writable: %v", err)})
	}

	// 5. Permissions summary
	if perms != nil {
		checks = append(checks, doctorCheck{"Permissions", true, fmt.Sprintf("%d allowed, %d denied", len(perms.AllowedCommands), len(perms.DeniedCommands))})
	} else {
		checks = append(checks, doctorCheck{"Permissions", true, "default (no rules)"})
	}

	// 6. MCP servers
	mcpCount := len(mcpServers)
	if mcpCount > 0 {
		checks = append(checks, doctorCheck{"MCP servers", true, fmt.Sprintf("%d configured", mcpCount)})
	} else {
		checks = append(checks, doctorCheck{"MCP servers", false, "none configured"})
	}

	return checks
}

// runDoctorWithHealth runs all checks including the async API health check.
func runDoctorWithHealth(shannonDir, apiKey, endpoint string, gw *client.GatewayClient, perms *permissions.PermissionsConfig, mcpServers map[string]mcp.MCPServerConfig, toolCount int) []doctorCheck {
	checks := runDoctorChecks(shannonDir, apiKey, perms, mcpServers, toolCount)

	// API health check (with timeout)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if gw != nil {
		if err := gw.Health(ctx); err == nil {
			checks = append(checks, doctorCheck{"API reachable", true, endpoint})
		} else {
			checks = append(checks, doctorCheck{"API reachable", false, fmt.Sprintf("%s: %v", endpoint, err)})
		}
	} else {
		checks = append(checks, doctorCheck{"API reachable", false, "gateway client not initialized"})
	}

	return checks
}

func formatDoctorResults(checks []doctorCheck) string {
	dimStyle := lipgloss.NewStyle().Foreground(colorDim)
	okStyle := lipgloss.NewStyle().Foreground(colorSuccess)
	failStyle := lipgloss.NewStyle().Foreground(colorError)

	var sb strings.Builder
	sb.WriteString(dimStyle.Render("  Diagnostics:") + "\n")
	for _, c := range checks {
		icon := okStyle.Render("[ok]")
		if !c.ok {
			icon = failStyle.Render("[!!]")
		}
		sb.WriteString(fmt.Sprintf("  %s %s: %s\n", icon, dimStyle.Render(c.name), dimStyle.Render(c.detail)))
	}
	return strings.TrimRight(sb.String(), "\n")
}
