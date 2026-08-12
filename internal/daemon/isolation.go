package daemon

const (
	IsolationMarkerMCPDisabled        = "daemon: isolated mode — MCP connections and health supervision disabled"
	IsolationMarkerAutomationDisabled = "daemon: isolated mode — Cloud WS, watchers, heartbeat, and scheduler disabled"
	IsolationMarkerCloudWSSuppressed  = "daemon: isolated mode — Cloud WS connection suppressed"
	IsolationMarkerBackgroundDisabled = "daemon: isolated server background services disabled"
	// IsolationMarkerMCPAllowlisted replaces IsolationMarkerMCPDisabled when
	// --isolated-mcp names servers to keep. Isolated runs used to disable MCP
	// unconditionally, which made every MCP-touching behavior untestable in
	// isolation: --port and --state-dir both require --isolated, so the only way
	// to exercise an MCP tool was the default port against the developer's real
	// state. The marker stays distinct so a test harness can tell "MCP is off"
	// from "MCP is on, narrowed to this list" instead of inferring it.
	IsolationMarkerMCPAllowlisted = "daemon: isolated mode — MCP limited to allowlist:"
)
