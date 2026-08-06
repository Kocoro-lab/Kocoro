package daemon

const (
	IsolationMarkerMCPDisabled        = "daemon: isolated mode — MCP connections and health supervision disabled"
	IsolationMarkerAutomationDisabled = "daemon: isolated mode — Cloud WS, watchers, heartbeat, and scheduler disabled"
	IsolationMarkerCloudWSSuppressed  = "daemon: isolated mode — Cloud WS connection suppressed"
	IsolationMarkerBackgroundDisabled = "daemon: isolated server background services disabled"
)
