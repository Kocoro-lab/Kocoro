package daemon

import (
	"sort"
	"strings"

	"github.com/Kocoro-lab/ShanClaw/internal/config"
)

const (
	IsolationMarkerMCPDisabled             = "daemon: isolated mode — MCP connections and health supervision disabled"
	IsolationMarkerAutomationDisabled      = "daemon: isolated mode — Cloud WS, watchers, heartbeat, and scheduler disabled"
	IsolationMarkerCloudWSSuppressed       = "daemon: isolated mode — Cloud WS connection suppressed"
	IsolationMarkerBackgroundDisabled      = "daemon: isolated server background services disabled"
	IsolationMarkerCredentialStoreDisabled = "daemon: isolated mode — credential store access disabled"
	// IsolationMarkerMCPAllowlisted replaces IsolationMarkerMCPDisabled when
	// --isolated-mcp names servers to keep. Isolated runs used to disable MCP
	// unconditionally, which made every MCP-touching behavior untestable in
	// isolation: --port and --state-dir both require --isolated, so the only way
	// to exercise an MCP tool was the default port against the developer's real
	// state. The marker stays distinct so a test harness can tell "MCP is off"
	// from "MCP is on, narrowed to this list" instead of inferring it.
	IsolationMarkerMCPAllowlisted = "daemon: isolated mode — MCP limited to allowlist:"
)

// RestrictMCPServersToAllowlist mutates cfg so only explicitly named,
// previously-enabled MCP servers remain enabled. Empty, unknown, and disabled
// names all fail closed to no enabled MCP servers. Callers must apply this
// before tool registration: registration captures the enabled server map and
// may eagerly prepare browser dependencies.
func RestrictMCPServersToAllowlist(cfg *config.Config, allowlist string) []string {
	requested := make(map[string]struct{})
	for _, name := range strings.Split(allowlist, ",") {
		name = strings.TrimSpace(name)
		if name != "" {
			requested[name] = struct{}{}
		}
	}
	if cfg == nil {
		return nil
	}

	kept := make([]string, 0, len(requested))
	for name, serverCfg := range cfg.MCPServers {
		_, allowed := requested[name]
		if allowed && !serverCfg.Disabled {
			kept = append(kept, name)
		} else {
			serverCfg.Disabled = true
			cfg.MCPServers[name] = serverCfg
		}
	}
	sort.Strings(kept)
	return kept
}
